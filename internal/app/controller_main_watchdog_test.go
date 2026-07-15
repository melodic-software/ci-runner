package app

import (
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
)

func TestReconcileStepTimeoutScalesWithRequestTimeout(t *testing.T) {
	t.Parallel()
	requestTimeout := 70 * time.Second
	cfg := config.Config{GitHub: config.GitHub{RequestTimeout: config.Duration{Duration: requestTimeout}}}

	if got, want := reconcileStepTimeout(cfg), reconcileStepTimeoutFactor*requestTimeout; got != want {
		t.Fatalf("reconcileStepTimeout = %s, want %s", got, want)
	}

	// The watchdog must sit above a single GitHub call's full bounded retry budget
	// (Retry.MaxAttempts requests, each capped at RequestTimeout) so it fires only
	// on a genuine wedge and never on legitimate retries.
	maxAttempts := 6
	if budget := time.Duration(maxAttempts) * requestTimeout; reconcileStepTimeout(cfg) <= budget {
		t.Fatalf("watchdog %s does not exceed one call's retry budget %s", reconcileStepTimeout(cfg), budget)
	}
}
