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
	return config.Config{
		GitHub: config.GitHub{
			RequestTimeout: config.Duration{Duration: requestTimeout},
			Retry:          config.Retry{Maximum: config.Duration{Duration: backoffMax}, MaxAttempts: maxAttempts},
			Targets:        make([]config.Target, targets),
		},
		Resources: config.Resources{MaximumConcurrentWorkers: maxConcurrentWorkers},
	}
}

func TestReconcileStepTimeoutClearsConfiguredRetryBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		requestTO            time.Duration
		backoffMax           time.Duration
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := reconcileStepTimeout(githubRetryConfig(tc.requestTO, tc.backoffMax, tc.maxAttempts, tc.targets, tc.maxConcurrentWorkers))
			// The watchdog must strictly exceed the whole-step worst case: an
			// ensure+statistics sweep across every target plus a CreateJITConfig
			// retry loop for every worker the host can concurrently start, each a
			// full retry budget (attempts requests at RequestTimeout plus attempts
			// backoff waits at Retry.Maximum). It must never trip on a legitimate
			// multi-target, high-maxAttempts, or multi-worker-JIT step.
			ops := reconcileStepOpsPerTarget*tc.targets + reconcileStepJITOpsPerWorker*tc.maxConcurrentWorkers
			budget := time.Duration(ops*tc.maxAttempts) * (tc.requestTO + tc.backoffMax)
			if got <= budget {
				t.Fatalf("reconcileStepTimeout = %s, want > whole-step retry budget %s (targets=%d, maxAttempts=%d, maxConcurrentWorkers=%d)", got, budget, tc.targets, tc.maxAttempts, tc.maxConcurrentWorkers)
			}
		})
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
