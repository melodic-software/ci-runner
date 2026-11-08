package docker

import (
	"strings"
	"testing"
)

const completeResourceEvidence = `{
  "schemaVersion": 2,
  "source": "cgroup-v2",
  "status": "complete",
  "missing": [],
  "memory": {"peakBytes": 1986422374, "swapPeakBytes": 0, "oomEvents": 0, "oomKillEvents": 0},
  "cpu": {"periods": 123, "throttledPeriods": 7, "throttledMicroseconds": 50000, "usageMicroseconds": 900000000},
  "pids": {"peak": 88},
  "io": {"readBytes": 2000000000, "writeBytes": 5500000000}
}`

// legacyResourceEvidence is the exact schema-version-1 shape older worker
// images write; a controller upgrade must keep adopting it.
const legacyResourceEvidence = `{
  "schemaVersion": 1,
  "source": "cgroup-v2",
  "status": "complete",
  "missing": [],
  "memory": {"peakBytes": 1986422374, "swapPeakBytes": 0, "oomEvents": 0, "oomKillEvents": 0},
  "cpu": {"periods": 123, "throttledPeriods": 7, "throttledMicroseconds": 50000},
  "pids": {"peak": 88},
  "io": {"readBytes": 2000000000, "writeBytes": 5500000000}
}`

const legacyUnavailableResourceEvidence = `{"schemaVersion":1,"source":"controller-fallback","status":"unavailable","reason":"docker-copy-unavailable",` +
	`"missing":["cpu.stat.nr_periods","cpu.stat.nr_throttled","cpu.stat.throttled_usec","io.stat","memory.events.oom","memory.events.oom_kill","memory.peak","memory.swap.peak","pids.peak"],` +
	`"memory":{"peakBytes":0,"swapPeakBytes":0,"oomEvents":0,"oomKillEvents":0},"cpu":{"periods":0,"throttledPeriods":0,"throttledMicroseconds":0},"pids":{"peak":0},"io":{"readBytes":0,"writeBytes":0}}`

func TestParseResourceEvidenceAcceptsExactCgroupV2Shape(t *testing.T) {
	t.Parallel()
	evidence, err := ParseResourceEvidence(strings.NewReader(completeResourceEvidence))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Memory.PeakBytes != 1986422374 || evidence.CPU.ThrottledPeriods != 7 || evidence.usageMicroseconds() != 900000000 || evidence.PIDs.Peak != 88 || evidence.IO.WriteBytes != 5500000000 {
		t.Fatalf("resource evidence = %#v", evidence)
	}
	if missing := evidence.uncapturedFields(); len(missing) != 0 {
		t.Fatalf("complete v2 uncaptured fields = %v", missing)
	}
}

func TestParseResourceEvidenceKeepsLegacySchemaReadable(t *testing.T) {
	t.Parallel()
	evidence, err := ParseResourceEvidence(strings.NewReader(legacyResourceEvidence))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.CPU.UsageMicroseconds != nil || evidence.CPU.ThrottledPeriods != 7 {
		t.Fatalf("legacy resource evidence = %#v", evidence)
	}
	// v1 never captured usage: telemetry must see it as uncaptured, not zero.
	if missing := evidence.uncapturedFields(); len(missing) != 1 || missing[0] != resourceEvidenceUsageField {
		t.Fatalf("legacy uncaptured fields = %v", missing)
	}
	// A re-persisted v1 record must stay byte-compatible with v1 readers.
	content, err := marshalResourceEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "usageMicroseconds") {
		t.Fatalf("re-persisted legacy evidence gained usageMicroseconds: %s", content)
	}
	if _, err := ParseResourceEvidence(strings.NewReader(legacyUnavailableResourceEvidence)); err != nil {
		t.Fatalf("legacy nine-field unavailable fallback rejected: %v", err)
	}
}

func TestParseResourceEvidenceRejectsUnknownAndInconsistentData(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"unknown field":      strings.Replace(completeResourceEvidence, `"missing": []`, `"unknown": true, "missing": []`, 1),
		"complete missing":   strings.Replace(completeResourceEvidence, `"missing": []`, `"missing": ["memory.peak"]`, 1),
		"unsupported source": strings.Replace(completeResourceEvidence, `"source": "cgroup-v2"`, `"source": "workflow"`, 1),
		"trailing value":     completeResourceEvidence + `{}`,
		"v2 without usage":   strings.Replace(completeResourceEvidence, `, "usageMicroseconds": 900000000`, ``, 1),
		"v1 with usage":      strings.Replace(completeResourceEvidence, `"schemaVersion": 2`, `"schemaVersion": 1`, 1),
		"v1 usage missing": strings.Replace(legacyResourceEvidence, `"status": "complete",
  "missing": []`, `"status": "partial",
  "missing": ["cpu.stat.usage_usec"]`, 1),
		"v2 nine-field unavailable": strings.Replace(strings.Replace(legacyUnavailableResourceEvidence, `"schemaVersion":1`, `"schemaVersion":2`, 1),
			`"throttledMicroseconds":0}`, `"throttledMicroseconds":0,"usageMicroseconds":0}`, 1),
		"unsupported version": strings.Replace(completeResourceEvidence, `"schemaVersion": 2`, `"schemaVersion": 3`, 1),
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
