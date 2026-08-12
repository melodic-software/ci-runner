//go:build windows

package host

import (
	"context"
	"errors"
	"fmt"
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
	executable, err := resolveExecutable(executablePath, func() (string, error) {
		return trustedSystemExecutable("schtasks.exe")
	})
	if err != nil {
		return err
	}
	runnerExecutable, err := currentExecutablePath(executablePath)
	if err != nil {
		return err
	}
	minutes := int(interval / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
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
	if _, err := resolvedCommandRunner(tasks.Runner).Run(ctx, executable, args...); err != nil {
		return fmt.Errorf("create scheduled task %q: %w", HealthWatchTaskName, err)
	}
	return nil
}

func currentExecutablePath(override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return filepath.Clean(override), nil
	}
	return "", errors.New("ci-runner executable path is required on Windows")
}
