// Package statefs provides the durable filesystem implementation of the
// controller state.Store contract.
package statefs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

const (
	desiredFilename  = "desired.json"
	observedFilename = "observed.json"
	maximumStateSize = 8 << 20
)

type Locker interface {
	Lock(context.Context) (func() error, error)
}

type AccessController interface {
	Harden(string) error
}

// ReplaceFileAtomic and SyncDirectory expose the same platform durability
// primitives to other state files (notably jobs.json) without duplicating
// Windows MoveFileExW semantics.
func ReplaceFileAtomic(source, target string) error { return atomicReplace(source, target) }
func SyncDirectory(path string) error               { return syncDirectory(path) }

type Store struct {
	directory string
	locker    Locker
	acl       AccessController
}

func New(directory string, locker Locker, acl AccessController) (*Store, error) {
	if directory == "" {
		return nil, errors.New("state directory is required")
	}
	if !filepath.IsAbs(directory) {
		return nil, errors.New("state directory must be absolute")
	}
	if locker == nil || acl == nil {
		return nil, errors.New("state locker and access controller are required")
	}
	return &Store{directory: filepath.Clean(directory), locker: locker, acl: acl}, nil
}

func (s *Store) LoadDesired(ctx context.Context) (model.DesiredState, error) {
	var value model.DesiredState
	if err := s.load(ctx, desiredFilename, &value); err != nil {
		return model.DesiredState{}, err
	}
	if err := validateDesired(value); err != nil {
		return model.DesiredState{}, fmt.Errorf("invalid desired state: %w", err)
	}
	return value, nil
}

func (s *Store) SaveDesired(ctx context.Context, value model.DesiredState) error {
	if err := validateDesired(value); err != nil {
		return fmt.Errorf("invalid desired state: %w", err)
	}
	return s.save(ctx, desiredFilename, value)
}

func (s *Store) LoadObserved(ctx context.Context) (model.ObservedState, error) {
	var value model.ObservedState
	if err := s.load(ctx, observedFilename, &value); err != nil {
		return model.ObservedState{}, err
	}
	if err := validateObserved(value); err != nil {
		return model.ObservedState{}, fmt.Errorf("invalid observed state: %w", err)
	}
	return value, nil
}

func (s *Store) SaveObserved(ctx context.Context, value model.ObservedState) error {
	if err := validateObserved(value); err != nil {
		return fmt.Errorf("invalid observed state: %w", err)
	}
	return s.save(ctx, observedFilename, value)
}

// QuarantineObserved preserves the exact corrupt observed.json bytes as a
// same-volume hard link while leaving the source in place. Leaving the corrupt
// source until a later successful atomic SaveObserved means a crash can never
// turn recovery into an unsafe "state missing" startup.
func (s *Store) QuarantineObserved(ctx context.Context) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := s.locker.Lock(ctx)
	if err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("unlock state: %w", unlockErr))
		}
	}()
	source := filepath.Join(s.directory, observedFilename)
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("inspect corrupt observed state: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	var evidence string
	for attempt := 0; attempt < 100; attempt++ {
		evidence = filepath.Join(s.directory, fmt.Sprintf("observed.corrupt-%s-%d.json", stamp, attempt))
		err = os.Link(source, evidence)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("preserve corrupt observed state: %w", err)
		}
	}
	if err != nil {
		return errors.New("preserve corrupt observed state: unique evidence name exhausted")
	}
	if err := s.acl.Harden(evidence); err != nil {
		return fmt.Errorf("secure corrupt observed evidence: %w", err)
	}
	if err := syncDirectory(s.directory); err != nil {
		return fmt.Errorf("flush corrupt observed evidence: %w", err)
	}
	return nil
}

func (s *Store) load(ctx context.Context, name string, destination any) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := s.locker.Lock(ctx)
	if err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("unlock state: %w", unlockErr))
		}
	}()

	file, err := os.Open(filepath.Join(s.directory, name))
	if errors.Is(err, os.ErrNotExist) {
		return state.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer file.Close()
	limited := io.LimitReader(file, maximumStateSize+1)
	contents, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if len(contents) > maximumStateSize {
		return fmt.Errorf("%s exceeds the %d-byte safety limit", name, maximumStateSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	var trailer any
	if err := decoder.Decode(&trailer); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode %s: multiple JSON values are not allowed", name)
		}
		return fmt.Errorf("decode %s trailer: %w", name, err)
	}
	return nil
}

