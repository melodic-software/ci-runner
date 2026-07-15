package app

import (
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
)

func githubRetryConfig(requestTimeout, backoffMax time.Duration, maxAttempts int) config.Config {
	return config.Config{GitHub: config.GitHub{
		RequestTimeout: config.Duration{Duration: requestTimeout},
		Retry:          config.Retry{Maximum: config.Duration{Duration: backoffMax}, MaxAttempts: maxAttempts},
	}}
}

func TestReconcileStepTimeoutClearsConfiguredRetryBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		requestTO   time.Duration
		backoffMax  time.Duration
		maxAttempts int
	}{
		{name: "golden config", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 6},
		{name: "high maxAttempts", requestTO: 70 * time.Second, backoffMax: time.Minute, maxAttempts: 40},
		{name: "large backoff", requestTO: 30 * time.Second, backoffMax: 5 * time.Minute, maxAttempts: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := reconcileStepTimeout(githubRetryConfig(tc.requestTO, tc.backoffMax, tc.maxAttempts))
			// The watchdog must strictly exceed the worst-case budget of one fully
			// retried GitHub call (attempts requests at RequestTimeout plus attempts
			// backoff waits at Retry.Maximum) so it never trips on a legitimate retry
			// sequence, including when maxAttempts is configured well above the golden 6.
			budget := time.Duration(tc.maxAttempts) * (tc.requestTO + tc.backoffMax)
			if got <= budget {
				t.Fatalf("reconcileStepTimeout = %s, want > full retry budget %s for maxAttempts=%d", got, budget, tc.maxAttempts)
			}
		})
	}
}

func TestReconcileStepTimeoutFloorsPathologicalMaxAttempts(t *testing.T) {
	t.Parallel()
	floored := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, reconcileStepMinRetryAttempts))
	for _, attempts := range []int{0, 1} {
		if got := reconcileStepTimeout(githubRetryConfig(70*time.Second, time.Minute, attempts)); got != floored {
			t.Fatalf("maxAttempts=%d: reconcileStepTimeout = %s, want floored (min %d attempts) %s", attempts, got, reconcileStepMinRetryAttempts, floored)
		}
	}
}
