package host

import (
	"context"
	"fmt"

	"github.com/melodic-software/ci-runner/internal/childprocess"
)

// CommandRunner is the process boundary used by host adapters. Keeping it
// narrow makes command construction and failure handling independently
// testable without starting Docker Desktop or WSL.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecCommandRunner executes a process directly, without an intermediate
// shell. None of the host lifecycle adapters pass credentials on command
// lines.
type ExecCommandRunner struct{}

func (ExecCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := childprocess.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("run %s: %w", name, err)
	}
	return out, nil
}
