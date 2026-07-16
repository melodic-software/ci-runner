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

// reconcileStepDesktopStartAttempts upper-bounds how many DesktopManager.Start
// calls a single Step could make. reconciler.go has two Start call sites: an
// eager bootstrap before resource admission and inventory (the observation
// section), and BuildPlan's plan.StartDesktop fallback. Tracing today's
// control flow, they are mutually exclusive within one Step: a failed eager
// Start sets observationFailed, which zeroes the resource snapshot
// (reconciler.go's "invalid observation fails closed in BuildPlan"), which
// fails BuildPlan's resourceHealthy gate closed and returns PhaseResourceConstrained
// before plan.StartDesktop is ever assigned (see evaluateResourceGate and
// BuildPlan's !resourceHealthy branch in plan.go). This budgets both call
// sites anyway rather than relying on that cross-file invariant holding
// forever: summing verified call sites is the more robust way to size a
// coarse backstop, it keeps the budget correct even if a future refactor
// changes that control flow, and — per reconcileStepTimeout's doc comment — a
// larger backstop has no downside.
const reconcileStepDesktopStartAttempts = 2

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
//
// Step also drives Docker Desktop's own lifecycle (reconciler.go's
// r.deps.Desktop.Start/Stop call sites), independently of the GitHub retry
// loops above and independently configured via DockerDesktop.StartTimeout and
// DockerDesktop.StopTimeout. Each Start/Stop call hard-bounds itself to its
// configured timeout (ControllerDesktopAdapter.Start/Stop applies it via
// context.WithTimeout), so — unlike the GitHub retry operations — it needs no
// attempts multiplier or margin of its own to bound its worst case exactly.
// Budget reconcileStepDesktopStartAttempts Start calls (see its doc comment
// for why that upper-bounds every Start call site rather than asserting they
// are all reachable in one Step) plus one Stop call, and add that directly to
// the GitHub-retry budget (including its 50% margin) so a policy-compliant
// desktop start or stop is never cut short by a watchdog sized only for
// GitHub retries.
func reconcileStepTimeout(cfg config.Config) time.Duration {
	attempts := cfg.GitHub.Retry.MaxAttempts
	if attempts < reconcileStepMinRetryAttempts {
		attempts = reconcileStepMinRetryAttempts
	}
	stepOps := reconcileStepOpsPerTarget * max(len(cfg.GitHub.Targets), 1)
	jitOps := reconcileStepJITOpsPerWorker * max(cfg.Resources.MaximumConcurrentWorkers, 1)
	retryBudget := time.Duration((stepOps+jitOps)*attempts) * (cfg.GitHub.RequestTimeout.Duration + cfg.GitHub.Retry.Maximum.Duration)
	githubBudget := retryBudget + retryBudget/2
	desktopBudget := reconcileStepDesktopStartAttempts*cfg.DockerDesktop.StartTimeout.Duration + cfg.DockerDesktop.StopTimeout.Duration
	return githubBudget + desktopBudget
}

// reconcileStepDrainGrace bounds how long the reconcile loop waits, after
// cancelling a watchdog-timed-out Step's context, for that Step goroutine to
// actually unwind and release Reconciler.stepMu before deciding it is safe to
// start the next one. RetryValue checks ctx.Err() before every attempt and
// every backoff wait is context-cancellable (see internal/controller/retry.go),
// so a well-behaved goroutine returns almost immediately after cancellation.
// The only legitimate remaining delay is one more in-flight adapter call,
// bounded by RequestTimeout, or a backoff sleep it was about to enter, bounded
// by Retry.Maximum. Reusing those same watchdog-mechanism constants keeps this
// grace period proportionate without introducing a new tunable.
func reconcileStepDrainGrace(cfg config.Config) time.Duration {
	return cfg.GitHub.RequestTimeout.Duration + cfg.GitHub.Retry.Maximum.Duration
}

