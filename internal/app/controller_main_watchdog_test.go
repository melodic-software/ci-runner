package app

import (
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
)

func githubRetryConfig(requestTimeout, backoffMax time.Duration, maxAttempts, targets, workers int) config.Config {
	return config.Config{
		GitHub: config.GitHub{
			RequestTimeout: config.Duration{Duration: requestTimeout},
			Retry:          config.Retry{Maximum: config.Duration{Duration: backoffMax}, MaxAttempts: maxAttempts},
			Targets:        make([]config.Target, targets),
		},
		Resources: config.Resources{MaximumConcurrentWorkers: workers},
	}
}

func TestReconcileStepTimeoutClearsConfiguredRetryBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		requestTO   time.Duration
		backoffMax  time.Duration
		maxAttempts int
		targets     int
		workers     int
	}{
		{name: "golden single target", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 6, targets: 1, workers: 1},
		{name: "high maxAttempts", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 40, targets: 1, workers: 1},
		{name: "large backoff", requestTO: 30 * time.Second, backoffMax: 5 * time.Minute, maxAttempts: 10, targets: 1, workers: 1},
		{name: "multi-target", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 6, targets: 3, workers: 1},
		{name: "large worker ceiling", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 6, targets: 3, workers: 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := reconcileStepTimeout(githubRetryConfig(tc.requestTO, tc.backoffMax, tc.maxAttempts, tc.targets, tc.workers))
			// The watchdog must strictly exceed the whole-step worst case: an
			// ensure+statistics sweep across every target PLUS the per-worker
			// start/verify/remove ops scaled by the worker ceiling, each a full
			// retry budget (attempts requests at RequestTimeout plus attempts
			// backoff waits at Retry.Maximum). It must never trip on a legitimate
			// multi-target, high-maxAttempts, or multi-worker step.
			ops := reconcileSweepOpsPerTarget*tc.targets + reconcileOpsPerWorker*tc.workers
			budget := time.Duration(ops*tc.maxAttempts) * (tc.requestTO + tc.backoffMax)
			if got <= budget {
				t.Fatalf("reconcileStepTimeout = %s, want > whole-step retry budget %s (targets=%d, workers=%d, maxAttempts=%d)", got, budget, tc.targets, tc.workers, tc.maxAttempts)
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

func TestReconcileStepTimeoutFloorsZeroWorkers(t *testing.T) {
	t.Parallel()
	if got, want := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 0)),
		reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1)); got != want {
		t.Fatalf("zero workers: reconcileStepTimeout = %s, want single-worker floor %s", got, want)
	}
}

func TestReconcileStepTimeoutScalesWithWorkerCeiling(t *testing.T) {
	t.Parallel()
	// Raising Resources.MaximumConcurrentWorkers must strictly increase the
	// deadline: a legitimate multi-worker provision after a demand spike makes
	// one JIT-config create, registration verify, and removal per worker, so a
	// larger ceiling means a longer worst-case sweep. This guards the
	// worker-scaling the watchdog previously ignored.
	previous := time.Duration(0)
	for _, workers := range []int{1, 2, 4, 8, 32} {
		got := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 3, workers))
		if got <= previous {
			t.Fatalf("workers=%d: reconcileStepTimeout = %s, want strictly greater than the smaller-ceiling deadline %s", workers, got, previous)
		}
		previous = got
	}
}

func TestReconcileStepTimeoutScalesWithTargets(t *testing.T) {
	t.Parallel()
	// The deadline must also strictly increase with the configured target count:
	// each target adds an ensure/statistics sweep to the worst-case step.
	previous := time.Duration(0)
	for _, targets := range []int{1, 2, 4, 8} {
		got := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, targets, 1))
		if got <= previous {
			t.Fatalf("targets=%d: reconcileStepTimeout = %s, want strictly greater than the smaller-target deadline %s", targets, got, previous)
		}
		previous = got
	}
}

func TestReconcileStepTimeoutIncludesDockerBudget(t *testing.T) {
	t.Parallel()
	// Docker Desktop bootstrap/drain is part of a legitimate step, so its
	// start+stop budget must widen the deadline beyond an otherwise identical
	// config with no Docker timeouts configured.
	base := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1))
	withDocker := githubRetryConfig(70*time.Second, time.Minute, 6, 1, 1)
	withDocker.DockerDesktop = config.DockerDesktop{
		StartTimeout: config.Duration{Duration: 3 * time.Minute},
		StopTimeout:  config.Duration{Duration: 2 * time.Minute},
	}
	if got := reconcileStepTimeout(withDocker); got <= base {
		t.Fatalf("with Docker start/stop budget: reconcileStepTimeout = %s, want strictly greater than the no-Docker deadline %s", got, base)
	}
}
