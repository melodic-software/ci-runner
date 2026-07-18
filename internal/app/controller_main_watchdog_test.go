package app

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
)

func githubRetryConfig(requestTimeout, backoffMax time.Duration, maxAttempts, targets, maxConcurrentWorkers int) config.Config {
	return desktopLifecycleConfig(requestTimeout, backoffMax, maxAttempts, targets, maxConcurrentWorkers, 0, 0)
}

func desktopLifecycleConfig(requestTimeout, backoffMax time.Duration, maxAttempts, targets, maxConcurrentWorkers int, desktopStart, desktopStop time.Duration) config.Config {
	return config.Config{
		GitHub: config.GitHub{
			RequestTimeout: config.Duration{Duration: requestTimeout},
			Retry:          config.Retry{Maximum: config.Duration{Duration: backoffMax}, MaxAttempts: maxAttempts},
			Targets:        make([]config.Target, targets),
		},
		Resources:     config.Resources{MaximumConcurrentWorkers: maxConcurrentWorkers},
		DockerDesktop: config.DockerDesktop{StartTimeout: config.Duration{Duration: desktopStart}, StopTimeout: config.Duration{Duration: desktopStop}},
		// Matches the recommended production default (reconcileStepWorkerImagePullBudget's
		// doc comment), so exact-equality assertions below are unaffected by this field's
		// introduction: they exercise the fixed-term contribution the same way regardless
		// of whether it comes from a constant or, now, this configured value.
		WorkerImage: config.WorkerImage{PullTimeout: config.Duration{Duration: 20 * time.Minute}},
	}
}

func TestReconcileStepTimeoutClearsConfiguredRetryBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		requestTO            time.Duration
		backoffMax           time.Duration
		jitterRatio          float64
		maxAttempts          int
		targets              int
		maxConcurrentWorkers int
	}{
		{name: "golden single target", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 6, targets: 1, maxConcurrentWorkers: 1},
		{name: "high maxAttempts", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 40, targets: 1, maxConcurrentWorkers: 1},
		{name: "large backoff", requestTO: 30 * time.Second, backoffMax: 5 * time.Minute, maxAttempts: 10, targets: 1, maxConcurrentWorkers: 1},
		{name: "multi-target", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 6, targets: 3, maxConcurrentWorkers: 1},
		{name: "multi-worker JIT", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 6, targets: 1, maxConcurrentWorkers: 8},
		{name: "multi-target and multi-worker JIT", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 6, targets: 3, maxConcurrentWorkers: 8},
		{name: "fully jittered backoff", requestTO: 70 * time.Second, backoffMax: time.Minute, jitterRatio: 1, maxAttempts: 6, targets: 1, maxConcurrentWorkers: 1},
		{name: "multi-target, multi-worker JIT, and fully jittered backoff", requestTO: 70 * time.Second, backoffMax: time.Minute, jitterRatio: 1, maxAttempts: 6, targets: 3, maxConcurrentWorkers: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := githubRetryConfig(tc.requestTO, tc.backoffMax, tc.maxAttempts, tc.targets, tc.maxConcurrentWorkers)
			cfg.GitHub.Retry.JitterRatio = tc.jitterRatio
			got := reconcileStepTimeout(cfg, tc.maxConcurrentWorkers)
			// The watchdog must strictly exceed the whole-step worst case: an
			// ensure+statistics sweep across every target, a CreateJITConfig retry
			// loop for every worker the host can concurrently start, and a
			// deregisterRunner retry loop for every worker the host can
			// concurrently retire in one step, each a full retry budget (attempts
			// requests at RequestTimeout plus attempts backoff waits jittered up to
			// Retry.Maximum*(1+JitterRatio) per internal/controller/retry.go's
			// BackoffPolicy.delay). It must never trip on a legitimate multi-target,
			// high-maxAttempts, multi-worker-JIT, or fully-jittered-backoff step.
			ops := reconcileStepOpsPerTarget*tc.targets + reconcileStepJITOpsPerWorker*tc.maxConcurrentWorkers + reconcileStepRetirementOpsPerWorker*tc.maxConcurrentWorkers
			maxJitteredBackoff := tc.backoffMax + time.Duration(float64(tc.backoffMax)*tc.jitterRatio)
			budget := time.Duration(ops*tc.maxAttempts) * (tc.requestTO + maxJitteredBackoff)
			if got <= budget {
				t.Fatalf("reconcileStepTimeout = %s, want > whole-step retry budget %s (targets=%d, maxAttempts=%d, maxConcurrentWorkers=%d, jitterRatio=%v)", got, budget, tc.targets, tc.maxAttempts, tc.maxConcurrentWorkers, tc.jitterRatio)
			}
		})
	}
}

