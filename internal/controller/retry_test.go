package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	clockpkg "github.com/melodic-software/ci-runner/internal/clock"
	"github.com/melodic-software/ci-runner/internal/scaleset"
)

func TestRetryUsesExponentialBackoffAndJitterHook(t *testing.T) {
	t.Parallel()
	clock := clockpkg.NewFake(time.Unix(0, 0))
	policy := BackoffPolicy{
		Initial:     time.Second,
		Maximum:     10 * time.Second,
		Multiplier:  2,
		JitterRatio: .2,
		MaxAttempts: 4,
		Jitter:      func(base time.Duration, _ float64) time.Duration { return base },
	}
	attempts := 0
	value, err := RetryValue(context.Background(), clock, policy, func(error) bool { return true }, func(context.Context) (string, error) {
		attempts++
		if attempts < 4 {
			return "", errors.New("transient")
		}
		return "ok", nil
	})
	if err != nil || value != "ok" {
		t.Fatalf("value=%q error=%v", value, err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	got := clock.Sleeps()
	if len(got) != len(want) {
		t.Fatalf("sleeps = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sleep[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestRetryHonorsServerRetryAfter(t *testing.T) {
	t.Parallel()
	clock := clockpkg.NewFake(time.Unix(0, 0))
	policy := BackoffPolicy{Initial: time.Second, Maximum: time.Minute, Multiplier: 2, MaxAttempts: 2}
	attempt := 0
	_, err := RetryValue(context.Background(), clock, policy, scaleset.Retryable, func(context.Context) (struct{}, error) {
		attempt++
		return struct{}{}, &scaleset.Error{Kind: scaleset.ErrorRateLimited, RetryAfterSeconds: 30}
	})
	if err == nil || attempt != 2 {
		t.Fatalf("attempt=%d error=%v", attempt, err)
	}
	if got := clock.Sleeps(); len(got) != 1 || got[0] != 30*time.Second {
		t.Fatalf("sleeps = %v", got)
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

func TestRetryStopsOnCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	clock := &cancelClock{cancel: cancel}
	policy := BackoffPolicy{Initial: time.Second, Maximum: time.Second, Multiplier: 1, MaxAttempts: 4}
	_, err := RetryValue(ctx, clock, policy, func(error) bool { return true }, func(context.Context) (struct{}, error) {
		return struct{}{}, errors.New("retry")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

type cancelClock struct{ cancel context.CancelFunc }

func (*cancelClock) Now() time.Time { return time.Unix(0, 0) }
func (c *cancelClock) Sleep(ctx context.Context, _ time.Duration) error {
	c.cancel()
	return ctx.Err()
}
