package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/melodic-software/ci-runner/internal/model"
)

func TestNoteStaleHandshakeArmsListenerReregisterAtLimit(t *testing.T) {
	t.Parallel()
	reconciler := &Reconciler{}
	for i := range handshakeStaleCycleLimit - 1 {
		reconciler.noteStaleHandshake()
		if reconciler.reregisterListeners {
			t.Fatalf("reregister armed after %d stale cycles", i+1)
		}
	}
	reconciler.noteStaleHandshake()
	if !reconciler.reregisterListeners {
		t.Fatal("expected reregister after the stale-cycle limit")
	}
	reconciler.clearStaleHandshake()
	if reconciler.reregisterListeners || reconciler.staleHandshakeCycles != 0 {
		t.Fatalf("clear left cycles=%d reregister=%t", reconciler.staleHandshakeCycles, reconciler.reregisterListeners)
	}
}

func TestAlignAdvertisedCapacityHoldsAckedPools(t *testing.T) {
	t.Parallel()
	planned := map[string]int{"org": 3, "review": 2}
	alignAdvertisedCapacity(planned, map[string]int{"org": 0, "review": 1}, map[string]bool{"org": true, "review": false})
	if planned["org"] != 0 {
		t.Fatalf("acked org capacity = %d, want the just-acknowledged 0", planned["org"])
	}
	if planned["review"] != 2 {
		t.Fatalf("unacked review capacity = %d, want the fresh plan 2", planned["review"])
	}
}

func TestSuccessfulPollKeepsPlanAlignedWithAckedCapacity(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	logs := &testLogSink{}
	harness.controller.deps.Logs = logs
	result, err := harness.controller.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Observed.Pools) != 1 || !result.Observed.Pools[0].CapacityAcknowledged {
		t.Fatalf("expected one acknowledged pool: %#v", result.Observed.Pools)
	}
	acked := result.Observed.Pools[0].MaxCapacity
	if result.Plan.AdvertisedCapacity["org"] != acked {
		t.Fatalf("post-poll plan %d forked from acknowledged %d", result.Plan.AdvertisedCapacity["org"], acked)
	}
	if logs.contains("worker-removal-capacity-stale") {
		t.Fatalf("successful poll still recorded a capacity-stale retirement: %s", logs)
	}
}

func TestStaleHandshakeReregistersListenerSession(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	logs := &testLogSink{}
	harness.controller.deps.Logs = logs
	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := harness.scaleSets.Identities["org"].ListenerID
	if first == "" {
		t.Fatal("initial listener session is empty")
	}

	harness.controller.staleHandshakeCycles = handshakeStaleCycleLimit
	harness.controller.reregisterListeners = true
	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := harness.scaleSets.Identities["org"].ListenerID
	if second == "" || second == first {
		t.Fatalf("listener id %q was not rotated after a stale handshake", first)
	}
	if !logs.contains("listener-handshake-reregister") {
		t.Fatalf("missing reregister log: %s", logs)
	}
	if harness.controller.reregisterListeners || harness.controller.staleHandshakeCycles != 0 {
		t.Fatalf("handshake flags were not cleared: cycles=%d reregister=%t", harness.controller.staleHandshakeCycles, harness.controller.reregisterListeners)
	}
}

func TestFailedHandshakeReregisterStaysArmed(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	if _, err := harness.controller.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	harness.controller.staleHandshakeCycles = handshakeStaleCycleLimit
	harness.controller.reregisterListeners = true
	harness.scaleSets.Errors["ensure:org"] = errors.New("replacement session failed")
	_, _ = harness.controller.Step(context.Background())
	if !harness.controller.reregisterListeners {
		t.Fatal("failed listener re-registration cleared the recovery flag")
	}
}