// TestReconcileStepTimeoutAccountsForJitteredBackoff proves the exact
// regression a reviewer flagged: the watchdog budget must not assume
// Retry.Maximum is already the maximum possible per-attempt sleep.
// internal/controller/retry.go's BackoffPolicy.delay applies jitter after
// capping the base delay to Maximum, drawing uniformly from
// [1-JitterRatio, 1+JitterRatio], so with a config where backoff dominates
// request time (a small RequestTimeout, a large Retry.Maximum) and
// JitterRatio at its validated ceiling of 1, a single policy-compliant wait
// can reach nearly 2x Retry.Maximum -- exceeding the watchdog's 50% margin if
// the budget were sized from bare Retry.Maximum alone.
func TestReconcileStepTimeoutAccountsForJitteredBackoff(t *testing.T) {
	t.Parallel()
	const requestTO = time.Second
	const backoffMax = time.Minute
	const attempts = reconcileStepMinRetryAttempts
	const targets = 1
	const maxConcurrentWorkers = 1

	unjittered := githubRetryConfig(requestTO, backoffMax, attempts, targets, maxConcurrentWorkers)
	fullyJittered := unjittered
	fullyJittered.GitHub.Retry.JitterRatio = 1

	baseline := reconcileStepTimeout(unjittered, maxConcurrentWorkers)
	got := reconcileStepTimeout(fullyJittered, maxConcurrentWorkers)

	if got <= baseline {
		t.Fatalf("reconcileStepTimeout with jitterRatio=1 = %s, want > jitterRatio=0 baseline %s", got, baseline)
	}

	// At jitterRatio=1, every retryable op's worst-case per-attempt backoff
	// grows from bare Maximum to Maximum*(1+1) = 2x Maximum: exactly one extra
	// Maximum per op per attempt, scaled by the watchdog's 1.5x margin. JIT
	// ops are floored at reconcileStepJITBudgetFloorWorkers (see its doc
	// comment), not just at maxConcurrentWorkers, since maxConcurrentWorkers=1
	// here is far below that floor.
	ops := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*max(maxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers) + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers + reconcileStepRegistrationCheckOpsPerWorker*maxConcurrentWorkers
	extraRetryBudget := time.Duration(ops*attempts) * backoffMax
	wantDelta := extraRetryBudget + extraRetryBudget/2
	if diff := got - baseline; diff != wantDelta {
		t.Fatalf("reconcileStepTimeout delta across jitterRatio 0->1 = %s, want exactly %s", diff, wantDelta)
	}

	// Concretely: the watchdog must clear a single worst-case jittered attempt
	// (RequestTimeout plus a backoff wait of nearly 2x Retry.Maximum), which a
	// budget sized from bare Retry.Maximum could fail to do once its 50% margin
	// is spent elsewhere.
	worstCaseJitteredAttempt := requestTO + 2*backoffMax
	if got <= worstCaseJitteredAttempt {
		t.Fatalf("reconcileStepTimeout = %s, want > single worst-case jittered attempt delay %s", got, worstCaseJitteredAttempt)
	}
}

// TestReconcileStepTimeoutIncludesRetirementRetryBudget proves the other
// regression a reviewer flagged: Step's worker-removal section calls
// deregisterRunner (internal/controller/reconciler.go:603-609) through the
// same full RetryValue budget as a JIT registration, once per idle worker it
// retires in a Step, up to Resources.MaximumConcurrentWorkers (see that
// loop's retirementDeregistrationCap). The watchdog must budget for that
// retirement work in addition to, not instead of, the JIT-start budget.
func TestReconcileStepTimeoutIncludesRetirementRetryBudget(t *testing.T) {
	t.Parallel()
	const requestTO = 70 * time.Second
	const backoffMax = time.Minute
	const attempts = 6
	const targets = 1
	const maxConcurrentWorkers = 4

	cfg := githubRetryConfig(requestTO, backoffMax, attempts, targets, maxConcurrentWorkers)
	got := reconcileStepTimeout(cfg, maxConcurrentWorkers)

	// The pre-fix formula budgeted only the target sweep plus JIT starts. Any
	// retirement contribution must be strictly additional to that. JIT ops are
	// floored at reconcileStepJITBudgetFloorWorkers (see its doc comment), not
	// just at maxConcurrentWorkers, since maxConcurrentWorkers=4 here is far
	// below that floor.
	preFixOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*max(maxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers)
	preFixRetryBudget := time.Duration(preFixOps*attempts) * (requestTO + backoffMax)
	preFixGithubBudget := preFixRetryBudget + preFixRetryBudget/2
	if got <= preFixGithubBudget {
		t.Fatalf("reconcileStepTimeout = %s, want > pre-fix (JIT-only) github budget %s once retirement retries are budgeted", got, preFixGithubBudget)
	}

	fullOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*max(maxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers) + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers + reconcileStepRegistrationCheckOpsPerWorker*maxConcurrentWorkers
	fullRetryBudget := time.Duration(fullOps*attempts) * (requestTO + backoffMax)
	// desktopStart/desktopStop are both 0 via githubRetryConfig, so the only
	// desktop-category contribution is the unconditional
	// reconcileStepWorkerImagePullBudget(cfg) term, derived from cfg.WorkerImage.PullTimeout.
	want := fullRetryBudget + fullRetryBudget/2 + reconcileStepWorkerImagePullBudget(cfg)
	if got != want {
		t.Fatalf("reconcileStepTimeout = %s, want exactly %s (target sweep + JIT starts + retirements + registration checks, all margined 1.5x, plus the configured image-pull term)", got, want)
	}
}

