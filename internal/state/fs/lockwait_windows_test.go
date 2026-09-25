//go:build windows

package statefs

import (
	"context"
	"testing"
	"time"
)

// TestWindowsMutexAttributesWin32MutexWait gives the contender its own free token
// so it reaches the kernel-mutex stage that normally only a second process can.
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
