package controller

import (
	"context"
	"errors"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/scaleset"
	statepkg "github.com/melodic-software/ci-runner/internal/state"
)

type ScaleSetClient interface{ scaleset.Client }
type StateStore interface{ statepkg.Store }

type ActiveJobLookup interface {
	ActiveJob(context.Context, string, string) (string, bool, error)
}

type StartWorkerRequest struct {
	PoolID       string
	Name         string
	ResourceTier string
	JITConfig    scaleset.JITConfig
	Limits       config.Worker
}

type WorkerStartError struct {
	Err               error
	RunnerMayBeActive bool
}

func (e *WorkerStartError) Error() string { return e.Err.Error() }
func (e *WorkerStartError) Unwrap() error { return e.Err }

// RunnerStartMayBeActive fails closed for untyped runtime errors.
func RunnerStartMayBeActive(err error) bool {
	var typed *WorkerStartError
	return !errors.As(err, &typed) || typed.RunnerMayBeActive
}

type WorkerRuntime interface {
	List(context.Context) ([]model.Worker, error)
	Start(context.Context, StartWorkerRequest) (model.Worker, error)
	// RemoveIfIdle must re-check the worker at the point of removal and return
	// false without stopping it if work has been acquired. This is the final
	// defense against a job-acquisition race during drain.
	RemoveIfIdle(context.Context, string) (bool, error)
}

type DesktopManager interface {
	Status(context.Context) (model.DesktopStatus, error)
	Start(context.Context, time.Duration) error
	Stop(context.Context, time.Duration) error
	ShutdownAllWSL(context.Context) error
}

type PowerMonitor interface {
	Snapshot(context.Context) (model.PowerSnapshot, error)
}

type ResourceMonitor interface {
	Snapshot(context.Context) (model.ResourceSnapshot, error)
}

// EngineMemoryProbe reports the total memory of the VM backing the Docker
// engine (the WSL2 VM on Windows), the kernel-truth ceiling a configured
// worker memory budget is cross-checked against.
type EngineMemoryProbe interface {
	EngineMemoryTotal(context.Context) (uint64, error)
}

// unknownEngineMemory is the default probe: total unknown, so a configured
// budget is trusted as-is.
type unknownEngineMemory struct{}

func (unknownEngineMemory) EngineMemoryTotal(context.Context) (uint64, error) { return 0, nil }

type SecretMaterial = scaleset.SecretMaterial
type SecretStore = scaleset.SecretStore

var NewSecretMaterial = scaleset.NewSecretMaterial

type LogEvent struct {
	At      time.Time
	Code    string
	Message string
	// Cause carries the underlying error detail behind a classified Message.
	// Message stays a stable, sanitized summary (see safeScaleSetMessage) while
	// Cause preserves diagnostic specifics such as a GitHub activity or request
	// ID. Sinks redact Cause like every other field before it is persisted.
	Cause    string
	Source   string
	PoolID   string
	WorkerID string
}

type LogSink interface {
	Write(context.Context, LogEvent) error
}

type DiscardLogSink struct{}

func (DiscardLogSink) Write(context.Context, LogEvent) error { return nil }
