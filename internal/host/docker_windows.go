//go:build windows

package host

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

const localDockerEngineHost = "npipe:////./pipe/docker_engine"

type DockerDesktopCLI struct {
	Runner         CommandRunner
	executablePath string
}

func (d DockerDesktopCLI) executable() (string, error) {
	return resolveExecutable(d.executablePath, trustedDockerDesktopExecutable)
}

func (d DockerDesktopCLI) runner() CommandRunner {
	return resolvedCommandRunner(d.Runner)
}

func (d DockerDesktopCLI) Status(ctx context.Context) (DesktopStatus, error) {
	executable, err := d.executable()
	if err != nil {
		return DesktopStatusUnknown, err
	}
	out, err := d.runner().Run(ctx, executable, "desktop", "status", "--format", "json")
	if err != nil {
		// A canceled or expired context kills the probe process, which also
		// surfaces as *exec.ExitError; that is an aborted probe, not evidence the
		// desktop is stopped, and misreading it would let a timed-out probe skip
		// the Docker worker inventory. Propagate the context error instead.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return DesktopStatusUnknown, ctxErr
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return DesktopStatusUnknown, err
		}
		// `docker desktop status` exits non-zero precisely when Docker Desktop is
		// not running -- the same signal EngineReachable maps to a factual state
		// rather than a hard error. Trust a concrete state named in the captured
		// output; otherwise a non-zero exit is itself the stopped signal.
		if status, parseErr := parseDesktopStatus(out); parseErr == nil {
			return status, nil
		}
		return DesktopStatusStopped, nil
	}
	return parseDesktopStatus(out)
}

func (d DockerDesktopCLI) Start(ctx context.Context) error {
	executable, err := d.executable()
	if err != nil {
		return err
	}
	_, err = d.runner().Run(ctx, executable, "desktop", "start")
	return err
}

func (d DockerDesktopCLI) Stop(ctx context.Context) error {
	executable, err := d.executable()
	if err != nil {
		return err
	}
	_, err = d.runner().Run(ctx, executable, "desktop", "stop")
	return err
}

type DockerCLIInspector struct {
	Runner         CommandRunner
	executablePath string
}

func (d DockerCLIInspector) executable() (string, error) {
	return resolveExecutable(d.executablePath, trustedDockerDesktopExecutable)
}

func (d DockerCLIInspector) runner() CommandRunner {
	return resolvedCommandRunner(d.Runner)
}

func (d DockerCLIInspector) EngineReachable(ctx context.Context) (bool, error) {
	executable, err := d.executable()
	if err != nil {
		return false, err
	}
	_, err = d.runner().Run(ctx, executable, "--host", localDockerEngineHost, "info", "--format", "{{json .ServerVersion}}")
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, fmt.Errorf("query Docker engine: %w", err)
}

func (d DockerCLIInspector) Containers(ctx context.Context) ([]Container, error) {
	executable, err := d.executable()
	if err != nil {
		return nil, err
	}
	out, err := d.runner().Run(ctx, executable, "--host", localDockerEngineHost, "ps", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return parseDockerPS(out)
}
