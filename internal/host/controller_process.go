package host

import "context"

type ScheduledTaskStarter interface {
	Start(context.Context, string) error
}

type ProcessObserver interface {
	Open(uint32) (ProcessHandle, error)
}

type ProcessHandle interface {
	Wait(context.Context) (uint32, error)
	Close() error
}
