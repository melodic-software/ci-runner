package app

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/controller"
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
		// Matches the recommended production default.
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
			// The watchdog must strictly exceed the worst-case Step: target sweep, JIT, and retirement
			// retry loops, each a full jittered retry budget.
			ops := reconcileStepOpsPerTarget*tc.targets + reconcileStepJITOpsPerWorker*tc.maxConcurrentWorkers + reconcileStepRetirementOpsPerWorker*tc.maxConcurrentWorkers
			maxJitteredBackoff := tc.backoffMax + time.Duration(float64(tc.backoffMax)*tc.jitterRatio)
			budget := time.Duration(ops*tc.maxAttempts) * (tc.requestTO + maxJitteredBackoff)
			if got <= budget {
				t.Fatalf("reconcileStepTimeout = %s, want > whole-step retry budget %s (targets=%d, maxAttempts=%d, maxConcurrentWorkers=%d, jitterRatio=%v)", got, budget, tc.targets, tc.maxAttempts, tc.maxConcurrentWorkers, tc.jitterRatio)
			}
		})
	}
}

// TestReconcileStepTimeoutAccountsForJitteredBackoff pins sizing backoff waits at
// Retry.Maximum*(1+JitterRatio): jitter applies after the cap, so a wait can reach 2x Maximum.
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

	// At jitterRatio=1 each backoff doubles to 2x Maximum, scaled by the 1.5x margin. JIT ops sit at
	// reconcileStepJITBudgetFloorWorkers, since maxConcurrentWorkers=1 is below it.
	ops := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*max(maxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers) + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers + reconcileStepRegistrationCheckOpsPerWorker*maxConcurrentWorkers
	extraRetryBudget := time.Duration(ops*attempts) * backoffMax
	wantDelta := extraRetryBudget + extraRetryBudget/2
	if diff := got - baseline; diff != wantDelta {
		t.Fatalf("reconcileStepTimeout delta across jitterRatio 0->1 = %s, want exactly %s", diff, wantDelta)
	}

	// The watchdog must clear one worst-case jittered attempt.
	worstCaseJitteredAttempt := requestTO + 2*backoffMax
	if got <= worstCaseJitteredAttempt {
		t.Fatalf("reconcileStepTimeout = %s, want > single worst-case jittered attempt delay %s", got, worstCaseJitteredAttempt)
	}
}

// TestReconcileStepTimeoutIncludesRetirementRetryBudget pins a deregisterRunner retry budget per
// retired worker (up to the static cap), added to the JIT-start budget.
func TestReconcileStepTimeoutIncludesRetirementRetryBudget(t *testing.T) {
	t.Parallel()
	const requestTO = 70 * time.Second
	const backoffMax = time.Minute
	const attempts = 6
	const targets = 1
	const maxConcurrentWorkers = 4

	cfg := githubRetryConfig(requestTO, backoffMax, attempts, targets, maxConcurrentWorkers)
	got := reconcileStepTimeout(cfg, maxConcurrentWorkers)

	// Retirement must add to the target sweep and JIT starts; JIT ops sit at the floor here.
	preFixOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*max(maxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers)
	preFixRetryBudget := time.Duration(preFixOps*attempts) * (requestTO + backoffMax)
	preFixGithubBudget := preFixRetryBudget + preFixRetryBudget/2
	if got <= preFixGithubBudget {
		t.Fatalf("reconcileStepTimeout = %s, want > pre-fix (JIT-only) github budget %s once retirement retries are budgeted", got, preFixGithubBudget)
	}

	fullOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*max(maxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers) + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers + reconcileStepRegistrationCheckOpsPerWorker*maxConcurrentWorkers
	fullRetryBudget := time.Duration(fullOps*attempts) * (requestTO + backoffMax)
	// desktopStart/desktopStop are 0, so the only desktop-category term is the image-pull budget.
	want := fullRetryBudget + fullRetryBudget/2 + reconcileStepWorkerImagePullBudget(cfg)
	if got != want {
		t.Fatalf("reconcileStepTimeout = %s, want exactly %s (target sweep + JIT starts + retirements + registration checks, all margined 1.5x, plus the configured image-pull term)", got, want)
	}
}

