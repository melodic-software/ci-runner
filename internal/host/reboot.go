package host

import (
	"context"
	"strconv"
	"time"
)

// plannedOtherRestart is shutdown.exe /d p:0:0 (Other, planned). /c requires
// /d first; omitting it makes Windows reject the restart after drain.
const plannedOtherRestart = "p:0:0"

func rebootArgs(delay time.Duration, comment string) []string {
	args := []string{"/r", "/t", strconv.Itoa(int(delay / time.Second))}
	if comment != "" {
		args = append(args, "/d", plannedOtherRestart, "/c", comment)
	}
	return args
}

// RebootInitiator restarts the operator host after a completed capacity-zero
// drain. The delay is the shutdown.exe /t value, not the drain bound.
type RebootInitiator interface {
	Reboot(ctx context.Context, delay time.Duration, comment string) error
}
