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
	if d.executablePath != "" {
		return d.executablePath, nil
	}
	return trustedDockerDesktopExecutable()
}

func (d DockerDesktopCLI) runner() CommandRunner {
	if d.Runner == nil {
		return ExecCommandRunner{}
	}
	return d.Runner
}

func (d DockerDesktopCLI) Status(ctx context.Context) (DesktopStatus, error) {
	executable, err := d.executable()
	if err != nil {
		return DesktopStatusUnknown, err
	}
	out, err := d.runner().Run(ctx, executable, "desktop", "status", "--format", "json")
	if err != nil {
		return DesktopStatusUnknown, err
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
	if d.executablePath != "" {
		return d.executablePath, nil
	}
	return trustedDockerDesktopExecutable()
}

func (d DockerCLIInspector) runner() CommandRunner {
	if d.Runner == nil {
		return ExecCommandRunner{}
	}
	return d.Runner
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
