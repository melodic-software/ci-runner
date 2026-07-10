package docker

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/melodic-software/ci-runner/internal/jobindex"
	statefs "github.com/melodic-software/ci-runner/internal/state/fs"
)

var ErrArtifactTooLarge = errors.New("worker artifact exceeds configured size limit")

const truncationMarker = "\n[ci-runner: worker output truncated at configured diagnostics.maxFileSize]\n"

type ArtifactMetadata struct {
	ContainerID string
	WorkerName  string
	PoolID      string
	StartedAt   time.Time
}

type ArtifactPolicy struct {
	MaxFileSizeBytes           uint64
	RawDiagnosticMaxInputBytes uint64
	Retention                  time.Duration
	TotalCapBytes              uint64
	CleanupEvery               time.Duration
}

// ArtifactSink receives streams copied out of the worker. Implementations must
// not be exposed to the worker through a bind mount.
type ArtifactSink interface {
	OpenLog(context.Context, ArtifactMetadata) (io.WriteCloser, error)
	WriteDiagnostics(context.Context, ArtifactMetadata, io.Reader) error
	Finalize(context.Context, ArtifactMetadata) error
	AdoptAndCleanup(context.Context, []ArtifactMetadata) error
}

// FileArtifactSink stores controller-owned worker stdout and compressed _diag
// archives. One diagnostics policy governs both classes and the durable job
// catalog is updated atomically as either GitHub events or artifacts arrive.
type FileArtifactSink struct {
	logDirectory        string
	diagnosticDirectory string
	jobs                jobindex.Store
	acl                 jobindex.AccessController
	policy              ArtifactPolicy

	mu            sync.Mutex
	lastCleanupAt time.Time
}

func NewFileArtifactSink(logDirectory, diagnosticDirectory string, jobs jobindex.Store, acl jobindex.AccessController, policy ArtifactPolicy) (*FileArtifactSink, error) {
	if !filepath.IsAbs(logDirectory) || !filepath.IsAbs(diagnosticDirectory) || jobs == nil || acl == nil {
		return nil, errors.New("artifact directories must be absolute and the durable job index and access controller are required")
	}
	if policy.MaxFileSizeBytes == 0 || policy.RawDiagnosticMaxInputBytes == 0 || policy.Retention <= 0 || policy.TotalCapBytes < policy.MaxFileSizeBytes || policy.CleanupEvery <= 0 {
		return nil, errors.New("artifact size, retention, total-cap, and cleanup policies must be positive and consistent")
	}
	if policy.MaxFileSizeBytes > math.MaxInt || policy.RawDiagnosticMaxInputBytes >= math.MaxInt64 {
		return nil, errors.New("artifact input and output bounds exceed platform-safe integer limits")
	}
	logDirectory = filepath.Clean(logDirectory)
	diagnosticDirectory = filepath.Clean(diagnosticDirectory)
	if samePath(logDirectory, diagnosticDirectory) {
		return nil, errors.New("worker log and diagnostic directories must be distinct")
	}
	for name, directory := range map[string]string{"worker log": logDirectory, "worker diagnostic": diagnosticDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create %s directory: %w", name, err)
		}
		if err := hardenAndVerify(acl, directory); err != nil {
			return nil, fmt.Errorf("secure %s directory: %w", name, err)
		}
	}
	return &FileArtifactSink{
		logDirectory: filepath.Clean(logDirectory), diagnosticDirectory: filepath.Clean(diagnosticDirectory),
		jobs: jobs, acl: acl, policy: policy,
	}, nil
}

func (s *FileArtifactSink) OpenLog(ctx context.Context, metadata ArtifactMetadata) (io.WriteCloser, error) {
	path := filepath.Join(s.logDirectory, artifactBaseName(metadata)+".log")
	if err := ensureRegularOrMissing(path); err != nil {
		return nil, fmt.Errorf("inspect worker log destination: %w", err)
	}
	open := true
	if _, err := s.jobs.Upsert(ctx, jobindex.Patch{
		PoolID: metadata.PoolID, RunnerName: metadata.WorkerName, ContainerID: metadata.ContainerID,
		ArtifactStartedAt: metadata.StartedAt, LogPath: path, Open: &open,
	}); err != nil {
		return nil, fmt.Errorf("index worker log: %w", err)
	}
	// ContainerLogs replays retained output when a controller adopts or retries.
	// Capture into a same-directory temporary file so an interrupted retry never
	// destroys the prior complete artifact.
	file, err := os.CreateTemp(s.logDirectory, ".ci-runner-log-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temporary worker log: %w", err)
	}
	temporaryPath := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("secure temporary worker log mode: %w", err)
	}
	if err := hardenAndVerify(s.acl, temporaryPath); err != nil {
		_ = file.Close()
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("secure worker log: %w", err)
	}
	atomic := &atomicLogFile{file: file, temporaryPath: temporaryPath, finalPath: path, directory: s.logDirectory, acl: s.acl}
	return &truncatingWriteCloser{destination: atomic, limit: s.policy.MaxFileSizeBytes}, nil
}

