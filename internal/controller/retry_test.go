package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/melodic-software/ci-runner/internal/scaleset"
)

func TestRetryUsesExponentialBackoffAndJitterHook(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		policy := BackoffPolicy{
			Initial:     time.Second,
			Maximum:     10 * time.Second,
			Multiplier:  2,
			JitterRatio: .2,
			MaxAttempts: 4,
			Jitter:      func(base time.Duration, _ float64) time.Duration { return base },
		}
		start := time.Now()
		var attemptAt []time.Duration
		value, err := RetryValue(context.Background(), policy, func(error) bool { return true }, func(context.Context) (string, error) {
			attemptAt = append(attemptAt, time.Since(start))
			if len(attemptAt) < 4 {
				return "", errors.New("transient")
			}
			return "ok", nil
		})
		if err != nil || value != "ok" {
			t.Fatalf("value=%q error=%v", value, err)
		}
		// Each attempt runs at the cumulative backoff elapsed so far, so the
		// per-attempt offsets encode the exact 1s/2s/4s doubling schedule the
		// injected fake clock used to record via Sleeps().
		want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}
		if len(attemptAt) != len(want) {
			t.Fatalf("attempts = %d, want %d (offsets %v)", len(attemptAt), len(want), attemptAt)
		}
		for i := range want {
			if attemptAt[i] != want[i] {
				t.Fatalf("attempt[%d] ran at +%s, want +%s (offsets %v)", i, attemptAt[i], want[i], attemptAt)
			}
		}
	})
}

func TestRetryHonorsServerRetryAfter(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		policy := BackoffPolicy{Initial: time.Second, Maximum: time.Minute, Multiplier: 2, MaxAttempts: 2}
		attempt := 0
		start := time.Now()
		_, err := RetryValue(context.Background(), policy, scaleset.Retryable, func(context.Context) (struct{}, error) {
			attempt++
			return struct{}{}, &scaleset.Error{Kind: scaleset.ErrorRateLimited, RetryAfterSeconds: 30}
		})
		if err == nil || attempt != 2 {
			t.Fatalf("attempt=%d error=%v", attempt, err)
		}
		// The single inter-attempt wait must honor the server's 30s Retry-After
		// rather than the 1s policy base; inside the bubble that wait is the
		// entire elapsed time.
		if elapsed := time.Since(start); elapsed != 30*time.Second {
			t.Fatalf("waited %s between attempts, want server Retry-After 30s", elapsed)
		}
	})
}

func TestRetryStopsOnCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	policy := BackoffPolicy{Initial: time.Second, Maximum: time.Second, Multiplier: 1, MaxAttempts: 4}
	_, err := RetryValue(ctx, policy, func(error) bool { return true }, func(context.Context) (struct{}, error) {
		cancel()
		return struct{}{}, errors.New("retry")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestRetryAfterCannotExceedConfiguredMaximum(t *testing.T) {
	t.Parallel()
	policy := BackoffPolicy{Initial: time.Second, Maximum: 45 * time.Second, Multiplier: 2, MaxAttempts: 2}
	delay := policy.delay(1, &scaleset.Error{Kind: scaleset.ErrorRateLimited, RetryAfterSeconds: int(^uint(0) >> 1)})
	if delay != policy.Maximum {
		t.Fatalf("delay = %s, want configured maximum %s", delay, policy.Maximum)
	}
}
