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
	watch := harness.controller.newHeartbeatWatch(start)
	limit := ReconcileLivenessLimit(harness.controller.config.Controller.ReconcileInterval.Duration)
	if watch.threshold != limit || limit != ReconcileLivenessFloor {
		t.Fatalf("watch threshold = %s, want the doctor liveness limit %s at the 5m floor", watch.threshold, limit)
	}
	ctx := context.Background()

	watch.check(ctx, start.Add(limit-time.Second))
	if dumps := goroutineDumps(t, diagnostics); len(dumps) != 0 {
		t.Fatalf("dumped before the stall threshold: %v", dumps)
	}
	watch.check(ctx, start.Add(limit))
	watch.check(ctx, start.Add(2*limit))
	if dumps := goroutineDumps(t, diagnostics); len(dumps) != 1 {
		t.Fatalf("dumps after one stall episode = %v, want exactly one", dumps)
	}

	harness.controller.heartbeat.Store(start.Add(3 * limit).UnixNano())
	watch.check(ctx, start.Add(3*limit+time.Second))
	watch.check(ctx, start.Add(4*limit+time.Second))
	if dumps := goroutineDumps(t, diagnostics); len(dumps) != 2 {
		t.Fatalf("dumps after recovery and a second stall = %v, want two", dumps)
	}
}

func TestControlHandlerGoroutineDumpWritesUnderDiagnostics(t *testing.T) {
	t.Parallel()
	harness := newHarness(t, model.ModeEnabled)
	diagnostics := t.TempDir()
	harness.controller.config.Paths.Diagnostics = diagnostics
	acl := &recordingHardener{}
	harness.controller.deps.ACL = acl
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
	if len(acl.paths) != 1 || acl.paths[0] != response.GoroutineDump {
		t.Fatalf("hardened = %v, want [%s]", acl.paths, response.GoroutineDump)
	}
}

type recordingHardener struct{ paths []string }

func (h *recordingHardener) Harden(path string) error {
	h.paths = append(h.paths, path)
	return nil
}
