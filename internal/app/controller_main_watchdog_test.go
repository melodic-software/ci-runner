package app

import (
	"context"
	"errors"
	"sync"
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
			got := reconcileStepTimeout(cfg)
			// The watchdog must strictly exceed the whole-step worst case: an
			// ensure+statistics sweep across every target plus a CreateJITConfig
			// retry loop for every worker the host can concurrently start, each a
			// full retry budget (attempts requests at RequestTimeout plus attempts
			// backoff waits jittered up to Retry.Maximum*(1+JitterRatio) per
			// internal/controller/retry.go's BackoffPolicy.delay). It must never
			// trip on a legitimate multi-target, high-maxAttempts,
			// multi-worker-JIT, or fully-jittered-backoff step.
			ops := reconcileStepOpsPerTarget*tc.targets + reconcileStepJITOpsPerWorker*tc.maxConcurrentWorkers
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

	baseline := reconcileStepTimeout(unjittered)
	got := reconcileStepTimeout(fullyJittered)

	if got <= baseline {
		t.Fatalf("reconcileStepTimeout with jitterRatio=1 = %s, want > jitterRatio=0 baseline %s", got, baseline)
	}

	// At jitterRatio=1, every retryable op's worst-case per-attempt backoff
	// grows from bare Maximum to Maximum*(1+1) = 2x Maximum: exactly one extra
	// Maximum per op per attempt, scaled by the watchdog's 1.5x margin.
	ops := reconcileStepOpsPerTarget*targets + reconcileStepJITOpsPerWorker*maxConcurrentWorkers
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
	githubOnlyBudget := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, 0, 0))

	got := reconcileStepTimeout(smallGitHubRetryCfg)

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
	shorter := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, time.Minute, 0))
	longer := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, 10*time.Minute, 0))
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
	shorter := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, 0, time.Minute))
	longer := reconcileStepTimeout(desktopLifecycleConfig(time.Second, time.Second, reconcileStepMinRetryAttempts, 1, 1, 0, 10*time.Minute))
	if longer <= shorter {
		t.Fatalf("reconcileStepTimeout with 10m StopTimeout = %s, want > with 1m StopTimeout = %s", longer, shorter)
	}
	if diff, want := longer-shorter, 10*time.Minute-time.Minute; diff != want {
		t.Fatalf("reconcileStepTimeout delta across StopTimeout change = %s, want exactly %s", diff, want)
	}
}

func TestReconcileStepTimeoutFloorsPathologicalMaxAttempts(t *testing.T) {
	t.Parallel()
	floored := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, reconcileStepMinRetryAttempts, 1, 1))
	for _, attempts := range []int{0, 1} {
		if got := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, attempts, 1, 1)); got != floored {
			t.Fatalf("maxAttempts=%d: reconcileStepTimeout = %s, want floored (min %d attempts) %s", attempts, got, reconcileStepMinRetryAttempts, floored)
		}
	}
}

func TestReconcileStepTimeoutFloorsZeroTargets(t *testing.T) {
	t.Parallel()
	if got, want := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 0, 1)),
		reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1)); got != want {
		t.Fatalf("zero targets: reconcileStepTimeout = %s, want single-target floor %s", got, want)
	}
}

func TestReconcileStepTimeoutFloorsZeroMaxConcurrentWorkers(t *testing.T) {
	t.Parallel()
	if got, want := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 0)),
		reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1)); got != want {
		t.Fatalf("zero maxConcurrentWorkers: reconcileStepTimeout = %s, want single-worker floor %s", got, want)
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

// captureLogSink is a minimal controller.LogSink recording every event's Code
// for assertions, guarded by a mutex so it is safe if a test ever calls it
// from more than one goroutine.
type captureLogSink struct {
	mu    sync.Mutex
	codes []string
}

func (s *captureLogSink) Write(_ context.Context, event controller.LogEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes = append(s.codes, event.Code)
	return nil
}

func (s *captureLogSink) codesSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.codes...)
}

func TestReconcileWatchdogStateReadyWithNoAbandonedStep(t *testing.T) {
	t.Parallel()
	var watchdog reconcileWatchdogState
	logs := &captureLogSink{}
	if !watchdog.readyForNextStep(logs) {
		t.Fatal("readyForNextStep = false, want true when no step has been abandoned")
	}
	if got := logs.codesSnapshot(); len(got) != 0 {
		t.Fatalf("logged events = %v, want none", got)
	}
}

// TestReconcileWatchdogStateSkipsTicksWhileAbandonedStepStaysBlocked proves the
// exact guarantee the fix adds: once a watchdog-timed-out Step goroutine fails
// to unwind within its drain grace period and is recorded as abandoned,
// readyForNextStep keeps refusing to let the reconcile loop start an
// overlapping Step — across many consecutive ticks — instead of silently
// proceeding as if the stuck goroutine had released Reconciler.stepMu.
func TestReconcileWatchdogStateSkipsTicksWhileAbandonedStepStaysBlocked(t *testing.T) {
	t.Parallel()
	var watchdog reconcileWatchdogState
	// A stepDone channel that is never written to models a Step goroutine
	// whose blocked adapter call never actually unwinds after cancellation.
	stuck := make(chan error)
	watchdog.abandon(stuck)

	logs := &captureLogSink{}
	const ticks = 5
	for i := 0; i < ticks; i++ {
		if watchdog.readyForNextStep(logs) {
			t.Fatalf("tick %d: readyForNextStep = true, want false while the abandoned step remains blocked", i)
		}
	}
	codes := logs.codesSnapshot()
	if len(codes) != ticks {
		t.Fatalf("logged %d events, want %d (one skip per tick)", len(codes), ticks)
	}
	for i, code := range codes {
		if code != "reconcile-watchdog-tick-skipped" {
			t.Fatalf("event %d code = %q, want %q", i, code, "reconcile-watchdog-tick-skipped")
		}
	}
}

func TestReconcileWatchdogStateResumesAfterAbandonedStepReleasesLock(t *testing.T) {
	t.Parallel()
	var watchdog reconcileWatchdogState
	stuck := make(chan error, 1)
	watchdog.abandon(stuck)

	logs := &captureLogSink{}
	if watchdog.readyForNextStep(logs) {
		t.Fatal("readyForNextStep = true before the abandoned step reported back")
	}

	stepErr := errors.New("adapter call finally aborted")
	stuck <- stepErr
	if !watchdog.readyForNextStep(logs) {
		t.Fatal("readyForNextStep = false, want true once the abandoned step released its lock")
	}
	codes := logs.codesSnapshot()
	if len(codes) != 3 {
		t.Fatalf("logged events = %v, want 3 (skip, reconcile-error, drain-recovered)", codes)
	}
	if codes[0] != "reconcile-watchdog-tick-skipped" {
		t.Fatalf("event 0 code = %q, want %q", codes[0], "reconcile-watchdog-tick-skipped")
	}
	if codes[1] != "reconcile-error" {
		t.Fatalf("event 1 code = %q, want %q", codes[1], "reconcile-error")
	}
	if codes[2] != "reconcile-watchdog-drain-recovered" {
		t.Fatalf("event 2 code = %q, want %q", codes[2], "reconcile-watchdog-drain-recovered")
	}

	// Once recovered, the state must not keep reporting the same resolution
	// and must allow the loop to proceed normally on subsequent ticks.
	if !watchdog.readyForNextStep(logs) {
		t.Fatal("readyForNextStep = false after recovery, want true")
	}
	if got := len(logs.codesSnapshot()); got != 3 {
		t.Fatalf("logged %d events after recovery settled, want still 3 (no further logging once cleared)", got)
	}
}
