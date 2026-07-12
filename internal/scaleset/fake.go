package scaleset

import (
	"context"
	"fmt"
	"sync"
)

// Fake is a race-safe programmable client for controller tests.
type Fake struct {
	mu sync.Mutex

	Identities     map[string]Identity
	Stats          map[string]Statistics
	Errors         map[string]error
	JIT            JITConfig
	Calls          []Call
	RemoveErr      error
	MissingRunners map[int64]bool
	RunnerErrors   map[int64]error
}

type Call struct {
	Operation   string
	TargetID    string
	ScaleSetID  int64
	MaxCapacity int
	RunnerName  string
}

func NewFake() *Fake {
	return &Fake{
		Identities:     map[string]Identity{},
		Stats:          map[string]Statistics{},
		Errors:         map[string]error{},
		JIT:            NewRunnerJITConfig([]byte("test-jit-config"), 101),
		MissingRunners: map[int64]bool{},
		RunnerErrors:   map[int64]error{},
	}
}

func (f *Fake) RunnerRegistered(ctx context.Context, poolID string, runnerID int64, runnerName string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Operation: "runner-registration", TargetID: poolID, ScaleSetID: runnerID, RunnerName: runnerName})
	if err := f.RunnerErrors[runnerID]; err != nil {
		return false, err
	}
	return !f.MissingRunners[runnerID], nil
}

func (f *Fake) Ensure(ctx context.Context, definition Definition, previous *Identity) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Operation: "ensure", TargetID: definition.TargetID})
	if err := f.Errors["ensure:"+definition.TargetID]; err != nil {
		return Identity{}, err
	}
	if identity, ok := f.Identities[definition.TargetID]; ok {
		return identity, nil
	}
	if previous != nil {
		return *previous, nil
	}
	identity := Identity{ScaleSetID: int64(len(f.Identities) + 1), ListenerID: fmt.Sprintf("listener-%s", definition.TargetID)}
	f.Identities[definition.TargetID] = identity
	return identity, nil
}

func (f *Fake) Statistics(ctx context.Context, identity Identity, maxCapacity int) (Statistics, error) {
	if err := ctx.Err(); err != nil {
		return Statistics{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Operation: "statistics", ScaleSetID: identity.ScaleSetID, MaxCapacity: maxCapacity})
	key := fmt.Sprintf("statistics:%d", identity.ScaleSetID)
	if err := f.Errors[key]; err != nil {
		return Statistics{}, err
	}
	return f.Stats[key], nil
}

func (f *Fake) CreateJITConfig(ctx context.Context, identity Identity, runnerName string) (JITConfig, error) {
	if err := ctx.Err(); err != nil {
		return JITConfig{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Operation: "jit", ScaleSetID: identity.ScaleSetID, RunnerName: runnerName})
	key := fmt.Sprintf("jit:%d", identity.ScaleSetID)
	if err := f.Errors[key]; err != nil {
		return JITConfig{}, err
	}
	return NewRunnerJITConfig(f.JIT.Reveal(), f.JIT.RunnerID()), nil
}

func (f *Fake) RemoveRunner(ctx context.Context, poolID string, runnerID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Operation: "remove-runner", TargetID: poolID, ScaleSetID: runnerID})
	return f.RemoveErr
}

func (f *Fake) SnapshotCalls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.Calls...)
}
