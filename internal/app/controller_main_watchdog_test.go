package app

import (
	"errors"
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
	// Maximum per op per attempt, scaled by the watchdog's 1.5x margin.
	ops := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*maxConcurrentWorkers + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers + reconcileStepRegistrationCheckOpsPerWorker*maxConcurrentWorkers
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
	// retirement contribution must be strictly additional to that.
	preFixOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*maxConcurrentWorkers
	preFixRetryBudget := time.Duration(preFixOps*attempts) * (requestTO + backoffMax)
	preFixGithubBudget := preFixRetryBudget + preFixRetryBudget/2
	if got <= preFixGithubBudget {
		t.Fatalf("reconcileStepTimeout = %s, want > pre-fix (JIT-only) github budget %s once retirement retries are budgeted", got, preFixGithubBudget)
	}

	fullOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*maxConcurrentWorkers + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers + reconcileStepRegistrationCheckOpsPerWorker*maxConcurrentWorkers
	fullRetryBudget := time.Duration(fullOps*attempts) * (requestTO + backoffMax)
	want := fullRetryBudget + fullRetryBudget/2
	if got != want {
		t.Fatalf("reconcileStepTimeout = %s, want exactly %s (target sweep + JIT starts + retirements + registration checks, all margined 1.5x)", got, want)
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
	// additional to that.
	preFixOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*maxConcurrentWorkers + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers
	preFixRetryBudget := time.Duration(preFixOps*attempts) * (requestTO + backoffMax)
	preFixGithubBudget := preFixRetryBudget + preFixRetryBudget/2
	if got <= preFixGithubBudget {
		t.Fatalf("reconcileStepTimeout = %s, want > pre-fix (JIT+retirement) github budget %s once registration checks are budgeted", got, preFixGithubBudget)
	}

	fullOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*maxConcurrentWorkers + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers + reconcileStepRegistrationCheckOpsPerWorker*maxConcurrentWorkers
	fullRetryBudget := time.Duration(fullOps*attempts) * (requestTO + backoffMax)
	want := fullRetryBudget + fullRetryBudget/2
	if got != want {
		t.Fatalf("reconcileStepTimeout = %s, want exactly %s (target sweep + JIT starts + retirements + registration checks, all margined 1.5x)", got, want)
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
// the override-aware parameter Fix 3/5 introduced.
func TestReconcileStepTimeoutFloorsZeroEffectiveMaxConcurrentWorkers(t *testing.T) {
	t.Parallel()
	if got, want := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1), 0),
		reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1), 1); got != want {
		t.Fatalf("zero effectiveMaxConcurrentWorkers: reconcileStepTimeout = %s, want single-worker floor %s", got, want)
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
	const override = 10

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
	// temporary override.
	jitOpsDelta := reconcileStepJITOpsPerWorker * (override - staticCap)
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
