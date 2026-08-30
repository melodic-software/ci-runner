// Package state defines persistence for desired and observed controller state.
// Atomic file replacement and the Windows mutex are adapter responsibilities.
package state

import (
	"context"
	"errors"
	"sync"

	"github.com/melodic-software/ci-runner/internal/model"
)

var (
	ErrNotFound = errors.New("state not found")
	// ErrCorruptObserved marks observed.json bytes that failed decode or
	// validation. Transient lock, open, read, and close failures are returned
	// without this sentinel so callers can retry without quarantining a valid
	// file.
	ErrCorruptObserved = errors.New("observed state is corrupt")
)

type Store interface {
	LoadDesired(context.Context) (model.DesiredState, error)
	SaveDesired(context.Context, model.DesiredState) error
	LoadObserved(context.Context) (model.ObservedState, error)
	SaveObserved(context.Context, model.ObservedState) error
}

// MemoryStore is a concurrency-safe fake for unit tests and embedding. Values
// are cloned on both read and write so callers cannot race through shared slices
// or pointer fields.
type MemoryStore struct {
	mu          sync.RWMutex
	desired     model.DesiredState
	observed    model.ObservedState
	hasDesired  bool
	hasObserved bool
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (s *MemoryStore) LoadDesired(ctx context.Context) (model.DesiredState, error) {
	if err := ctx.Err(); err != nil {
		return model.DesiredState{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.hasDesired {
		return model.DesiredState{}, ErrNotFound
	}
	return cloneDesired(s.desired), nil
}

func (s *MemoryStore) SaveDesired(ctx context.Context, desired model.DesiredState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.desired = cloneDesired(desired)
	s.hasDesired = true
	return nil
}

func (s *MemoryStore) LoadObserved(ctx context.Context) (model.ObservedState, error) {
	if err := ctx.Err(); err != nil {
		return model.ObservedState{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.hasObserved {
		return model.ObservedState{}, ErrNotFound
	}
	return cloneObserved(s.observed), nil
}

func (s *MemoryStore) SaveObserved(ctx context.Context, observed model.ObservedState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed = cloneObserved(observed)
	s.hasObserved = true
	return nil
}

func cloneDesired(in model.DesiredState) model.DesiredState {
	out := in
	out.TemporaryCapacityOverride = clonePointer(in.TemporaryCapacityOverride)
	return out
}

func cloneObserved(in model.ObservedState) model.ObservedState {
	out := in
	out.Pools = append([]model.PoolObservation(nil), in.Pools...)
	out.Workers = append([]model.Worker(nil), in.Workers...)
	out.Problems = append([]model.Problem(nil), in.Problems...)
	out.DrainStartedAt = clonePointer(in.DrainStartedAt)
	out.ResourceGate.HighCPUSince = clonePointer(in.ResourceGate.HighCPUSince)
	out.ResourceGate.HealthySince = clonePointer(in.ResourceGate.HealthySince)
	out.PowerGate.ACSince = clonePointer(in.PowerGate.ACSince)
	return out
}

// clonePointer copies the pointed-to value so callers cannot mutate stored
// state through a shared pointer. nil stays nil.
func clonePointer[T any](in *T) *T {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}