// TestReconcileStepTimeoutIncludesRegistrationCheckRetryBudget proves a third
// reviewer-flagged regression: Step's registration-check section calls
// RunnerRegistered (internal/controller/reconciler.go's JIT-cancellation
// detector) through the same full RetryValue budget as a JIT registration,
// once per idle worker it verifies in a Step, up to
// Resources.MaximumConcurrentWorkers (see that loop's registrationCheckCap).
// The watchdog must budget for that verification work in addition to, not
// instead of, the JIT-start and retirement budgets.
func TestReconcileStepTimeoutIncludesRegistrationCheckRetryBudget(t *testing.T) {
	t.Parallel()
	const requestTO = 70 * time.Second
	const backoffMax = time.Minute
	const attempts = 6
	const targets = 1
	const maxConcurrentWorkers = 4

	cfg := githubRetryConfig(requestTO, backoffMax, attempts, targets, maxConcurrentWorkers)
	got := reconcileStepTimeout(cfg, maxConcurrentWorkers)

	// The pre-fix formula budgeted the target sweep plus JIT starts and
	// retirements. Any registration-check contribution must be strictly
	// additional to that. JIT ops are floored at
	// reconcileStepJITBudgetFloorWorkers (see its doc comment), not just at
	// maxConcurrentWorkers, since maxConcurrentWorkers=4 here is far below
	// that floor.
	preFixOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*max(maxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers) + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers
	preFixRetryBudget := time.Duration(preFixOps*attempts) * (requestTO + backoffMax)
	preFixGithubBudget := preFixRetryBudget + preFixRetryBudget/2
	if got <= preFixGithubBudget {
		t.Fatalf("reconcileStepTimeout = %s, want > pre-fix (JIT+retirement) github budget %s once registration checks are budgeted", got, preFixGithubBudget)
	}

	fullOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*max(maxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers) + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers + reconcileStepRegistrationCheckOpsPerWorker*maxConcurrentWorkers
	fullRetryBudget := time.Duration(fullOps*attempts) * (requestTO + backoffMax)
	// desktopStart/desktopStop are both 0 via githubRetryConfig, so the only
	// desktop-category contribution is the unconditional
	// reconcileStepWorkerImagePullBudget(cfg) term, derived from cfg.WorkerImage.PullTimeout.
	want := fullRetryBudget + fullRetryBudget/2 + reconcileStepWorkerImagePullBudget(cfg)
	if got != want {
		t.Fatalf("reconcileStepTimeout = %s, want exactly %s (target sweep + JIT starts + retirements + registration checks, all margined 1.5x, plus the configured image-pull term)", got, want)
	}
}

// TestReconcileStepTimeoutAccountsForDesktopLifecycleTimeouts proves the exact
// regression the fix addresses: with a small, otherwise-legal GitHub retry
// configuration (one target, one worker, 1s request timeout, 1s max backoff),
// the GitHub-retry-only budget is tiny, but a policy-compliant
// DockerDesktop.StartTimeout can legitimately be minutes. The watchdog must
// budget for reconcileStepDesktopStartAttempts Start calls (and, separately,
// one Stop call) at their full configured timeouts on top of the GitHub-retry
// budget, not just the GitHub-retry budget alone.
func TestReconcileStepTimeoutAccountsForDesktopLifecycleTimeouts(t *testing.T) {
	t.Parallel()
	const startTimeout = 2 * time.Minute
	const stopTimeout = 90 * time.Second

	smallGitHubRetryCfg := desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, startTimeout, stopTimeout)
	githubOnlyBudget := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, 0, 0), 1)

	got := reconcileStepTimeout(smallGitHubRetryCfg, 1)

	// The watchdog must clear the GitHub-only budget by at least the desktop
	// lifecycle worst case: two Start attempts plus one Stop attempt, each at
	// its full configured timeout.
	desktopWorstCase := reconcileStepDesktopStartAttempts*startTimeout + stopTimeout
	if got < githubOnlyBudget+desktopWorstCase {
		t.Fatalf("reconcileStepTimeout = %s, want >= github-only budget %s + desktop worst case %s (= %s)",
			got, githubOnlyBudget, desktopWorstCase, githubOnlyBudget+desktopWorstCase)
	}

	// Concretely: the watchdog must never be shorter than a single
	// policy-compliant desktop start, which the original bug allowed (a small
	// GitHub retry budget could compute a watchdog deadline of only ~27s,
	// far under a 2-minute desktop startup).
	if got <= startTimeout {
		t.Fatalf("reconcileStepTimeout = %s, want > single desktop StartTimeout %s", got, startTimeout)
	}
}

// TestReconcileStepTimeoutScalesWithDesktopStartTimeout proves the desktop
// portion of the budget tracks DockerDesktop.StartTimeout, mirroring how
// TestReconcileStepTimeoutClearsConfiguredRetryBudget proves the GitHub
// portion tracks the retry configuration.
func TestReconcileStepTimeoutScalesWithDesktopStartTimeout(t *testing.T) {
	t.Parallel()
	shorter := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, time.Minute, 0), 1)
	longer := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, 10*time.Minute, 0), 1)
	if longer <= shorter {
		t.Fatalf("reconcileStepTimeout with 10m StartTimeout = %s, want > with 1m StartTimeout = %s", longer, shorter)
	}
	if diff, want := longer-shorter, reconcileStepDesktopStartAttempts*(10*time.Minute-time.Minute); diff != want {
		t.Fatalf("reconcileStepTimeout delta across StartTimeout change = %s, want exactly %s (%d start attempts)", diff, want, reconcileStepDesktopStartAttempts)
	}
}

