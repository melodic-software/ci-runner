package host

import (
	"context"
	"errors"
	"testing"
	"time"
)

// blockingDesktop models `docker desktop status` while Docker Desktop is
// stopped: it outlives any budget it is given and reports the context's error,
// exactly as an aborted probe process does.
type blockingDesktop struct{ calls int }

func (d *blockingDesktop) Status(ctx context.Context) (DesktopStatus, error) {
	d.calls++
	<-ctx.Done()
	return DesktopStatusUnknown, ctx.Err()
}

func (d *blockingDesktop) Start(context.Context) error { return nil }
func (d *blockingDesktop) Stop(context.Context) error  { return nil }

// failingDesktop models a probe that fails for its own reasons — executable
// resolution, launch, or output parsing — rather than by exhausting its budget.
type failingDesktop struct{ err error }

func (d failingDesktop) Status(context.Context) (DesktopStatus, error) {
	return DesktopStatusUnknown, d.err
}

func (d failingDesktop) Start(context.Context) error { return nil }
func (d failingDesktop) Stop(context.Context) error  { return nil }

// slowDesktop answers, but only after a delay that exceeds the sibling probes'
// budget — the real `docker desktop status` shape while Desktop is stopped.
type slowDesktop struct {
	delay  time.Duration
	status DesktopStatus
}

func (d *slowDesktop) Status(ctx context.Context) (DesktopStatus, error) {
	timer := time.NewTimer(d.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return DesktopStatusUnknown, ctx.Err()
	case <-timer.C:
		return d.status, nil
	}
}

func (d *slowDesktop) Start(context.Context) error { return nil }
func (d *slowDesktop) Stop(context.Context) error  { return nil }

type answeringDesktop struct{ status DesktopStatus }

func (d answeringDesktop) Status(context.Context) (DesktopStatus, error) { return d.status, nil }
func (d answeringDesktop) Start(context.Context) error                   { return nil }
func (d answeringDesktop) Stop(context.Context) error                    { return nil }

type recordingDocker struct {
	reachable      bool
	engineCalls    int
	containerCalls int
}

func (d *recordingDocker) EngineReachable(ctx context.Context) (bool, error) {
	d.engineCalls++
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return d.reachable, nil
}