// reconcileWatchdogState tracks a Step goroutine that overran the watchdog
// deadline and then failed to unwind within its drain grace period. Step
// holds stepMu for its entire duration, so if the blocked adapter call inside
// it did not actually abort on context cancellation (the exact half-open /
// stuck-operation scenario the watchdog exists to survive), starting another
// Step would just queue forever behind the same held mutex — piling up more
// leaked, blocked goroutines instead of making progress. Once a goroutine is
// abandoned, readyForNextStep keeps the loop from starting a new Step until
// that goroutine is confirmed to have released stepMu.
//
// This does not, by itself, prevent a graceful shutdown from hanging: the
// shutdown closure below calls reconciler.Shutdown, which loops on
// Reconciler.Step and therefore also blocks on stepMu.Lock (an ordinary
// sync.Mutex, not context-aware) if the same goroutine is still stuck holding
// it. Fixing that fully requires a change to Reconciler's locking (e.g. a
// TryLock-style path) in internal/controller/reconciler.go; this fix only
// stops the reconcile loop from compounding the problem by piling up more
// blocked goroutines behind the same held lock.
type reconcileWatchdogState struct {
	abandoned <-chan error
}

// abandon records a watchdog-timed-out Step goroutine that did not unwind
// within its drain grace period.
func (w *reconcileWatchdogState) abandon(stepDone <-chan error) {
	w.abandoned = stepDone
}

// readyForNextStep reports whether the loop may start a new Step this tick.
// When a Step goroutine is currently abandoned, it performs a non-blocking
// check for that goroutine's stepDone signal: if it has finally unwound,
// normal reconciliation resumes; otherwise this tick is skipped so the loop
// does not stack another goroutine behind the still-held stepMu.
func (w *reconcileWatchdogState) readyForNextStep(logs controller.LogSink) bool {
	if w.abandoned == nil {
		return true
	}
	select {
	case stepErr := <-w.abandoned:
		w.abandoned = nil
		if stepErr != nil {
			_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-error", Message: stepErr.Error()})
		}
		_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-watchdog-drain-recovered", Message: "a reconcile step abandoned after a watchdog timeout released its lock; resuming normal reconciliation"})
		return true
	default:
		_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-watchdog-tick-skipped", Message: "skipping this reconcile tick: a previously abandoned step has not released its lock and starting a new one would only queue behind it"})
		return false
	}
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
	stepDrainGrace := reconcileStepDrainGrace(cfg)
	var watchdog reconcileWatchdogState
	for {
		if !watchdog.readyForNextStep(logs) {
			// A prior watchdog-timed-out Step goroutine has still not released
			// stepMu. Starting a new Step now would only block forever behind
			// it, so wait out this tick without starting one and re-check on
			// the next iteration.
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
			continue
		}

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
			// The step overran its watchdog deadline. Cancel it and surface the
			// stall, then give the goroutine a bounded grace period to unwind and
			// release stepMu before deciding whether it is safe to start the next
			// Step: a wedged Step (for example a half-open listener long poll that
			// does not honor cancellation) must not let a fresh Step queue forever
			// behind the same held mutex.
			cancelStep()
			_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-watchdog-timeout", Message: fmt.Sprintf("reconcile step exceeded its %s watchdog deadline and was cancelled", stepTimeout)})
			// This inner select must stay responsive to shutdown/interrupt/server
			// signals just like every other select in this loop: draining a stuck
			// step must never make the controller unresponsive for up to
			// stepDrainGrace. stepDone is already buffered and the step's context
			// is already cancelled, so the shutdown/interrupt/server branches below
			// return immediately without waiting on it further.
			select {
			case signal := <-handler.ShutdownRequests():
				return shutdown(signal, true)
			case <-ctx.Done():
				return shutdown(controller.ShutdownSignal{Reason: "process interrupt"}, true)
			case serveErr := <-serverErrors:
				if serveErr == nil {
					serveErr = errors.New("control server exited unexpectedly")
				}
				return errors.Join(serveErr, shutdown(controller.ShutdownSignal{Reason: "control server exited unexpectedly"}, false))
			case stepErr := <-stepDone:
				if stepErr != nil {
					_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-error", Message: stepErr.Error()})
				}
			case <-time.After(stepDrainGrace):
				// The goroutine did not unwind: Step holds stepMu for its entire
				// duration, so the blocked adapter call inside it did not actually
				// abort on context cancellation. Leave stepDone unclaimed — the
				// goroutine will still deliver to it once it eventually unwinds —
				// and escalate instead of starting a new Step that would just
				// queue behind the same held lock.
				_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-watchdog-stuck", Message: fmt.Sprintf("reconcile step did not release its lock within %s of cancellation; skipping reconcile ticks until it unwinds", stepDrainGrace)})
				watchdog.abandon(stepDone)
			}
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
