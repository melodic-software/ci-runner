// Package clock isolates time observation and cancellable waits so controller
// policy and retry behavior are deterministic in tests.
package clock

import (
	"context"
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) Sleep(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Fake advances immediately on Sleep. It is safe for concurrent tests.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func NewFake(now time.Time) *Fake { return &Fake{now: now} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleeps = append(f.sleeps, duration)
	f.now = f.now.Add(duration)
	return nil
}

func (f *Fake) Advance(duration time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(duration)
}

func (f *Fake) Set(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

func (f *Fake) Sleeps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.sleeps...)
}