func (d *recordingDocker) Containers(ctx context.Context) ([]Container, error) {
	d.containerCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

type recordingWSL struct {
	running []string
	calls   int
}

func (w *recordingWSL) Running(ctx context.Context) ([]string, error) {
	w.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return w.running, nil
}

func (w *recordingWSL) Shutdown(context.Context) error { return nil }

func TestInventoryRunsEveryProbeOnItsOwnBudgetWhenTheDesktopProbeOutlivesIts(t *testing.T) {
	desktop := &blockingDesktop{}
	docker := &recordingDocker{reachable: false}
	wsl := &recordingWSL{running: []string{"docker-desktop"}}
	manager := GamingManager{Desktop: desktop, Docker: docker, WSL: wsl, ProbeTimeout: 20 * time.Millisecond}

	inventory := manager.Inventory(context.Background())

	if desktop.calls != 1 || docker.engineCalls != 1 || wsl.calls != 1 {
		t.Fatalf("every probe must run exactly once, got desktop=%d engine=%d wsl=%d", desktop.calls, docker.engineCalls, wsl.calls)
	}
	if inventory.DesktopStatus != DesktopStatusUnknown {
		t.Fatalf("a probe that never answered must not report a concrete status, got %#v", inventory.DesktopStatus)
	}
	if len(inventory.Problems) != 1 {
		t.Fatalf("only the desktop probe should have failed, got problems %#v", inventory.Problems)
	}
	if len(inventory.RunningDistributions) != 1 || inventory.RunningDistributions[0] != "docker-desktop" {
		t.Fatalf("the WSL probe must report its real value after a slow desktop probe, got %#v", inventory.RunningDistributions)
	}
}

func TestInventorySkipsTheContainerProbeWhenTheEngineIsUnreachable(t *testing.T) {
	docker := &recordingDocker{reachable: false}
	manager := GamingManager{
		Desktop:      answeringDesktop{status: DesktopStatusStopped},
		Docker:       docker,
		WSL:          &recordingWSL{},
		ProbeTimeout: time.Second,
	}

	manager.Inventory(context.Background())

	if docker.containerCalls != 0 {
		t.Fatalf("container inventory must not run against an unreachable engine, got %d calls", docker.containerCalls)
	}
}

func TestVerifyReportsATimedOutPostconditionAsUnverifiedRatherThanUnsatisfied(t *testing.T) {
	manager := GamingManager{
		Desktop:      &blockingDesktop{},
		Docker:       &recordingDocker{reachable: false},
		WSL:          &recordingWSL{},
		ProbeTimeout: 20 * time.Millisecond,
	}

	verification, err := manager.Verify(context.Background())

	if err == nil {
		t.Fatal("a postcondition that could not be checked must not verify clean")
	}
	if !verification.DesktopUnverified {
		t.Fatalf("a timed-out desktop probe must be marked unverified, got %#v", verification)
	}
	if verification.DesktopStopped {
		t.Fatalf("an unverified postcondition must not claim to be satisfied, got %#v", verification)
	}
	if verification.DockerUnverified || verification.WSLUnverified {
		t.Fatalf("probes that answered must not be marked unverified, got %#v", verification)
	}
}

func TestVerifyMarksAPostconditionUnverifiedWhenTheProbeFailsWithoutTimingOut(t *testing.T) {
	manager := GamingManager{
		Desktop:      failingDesktop{err: errors.New("resolve docker.exe")},
		Docker:       &recordingDocker{reachable: false},
		WSL:          &recordingWSL{},
		ProbeTimeout: time.Second,
	}

	verification, err := manager.Verify(context.Background())

	if err == nil {
		t.Fatal("a probe that returned an error must not verify clean")
	}
	if !verification.DesktopUnverified {
		t.Fatalf("a probe that answered with an error observed nothing, so it is unverified, got %#v", verification)
	}
}

func TestTheDesktopProbeUsesItsOwnBudgetWhileSiblingsKeepTheShorterOne(t *testing.T) {
	desktop := &slowDesktop{delay: 60 * time.Millisecond, status: DesktopStatusStopped}
	manager := GamingManager{
		Desktop:             desktop,
		Docker:              &recordingDocker{reachable: false},
		WSL:                 &recordingWSL{},
		ProbeTimeout:        20 * time.Millisecond,
		DesktopProbeTimeout: time.Second,
	}

	inventory := manager.Inventory(context.Background())

	if inventory.DesktopStatus != DesktopStatusStopped {
		t.Fatalf("the desktop probe must outlive the sibling budget and report its real status, got %#v", inventory.DesktopStatus)
	}
	if len(inventory.Problems) != 0 {
		t.Fatalf("no probe should have failed, got %#v", inventory.Problems)
	}
}

func TestVerifyDistinguishesAnObservedFailureFromAnUncheckedOne(t *testing.T) {
	manager := GamingManager{
		Desktop:      answeringDesktop{status: DesktopStatusRunning},
		Docker:       &recordingDocker{reachable: false},
		WSL:          &recordingWSL{},
		ProbeTimeout: time.Second,
	}

	verification, err := manager.Verify(context.Background())

	if err == nil {
		t.Fatal("a running desktop must fail verification")
	}
	if verification.DesktopUnverified {
		t.Fatalf("an answered probe reporting a bad state is observed, not unverified, got %#v", verification)
	}
}

func TestProbeWithoutATimeoutStillDerivesFromTheCallerContext(t *testing.T) {
	manager := GamingManager{Desktop: &blockingDesktop{}, Docker: &recordingDocker{}, WSL: &recordingWSL{}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	inventory := manager.Inventory(ctx)

	if len(inventory.Problems) == 0 {
		t.Fatal("an unset ProbeTimeout must still honor the caller's deadline")
	}
	if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		t.Fatalf("the caller's own deadline must be what expired, got %#v", context.Cause(ctx))
	}
}