func (s *FileArtifactSink) WriteDiagnostics(ctx context.Context, metadata ArtifactMetadata, source io.Reader) error {
	finalPath := filepath.Join(s.diagnosticDirectory, artifactBaseName(metadata)+"-diag.tar.gz")
	if info, err := os.Lstat(finalPath); err == nil && info.Mode().IsRegular() && info.Size() > 0 && uint64(info.Size()) <= s.policy.MaxFileSizeBytes {
		if err := hardenAndVerify(s.acl, finalPath); err != nil {
			return fmt.Errorf("secure existing diagnostic archive: %w", err)
		}
		_, upsertErr := s.jobs.Upsert(ctx, jobindex.Patch{
			PoolID: metadata.PoolID, RunnerName: metadata.WorkerName, ContainerID: metadata.ContainerID,
			ArtifactStartedAt: metadata.StartedAt, DiagnosticPath: finalPath,
		})
		return upsertErr
	} else if err == nil && !info.Mode().IsRegular() {
		return errors.New("existing diagnostic destination is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect existing diagnostic archive: %w", err)
	}
	temporary, err := os.CreateTemp(s.diagnosticDirectory, ".ci-runner-diag-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary diagnostic archive: %w", err)
	}
	temporaryName := temporary.Name()
	succeeded := false
	defer func() {
		_ = temporary.Close()
		if !succeeded {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary diagnostic archive: %w", err)
	}
	if err := hardenAndVerify(s.acl, temporaryName); err != nil {
		return fmt.Errorf("secure temporary diagnostic archive: %w", err)
	}
	boundedOutput := &boundedWriter{destination: temporary, limit: s.policy.MaxFileSizeBytes}
	gzipWriter := gzip.NewWriter(boundedOutput)
	boundedInput := &io.LimitedReader{R: source, N: int64(s.policy.RawDiagnosticMaxInputBytes) + 1}
	_, copyErr := io.Copy(gzipWriter, boundedInput)
	closeGzipErr := gzipWriter.Close()
	if copyErr != nil || closeGzipErr != nil {
		return fmt.Errorf("write diagnostic archive: %w", errors.Join(copyErr, closeGzipErr))
	}
	if boundedInput.N <= 0 {
		return fmt.Errorf("raw diagnostic input: %w", ErrArtifactTooLarge)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush diagnostic archive: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close diagnostic archive: %w", err)
	}
	if err := statefs.ReplaceFileAtomic(temporaryName, finalPath); err != nil {
		return fmt.Errorf("publish diagnostic archive: %w", err)
	}
	succeeded = true
	if err := hardenAndVerify(s.acl, finalPath); err != nil {
		_ = os.Remove(finalPath)
		return fmt.Errorf("secure diagnostic archive: %w", err)
	}
	if err := statefs.SyncDirectory(s.diagnosticDirectory); err != nil {
		return fmt.Errorf("flush diagnostic directory: %w", err)
	}
	if _, err := s.jobs.Upsert(ctx, jobindex.Patch{
		PoolID: metadata.PoolID, RunnerName: metadata.WorkerName, ContainerID: metadata.ContainerID,
		ArtifactStartedAt: metadata.StartedAt, DiagnosticPath: finalPath,
	}); err != nil {
		return fmt.Errorf("index worker diagnostics: %w", err)
	}
	return nil
}

