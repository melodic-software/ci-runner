package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/jobindex"
	dockerruntime "github.com/melodic-software/ci-runner/internal/runtime/docker"
)

type workerArtifactMaintenance struct {
	cfg  config.Config
	acl  runtimeAccessController
	jobs jobindex.Store
}

func newWorkerArtifactMaintenance(cfg config.Config, acl runtimeAccessController, jobs jobindex.Store) *workerArtifactMaintenance {
	return &workerArtifactMaintenance{cfg: cfg, acl: acl, jobs: jobs}
}

func (m *workerArtifactMaintenance) Cleanup(ctx context.Context) error {
	artifacts, err := newWorkerArtifactSink(m.cfg, m.acl, m.jobs)
	if err != nil {
		return err
	}
	adopted, err := dockerruntime.InventoryLocalArtifacts(ctx, buildinfo.Version, m.cfg.Host.ID)
	if err != nil {
		return fmt.Errorf("inventory local workers before cleanup: %w", err)
	}
	return artifacts.CleanupNow(ctx, adopted)
}

func (m *workerArtifactMaintenance) Audit(ctx context.Context) (dockerruntime.ArtifactAuditReport, error) {
	artifacts, err := newWorkerArtifactSink(m.cfg, m.acl, m.jobs)
	if err != nil {
		return dockerruntime.ArtifactAuditReport{}, err
	}
	adopted, invErr := dockerruntime.InventoryLocalArtifacts(ctx, buildinfo.Version, m.cfg.Host.ID)
	inventoryAvailable := invErr == nil
	inventoryError := ""
	if invErr != nil {
		inventoryError = invErr.Error()
	}
	var inventory []dockerruntime.ArtifactMetadata
	if inventoryAvailable {
		inventory = adopted
	}
	return artifacts.Audit(ctx, inventory, inventoryAvailable, inventoryError)
}

func (m *workerArtifactMaintenance) Purge(ctx context.Context, dryRun bool) (dockerruntime.ArtifactPurgeResult, error) {
	artifacts, err := newWorkerArtifactSink(m.cfg, m.acl, m.jobs)
	if err != nil {
		return dockerruntime.ArtifactPurgeResult{DryRun: dryRun}, err
	}
	adopted, err := dockerruntime.InventoryLocalArtifacts(ctx, buildinfo.Version, m.cfg.Host.ID)
	if err != nil {
		return dockerruntime.ArtifactPurgeResult{DryRun: dryRun}, fmt.Errorf("inventory local workers before purge: %w", err)
	}
	return artifacts.PurgeUnreferencedNow(ctx, adopted, dryRun)
}

type artifactMaintainer interface {
	Audit(context.Context) (dockerruntime.ArtifactAuditReport, error)
	Purge(context.Context, bool) (dockerruntime.ArtifactPurgeResult, error)
}

func writeArtifactAudit(destination io.Writer, report dockerruntime.ArtifactAuditReport) error {
	writef(destination, "Worker artifact audit\n")
	writef(destination, "Retention cutoff: %s\n", report.RetentionCutoff.Format(time.RFC3339))
	writeCatalogAvailability(destination, report.CatalogAvailable, report.CatalogError)
	writeInventoryAvailability(destination, report.InventoryAvailable, report.InventoryError)
	for _, directory := range report.Directories {
		writef(destination, "\nDirectory: %s (%d bytes)\n", directory.Directory, directory.TotalBytes)
		for _, class := range []dockerruntime.ArtifactFileClass{
			dockerruntime.ArtifactReferencedPresent,
			dockerruntime.ArtifactUnreferencedPastCutoff,
			dockerruntime.ArtifactUnreferencedWithinCutoff,
		} {
			count := directory.Counts[class]
			if count == 0 {
				continue
			}
			inFlight := inFlightCount(directory.Files, class)
			if inFlight > 0 {
				writef(destination, "  %s: %d files, %d bytes (%d in-flight temporary)\n",
					class, count, directory.BytesByClass[class], inFlight)
				continue
			}
			writef(destination, "  %s: %d files, %d bytes\n", class, count, directory.BytesByClass[class])
		}
	}
	if len(report.ReferencedMissing) > 0 {
		writef(destination, "\nReferenced-and-missing: %d catalog paths\n", len(report.ReferencedMissing))
		for _, file := range report.ReferencedMissing {
			writef(destination, "  %s\n", file.Path)
		}
	}
	if len(report.TombstonedAdopted) > 0 {
		writef(destination, "\nAdopted containers on tombstoned pool/runner keys: %d\n", len(report.TombstonedAdopted))
		for _, container := range report.TombstonedAdopted {
			writef(destination, "  %s/%s container=%s\n", container.PoolID, container.RunnerName, container.ContainerID)
		}
	}
	if len(report.CompactionsDropped) > 0 {
		writef(destination, "\nSave-cap compaction drops (journal): %d entries\n", len(report.CompactionsDropped))
		for _, entry := range report.CompactionsDropped {
			writef(destination, "  %s/%s dropped=%s job=%s\n",
				entry.PoolID, entry.RunnerName, entry.DroppedAt.Format(time.RFC3339), entry.JobID)
		}
	} else if !report.DropJournalAvailable && report.DropJournalError != "" {
		writef(destination, "\nDrop journal: unavailable (%s)\n", report.DropJournalError)
	}
	return nil
}

func writeArtifactPurge(destination io.Writer, result dockerruntime.ArtifactPurgeResult) error {
	if result.DryRun {
		writef(destination, "Reference-based purge dry run\n")
	} else {
		writef(destination, "Reference-based purge completed\n")
	}
	writef(destination, "Delete candidates: %d files, %d bytes\n", result.DeleteCount, result.TotalBytes)
	for _, file := range result.Deleted {
		writef(destination, "  delete %s (%d bytes)\n", file.Path, file.SizeBytes)
	}
	if len(result.Skipped) > 0 {
		writef(destination, "Skipped in-flight temporary files: %d\n", len(result.Skipped))
		for _, file := range result.Skipped {
			writef(destination, "  skip %s\n", file.Path)
		}
	}
	return nil
}

func writeCatalogAvailability(destination io.Writer, available bool, detail string) {
	if available && detail == "" {
		writef(destination, "Catalog: available\n")
		return
	}
	if available {
		writef(destination, "Catalog: available (%s)\n", detail)
		return
	}
	writef(destination, "Catalog: unavailable (%s)\n", detail)
}

func writeInventoryAvailability(destination io.Writer, available bool, detail string) {
	if available {
		writef(destination, "Docker inventory: available\n")
		return
	}
	if detail == "" {
		writef(destination, "Docker inventory: unavailable\n")
		return
	}
	writef(destination, "Docker inventory: unavailable (%s)\n", detail)
}

func inFlightCount(files []dockerruntime.ArtifactAuditFile, class dockerruntime.ArtifactFileClass) int {
	count := 0
	for _, file := range files {
		if file.Classification == class && file.InFlightTemporary {
			count++
		}
	}
	return count
}

func encodeJSON(destination io.Writer, value any) error {
	encoder := json.NewEncoder(destination)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