// TestReconcileStepTimeoutScalesWithDesktopStopTimeout mirrors
// TestReconcileStepTimeoutScalesWithDesktopStartTimeout for
// DockerDesktop.StopTimeout.
func TestReconcileStepTimeoutScalesWithDesktopStopTimeout(t *testing.T) {
	t.Parallel()
	shorter := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, 0, time.Minute), 1)
	longer := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, 0, 10*time.Minute), 1)
	if longer <= shorter {
		t.Fatalf("reconcileStepTimeout with 10m StopTimeout = %s, want > with 1m StopTimeout = %s", longer, shorter)
	}
	if diff, want := longer-shorter, 10*time.Minute-time.Minute; diff != want {
		t.Fatalf("reconcileStepTimeout delta across StopTimeout change = %s, want exactly %s", diff, want)
	}
}

func TestReconcileStepTimeoutFloorsPathologicalMaxAttempts(t *testing.T) {
	t.Parallel()
	floored := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, reconcileStepMinRetryAttempts, 1, 1), 1)
	for _, attempts := range []int{0, 1} {
		if got := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, attempts, 1, 1), 1); got != floored {
			t.Fatalf("maxAttempts=%d: reconcileStepTimeout = %s, want floored (min %d attempts) %s", attempts, got, reconcileStepMinRetryAttempts, floored)
		}
	}
}

func TestReconcileStepTimeoutFloorsZeroTargets(t *testing.T) {
	t.Parallel()
	if got, want := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 0, 1), 1),
		reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1), 1); got != want {
		t.Fatalf("zero targets: reconcileStepTimeout = %s, want single-target floor %s", got, want)
	}
}

func TestReconcileStepTimeoutFloorsZeroMaxConcurrentWorkers(t *testing.T) {
	t.Parallel()
	if got, want := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 0), 0),
		reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1), 1); got != want {
		t.Fatalf("zero maxConcurrentWorkers: reconcileStepTimeout = %s, want single-worker floor %s", got, want)
	}
}

// TestReconcileStepTimeoutFloorsZeroEffectiveMaxConcurrentWorkers proves the
// JIT-start portion of the budget floors its effectiveMaxConcurrentWorkers
// argument independently of the static
// Resources.MaximumConcurrentWorkers-derived floor above, mirroring it for
// the override-aware parameter. Both 0 and 1 are far below
// reconcileStepJITBudgetFloorWorkers, so both collapse to that same
// generous floor (see its doc comment), not to a "single worker" value.
func TestReconcileStepTimeoutFloorsZeroEffectiveMaxConcurrentWorkers(t *testing.T) {
	t.Parallel()
	if got, want := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1), 0),
		reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1), 1); got != want {
		t.Fatalf("zero effectiveMaxConcurrentWorkers: reconcileStepTimeout = %s, want equal to effectiveMaxConcurrentWorkers=1's budget %s (both below reconcileStepJITBudgetFloorWorkers, so both collapse to the same floor)", got, want)
	}
}

// TestReconcileStepTimeoutSizesJITBudgetFromEffectiveOverride proves the fix
// for a reviewer-flagged watchdog gap: BuildPlan replaces the static
// Resources.MaximumConcurrentWorkers cap with Desired.TemporaryCapacityOverride
// when an operator has set one (internal/controller/plan.go's
// EffectiveMaximumConcurrentWorkers), and validation only rejects negative
// overrides -- a legitimate temporary scale-up can authorize starting far
// more workers in one Step than the static cap suggests. The JIT-start
// portion of the watchdog budget must scale with the caller-supplied
// effectiveMaxConcurrentWorkers argument (which the caller sizes from that
// same effective limit), not from cfg.Resources.MaximumConcurrentWorkers
// alone, or a policy-compliant burst reconcile authorized by the override
// gets its watchdog tripped by a budget sized only for the static cap.
func TestReconcileStepTimeoutSizesJITBudgetFromEffectiveOverride(t *testing.T) {
	t.Parallel()
	const requestTO = 70 * time.Second
	const backoffMax = time.Minute
	const attempts = 6
	const targets = 1
	const staticCap = 1
	// override must clear reconcileStepJITBudgetFloorWorkers (see its doc
	// comment) for this test to observe the override actually widening the
	// budget beyond the floor: a staticCap-vs-override comparison where both
	// sides are floored to the same value would show no delta at all.
	const override = reconcileStepJITBudgetFloorWorkers + 50

	cfg := githubRetryConfig(requestTO, backoffMax, attempts, targets, staticCap)
	staticBudget := reconcileStepTimeout(cfg, staticCap)
	overrideBudget := reconcileStepTimeout(cfg, override)

	if overrideBudget <= staticBudget {
		t.Fatalf("reconcileStepTimeout with effectiveMaxConcurrentWorkers=%d (override) = %s, want > effectiveMaxConcurrentWorkers=%d (static cap) budget %s", override, overrideBudget, staticCap, staticBudget)
	}

	// Concretely: only the JIT-start ops scale with the override; the
	// retirement and registration-check ops stay tied to the static cap
	// because reconciler.go's removal and registration-check loops cap
	// themselves at Resources.MaximumConcurrentWorkers regardless of any
	// temporary override. staticCap is below reconcileStepJITBudgetFloorWorkers,
	// so the static side of the delta is anchored at the floor, not staticCap.
	jitOpsDelta := reconcileStepJITOpsPerWorker * (override - reconcileStepJITBudgetFloorWorkers)
	wantDelta := time.Duration(jitOpsDelta*attempts) * (requestTO + backoffMax)
	wantDelta = wantDelta + wantDelta/2
	if diff := overrideBudget - staticBudget; diff != wantDelta {
		t.Fatalf("reconcileStepTimeout delta across effectiveMaxConcurrentWorkers %d->%d = %s, want exactly %s (only JIT ops scale with the override; retirement/registration-check ops stay tied to the static cap %d)", staticCap, override, diff, wantDelta, staticCap)
	}
}

