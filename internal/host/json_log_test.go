package host

import (
	"context"
	"errors"
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

func TestJSONLogSinkEmitsAndRedactsCause(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "controller")
	sink, err := NewJSONLogSink(directory, config.LogClass{
		MaxFileSize: config.ByteSize(4096), Retention: config.Duration{Duration: 24 * time.Hour}, TotalCap: config.ByteSize(8192),
	}, 24*time.Hour, &logACL{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(context.Background(), controller.LogEvent{
		Code:    "scale-set-statistics-error",
		Message: "scale-set poll failed (forbidden, HTTP 403)",
		Cause:   `github_request_id="req-visible" token=ghs_abcdefghijklmnopqrstuvwxyz012345`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var combined strings.Builder
	for _, entry := range entries {
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		combined.Write(contents)
	}
	if !strings.Contains(combined.String(), `"cause"`) || !strings.Contains(combined.String(), "req-visible") {
		t.Fatalf("underlying cause was not emitted to the structured log: %s", combined.String())
	}
	if strings.Contains(combined.String(), "ghs_abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("cause field bypassed secret redaction: %s", combined.String())
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
	if err := sink.Write(ctx, controller.LogEvent{Code: "canceled-operation", Message: "diagnostic survived cancellation", Source: "resources"}); err != nil {
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
	if !strings.Contains(string(contents), `"source":"resources"`) {
		t.Fatalf("structured observation source was not written: %s", contents)
	}
}

func TestJSONLogSinkBoundsCanceledPathWhenFileWorkerStalls(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "controller")
	sink, err := NewJSONLogSink(directory, config.LogClass{
		MaxFileSize: config.ByteSize(1024), Retention: config.Duration{Duration: 24 * time.Hour}, TotalCap: config.ByteSize(1024),
	}, 24*time.Hour, &logACL{})
	if err != nil {
		t.Fatal(err)
	}
	sink.writeTimeout = 25 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink.mu.Lock()
	started := time.Now()
	err = sink.Write(ctx, controller.LogEvent{Code: "stalled-cancellation-diagnostic"})
	elapsed := time.Since(started)
	sink.mu.Unlock()

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled write error = %v, want deadline exceeded", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("stalled cancellation-path write took %s, want a bounded return", elapsed)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLogSinkCloseRemainsBoundedWhenFileWorkerNeverRecovers(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "controller")
	sink, err := NewJSONLogSink(directory, config.LogClass{
		MaxFileSize: config.ByteSize(1024), Retention: config.Duration{Duration: 24 * time.Hour}, TotalCap: config.ByteSize(1024),
	}, 24*time.Hour, &logACL{})
	if err != nil {
		t.Fatal(err)
	}
	sink.writeTimeout = 10 * time.Millisecond
	sink.closeTimeout = 10 * time.Millisecond

	// Keep the worker stalled for the remainder of the test. Close must not
	// rely on the test eventually releasing this injected storage stall.
	sink.mu.Lock()
	if err := sink.Write(context.Background(), controller.LogEvent{Code: "permanent-stall"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled write error = %v, want deadline exceeded", err)
	}
	started := time.Now()
	err = sink.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled close error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("stalled close took %s, want a bounded return", elapsed)
	}
	if health := sink.Health(); health.WriteTimeouts != 1 || health.CloseTimeouts != 1 {
		t.Fatalf("sink health = %#v, want one write and close timeout", health)
	}
}

func TestJSONLogSinkCloseDrainsAcceptedWrites(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "controller")
	sink, err := NewJSONLogSink(directory, config.LogClass{
		MaxFileSize: config.ByteSize(4096), Retention: config.Duration{Duration: 24 * time.Hour}, TotalCap: config.ByteSize(4096),
	}, 24*time.Hour, &logACL{})
	if err != nil {
		t.Fatal(err)
	}

	for _, code := range []string{"first", "second", "third"} {
		if err := sink.Write(context.Background(), controller.LogEvent{Code: code}); err != nil {
			t.Fatalf("write %q: %v", code, err)
		}
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
	text := string(contents)
	first, second, third := strings.Index(text, `"code":"first"`), strings.Index(text, `"code":"second"`), strings.Index(text, `"code":"third"`)
	if first < 0 || second <= first || third <= second {
		t.Fatalf("accepted events were not drained in FIFO order: %s", text)
	}
}

func TestJSONLogSinkHealthReportsQueueTimeoutWithoutBlocking(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "controller")
	sink, err := NewJSONLogSink(directory, config.LogClass{
		MaxFileSize: config.ByteSize(1024), Retention: config.Duration{Duration: 24 * time.Hour}, TotalCap: config.ByteSize(1024),
	}, 24*time.Hour, &logACL{})
	if err != nil {
		t.Fatal(err)
	}
	sink.writeTimeout = 2 * time.Millisecond
	sink.closeTimeout = 2 * time.Millisecond
	sink.mu.Lock()

	// One request stalls in the worker and the next queue-capacity requests
	// fill the buffer. Each caller returns on its own delivery deadline.
	for index := 0; index <= structuredLogQueueCapacity; index++ {
		_ = sink.Write(context.Background(), controller.LogEvent{Code: "fill-queue"})
	}
	if err := sink.Write(context.Background(), controller.LogEvent{Code: "queue-overflow"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("full-queue write error = %v, want deadline exceeded", err)
	}
	if health := sink.Health(); health.QueueTimeouts != 1 || health.WriteTimeouts != structuredLogQueueCapacity+1 {
		t.Fatalf("sink health = %#v, want one queue timeout and %d write timeouts", health, structuredLogQueueCapacity+1)
	}
	_ = sink.Close()
}
