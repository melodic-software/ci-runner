package healthwatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	managedLabel = "com.melodic-software.ci-runner.managed"
	hostLabel    = "com.melodic-software.ci-runner.host"
	poolLabel    = "com.melodic-software.ci-runner.pool"
)

type ContainerLister interface {
	Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error)
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	Close() error
}

type DockerInventory struct {
	NewClient func(controllerVersion string) (ContainerLister, error)
	Version   string
}

func NewDockerInventory(controllerVersion string) DockerInventory {
	return DockerInventory{
		Version: controllerVersion,
		NewClient: func(controllerVersion string) (ContainerLister, error) {
			return client.New(
				client.WithHost("npipe:////./pipe/dockerDesktopLinuxEngine"),
				client.WithUserAgent("ci-runner/"+controllerVersion),
			)
		},
	}
}

func (d DockerInventory) RunningByPool(ctx context.Context, hostID string) (map[string]int, error) {
	if strings.TrimSpace(hostID) == "" {
		return nil, fmt.Errorf("host ID is required")
	}
	apiClient, err := d.NewClient(d.Version)
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	info, infoErr := apiClient.Info(ctx, client.InfoOptions{})
	if infoErr != nil {
		return nil, errors.Join(infoErr, apiClient.Close())
	}
	if err := validateEngineInfo(info.Info.OSType, info.Info.Architecture); err != nil {
		return nil, errors.Join(err, apiClient.Close())
	}
	filters := make(client.Filters).Add("label", managedLabel+"=true", hostLabel+"="+hostID)
	result, listErr := apiClient.ContainerList(ctx, client.ContainerListOptions{All: false, Filters: filters})
	closeErr := apiClient.Close()
	if listErr != nil {
		return nil, errors.Join(fmt.Errorf("list managed worker containers: %w", listErr), closeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close Docker client: %w", closeErr)
	}
	counts := map[string]int{}
	for _, item := range result.Items {
		if item.State != containertypes.StateRunning {
			continue
		}
		poolID := item.Labels[poolLabel]
		if poolID == "" {
			continue
		}
		counts[poolID]++
	}
	return counts, nil
}

func validateEngineInfo(operatingSystem, architecture string) error {
	if operatingSystem != "linux" || architecture != "x86_64" && architecture != "amd64" {
		return fmt.Errorf("local Docker Engine must be linux/amd64, got %s/%s", operatingSystem, architecture)
	}
	return nil
}
