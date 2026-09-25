package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"
)

// ReconcileLivenessIntervals is how many reconcile intervals the heartbeat may
// miss before the reconcile loop counts as stalled.
const ReconcileLivenessIntervals = 6

// ReconcileLivenessFloor keeps a short reconcile interval from reporting a
// stall during Step phases that write no heartbeat (Docker Desktop start, JIT
// config requests, image pulls).
const ReconcileLivenessFloor = 5 * time.Minute

// ReconcileLivenessLimit bounds heartbeat age for a live controller. Listener
// polls checkpoint the heartbeat every interval, so a heartbeat older than
// this means the loop itself has stopped. WatchHeartbeat dumps goroutines and
// host doctor reports FAIL at this same age. The product saturates at the
// largest representable time.Duration instead of overflowing.
func ReconcileLivenessLimit(interval time.Duration) time.Duration {
	if interval > math.MaxInt64/ReconcileLivenessIntervals {
		return math.MaxInt64
	}
	return max(ReconcileLivenessFloor, interval*ReconcileLivenessIntervals)
}

type Hardener interface {
	Harden(string) error
}

// Top-level regular files under directory are bounded by the diagnostics retention and
// total-cap sweeps.
func dumpGoroutines(directory, reason string, at time.Time, acl Hardener) (string, error) {
	path := filepath.Join(directory, fmt.Sprintf("controller-goroutines-%s-%s.txt", reason, at.UTC().Format("20060102T150405.000000000Z")))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create goroutine dump: %w", err)
	}
	err = errors.Join(pprof.Lookup("goroutine").WriteTo(file, 2), file.Close())
	if err == nil && acl != nil {
		err = acl.Harden(path)
	}
	if err != nil {
		return "", errors.Join(fmt.Errorf("write goroutine dump %q: %w", path, err), os.Remove(path))
	}
	return path, nil
}

func (r *Reconciler) writeGoroutineDump(ctx context.Context, reason string, at time.Time) (string, error) {
	path, err := dumpGoroutines(r.config.Paths.Diagnostics, reason, at, r.deps.ACL)
	if err != nil {
		r.writeLog(ctx, LogEvent{At: at.UTC(), Code: "goroutine-dump-error", Message: err.Error()})
		return "", err
	}
	r.writeLog(ctx, LogEvent{At: at.UTC(), Code: "goroutine-dump-written", Message: fmt.Sprintf("reason=%s path=%s", reason, path)})
	return path, nil
}

// heartbeatWatch captures one goroutine dump per heartbeat stall episode and
// re-arms once the heartbeat is fresh again.
type heartbeatWatch struct {
	reconciler *Reconciler
	threshold  time.Duration
	started    time.Time
	dumped     bool
}

func (r *Reconciler) newHeartbeatWatch(started time.Time) heartbeatWatch {
	return heartbeatWatch{reconciler: r, threshold: ReconcileLivenessLimit(r.config.Controller.ReconcileInterval.Duration), started: started}
}

func (w *heartbeatWatch) check(ctx context.Context, now time.Time) {
	last := time.Unix(0, w.reconciler.heartbeat.Load())
	if last.Before(w.started) {
		// A process that has not persisted a heartbeat yet stalls from start.
		last = w.started
	}
	if now.Sub(last) < w.threshold {
		w.dumped = false
		return
	}
	if w.dumped {
		return
	}
	w.dumped = true
	_, _ = w.reconciler.writeGoroutineDump(ctx, "heartbeat-stall", now)
}

// WatchHeartbeat runs until ctx is canceled, writing a goroutine dump under
// the diagnostics directory when the reconcile heartbeat stalls. It takes no
// reconciler lock, so it keeps running when the reconcile loop is wedged.
func (r *Reconciler) WatchHeartbeat(ctx context.Context) {
	watch := r.newHeartbeatWatch(time.Now())
	ticker := time.NewTicker(r.config.Controller.ReconcileInterval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			watch.check(ctx, now)
		}
	}
}