// TestReconcileStepTimeoutIncludesIdleConfirmationWindowBudget proves the fix
// for a reviewer-flagged watchdog gap: registered retirements call
// RemoveIfIdle after deregisterRunner, and the Docker runtime
// (internal/runtime/docker/runtime.go's RemoveIfIdle) waits the full
// configured Drain.IdleConfirmationWindow before its second idle check. That
// wait is not itself a retryable GitHub operation, so with a small GitHub
// retry configuration but a large idle-confirmation window, the pre-fix
// budget could be far shorter than a single legitimate retirement's actual
// worst-case duration. The watchdog must add
// (reconcileStepIdleConfirmationWaitsPerWorker +
// reconcileStepUnregisteredRemovalIdleConfirmationWaitsPerWorker)
// idle-confirmation waits per unit of the same static retirement cap
// directly to the budget: one cap's worth for the registered-retirement
// path, plus one cap's worth for the separate unregistered-removal path,
// since a single Step can legitimately spend both back to back.
func TestReconcileStepTimeoutIncludesIdleConfirmationWindowBudget(t *testing.T) {
	t.Parallel()
	const maxConcurrentWorkers = 3
	const idleConfirmationWindow = 5 * time.Minute

	withoutWindow := githubRetryConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, maxConcurrentWorkers)
	withWindow := withoutWindow
	withWindow.Drain.IdleConfirmationWindow = config.Duration{Duration: idleConfirmationWindow}

	baseline := reconcileStepTimeout(withoutWindow, maxConcurrentWorkers)
	got := reconcileStepTimeout(withWindow, maxConcurrentWorkers)

	wantDelta := (reconcileStepIdleConfirmationWaitsPerWorker + reconcileStepUnregisteredRemovalIdleConfirmationWaitsPerWorker) * maxConcurrentWorkers * idleConfirmationWindow
	if diff := got - baseline; diff != wantDelta {
		t.Fatalf("reconcileStepTimeout delta across Drain.IdleConfirmationWindow 0->%s = %s, want exactly %s (%d workers)", idleConfirmationWindow, diff, wantDelta, maxConcurrentWorkers)
	}

	// Concretely: a small GitHub retry budget must not let the watchdog trip
	// mid-drain on a policy-compliant idle-confirmation wait.
	if got <= idleConfirmationWindow {
		t.Fatalf("reconcileStepTimeout = %s, want > single idle-confirmation window %s", got, idleConfirmationWindow)
	}
}

// TestReconcileStepTimeoutBudgetsBothIdleConfirmationRemovalPathsAdditively
// proves the exact reviewer-flagged regression: when a Step's registered-
// retirement path and its SEPARATE unregistered-removal path (reconciler.go's
// two distinct plan.Remove branches -- one after deregisterRunner, one
// standalone for model.WorkerUnregistered workers) both legitimately spend
// their full per-step idle-confirmation-wait budget in the same Step, the
// watchdog must clear the sum of BOTH, not just one cap's worth. Before this
// fix, the pre-fix budget only counted one cap's worth of confirmation
// waits, so a Step doing both legitimately could spend roughly twice the
// budgeted idle-confirmation time and get canceled mid-drain.
func TestReconcileStepTimeoutBudgetsBothIdleConfirmationRemovalPathsAdditively(t *testing.T) {
	t.Parallel()
	const maxConcurrentWorkers = 4
	const idleConfirmationWindow = 2 * time.Minute

	cfg := githubRetryConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, maxConcurrentWorkers)
	cfg.Drain.IdleConfirmationWindow = config.Duration{Duration: idleConfirmationWindow}

	got := reconcileStepTimeout(cfg, maxConcurrentWorkers)

	// The worst-case legitimate scenario the reviewer flagged: this Step
	// spends its entire registered-retirement idle-confirmation budget AND
	// its entire unregistered-removal idle-confirmation budget, back to back.
	singlePathBudget := time.Duration(maxConcurrentWorkers) * idleConfirmationWindow
	bothPathsWorstCase := 2 * singlePathBudget
	if got <= bothPathsWorstCase {
		t.Fatalf("reconcileStepTimeout = %s, want > both-paths worst case %s (a Step spending both this Step's registered-retirement and unregistered-removal idle-confirmation budgets must not be cancelled mid-drain)", got, bothPathsWorstCase)
	}

	// A pre-fix budget sized for only one path would also clear a single
	// path's worst case; the meaningful assertion is strictly the 2x one
	// above. This confirms the single-path floor is not itself the binding
	// constraint here (it would pass both pre- and post-fix).
	if got <= singlePathBudget {
		t.Fatalf("reconcileStepTimeout = %s, want > single-path idle-confirmation budget %s", got, singlePathBudget)
	}
}

