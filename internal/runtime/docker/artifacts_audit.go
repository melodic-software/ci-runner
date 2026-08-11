package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/melodic-software/ci-runner/internal/jobindex"
)

var (
	inFlightLogTempPattern       = regexp.MustCompile(`^\.ci-runner-log-.*\.tmp$`)
	inFlightDiagTempPattern      = regexp.MustCompile(`^\.ci-runner-diag-.*\.tmp$`)
	inFlightResourcesTempPattern = regexp.MustCompile(`^\.ci-runner-resources-.*\.tmp$`)
)

// ArtifactFileClass is the disk-vs-catalog classification for one artifact path.
type ArtifactFileClass string

const (
	ArtifactReferencedPresent        ArtifactFileClass = "referenced-and-present"
	ArtifactReferencedMissing        ArtifactFileClass = "referenced-and-missing"
	ArtifactUnreferencedPastCutoff   ArtifactFileClass = "unreferenced-and-past-cutoff"
	ArtifactUnreferencedWithinCutoff ArtifactFileClass = "unreferenced-and-within-cutoff"
)

// ArtifactAuditFile reports one catalog path or on-disk artifact entry.
type ArtifactAuditFile struct {
	Path              string            `json:"path"`
	SizeBytes         uint64            `json:"sizeBytes"`
	Classification    ArtifactFileClass `json:"classification"`
	InFlightTemporary bool              `json:"inFlightTemporary,omitempty"`
	ModifiedAt        time.Time         `json:"modifiedAt,omitempty"`
}

// ArtifactAuditDirectory summarizes one configured artifact root.
type ArtifactAuditDirectory struct {
	Directory    string                       `json:"directory"`
	TotalBytes   uint64                       `json:"totalBytes"`
	Counts       map[ArtifactFileClass]int    `json:"counts"`
	BytesByClass map[ArtifactFileClass]uint64 `json:"bytesByClass"`
	Files        []ArtifactAuditFile          `json:"files"`
}

// TombstonedAdoptedContainer reports a live inventory entry whose pool/runner
// key is frozen in the catalog.
type TombstonedAdoptedContainer struct {
	ContainerID string `json:"containerId"`
	PoolID      string `json:"poolId"`
	RunnerName  string `json:"runnerName"`
}

// ArtifactAuditReport is the read-only disk-vs-catalog audit result.
type ArtifactAuditReport struct {
	RetentionCutoff    time.Time                  `json:"retentionCutoff"`
	CatalogAvailable   bool                       `json:"catalogAvailable"`
	CatalogError       string                     `json:"catalogError,omitempty"`
	InventoryAvailable bool                       `json:"inventoryAvailable"`
	InventoryError     string                     `json:"inventoryError,omitempty"`
	Directories        []ArtifactAuditDirectory   `json:"directories"`
	ReferencedMissing  []ArtifactAuditFile        `json:"referencedMissing"`
	TombstonedAdopted  []TombstonedAdoptedContainer `json:"tombstonedAdopted,omitempty"`
}

// ArtifactPurgeResult reports reference-based purge actions.
type ArtifactPurgeResult struct {
	DryRun      bool                `json:"dryRun"`
	Deleted     []ArtifactAuditFile `json:"deleted"`
	Skipped     []ArtifactAuditFile `json:"skipped"`
	TotalBytes  uint64              `json:"totalBytes"`
	DeleteCount int                 `json:"deleteCount"`
}

func isInFlightTemporary(name string) bool {
	return inFlightLogTempPattern.MatchString(name) ||
		inFlightDiagTempPattern.MatchString(name) ||
		inFlightResourcesTempPattern.MatchString(name)
}

