//go:build windows

package host

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// NewEngineMemoryProbe reports the Docker engine VM's total memory (the WSL2
// VM's kernel MemTotal) for the controller's worker-memory-budget cross-check.
func NewEngineMemoryProbe() DockerCLIInspector {
	return DockerCLIInspector{}
}

func (d DockerCLIInspector) EngineMemoryTotal(ctx context.Context) (uint64, error) {
	executable, err := d.executable()
	if err != nil {
		return 0, err
	}
	out, err := d.runner().Run(ctx, executable, "--host", localDockerEngineHost, "info", "--format", "{{json .MemTotal}}")
	if err != nil {
		return 0, fmt.Errorf("query Docker engine memory: %w", err)
	}
	value := strings.TrimSpace(string(out))
	total, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse engine MemTotal %q: %w", value, err)
	}
	if total == 0 {
		return 0, errors.New("Docker engine reported zero MemTotal")
	}
	return total, nil
}
