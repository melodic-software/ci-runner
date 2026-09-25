package statefs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPlatformLockerAttributesInProcessTokenWait pins the in-process token stage:
// lockers on one scope share a token, so a second acquisition blocks there.
func TestPlatformLockerAttributesInProcessTokenWait(t *testing.T) {
	holder, err := NewPlatformLocker("lock-wait-attribution-token-test")
	if err != nil {
		t.Fatal(err)
	}
	contender, err := NewPlatformLocker("lock-wait-attribution-token-test")
	if err != nil {
		t.Fatal(err)
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

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, lockErr := contender.Lock(ctx)
	assertLockWait(t, lockErr, LockWaitInProcess, context.DeadlineExceeded)
}

func assertLockWait(t *testing.T, err error, class LockWaitClass, cause error) {
	t.Helper()
	var wait *LockWaitError
	if !errors.As(err, &wait) {
		t.Fatalf("lock error = %v (%T), want a *LockWaitError", err, err)
	}
	if wait.Class != class {
		t.Errorf("wait class = %q, want %q", wait.Class, class)
	}
	if wait.Elapsed <= 0 {
		t.Errorf("wait elapsed = %s, want a positive duration", wait.Elapsed)
	}
	if !errors.Is(err, cause) {
		t.Errorf("lock error = %v, want it to unwrap to %v", err, cause)
	}
	message := err.Error()
	if !strings.Contains(message, string(class)) {
		t.Errorf("lock error message = %q, want it to name the %q wait class", message, class)
	}
	if !strings.Contains(message, "waited ") {
		t.Errorf("lock error message = %q, want it to report the wait duration", message)
	}
}
