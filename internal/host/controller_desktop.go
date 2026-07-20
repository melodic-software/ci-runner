package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/melodic-software/ci-runner/internal/model"
)

// ControllerDesktopAdapter presents Docker Desktop and WSL through the narrow
// factual interface consumed by the platform-neutral controller.
type ControllerDesktopAdapter struct {
	Desktop      DesktopManager
	Docker       DockerInspector
	WSL          WSLManager
	PollInterval time.Duration
}

func (a ControllerDesktopAdapter) Status(ctx context.Context) (model.DesktopStatus, error) {
	if a.Desktop == nil || a.Docker == nil || a.WSL == nil {
		return model.DesktopStatus{}, errors.New("desktop adapter dependencies are incomplete")
	}
	desktopStatus, desktopErr := a.Desktop.Status(ctx)
	reachable, dockerErr := a.Docker.EngineReachable(ctx)
	distributions, wslErr := a.WSL.Running(ctx)
	if err := errors.Join(desktopErr, dockerErr, wslErr); err != nil {
		return model.DesktopStatus{}, err
	}
	return model.DesktopStatus{
		DesktopRunning:  desktopStatus == DesktopStatusRunning,
		EngineReachable: reachable,
		RunningWSLCount: len(distributions),
	}, nil
}

func (a ControllerDesktopAdapter) Start(ctx context.Context, timeout time.Duration) error {
	if a.Desktop == nil || a.Docker == nil {
		return errors.New("desktop adapter dependencies are incomplete")
	}
	operation, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := a.Desktop.Start(operation); err != nil {
		return fmt.Errorf("start Docker Desktop: %w", err)
	}
	return poll(operation, a.interval(), func(ctx context.Context) (bool, error) {
		status, reachable, ok := a.probe(ctx)
		return ok && status == DesktopStatusRunning && reachable, nil
	}, "Docker Desktop did not become ready")
}

func (a ControllerDesktopAdapter) Stop(ctx context.Context, timeout time.Duration) error {
	if a.Desktop == nil || a.Docker == nil {
		return errors.New("desktop adapter dependencies are incomplete")
	}
	operation, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := a.Desktop.Stop(operation); err != nil {
		return fmt.Errorf("stop Docker Desktop: %w", err)
	}
	return poll(operation, a.interval(), func(ctx context.Context) (bool, error) {
		status, reachable, ok := a.probe(ctx)
		return ok && status == DesktopStatusStopped && !reachable, nil
	}, "Docker Desktop did not stop cleanly")
}

func (a ControllerDesktopAdapter) ShutdownAllWSL(ctx context.Context) error {
	if a.WSL == nil {
		return errors.New("WSL adapter is unavailable")
	}
	return a.WSL.Shutdown(ctx)
}

// probe reports Docker Desktop's current status and engine reachability. ok
// is false when either query failed, so Start/Stop's poll predicates keep
// polling on a transient query error rather than treating it as ready.
func (a ControllerDesktopAdapter) probe(ctx context.Context) (status DesktopStatus, reachable bool, ok bool) {
	status, err := a.Desktop.Status(ctx)
	if err != nil {
		return DesktopStatusUnknown, false, false
	}
	reachable, err = a.Docker.EngineReachable(ctx)
	if err != nil {
		return DesktopStatusUnknown, false, false
	}
	return status, reachable, true
}

func (a ControllerDesktopAdapter) interval() time.Duration {
	if a.PollInterval > 0 {
		return a.PollInterval
	}
	return time.Second
}

func poll(ctx context.Context, interval time.Duration, predicate func(context.Context) (bool, error), timeoutMessage string) error {
	for {
		ready, err := predicate(ctx)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("%s: %w", timeoutMessage, ctx.Err())
		case <-timer.C:
		}
	}
}
