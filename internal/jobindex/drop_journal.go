package jobindex

import (
	"bytes"
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
	dropJournalFilename      = "jobs-drop.json"
	dropJournalSchemaVersion = 1
)

// DropEntry records a terminal job index record shed by tier-2 capacity
// compaction. Phase 2.1's artifact audit reads these entries to classify
// on-disk files whose catalog mapping was dropped under save-cap pressure.
type DropEntry struct {
	DroppedAt      time.Time `json:"droppedAt"`
	PoolID         string    `json:"poolId"`
	RunnerName     string    `json:"runnerName"`
	ContainerID    string    `json:"containerId,omitempty"`
	JobID          string    `json:"jobId,omitempty"`
	LogPath        string    `json:"logPath,omitempty"`
	DiagnosticPath string    `json:"diagnosticPath,omitempty"`
}

// DropJournal is the bounded on-disk sink beside jobs.json for tier-2 drops.
type DropJournal struct {
	SchemaVersion int         `json:"schemaVersion"`
	Entries       []DropEntry `json:"entries"`
}

// ArtifactPaths returns every artifact path the entry references, including
// the derived resource-evidence sidecar when a diagnostic path is present.
func (entry DropEntry) ArtifactPaths() []string {
	paths := make([]string, 0, 3)
	if entry.LogPath != "" {
		paths = append(paths, entry.LogPath)
	}
	if entry.DiagnosticPath != "" {
		paths = append(paths, entry.DiagnosticPath)
		if sidecar, err := ResourceEvidencePath(Record{DiagnosticPath: entry.DiagnosticPath}); err == nil && sidecar != "" {
			paths = append(paths, sidecar)
		}
	}
	return paths
}

// LoadDropJournal reads the drop journal from directory. A missing file is
// not an error and yields an empty journal.
func LoadDropJournal(directory string) (DropJournal, error) {
	path := filepath.Join(directory, dropJournalFilename)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return DropJournal{SchemaVersion: dropJournalSchemaVersion}, nil
	}
	if err != nil {
		return DropJournal{}, fmt.Errorf("open drop journal: %w", err)
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, maximumJobState+1))
	if err != nil {
		return DropJournal{}, fmt.Errorf("read drop journal: %w", err)
	}
	if len(contents) > maximumJobState {
		return DropJournal{}, fmt.Errorf("drop journal exceeds the %d-byte cap", maximumJobState)
	}
	var journal DropJournal
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return DropJournal{}, fmt.Errorf("decode drop journal: %w", err)
	}
	var trailer any
	if err := decoder.Decode(&trailer); !errors.Is(err, io.EOF) {
		if err == nil {
			return DropJournal{}, errors.New("decode drop journal: multiple JSON values are not allowed")
		}
		return DropJournal{}, fmt.Errorf("decode drop journal trailer: %w", err)
	}
	if journal.SchemaVersion != dropJournalSchemaVersion {
		return DropJournal{}, fmt.Errorf("unsupported drop journal schemaVersion %d", journal.SchemaVersion)
	}
	return journal, nil
}

func dropEntryFromRecord(record Record, droppedAt time.Time) DropEntry {
	return DropEntry{
		DroppedAt:      droppedAt.UTC(),
		PoolID:         record.PoolID,
		RunnerName:     record.RunnerName,
		ContainerID:    record.ContainerID,
		JobID:          record.JobID,
		LogPath:        record.LogPath,
		DiagnosticPath: record.DiagnosticPath,
	}
}

// appendDropJournal records tier-2 compaction drops beside jobs.json. It uses
// raw os calls because saveUnlocked holds the store lock and exported methods
// would deadlock. The journal is written only after jobs.json commits so a
// failed catalog save never leaves phantom drop entries; a crash between the
// two writes loses at most the latest batch, matching the pre-journal state.
// Journal write failures are ignored so compaction cannot reintroduce the
// permanent-write-failure livelock the save cap exists to prevent.
func appendDropJournal(directory string, acl AccessController, dropped []Record, droppedAt time.Time) {
	if len(dropped) == 0 {
		return
	}
	journal, err := LoadDropJournal(directory)
	if err != nil {
		return
	}
	for _, record := range dropped {
		journal.Entries = append(journal.Entries, dropEntryFromRecord(record, droppedAt))
	}
	encoded, err := encodeDropJournalWithinCapacity(journal)
	if err != nil {
		return
	}
	temporary, err := os.CreateTemp(directory, ".jobs-drop.json-*")
	if err != nil {
		return
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
		return
	}
	if acl != nil {
		_ = acl.Harden(temporaryPath)
		_ = acl.Verify(temporaryPath)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return
	}
	if err := temporary.Sync(); err != nil {
		return
	}
	if err := temporary.Close(); err != nil {
		return
	}
	target := filepath.Join(directory, dropJournalFilename)
	if err := statefs.ReplaceFileAtomic(temporaryPath, target); err != nil {
		return
	}
	committed = true
	if acl != nil {
		_ = acl.Harden(target)
		_ = acl.Verify(target)
	}
	_ = statefs.SyncDirectory(directory)
}

func encodeDropJournalWithinCapacity(journal DropJournal) ([]byte, error) {
	journal.SchemaVersion = dropJournalSchemaVersion
	for len(journal.Entries) > 0 {
		encoded, err := json.Marshal(journal)
		if err != nil {
			return nil, fmt.Errorf("encode drop journal: %w", err)
		}
		encoded = append(encoded, '\n')
		if len(encoded) <= maximumJobState {
			return encoded, nil
		}
		overshoot := len(encoded) - maximumJobState
		averageEntryBytes := max(len(encoded)/len(journal.Entries), 1)
		dropCount := min(overshoot/averageEntryBytes+1, len(journal.Entries))
		journal.Entries = journal.Entries[dropCount:]
	}
	encoded, err := json.Marshal(journal)
	if err != nil {
		return nil, fmt.Errorf("encode drop journal: %w", err)
	}
	return append(encoded, '\n'), nil
}
