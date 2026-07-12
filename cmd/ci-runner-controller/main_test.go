package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/melodic-software/ci-runner/internal/app"
)

func TestControllerExitCodeDistinguishesVerifiedRestart(t *testing.T) {
	t.Parallel()
	if got := controllerExitCode(fmt.Errorf("controller stopped: %w", app.ErrControllerRestartRequested)); got != int(app.ControllerRestartExitCode) {
		t.Fatalf("restart exit code = %d, want %d", got, app.ControllerRestartExitCode)
	}
	if got := controllerExitCode(errors.New("ordinary controller failure")); got != 1 {
		t.Fatalf("ordinary failure exit code = %d, want 1", got)
	}
}
