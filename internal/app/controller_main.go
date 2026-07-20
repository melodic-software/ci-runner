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
// one r.statistics, each a full RetryValue budget -- PLUS, when the persisted
// scale set was deleted externally, r.statistics's own not-found recovery
// path (internal/controller/reconciler.go's statistics function): a second
// r.ensure call to recreate this host's identity, then a second r.statistics
// call against the recreated identity, each again a full RetryValue budget.
// That recovery path is not a rare/unbounded retry -- it fires at most once
// per target per Step (statistics only recurses one level after a single
// ErrorNotFound) -- but it is a real, policy-compliant doubling of a target's
// worst-case retryable-call count that the budget must clear, or a Step
// legitimately recovering a deleted scale set for enough targets can exceed
// the watchdog even though every individual retry obeyed policy.
const reconcileStepOpsPerTarget = 4

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

// reconcileStepRegistrationCheckOpsPerWorker is how many retryable GitHub
// calls a Step makes per idle worker it verifies during its worst-case
// legitimate JIT-cancellation check: one RunnerRegistered call, a full
// RetryValue budget, mirroring the same policy-compliant retry/backoff shape
// as reconcileStepJITOpsPerWorker. Like retirement (and unlike JIT starts),
// the number of idle workers eligible for this check is not itself bounded
// by Resources.MaximumConcurrentWorkers — idle inventory accumulates
// independently of that cap — so reconciler.go's registration-check loop
// caps how many RunnerRegistered calls it actually issues in a single Step
// at Resources.MaximumConcurrentWorkers too, rotating which candidates get
// picked so deferred ones are eventually checked instead of starving.
const reconcileStepRegistrationCheckOpsPerWorker = 1

// reconcileStepIdleConfirmationWaitsPerWorker is how many
// Drain.IdleConfirmationWindow waits a Step incurs per worker it retires in
// its worst-case legitimate registered-retirement sequence:
// reconciler.go's plan.Remove loop calls RemoveIfIdle once after a successful
// deregisterRunner, and the Docker runtime's RemoveIfIdle
// (internal/runtime/docker/runtime.go) waits the full configured
// Drain.IdleConfirmationWindow before its second idle check. That wait is not
// a retryable GitHub operation -- it is a fixed local wait, unaffected by
// RequestTimeout, backoff, or attempts -- so it is budgeted additively
// alongside the desktop lifecycle budget below rather than folded into the
// margined GitHub retry budget, and sized per the same
// retirementDeregistrationCap this Step's registered retirements already
// share (max(Resources.MaximumConcurrentWorkers, 1)).
const reconcileStepIdleConfirmationWaitsPerWorker = 1

// reconcileStepUnregisteredRemovalIdleConfirmationWaitsPerWorker is how many
// ADDITIONAL Drain.IdleConfirmationWindow waits a Step incurs through its
// SEPARATE unregistered-removal path: reconciler.go's plan.Remove loop calls
// RemoveIfIdle directly (with no deregisterRunner call first) for every
// worker whose State is model.WorkerUnregistered, and the Docker runtime's
// RemoveIfIdle waits the same full Drain.IdleConfirmationWindow described in
// reconcileStepIdleConfirmationWaitsPerWorker's doc comment. A worker can
// only ever become model.WorkerUnregistered through reconciler.go's
// registration-check loop (the JIT-cancellation detector, a few dozen lines
// above the removal loop in step()), which is itself capped at
// registrationCheckCap -- the same max(Resources.MaximumConcurrentWorkers, 1)
// formula as retirementDeregistrationCap -- and that loop is the ONLY place
// in the codebase that ever assigns model.WorkerUnregistered: the live
// Docker inventory (Workers.List) never reports it as a container state, and
// BuildPlan never consults Previous.Workers when building plan.Remove, so no
// worker can carry that state into plan.Remove from an earlier Step. This
// path's worst-case legitimate RemoveIfIdle-driven idle-confirmation-wait
// count per Step is therefore ALREADY bounded by registrationCheckCap by
// construction, with no separate cap needed in reconciler.go itself.
//
// A single Step can legitimately need both this path's confirmation waits
// AND reconcileStepIdleConfirmationWaitsPerWorker's registered-retirement
// confirmation waits back to back -- missing registrations and excess
// registered idle workers can both be present in the same reconcile -- so
// this is budgeted ADDITIVELY alongside, not instead of, that budget: before
// this constant existed, the watchdog budgeted only one cap's worth of
// confirmation waits, so a Step legitimately needing both could spend
// roughly twice its budgeted idle-confirmation time and get canceled
// mid-drain.
const reconcileStepUnregisteredRemovalIdleConfirmationWaitsPerWorker = 1

