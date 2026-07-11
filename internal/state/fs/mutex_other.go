//go:build !windows

package statefs

import (
	"context"
	"sync"
)

var (
	processLockersMu sync.Mutex
	processLockers   = map[string]chan struct{}{}
)

type processLocker struct{ semaphore chan struct{} }

func NewPlatformLocker(scope string) (Locker, error) {
	processLockersMu.Lock()
	defer processLockersMu.Unlock()
	semaphore, ok := processLockers[scope]
	if !ok {
		semaphore = make(chan struct{}, 1)
		semaphore <- struct{}{}
		processLockers[scope] = semaphore
	}
	return processLocker{semaphore: semaphore}, nil
}

func (l processLocker) Lock(ctx context.Context) (func() error, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.semaphore:
		var once sync.Once
		return func() error {
			once.Do(func() { l.semaphore <- struct{}{} })
			return nil
		}, nil
	}
}
