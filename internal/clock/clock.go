// Package clock provides a context-cancellable sleep. Time observation and
// deterministic retry/backoff timing in tests are now handled by the stdlib
// testing/synctest bubble clock, so production reads time.Now directly and no
// longer threads a Clock interface through its dependency graph.
package clock

import (
	"context"
	"time"
)

// Sleep waits for duration to elapse or ctx to be done, whichever comes first.
// The stdlib has no context.Sleep, so this remains the idiomatic context-aware
// wait rather than a reinvented wheel. A non-positive duration returns ctx.Err
// without waiting.
func Sleep(ctx context.Context, duration time.Duration) error {
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
