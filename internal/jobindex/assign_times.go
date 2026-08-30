package jobindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	statefs "github.com/melodic-software/ci-runner/internal/state/fs"
)

const (
	assignTimesFilename      = "runner-assign-times.json"
	assignTimesSchemaVersion = 1
)

// AssignTimesCatalog stores GitHub runner-assignment timestamps beside
// jobs.json. Schema-version-1 job records stay rollback-readable by older
// controllers that decode with DisallowUnknownFields; this sidecar is ignored
// by those releases and hydrated only by readers that know it.
type AssignTimesCatalog struct {
	SchemaVersion int               `json:"schemaVersion"`
	Entries       []AssignTimeEntry `json:"entries"`
}

type AssignTimeEntry struct {
	PoolID           string    `json:"poolId"`
	RunnerName       string    `json:"runnerName"`
	JobID            string    `json:"jobId,omitempty"`
	RunnerAssignedAt time.Time `json:"runnerAssignedAt"`
}

func loadAssignTimes(directory string) (AssignTimesCatalog, error) {
	path := filepath.Join(directory, assignTimesFilename)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return AssignTimesCatalog{SchemaVersion: assignTimesSchemaVersion}, nil
	}
	if err != nil {
		return AssignTimesCatalog{}, fmt.Errorf("open assign-times sidecar: %w", err)
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, maximumJobState+1))
	if err != nil {
		return AssignTimesCatalog{}, fmt.Errorf("read assign-times sidecar: %w", err)
	}
	if len(contents) > maximumJobState {
		return AssignTimesCatalog{}, fmt.Errorf("assign-times sidecar exceeds the %d-byte cap", maximumJobState)
	}
	var catalog AssignTimesCatalog
	if err := decodeStrictJSON(contents, "assign-times sidecar", &catalog); err != nil {
		return AssignTimesCatalog{}, err
	}
	if catalog.SchemaVersion != assignTimesSchemaVersion {
		return AssignTimesCatalog{}, fmt.Errorf("unsupported assign-times schemaVersion %d", catalog.SchemaVersion)
	}
	return catalog, nil
}

func hydrateAssignTimes(catalog *Catalog, times AssignTimesCatalog) {
	if catalog == nil || len(times.Entries) == 0 {
		return
	}
	byKey := make(map[string]time.Time, len(times.Entries))
	byJob := make(map[string]time.Time, len(times.Entries))
	for _, entry := range times.Entries {
		if entry.RunnerAssignedAt.IsZero() {
			continue
		}
		assigned := entry.RunnerAssignedAt.UTC()
		byKey[entry.PoolID+"\x00"+entry.RunnerName] = assigned
		if entry.JobID != "" {
			byJob[entry.JobID] = assigned
		}
	}
	for index := range catalog.Records {
		record := &catalog.Records[index]
		if record.RunnerAssignedAt != nil {
			continue
		}
		if assigned, ok := byKey[record.PoolID+"\x00"+record.RunnerName]; ok {
			record.RunnerAssignedAt = &assigned
			continue
		}
		if record.JobID == "" {
			continue
		}
		if assigned, ok := byJob[record.JobID]; ok {
			record.RunnerAssignedAt = &assigned
		}
	}
}

func assignTimesFromCatalog(catalog Catalog) AssignTimesCatalog {
	times := AssignTimesCatalog{SchemaVersion: assignTimesSchemaVersion, Entries: make([]AssignTimeEntry, 0, len(catalog.Records))}
	for _, record := range catalog.Records {
		if record.RunnerAssignedAt == nil || record.RunnerAssignedAt.IsZero() {
			continue
		}
		times.Entries = append(times.Entries, AssignTimeEntry{
			PoolID:           record.PoolID,
			RunnerName:       record.RunnerName,
			JobID:            record.JobID,
			RunnerAssignedAt: record.RunnerAssignedAt.UTC(),
		})
	}
	sort.SliceStable(times.Entries, func(i, j int) bool {
		if times.Entries[i].PoolID == times.Entries[j].PoolID {
			return times.Entries[i].RunnerName < times.Entries[j].RunnerName
		}
		return times.Entries[i].PoolID < times.Entries[j].PoolID
	})
	return times
}

// saveAssignTimesUnlocked writes the assignment sidecar after jobs.json commits.
// Failures are returned to the caller so Upsert does not claim success when the
// durable assignment evidence was lost; the jobs.json record remains valid.
func saveAssignTimesUnlocked(directory string, acl AccessController, catalog Catalog) error {
	times := assignTimesFromCatalog(catalog)
	if len(times.Entries) == 0 {
		path := filepath.Join(directory, assignTimesFilename)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty assign-times sidecar: %w", err)
		}
		return nil
	}
	encoded, err := json.MarshalIndent(times, "", "  ")
	if err != nil {
		return fmt.Errorf("encode assign-times sidecar: %w", err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maximumJobState {
		return fmt.Errorf("assign-times sidecar exceeds the %d-byte cap", maximumJobState)
	}
	temporary, err := os.CreateTemp(directory, ".runner-assign-times.json-*")
	if err != nil {
		return fmt.Errorf("create temporary assign-times sidecar: %w", err)
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
		return fmt.Errorf("secure temporary assign-times sidecar: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write temporary assign-times sidecar: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush temporary assign-times sidecar: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary assign-times sidecar: %w", err)
	}
	if err := acl.Harden(temporaryPath); err != nil {
		return fmt.Errorf("secure temporary assign-times sidecar ACL: %w", err)
	}
	if err := acl.Verify(temporaryPath); err != nil {
		return fmt.Errorf("verify temporary assign-times sidecar ACL: %w", err)
	}
	target := filepath.Join(directory, assignTimesFilename)
	if err := statefs.ReplaceFileAtomic(temporaryPath, target); err != nil {
		return fmt.Errorf("replace assign-times sidecar atomically: %w", err)
	}
	committed = true
	if err := acl.Harden(target); err != nil {
		return fmt.Errorf("verify assign-times sidecar ACL: %w", err)
	}
	if err := acl.Verify(target); err != nil {
		return fmt.Errorf("verify assign-times sidecar ACL: %w", err)
	}
	return nil
}
