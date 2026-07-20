//go:build windows

package host

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

const (
	processSynchronize             = 0x00100000
	processQueryLimitedInformation = 0x00001000
	processWaitObject0             = 0x00000000
	processWaitTimeout             = 0x00000102
	processWaitPollMS              = 200
)

var (
	processKernel32        = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess        = processKernel32.NewProc("OpenProcess")
	procWaitForProcess     = processKernel32.NewProc("WaitForSingleObject")
	procGetExitCodeProcess = processKernel32.NewProc("GetExitCodeProcess")
	procCloseProcessHandle = processKernel32.NewProc("CloseHandle")
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
	handle, _, callErr := procOpenProcess.Call(
		processSynchronize|processQueryLimitedInformation,
		0,
		uintptr(processID),
	)
	if handle == 0 {
		return nil, fmt.Errorf("open controller process %d: %w", processID, monitorCallError(callErr))
	}
	return &windowsProcessHandle{handle: handle}, nil
}

type windowsProcessHandle struct {
	handle uintptr
}

func (p *windowsProcessHandle) Wait(ctx context.Context) (uint32, error) {
	if p.handle == 0 {
		return 0, errors.New("controller process handle is closed")
	}
	for {
		result, _, callErr := procWaitForProcess.Call(p.handle, processWaitPollMS)
		switch result {
		case processWaitObject0:
			var exitCode uint32
			ok, _, exitErr := procGetExitCodeProcess.Call(p.handle, uintptr(unsafe.Pointer(&exitCode)))
			if ok == 0 {
				return 0, fmt.Errorf("read controller exit code: %w", monitorCallError(exitErr))
			}
			return exitCode, nil
		case processWaitTimeout:
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("wait for controller process: %w", monitorCallError(callErr))
		}
	}
}

func (p *windowsProcessHandle) Close() error {
	if p.handle == 0 {
		return nil
	}
	ok, _, callErr := procCloseProcessHandle.Call(p.handle)
	p.handle = 0
	if ok == 0 {
		return fmt.Errorf("close controller process handle: %w", monitorCallError(callErr))
	}
	return nil
}
