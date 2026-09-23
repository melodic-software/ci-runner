package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"
)

// heartbeatStallIntervals is how many reconcile intervals may pass without a
// persisted observed-state heartbeat before WatchHeartbeat captures a
// goroutine dump. Listener polls checkpoint the heartbeat every interval, but
// a legitimately long Step outside a poll (a Docker Desktop start or worker
// image pull) also goes this long silent and costs one dump per occurrence;
// raise this if those dumps prove noisy.
const heartbeatStallIntervals = 12

type Hardener interface {
	Harden(string) error
}

// dumpGoroutines writes every goroutine's stack (the runtime/pprof
// goroutine profile at debug=2) to a new file under directory and returns its
// path. Top-level regular files there are bounded by the diagnostics
// retention and total-cap sweeps.
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
	interval := r.config.Controller.ReconcileInterval.Duration
	watch := heartbeatWatch{reconciler: r, threshold: heartbeatStallIntervals * interval, started: time.Now()}
	ticker := time.NewTicker(interval)
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