func (s *Store) save(ctx context.Context, name string, value any) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := s.locker.Lock(ctx)
	if err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("unlock state: %w", unlockErr))
		}
	}()

	if err := os.MkdirAll(s.directory, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := s.acl.Harden(s.directory); err != nil {
		return fmt.Errorf("secure state directory: %w", err)
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(s.directory, "."+name+"-*")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", name, err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary %s permissions: %w", name, err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write temporary %s: %w", name, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush temporary %s: %w", name, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary %s: %w", name, err)
	}
	if err := s.acl.Harden(temporaryPath); err != nil {
		return fmt.Errorf("secure temporary %s: %w", name, err)
	}
	target := filepath.Join(s.directory, name)
	if err := atomicReplace(temporaryPath, target); err != nil {
		return fmt.Errorf("replace %s atomically: %w", name, err)
	}
	committed = true
	if err := s.acl.Harden(target); err != nil {
		return fmt.Errorf("verify %s ACL: %w", name, err)
	}
	if err := syncDirectory(s.directory); err != nil {
		return fmt.Errorf("flush state directory: %w", err)
	}
	return nil
}

func validateDesired(value model.DesiredState) error {
	if value.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schemaVersion %d", value.SchemaVersion)
	}
	if !value.Mode.Valid() {
		return fmt.Errorf("unsupported mode %q", value.Mode)
	}
	if value.TemporaryCapacityOverride != nil && *value.TemporaryCapacityOverride < 0 {
		return errors.New("temporaryCapacityOverride must not be negative")
	}
	if value.UpdatedAt.IsZero() {
		return errors.New("updatedAt is required")
	}
	return nil
}

func validateObserved(value model.ObservedState) error {
	if value.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schemaVersion %d", value.SchemaVersion)
	}
	if !validPhase(value.Phase) {
		return fmt.Errorf("unsupported phase %q", value.Phase)
	}
	if value.HeartbeatAt.IsZero() {
		return errors.New("heartbeatAt is required")
	}
	workerIDs := make(map[string]struct{}, len(value.Workers))
	for _, worker := range value.Workers {
		if worker.ID == "" || worker.PoolID == "" {
			return errors.New("workers require id and poolId")
		}
		if _, duplicate := workerIDs[worker.ID]; duplicate {
			return fmt.Errorf("duplicate worker ID %q", worker.ID)
		}
		workerIDs[worker.ID] = struct{}{}
		switch worker.State {
		case model.WorkerStarting, model.WorkerIdle, model.WorkerBusy, model.WorkerUnregistered, model.WorkerExited:
		default:
			return fmt.Errorf("worker %q has unsupported state %q", worker.ID, worker.State)
		}
	}
	poolIDs := make(map[string]struct{}, len(value.Pools))
	for _, pool := range value.Pools {
		if pool.ID == "" {
			return errors.New("pools require id")
		}
		if _, duplicate := poolIDs[pool.ID]; duplicate {
			return fmt.Errorf("duplicate pool ID %q", pool.ID)
		}
		poolIDs[pool.ID] = struct{}{}
		if pool.TotalAssignedJobs < 0 || pool.MaxCapacity < 0 || pool.DrainServiceCapacity < 0 || pool.DesiredWorkers < 0 {
			return fmt.Errorf("pool %q contains a negative count", pool.ID)
		}
	}
	return nil
}

func validPhase(value model.Phase) bool {
	switch value {
	case model.PhaseStarting, model.PhaseReady, model.PhaseResourceConstrained,
		model.PhasePowerSuspended, model.PhaseDraining, model.PhaseDisabled,
		model.PhaseGaming, model.PhaseDegraded:
		return true
	default:
		return false
	}
}

var _ state.Store = (*Store)(nil)
