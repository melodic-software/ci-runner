// Package jobindex defines the durable, exact mapping between GitHub job
// lifecycle events and controller-owned worker artifacts.
package jobindex

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("job record not found")
	ErrConflict = errors.New("job record conflict")
)

const SchemaVersion = 1

type Catalog struct {
	SchemaVersion int      `json:"schemaVersion"`
	Records       []Record `json:"records"`
}

type Record struct {
	PoolID            string     `json:"poolId"`
	RunnerName        string     `json:"runnerName"`
	ContainerID       string     `json:"containerId,omitempty"`
	JobID             string     `json:"jobId,omitempty"`
	Result            string     `json:"result,omitempty"`
	LogPath           string     `json:"logPath,omitempty"`
	DiagnosticPath    string     `json:"diagnosticPath,omitempty"`
	ResourcePath      string     `json:"resourcePath,omitempty"`
	ArtifactStartedAt time.Time  `json:"artifactStartedAt,omitempty"`
	JobStartedAt      time.Time  `json:"jobStartedAt,omitempty"`
	CompletedAt       time.Time  `json:"completedAt,omitempty"`
	FinalizedAt       time.Time  `json:"finalizedAt,omitempty"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	Open              bool       `json:"open"`
	TombstonedAt      *time.Time `json:"tombstonedAt,omitempty"`
}

type Patch struct {
	PoolID            string
	RunnerName        string
	ContainerID       string
	JobID             string
	Result            string
	LogPath           string
	DiagnosticPath    string
	ResourcePath      string
	ArtifactStartedAt time.Time
	JobStartedAt      time.Time
	CompletedAt       time.Time
	FinalizedAt       time.Time
	Open              *bool
	TombstonedAt      *time.Time
}

type Store interface {
	Load(context.Context) (Catalog, error)
	Upsert(context.Context, Patch) (Record, error)
	FindByJobID(context.Context, string) (Record, error)
	FindByRunner(context.Context, string, string) (Record, error)
	PruneTombstones(context.Context, time.Time) (int, error)
	ActiveJob(context.Context, string, string) (string, bool, error)
}

func Merge(existing Record, patch Patch, now time.Time) (Record, error) {
	if strings.TrimSpace(patch.PoolID) == "" || strings.TrimSpace(patch.RunnerName) == "" {
		return Record{}, errors.New("job patch requires pool ID and runner name")
	}
	if now.IsZero() {
		return Record{}, errors.New("job patch time is required")
	}
	if existing.PoolID == "" {
		existing.PoolID = patch.PoolID
		existing.RunnerName = patch.RunnerName
	} else if existing.PoolID != patch.PoolID || existing.RunnerName != patch.RunnerName {
		return Record{}, fmt.Errorf("%w: pool/runner identity changed", ErrConflict)
	}
	if existing.TombstonedAt != nil {
		return existing, nil
	}
	var err error
	if existing.ContainerID, err = mergeImmutable("container ID", existing.ContainerID, patch.ContainerID); err != nil {
		return Record{}, err
	}
	if existing.JobID, err = mergeImmutable("job ID", existing.JobID, patch.JobID); err != nil {
		return Record{}, err
	}
	if existing.Result, err = mergeImmutable("job result", existing.Result, patch.Result); err != nil {
		return Record{}, err
	}
	if existing.LogPath, err = mergePath("worker log", existing.LogPath, patch.LogPath); err != nil {
		return Record{}, err
	}
	if existing.DiagnosticPath, err = mergePath("worker diagnostics", existing.DiagnosticPath, patch.DiagnosticPath); err != nil {
		return Record{}, err
	}
	if existing.ResourcePath, err = mergePath("worker resource evidence", existing.ResourcePath, patch.ResourcePath); err != nil {
		return Record{}, err
	}
	mergeTime := func(destination *time.Time, value time.Time) {
		if destination.IsZero() && !value.IsZero() {
			*destination = value.UTC()
		}
	}
	mergeTime(&existing.ArtifactStartedAt, patch.ArtifactStartedAt)
	mergeTime(&existing.JobStartedAt, patch.JobStartedAt)
	mergeTime(&existing.CompletedAt, patch.CompletedAt)
	mergeTime(&existing.FinalizedAt, patch.FinalizedAt)
	if patch.Open != nil {
		existing.Open = *patch.Open
	}
	if patch.TombstonedAt != nil {
		value := patch.TombstonedAt.UTC()
		existing.TombstonedAt = &value
		existing.Open = false
	}
	existing.UpdatedAt = now.UTC()
	return existing, nil
}

func Validate(catalog Catalog) error {
	if catalog.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported jobs schemaVersion %d", catalog.SchemaVersion)
	}
	keys := make(map[string]struct{}, len(catalog.Records))
	jobs := make(map[string]struct{}, len(catalog.Records))
	containers := make(map[string]struct{}, len(catalog.Records))
	for index, record := range catalog.Records {
		if record.PoolID == "" || record.RunnerName == "" || record.UpdatedAt.IsZero() {
			return fmt.Errorf("record %d requires poolId, runnerName, and updatedAt", index)
		}
		key := record.PoolID + "\x00" + record.RunnerName
		if _, duplicate := keys[key]; duplicate {
			return fmt.Errorf("duplicate pool/runner record %q", key)
		}
		keys[key] = struct{}{}
		if record.JobID != "" {
			if _, duplicate := jobs[record.JobID]; duplicate {
				return fmt.Errorf("duplicate job ID %q", record.JobID)
			}
			jobs[record.JobID] = struct{}{}
		}
		if record.ContainerID != "" {
			if _, duplicate := containers[record.ContainerID]; duplicate {
				return fmt.Errorf("duplicate container ID %q", record.ContainerID)
			}
			containers[record.ContainerID] = struct{}{}
		}
		for name, path := range map[string]string{"logPath": record.LogPath, "diagnosticPath": record.DiagnosticPath, "resourcePath": record.ResourcePath} {
			if path != "" && !filepath.IsAbs(path) {
				return fmt.Errorf("record %d %s must be absolute", index, name)
			}
		}
	}
	return nil
}

func Sort(catalog *Catalog) {
	sort.Slice(catalog.Records, func(i, j int) bool {
		if catalog.Records[i].PoolID == catalog.Records[j].PoolID {
			return catalog.Records[i].RunnerName < catalog.Records[j].RunnerName
		}
		return catalog.Records[i].PoolID < catalog.Records[j].PoolID
	})
}

func mergeImmutable(name, current, incoming string) (string, error) {
	if incoming == "" {
		return current, nil
	}
	if current != "" && current != incoming {
		return "", fmt.Errorf("%w: %s changed", ErrConflict, name)
	}
	return incoming, nil
}

func mergePath(name, current, incoming string) (string, error) {
	if incoming == "" {
		return current, nil
	}
	if !filepath.IsAbs(incoming) {
		return "", fmt.Errorf("%s path must be absolute", name)
	}
	return mergeImmutable(name+" path", current, filepath.Clean(incoming))
}

type EventSink struct {
	Store Store
	Now   func() time.Time
}

func (s EventSink) JobStarted(ctx context.Context, poolID, runnerName, jobID string) error {
	_, err := s.upsert(ctx, Patch{PoolID: poolID, RunnerName: runnerName, JobID: jobID, JobStartedAt: s.now()})
	return err
}

func (s EventSink) JobCompleted(ctx context.Context, poolID, runnerName, jobID, result string) error {
	_, err := s.upsert(ctx, Patch{PoolID: poolID, RunnerName: runnerName, JobID: jobID, Result: result, CompletedAt: s.now()})
	return err
}

func (s EventSink) upsert(ctx context.Context, patch Patch) (Record, error) {
	if s.Store == nil {
		return Record{}, errors.New("job event store is required")
	}
	return s.Store.Upsert(ctx, patch)
}

func (s EventSink) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
