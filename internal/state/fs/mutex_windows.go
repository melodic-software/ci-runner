//go:build windows

package statefs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/user"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	waitObject0   = 0x00000000
	waitAbandoned = 0x00000080
	waitTimeout   = 0x00000102
	mutexPollMS   = 200
)

type WindowsMutex struct {
	name  string
	sid   string
	local chan struct{}
}

type windowsMutexAcquisition struct {
	release chan struct{}
	done    chan error
	err     error
}

var (
	windowsLockersMu sync.Mutex
	windowsLockers   = map[string]chan struct{}{}
)

func NewPlatformLocker(scope string) (Locker, error) {
	current, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("resolve current Windows identity: %w", err)
	}
	if current.Uid == "" {
		return nil, errors.New("current Windows identity has no SID")
	}
	sum := sha256.Sum256([]byte(scope + "\x00" + current.Uid))
	name := fmt.Sprintf(`Local\ci-runner-state-%x`, sum[:12])
	windowsLockersMu.Lock()
	local, exists := windowsLockers[name]
	if !exists {
		local = make(chan struct{}, 1)
		local <- struct{}{}
		windowsLockers[name] = local
	}
	windowsLockersMu.Unlock()
	return &WindowsMutex{name: name, sid: current.Uid, local: local}, nil
}

// Lock passes through two waits with different holder classes: first the
// in-process token shared by every locker on this scope, then the named Win32
// mutex shared with every other process on this host. Each stage times itself
// and attributes its own failure so the wait that actually blocked is named.
func (m *WindowsMutex) Lock(ctx context.Context) (func() error, error) {
	tokenWaitStarted := time.Now()
	select {
	case <-ctx.Done():
		return nil, lockWaitError(LockWaitInProcess, tokenWaitStarted, ctx.Err())
	case <-m.local:
	}
	releaseLocal := true
	defer func() {
		if releaseLocal {
			m.local <- struct{}{}
		}
	}()
	result := make(chan windowsMutexAcquisition, 1)
	go m.acquireOnOwnedThread(ctx, result)
	acquisition := <-result
	if acquisition.err != nil {
		return nil, acquisition.err
	}
	releaseLocal = false
	var once sync.Once
	var unlockErr error
	return func() error {
		once.Do(func() {
			close(acquisition.release)
			unlockErr = <-acquisition.done
			m.local <- struct{}{}
		})
		return unlockErr
	}, nil
}

// acquireOnOwnedThread keeps WaitForSingleObject and ReleaseMutex on the same
// native thread. A Win32 mutex is owned by the acquiring thread, not by the Go
// goroutine; returning a raw release closure lets the goroutine migrate and can
// fail with ERROR_NOT_OWNER. The dedicated owner accepts release through a
// channel so callers may safely unlock from any goroutine.
func (m *WindowsMutex) acquireOnOwnedThread(ctx context.Context, result chan<- windowsMutexAcquisition) {
	runtime.LockOSThread()
	terminateOwnedThread := false
	defer func() {
		if !terminateOwnedThread {
			runtime.UnlockOSThread()
		}
	}()
	if err := ctx.Err(); err != nil {
		result <- windowsMutexAcquisition{err: err}
		return
	}
	descriptor, err := mutexSecurityDescriptor(m.sid)
	if err != nil {
		result <- windowsMutexAcquisition{err: err}
		return
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	name, err := windows.UTF16PtrFromString(m.name)
	if err != nil {
		result <- windowsMutexAcquisition{err: err}
		return
	}
	// CreateMutexW returns a usable handle to the pre-existing object and sets
	// ERROR_ALREADY_EXISTS whenever another process created the mutex first, and
	// x/sys reports that as an error alongside the valid handle. Treating it as a
	// failure defeats the entire point of a named mutex: every process but the
	// first aborted here instead of entering the wait below, so nothing ever
	// serialized across processes and the handle leaked. Opening the existing
	// object ignores the security descriptor above and inherits the creator's --
	// the same DACL, because the name is salted with the current user's SID.
	handle, err := windows.CreateMutex(&attributes, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		result <- windowsMutexAcquisition{err: fmt.Errorf("CreateMutexW: %w", err)}
		return
	}
	// Timed from here, after the handle exists, so the reported duration covers
	// only the kernel wait and never the earlier in-process token wait.
	mutexWaitStarted := time.Now()
	for {
		waitResult, waitErr := windows.WaitForSingleObject(handle, mutexPollMS)
		switch waitResult {
		case waitObject0, waitAbandoned:
			release := make(chan struct{})
			done := make(chan error, 1)
			acquired := windowsMutexAcquisition{release: release, done: done}
			result <- acquired
			<-release
			var releaseFailure error
			if err := windows.ReleaseMutex(handle); err != nil {
				releaseFailure = fmt.Errorf("ReleaseMutex: %w", err)
				// Exiting while still locked to this OS thread forces the runtime
				// to terminate it, so Windows abandons rather than leaks the mutex.
				terminateOwnedThread = true
			}
			closeFailure := closeWindowsHandle(handle)
			done <- errors.Join(releaseFailure, closeFailure)
			return
		case waitTimeout:
			if err := ctx.Err(); err != nil {
				attributed := lockWaitError(LockWaitWin32Mutex, mutexWaitStarted, err)
				result <- windowsMutexAcquisition{err: errors.Join(attributed, closeWindowsHandle(handle))}
				return
			}
		default:
			waitFailure := fmt.Errorf("WaitForSingleObject returned %#x: %w", waitResult, waitErr)
			result <- windowsMutexAcquisition{err: errors.Join(waitFailure, closeWindowsHandle(handle))}
			return
		}
	}
}

func closeWindowsHandle(handle windows.Handle) error {
	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("CloseHandle: %w", err)
	}
	return nil
}

func mutexSecurityDescriptor(sid string) (*windows.SECURITY_DESCRIPTOR, error) {
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;%s)", sid))
	if err != nil {
		return nil, fmt.Errorf("convert mutex security descriptor: %w", err)
	}
	return descriptor, nil
}
