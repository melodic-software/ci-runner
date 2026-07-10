//go:build windows

package statefs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWindowsMutexSerializesSameUserScopeAndHonorsCancellation(t *testing.T) {
	first, err := NewPlatformLocker("native-mutex-test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewPlatformLocker("native-mutex-test")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := first.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := second.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want deadline", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	release, err := second.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsMutexRepeatedReleaseFromDifferentGoroutine(t *testing.T) {
	locker, err := NewPlatformLocker("native-mutex-cross-goroutine-test")
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 128; iteration++ {
		unlock, err := locker.Lock(context.Background())
		if err != nil {
			t.Fatalf("iteration %d lock: %v", iteration, err)
		}
		released := make(chan error, 1)
		go func() { released <- unlock() }()
		select {
		case err := <-released:
			if err != nil {
				t.Fatalf("iteration %d unlock: %v", iteration, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d unlock timed out", iteration)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := locker.Lock(ctx)
	if err != nil {
		t.Fatalf("lock after repeated releases: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatalf("final unlock: %v", err)
	}
}
