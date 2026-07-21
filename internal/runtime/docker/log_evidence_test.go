package docker

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestResourceEvidenceLogObserverForwardsChunksAndKeepsLastValidMarker(t *testing.T) {
	t.Parallel()
	first := compactResourceEvidence(t, completeResourceEvidence)
	last := strings.Replace(first, `"peakBytes":1986422374`, `"peakBytes":42`, 1)
	input := strings.Join([]string{
		"ordinary stdout",
		"workflow-prefix " + resourceEvidenceLogMarker + first,
		resourceEvidenceLogMarker + `{`,
		resourceEvidenceLogMarker + strings.Repeat("x", maximumResourceEvidenceLogLineSize+1),
		resourceEvidenceLogMarker + first + `{}`,
		"2026-07-20T23:59:59.123456789Z " + resourceEvidenceLogMarker + first,
		resourceEvidenceLogMarker + last,
	}, "\n") + "\n" + resourceEvidenceLogMarker + first

	var forwarded bytes.Buffer
	observer := newResourceEvidenceLogObserver(&forwarded)
	for offset, chunk := 0, 1; offset < len(input); chunk = chunk%19 + 1 {
		end := min(offset+chunk, len(input))
		written, err := observer.Write([]byte(input[offset:end]))
		if err != nil || written != end-offset {
			t.Fatalf("write = %d, %v", written, err)
		}
		offset = end
	}
	if forwarded.String() != input {
		t.Fatal("observer changed forwarded worker logs")
	}
	evidence := observer.lastEvidence()
	if evidence == nil || evidence.Memory.PeakBytes != 42 {
		t.Fatalf("last valid evidence = %#v", evidence)
	}
	if len(observer.line) > maximumResourceEvidenceLogLineSize {
		t.Fatalf("observer retained %d bytes", len(observer.line))
	}
}

func TestParseResourceEvidenceLogLineAcceptsOnlyCompleteReservedLines(t *testing.T) {
	t.Parallel()
	payload := compactResourceEvidence(t, completeResourceEvidence)
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "plain", line: resourceEvidenceLogMarker + payload, want: true},
		{name: "timestamped CRLF", line: "2026-07-20T23:59:59Z " + resourceEvidenceLogMarker + payload + "\r", want: true},
		{name: "spoofed prefix", line: "workflow " + resourceEvidenceLogMarker + payload},
		{name: "invalid timestamp prefix", line: "not-a-time " + resourceEvidenceLogMarker + payload},
		{name: "malformed", line: resourceEvidenceLogMarker + `{`},
		{name: "trailing value", line: resourceEvidenceLogMarker + payload + `{}`},
		{name: "oversized", line: resourceEvidenceLogMarker + strings.Repeat("x", maximumResourceEvidenceBytes+1)},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, got := parseResourceEvidenceLogLine([]byte(test.line))
			if got != test.want {
				t.Fatalf("accepted = %t, want %t", got, test.want)
			}
		})
	}
}

func compactResourceEvidence(t *testing.T, source string) string {
	t.Helper()
	var destination bytes.Buffer
	if err := json.Compact(&destination, []byte(source)); err != nil {
		t.Fatal(err)
	}
	return destination.String()
}
