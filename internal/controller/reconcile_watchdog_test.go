package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/scaleset"
)

// blockingListener wedges the scale-set listener poll until its context is
// cancelled, standing in for a half-open long-poll socket that never returns.
type blockingListener struct {
	*scaleset.Fake
	entered chan struct{}
	once    sync.Once
}

func (b *blockingListener) Statistics(ctx context.Context, _ scaleset.Identity, _ int) (scaleset.Statistics, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return scaleset.Statistics{}, ctx.Err()
}

// A deadline-less Step let a wedged listener poll park the controller for hours.
// Bounding the step context must abort the poll and unblock Step at the deadline.
func TestStepAbortsWedgedListenerPollAtContextDeadline(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	listener := &blockingListener{Fake: harness.scaleSets, entered: make(chan struct{})}
	harness.controller.deps.ScaleSets = listener

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := harness.controller.Step(ctx)
		done <- err
	}()

	select {
	case <-listener.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("listener poll was never reached")
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Step error = %v, want context.DeadlineExceeded once the bounded step context aborts the wedged poll", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Step did not return after its context deadline; a wedged listener poll parked the reconcile")
	}
}