// TestReconcileStepTimeoutSaturatesInsteadOfOverflowingWithHugeOverride
// proves the fix for a reviewer-flagged arithmetic-safety gap: validation
// (internal/state/fs/store.go's SaveDesired, internal/app's parseCapacity)
// only rejects a NEGATIVE Desired.TemporaryCapacityOverride, so an operator
// can legally set a very large one. The pre-fix formula multiplied
// effectiveMaxConcurrentWorkers into the op count with bare int arithmetic
// and then into nanoseconds with bare Duration arithmetic, either of which
// could silently overflow (wrapping negative or near-zero) before the
// result reached context.WithTimeout, which would cancel every reconcile
// immediately. The result must instead saturate to a large, positive
// duration for any legal (non-negative) override, all the way up to
// math.MaxInt.
func TestReconcileStepTimeoutSaturatesInsteadOfOverflowingWithHugeOverride(t *testing.T) {
	t.Parallel()
	cfg := githubRetryConfig(70*time.Second, time.Minute, 6, 3, 4)

	sane := reconcileStepTimeout(cfg, 4)

	for _, override := range []int{1 << 40, math.MaxInt32, math.MaxInt} {
		got := reconcileStepTimeout(cfg, override)
		if got <= 0 {
			t.Fatalf("reconcileStepTimeout with effectiveMaxConcurrentWorkers=%d = %s, want a large positive duration, not <= 0 (overflowed)", override, got)
		}
		if got < sane {
			t.Fatalf("reconcileStepTimeout with effectiveMaxConcurrentWorkers=%d = %s, want >= the small-override budget %s (a larger override must never produce a SMALLER watchdog)", override, got, sane)
		}
	}
}

// TestReconcileStepTimeoutIncludesWorkerImagePullBudget proves the watchdog
// budgets ensureImage's own configured pull timeout: Workers.Start's Docker
// runtime implementation (internal/runtime/docker/runtime.go) calls
// ensureImage before creating a container, which pulls the configured worker
// image whenever ImageInspect reports it missing (a first-run host, or after
// the pinned digest changes), now bounded by its own WorkerImage.PullTimeout
// (applied via context.WithTimeout around the ImagePull+Wait sequence). The
// watchdog must budget reconcileStepWorkerImagePullBudget(cfg) even when
// every other term (GitHub retries, desktop lifecycle, idle confirmation) is
// at its own floor.
func TestReconcileStepTimeoutIncludesWorkerImagePullBudget(t *testing.T) {
	t.Parallel()
	tiny := githubRetryConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1)
	got := reconcileStepTimeout(tiny, 1)
	if got < reconcileStepWorkerImagePullBudget(tiny) {
		t.Fatalf("reconcileStepTimeout = %s, want >= the configured worker image-pull term %s even with an otherwise-tiny configuration", got, reconcileStepWorkerImagePullBudget(tiny))
	}
}

// TestReconcileStepTimeoutWorkerImagePullBudgetIsFixedNotScaled proves the
// image-pull term is added once per Step, not multiplied by worker count:
// reconciler.go's plan.Start loop issues Workers.Start calls sequentially,
// never concurrently, and ensureImage's own ImageInspect check means only
// the first Start call that finds the image missing actually pulls it, so a
// larger effectiveMaxConcurrentWorkers must not multiply this term the way
// it multiplies the JIT-start retry budget. Proved by an exact delta: if the
// image-pull term scaled with worker count, the observed delta across two
// effectiveMaxConcurrentWorkers values would exceed pure JIT-ops scaling; it
// must match exactly.
func TestReconcileStepTimeoutWorkerImagePullBudgetIsFixedNotScaled(t *testing.T) {
	t.Parallel()
	const requestTO = 70 * time.Second
	const backoffMax = time.Minute
	const attempts = 6
	const low = reconcileStepJITBudgetFloorWorkers
	const high = reconcileStepJITBudgetFloorWorkers * 4

	cfg := githubRetryConfig(requestTO, backoffMax, attempts, 1, 1)
	lowBudget := reconcileStepTimeout(cfg, low)
	highBudget := reconcileStepTimeout(cfg, high)

	jitOpsDelta := reconcileStepJITOpsPerWorker * (high - low)
	wantDelta := time.Duration(jitOpsDelta*attempts) * (requestTO + backoffMax)
	wantDelta = wantDelta + wantDelta/2
	if diff := highBudget - lowBudget; diff != wantDelta {
		t.Fatalf("reconcileStepTimeout delta across effectiveMaxConcurrentWorkers %d->%d = %s, want exactly %s (the configured image-pull term must not scale with worker count; only JIT ops may)", low, high, diff, wantDelta)
	}
}

// TestReconcileStepTimeoutScalesWithWorkerImagePullTimeout proves the
// image-pull portion of the budget tracks WorkerImage.PullTimeout directly,
// mirroring how TestReconcileStepTimeoutScalesWithDesktopStartTimeout proves
// the desktop portion tracks DockerDesktop.StartTimeout.
func TestReconcileStepTimeoutScalesWithWorkerImagePullTimeout(t *testing.T) {
	t.Parallel()
	cfg := githubRetryConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1)
	shorter := cfg
	shorter.WorkerImage = config.WorkerImage{PullTimeout: config.Duration{Duration: time.Minute}}
	longer := cfg
	longer.WorkerImage = config.WorkerImage{PullTimeout: config.Duration{Duration: 10 * time.Minute}}

	shorterBudget := reconcileStepTimeout(shorter, 1)
	longerBudget := reconcileStepTimeout(longer, 1)
	if longerBudget <= shorterBudget {
		t.Fatalf("reconcileStepTimeout with 10m PullTimeout = %s, want > with 1m PullTimeout = %s", longerBudget, shorterBudget)
	}
	if diff, want := longerBudget-shorterBudget, 10*time.Minute-time.Minute; diff != want {
		t.Fatalf("reconcileStepTimeout delta across WorkerImage.PullTimeout change = %s, want exactly %s", diff, want)
	}
}

