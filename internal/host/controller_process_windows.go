//go:build windows

package host

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

const (
	processSynchronize             = 0x00100000
	processQueryLimitedInformation = 0x00001000
	processWaitObject0             = 0x00000000
	processWaitTimeout             = 0x00000102
	processWaitPollMS              = 200
)

type ScheduledTaskCLI struct {
	Runner         CommandRunner
	executablePath string
}

func (s ScheduledTaskCLI) Start(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("scheduled task name is required")
	}
	executable, err := resolveExecutable(s.executablePath, func() (string, error) { return trustedSystemExecutable("schtasks.exe") })
	if err != nil {
		return err
	}
	if _, err := resolvedCommandRunner(s.Runner).Run(ctx, executable, "/Run", "/TN", name); err != nil {
		return fmt.Errorf("start scheduled task %q: %w", name, err)
	}
	return nil
}

type WindowsProcessObserver struct{}

func (WindowsProcessObserver) Open(processID uint32) (ProcessHandle, error) {
	if processID == 0 {
		return nil, errors.New("controller process ID is required")
	}
	handle, err := windows.OpenProcess(processSynchronize|processQueryLimitedInformation, false, processID)
	if err != nil {
		return nil, fmt.Errorf("open controller process %d: %w", processID, err)
	}
	return &windowsProcessHandle{handle: handle}, nil
}

type windowsProcessHandle struct {
	handle windows.Handle
}

func (p *windowsProcessHandle) Wait(ctx context.Context) (uint32, error) {
	if p.handle == 0 {
		return 0, errors.New("controller process handle is closed")
	}
	for {
		result, waitErr := windows.WaitForSingleObject(p.handle, processWaitPollMS)
		switch result {
		case processWaitObject0:
			var exitCode uint32
			if err := windows.GetExitCodeProcess(p.handle, &exitCode); err != nil {
				return 0, fmt.Errorf("read controller exit code: %w", err)
			}
			return exitCode, nil
		case processWaitTimeout:
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("wait for controller process: %w", waitErr)
		}
	}
}

func (p *windowsProcessHandle) Close() error {
	if p.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(p.handle)
	p.handle = 0
	if err != nil {
		return fmt.Errorf("close controller process handle: %w", err)
	}
	return nil
}