func (s *FileArtifactSink) Audit(ctx context.Context, adopted []ArtifactMetadata, inventoryAvailable bool, inventoryError string) (ArtifactAuditReport, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-s.policy.Retention)
	report := ArtifactAuditReport{
		RetentionCutoff:    cutoff,
		InventoryAvailable: inventoryAvailable,
		InventoryError:     inventoryError,
	}

	catalog, catalogAvailable, catalogError, err := s.loadCatalogForAudit(ctx)
	report.CatalogAvailable = catalogAvailable
	report.CatalogError = catalogError
	if err != nil {
		return report, err
	}

	referenced, catalogPaths := buildReferencedFromCatalog(s.logDirectory, s.diagnosticDirectory, catalog)
	if inventoryAvailable {
		report.TombstonedAdopted = findTombstonedAdopted(catalog, adopted)
	}

	for _, root := range []string{s.logDirectory, s.diagnosticDirectory} {
		directory, err := auditArtifactDirectory(root, referenced, cutoff)
		if err != nil {
			return report, err
		}
		report.Directories = append(report.Directories, directory)
	}

	for _, pathEntry := range catalogPaths {
		info, err := os.Lstat(pathEntry.path)
		if errors.Is(err, os.ErrNotExist) {
			report.ReferencedMissing = append(report.ReferencedMissing, ArtifactAuditFile{
				Path:           pathEntry.path,
				Classification: ArtifactReferencedMissing,
			})
			continue
		}
		if err != nil {
			return report, fmt.Errorf("inspect referenced artifact %q: %w", pathEntry.path, err)
		}
		if !info.Mode().IsRegular() {
			report.ReferencedMissing = append(report.ReferencedMissing, ArtifactAuditFile{
				Path:           pathEntry.path,
				Classification: ArtifactReferencedMissing,
			})
		}
	}

	return report, nil
}

func (s *FileArtifactSink) PurgeUnreferencedNow(ctx context.Context, adopted []ArtifactMetadata, dryRun bool) (ArtifactPurgeResult, error) {
	adoptedIDs, err := s.indexAdopted(ctx, adopted)
	if err != nil {
		return ArtifactPurgeResult{DryRun: dryRun}, err
	}
	now := time.Now().UTC()
	if err := s.reconcileStaleOpen(ctx, adoptedIDs, now); err != nil {
		return ArtifactPurgeResult{DryRun: dryRun}, err
	}
	return s.PurgeUnreferenced(ctx, dryRun)
}

func (s *FileArtifactSink) PurgeUnreferenced(ctx context.Context, dryRun bool) (ArtifactPurgeResult, error) {
	catalog, catalogAvailable, catalogError, err := s.loadCatalogForAudit(ctx)
	if err != nil {
		return ArtifactPurgeResult{DryRun: dryRun}, err
	}
	if !catalogAvailable {
		return ArtifactPurgeResult{DryRun: dryRun}, fmt.Errorf("catalog is unavailable: %s", catalogError)
	}
	_ = catalog

	report, err := s.Audit(ctx, nil, true, "")
	if err != nil {
		return ArtifactPurgeResult{DryRun: dryRun}, err
	}

	result := ArtifactPurgeResult{DryRun: dryRun}
	for _, directory := range report.Directories {
		for _, file := range directory.Files {
			if file.Classification != ArtifactUnreferencedPastCutoff &&
				file.Classification != ArtifactUnreferencedWithinCutoff {
				continue
			}
			if file.InFlightTemporary {
				result.Skipped = append(result.Skipped, file)
				continue
			}
			if dryRun {
				result.Deleted = append(result.Deleted, file)
				result.TotalBytes += file.SizeBytes
				result.DeleteCount++
				continue
			}
			if err := removeRegularArtifact(file.Path); err != nil {
				return result, err
			}
			result.Deleted = append(result.Deleted, file)
			result.TotalBytes += file.SizeBytes
			result.DeleteCount++
		}
	}
	return result, nil
}

func (s *FileArtifactSink) loadCatalogForAudit(ctx context.Context) (jobindex.Catalog, bool, string, error) {
	catalog, err := s.jobs.Load(ctx)
	if errors.Is(err, jobindex.ErrNotFound) {
		return jobindex.Catalog{}, true, "", nil
	}
	if err != nil {
		return jobindex.Catalog{}, false, err.Error(), nil
	}
	return catalog, true, "", nil
}