// TestReconcileStepTimeoutFloorsJITOpsAgainstOverrideStaleness proves the fix
// for a reviewer-flagged staleness gap: effectiveMaxConcurrentWorkers is read
// from Desired.TemporaryCapacityOverride immediately before this function is
// called, but Step() can re-run step() under this SAME deadline
// (errReconcileInputsChanged, when watchSafetyInputs or freshStartAllowed
// observes the desired state changed mid-step) using a FRESH LoadDesired
// read -- so an operator raising the override during a Step can need more
// JIT-start budget than the pre-Step snapshot provided, against a deadline
// that already exists and cannot grow. reconcileStepJITBudgetFloorWorkers
// floors the JIT-ops worker count generously enough to absorb any override
// raise within a realistic single-host operational envelope, regardless of
// the live snapshot's exact value at read time.
func TestReconcileStepTimeoutFloorsJITOpsAgainstOverrideStaleness(t *testing.T) {
	t.Parallel()
	cfg := githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1)

	atFloor := reconcileStepTimeout(cfg, reconcileStepJITBudgetFloorWorkers)
	for _, small := range []int{0, 1, reconcileStepJITBudgetFloorWorkers - 1} {
		got := reconcileStepTimeout(cfg, small)
		if got != atFloor {
			t.Fatalf("reconcileStepTimeout with effectiveMaxConcurrentWorkers=%d = %s, want exactly the at-floor budget %s (any snapshot below the floor must be treated identically to the floor itself, so a mid-step override raise within the floor cannot exceed the budget regardless of the snapshot's exact value)", small, got, atFloor)
		}
	}
}

// TestReconcileStepTimeoutIncludesNotFoundRecoveryOpsPerTarget proves the fix
// for a reviewer-flagged gap: when a target's persisted scale set is deleted
// externally, r.statistics's not-found recovery path
// (internal/controller/reconciler.go:945-955) issues a SECOND r.ensure call
// plus a SECOND r.statistics call, each a full RetryValue budget, on top of
// the target's normal one-ensure-one-statistics sweep. reconcileStepOpsPerTarget
// must therefore budget 4 retryable operations per target, not 2, or a Step
// legitimately recovering enough externally-deleted scale sets can exceed
// the watchdog even though every individual retry obeyed policy.
func TestReconcileStepTimeoutIncludesNotFoundRecoveryOpsPerTarget(t *testing.T) {
	t.Parallel()
	if reconcileStepOpsPerTarget != 4 {
		t.Fatalf("reconcileStepOpsPerTarget = %d, want exactly 4 (ensure + statistics, each doubled for the not-found recovery path)", reconcileStepOpsPerTarget)
	}

	const requestTO = 70 * time.Second
	const backoffMax = time.Minute
	const attempts = 6
	const maxConcurrentWorkers = 1

	oneTarget := reconcileStepTimeout(githubRetryConfig(requestTO, backoffMax, attempts, 1, maxConcurrentWorkers), maxConcurrentWorkers)
	twoTargets := reconcileStepTimeout(githubRetryConfig(requestTO, backoffMax, attempts, 2, maxConcurrentWorkers), maxConcurrentWorkers)

	// Each additional target adds reconcileStepOpsPerTarget more retryable
	// operations, margined 1.5x, same as the golden-path delta tests above.
	perTargetOps := time.Duration(reconcileStepOpsPerTarget*attempts) * (requestTO + backoffMax)
	wantDelta := perTargetOps + perTargetOps/2
	if diff := twoTargets - oneTarget; diff != wantDelta {
		t.Fatalf("reconcileStepTimeout delta across targets 1->2 = %s, want exactly %s (%d ops per target, including the not-found recovery pair)", diff, wantDelta, reconcileStepOpsPerTarget)
	}
}

// TestSaturatingMulIntClampsInsteadOfWrapping and its siblings below prove
// the arithmetic-safety helpers reconcileStepTimeout relies on never wrap
// past math.MaxInt/math.MaxInt64, mirroring
// internal/controller/reconciler.go's saturatingAddUint64 test coverage for
// the signed, multiplicative cases this watchdog needs.
func TestSaturatingMulIntClampsInsteadOfWrapping(t *testing.T) {
	t.Parallel()
	if got := saturatingMulInt(math.MaxInt, 2); got != math.MaxInt {
		t.Fatalf("saturatingMulInt(MaxInt, 2) = %d, want math.MaxInt", got)
	}
	if got := saturatingMulInt(3, 4); got != 12 {
		t.Fatalf("saturatingMulInt(3, 4) = %d, want 12", got)
	}
	if got := saturatingMulInt(0, math.MaxInt); got != 0 {
		t.Fatalf("saturatingMulInt(0, MaxInt) = %d, want 0", got)
	}
}

func TestSaturatingAddIntClampsInsteadOfWrapping(t *testing.T) {
	t.Parallel()
	if got := saturatingAddInt(math.MaxInt, 1); got != math.MaxInt {
		t.Fatalf("saturatingAddInt(MaxInt, 1) = %d, want math.MaxInt", got)
	}
	if got := saturatingAddInt(3, 4); got != 7 {
		t.Fatalf("saturatingAddInt(3, 4) = %d, want 7", got)
	}
}

