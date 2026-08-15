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
	"sort"
	"time"

	statefs "github.com/melodic-software/ci-runner/internal/state/fs"
)

const (
	jobsFilename    = "jobs.json"
	maximumJobState = 8 << 20
	// The load tolerance exceeds the save cap so a file that breached the cap
	// under an older controller (or was restored from a backup) still loads;
	// the next save compacts it back under the cap instead of bricking the
	// index until someone hand-edits state.
	maximumJobStateLoad = 4 * maximumJobState

	// MaximumJobStateBytes exports the save cap for read-only observers
	// (health watch), which compare record subsets against it rather than
	// restating the limit. It is the ceiling saves commit under, not the 4x
	// load tolerance above.
	MaximumJobStateBytes = maximumJobState
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

func (s *FileStore) JobsStateDirectory() string { return s.directory }

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
		// A tombstoned record is dead bookkeeping and must not shadow its key,
		// matching FindByJobID and FindByRunner. It can legitimately still look
		// active: FinalizedAt and CompletedAt have independent producers, so a
		// lost completion event leaves JobID and JobStartedAt set with
		// CompletedAt zero, and retention tombstones it on FinalizedAt alone.
		if record.PoolID != poolID || record.RunnerName != runnerName || record.TombstonedAt != nil {
			continue
		}
		return record.JobID, record.JobID != "" && !record.JobStartedAt.IsZero() && record.CompletedAt.IsZero(), nil
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

// SnapshotBytes returns one committed jobs.json document, taken under the
// store lock so the read can never collide with a concurrent save's atomic
// replace (see readCatalogBytes for why out-of-lock reads are unsafe on
// Windows). A missing file returns ErrNotFound. It is the read surface for
// out-of-process observers (health watch); the raw bytes carry the on-disk
// size, and DecodeCatalog turns them into records.
func (s *FileStore) SnapshotBytes(ctx context.Context) (contents []byte, resultErr error) {
	unlock, err := s.locker.Lock(ctx)
	if err != nil {
		return nil, fmt.Errorf("lock jobs index: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, unlock()) }()
	contents, err = readCatalogBytes(filepath.Join(s.directory, jobsFilename))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return contents, nil
}

func (s *FileStore) loadUnlocked() (Catalog, error) {
	contents, err := readCatalogBytes(filepath.Join(s.directory, jobsFilename))
	if errors.Is(err, os.ErrNotExist) {
		return Catalog{}, ErrNotFound
	}
	if err != nil {
		return Catalog{}, err
	}
	catalog, err := DecodeCatalog(contents)
	if err != nil {
		return Catalog{}, err
	}
	times, err := loadAssignTimes(s.directory)
	if err != nil {
		return Catalog{}, err
	}
	hydrateAssignTimes(&catalog, times)
	return catalog, nil
}

// readCatalogBytes reads a jobs.json bounded by the load safety limit, so a
// runaway file never fills memory before DecodeCatalog rejects it. The
// caller must hold the store lock: on Windows any open handle on the file —
// share mode notwithstanding, verified empirically against MoveFileEx — makes
// a concurrent save's atomic replace fail with a sharing violation, so an
// out-of-lock read turns a read-only observer into a writer fault. A missing
// file surfaces as os.ErrNotExist.
func readCatalogBytes(path string) (contents []byte, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("open jobs.json: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close jobs.json: %w", closeErr))
		}
	}()
	contents, err = io.ReadAll(io.LimitReader(file, maximumJobStateLoad+1))
	if err != nil {
		return nil, fmt.Errorf("read jobs.json: %w", err)
	}
	return contents, nil
}

