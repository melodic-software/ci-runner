package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/controller"
)

type logACL struct{ paths []string }

func (a *logACL) Harden(path string) error {
	a.paths = append(a.paths, path)
	return nil
}

func TestJSONLogSinkRotatesRetainsAndRedacts(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "controller")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(directory, "controller-20000101T000000.000000000Z.jsonl")
	if err := os.WriteFile(old, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	acl := &logACL{}
	now := time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC)
	sink, err := NewJSONLogSink(directory, config.LogClass{
		MaxFileSize: config.ByteSize(220), Retention: config.Duration{Duration: 14 * 24 * time.Hour}, TotalCap: config.ByteSize(1024),
	}, 24*time.Hour, acl)
	if err != nil {
		t.Fatal(err)
	}
	sink.now = func() time.Time { return now }
	secretMessage := "authorization=Bearer abcdef token=ghs_abcdefghijklmnopqrstuvwxyz012345 ACTIONS_RUNNER_INPUT_JITCONFIG=c2VjcmV0aml0 -----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY----- eyJabcdefghijk.abcdefghijkl.abcdefghijkl"
	for index := 0; index < 4; index++ {
		if err := sink.Write(context.Background(), controller.LogEvent{Code: "test", Message: secretMessage}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Nanosecond)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired log was not removed: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected size rotation, found %d file(s)", len(entries))
	}
	var combined strings.Builder
	for _, entry := range entries {
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		combined.Write(contents)
	}
	for _, forbidden := range []string{"ghs_abcdefghijklmnopqrstuvwxyz", "BEGIN PRIVATE KEY", "eyJabcdefghijk", "Bearer abcdef", "c2VjcmV0aml0"} {
		if strings.Contains(combined.String(), forbidden) {
			t.Fatalf("structured logs contain secret fragment %q: %s", forbidden, combined.String())
		}
	}
	if len(acl.paths) < 2 {
		t.Fatalf("expected directory and file ACL hardening: %#v", acl.paths)
	}
}

func TestJSONLogSinkWritesAfterContextCancellation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "controller")
	sink, err := NewJSONLogSink(directory, config.LogClass{
		MaxFileSize: config.ByteSize(1024), Retention: config.Duration{Duration: 24 * time.Hour}, TotalCap: config.ByteSize(1024),
	}, 24*time.Hour, &logACL{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.Write(ctx, controller.LogEvent{Code: "canceled-operation", Message: "diagnostic survived cancellation"}); err != nil {
		t.Fatalf("write with canceled context: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("log files = %d, want 1", len(entries))
	}
	contents, err := os.ReadFile(filepath.Join(directory, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), `"code":"canceled-operation"`) {
		t.Fatalf("cancellation diagnostic was not written: %s", contents)
	}
}