func (s *FileArtifactSink) Finalize(ctx context.Context, metadata ArtifactMetadata) error {
	record, err := s.jobs.FindByRunner(ctx, metadata.PoolID, metadata.WorkerName)
	if err != nil {
		return fmt.Errorf("load worker artifact index before finalization: %w", err)
	}
	if record.ContainerID != metadata.ContainerID || record.LogPath == "" || record.DiagnosticPath == "" {
		return errors.New("worker artifacts are incomplete; container is retained for retry")
	}
	if err := errors.Join(
		verifyFinalArtifact(s.acl, s.logDirectory, record.LogPath, true),
		verifyFinalArtifact(s.acl, s.diagnosticDirectory, record.DiagnosticPath, false),
	); err != nil {
		return fmt.Errorf("verify worker artifacts before finalization: %w", err)
	}
	open := false
	_, err = s.jobs.Upsert(ctx, jobindex.Patch{
		PoolID: metadata.PoolID, RunnerName: metadata.WorkerName, ContainerID: metadata.ContainerID,
		ArtifactStartedAt: metadata.StartedAt, FinalizedAt: time.Now().UTC(), Open: &open,
	})
	if err != nil {
		return fmt.Errorf("finalize job artifact index: %w", err)
	}
	return nil
}

func (s *FileArtifactSink) AdoptAndCleanup(ctx context.Context, adopted []ArtifactMetadata) error {
	adoptedIDs, err := s.indexAdopted(ctx, adopted)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := s.reconcileStaleOpen(ctx, adoptedIDs, now); err != nil {
		return err
	}
	s.mu.Lock()
	due := s.lastCleanupAt.IsZero() || now.Sub(s.lastCleanupAt) >= s.policy.CleanupEvery
	s.mu.Unlock()
	if !due {
		return nil
	}
	if err := s.cleanup(ctx, adoptedIDs, now); err != nil {
		return err
	}
	s.mu.Lock()
	s.lastCleanupAt = now
	s.mu.Unlock()
	return nil
}

// CleanupNow is the explicit operator retention escape hatch. Callers must
// provide a fresh fixed-endpoint Docker inventory; all listed containers are
// durably marked open and excluded before cleanup begins.
func (s *FileArtifactSink) CleanupNow(ctx context.Context, adopted []ArtifactMetadata) error {
	adoptedIDs, err := s.indexAdopted(ctx, adopted)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := s.reconcileStaleOpen(ctx, adoptedIDs, now); err != nil {
		return err
	}
	if err := s.cleanup(ctx, adoptedIDs, now); err != nil {
		return err
	}
	s.mu.Lock()
	s.lastCleanupAt = now
	s.mu.Unlock()
	return nil
}

func (s *FileArtifactSink) indexAdopted(ctx context.Context, adopted []ArtifactMetadata) (map[string]struct{}, error) {
	adoptedIDs := make(map[string]struct{}, len(adopted))
	for _, metadata := range adopted {
		adoptedIDs[metadata.ContainerID] = struct{}{}
		open := true
		if _, err := s.jobs.Upsert(ctx, jobindex.Patch{
			PoolID: metadata.PoolID, RunnerName: metadata.WorkerName, ContainerID: metadata.ContainerID,
			ArtifactStartedAt: metadata.StartedAt, Open: &open,
		}); err != nil {
			return nil, fmt.Errorf("adopt worker artifact record: %w", err)
		}
	}
	return adoptedIDs, nil
}