// TestReconcileStepTimeoutIncludesRegistrationCheckRetryBudget pins a RunnerRegistered retry budget
// per checked idle worker, added to the JIT-start and retirement budgets.
func TestReconcileStepTimeoutIncludesRegistrationCheckRetryBudget(t *testing.T) {
	t.Parallel()
	const requestTO = 70 * time.Second
	const backoffMax = time.Minute
	const attempts = 6
	const targets = 1
	const maxConcurrentWorkers = 4

	cfg := githubRetryConfig(requestTO, backoffMax, attempts, targets, maxConcurrentWorkers)
	got := reconcileStepTimeout(cfg, maxConcurrentWorkers)

	// Registration checks must add to the sweep, JIT starts, and retirements; JIT ops sit at the floor.
	preFixOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*max(maxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers) + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers
	preFixRetryBudget := time.Duration(preFixOps*attempts) * (requestTO + backoffMax)
	preFixGithubBudget := preFixRetryBudget + preFixRetryBudget/2
	if got <= preFixGithubBudget {
		t.Fatalf("reconcileStepTimeout = %s, want > pre-fix (JIT+retirement) github budget %s once registration checks are budgeted", got, preFixGithubBudget)
	}

	fullOps := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*max(maxConcurrentWorkers, reconcileStepJITBudgetFloorWorkers) + reconcileStepRetirementOpsPerWorker*maxConcurrentWorkers + reconcileStepRegistrationCheckOpsPerWorker*maxConcurrentWorkers
	fullRetryBudget := time.Duration(fullOps*attempts) * (requestTO + backoffMax)
	// desktopStart/desktopStop are 0, so the only desktop-category term is the image-pull budget.
	want := fullRetryBudget + fullRetryBudget/2 + reconcileStepWorkerImagePullBudget(cfg)
	if got != want {
		t.Fatalf("reconcileStepTimeout = %s, want exactly %s (target sweep + JIT starts + retirements + registration checks, all margined 1.5x, plus the configured image-pull term)", got, want)
	}
}

// TestReconcileStepTimeoutAccountsForDesktopLifecycleTimeouts pins two desktop Starts plus one Stop
// at full timeout on top of the GitHub budget, which alone can be far below one desktop start.
func TestReconcileStepTimeoutAccountsForDesktopLifecycleTimeouts(t *testing.T) {
	t.Parallel()
	const startTimeout = 2 * time.Minute
	const stopTimeout = 90 * time.Second

	smallGitHubRetryCfg := desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, startTimeout, stopTimeout)
	githubOnlyBudget := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, 0, 0), 1)

	got := reconcileStepTimeout(smallGitHubRetryCfg, 1)

	// The watchdog must clear the GitHub-only budget by two Starts plus one Stop at full timeout.
	desktopWorstCase := reconcileStepDesktopStartAttempts*startTimeout + stopTimeout
	if got < githubOnlyBudget+desktopWorstCase {
		t.Fatalf("reconcileStepTimeout = %s, want >= github-only budget %s + desktop worst case %s (= %s)",
			got, githubOnlyBudget, desktopWorstCase, githubOnlyBudget+desktopWorstCase)
	}

	// The watchdog must never be shorter than one policy-compliant desktop start.
	if got <= startTimeout {
		t.Fatalf("reconcileStepTimeout = %s, want > single desktop StartTimeout %s", got, startTimeout)
	}
}

// TestReconcileStepTimeoutScalesWithDesktopStartTimeout pins the desktop term tracking
// DockerDesktop.StartTimeout.
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

// TestReconcileStepTimeoutScalesWithDesktopStopTimeout pins the desktop term tracking
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

// TestReconcileStepTimeoutFloorsZeroEffectiveMaxConcurrentWorkers pins effective limits of 0 and 1
// collapsing to reconcileStepJITBudgetFloorWorkers, not to one worker.
func TestReconcileStepTimeoutFloorsZeroEffectiveMaxConcurrentWorkers(t *testing.T) {
	t.Parallel()
	if got, want := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1), 0),
		reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1), 1); got != want {
		t.Fatalf("zero effectiveMaxConcurrentWorkers: reconcileStepTimeout = %s, want equal to effectiveMaxConcurrentWorkers=1's budget %s (both below reconcileStepJITBudgetFloorWorkers, so both collapse to the same floor)", got, want)
	}
}

