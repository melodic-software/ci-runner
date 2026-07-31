//go:build windows

package statefs

import (
	"context"
	"testing"
	"time"
)

// TestWindowsMutexAttributesWin32MutexWait covers the second wait stage, which
// only a second process can normally reach: NewPlatformLocker deliberately
// shares one in-process token per mutex name, so an in-process contender always
// blocks at the earlier stage. Constructing the contender directly with its own
// free token reproduces the cross-process shape -- token available, kernel
// mutex held elsewhere -- inside one test process.
func TestWindowsMutexAttributesWin32MutexWait(t *testing.T) {
	locker, err := NewPlatformLocker("lock-wait-attribution-win32-test")
	if err != nil {
		t.Fatal(err)
	}
	holder, ok := locker.(*WindowsMutex)
	if !ok {
		t.Fatalf("platform locker = %T, want *WindowsMutex", locker)
	}
	unlock, err := holder.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unlock(); err != nil {
			t.Error(err)
		}
	}()

	token := make(chan struct{}, 1)
	token <- struct{}{}
	contender := &WindowsMutex{name: holder.name, sid: holder.sid, local: token}

	// Longer than one mutexPollMS interval so the poll loop reaches its
	// cancellation check rather than failing before the first wait completes.
	ctx, cancel := context.WithTimeout(context.Background(), 3*mutexPollMS*time.Millisecond)
	defer cancel()
	_, lockErr := contender.Lock(ctx)
	assertLockWait(t, lockErr, LockWaitWin32Mutex, context.DeadlineExceeded)
}