func (s *FileArtifactSink) reconcileStaleOpen(ctx context.Context, adopted map[string]struct{}, now time.Time) error {
	catalog, err := s.jobs.Load(ctx)
	if errors.Is(err, jobindex.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	closed := false
	for _, record := range catalog.Records {
		if !record.Open || record.ContainerID == "" || record.TombstonedAt != nil {
			continue
		}
		if _, active := adopted[record.ContainerID]; active {
			continue
		}
		if _, err := s.jobs.Upsert(ctx, jobindex.Patch{
			PoolID: record.PoolID, RunnerName: record.RunnerName, ContainerID: record.ContainerID,
			FinalizedAt: now, Open: &closed,
		}); err != nil {
			return fmt.Errorf("close stale worker artifact record: %w", err)
		}
	}
	return nil
}

func (s *FileArtifactSink) cleanup(ctx context.Context, adopted map[string]struct{}, now time.Time) error {
	catalog, err := s.jobs.Load(ctx)
	if errors.Is(err, jobindex.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	type candidate struct {
		record jobindex.Record
		size   uint64
	}
	var candidates []candidate
	var total uint64
	referenced := make(map[string]struct{}, len(catalog.Records)*2)
	var cleanupErrors []error
	for _, record := range catalog.Records {
		if record.TombstonedAt != nil {
			continue
		}
		logSize, logErr := artifactSizeWithin(s.logDirectory, record.LogPath)
		diagnosticSize, diagnosticErr := artifactSizeWithin(s.diagnosticDirectory, record.DiagnosticPath)
		if err := errors.Join(logErr, diagnosticErr); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("validate artifact paths for %s/%s: %w", record.PoolID, record.RunnerName, err))
			continue
		}
		for _, path := range []string{record.LogPath, record.DiagnosticPath} {
			if path != "" {
				referenced[canonicalPath(path)] = struct{}{}
			}
		}
		size := saturatingAdd(logSize, diagnosticSize)
		total = saturatingAdd(total, size)
		_, isAdopted := adopted[record.ContainerID]
		if !record.Open && !isAdopted && !record.FinalizedAt.IsZero() {
			candidates = append(candidates, candidate{record: record, size: size})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].record.FinalizedAt.Equal(candidates[j].record.FinalizedAt) {
			return candidates[i].record.UpdatedAt.Before(candidates[j].record.UpdatedAt)
		}
		return candidates[i].record.FinalizedAt.Before(candidates[j].record.FinalizedAt)
	})
	for _, candidate := range candidates {
		expired := now.Sub(candidate.record.FinalizedAt) >= s.policy.Retention
		if !expired && total <= s.policy.TotalCapBytes {
			continue
		}
		removeErr := errors.Join(
			removeIfPresentWithin(s.logDirectory, candidate.record.LogPath),
			removeIfPresentWithin(s.diagnosticDirectory, candidate.record.DiagnosticPath),
		)
		if removeErr != nil {
			cleanupErrors = append(cleanupErrors, removeErr)
			continue
		}
		tombstone := now
		if _, err := s.jobs.Upsert(ctx, jobindex.Patch{
			PoolID: candidate.record.PoolID, RunnerName: candidate.record.RunnerName,
			ContainerID: candidate.record.ContainerID, JobID: candidate.record.JobID, TombstonedAt: &tombstone,
		}); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if candidate.size <= total {
			total -= candidate.size
		} else {
			total = 0
		}
	}
	cutoff := now.Add(-s.policy.Retention)
	cleanupErrors = append(cleanupErrors,
		cleanupOrphans(s.logDirectory, referenced, cutoff),
		cleanupOrphans(s.diagnosticDirectory, referenced, cutoff),
	)
	if _, err := s.jobs.PruneTombstones(ctx, cutoff); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("compact expired artifact tombstones: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

type truncatingWriteCloser struct {
	destination io.WriteCloser
	limit       uint64
	written     uint64
	truncated   bool
}

func (w *truncatingWriteCloser) Write(value []byte) (int, error) {
	accepted := len(value)
	if w.truncated {
		return accepted, nil
	}
	marker := []byte(truncationMarker)
	contentLimit := w.limit
	if uint64(len(marker)) < contentLimit {
		contentLimit -= uint64(len(marker))
	} else {
		contentLimit = 0
	}
	remaining := uint64(0)
	if w.written < contentLimit {
		remaining = contentLimit - w.written
	}
	writeCount := len(value)
	if uint64(writeCount) > remaining {
		writeCount = int(remaining)
	}
	if writeCount > 0 {
		n, err := w.destination.Write(value[:writeCount])
		w.written += uint64(n)
		if err != nil || n != writeCount {
			if err == nil {
				err = io.ErrShortWrite
			}
			return n, err
		}
	}
	if writeCount < len(value) {
		marker = marker[:minInt(len(marker), int(w.limit-w.written))]
		if len(marker) > 0 {
			n, err := w.destination.Write(marker)
			w.written += uint64(n)
			if err != nil || n != len(marker) {
				if err == nil {
					err = io.ErrShortWrite
				}
				return writeCount, err
			}
		}
		w.truncated = true
	}
	return accepted, nil
}

func (w *truncatingWriteCloser) Close() error { return w.destination.Close() }

type atomicLogFile struct {
	file          *os.File
	temporaryPath string
	finalPath     string
	directory     string
	acl           jobindex.AccessController
	once          sync.Once
	err           error
}

func (w *atomicLogFile) Write(value []byte) (int, error) {
	return w.file.Write(value)
}

func (w *atomicLogFile) Close() error {
	w.once.Do(func() {
		defer func() {
			if w.err != nil {
				_ = os.Remove(w.temporaryPath)
			}
		}()
		if err := w.file.Sync(); err != nil {
			w.err = fmt.Errorf("flush temporary worker log: %w", err)
			_ = w.file.Close()
			return
		}
		if err := w.file.Close(); err != nil {
			w.err = fmt.Errorf("close temporary worker log: %w", err)
			return
		}
		if err := hardenAndVerify(w.acl, w.temporaryPath); err != nil {
			w.err = fmt.Errorf("verify temporary worker log: %w", err)
			return
		}
		if err := ensureRegularOrMissing(w.finalPath); err != nil {
			w.err = fmt.Errorf("recheck worker log destination: %w", err)
			return
		}
		if err := statefs.ReplaceFileAtomic(w.temporaryPath, w.finalPath); err != nil {
			w.err = fmt.Errorf("publish worker log atomically: %w", err)
			return
		}
		if err := hardenAndVerify(w.acl, w.finalPath); err != nil {
			w.err = fmt.Errorf("verify published worker log: %w", err)
			return
		}
		if err := statefs.SyncDirectory(w.directory); err != nil {
			w.err = fmt.Errorf("flush worker log directory: %w", err)
			return
		}
	})
	return w.err
}

type boundedWriter struct {
	destination io.Writer
	limit       uint64
	written     uint64
}

func (w *boundedWriter) Write(value []byte) (int, error) {
	if w.written >= w.limit {
		return 0, ErrArtifactTooLarge
	}
	remaining := w.limit - w.written
	if uint64(len(value)) > remaining {
		n, err := w.destination.Write(value[:remaining])
		w.written += uint64(n)
		if err != nil {
			return n, err
		}
		return n, ErrArtifactTooLarge
	}
	n, err := w.destination.Write(value)
	w.written += uint64(n)
	return n, err
}

func artifactSizeWithin(root, path string) (uint64, error) {
	if path == "" {
		return 0, nil
	}
	if err := validateArtifactPath(root, path); err != nil {
		return 0, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		return 0, fmt.Errorf("artifact %q is not a regular file", path)
	}
	return uint64(info.Size()), nil
}

func saturatingAdd(left, right uint64) uint64 {
	if ^uint64(0)-left < right {
		return ^uint64(0)
	}
	return left + right
}

func removeIfPresentWithin(root, path string) error {
	if path == "" {
		return nil
	}
	if err := validateArtifactPath(root, path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to remove non-regular artifact %q", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove finalized artifact %q: %w", path, err)
	}
	return nil
}