// reconcileStepDesktopStartAttempts upper-bounds how many DesktopManager.Start
// calls a single Step could make. reconciler.go has two Start call sites: an
// eager bootstrap before resource admission and inventory (the observation
// section), and BuildPlan's plan.StartDesktop fallback. Both can fire within one
// Step: a failed eager Start sets observationFailed, but the resource snapshot is
// preserved when the desktop is intentionally stopped, and BuildPlan assigns
// plan.StartDesktop ahead of its resource gate, so a blocked gate no longer
// suppresses the fallback (see reconciler.go's desktopIntentionallyStopped guard
// and BuildPlan's StartDesktop-before-resource-gate ordering in plan.go). Summing
// both verified call sites is the robust way to size a coarse backstop: it stays
// correct regardless of that control flow, and — per reconcileStepTimeout's doc
// comment — a larger backstop has no downside.
const reconcileStepDesktopStartAttempts = 2

// reconcileStepWorkerImagePullBudget bounds the one Docker image pull a
// Step's worker-start section can incur. reconciler.go's plan.Start loop
// calls Workers.Start once per worker it starts, and the Docker runtime
// implementation's Start (internal/runtime/docker/runtime.go) calls
// ensureImage first, which pulls the configured worker image only when
// ImageInspect reports it missing (a first-run host, or after the pinned
// digest changes). ensureImage now bounds that pull with its own configured
// WorkerImage.PullTimeout (applied via context.WithTimeout around the
// ImagePull+Wait sequence, mirroring how ControllerDesktopAdapter.Start/Stop
// bound Docker Desktop's lifecycle with DockerDesktop.StartTimeout/
// StopTimeout), so this term is now an exact derived bound read straight from
// that runtime policy value, the same way desktopBudget derives from
// DockerDesktop.StartTimeout/StopTimeout below -- not a generous guess.
//
// This is budgeted as a single occurrence, not scaled by worker count:
// plan.Start's loop issues Workers.Start calls one at a time, never
// concurrently, and ensureImage's own ImageInspect check means only the
// FIRST Start call in a Step that finds the image missing actually pulls it
// -- Docker has the image on disk for every subsequent Start call in that
// same Step.
func reconcileStepWorkerImagePullBudget(cfg config.Config) time.Duration {
	return cfg.WorkerImage.PullTimeout.Duration
}