func TestSaturatingScaleDurationClampsInsteadOfWrapping(t *testing.T) {
	t.Parallel()
	if got := saturatingScaleDuration(time.Hour, math.MaxInt); got != math.MaxInt64 {
		t.Fatalf("saturatingScaleDuration(1h, MaxInt) = %s, want math.MaxInt64", got)
	}
	if got := saturatingScaleDuration(time.Second, 3); got != 3*time.Second {
		t.Fatalf("saturatingScaleDuration(1s, 3) = %s, want 3s", got)
	}
}

func TestSaturatingAddDurationClampsInsteadOfWrapping(t *testing.T) {
	t.Parallel()
	if got := saturatingAddDuration(math.MaxInt64, time.Second); got != math.MaxInt64 {
		t.Fatalf("saturatingAddDuration(MaxInt64, 1s) = %s, want math.MaxInt64", got)
	}
	if got := saturatingAddDuration(time.Second, 2*time.Second); got != 3*time.Second {
		t.Fatalf("saturatingAddDuration(1s, 2s) = %s, want 3s", got)
	}
}

func TestReconcileStepDrainGraceReusesWatchdogConstants(t *testing.T) {
	t.Parallel()
	cfg := githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1)
	want := 70*time.Second + time.Minute
	if got := reconcileStepDrainGrace(cfg); got != want {
		t.Fatalf("reconcileStepDrainGrace = %s, want %s (RequestTimeout + Retry.Maximum)", got, want)
	}
}

// TestErrReconcileStepAbandonedIsDistinctSentinel proves
// errReconcileStepAbandoned is a stable, non-nil sentinel distinguishable
// from any other error, since main.go's controllerExitCode only special-cases
// ErrControllerRestartRequested and must fall through to the ordinary
// nonzero-exit path (which the ci-runner-fleet Scheduled Task's
// restart-on-failure policy relies on) for this one.
func TestErrReconcileStepAbandonedIsDistinctSentinel(t *testing.T) {
	t.Parallel()
	if errReconcileStepAbandoned == nil {
		t.Fatal("errReconcileStepAbandoned = nil, want a non-nil sentinel")
	}
	if errors.Is(errReconcileStepAbandoned, ErrControllerRestartRequested) {
		t.Fatal("errReconcileStepAbandoned must not match ErrControllerRestartRequested: a wedged-step exit has not completed an authenticated drain and must not claim the dedicated restart exit code")
	}
}

func TestReconcileFailureStreakEscalatesOnlyOnUnbrokenBlockedErrorRun(t *testing.T) {
	t.Parallel()
	var streak reconcileFailureStreak
	blocked := stepOutcome{err: errors.New("adopt workers before artifact cleanup: lock jobs index: context canceled"), newWorkBlocked: true}
	for i := 1; i < maxConsecutiveReconcileStepErrors; i++ {
		if streak.observe(blocked) {
			t.Fatalf("streak escalated at %d errors, before the %d threshold", i, maxConsecutiveReconcileStepErrors)
		}
	}
	if !streak.observe(blocked) {
		t.Fatalf("streak did not escalate at %d consecutive blocked errors", maxConsecutiveReconcileStepErrors)
	}
}

func TestReconcileFailureStreakResetsOnAnyCleanStep(t *testing.T) {
	t.Parallel()
	var streak reconcileFailureStreak
	blocked := stepOutcome{err: errors.New("worker inventory failed"), newWorkBlocked: true}
	for i := 0; i < maxConsecutiveReconcileStepErrors-1; i++ {
		streak.observe(blocked)
	}
	if streak.observe(stepOutcome{}) {
		t.Fatal("clean step escalated")
	}
	if streak.count != 0 {
		t.Fatalf("clean step left streak count %d, want 0", streak.count)
	}
	if streak.observe(blocked) {
		t.Fatal("first error after a clean step escalated")
	}
}

// A per-target failure (scale-set ensure error for one pool) returns a Step
// error while the remaining pools keep reconciling; it must never feed the
// escalation streak, or a single misconfigured target would consume the
// scheduled task's bounded restart budget.
func TestReconcileFailureStreakIgnoresPartialPerTargetFailures(t *testing.T) {
	t.Parallel()
	var streak reconcileFailureStreak
	partial := stepOutcome{err: errors.New("ensure scale set: credentials rejected")}
	for i := 0; i < maxConsecutiveReconcileStepErrors*2; i++ {
		if streak.observe(partial) {
			t.Fatal("partial per-target failure escalated")
		}
	}
	if streak.count != 0 {
		t.Fatalf("partial failures accumulated streak count %d, want 0", streak.count)
	}
	blocked := stepOutcome{err: errors.New("worker inventory failed"), newWorkBlocked: true}
	for i := 0; i < maxConsecutiveReconcileStepErrors-1; i++ {
		streak.observe(blocked)
	}
	if streak.observe(partial) {
		t.Fatal("partial failure escalated an existing blocked streak")
	}
	if streak.count != 0 {
		t.Fatalf("partial failure did not reset the blocked streak: count %d", streak.count)
	}
}

func TestErrReconcilePersistentlyFailingDoesNotClaimRestartExitCode(t *testing.T) {
	t.Parallel()
	if errors.Is(errReconcilePersistentlyFailing, ErrControllerRestartRequested) {
		t.Fatal("errReconcilePersistentlyFailing must not match ErrControllerRestartRequested: a persistent-failure exit has not completed an authenticated drain and must not claim the dedicated restart exit code")
	}
}
