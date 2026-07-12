package childprocess

import (
	"context"
	"os/exec"
)

// CommandContext constructs a child process with the platform safeguards used
// by the controller's non-interactive command boundaries.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	configure(cmd)
	return cmd
}
