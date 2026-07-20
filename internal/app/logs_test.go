package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/jobindex"
	"github.com/melodic-software/ci-runner/internal/state"
)

func TestJobLogsUsesExactDurableIndexAndToleratesMissingArtifactFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	neighbor := filepath.Join(root, "job-123.log")
	diagnostic := filepath.Join(root, "job-12-diag.tar.gz")
	resources := filepath.Join(root, "job-12-resources.json")
	if err := os.WriteFile(neighbor, []byte("wrong"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diagnostic, []byte("right"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resources, []byte(`{"schemaVersion":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := FileLogs{WorkerLogDirectory: root, DiagnosticDirectory: root, Jobs: staticJobStore{records: map[string]jobindex.Record{
		"12": {
			PoolID: "org", RunnerName: "runner-12", JobID: "12",
			LogPath: filepath.Join(root, "already-removed.log"), DiagnosticPath: diagnostic,
		},
		"123": {PoolID: "org", RunnerName: "runner-123", JobID: "123", LogPath: neighbor},
	}}}
	var output bytes.Buffer
	if err := logs.Write(context.Background(), &output, false, "12"); err != nil {
		t.Fatal(err)
	}
	want := diagnostic + "\n" + resources
	if got := strings.TrimSpace(output.String()); got != want {
		t.Fatalf("job 12 output = %q, want exact indexed paths %q", got, want)
	}
}

func TestJobLogsRejectsIndexedPathOutsideConfiguredRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("sensitive"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := FileLogs{WorkerLogDirectory: root, DiagnosticDirectory: root, Jobs: staticJobStore{records: map[string]jobindex.Record{
		"12": {PoolID: "org", RunnerName: "runner", JobID: "12", LogPath: outside},
	}}}
	if err := logs.Write(context.Background(), io.Discard, false, "12"); err == nil || !strings.Contains(err.Error(), "escapes configured root") {
		t.Fatalf("outside-root error = %v", err)
	}
}

func TestLogsCleanupIsExplicitAndMutuallyExclusive(t *testing.T) {
	t.Parallel()
	application, output, _ := newTestApplication(t, "", state.NewMemoryStore(), nil)
	cleaned := false
	application.dependencies.Logs = &FileLogs{Cleaner: LogCleanupFunc(func(context.Context) error {
		cleaned = true
		return nil
	})}
	if code := application.Run(context.Background(), []string{"host", "logs", "--cleanup"}); code != ExitOK || !cleaned {
		t.Fatalf("cleanup exit=%d cleaned=%t output=%q", code, cleaned, output.String())
	}
	if code := application.Run(context.Background(), []string{"host", "logs", "--cleanup", "--follow"}); code != ExitUsage {
		t.Fatalf("mutually exclusive cleanup exit=%d", code)
	}
}

type staticJobStore struct{ records map[string]jobindex.Record }

func (s staticJobStore) Load(context.Context) (jobindex.Catalog, error) {
	records := make([]jobindex.Record, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record)
	}
	return jobindex.Catalog{SchemaVersion: jobindex.SchemaVersion, Records: records}, nil
}
func (s staticJobStore) Upsert(context.Context, jobindex.Patch) (jobindex.Record, error) {
	return jobindex.Record{}, errors.New("unexpected upsert")
}
func (s staticJobStore) FindByJobID(_ context.Context, jobID string) (jobindex.Record, error) {
	record, ok := s.records[jobID]
	if !ok {
		return jobindex.Record{}, jobindex.ErrNotFound
	}
	return record, nil
}
func (s staticJobStore) FindByRunner(_ context.Context, poolID, runnerName string) (jobindex.Record, error) {
	for _, record := range s.records {
		if record.PoolID == poolID && record.RunnerName == runnerName {
			return record, nil
		}
	}
	return jobindex.Record{}, jobindex.ErrNotFound
}
func (s staticJobStore) PruneTombstones(context.Context, time.Time) (int, error) { return 0, nil }
func (s staticJobStore) ActiveJob(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
