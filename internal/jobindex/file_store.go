package jobindex

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

	statefs "github.com/melodic-software/ci-runner/internal/state/fs"
)

const (
	jobsFilename    = "jobs.json"
	maximumJobState = 8 << 20
)

type FileStore struct {
	directory string
	locker    statefs.Locker
	acl       AccessController
	now       func() time.Time
}

type AccessController interface {
	Harden(string) error
	Verify(string) error
}

func NewFileStore(directory string, locker statefs.Locker, acl AccessController) (*FileStore, error) {
	if !filepath.IsAbs(directory) || locker == nil || acl == nil {
		return nil, errors.New("job index requires an absolute state directory, locker, and access controller")
	}
	return &FileStore{
		directory: filepath.Clean(directory), locker: locker, acl: acl,
		now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (s *FileStore) PruneTombstones(ctx context.Context, before time.Time) (removed int, resultErr error) {
	if before.IsZero() {
		return 0, errors.New("tombstone prune cutoff is required")
	}
	unlock, err := s.locker.Lock(ctx)
	if err != nil {
		return 0, fmt.Errorf("lock jobs index: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	catalog, err := s.loadUnlocked()
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	kept := catalog.Records[:0]
	for _, record := range catalog.Records {
		if record.TombstonedAt != nil && !record.TombstonedAt.After(before) {
			removed++
			continue
		}
		kept = append(kept, record)
	}
	if removed == 0 {
		return 0, nil
	}
	catalog.Records = kept
	if err := s.saveUnlocked(catalog); err != nil {
		return 0, err
	}
	return removed, nil
}

func (s *FileStore) Load(ctx context.Context) (Catalog, error) {
	unlock, err := s.locker.Lock(ctx)
	if err != nil {
		return Catalog{}, fmt.Errorf("lock jobs index: %w", err)
	}
	value, loadErr := s.loadUnlocked()
	return value, errors.Join(loadErr, unlock())
}

func (s *FileStore) FindByJobID(ctx context.Context, jobID string) (Record, error) {
	if jobID == "" {
		return Record{}, ErrNotFound
	}
	catalog, err := s.Load(ctx)
	if err != nil {
		return Record{}, err
	}
	for _, record := range catalog.Records {
		if record.JobID == jobID && record.TombstonedAt == nil {
			return record, nil
		}
	}
	return Record{}, ErrNotFound
}

func (s *FileStore) FindByRunner(ctx context.Context, poolID, runnerName string) (Record, error) {
	if poolID == "" || runnerName == "" {
		return Record{}, ErrNotFound
	}
	catalog, err := s.Load(ctx)
	if err != nil {
		return Record{}, err
	}
	for _, record := range catalog.Records {
		if record.PoolID == poolID && record.RunnerName == runnerName && record.TombstonedAt == nil {
			return record, nil
		}
	}
	return Record{}, ErrNotFound
}

func (s *FileStore) ActiveJob(ctx context.Context, poolID, runnerName string) (string, bool, error) {
	if poolID == "" || runnerName == "" {
		return "", false, nil
	}
	catalog, err := s.Load(ctx)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, record := range catalog.Records {
		if record.PoolID != poolID || record.RunnerName != runnerName {
			continue
		}
		active := record.JobID != "" && !record.JobStartedAt.IsZero() && record.CompletedAt.IsZero()
		if active && record.TombstonedAt != nil {
			return "", false, fmt.Errorf("%w: active job %q is tombstoned", ErrConflict, record.JobID)
		}
		return record.JobID, active, nil
	}
	return "", false, nil
}

func (s *FileStore) Upsert(ctx context.Context, patch Patch) (result Record, resultErr error) {
	unlock, err := s.locker.Lock(ctx)
	if err != nil {
		return Record{}, fmt.Errorf("lock jobs index: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	catalog, err := s.loadUnlocked()
	if errors.Is(err, ErrNotFound) {
		catalog = Catalog{SchemaVersion: SchemaVersion}
	} else if err != nil {
		return Record{}, err
	}
	index := -1
	for i, record := range catalog.Records {
		if record.PoolID == patch.PoolID && record.RunnerName == patch.RunnerName {
			index = i
			break
		}
	}
	var current Record
	if index >= 0 {
		current = catalog.Records[index]
	}
	merged, err := Merge(current, patch, s.now())
	if err != nil {
		return Record{}, err
	}
	if index < 0 {
		catalog.Records = append(catalog.Records, merged)
	} else {
		catalog.Records[index] = merged
	}
	Sort(&catalog)
	if err := Validate(catalog); err != nil {
		return Record{}, fmt.Errorf("validate jobs index: %w", err)
	}
	if err := s.saveUnlocked(catalog); err != nil {
		return Record{}, err
	}
	return merged, nil
}

func (s *FileStore) loadUnlocked() (Catalog, error) {
	file, err := os.Open(filepath.Join(s.directory, jobsFilename))
	if errors.Is(err, os.ErrNotExist) {
		return Catalog{}, ErrNotFound
	}
	if err != nil {
		return Catalog{}, fmt.Errorf("open jobs.json: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maximumJobState+1))
	if err != nil {
		return Catalog{}, fmt.Errorf("read jobs.json: %w", err)
	}
	if len(contents) > maximumJobState {
		return Catalog{}, fmt.Errorf("jobs.json exceeds the %d-byte safety limit", maximumJobState)
	}
	var catalog Catalog
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode jobs.json: %w", err)
	}
	var trailer any
	if err := decoder.Decode(&trailer); !errors.Is(err, io.EOF) {
		if err == nil {
			return Catalog{}, errors.New("decode jobs.json: multiple JSON values are not allowed")
		}
		return Catalog{}, fmt.Errorf("decode jobs.json trailer: %w", err)
	}
	if err := Validate(catalog); err != nil {
		return Catalog{}, fmt.Errorf("invalid jobs.json: %w", err)
	}
	return catalog, nil
}

func (s *FileStore) saveUnlocked(catalog Catalog) error {
	if err := os.MkdirAll(s.directory, 0o700); err != nil {
		return fmt.Errorf("create jobs state directory: %w", err)
	}
	if err := s.acl.Harden(s.directory); err != nil {
		return fmt.Errorf("secure jobs state directory: %w", err)
	}
	if err := s.acl.Verify(s.directory); err != nil {
		return fmt.Errorf("verify jobs state directory ACL: %w", err)
	}
	encoded, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return fmt.Errorf("encode jobs.json: %w", err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maximumJobState {
		return fmt.Errorf("jobs.json exceeds the %d-byte safety limit", maximumJobState)
	}
	temporary, err := os.CreateTemp(s.directory, ".jobs.json-*")
	if err != nil {
		return fmt.Errorf("create temporary jobs.json: %w", err)
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
		return fmt.Errorf("secure temporary jobs.json: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write temporary jobs.json: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush temporary jobs.json: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary jobs.json: %w", err)
	}
	if err := s.acl.Harden(temporaryPath); err != nil {
		return fmt.Errorf("secure temporary jobs.json ACL: %w", err)
	}
	if err := s.acl.Verify(temporaryPath); err != nil {
		return fmt.Errorf("verify temporary jobs.json ACL: %w", err)
	}
	target := filepath.Join(s.directory, jobsFilename)
	if err := statefs.ReplaceFileAtomic(temporaryPath, target); err != nil {
		return fmt.Errorf("replace jobs.json atomically: %w", err)
	}
	committed = true
	if err := s.acl.Harden(target); err != nil {
		return fmt.Errorf("verify jobs.json ACL: %w", err)
	}
	if err := s.acl.Verify(target); err != nil {
		return fmt.Errorf("verify jobs.json ACL: %w", err)
	}
	if err := statefs.SyncDirectory(s.directory); err != nil {
		return fmt.Errorf("flush jobs state directory: %w", err)
	}
	return nil
}

var _ Store = (*FileStore)(nil)
