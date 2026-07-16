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

// reconcileStepRetirementOpsPerWorker is how many retryable GitHub calls a
// Step makes per worker it retires during its worst-case legitimate
// deregistration sequence: one RemoveRunner call, a full RetryValue budget,
// mirroring the same policy-compliant retry/backoff shape as
// reconcileStepJITOpsPerWorker. Unlike JIT starts, the number of idle workers
// eligible for retirement in plan.Remove is not itself bounded by
// Resources.MaximumConcurrentWorkers: lowering that setting (or warm
// capacity) can leave more existing idle workers to drain than the new cap
// allows for. So reconciler.go's plan.Remove loop caps how many
// deregisterRunner calls it actually issues in a single Step at
// Resources.MaximumConcurrentWorkers (deferring any remainder to a later
// Step, where plan.Remove is recomputed and picks them back up), which keeps
// this budget derivable from static config instead of unbounded runtime
// worker inventory.
const reconcileStepRetirementOpsPerWorker = 1

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
// Each of those backoff waits is not itself capped at bare Retry.Maximum:
// BackoffPolicy.delay (internal/controller/retry.go) applies jitter after
// capping the base delay to Maximum, drawing uniformly from
// [1-JitterRatio, 1+JitterRatio], so a single policy-compliant wait can reach
// Maximum*(1+JitterRatio) — up to nearly 2x Maximum when JitterRatio is at its
// validated ceiling of 1. Every retry-budget calculation below therefore uses
// that jittered worst-case delay (maxJitteredBackoff), not bare Retry.Maximum,
// so a legitimately jittered retry loop can never exceed this deadline.
//
// Step's worker-start section also calls CreateJITConfig once per worker it
// starts, each likewise run through RetryValue for up to Retry.MaxAttempts
// attempts. Resources.MaximumConcurrentWorkers is the host-wide cap on workers
// started (and therefore on JIT registrations) within a single Step, so budget
// reconcileStepJITOpsPerWorker retryable operations per unit of that cap, using
// the same per-attempt cap and attempt count, and add it to the sweep budget
// before the 50% margin for remaining per-worker provisioning work.
//
// Step's worker-removal section symmetrically calls deregisterRunner (a
// RemoveRunner call run through the same RetryValue budget) once per idle
// worker it retires from plan.Remove, before RemoveIfIdle
// (internal/controller/reconciler.go:603-609). That retirement count is not
// itself bounded by Resources.MaximumConcurrentWorkers — plan.Remove can
// legitimately carry more idle workers than the current cap allows for after
// MaximumConcurrentWorkers or warm capacity is lowered, since existing workers
// are drained rather than force-dropped — so reconciler.go's removal loop caps
// the deregisterRunner calls it issues in a single Step at
// Resources.MaximumConcurrentWorkers, deferring any remainder to a later Step.
// Budget reconcileStepRetirementOpsPerWorker retryable operations per unit of
// that same cap, using the same per-attempt cap and attempt count, mirroring
// the JIT budget above.
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
	retirementOps := reconcileStepRetirementOpsPerWorker * max(cfg.Resources.MaximumConcurrentWorkers, 1)
	maxJitteredBackoff := cfg.GitHub.Retry.Maximum.Duration +
		time.Duration(float64(cfg.GitHub.Retry.Maximum.Duration)*cfg.GitHub.Retry.JitterRatio)
	retryBudget := time.Duration((stepOps+jitOps+retirementOps)*attempts) * (cfg.GitHub.RequestTimeout.Duration + maxJitteredBackoff)
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

// errReconcileStepAbandoned is returned by runControllerLoop when a
// watchdog-timed-out Step goroutine fails to release Reconciler.stepMu within
// its drain grace period. Step holds stepMu for its entire duration, so if
// the blocked adapter call inside it did not actually abort on context
// cancellation (the exact half-open/stuck-operation scenario the watchdog
// exists to survive), no later Step in this process can ever acquire stepMu
// again, and reconciler.Shutdown (which itself loops on Step and therefore
// also blocks on the same non-context-aware sync.Mutex) would hang behind it
// too instead of draining cleanly.
//
// Returning this error, rather than skipping reconcile ticks indefinitely,
// lets the process exit non-zero instead of idling forever in a permanently
// degraded, un-reconciling state. The ci-runner-fleet Scheduled Task is
// registered with a restart-on-failure policy (RestartCount 3,
// RestartInterval 1 minute; see provisioning's
// Register-CiRunnerLogonTask/Get-LogonTaskDrift), so process exit is also the
// only way to reclaim stepMu here: Go cannot kill a single goroutine, but the
// OS tears down the whole wedged process -- goroutine included -- on exit,
// and the scheduled task then starts a fresh one.
var errReconcileStepAbandoned = errors.New("reconcile step did not release its lock within the watchdog drain grace period")

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
				// abort on context cancellation. No future Step in this process can
				// ever acquire stepMu again, so escalate to a controlled process
				// exit instead of leaving the controller permanently stuck skipping
				// reconcile ticks: the scheduled task's restart-on-failure policy
				// starts a fresh process, which reclaims the lock because the OS
				// tears down this entire process -- goroutine included -- on exit.
				_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-watchdog-stuck", Message: fmt.Sprintf("reconcile step did not release its lock within %s of cancellation; exiting so the scheduled task restarts the controller", stepDrainGrace)})
				return errReconcileStepAbandoned
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
