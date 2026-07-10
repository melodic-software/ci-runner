// Package model contains the platform-neutral state shared by the controller,
// its adapters, and the CLI. It deliberately contains no Docker, Windows, or
// GitHub API types.
package model

import "time"

// Mode is the user-owned desired lifecycle mode.
type Mode string

const (
	ModeEnabled  Mode = "enabled"
	ModeDisabled Mode = "disabled"
	ModeGaming   Mode = "gaming"
)

func (m Mode) Valid() bool {
	switch m {
	case ModeEnabled, ModeDisabled, ModeGaming:
		return true
	default:
		return false
	}
}

// Phase is the controller-owned observed lifecycle phase.
type Phase string

const (
	PhaseStarting            Phase = "starting"
	PhaseReady               Phase = "ready"
	PhaseResourceConstrained Phase = "resource-constrained"
	PhasePowerSuspended      Phase = "power-suspended"
	PhaseDraining            Phase = "draining"
	PhaseDisabled            Phase = "disabled"
	PhaseGaming              Phase = "gaming"
	PhaseDegraded            Phase = "degraded"
)

// DesiredState is written by the user-facing CLI and never overwritten by
// provisioning. A nil capacity override means use the checked-in host limit.
type DesiredState struct {
	SchemaVersion             int       `json:"schemaVersion"`
	Mode                      Mode      `json:"mode"`
	TemporaryCapacityOverride *int      `json:"temporaryCapacityOverride,omitempty"`
	UpdatedAt                 time.Time `json:"updatedAt"`
}

// WorkerState is the small lifecycle vocabulary understood by policy code.
type WorkerState string

const (
	WorkerStarting WorkerState = "starting"
	WorkerIdle     WorkerState = "idle"
	WorkerBusy     WorkerState = "busy"
	WorkerExited   WorkerState = "exited"
)

// Worker describes a managed, one-job worker. Adapter-specific container data
// belongs in AdapterID, not in controller policy.
type Worker struct {
	ID        string      `json:"id"`
	AdapterID string      `json:"adapterId,omitempty"`
	PoolID    string      `json:"poolId"`
	Name      string      `json:"name"`
	State     WorkerState `json:"state"`
	JobID     string      `json:"jobId,omitempty"`
	RunnerID  int64       `json:"runnerId,omitempty"`
	StartedAt time.Time   `json:"startedAt"`
}

func (w Worker) Active() bool {
	return w.State == WorkerStarting || w.State == WorkerIdle || w.State == WorkerBusy
}

// PoolObservation is persisted so a restart can resume an existing scale set
// rather than creating or sharing a listener accidentally.
type PoolObservation struct {
	ID                string `json:"id"`
	ScaleSetID        int64  `json:"scaleSetId,omitempty"`
	ListenerID        string `json:"listenerId,omitempty"`
	TotalAssignedJobs int    `json:"totalAssignedJobs"`
	MaxCapacity       int    `json:"maxCapacity"`
	// CapacityAcknowledged is true only when the most recent listener poll
	// successfully reported MaxCapacity. A numeric zero without this bit is not
	// proof that GitHub accepted a drain.
	CapacityAcknowledged bool `json:"capacityAcknowledged"`
	// ZeroCapacityConfirmations counts consecutive successful listener polls
	// that advertised zero and reported zero assigned jobs. Automatic idle
	// retirement requires two confirmations so a stale pre-drain statistic can
	// never authorize SIGTERM.
	ZeroCapacityConfirmations int       `json:"zeroCapacityConfirmations"`
	DrainServiceCapacity      int       `json:"drainServiceCapacity,omitempty"`
	DesiredWorkers            int       `json:"desiredWorkers"`
	UpdatedAt                 time.Time `json:"updatedAt"`
}

// ResourceSnapshot is an instantaneous host observation. Total and available
// physical memory use bytes; CPUUtilizationPercent is in [0,100].
type ResourceSnapshot struct {
	TotalMemoryBytes      uint64  `json:"totalMemoryBytes"`
	AvailableMemoryBytes  uint64  `json:"availableMemoryBytes"`
	CPUUtilizationPercent float64 `json:"cpuUtilizationPercent"`
}

// PowerSnapshot is supplied by the host adapter. ACConnected is deliberately
// explicit even on a desktop so policy remains deterministic.
type PowerSnapshot struct {
	ACConnected bool      `json:"acConnected"`
	ObservedAt  time.Time `json:"observedAt"`
}

// DesktopStatus captures only facts that affect lifecycle policy.
type DesktopStatus struct {
	DesktopRunning  bool `json:"desktopRunning"`
	EngineReachable bool `json:"engineReachable"`
	RunningWSLCount int  `json:"runningWslCount"`
}

// ResourceGateState persists observation and hysteresis windows across a
// controller restart.
type ResourceGateState struct {
	Blocked      bool       `json:"blocked"`
	HighCPUSince *time.Time `json:"highCpuSince,omitempty"`
	HealthySince *time.Time `json:"healthySince,omitempty"`
}

// PowerGateState persists stable-AC recovery state across a restart.
type PowerGateState struct {
	ACSince *time.Time `json:"acSince,omitempty"`
}

// Problem is safe, structured status intended for both JSON and human output.
type Problem struct {
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	PoolID    string    `json:"poolId,omitempty"`
	Retryable bool      `json:"retryable"`
	At        time.Time `json:"at"`
}

// ObservedState is controller-owned. Sensitive values, JIT configurations,
// credentials, and adapter request payloads must never be put here.
type ObservedState struct {
	SchemaVersion  int               `json:"schemaVersion"`
	Phase          Phase             `json:"phase"`
	HeartbeatAt    time.Time         `json:"heartbeatAt"`
	DrainStartedAt *time.Time        `json:"drainStartedAt,omitempty"`
	Version        string            `json:"version"`
	Pools          []PoolObservation `json:"pools"`
	Workers        []Worker          `json:"workers"`
	Resources      ResourceSnapshot  `json:"resources"`
	Power          PowerSnapshot     `json:"power"`
	Desktop        DesktopStatus     `json:"desktop"`
	ResourceGate   ResourceGateState `json:"resourceGate"`
	PowerGate      PowerGateState    `json:"powerGate"`
	Problems       []Problem         `json:"problems,omitempty"`
}
