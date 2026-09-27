package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/jobindex"
)

// seedArtifactSweep writes a jobs.json holding records finalized artifact
// records (the first expired of them past retention) plus padding tombstones,
// and one log, diagnostic, and resource file per finalized record.
func seedArtifactSweep(tb testing.TB, root string, records, expired, padding int, retention time.Duration) {
	tb.Helper()
	now := time.Now().UTC()
	catalog := jobindex.Catalog{SchemaVersion: jobindex.SchemaVersion}
	for index := range records {
		name := fmt.Sprintf("runner-%05d", index)
		logPath := filepath.Join(root, "logs", name+".log")
		diagnosticPath := filepath.Join(root, "diag", name+"-diag.tar.gz")
		for _, path := range []string{logPath, diagnosticPath, filepath.Join(root, "diag", name+"-resources.json")} {
			if err := os.WriteFile(path, []byte("evidence"), 0o600); err != nil {
				tb.Fatal(err)
			}
		}
		finalizedAt := now.Add(-time.Minute - time.Duration(records-index)*time.Second)
		if index < expired {
			finalizedAt = finalizedAt.Add(-2 * retention)
		}
		catalog.Records = append(catalog.Records, jobindex.Record{
			PoolID: "org", RunnerName: name, ContainerID: "container-" + name, JobID: "job-" + name,
			Result: "success", LogPath: logPath, DiagnosticPath: diagnosticPath,
			ArtifactStartedAt: finalizedAt.Add(-time.Hour), JobStartedAt: finalizedAt.Add(-time.Hour),
			CompletedAt: finalizedAt, FinalizedAt: finalizedAt, UpdatedAt: finalizedAt,
		})
	}
	for index := range padding {
		name := fmt.Sprintf("tombstone-%06d", index)
		tombstonedAt := now.Add(-time.Minute)
		catalog.Records = append(catalog.Records, jobindex.Record{
			PoolID: "org", RunnerName: name, ContainerID: "container-" + name, JobID: "job-" + name,
			Result: "success", LogPath: filepath.Join(root, "logs", name+".log"),
			DiagnosticPath: filepath.Join(root, "diag", name+"-diag.tar.gz"),
			FinalizedAt:    tombstonedAt, UpdatedAt: tombstonedAt, TombstonedAt: &tombstonedAt,
		})
	}
	jobindex.Sort(&catalog)
	encoded, err := json.Marshal(catalog)
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state", "jobs.json"), encoded, 0o600); err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkArtifactRetentionSweep runs one first-tick retention sweep at the
// scale of the 2026-09-04 host (about 3.2k logs, 6.4k diagnostics, 8 MB
// jobs.json). Run with -benchtime 1x.
func BenchmarkArtifactRetentionSweep(b *testing.B) {
	for _, scenario := range []struct {
		name    string
		expired int
	}{{"few-expired", 100}, {"all-expired", 3200}} {
		b.Run(scenario.name, func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				root := b.TempDir()
				policy := ArtifactPolicy{
					MaxFileSizeBytes: 1 << 20, RawDiagnosticMaxInputBytes: 2 << 20,
					Retention: 24 * time.Hour, TotalCapBytes: 1 << 40, CleanupEvery: 24 * time.Hour,
				}
				store, err := jobindex.NewFileStore(filepath.Join(root, "state"), &testJobLocker{}, testJobACL{})
				if err != nil {
					b.Fatal(err)
				}
				sink, err := NewFileArtifactSink(filepath.Join(root, "logs"), filepath.Join(root, "diag"), store, testJobACL{}, policy)
				if err != nil {
					b.Fatal(err)
				}
				seedArtifactSweep(b, root, 3200, scenario.expired, 12000, policy.Retention)
				b.StartTimer()
				if err := sink.AdoptAndCleanup(context.Background(), nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