// reconcileStepJITBudgetFloorWorkers floors the worker count the JIT-start
// portion of the budget is sized from, in addition to (not instead of) the
// live effectiveMaxConcurrentWorkers value the caller supplies.
//
// effectiveMaxConcurrentWorkers is read from Desired.TemporaryCapacityOverride
// (via Reconciler.EffectiveMaximumConcurrentWorkers) immediately before this
// function is called, and validation places no upper bound on that override
// -- only a non-negative check (internal/state/fs/store.go's SaveDesired,
// internal/app's parseCapacity). That snapshot can go stale in two distinct
// ways once Step actually starts running: (1) an ordinary timing race between
// this read and step()'s own LoadDesired a moment later, and (2) far more
// significantly, step()'s own errReconcileInputsChanged retry path
// (reconciler.go's watchSafetyInputs and freshStartAllowed both cancel with
// that cause the instant they observe the desired state has changed, and
// Step's outer loop immediately re-runs step() under the SAME
// context.WithTimeout deadline this function computed for the OLD snapshot):
// an operator legitimately raising the override at any point during a
// long-running Step causes the re-run to need MORE JIT-start budget than was
// sized, against a deadline that cannot grow to compensate.
//
// No term computed purely from the pre-Step snapshot can close that class of
// gap: the override can change to any non-negative value after the snapshot
// is taken, so no snapshot-derived exact term is staleness-immune by
// construction. Rather than adding yet another itemized term chasing this
// (each prior fix in this budget's history has surfaced another snapshot-
// sensitive gap the same way), floor the JIT-start worker count at a value
// generous enough to absorb any realistic single-host operator burst --
// concurrent Windows/Docker CI worker containers on one host are practically
// bounded by CPU and memory long before reaching this floor -- so an override
// raised mid-Step, within that realistic envelope, cannot exceed the budget
// regardless of exactly when the snapshot was taken. This does not achieve
// mathematical staleness-immunity against an unbounded override (that would
// require either a real product ceiling on TemporaryCapacityOverride or a
// fundamentally different watchdog design that resizes as Step runs, both
// out of scope here); it trades that for a floor that comfortably covers
// every operationally realistic case, consistent with this function's
// existing "a larger backstop has no downside" principle.
const reconcileStepJITBudgetFloorWorkers = 64

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
// attempts. BuildPlan bounds workers started (and therefore JIT registrations)
// within a single Step by the EFFECTIVE host limit -- the desired state's
// TemporaryCapacityOverride when an operator has set one, otherwise the
// static configured Resources.MaximumConcurrentWorkers (see
// controller.EffectiveMaximumConcurrentWorkers in internal/controller/plan.go)
// -- not the static cap alone: a legitimate temporary scale-up can
// authorize starting far more workers in one Step than the static cap would
// suggest. The caller therefore passes the effective limit in as
// effectiveMaxConcurrentWorkers (queried fresh before every Step via
// Reconciler.EffectiveMaximumConcurrentWorkers), and this budgets
// reconcileStepJITOpsPerWorker retryable operations per unit of that limit,
// using the same per-attempt cap and attempt count, adding it to the sweep
// budget before the 50% margin for remaining per-worker provisioning work.
//
// Step's worker-removal section symmetrically calls deregisterRunner (a
// RemoveRunner call run through the same RetryValue budget) once per idle
// worker it retires from plan.Remove, before RemoveIfIdle
// (internal/controller/reconciler.go:603-609). Unlike JIT starts, that
// retirement count is capped by reconciler.go's removal loop at the STATIC
// Resources.MaximumConcurrentWorkers regardless of any temporary override --
// plan.Remove can legitimately carry more idle workers than that cap allows
// for after MaximumConcurrentWorkers or warm capacity is lowered, since
// existing workers are drained rather than force-dropped, deferring any
// remainder to a later Step. Budget reconcileStepRetirementOpsPerWorker
// retryable operations per unit of the static cap, using the same per-attempt
// cap and attempt count, mirroring the JIT budget above.
//
// Step's registration-check section (the JIT-cancellation detector) calls
// RunnerRegistered once per idle, job-free worker it verifies, likewise
// through a full RetryValue budget and likewise capped by reconciler.go at
// the STATIC Resources.MaximumConcurrentWorkers, not any temporary override.
// Budget reconcileStepRegistrationCheckOpsPerWorker retryable operations per
// unit of that same static cap.
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
// GitHub retries. Add reconcileStepWorkerImagePullBudget(cfg) alongside it: a
// separate, likewise exactly-bounded Docker operation (see its doc comment
// for why it derives directly from WorkerImage.PullTimeout instead of a
// computed retry term).
//
// Registered retirements also incur a Drain.IdleConfirmationWindow wait each
// (see reconcileStepIdleConfirmationWaitsPerWorker's doc comment): a fixed
// local wait, not a retryable GitHub operation, so it is added directly
// alongside the desktop budget rather than folded into the margined GitHub
// retry budget, sized per the same static retirement cap. Step's SEPARATE
// unregistered-removal path incurs the same kind of wait per worker it
// removes (see reconcileStepUnregisteredRemovalIdleConfirmationWaitsPerWorker's
// doc comment), sized per the same static cap, and is budgeted additively
// alongside the registered-retirement idle-confirmation budget: a Step can
// legitimately need both in the same reconcile.
//
// jitOps floors effectiveMaxConcurrentWorkers at reconcileStepJITBudgetFloorWorkers
// (see that constant's doc comment) rather than just at 1: the live value can
// go stale between the pre-Step snapshot and Step's own desired-state reload,
// including across an errReconcileInputsChanged re-run of step() under this
// same deadline, so the JIT-start term uses the LARGER of the live value and
// the generous floor instead of the live value alone.
//
// Every term below is computed with saturating arithmetic (see
// saturatingMulInt, saturatingAddInt, saturatingScaleDuration, and
// saturatingAddDuration) rather than bare + and *. jitOps scales with
// effectiveMaxConcurrentWorkers, which the caller sizes from
// controller.EffectiveMaximumConcurrentWorkers -- an operator-set
// Desired.TemporaryCapacityOverride when one is set. Current validation
// (internal/state/fs/store.go's SaveDesired, internal/app's parseCapacity)
// only rejects a NEGATIVE override; a legitimate (if operationally silly)
// very large override must not silently overflow this arithmetic into a
// negative or near-zero time.Duration, which would violate this function's
// one correctness requirement (never interrupt a legitimate Step) far worse
// than any of the scenarios the budget above defends against: it would
// cancel EVERY reconcile immediately, not just an unusually slow one.
// Saturating toward the largest representable time.Duration instead keeps
// every input legal while guaranteeing the result is always large and
// positive, consistent with "a larger backstop has no downside" above.
func reconcileStepTimeout(cfg config.Config, effectiveMaxConcurrentWorkers int) time.Duration {
	attempts := max(cfg.GitHub.Retry.MaxAttempts, reconcileStepMinRetryAttempts)
	staticWorkerCap := max(cfg.Resources.MaximumConcurrentWorkers, 1)
	stepOps := reconcileStepOpsPerTarget * max(len(cfg.GitHub.Targets), 1)
	jitOps := saturatingMulInt(reconcileStepJITOpsPerWorker, max(effectiveMaxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers))
	retirementOps := reconcileStepRetirementOpsPerWorker * staticWorkerCap
	registrationCheckOps := reconcileStepRegistrationCheckOpsPerWorker * staticWorkerCap
	totalOps := saturatingAddInt(stepOps, saturatingAddInt(jitOps, saturatingAddInt(retirementOps, registrationCheckOps)))
	totalRetryUnits := saturatingMulInt(totalOps, attempts)
	maxJitteredBackoff := cfg.GitHub.Retry.Maximum.Duration +
		time.Duration(float64(cfg.GitHub.Retry.Maximum.Duration)*cfg.GitHub.Retry.JitterRatio)
	perAttemptBudget := cfg.GitHub.RequestTimeout.Duration + maxJitteredBackoff
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

// saturatingMulInt multiplies two non-negative ints, clamping to math.MaxInt
// instead of silently wrapping negative on overflow, mirroring
// internal/controller/reconciler.go's saturatingAddUint64 for the
// signed-int, multiplicative case reconcileStepTimeout needs. See that
// function's doc comment for why this matters: an operator-set
// Desired.TemporaryCapacityOverride is validated only to be non-negative, so
// reconcileStepTimeout's op-count arithmetic must degrade to a saturated
// upper bound instead of a wrapped, possibly negative one.
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

// saturatingScaleDuration multiplies unit by a non-negative count, clamping
// to the largest representable time.Duration instead of overflowing int64
// nanoseconds. See saturatingMulInt.
func saturatingScaleDuration(unit time.Duration, count int) time.Duration {
	if unit <= 0 || count <= 0 {
		return 0
	}
	if int64(unit) > math.MaxInt64/int64(count) {
		return math.MaxInt64
	}
	return unit * time.Duration(count)
}

// saturatingAddDuration adds two non-negative durations, clamping to the
// largest representable time.Duration instead of overflowing int64
// nanoseconds. See saturatingMulInt.
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

// maxConsecutiveReconcileStepErrors bounds how many reconcile Steps may
// complete with an error in unbroken succession before the controller
// escalates to a controlled process exit. The Step watchdog only catches a
// Step that never returns; a Step that fails fast every cycle (a saturated
// jobs index, a wedged state lock) reconciles nothing for hours while the
// process looks alive — the completes-with-error outage class of #54/#93/#98.
// Both observed incidents recovered on process restart, and the scheduled
// task's restart-on-failure policy provides exactly that. At the default
// reconcile interval this threshold escalates after roughly ten minutes of
// continuous failure; any single clean Step resets the count, so ordinary
// transient errors and flapping adapters never approach it. Sized as a fixed
// multiple of the reconcile cadence rather than a new tunable, matching the
// watchdog-mechanism constants above.
const maxConsecutiveReconcileStepErrors = 20

// errReconcilePersistentlyFailing is returned by runControllerLoop when every
// recent reconcile Step completed with an error. Exiting non-zero (rather
// than degrading forever) hands recovery to the scheduled task's
// restart-on-failure policy: a fresh process reclaims any wedged in-process
// state (locks, leaked tokens), and a fresh controller re-adopts surviving
// worker containers at startup. Graceful shutdown is deliberately skipped —
// reconciler.Shutdown exercises the same failing subsystem and would either
// hang or fail the same way.
var errReconcilePersistentlyFailing = errors.New("every recent reconcile step completed with an error; exiting so the scheduled task restarts the controller")

// stepOutcome carries a completed Step's error together with whether its
// observation failure blocks all new work, so the loop can distinguish a
// globally wedged controller from a per-target error that leaves the
// remaining pools reconciling.
type stepOutcome struct {
	err            error
	newWorkBlocked bool
}

// reconcileFailureStreak counts consecutive Steps that completed with an
// error while all new work was blocked; any other outcome — a clean Step or
// a partial per-target failure — resets it. Keying on the blocked flag keeps
// one persistently misconfigured target (which the live controller retries
// harmlessly forever) from consuming the scheduled task's bounded restart
// budget. observe reports whether the streak has reached the escalation
// threshold.
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
		// The effective worker limit is queried fresh before every Step (rather
		// than once outside the loop) because Desired.TemporaryCapacityOverride
		// is dynamic operator state that can change between Steps: sizing the
		// JIT-start portion of the watchdog budget from a stale effective limit
		// could let a later legitimate temporary scale-up exceed the budget this
		// Step actually gets. See reconcileStepTimeout's doc comment.
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
			// The step overran its watchdog deadline. Cancel it and surface the
			// stall, then give the goroutine a bounded grace period to unwind and
			// release stepMu before deciding whether it is safe to start the next
			// Step: a wedged Step (for example a half-open listener long poll that
			// does not honor cancellation) must not let a fresh Step queue forever
			// behind the same held mutex.
			cancelStep()
			_ = logs.Write(context.Background(), controller.LogEvent{At: time.Now().UTC(), Code: "reconcile-watchdog-timeout", Message: fmt.Sprintf("reconcile step exceeded its %s watchdog deadline and was cancelled", stepTimeout)})
			// This inner select must stay responsive to shutdown/interrupt/server
			// signals just like every other select in this loop, but it must NOT
			// call shutdown() directly for a signal that arrives here: shutdown()'s
			// first action is reconciler.Shutdown, which itself calls Step and
			// would block on the very same stepMu this watchdog exists to survive
			// if the timed-out Step goroutine has not actually released it yet --
			// defeating the whole point of the bounded drain below. So a
			// shutdown-triggering signal observed during the drain window is
			// recorded but not acted on immediately; the code below waits for the
			// same outcome (stepMu released vs. still wedged past stepDrainGrace)
			// that an undirected watchdog timeout would, and only calls shutdown()
			// once that outcome confirms it is safe to do so.
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
			if haveSignal {
				// A shutdown-triggering signal arrived while the timed-out Step
				// might still hold stepMu. Keep waiting, bounded by the same drain
				// grace period, for the old Step to actually release it before
				// calling shutdown(); escalate to process exit (rather than
				// invoking reconciler.Shutdown and hanging behind the wedged Step)
				// if it does not.
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
	// A degraded drain (bounded escape after persistent Step errors) is
	// completable for restart only: the scheduled task starts a fresh controller
	// either way, and refusing the restart sentinel would strand the wedge the
	// escape exists to break. Every other shutdown flavor fails closed -- a
	// stop-for-update or process interrupt must not report an unverified drain
	// as a safe stop.
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
