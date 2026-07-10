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
	"syscall"
	"unsafe"
)

const (
	waitObject0   = 0x00000000
	waitAbandoned = 0x00000080
	waitTimeout   = 0x00000102
	mutexPollMS   = 200
)

var (
	mutexKernel32           = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex         = mutexKernel32.NewProc("CreateMutexW")
	procWaitForSingleObject = mutexKernel32.NewProc("WaitForSingleObject")
	procReleaseMutex        = mutexKernel32.NewProc("ReleaseMutex")
	procCloseHandle         = mutexKernel32.NewProc("CloseHandle")
	procMutexLocalFree      = mutexKernel32.NewProc("LocalFree")
	mutexAdvapi32           = syscall.NewLazyDLL("advapi32.dll")
	procStringSDToSD        = mutexAdvapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
)

type securityAttributes struct {
	Length             uint32
	SecurityDescriptor uintptr
	InheritHandle      int32
}

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

func (m *WindowsMutex) Lock(ctx context.Context) (func() error, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
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
	defer procMutexLocalFree.Call(descriptor)
	attributes := securityAttributes{
		Length:             uint32(unsafe.Sizeof(securityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	name, err := syscall.UTF16PtrFromString(m.name)
	if err != nil {
		result <- windowsMutexAcquisition{err: err}
		return
	}
	handle, _, callErr := procCreateMutex.Call(
		uintptr(unsafe.Pointer(&attributes)),
		0,
		uintptr(unsafe.Pointer(name)),
	)
	if handle == 0 {
		result <- windowsMutexAcquisition{err: fmt.Errorf("CreateMutexW: %w", windowsCallError(callErr))}
		return
	}
	for {
		waitResult, _, waitErr := procWaitForSingleObject.Call(handle, mutexPollMS)
		switch waitResult {
		case waitObject0, waitAbandoned:
			release := make(chan struct{})
			done := make(chan error, 1)
			acquired := windowsMutexAcquisition{release: release, done: done}
			result <- acquired
			<-release
			var releaseFailure error
			released, _, releaseErr := procReleaseMutex.Call(handle)
			if released == 0 {
				releaseFailure = fmt.Errorf("ReleaseMutex: %w", windowsCallError(releaseErr))
				// Exiting while still locked to this OS thread forces the runtime
				// to terminate it, so Windows abandons rather than leaks the mutex.
				terminateOwnedThread = true
			}
			closeFailure := closeWindowsHandle(handle)
			done <- errors.Join(releaseFailure, closeFailure)
			return
		case waitTimeout:
			if err := ctx.Err(); err != nil {
				result <- windowsMutexAcquisition{err: errors.Join(err, closeWindowsHandle(handle))}
				return
			}
		default:
			waitFailure := fmt.Errorf("WaitForSingleObject returned %#x: %w", waitResult, windowsCallError(waitErr))
			result <- windowsMutexAcquisition{err: errors.Join(waitFailure, closeWindowsHandle(handle))}
			return
		}
	}
}

func closeWindowsHandle(handle uintptr) error {
	closed, _, closeErr := procCloseHandle.Call(handle)
	if closed == 0 {
		return fmt.Errorf("CloseHandle: %w", windowsCallError(closeErr))
	}
	return nil
}

func mutexSecurityDescriptor(sid string) (uintptr, error) {
	sddl := fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;%s)", sid)
	pointer, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return 0, err
	}
	var descriptor uintptr
	result, _, callErr := procStringSDToSD.Call(
		uintptr(unsafe.Pointer(pointer)),
		1,
		uintptr(unsafe.Pointer(&descriptor)),
		0,
	)
	if result == 0 {
		return 0, fmt.Errorf("convert mutex security descriptor: %w", windowsCallError(callErr))
	}
	return descriptor, nil
}

func windowsCallError(err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return errors.New("Windows API call failed")
	}
	return err
}