type catalogPathEntry struct {
	path string
}

func buildReferencedFromCatalog(logDirectory, diagnosticDirectory string, catalog jobindex.Catalog) (map[string]struct{}, []catalogPathEntry) {
	referenced := make(map[string]struct{}, len(catalog.Records)*3)
	var catalogPaths []catalogPathEntry
	for _, record := range catalog.Records {
		if record.TombstonedAt != nil {
			continue
		}
		if record.LogPath != "" {
			if err := validateArtifactPath(logDirectory, record.LogPath); err == nil {
				referenced[canonicalPath(record.LogPath)] = struct{}{}
				catalogPaths = append(catalogPaths, catalogPathEntry{path: record.LogPath})
			}
		}
		if record.DiagnosticPath != "" {
			if err := validateArtifactPath(diagnosticDirectory, record.DiagnosticPath); err == nil {
				referenced[canonicalPath(record.DiagnosticPath)] = struct{}{}
				catalogPaths = append(catalogPaths, catalogPathEntry{path: record.DiagnosticPath})
			}
		}
		resourcePath, resourcePathErr := jobindex.ResourceEvidencePath(record)
		if resourcePathErr == nil && resourcePath != "" {
			if err := validateArtifactPath(diagnosticDirectory, resourcePath); err == nil {
				referenced[canonicalPath(resourcePath)] = struct{}{}
				catalogPaths = append(catalogPaths, catalogPathEntry{path: resourcePath})
			}
		}
	}
	return referenced, catalogPaths
}

func auditArtifactDirectory(root string, referenced map[string]struct{}, cutoff time.Time) (ArtifactAuditDirectory, error) {
	directory := ArtifactAuditDirectory{
		Directory:    root,
		Counts:       make(map[ArtifactFileClass]int),
		BytesByClass: make(map[ArtifactFileClass]uint64),
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return directory, fmt.Errorf("scan artifact directory %q: %w", root, err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return directory, err
		}
		path := filepath.Join(root, entry.Name())
		size := uint64(info.Size())
		classification := classifyOnDiskArtifact(entry.Name(), path, referenced, info.ModTime(), cutoff)
		file := ArtifactAuditFile{
			Path:              path,
			SizeBytes:         size,
			Classification:    classification,
			InFlightTemporary: isInFlightTemporary(entry.Name()),
			ModifiedAt:        info.ModTime().UTC(),
		}
		directory.Files = append(directory.Files, file)
		directory.TotalBytes += size
		directory.Counts[classification]++
		directory.BytesByClass[classification] += size
	}
	return directory, nil
}

func classifyOnDiskArtifact(name, path string, referenced map[string]struct{}, modifiedAt, cutoff time.Time) ArtifactFileClass {
	if _, known := referenced[canonicalPath(path)]; known {
		return ArtifactReferencedPresent
	}
	if modifiedAt.After(cutoff) {
		return ArtifactUnreferencedWithinCutoff
	}
	return ArtifactUnreferencedPastCutoff
}

func findTombstonedAdopted(catalog jobindex.Catalog, adopted []ArtifactMetadata) []TombstonedAdoptedContainer {
	tombstoned := make(map[string]struct{})
	for _, record := range catalog.Records {
		if record.TombstonedAt != nil {
			tombstoned[record.PoolID+"\x00"+record.RunnerName] = struct{}{}
		}
	}
	var matches []TombstonedAdoptedContainer
	for _, metadata := range adopted {
		key := metadata.PoolID + "\x00" + metadata.WorkerName
		if _, frozen := tombstoned[key]; frozen {
			matches = append(matches, TombstonedAdoptedContainer{
				ContainerID: metadata.ContainerID,
				PoolID:      metadata.PoolID,
				RunnerName:  metadata.WorkerName,
			})
		}
	}
	return matches
}

func removeRegularArtifact(path string) error {
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
		return fmt.Errorf("remove unreferenced artifact %q: %w", path, err)
	}
	return nil
}