// DecodeCatalog strictly decodes one jobs.json document and validates it,
// enforcing the load safety limit. It performs no locking and no
// assign-times hydration, so read-only observers (health watch) can decode a
// snapshot without contending with the live controller.
func DecodeCatalog(contents []byte) (Catalog, error) {
	if len(contents) > maximumJobStateLoad {
		return Catalog{}, fmt.Errorf("jobs.json exceeds the %d-byte load safety limit", maximumJobStateLoad)
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
	encoded, dropped, err := encodeWithinCapacity(&catalog)
	if err != nil {
		return err
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
	appendDropJournal(s.directory, s.acl, dropped, s.now())
	if err := saveAssignTimesUnlocked(s.directory, s.acl, catalog); err != nil {
		return err
	}
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

// encodeWithinCapacity marshals the catalog, compacting records when the
// encoding would exceed the save cap. Tombstoned records go first: they are
// dead bookkeeping whose only remaining value is retention history. When no
// tombstones remain, the oldest completed records go next — the catalog keys
// one record per ephemeral JIT runner per job, so completed records grow with
// job churn (not concurrent worker count) and can saturate the cap before the
// artifact-retention pass ever tombstones them. In both tiers capacity
// pressure outranks the retention window: without compaction, jobs.json
// reaches the safety limit, every subsequent index write fails permanently,
// and worker finalization retries livelock reconciliation. The cap can then
// only be exceeded by open or still-running records alone, which the
// concurrent worker ceiling makes unreachable at supported worker counts.
func encodeWithinCapacity(catalog *Catalog) ([]byte, []Record, error) {
	var dropped []Record
	for {
		encoded, err := encodeCatalog(*catalog)
		if err != nil {
			return nil, nil, err
		}
		if len(encoded) <= maximumJobState {
			return encoded, dropped, nil
		}
		overshoot := len(encoded) - maximumJobState
		if compactOldestTombstones(catalog, overshoot, len(encoded)) != 0 {
			continue
		}
		if removed := compactOldestCompleted(catalog, overshoot, len(encoded)); len(removed) != 0 {
			dropped = append(dropped, removed...)
			continue
		}
		return nil, nil, fmt.Errorf("jobs.json exceeds the %d-byte safety limit with no tombstoned or completed records left to compact", maximumJobState)
	}
}

// encodeCatalog is the one jobs.json encoding: every byte count compared
// against MaximumJobStateBytes must come from this shape.
func encodeCatalog(catalog Catalog) ([]byte, error) {
	encoded, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode jobs.json: %w", err)
	}
	return append(encoded, '\n'), nil
}

// EncodedCatalogSize returns the byte size the catalog would occupy on disk,
// using the same encoding a save commits, so callers can compare a record
// subset against MaximumJobStateBytes without re-deriving the format.
func EncodedCatalogSize(catalog Catalog) (int, error) {
	encoded, err := encodeCatalog(catalog)
	if err != nil {
		return 0, err
	}
	return len(encoded), nil
}

// compactOldestTombstones removes the oldest tombstoned records, sized from
// the average encoded record so one pass usually suffices; the caller's
// re-encode loop guarantees convergence regardless.
func compactOldestTombstones(catalog *Catalog, overshootBytes, encodedBytes int) (removed int) {
	if len(catalog.Records) == 0 {
		return 0
	}
	tombstoned := make([]int, 0, len(catalog.Records))
	for i, record := range catalog.Records {
		if record.TombstonedAt != nil {
			tombstoned = append(tombstoned, i)
		}
	}
	if len(tombstoned) == 0 {
		return 0
	}
	sort.Slice(tombstoned, func(a, b int) bool {
		left, right := catalog.Records[tombstoned[a]], catalog.Records[tombstoned[b]]
		if !left.TombstonedAt.Equal(*right.TombstonedAt) {
			return left.TombstonedAt.Before(*right.TombstonedAt)
		}
		if left.PoolID != right.PoolID {
			return left.PoolID < right.PoolID
		}
		return left.RunnerName < right.RunnerName
	})
	return len(compactOldestFirst(catalog, tombstoned, overshootBytes, encodedBytes))
}

// terminalTime is the tier-2 eviction ordering timestamp.
func terminalTime(record Record) time.Time {
	if !record.FinalizedAt.IsZero() {
		return record.FinalizedAt
	}
	return record.CompletedAt
}

// CompactableUnderCapacityPressure reports whether a save that breaches
// MaximumJobStateBytes may reclaim the record: tombstoned records (tier 1)
// and closed terminal records that are not still the durable active-job
// mapping (tier 2). A finalized record whose job completion has not arrived
// yet is still the mapping ActiveJob and worker enrichment rely on; evicting
// it would unmark a busy worker until the event lands. Records outside this
// predicate are the only ones that can permanently wedge index writes, which
// makes it the boundary between designed cap saturation and a genuine
// capacity fault for read-only observers (health watch).
func CompactableUnderCapacityPressure(record Record) bool {
	if record.TombstonedAt != nil {
		return true
	}
	if record.Open || terminalTime(record).IsZero() {
		return false
	}
	return record.JobID == "" || record.JobStartedAt.IsZero() || !record.CompletedAt.IsZero()
}

// compactOldestCompleted removes the oldest terminal (closed and completed or
// finalized, never tombstoned) records once no tombstones remain to compact.
// Evicting a terminal record before its artifacts are cleaned leaves those
// files to the age-based orphan sweep instead of record-driven cleanup — an
// accepted trade against livelocking the index. Open records and records
// without a terminal marker are never touched here.
func compactOldestCompleted(catalog *Catalog, overshootBytes, encodedBytes int) (removed []Record) {
	if len(catalog.Records) == 0 {
		return nil
	}
	completed := make([]int, 0, len(catalog.Records))
	for i, record := range catalog.Records {
		if record.TombstonedAt != nil || !CompactableUnderCapacityPressure(record) {
			continue
		}
		completed = append(completed, i)
	}
	if len(completed) == 0 {
		return nil
	}
	sort.Slice(completed, func(a, b int) bool {
		left, right := catalog.Records[completed[a]], catalog.Records[completed[b]]
		leftTime, rightTime := terminalTime(left), terminalTime(right)
		if !leftTime.Equal(rightTime) {
			return leftTime.Before(rightTime)
		}
		if left.PoolID != right.PoolID {
			return left.PoolID < right.PoolID
		}
		return left.RunnerName < right.RunnerName
	})
	return compactOldestFirst(catalog, completed, overshootBytes, encodedBytes)
}

// compactOldestFirst drops the oldest-first candidate record indices from
// catalog.Records, capped at a count sized from the average encoded record so
// the caller's re-encode loop usually converges in one compaction pass.
func compactOldestFirst(catalog *Catalog, oldestFirst []int, overshootBytes, encodedBytes int) (removed []Record) {
	averageRecordBytes := max(encodedBytes/len(catalog.Records), 1)
	dropCount := min(overshootBytes/averageRecordBytes+1, len(oldestFirst))
	drop := make(map[int]struct{}, dropCount)
	for _, index := range oldestFirst[:dropCount] {
		drop[index] = struct{}{}
	}
	kept := catalog.Records[:0]
	for i, record := range catalog.Records {
		if _, dropped := drop[i]; !dropped {
			kept = append(kept, record)
			continue
		}
		removed = append(removed, record)
	}
	catalog.Records = kept
	return removed
}

var _ Store = (*FileStore)(nil)
