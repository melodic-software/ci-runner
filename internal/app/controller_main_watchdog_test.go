package app

import (
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
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