// TestReconcileStepTimeoutSizesJITBudgetFromEffectiveOverride pins the JIT budget scaling with the
// effective limit (capacity override), not the static cap alone.
func TestReconcileStepTimeoutSizesJITBudgetFromEffectiveOverride(t *testing.T) {
	t.Parallel()
	const requestTO = 70 * time.Second
	const backoffMax = time.Minute
	const attempts = 6
	const targets = 1
	const staticCap = 1
	// The override must exceed the JIT floor, or both sides floor to the same value and show no delta.
	const override = reconcileStepJITBudgetFloorWorkers + 50

	cfg := githubRetryConfig(requestTO, backoffMax, attempts, targets, staticCap)
	staticBudget := reconcileStepTimeout(cfg, staticCap)
	overrideBudget := reconcileStepTimeout(cfg, override)

	if overrideBudget <= staticBudget {
		t.Fatalf("reconcileStepTimeout with effectiveMaxConcurrentWorkers=%d (override) = %s, want > effectiveMaxConcurrentWorkers=%d (static cap) budget %s", override, overrideBudget, staticCap, staticBudget)
	}

	// Only JIT ops scale with the override; retirement and registration checks stay on the static
	// cap, and the static side of the delta is anchored at the JIT floor.
	jitOpsDelta := reconcileStepJITOpsPerWorker * (override - reconcileStepJITBudgetFloorWorkers)
	wantDelta := time.Duration(jitOpsDelta*attempts) * (requestTO + backoffMax)
	wantDelta = wantDelta + wantDelta/2
	if diff := overrideBudget - staticBudget; diff != wantDelta {
		t.Fatalf("reconcileStepTimeout delta across effectiveMaxConcurrentWorkers %d->%d = %s, want exactly %s (only JIT ops scale with the override; retirement/registration-check ops stay tied to the static cap %d)", staticCap, override, diff, wantDelta, staticCap)
	}
}

// TestReconcileStepTimeoutIncludesIdleConfirmationWindowBudget pins two IdleConfirmationWindow waits
// per static-cap worker (registered and unregistered removal), added outside the retry margin.
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

// TestReconcileStepTimeoutBudgetsBothIdleConfirmationRemovalPathsAdditively pins the budget clearing
// both removal paths' full idle-confirmation waits back to back in one Step.
func TestReconcileStepTimeoutBudgetsBothIdleConfirmationRemovalPathsAdditively(t *testing.T) {
	t.Parallel()
	const maxConcurrentWorkers = 4
	const idleConfirmationWindow = 2 * time.Minute

	cfg := githubRetryConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, maxConcurrentWorkers)
	cfg.Drain.IdleConfirmationWindow = config.Duration{Duration: idleConfirmationWindow}

	got := reconcileStepTimeout(cfg, maxConcurrentWorkers)

	// Worst case: both idle-confirmation budgets spent back to back in one Step.
	singlePathBudget := time.Duration(maxConcurrentWorkers) * idleConfirmationWindow
	bothPathsWorstCase := 2 * singlePathBudget
	if got <= bothPathsWorstCase {
		t.Fatalf("reconcileStepTimeout = %s, want > both-paths worst case %s (a Step spending both this Step's registered-retirement and unregistered-removal idle-confirmation budgets must not be cancelled mid-drain)", got, bothPathsWorstCase)
	}

	// The single-path floor holds pre- and post-fix; the 2x assertion above is the meaningful one.
	if got <= singlePathBudget {
		t.Fatalf("reconcileStepTimeout = %s, want > single-path idle-confirmation budget %s", got, singlePathBudget)
	}
}

// TestReconcileStepTimeoutSaturatesInsteadOfOverflowingWithHugeOverride pins a large positive
// result for every non-negative override up to math.MaxInt, never a wrapped one.
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

// TestReconcileStepTimeoutIncludesWorkerImagePullBudget pins the WorkerImage.PullTimeout term even
// when every other term is at its floor.
func TestReconcileStepTimeoutIncludesWorkerImagePullBudget(t *testing.T) {
	t.Parallel()
	tiny := githubRetryConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1)
	got := reconcileStepTimeout(tiny, 1)
	if got < reconcileStepWorkerImagePullBudget(tiny) {
		t.Fatalf("reconcileStepTimeout = %s, want >= the configured worker image-pull term %s even with an otherwise-tiny configuration", got, reconcileStepWorkerImagePullBudget(tiny))
	}
}

