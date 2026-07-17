package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/controller"
)

const (
	maximumStructuredLogEvent  = 1 << 20
	structuredLogQueueCapacity = 64
	structuredLogWriteTimeout  = 2 * time.Second
	structuredLogCloseTimeout  = 2 * time.Second
)

var (
	pemLogPattern           = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?-----END [A-Z0-9 ]+-----`)
	jwtLogPattern           = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	githubTokenLogPattern   = regexp.MustCompile(`\b(?:gh[pousr]_|github_pat_)[A-Za-z0-9_]{20,}\b`)
	authorizationLogPattern = regexp.MustCompile(`(?i)authorization\s*[:=]\s*(?:bearer\s+)?\S+`)
	sensitiveLogPattern     = regexp.MustCompile(`(?i)(token|secret|private[_ -]?key|jit(?:config)?)\s*[:=]\s*\S+`)
)

type LogAccessController interface {
	Harden(string) error
}

type JSONLogSink struct {
	mu           sync.Mutex
	directory    string
	policy       config.LogClass
	cleanupEvery time.Duration
	acl          LogAccessController
	now          func() time.Time

	file          *os.File
	path          string
	size          uint64
	day           string
	lastCleanupAt time.Time

	lifecycle    sync.RWMutex
	closed       bool
	requests     chan structuredLogWriteRequest
	queueSpace   chan struct{}
	workerDone   chan struct{}
	closeErr     error
	writeTimeout time.Duration
	closeTimeout time.Duration

	queueTimeouts atomic.Uint64
	writeTimeouts atomic.Uint64
	closeTimeouts atomic.Uint64
}

type structuredLogWriteRequest struct {
	event  controller.LogEvent
	result chan error
}

// JSONLogSinkHealth is a lock-free process-lifetime snapshot of diagnostic
// delivery failures. Callers can inspect it even when the file worker is
// stalled and therefore cannot emit its own failure diagnostic.
type JSONLogSinkHealth struct {
	QueueTimeouts uint64
	WriteTimeouts uint64
	CloseTimeouts uint64
}

func NewJSONLogSink(directory string, policy config.LogClass, cleanupEvery time.Duration, acl LogAccessController) (*JSONLogSink, error) {
	if directory == "" || !filepath.IsAbs(directory) {
		return nil, errors.New("controller log directory must be absolute")
	}
	if policy.MaxFileSize == 0 || policy.Retention.Duration <= 0 || policy.TotalCap < policy.MaxFileSize {
		return nil, errors.New("controller log rotation, retention, and total cap must be positive and consistent")
	}
	if acl == nil {
		return nil, errors.New("controller log access controller is required")
	}
	if cleanupEvery <= 0 {
		return nil, errors.New("controller log cleanup interval must be positive")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create controller log directory: %w", err)
	}
	if err := acl.Harden(directory); err != nil {
		return nil, fmt.Errorf("secure controller log directory: %w", err)
	}
	sink := &JSONLogSink{
		directory: filepath.Clean(directory), policy: policy, cleanupEvery: cleanupEvery, acl: acl,
		now: func() time.Time { return time.Now().UTC() }, requests: make(chan structuredLogWriteRequest, structuredLogQueueCapacity),
		queueSpace: make(chan struct{}, 1), workerDone: make(chan struct{}),
		writeTimeout: structuredLogWriteTimeout, closeTimeout: structuredLogCloseTimeout,
	}
	sink.mu.Lock()
	err := sink.cleanupLocked(sink.now())
	sink.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go sink.run()
	return sink, nil
}

func (s *JSONLogSink) Write(ctx context.Context, event controller.LogEvent) error {
	writeContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.writeTimeout)
	defer cancel()
	request := structuredLogWriteRequest{event: event, result: make(chan error, 1)}

	for {
		// Keep the lifecycle lock around only a nonblocking send. Close can
		// therefore acquire the writer lock and start its own deadline even
		// when the file worker and queue are both stalled.
		s.lifecycle.RLock()
		if s.closed {
			s.lifecycle.RUnlock()
			return errors.New("controller log sink is closed")
		}
		select {
		case s.requests <- request:
			s.lifecycle.RUnlock()
			goto accepted
		default:
			s.lifecycle.RUnlock()
		}
		select {
		case <-writeContext.Done():
			s.queueTimeouts.Add(1)
			return fmt.Errorf("queue controller log event: %w", writeContext.Err())
		case <-s.queueSpace:
		}
	}

accepted:
	select {
	case err := <-request.result:
		return err
	case <-writeContext.Done():
		s.writeTimeouts.Add(1)
		return fmt.Errorf("write controller log event: %w", writeContext.Err())
	}
}

// Health reports diagnostic delivery failures without taking either sink
// lock. This remains safe to call when a platform file write is hung.
func (s *JSONLogSink) Health() JSONLogSinkHealth {
	return JSONLogSinkHealth{
		QueueTimeouts: s.queueTimeouts.Load(),
		WriteTimeouts: s.writeTimeouts.Load(),
		CloseTimeouts: s.closeTimeouts.Load(),
	}
}

func (s *JSONLogSink) run() {
	defer close(s.workerDone)
	for request := range s.requests {
		select {
		case s.queueSpace <- struct{}{}:
		default:
		}
		request.result <- s.write(request.event)
	}
	s.mu.Lock()
	s.closeErr = s.closeFileLocked()
	s.mu.Unlock()
}

func (s *JSONLogSink) write(event controller.LogEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if event.At.IsZero() {
		event.At = now
	}
	record := struct {
		At       time.Time `json:"at"`
		Code     string    `json:"code"`
		Message  string    `json:"message"`
		Cause    string    `json:"cause,omitempty"`
		Source   string    `json:"source,omitempty"`
		PoolID   string    `json:"poolId,omitempty"`
		WorkerID string    `json:"workerId,omitempty"`
	}{
		At: event.At.UTC(), Code: redactLogValue(event.Code), Message: redactLogValue(event.Message),
		Cause:  redactLogValue(event.Cause),
		Source: redactLogValue(event.Source), PoolID: redactLogValue(event.PoolID), WorkerID: redactLogValue(event.WorkerID),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode controller log event: %w", err)
	}
	if len(encoded) > maximumStructuredLogEvent {
		record.Code = truncateLogValue(record.Code, 256)
		// Message and Cause each cap at a quarter of the ceiling so their combined
		// worst case, plus the small fields and JSON overhead, still fits after the
		// single retry below.
		record.Message = truncateLogValue(record.Message, maximumStructuredLogEvent/4)
		record.Cause = truncateLogValue(record.Cause, maximumStructuredLogEvent/4)
		record.Source = truncateLogValue(record.Source, 256)
		record.PoolID = truncateLogValue(record.PoolID, 256)
		record.WorkerID = truncateLogValue(record.WorkerID, 256)
		encoded, err = json.Marshal(record)
		if err != nil {
			return fmt.Errorf("encode truncated controller log event: %w", err)
		}
		if len(encoded) > maximumStructuredLogEvent {
			return errors.New("controller log event remains over safety limit after truncation")
		}
	}
	encoded = append(encoded, '\n')
	day := now.Format("20060102")
	if s.file == nil || s.day != day || (s.size > 0 && s.size+uint64(len(encoded)) > uint64(s.policy.MaxFileSize)) {
		if err := s.rotateLocked(now); err != nil {
			return err
		}
	}
	written, err := s.file.Write(encoded)
	if err != nil {
		return fmt.Errorf("write controller log: %w", err)
	}
	if written != len(encoded) {
		return io.ErrShortWrite
	}
	s.size += uint64(written)
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("flush controller log: %w", err)
	}
	if s.lastCleanupAt.IsZero() || now.Sub(s.lastCleanupAt) >= s.cleanupEvery {
		if err := s.cleanupLocked(now); err != nil {
			return err
		}
	}
	return nil
}

func (s *JSONLogSink) Close() error {
	s.lifecycle.Lock()
	if !s.closed {
		s.closed = true
		close(s.requests)
	}
	s.lifecycle.Unlock()
	select {
	case <-s.workerDone:
		return s.closeErr
	case <-time.After(s.closeTimeout):
		s.closeTimeouts.Add(1)
		return fmt.Errorf("close controller log sink: %w", context.DeadlineExceeded)
	}
}

func (s *JSONLogSink) closeFileLocked() error {
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	s.path = ""
	s.size = 0
	return err
}

func (s *JSONLogSink) rotateLocked(now time.Time) error {
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			return fmt.Errorf("close rotated controller log: %w", err)
		}
		s.file = nil
	}
	base := "controller-" + now.UTC().Format("20060102T150405.000000000Z")
	var file *os.File
	var path string
	for suffix := 0; suffix < 1000; suffix++ {
		name := base + ".jsonl"
		if suffix > 0 {
			name = fmt.Sprintf("%s-%03d.jsonl", base, suffix)
		}
		path = filepath.Join(s.directory, name)
		opened, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("create controller log: %w", err)
		}
		file = opened
		break
	}
	if file == nil {
		return errors.New("could not allocate a unique controller log filename")
	}
	if err := s.acl.Harden(path); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("secure controller log: %w", err)
	}
	s.file = file
	s.path = path
	s.size = 0
	s.day = now.UTC().Format("20060102")
	return nil
}

type retainedLog struct {
	path     string
	modified time.Time
	size     uint64
}

func (s *JSONLogSink) cleanupLocked(now time.Time) error {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return fmt.Errorf("list controller logs: %w", err)
	}
	cutoff := now.Add(-s.policy.Retention.Duration)
	logs := make([]retainedLog, 0, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "controller-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(s.directory, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect controller log %q: %w", entry.Name(), err)
		}
		if path != s.path && info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove expired controller log %q: %w", entry.Name(), err)
			}
			continue
		}
		logs = append(logs, retainedLog{path: path, modified: info.ModTime(), size: uint64(info.Size())})
	}
	sort.Slice(logs, func(i, j int) bool {
		if logs[i].modified.Equal(logs[j].modified) {
			return logs[i].path < logs[j].path
		}
		return logs[i].modified.Before(logs[j].modified)
	})
	total := uint64(0)
	for _, log := range logs {
		total += log.size
	}
	for _, log := range logs {
		if total <= uint64(s.policy.TotalCap) {
			break
		}
		if log.path == s.path {
			continue
		}
		if err := os.Remove(log.path); err != nil {
			return fmt.Errorf("remove controller log over total cap: %w", err)
		}
		total -= log.size
	}
	s.lastCleanupAt = now.UTC()
	return nil
}

func redactLogValue(value string) string {
	redacted := pemLogPattern.ReplaceAllString(value, "[REDACTED PEM]")
	redacted = jwtLogPattern.ReplaceAllString(redacted, "[REDACTED JWT]")
	redacted = githubTokenLogPattern.ReplaceAllString(redacted, "[REDACTED GITHUB TOKEN]")
	redacted = authorizationLogPattern.ReplaceAllString(redacted, "authorization=[REDACTED]")
	redacted = sensitiveLogPattern.ReplaceAllStringFunc(redacted, func(match string) string {
		key, _, _ := strings.Cut(match, "=")
		if key == match {
			key, _, _ = strings.Cut(match, ":")
		}
		return strings.TrimSpace(key) + "=[REDACTED]"
	})
	return redacted
}

func truncateLogValue(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + " [TRUNCATED]"
}

var _ controller.LogSink = (*JSONLogSink)(nil)
