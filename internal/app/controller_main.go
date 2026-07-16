package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/clock"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/controller"
	"github.com/melodic-software/ci-runner/internal/host"
	"github.com/melodic-software/ci-runner/internal/jobindex"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/scaleset"
	"github.com/melodic-software/ci-runner/internal/secret"
	statefs "github.com/melodic-software/ci-runner/internal/state/fs"
	"github.com/melodic-software/ci-runner/internal/telemetry"
)

// ControllerRestartExitCode is emitted only after the authenticated restart
// drain and durable receipt commit both succeed. The CLI requires this exact
// code in addition to the receipt before it may start the scheduled task.
const ControllerRestartExitCode uint32 = 75

var ErrControllerRestartRequested = errors.New("controller restart requested after graceful drain")

type restartReceiptWriter interface {
	SaveRestartReceipt(context.Context, model.RestartReceipt) error
}

// RunControllerMain composes the native Windows controller. It returns nil
// only after a clean transient drain has closed the message sessions and Docker
// runtime; it never changes the user's persisted desired mode.
func RunControllerMain(ctx context.Context, args []string, errOut io.Writer) error {
	configPath, remaining, err := resolveConfigArgument(args)
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		return errors.New("usage: ci-runner-controller [--config ABSOLUTE_PATH]")
	}
	cfg, err := loadConfiguration(configPath)
	if err != nil {
		return err
	}
	acl := secret.NewAccessController()
	controllerLogDirectory := filepath.Join(cfg.Paths.Logs, "controller")
	for _, directory := range []string{cfg.Paths.State, controllerLogDirectory, filepath.Join(cfg.Paths.Logs, "workers"), cfg.Paths.Diagnostics} {
		if err := preparePrivateRuntimeDirectory(directory, acl); err != nil {
			return err
		}
	}
	if err := ensureNoReparsePoints(cfg.Paths.Secrets); err != nil {
		return fmt.Errorf("verify secret directory path: %w", err)
	}
	if err := acl.Verify(cfg.Paths.Secrets); err != nil {
		return fmt.Errorf("verify secret directory ACL: %w", err)
	}
	logs, err := host.NewJSONLogSink(controllerLogDirectory, cfg.Logs.Controller, cfg.Logs.CleanupEvery.Duration, acl)
	if err != nil {
		return err
	}
	defer func() {
		// The controller cannot report a final sink-close failure through the
		// same sink, and shutdown behavior must remain independent of logging.
		_ = logs.Close()
	}()
	logEvent := func(code, message string) {
		_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: code, Message: message})
	}
	fail := func(code string, err error) error {
		if err != nil {
			logEvent(code, err.Error())
		}
		return err
	}
	telemetryOptions := telemetry.Options{
		HostID: cfg.Host.ID, Version: buildinfo.Version,
		OnError: func(exportErr error) {
			if exportErr != nil {
				logEvent("telemetry-export-error", exportErr.Error())
			}
		},
	}
	if cfg.Telemetry.Enabled() {
		telemetryOptions.Export = &telemetry.ExportConfig{
			Endpoint: cfg.Telemetry.Endpoint, Protocol: cfg.Telemetry.Protocol,
			Traces: cfg.Telemetry.Traces, Metrics: cfg.Telemetry.Metrics,
			MetricExportInterval: cfg.Telemetry.MetricExportInterval.Duration,
			MetricExportTimeout:  cfg.Telemetry.MetricExportTimeout.Duration,
		}
	}
	telemetryProvider, telemetryProblems := telemetry.NewFromEnv(ctx, telemetryOptions)
	for _, telemetryErr := range telemetryProblems {
		logEvent("telemetry-configuration-error", telemetryErr.Error())
	}
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := telemetryProvider.Shutdown(shutdownContext); shutdownErr != nil {
			logEvent("telemetry-shutdown-error", shutdownErr.Error())
		}
	}()

	manifest, err := LoadCompatibilityManifest(cfg.Release.CompatibilityManifest, buildinfo.Version)
	if err != nil {
		return fail("compatibility-manifest-error", err)
	}
	locker, err := statefs.NewPlatformLocker(cfg.Paths.State)
	if err != nil {
		return fail("state-mutex-error", err)
	}
	store, err := statefs.New(cfg.Paths.State, locker, acl)
	if err != nil {
		return fail("state-store-error", err)
	}
	jobs, err := jobindex.NewFileStore(cfg.Paths.State, locker, acl)
	if err != nil {
		return fail("job-index-error", err)
	}
	secretStore := secret.Store{Protector: secret.NewDPAPIProtector(), Directory: cfg.Paths.Secrets}
	scaleSets, err := scaleset.NewOfficialClient(scaleset.OfficialOptions{
		HostID: cfg.Host.ID, Version: buildinfo.Version, CommitSHA: manifest.Source.SHA,
		RequestTimeout: cfg.GitHub.RequestTimeout.Duration, Secrets: secretStore,
		Events:   jobindex.EventSink{Store: jobs},
		Observer: telemetryProvider,
	})
	if err != nil {
		return fail("scale-set-client-error", err)
	}
	workers, err := newWorkerRuntime(cfg, manifest, acl, jobs, telemetryProvider, func(runtimeErr error) {
		if runtimeErr != nil {
			logEvent("worker-runtime-error", runtimeErr.Error())
		}
	})
	if err != nil {
		_ = scaleSets.Close(context.Background())
		return fail("docker-runtime-error", err)
	}
	reconciler, err := controller.NewReconciler(cfg, buildinfo.Version, controller.Dependencies{
		ScaleSets: scaleSets,
		Workers:   workers,
		Desktop:   host.NewControllerDesktopAdapter(),
		Power:     host.WindowsPowerMonitor{},
		Resources: &host.WindowsResourceMonitor{},
		State:     store,
		Jobs:      jobs,
		Clock:     clock.Real{},
		Logs:      logs,
		Telemetry: telemetryProvider,
	})
	if err != nil {
		_ = workers.Close()
		_ = scaleSets.Close(context.Background())
		return fail("controller-construction-error", err)
	}
	handler, err := controller.NewControlHandler(reconciler, uint32(os.Getpid()))
	if err != nil {
		_ = workers.Close()
		_ = scaleSets.Close(context.Background())
		return fail("control-handler-error", err)
	}
	server, err := control.NewCurrentUserServer(handler)
	if err != nil {
		_ = workers.Close()
		_ = scaleSets.Close(context.Background())
		return fail("control-server-error", err)
	}
	logEvent("controller-started", fmt.Sprintf("version=%s host=%s worker=%s", buildinfo.Version, cfg.Host.ID, manifest.WorkerReference()))
	err = runControllerLoop(ctx, cfg, reconciler, handler, server, logs, store, uint32(os.Getpid()), buildinfo.Version)
	if err != nil {
		return fail("controller-stopped-with-error", err)
	}
	logEvent("controller-stopped", "graceful shutdown completed")
	return nil
}

