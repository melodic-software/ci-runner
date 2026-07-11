package state

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/melodic-software/ci-runner/internal/model"
)

func TestMemoryStoreClonesValues(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	capacity := 3
	desired := model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, TemporaryCapacityOverride: &capacity}
	if err := store.SaveDesired(context.Background(), desired); err != nil {
		t.Fatal(err)
	}
	capacity = 99
	loaded, err := store.LoadDesired(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := *loaded.TemporaryCapacityOverride; got != 3 {
		t.Fatalf("capacity = %d, want 3", got)
	}
	*loaded.TemporaryCapacityOverride = 42
	reloaded, _ := store.LoadDesired(context.Background())
	if got := *reloaded.TemporaryCapacityOverride; got != 3 {
		t.Fatalf("store was mutated through returned pointer: %d", got)
	}
}

func TestMemoryStoreConcurrentAccess(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			_ = store.SaveObserved(ctx, model.ObservedState{SchemaVersion: 1, Pools: []model.PoolObservation{{MaxCapacity: value}}})
			_, _ = store.LoadObserved(ctx)
		}(i)
	}
	wg.Wait()
	if _, err := store.LoadObserved(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryStoreHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewMemoryStore().LoadDesired(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
