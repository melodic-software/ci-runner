//go:build windows

package host

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
)

// fakeEngineClient is a deterministic double for engineClient. *client.Client
// structurally satisfies the same interface in production; tests never dial a
// real Docker Engine.
type fakeEngineClient struct {
	infoResult client.SystemInfoResult
	infoErr    error
	listResult client.ContainerListResult
	listErr    error
	closeErr   error
	closed     bool
}

func (f *fakeEngineClient) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	return f.infoResult, f.infoErr
}

func (f *fakeEngineClient) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return f.listResult, f.listErr
}

func (f *fakeEngineClient) Close() error {
	f.closed = true
	return f.closeErr
}

func inspectorWith(fake *fakeEngineClient) DockerEngineInspector {
	return DockerEngineInspector{NewClient: func() (engineClient, error) { return fake, nil }}
}

type recordingCommandRunner struct {
	name string
	args []string
	out  []byte
	err  error
}

func (r *recordingCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return r.out, r.err
}

func TestDockerDesktopStatusClassifiesNonZeroExit(t *testing.T) {
	t.Parallel()
	// A stopped Docker Desktop makes `docker desktop status` exit non-zero, so the
	// runner returns a genuine *exec.ExitError. Produce one rather than a sentinel
	// so errors.As exercises the same classification path the CLI hits in the wild.
	exitErr := exec.CommandContext(context.Background(), "cmd", "/c", "exit 1").Run()
	var asExit *exec.ExitError
	if !errors.As(exitErr, &asExit) {
		t.Fatalf("prerequisite: %v is not an *exec.ExitError", exitErr)
	}
	executable := `C:\Program Files\Docker\Docker\resources\bin\docker.exe`
	tests := []struct {
		name    string
		out     []byte
		err     error
		want    DesktopStatus
		wantErr bool
	}{
		{name: "stopped output on non-zero exit", out: []byte("stopped"), err: exitErr, want: DesktopStatusStopped},
		{name: "empty output on non-zero exit", out: nil, err: exitErr, want: DesktopStatusStopped},
		{name: "running output on non-zero exit", out: []byte(`{"Status":"running"}`), err: exitErr, want: DesktopStatusRunning},
		{name: "non-exit error propagates", out: nil, err: errors.New("docker.exe not found"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner := &recordingCommandRunner{out: test.out, err: test.err}
			status, err := DockerDesktopCLI{Runner: runner, executablePath: executable}.Status(context.Background())
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error, got status %v", status)
				}
				if status != DesktopStatusUnknown {
					t.Fatalf("status = %v, want DesktopStatusUnknown on a non-exit error", status)
				}
				return
			}
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status != test.want {
				t.Fatalf("status = %v, want %v", status, test.want)
			}
		})
	}
}

func TestDockerDesktopStatusPreservesContextCancellation(t *testing.T) {
	t.Parallel()
	// A canceled or expired context kills the probe process, which also surfaces
	// as an *exec.ExitError. That aborted probe must propagate the context error
	// instead of classifying the desktop as stopped: in watchdog and shutdown
	// paths a timed-out probe otherwise records a known-stopped desktop and
	// skips the Docker worker inventory.
	exitErr := exec.CommandContext(context.Background(), "cmd", "/c", "exit 1").Run()
	var asExit *exec.ExitError
	if !errors.As(exitErr, &asExit) {
		t.Fatalf("prerequisite: %v is not an *exec.ExitError", exitErr)
	}
	executable := `C:\Program Files\Docker\Docker\resources\bin\docker.exe`
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &recordingCommandRunner{out: nil, err: exitErr}
	status, err := DockerDesktopCLI{Runner: runner, executablePath: executable}.Status(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context cancellation preserved", err)
	}
	if status != DesktopStatusUnknown {
		t.Fatalf("status = %v, want DesktopStatusUnknown for an aborted probe", status)
	}
}

func TestEngineReachableTrueWhenInfoSucceeds(t *testing.T) {
	t.Parallel()
	fake := &fakeEngineClient{infoResult: client.SystemInfoResult{Info: system.Info{ServerVersion: "27.0.0"}}}
	reachable, err := inspectorWith(fake).EngineReachable(context.Background())
	if err != nil {
		t.Fatalf("EngineReachable: %v", err)
	}
	if !reachable {
		t.Fatal("reachable = false, want true")
	}
	if !fake.closed {
		t.Fatal("engine client was not closed")
	}
}

