package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

const (
	resourceEvidenceSchemaVersion = 1
	maximumResourceEvidenceBytes  = 32 * 1024
	resourceEvidenceSourceCgroup  = "cgroup-v2"
	resourceEvidenceSourceHost    = "controller-fallback"
)

var resourceEvidenceFields = []string{
	"cpu.stat.nr_periods",
	"cpu.stat.nr_throttled",
	"cpu.stat.throttled_usec",
	"io.stat",
	"memory.events.oom",
	"memory.events.oom_kill",
	"memory.peak",
	"memory.swap.peak",
	"pids.peak",
}

type ResourceEvidence struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Source        string                 `json:"source"`
	Status        string                 `json:"status"`
	Reason        string                 `json:"reason,omitempty"`
	Missing       []string               `json:"missing"`
	Memory        ResourceEvidenceMemory `json:"memory"`
	CPU           ResourceEvidenceCPU    `json:"cpu"`
	PIDs          ResourceEvidencePIDs   `json:"pids"`
	IO            ResourceEvidenceIO     `json:"io"`
}

type ResourceEvidenceMemory struct {
	PeakBytes     uint64 `json:"peakBytes"`
	SwapPeakBytes uint64 `json:"swapPeakBytes"`
	OOMEvents     uint64 `json:"oomEvents"`
	OOMKillEvents uint64 `json:"oomKillEvents"`
}

type ResourceEvidenceCPU struct {
	Periods               uint64 `json:"periods"`
	ThrottledPeriods      uint64 `json:"throttledPeriods"`
	ThrottledMicroseconds uint64 `json:"throttledMicroseconds"`
}

type ResourceEvidencePIDs struct {
	Peak uint64 `json:"peak"`
}

type ResourceEvidenceIO struct {
	ReadBytes  uint64 `json:"readBytes"`
	WriteBytes uint64 `json:"writeBytes"`
}

func ParseResourceEvidence(source io.Reader) (ResourceEvidence, error) {
	limited := &io.LimitedReader{R: source, N: maximumResourceEvidenceBytes + 1}
	content, err := io.ReadAll(limited)
	if err != nil {
		return ResourceEvidence{}, fmt.Errorf("read terminal resource evidence: %w", err)
	}
	if limited.N <= 0 {
		return ResourceEvidence{}, errors.New("terminal resource evidence exceeds 32 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var evidence ResourceEvidence
	if err := decoder.Decode(&evidence); err != nil {
		return ResourceEvidence{}, fmt.Errorf("decode terminal resource evidence: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ResourceEvidence{}, errors.New("terminal resource evidence must contain exactly one JSON value")
	}
	if err := evidence.Validate(); err != nil {
		return ResourceEvidence{}, err
	}
	return evidence, nil
}

func (e ResourceEvidence) Validate() error {
	if e.SchemaVersion != resourceEvidenceSchemaVersion {
		return fmt.Errorf("terminal resource evidence has unsupported schema version %d", e.SchemaVersion)
	}
	if e.Source != resourceEvidenceSourceCgroup && e.Source != resourceEvidenceSourceHost {
		return fmt.Errorf("terminal resource evidence has unsupported source %q", e.Source)
	}
	switch e.Status {
	case "complete", "partial", "unavailable", "invalid":
	default:
		return fmt.Errorf("terminal resource evidence has unsupported status %q", e.Status)
	}
	if e.Source == resourceEvidenceSourceCgroup && e.Reason != "" {
		return errors.New("cgroup terminal resource evidence must not contain a fallback reason")
	}
	if e.Source == resourceEvidenceSourceCgroup && e.Status == "invalid" {
		return errors.New("cgroup terminal resource evidence must not self-classify as invalid")
	}
	if e.Source == resourceEvidenceSourceHost {
		if e.Status != "unavailable" && e.Status != "invalid" {
			return errors.New("controller fallback resource evidence must be unavailable or invalid")
		}
		if e.Reason != "docker-copy-unavailable" && e.Reason != "invalid-evidence" {
			return fmt.Errorf("terminal resource fallback has unsupported reason %q", e.Reason)
		}
	}
	seen := make(map[string]struct{}, len(e.Missing))
	for _, field := range e.Missing {
		if !slices.Contains(resourceEvidenceFields, field) {
			return fmt.Errorf("terminal resource evidence has unsupported missing field %q", field)
		}
		if _, duplicate := seen[field]; duplicate {
			return fmt.Errorf("terminal resource evidence repeats missing field %q", field)
		}
		seen[field] = struct{}{}
	}
	if e.Status == "complete" && len(e.Missing) != 0 {
		return errors.New("complete terminal resource evidence must not name missing fields")
	}
	if e.Status == "partial" && (len(e.Missing) == 0 || len(e.Missing) == len(resourceEvidenceFields)) {
		return errors.New("partial terminal resource evidence must name some but not all fields")
	}
	if (e.Status == "unavailable" || e.Status == "invalid") && len(e.Missing) != len(resourceEvidenceFields) {
		return errors.New("unavailable or invalid terminal resource evidence must name every field")
	}
	return nil
}

func fallbackResourceEvidence(status, reason string) ResourceEvidence {
	return ResourceEvidence{
		SchemaVersion: resourceEvidenceSchemaVersion,
		Source:        resourceEvidenceSourceHost,
		Status:        status,
		Reason:        reason,
		Missing:       append([]string(nil), resourceEvidenceFields...),
	}
}

func marshalResourceEvidence(evidence ResourceEvidence) ([]byte, error) {
	if err := evidence.Validate(); err != nil {
		return nil, err
	}
	content, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}
