package docker

import (
	"strings"
	"testing"
)

const completeResourceEvidence = `{
  "schemaVersion": 1,
  "source": "cgroup-v2",
  "status": "complete",
  "missing": [],
  "memory": {"peakBytes": 1986422374, "swapPeakBytes": 0, "oomEvents": 0, "oomKillEvents": 0},
  "cpu": {"periods": 123, "throttledPeriods": 7, "throttledMicroseconds": 50000},
  "pids": {"peak": 88},
  "io": {"readBytes": 2000000000, "writeBytes": 5500000000}
}`

func TestParseResourceEvidenceAcceptsExactCgroupV2Shape(t *testing.T) {
	t.Parallel()
	evidence, err := ParseResourceEvidence(strings.NewReader(completeResourceEvidence))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Memory.PeakBytes != 1986422374 || evidence.CPU.ThrottledPeriods != 7 || evidence.PIDs.Peak != 88 || evidence.IO.WriteBytes != 5500000000 {
		t.Fatalf("resource evidence = %#v", evidence)
	}
}

func TestParseResourceEvidenceRejectsUnknownAndInconsistentData(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"unknown field":      strings.Replace(completeResourceEvidence, `"missing": []`, `"unknown": true, "missing": []`, 1),
		"complete missing":   strings.Replace(completeResourceEvidence, `"missing": []`, `"missing": ["memory.peak"]`, 1),
		"unsupported source": strings.Replace(completeResourceEvidence, `"source": "cgroup-v2"`, `"source": "workflow"`, 1),
		"trailing value":     completeResourceEvidence + `{}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseResourceEvidence(strings.NewReader(input)); err == nil {
				t.Fatal("invalid resource evidence accepted")
			}
		})
	}
}

func TestFallbackResourceEvidenceIsBoundedAndRoundTrips(t *testing.T) {
	t.Parallel()
	evidence := fallbackResourceEvidence("unavailable", "docker-copy-unavailable")
	content, err := marshalResourceEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseResourceEvidence(strings.NewReader(string(content)))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Source != resourceEvidenceSourceHost || parsed.Status != "unavailable" || len(parsed.Missing) != len(resourceEvidenceFields) {
		t.Fatalf("fallback = %#v", parsed)
	}
}