func TestEngineReachableFalseWithoutErrorWhenEngineDown(t *testing.T) {
	t.Parallel()
	// A connection failure against the pinned npipe endpoint (Docker Desktop
	// not running) is a factual "not reachable" answer, not a hard error.
	fake := &fakeEngineClient{infoErr: errors.New("open //./pipe/docker_engine: the system cannot find the file specified")}
	reachable, err := inspectorWith(fake).EngineReachable(context.Background())
	if err != nil {
		t.Fatalf("EngineReachable: %v", err)
	}
	if reachable {
		t.Fatal("reachable = true, want false")
	}
}

func TestEngineReachablePropagatesContextCancellation(t *testing.T) {
	t.Parallel()
	// A context that is already done when Info fails must propagate the
	// context error instead of reporting a factual "unreachable" -- mirroring
	// DockerDesktopCLI.Status's context-preservation contract so a timed-out
	// probe cannot be misread as a known-down engine.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := &fakeEngineClient{infoErr: errors.New("context canceled")}
	reachable, err := inspectorWith(fake).EngineReachable(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled preserved", err)
	}
	if reachable {
		t.Fatal("reachable = true, want false for an aborted probe")
	}
}

func TestEngineReachablePropagatesClientConstructionFailure(t *testing.T) {
	t.Parallel()
	constructionErr := errors.New("dial failure")
	inspector := DockerEngineInspector{NewClient: func() (engineClient, error) { return nil, constructionErr }}
	reachable, err := inspector.EngineReachable(context.Background())
	if !errors.Is(err, constructionErr) {
		t.Fatalf("err = %v, want construction failure propagated as a hard error", err)
	}
	if reachable {
		t.Fatal("reachable = true, want false")
	}
}

func TestContainersMarksOnlyManagedAndStripsNamePrefix(t *testing.T) {
	t.Parallel()
	fake := &fakeEngineClient{listResult: client.ContainerListResult{Items: []containertypes.Summary{
		{ID: "one", Names: []string{"/worker"}, Image: "runner", Status: "Up 2 minutes", Labels: map[string]string{managedContainerLabel: "true", "pool": "org"}},
		{ID: "two", Names: []string{"/database"}, Image: "postgres", Status: "Up 5 minutes", Labels: map[string]string{"project": "local"}},
	}}}
	got, err := inspectorWith(fake).Containers(context.Background())
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("containers = %#v, want 2", got)
	}
	if !got[0].Managed || got[0].Name != "worker" || got[0].Status != "Up 2 minutes" {
		t.Fatalf("managed container = %#v", got[0])
	}
	if got[1].Managed || got[1].Name != "database" {
		t.Fatalf("unmanaged container = %#v", got[1])
	}
	if !fake.closed {
		t.Fatal("engine client was not closed")
	}
}

func TestContainersPropagatesListError(t *testing.T) {
	t.Parallel()
	listErr := errors.New("list failure")
	fake := &fakeEngineClient{listErr: listErr}
	if _, err := inspectorWith(fake).Containers(context.Background()); !errors.Is(err, listErr) {
		t.Fatalf("err = %v, want list failure propagated", err)
	}
}

func TestEngineMemoryTotalReadsMemTotal(t *testing.T) {
	t.Parallel()
	fake := &fakeEngineClient{infoResult: client.SystemInfoResult{Info: system.Info{MemTotal: 33328562176}}}
	total, err := inspectorWith(fake).EngineMemoryTotal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if total != 33328562176 {
		t.Fatalf("total = %d, want 33328562176", total)
	}
}

func TestEngineMemoryTotalRejectsZero(t *testing.T) {
	t.Parallel()
	fake := &fakeEngineClient{infoResult: client.SystemInfoResult{Info: system.Info{MemTotal: 0}}}
	if _, err := inspectorWith(fake).EngineMemoryTotal(context.Background()); err == nil {
		t.Fatal("expected error for zero MemTotal")
	}
}

func TestEngineMemoryTotalPropagatesInfoError(t *testing.T) {
	t.Parallel()
	infoErr := errors.New("info failure")
	fake := &fakeEngineClient{infoErr: infoErr}
	if _, err := inspectorWith(fake).EngineMemoryTotal(context.Background()); !errors.Is(err, infoErr) {
		t.Fatalf("expected info failure propagated, got %v", err)
	}
}