func cleanupOrphans(root string, referenced map[string]struct{}, cutoff time.Time) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("scan artifact directory %q: %w", root, err)
	}
	var cleanupErrors []error
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if _, known := referenced[canonicalPath(path)]; known {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if !info.Mode().IsRegular() {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("refuse to remove non-regular orphan %q", path))
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove orphan artifact %q: %w", path, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func validateArtifactPath(root, path string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return errors.New("artifact root and path must be absolute")
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return err
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("artifact path %q escapes configured root %q", path, root)
	}
	return nil
}

func ensureRegularOrMissing(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("path %q is not a regular file", path)
	}
	return nil
}

func verifyFinalArtifact(acl jobindex.AccessController, root, path string, allowEmpty bool) error {
	if err := validateArtifactPath(root, path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || (!allowEmpty && info.Size() <= 0) {
		return fmt.Errorf("artifact %q is not a valid regular evidence file", path)
	}
	return acl.Verify(path)
}

func canonicalPath(path string) string { return strings.ToLower(filepath.Clean(path)) }

var unsafeArtifactCharacter = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func artifactBaseName(metadata ArtifactMetadata) string {
	timestamp := metadata.StartedAt.UTC()
	if timestamp.IsZero() {
		timestamp = time.Unix(0, 0).UTC()
	}
	name := unsafeArtifactCharacter.ReplaceAllString(metadata.WorkerName, "-")
	name = strings.Trim(name, ".-")
	if name == "" {
		name = "worker"
	}
	id := unsafeArtifactCharacter.ReplaceAllString(metadata.ContainerID, "-")
	id = strings.Trim(id, ".-")
	if id == "" {
		id = "container"
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return timestamp.Format("20060102T150405.000000000Z") + "_" + name + "_" + id
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func hardenAndVerify(acl jobindex.AccessController, path string) error {
	if err := acl.Harden(path); err != nil {
		return err
	}
	return acl.Verify(path)
}

func samePath(left, right string) bool {
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
