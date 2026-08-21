package host

import (
	"context"
	"time"
)

// RebootInitiator restarts the operator host after a completed capacity-zero
// drain. The delay is the shutdown.exe /t value, not the drain bound.
type RebootInitiator interface {
	Reboot(ctx context.Context, delay time.Duration, comment string) error
}