// TestReconcileStepTimeoutWorkerImagePullBudgetIsFixedNotScaled pins the image-pull term as once per
// Step: the delta across worker counts must match pure JIT-ops scaling exactly.
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

// TestReconcileStepTimeoutScalesWithWorkerImagePullTimeout pins the image-pull term tracking
// WorkerImage.PullTimeout.
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

// TestReconcileStepTimeoutFloorsJITOpsAgainstOverrideStaleness pins the JIT floor absorbing an
// override raised mid-Step, when step() re-runs under the same deadline.
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

// TestReconcileStepTimeoutIncludesNotFoundRecoveryOpsPerTarget pins 4 retryable ops per target:
// not-found recovery repeats ensure and statistics.
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

// TestSaturatingMulIntClampsInsteadOfWrapping and its siblings pin the saturating helpers never
// wrapping past math.MaxInt/math.MaxInt64.
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
	want := 70*time.Second + time.Minute + controller.StepDetachedPersistDrain
	if got := reconcileStepDrainGrace(cfg); got != want {
		t.Fatalf("reconcileStepDrainGrace = %s, want %s (RequestTimeout + Retry.Maximum + per-Step detached persist drain)", got, want)
	}
}

// TestReconcileStepDrainGraceClearsDetachedPersistDrain pins the grace outlasting all three serial
// detached persists for any valid config, whose timeouts may total one nanosecond.
func TestReconcileStepDrainGraceClearsDetachedPersistDrain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		requestTimeout time.Duration
		backoffMax     time.Duration
	}{
		{name: "validation floor", requestTimeout: time.Nanosecond, backoffMax: time.Nanosecond},
		{name: "sub-second timeouts", requestTimeout: 100 * time.Millisecond, backoffMax: 10 * time.Millisecond},
		{name: "configured terms below one write bound", requestTimeout: time.Second, backoffMax: time.Second},
		{name: "configured terms between one write and the aggregate", requestTimeout: 4 * time.Second, backoffMax: 3 * time.Second},
		{name: "production defaults", requestTimeout: 70 * time.Second, backoffMax: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := githubRetryConfig(tc.requestTimeout, tc.backoffMax, 6, 1, 1)
			if got := reconcileStepDrainGrace(cfg); got <= controller.StepDetachedPersistDrain {
				t.Fatalf("reconcileStepDrainGrace = %s, want > per-Step detached persist drain %s (requestTimeout=%s, retry.maximum=%s)", got, controller.StepDetachedPersistDrain, tc.requestTimeout, tc.backoffMax)
			}
		})
	}
}

func TestReconcileStepDrainGraceClampsInsteadOfWrapping(t *testing.T) {
	t.Parallel()
	cfg := githubRetryConfig(math.MaxInt64, time.Minute, 6, 1, 1)
	if got := reconcileStepDrainGrace(cfg); got != math.MaxInt64 {
		t.Fatalf("reconcileStepDrainGrace = %s, want math.MaxInt64: a grace that wraps negative makes every cancelled Step look abandoned", got)
	}
}

// TestErrReconcileStepAbandonedIsDistinctSentinel pins a distinct sentinel that must not match
// ErrControllerRestartRequested, so the controller takes the ordinary nonzero exit.
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
	for range maxConsecutiveReconcileStepErrors - 1 {
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

// A per-target failure must never feed the escalation streak, or one misconfigured target
// would spend the scheduled task's bounded restart budget.
func TestReconcileFailureStreakIgnoresPartialPerTargetFailures(t *testing.T) {
	t.Parallel()
	var streak reconcileFailureStreak
	partial := stepOutcome{err: errors.New("ensure scale set: credentials rejected")}
	for range maxConsecutiveReconcileStepErrors * 2 {
		if streak.observe(partial) {
			t.Fatal("partial per-target failure escalated")
		}
	}
	if streak.count != 0 {
		t.Fatalf("partial failures accumulated streak count %d, want 0", streak.count)
	}
	blocked := stepOutcome{err: errors.New("worker inventory failed"), newWorkBlocked: true}
	for range maxConsecutiveReconcileStepErrors - 1 {
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
