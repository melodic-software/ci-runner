//go:build !windows

package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/state"
)

func TestHealthWatchInstallTaskRequiresWindows(t *testing.T) {
	var out, errOut bytes.Buffer
	application, err := New(Dependencies{
		Config: config.Config{
			HealthWatchdog: config.HealthWatchdog{
				CheckInterval: config.Duration{Duration: time.Minute},
			},
		},
		Store: state.NewMemoryStore(),
	}, strings.NewReader(""), &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	code := application.Run(context.Background(), []string{
		"host", "health-watch", "install-task",
		"--config", `C:\Users\runner\AppData\Local\ci-runner\config.yaml`,
		"--executable", `C:\Program Files\ci-runner\ci-runner.exe`,
	})
	if code != ExitRuntime {
		t.Fatalf("exit code = %d, want %d", code, ExitRuntime)
	}
	if !strings.Contains(errOut.String(), "host operations for Docker Desktop and WSL require Windows") {
		t.Fatalf("stderr = %q, want Windows host required", errOut.String())
	}
}
