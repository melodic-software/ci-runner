package docker

import (
	"bytes"
	"io"
	"time"
)

const (
	resourceEvidenceLogMarker          = "ci-runner-resource-evidence-v1:"
	maximumResourceEvidenceLogLineSize = maximumResourceEvidenceBytes + len(resourceEvidenceLogMarker) + 64
)

// resourceEvidenceLogObserver forwards the Docker log stream unchanged while
// retaining only one bounded line at a time. Workflow code can emit the same
// marker, so this is capacity telemetry transport rather than an attestation.
// The official completion hook's last schema-valid marker wins.
type resourceEvidenceLogObserver struct {
	destination io.Writer
	line        []byte
	discardLine bool
	evidence    *ResourceEvidence
}

func newResourceEvidenceLogObserver(destination io.Writer) *resourceEvidenceLogObserver {
	return &resourceEvidenceLogObserver{destination: destination}
}

func (w *resourceEvidenceLogObserver) Write(content []byte) (int, error) {
	written, err := w.destination.Write(content)
	w.observe(content[:written])
	return written, err
}

func (w *resourceEvidenceLogObserver) observe(content []byte) {
	for len(content) > 0 {
		newline := bytes.IndexByte(content, '\n')
		if newline < 0 {
			w.appendLine(content)
			return
		}
		w.appendLine(content[:newline])
		w.completeLine()
		content = content[newline+1:]
	}
}

func (w *resourceEvidenceLogObserver) appendLine(content []byte) {
	if w.discardLine {
		return
	}
	if len(w.line)+len(content) > maximumResourceEvidenceLogLineSize {
		w.line = nil
		w.discardLine = true
		return
	}
	w.line = append(w.line, content...)
}

func (w *resourceEvidenceLogObserver) completeLine() {
	if !w.discardLine {
		if evidence, ok := parseResourceEvidenceLogLine(w.line); ok {
			copy := evidence
			copy.Missing = append([]string(nil), evidence.Missing...)
			w.evidence = &copy
		}
	}
	w.line = nil
	w.discardLine = false
}

func (w *resourceEvidenceLogObserver) lastEvidence() *ResourceEvidence {
	if w.evidence == nil {
		return nil
	}
	copy := *w.evidence
	copy.Missing = append([]string(nil), w.evidence.Missing...)
	return &copy
}

func parseResourceEvidenceLogLine(line []byte) (ResourceEvidence, bool) {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if separator := bytes.IndexByte(line, ' '); separator > 0 {
		if _, err := time.Parse(time.RFC3339Nano, string(line[:separator])); err == nil {
			line = line[separator+1:]
		}
	}
	if !bytes.HasPrefix(line, []byte(resourceEvidenceLogMarker)) {
		return ResourceEvidence{}, false
	}
	payload := line[len(resourceEvidenceLogMarker):]
	evidence, err := ParseResourceEvidence(bytes.NewReader(payload))
	return evidence, err == nil
}
