package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
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
	for _, warning := range cfg.Warnings {
		logEvent("config-warning", warning)
	}
	fail := func(code string, err error) error {
		if err != nil {
			logEvent(code, err.Error())
		}
		return err
	}
	telemetryOptions := telemetry.Options{
		HostID: cfg.Host.ID, Version: buildinfo.Version,
		OnExportNotice: func(notice telemetry.ExportNotice) {
			switch notice.Kind {
			case telemetry.ExportNoticeUnreachable:
				if notice.Err != nil {
					logEvent("telemetry-export-unreachable", notice.Err.Error())
				}
			case telemetry.ExportNoticeDegradedSummary:
				message := "OTLP export still failing"
				if notice.Err != nil {
					message = notice.Err.Error()
				}
				if notice.Suppressed > 0 {
					message = fmt.Sprintf("%s (%d suppressed export failures since last notice)", message, notice.Suppressed)
				}
				logEvent("telemetry-export-degraded", message)
			case telemetry.ExportNoticeRestored:
				logEvent("telemetry-export-restored", "OTLP export resumed")
			default:
				if notice.Err != nil {
					logEvent("telemetry-export-error", notice.Err.Error())
				}
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
		ScaleSets:    scaleSets,
		Workers:      workers,
		Desktop:      host.NewControllerDesktopAdapter(),
		Power:        host.WindowsPowerMonitor{},
		Resources:    &host.WindowsResourceMonitor{},
		State:        store,
		Jobs:         jobs,
		Logs:         logs,
		Telemetry:    telemetryProvider,
		EngineMemory: host.NewEngineMemoryProbe(),
		ACL:          acl,
	})
	if err != nil {
		_ = workers.Close()
		_ = scaleSets.Close(context.Background())
		return fail("controller-construction-error", err)
	}
	processID := uint32(os.Getpid())
	handler, err := controller.NewControlHandler(reconciler, processID)
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
	err = runControllerLoop(ctx, cfg, reconciler, handler, server, logs, store, processID, buildinfo.Version)
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

// reconcileStepMinRetryAttempts floors maxAttempts so a configured 0 or 1 cannot collapse
// the Step deadline toward a single request.
const reconcileStepMinRetryAttempts = 3

// reconcileStepOpsPerTarget counts retryable calls per target per Step: ensure and statistics,
// doubled by statistics' one-level not-found recovery (re-ensure, re-statistics).
const reconcileStepOpsPerTarget = 4

// reconcileStepJITOpsPerWorker counts CreateJITConfig calls per started worker per Step: one
// in the pre-poll warm-pool pass, one in the post-poll assignment pass.
const reconcileStepJITOpsPerWorker = 2

// reconcileStepRetirementOpsPerWorker counts RemoveRunner calls per retired worker; it holds only
// because reconciler.go caps retirements per Step at MaximumConcurrentWorkers.
const reconcileStepRetirementOpsPerWorker = 1

// reconcileStepRegistrationCheckOpsPerWorker counts RunnerRegistered calls per checked idle worker;
// it holds only because reconciler.go caps checks per Step at MaximumConcurrentWorkers.
const reconcileStepRegistrationCheckOpsPerWorker = 1

// reconcileStepIdleConfirmationWaitsPerWorker counts IdleConfirmationWindow waits per registered
// retirement: a fixed local wait, budgeted outside the margined GitHub retry budget.
const reconcileStepIdleConfirmationWaitsPerWorker = 1

// reconcileStepUnregisteredRemovalIdleConfirmationWaitsPerWorker adds the same wait per
// unregistered removal; a Step can need both, and registrationCheckCap bounds this path.
const reconcileStepUnregisteredRemovalIdleConfirmationWaitsPerWorker = 1

// reconcileStepDesktopStartAttempts sums reconciler.go's two DesktopManager.Start call sites
// (eager bootstrap, BuildPlan's StartDesktop fallback); both can fire in one Step.
const reconcileStepDesktopStartAttempts = 2

// reconcileStepWorkerImagePullBudget budgets one pull, not one per worker: starts run serially and
// only the first Start that finds the image missing pulls it.
func reconcileStepWorkerImagePullBudget(cfg config.Config) time.Duration {
	return cfg.WorkerImage.PullTimeout.Duration
}

// reconcileStepJITBudgetFloorWorkers floors the JIT-start worker count because an operator can
// raise the capacity override mid-Step, after the budget was sized from a snapshot.
const reconcileStepJITBudgetFloorWorkers = 64

// reconcileStepTimeout is a coarse backstop that must never interrupt a legitimate Step, so it sums
// every worst-case term with saturating arithmetic; a larger backstop has no downside.
func reconcileStepTimeout(cfg config.Config, effectiveMaxConcurrentWorkers int) time.Duration {
	attempts := max(cfg.GitHub.Retry.MaxAttempts, reconcileStepMinRetryAttempts)
	staticWorkerCap := max(cfg.Resources.MaximumConcurrentWorkers, 1)
	stepOps := reconcileStepOpsPerTarget * max(len(cfg.GitHub.Targets), 1)
	jitOps := saturatingMulInt(reconcileStepJITOpsPerWorker, max(effectiveMaxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers))
	retirementOps := reconcileStepRetirementOpsPerWorker * staticWorkerCap
	registrationCheckOps := reconcileStepRegistrationCheckOpsPerWorker * staticWorkerCap
	totalOps := saturatingAddInt(stepOps, saturatingAddInt(jitOps, saturatingAddInt(retirementOps, registrationCheckOps)))
	totalRetryUnits := saturatingMulInt(totalOps, attempts)
	perAttemptBudget := cfg.GitHub.RequestTimeout.Duration + maxJitteredBackoff(cfg)
	retryBudget := saturatingScaleDuration(perAttemptBudget, totalRetryUnits)
	githubBudget := saturatingAddDuration(retryBudget, retryBudget/2)
	desktopBudget := saturatingAddDuration(
		saturatingAddDuration(
			saturatingScaleDuration(cfg.DockerDesktop.StartTimeout.Duration, reconcileStepDesktopStartAttempts),
			cfg.DockerDesktop.StopTimeout.Duration,
		),
		reconcileStepWorkerImagePullBudget(cfg),
	)
	idleConfirmationWaits := saturatingMulInt(reconcileStepIdleConfirmationWaitsPerWorker+reconcileStepUnregisteredRemovalIdleConfirmationWaitsPerWorker, staticWorkerCap)
	idleConfirmationBudget := saturatingScaleDuration(cfg.Drain.IdleConfirmationWindow.Duration, idleConfirmationWaits)
	return saturatingAddDuration(saturatingAddDuration(githubBudget, desktopBudget), idleConfirmationBudget)
}

// maxJitteredBackoff is the longest policy-compliant backoff: jitter applies after the cap, so size
// deadlines from this, never from Retry.Maximum.
func maxJitteredBackoff(cfg config.Config) time.Duration {
	return cfg.GitHub.Retry.Maximum.Duration +
		time.Duration(float64(cfg.GitHub.Retry.Maximum.Duration)*cfg.GitHub.Retry.JitterRatio)
}

// saturatingMulInt clamps to math.MaxInt instead of wrapping: the capacity override that feeds
// reconcileStepTimeout is validated only as non-negative.
func saturatingMulInt(left, right int) int {
	if left <= 0 || right <= 0 {
		return 0
	}
	product := left * right
	if product/left != right {
		return math.MaxInt
	}
	return product
}

// saturatingAddInt adds two non-negative ints, clamping to math.MaxInt
// instead of wrapping negative on overflow. See saturatingMulInt.
func saturatingAddInt(left, right int) int {
	if left < 0 || right < 0 {
		return 0
	}
	sum := left + right
	if sum < left {
		return math.MaxInt
	}
	return sum
}

// saturatingScaleDuration clamps to the largest time.Duration instead of overflowing.
func saturatingScaleDuration(unit time.Duration, count int) time.Duration {
	if unit <= 0 || count <= 0 {
		return 0
	}
	if int64(unit) > math.MaxInt64/int64(count) {
		return math.MaxInt64
	}
	return unit * time.Duration(count)
}

// saturatingAddDuration clamps to the largest time.Duration instead of overflowing.
func saturatingAddDuration(left, right time.Duration) time.Duration {
	if left < 0 || right < 0 {
		return 0
	}
	sum := left + right
	if sum < left {
		return math.MaxInt64
	}
	return sum
}

// reconcileStepDrainGrace bounds the wait for a cancelled Step to release stepMu: one in-flight call
// or backoff, plus the detached observed-state writes that outlive cancellation.
func reconcileStepDrainGrace(cfg config.Config) time.Duration {
	return saturatingAddDuration(
		saturatingAddDuration(cfg.GitHub.RequestTimeout.Duration, cfg.GitHub.Retry.Maximum.Duration),
		controller.StepDetachedPersistDrain,
	)
}

// errReconcileStepAbandoned exits the process when a timed-out Step keeps stepMu: only exit frees
// it, and the scheduled task's restart-on-failure policy starts a fresh controller.
var errReconcileStepAbandoned = errors.New("reconcile step did not release its lock within the watchdog drain grace period")

// maxConsecutiveReconcileStepErrors is roughly ten minutes of failure at the default interval;
// one clean Step resets the count.
const maxConsecutiveReconcileStepErrors = 20

// errReconcilePersistentlyFailing skips graceful shutdown: reconciler.Shutdown exercises the
// failing subsystem and would hang or fail the same way.
var errReconcilePersistentlyFailing = errors.New("every recent reconcile step completed with an error; exiting so the scheduled task restarts the controller")

// stepOutcome separates a Step that blocked all new work from a per-target error.
type stepOutcome struct {
	err            error
	newWorkBlocked bool
}

// reconcileFailureStreak counts only failures that blocked all new work, so one misconfigured
// target cannot spend the scheduled task's bounded restart budget.
type reconcileFailureStreak struct{ count int }

func (s *reconcileFailureStreak) observe(outcome stepOutcome) bool {
	if outcome.err == nil || !outcome.newWorkBlocked {
		s.count = 0
		return false
	}
	s.count++
	return s.count >= maxConsecutiveReconcileStepErrors
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
	go reconciler.WatchHeartbeat(serverContext)

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

	stepDrainGrace := reconcileStepDrainGrace(cfg)
	var failureStreak reconcileFailureStreak
	noteStepOutcome := func(outcome stepOutcome) error {
		if !failureStreak.observe(outcome) {
			return nil
		}
		_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-persistent-failure", Message: fmt.Sprintf("%d consecutive reconcile steps completed with an error while all new work was blocked; exiting so the scheduled task restarts the controller", failureStreak.count)})
		return errReconcilePersistentlyFailing
	}
	for {
		// Re-queried every Step: the capacity override can change between Steps and sizes the JIT budget.
		stepTimeout := reconcileStepTimeout(cfg, reconciler.EffectiveMaximumConcurrentWorkers(ctx))
		stepContext, cancelStep := context.WithTimeout(context.Background(), stepTimeout)
		stepDone := make(chan stepOutcome, 1)
		go func() {
			result, stepErr := reconciler.Step(stepContext)
			stepDone <- stepOutcome{err: stepErr, newWorkBlocked: result.NewWorkBlocked}
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
			// Bounded grace for the cancelled Step to release stepMu, so the next Step never queues behind
			// a wedged one.
			cancelStep()
			_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-watchdog-timeout", Message: fmt.Sprintf("reconcile step exceeded its %s watchdog deadline and was cancelled", stepTimeout)})
			// Never call shutdown() in this select: reconciler.Shutdown runs Step and would block on the
			// stepMu being waited out. Record the signal and act once the Step has released.
			var (
				pendingSignal      controller.ShutdownSignal
				pendingAwaitServer bool
				pendingJoinErr     error
				haveSignal         bool
			)
			select {
			case signal := <-handler.ShutdownRequests():
				pendingSignal, pendingAwaitServer, haveSignal = signal, true, true
			case <-ctx.Done():
				pendingSignal = controller.ShutdownSignal{Reason: "process interrupt"}
				pendingAwaitServer = true
				haveSignal = true
			case serveErr := <-serverErrors:
				if serveErr == nil {
					serveErr = errors.New("control server exited unexpectedly")
				}
				pendingSignal = controller.ShutdownSignal{Reason: "control server exited unexpectedly"}
				pendingAwaitServer = false
				pendingJoinErr = serveErr
				haveSignal = true
			case outcome := <-stepDone:
				if outcome.err != nil {
					_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-error", Message: outcome.err.Error()})
				}
				if escalate := noteStepOutcome(outcome); escalate != nil {
					return escalate
				}
			case <-time.After(stepDrainGrace):
				// The Step never released stepMu; only process exit frees it.
				_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-watchdog-stuck", Message: fmt.Sprintf("reconcile step did not release its lock within %s of cancellation; exiting so the scheduled task restarts the controller", stepDrainGrace)})
				return errReconcileStepAbandoned
			}
			if haveSignal {
				// The Step may still hold stepMu: wait the same grace before shutdown(), else exit.
				select {
				case outcome := <-stepDone:
					if outcome.err != nil {
						_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-error", Message: outcome.err.Error()})
					}
					if pendingJoinErr != nil {
						return errors.Join(pendingJoinErr, shutdown(pendingSignal, pendingAwaitServer))
					}
					return shutdown(pendingSignal, pendingAwaitServer)
				case <-time.After(stepDrainGrace):
					_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-watchdog-stuck", Message: fmt.Sprintf("reconcile step did not release its lock within %s of cancellation; exiting so the scheduled task restarts the controller", stepDrainGrace)})
					return errReconcileStepAbandoned
				}
			}
		case outcome := <-stepDone:
			cancelStep()
			if outcome.err != nil {
				_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-error", Message: outcome.err.Error()})
			}
			if escalate := noteStepOutcome(outcome); escalate != nil {
				return escalate
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

// completeControllerShutdown writes the restart receipt; any earlier failure returns without the
// restart sentinel so the CLI fails closed.
func completeControllerShutdown(
	ctx context.Context,
	shutdownErr error,
	signal controller.ShutdownSignal,
	restartReceipts restartReceiptWriter,
	processID uint32,
	version string,
) error {
	// A degraded drain may still complete a restart, since the task restarts the controller either
	// way; every other shutdown error fails closed.
	if shutdownErr != nil && (!signal.Restart || !errors.Is(shutdownErr, controller.ErrShutdownDegraded)) {
		return shutdownErr
	}
	if !signal.Restart {
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
