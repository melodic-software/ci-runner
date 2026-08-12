package host

import (
	"context"
	"time"
)

const HealthWatchTaskName = "ci-runner-health-watch"

type HealthWatchTaskInstaller interface {
	InstallHealthWatchTask(context.Context, string, string, time.Duration) error
}
