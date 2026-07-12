// Package control defines the local, current-user controller control plane.
// It carries no credentials or JIT data.
package control

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/melodic-software/ci-runner/internal/model"
)

const SchemaVersion = 1

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Operation string

const (
	OperationStatus           Operation = "status"
	OperationShutdown         Operation = "shutdown"
	OperationForceStopPreview Operation = "force-stop-preview"
	OperationForceStopExecute Operation = "force-stop-execute"
)

type Request struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     string            `json:"requestId"`
	Operation     Operation         `json:"op"`
	Shutdown      *ShutdownRequest  `json:"shutdown,omitempty"`
	ForceStop     *ForceStopRequest `json:"forceStop,omitempty"`
}

type ForceStopRequest struct {
	Expected []ForceStopTarget `json:"expected"`
}

type ForceStopTarget struct {
	WorkerID string            `json:"workerId"`
	PoolID   string            `json:"poolId"`
	Name     string            `json:"name"`
	State    model.WorkerState `json:"state"`
	JobID    string            `json:"jobId,omitempty"`
}

type ShutdownRequest struct {
	Reason                    string `json:"reason"`
	ExpectedProcessID         uint32 `json:"expectedProcessId"`
	ExpectedVersion           string `json:"expectedVersion"`
	ExpectedAssignedJobCount  int    `json:"expectedAssignedJobCount"`
	ExpectedActiveJobCount    int    `json:"expectedActiveJobCount"`
	ExpectedActiveWorkerCount int    `json:"expectedActiveWorkerCount"`
	RestartViaTaskScheduler   bool   `json:"restartViaTaskScheduler"`
}

type Response struct {
	SchemaVersion    int               `json:"schemaVersion"`
	RequestID        string            `json:"requestId"`
	OK               bool              `json:"ok"`
	ErrorCode        string            `json:"errorCode,omitempty"`
	Error            string            `json:"error,omitempty"`
	Status           *Status           `json:"status,omitempty"`
	ForceStopTargets []ForceStopTarget `json:"forceStopTargets,omitempty"`
}

type Status struct {
	Phase             model.Phase `json:"phase"`
	ProcessID         uint32      `json:"pid"`
	Version           string      `json:"version"`
	AssignedJobCount  int         `json:"assignedJobCount"`
	ActiveJobCount    int         `json:"activeJobCount"`
	ActiveWorkerCount int         `json:"activeWorkerCount"`
	ShuttingDown      bool        `json:"shuttingDown"`
	// RestartRequestID identifies an accepted authenticated restart. It is also
	// exposed by status while that exact drain is in progress so a reconnecting
	// CLI can retain the proof chain. The controller binds the same ID into its
	// durable completion receipt after all drain and runtime closes succeed.
	RestartRequestID string `json:"restartRequestId,omitempty"`
}

// Handler uses a two-phase shutdown contract. Handle may mark shutdown as
// accepted, but CommitShutdown is invoked only after that response has been
// written and flushed to the authenticated client. AbortShutdown releases the
// reservation when the response cannot be flushed.
type Handler interface {
	Handle(context.Context, Request) Response
	CommitShutdown(string)
	AbortShutdown(string)
}

func (r Request) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d", r.SchemaVersion)
	}
	if !requestIDPattern.MatchString(r.RequestID) {
		return errors.New("requestId contains unsupported characters or is too long")
	}
	switch r.Operation {
	case OperationStatus:
		if r.Shutdown != nil || r.ForceStop != nil {
			return errors.New("status request must not include operation data")
		}
	case OperationShutdown:
		if r.Shutdown == nil {
			return errors.New("shutdown request data is required")
		}
		if strings.TrimSpace(r.Shutdown.Reason) == "" || len(r.Shutdown.Reason) > 256 {
			return errors.New("shutdown reason is required and must be at most 256 characters")
		}
		if r.Shutdown.ExpectedProcessID == 0 {
			return errors.New("expected shutdown process ID is required")
		}
		if strings.TrimSpace(r.Shutdown.ExpectedVersion) == "" || len(r.Shutdown.ExpectedVersion) > 128 {
			return errors.New("expected shutdown version is required and must be at most 128 characters")
		}
		if r.Shutdown.ExpectedAssignedJobCount < 0 || r.Shutdown.ExpectedActiveJobCount < 0 || r.Shutdown.ExpectedActiveWorkerCount < 0 {
			return errors.New("expected shutdown counts must not be negative")
		}
		if r.ForceStop != nil {
			return errors.New("shutdown request must not include force-stop data")
		}
	case OperationForceStopPreview:
		if r.Shutdown != nil || r.ForceStop != nil {
			return errors.New("force-stop preview must not include operation data")
		}
	case OperationForceStopExecute:
		if r.Shutdown != nil || r.ForceStop == nil {
			return errors.New("force-stop execute requires only forceStop data")
		}
		seen := make(map[string]struct{}, len(r.ForceStop.Expected))
		for _, target := range r.ForceStop.Expected {
			if target.WorkerID == "" || target.PoolID == "" || target.Name == "" {
				return errors.New("force-stop targets require workerId, poolId, and name")
			}
			if _, duplicate := seen[target.WorkerID]; duplicate {
				return fmt.Errorf("duplicate force-stop workerId %q", target.WorkerID)
			}
			seen[target.WorkerID] = struct{}{}
			if target.State != model.WorkerStarting && target.State != model.WorkerIdle && target.State != model.WorkerBusy {
				return fmt.Errorf("force-stop target %q has inactive state %q", target.WorkerID, target.State)
			}
		}
	default:
		return fmt.Errorf("unsupported operation %q", r.Operation)
	}
	return nil
}

func ErrorResponse(requestID, code string, err error) Response {
	message := "request failed"
	if err != nil {
		message = err.Error()
	}
	return Response{SchemaVersion: SchemaVersion, RequestID: requestID, OK: false, ErrorCode: code, Error: message}
}
