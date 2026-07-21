//go:build windows

package host

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const localDockerEngineHost = "npipe:////./pipe/docker_engine"

// managedContainerLabel is the label GamingManager's Docker inventory uses to
// classify a container as CI-managed; only Windows hosts query it, since
// Containers is the sole consumer of a live container listing.
const managedContainerLabel = "com.melodic-software.ci-runner.managed"

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

// engineClient is the narrow subset of the official Moby SDK client that
// DockerEngineInspector depends on. *client.Client structurally implements
// it; tests substitute a fake to stay independent of a real Docker Engine.
type engineClient interface {
	Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error)
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	Close() error
}

// newLocalEngineClient opens a Moby SDK client pinned to the fixed local
// Docker Engine endpoint, mirroring internal/runtime/docker's newLocalClient.
// host constructs its own client rather than sharing runtime/docker's Engine
// type, keeping the two packages decoupled.
func newLocalEngineClient() (engineClient, error) {
	return client.New(client.WithHost(localDockerEngineHost), client.WithUserAgent("ci-runner-host"))
}

// DockerEngineInspector answers factual questions about the local Docker
// Engine API via the official Moby SDK client, opened per call and closed
// before returning -- the same lifecycle internal/runtime/docker's
// newLocalClient callers use.
type DockerEngineInspector struct {
	// NewClient constructs an engine client for a single call. Tests
	// substitute a fake; nil uses the real local Moby client pinned to
	// localDockerEngineHost.
	NewClient func() (engineClient, error)
}

func (d DockerEngineInspector) dial() (engineClient, error) {
	if d.NewClient != nil {
		return d.NewClient()
	}
	return newLocalEngineClient()
}

func (d DockerEngineInspector) EngineReachable(ctx context.Context) (bool, error) {
	apiClient, err := d.dial()
	if err != nil {
		return false, fmt.Errorf("create Docker engine client: %w", err)
	}
	// A close failure on a short-lived reachability probe carries no
	// actionable signal beyond the reachability answer already computed
	// below, and folding it in here would break the exact (false, nil)
	// unreachable contract GamingManager depends on.
	defer func() { _ = apiClient.Close() }()
	if _, err := apiClient.Info(ctx, client.InfoOptions{}); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, nil
	}
	return true, nil
}

func (d DockerEngineInspector) Containers(ctx context.Context) ([]Container, error) {
	apiClient, err := d.dial()
	if err != nil {
		return nil, fmt.Errorf("create Docker engine client: %w", err)
	}
	result, listErr := apiClient.ContainerList(ctx, client.ContainerListOptions{})
	closeErr := apiClient.Close()
	if listErr != nil {
		return nil, errors.Join(fmt.Errorf("list Docker containers: %w", listErr), closeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close Docker engine client: %w", closeErr)
	}
	containers := make([]Container, 0, len(result.Items))
	for _, item := range result.Items {
		containers = append(containers, containerFromSummary(item))
	}
	return containers, nil
}

func containerFromSummary(item containertypes.Summary) Container {
	return Container{
		ID:      item.ID,
		Name:    containerDisplayName(item.Names),
		Image:   item.Image,
		Status:  item.Status,
		Labels:  item.Labels,
		Managed: strings.EqualFold(item.Labels[managedContainerLabel], "true"),
	}
}

// containerDisplayName strips the Engine API's leading "/" from each
// container name, matching what `docker ps` displays.
func containerDisplayName(names []string) string {
	trimmed := make([]string, len(names))
	for i, name := range names {
		trimmed[i] = strings.TrimPrefix(name, "/")
	}
	return strings.Join(trimmed, ",")
}
