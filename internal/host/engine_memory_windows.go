//go:build windows

package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/moby/moby/client"
)

// NewEngineMemoryProbe reports the Docker engine VM's total memory (the WSL2
// VM's kernel MemTotal) for the controller's worker-memory-budget cross-check.
func NewEngineMemoryProbe() DockerEngineInspector {
	return DockerEngineInspector{}
}

func (d DockerEngineInspector) EngineMemoryTotal(ctx context.Context) (uint64, error) {
	apiClient, err := d.dial()
	if err != nil {
		return 0, fmt.Errorf("create Docker engine client: %w", err)
	}
	result, infoErr := apiClient.Info(ctx, client.InfoOptions{})
	closeErr := apiClient.Close()
	if infoErr != nil {
		return 0, errors.Join(fmt.Errorf("query Docker engine memory: %w", infoErr), closeErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close Docker engine client: %w", closeErr)
	}
	if result.Info.MemTotal <= 0 {
		return 0, errors.New("docker engine reported zero MemTotal")
	}
	return uint64(result.Info.MemTotal), nil
}
