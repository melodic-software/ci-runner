package statefs

import (
	"fmt"
	"time"
)

// LockWaitClass names which serialization stage a state-lock acquisition was
// waiting in when it gave up. The two stages have different holder classes --
// another goroutine inside this process versus another process on this host --
// and both previously surfaced the same bare "context canceled", leaving an
// operator no way to tell which one to look for.
type LockWaitClass string

const (
	// LockWaitInProcess is the per-scope token that serializes every in-process
	// user of one state scope. bootstrap.go and controller_main.go each share a
	// single locker between the state store and the jobs index, so this stage
	// is contended by the controller's own concurrent state work.
	LockWaitInProcess LockWaitClass = "in-process-token"
	// LockWaitWin32Mutex is the named Win32 mutex that serializes across
	// processes on the host. Only the Windows locker reaches this stage; a
	// wait here means another ci-runner process owns the scope.
	LockWaitWin32Mutex LockWaitClass = "win32-mutex"
)

// LockWaitError attributes a failed state-lock acquisition to exactly one wait
// class and reports how long that stage alone waited.
//
// Elapsed is per-stage, never cumulative: a long in-process wait followed by a
// short kernel wait must not be reported as kernel contention, because acting
// on that attribution means hunting the wrong holder.
//
// The class and the duration are part of Error() rather than only struct
// fields because the consuming call sites wrap this error into an operator-
// facing message (jobindex wraps it as "lock jobs index: %w", the reconciler
// records it as a problem cause); none of them type-assert.
type LockWaitError struct {
	Class   LockWaitClass
	Elapsed time.Duration
	Err     error
}

func (e *LockWaitError) Error() string {
	return fmt.Sprintf("waited %s on the %s: %v", e.Elapsed.Round(time.Millisecond), e.Class, e.Err)
}

// Unwrap exposes the underlying context error so existing classification --
// notably controller.pollSuperseded's errors.Is(err, context.Canceled) --
// keeps working against an attributed lock failure.
func (e *LockWaitError) Unwrap() error { return e.Err }

func lockWaitError(class LockWaitClass, started time.Time, err error) *LockWaitError {
	return &LockWaitError{Class: class, Elapsed: time.Since(started), Err: err}
}