func loadConfiguration(path string) (config.Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return config.Config{}, fmt.Errorf("open configuration %q: %w", path, err)
	}
	cfg, loadErr := config.Load(file)
	closeErr := file.Close()
	if err := errors.Join(loadErr, closeErr); err != nil {
		return config.Config{}, fmt.Errorf("load configuration %q: %w", path, err)
	}
	return cfg, nil
}

// reconcileStepMinRetryAttempts floors the configured retry count used to size
// the Step watchdog so a pathological maxAttempts of 0 or 1 cannot collapse the
// deadline toward a single request.
const reconcileStepMinRetryAttempts = 3

// reconcileStepOpsPerTarget is how many retryable GitHub calls a Step makes per
// configured target during its worst-case legitimate sweep: one r.ensure plus
// one r.statistics, each a full RetryValue budget.
const reconcileStepOpsPerTarget = 2

// reconcileStepJITOpsPerWorker is how many retryable GitHub calls a Step makes
// per worker it starts during its worst-case legitimate JIT registration
// sequence: one CreateJITConfig call, a full RetryValue budget, mirroring the
// same policy-compliant retry/backoff shape as reconcileStepOpsPerTarget.
const reconcileStepJITOpsPerWorker = 1

// reconcileStepTimeout bounds one reconcile Step. It is a COARSE BACKSTOP, not
// the primary stall detector: the scale-set transport hardening (HTTP/2 health
// pings plus TCP keepalive) already errors a half-open socket in well under a
// minute, so this deadline essentially never fires in practice. Its only
// correctness requirement is therefore to NEVER interrupt a legitimate Step, so
// it is sized deliberately generously — a larger backstop has no downside.
//
// A Step is not one poll: step() sweeps every configured target, calling
// r.ensure and then r.statistics per target, each running through RetryValue for
// up to Retry.MaxAttempts attempts (each capped at RequestTimeout and separated
// by backoff waits capped at Retry.Maximum). The worst-case legitimate duration
// therefore scales with the target count, so budget reconcileStepOpsPerTarget
// retryable operations per target, multiply by the per-attempt cap and the
// attempt count to upper bound one full sweep.
//
// Step's worker-start section also calls CreateJITConfig once per worker it
// starts, each likewise run through RetryValue for up to Retry.MaxAttempts
// attempts. Resources.MaximumConcurrentWorkers is the host-wide cap on workers
// started (and therefore on JIT registrations) within a single Step, so budget
// reconcileStepJITOpsPerWorker retryable operations per unit of that cap, using
// the same per-attempt cap and attempt count, and add it to the sweep budget
// before the 50% margin for remaining per-worker provisioning work.
func reconcileStepTimeout(cfg config.Config) time.Duration {
	attempts := cfg.GitHub.Retry.MaxAttempts
	if attempts < reconcileStepMinRetryAttempts {
		attempts = reconcileStepMinRetryAttempts
	}
	stepOps := reconcileStepOpsPerTarget * max(len(cfg.GitHub.Targets), 1)
	jitOps := reconcileStepJITOpsPerWorker * max(cfg.Resources.MaximumConcurrentWorkers, 1)
	retryBudget := time.Duration((stepOps+jitOps)*attempts) * (cfg.GitHub.RequestTimeout.Duration + cfg.GitHub.Retry.Maximum.Duration)
	return retryBudget + retryBudget/2
}

