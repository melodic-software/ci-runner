package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/model"
)

func goroutineDumps(t *testing.T, directory string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(directory, "controller-goroutines-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		content, err := os.ReadFile(match)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), "goroutine ") {
			t.Fatalf("dump %s does not contain goroutine stacks", match)
		}
	}
	return matches
}

func TestHeartbeatWatchWritesOneDumpPerStallEpisode(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	diagnostics := t.TempDir()
	harness.controller.config.Paths.Diagnostics = diagnostics
	start := time.Date(2026, 9, 4, 1, 0, 0, 0, time.UTC)
	watch := heartbeatWatch{reconciler: harness.controller, threshold: time.Minute, started: start}
	ctx := context.Background()

	watch.check(ctx, start.Add(59*time.Second))
	if dumps := goroutineDumps(t, diagnostics); len(dumps) != 0 {
		t.Fatalf("dumped before the stall threshold: %v", dumps)
	}
	watch.check(ctx, start.Add(time.Minute))
	watch.check(ctx, start.Add(2*time.Minute))
	if dumps := goroutineDumps(t, diagnostics); len(dumps) != 1 {
		t.Fatalf("dumps after one stall episode = %v, want exactly one", dumps)
	}

	harness.controller.heartbeat.Store(start.Add(3 * time.Minute).UnixNano())
	watch.check(ctx, start.Add(3*time.Minute+time.Second))
	watch.check(ctx, start.Add(4*time.Minute+time.Second))
	if dumps := goroutineDumps(t, diagnostics); len(dumps) != 2 {
		t.Fatalf("dumps after recovery and a second stall = %v, want two", dumps)
	}
}

func TestControlHandlerGoroutineDumpWritesUnderDiagnostics(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	diagnostics := t.TempDir()
	harness.controller.config.Paths.Diagnostics = diagnostics
	handler, err := NewControlHandler(harness.controller, 1234)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Handle(context.Background(), control.Request{
		SchemaVersion: control.SchemaVersion, RequestID: "dump-1", Operation: control.OperationGoroutineDump,
	})
	if !response.OK || filepath.Dir(response.GoroutineDump) != diagnostics {
		t.Fatalf("response = %#v", response)
	}
	if dumps := goroutineDumps(t, diagnostics); len(dumps) != 1 || dumps[0] != response.GoroutineDump {
		t.Fatalf("dumps = %v, want [%s]", dumps, response.GoroutineDump)
	}
}
