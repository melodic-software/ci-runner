//go:build windows

package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func InstallHealthWatchTask(ctx context.Context, tasks ScheduledTaskCLI, executablePath, configPath string, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("health watch check interval must be positive")
	}
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return errors.New("host configuration path is required")
	}
	schtasks, err := trustedSystemExecutable("schtasks.exe")
	if err != nil {
		return err
	}
	runnerExecutable, err := resolveExecutable(executablePath, func() (string, error) {
		path, execErr := os.Executable()
		if execErr != nil {
			return "", fmt.Errorf("resolve ci-runner executable: %w", execErr)
		}
		return filepath.Clean(path), nil
	})
	if err != nil {
		return err
	}
	minutes := max(int(interval/time.Minute), 1)
	command := fmt.Sprintf(
		`"%s" --config "%s" host health-watch check`,
		runnerExecutable,
		filepath.Clean(configPath),
	)
	args := []string{
		"/Create",
		"/F",
		"/SC", "MINUTE",
		"/MO", strconv.Itoa(minutes),
		"/TN", HealthWatchTaskName,
		"/TR", command,
		"/RU", "BUILTIN\\Users",
		"/RL", "LIMITED",
	}
	if _, err := resolvedCommandRunner(tasks.Runner).Run(ctx, schtasks, args...); err != nil {
		return fmt.Errorf("create scheduled task %q: %w", HealthWatchTaskName, err)
	}
	return nil
}