func runControllerLoop(
	ctx context.Context,
	cfg config.Config,
	reconciler *controller.Reconciler,
	handler *controller.ControlHandler,
	server *control.Server,
	logs controller.LogSink,
	restartReceipts restartReceiptWriter,
	processID uint32,
	version string,
) error {
	serverContext, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(serverContext) }()

	shutdown := func(signal controller.ShutdownSignal, awaitServer bool) error {
		_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "controller-draining", Message: signal.Reason})
		shutdownErr := reconciler.Shutdown(context.Background())
		// Close before canceling so this caller wins Server.Close's sync.Once and
		// retains any listener/connection close error in the restart proof.
		closeErr := server.Close()
		stopServer()
		if !awaitServer {
			return completeControllerShutdown(context.Background(), errors.Join(shutdownErr, closeErr), signal, restartReceipts, processID, version)
		}
		var result error
		select {
		case serveErr := <-serverErrors:
			result = errors.Join(shutdownErr, closeErr, serveErr)
		case <-time.After(cfg.Controller.ShutdownPollInterval.Duration):
			result = errors.Join(shutdownErr, closeErr, errors.New("control server did not stop after listener close"))
		}
		return completeControllerShutdown(context.Background(), result, signal, restartReceipts, processID, version)
	}

	stepTimeout := reconcileStepTimeout(cfg)
	for {
		stepContext, cancelStep := context.WithTimeout(context.Background(), stepTimeout)
		stepDone := make(chan error, 1)
		go func() {
			_, stepErr := reconciler.Step(stepContext)
			stepDone <- stepErr
		}()
		select {
		case signal := <-handler.ShutdownRequests():
			cancelStep()
			<-stepDone
			return shutdown(signal, true)
		case <-ctx.Done():
			cancelStep()
			<-stepDone
			return shutdown(controller.ShutdownSignal{Reason: "process interrupt"}, true)
		case serveErr := <-serverErrors:
			cancelStep()
			<-stepDone
			if serveErr == nil {
				serveErr = errors.New("control server exited unexpectedly")
			}
			return errors.Join(serveErr, shutdown(controller.ShutdownSignal{Reason: "control server exited unexpectedly"}, false))
		case <-stepContext.Done():
			// The step overran its watchdog deadline. Cancel it, surface the stall,
			// and continue to the next reconcile rather than blocking on stepDone: a
			// wedged Step (for example a half-open listener long poll) must not park
			// the controller and let the warm pool decay. The deadline has already
			// cancelled stepContext, so the goroutine unwinds and reports to the
			// buffered stepDone channel on its own without leaking.
			cancelStep()
			_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-watchdog-timeout", Message: fmt.Sprintf("reconcile step exceeded its %s watchdog deadline and was cancelled", stepTimeout)})
		case stepErr := <-stepDone:
			cancelStep()
			if stepErr != nil {
				_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-error", Message: stepErr.Error()})
			}
		}

		timer := time.NewTimer(cfg.Controller.ReconcileInterval.Duration)
		select {
		case signal := <-handler.ShutdownRequests():
			if !timer.Stop() {
				<-timer.C
			}
			return shutdown(signal, true)
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return shutdown(controller.ShutdownSignal{Reason: "process interrupt"}, true)
		case serveErr := <-serverErrors:
			if !timer.Stop() {
				<-timer.C
			}
			if serveErr == nil {
				serveErr = errors.New("control server exited unexpectedly")
			}
			return errors.Join(serveErr, shutdown(controller.ShutdownSignal{Reason: "control server exited unexpectedly"}, false))
		case <-timer.C:
		}
	}
}

// completeControllerShutdown commits the durable half of task-restart
// authorization. Any drain, runtime-close, listener-close, or receipt-write
// failure returns without the restart sentinel, so main emits an ordinary exit
// code and the CLI fails closed even if a partial receipt became visible.
func completeControllerShutdown(
	ctx context.Context,
	shutdownErr error,
	signal controller.ShutdownSignal,
	restartReceipts restartReceiptWriter,
	processID uint32,
	version string,
) error {
	if shutdownErr != nil || !signal.Restart {
		return shutdownErr
	}
	if restartReceipts == nil || signal.RequestID == "" || processID == 0 || version == "" {
		return errors.New("restart completion receipt dependencies are invalid")
	}
	receipt := model.RestartReceipt{
		SchemaVersion: 1,
		RequestID:     signal.RequestID,
		ProcessID:     processID,
		Version:       version,
		CompletedAt:   time.Now().UTC(),
	}
	if err := restartReceipts.SaveRestartReceipt(ctx, receipt); err != nil {
		return fmt.Errorf("persist restart completion receipt: %w", err)
	}
	return ErrControllerRestartRequested
}
