package telemetry

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestExportDegradedSinkLogsUnreachableOnce(t *testing.T) {
	t.Parallel()
	var notices []ExportNotice
	sink := newExportDegradedSink(func(notice ExportNotice) {
		notices = append(notices, notice)
	}, time.Minute, true, true)

	exportErr := errors.New(`traces export: exporter export timeout: rpc error: code = Unavailable desc = connection refused`)
	for range 5 {
		sink.handle(exportErr)
	}

	if len(notices) != 1 {
		t.Fatalf("notices = %#v, want one unreachable notice", notices)
	}
	if notices[0].Kind != ExportNoticeUnreachable || notices[0].Err != exportErr {
		t.Fatalf("first notice = %#v", notices[0])
	}
}

func TestExportDegradedSinkEmitsSummaryAfterInterval(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	var notices []ExportNotice
	var mu sync.Mutex
	sink := newExportDegradedSink(func(notice ExportNotice) {
		mu.Lock()
		notices = append(notices, notice)
		mu.Unlock()
	}, time.Minute, false, true)
	sink.now = func() time.Time { return now }

	exportErr := errors.New("metrics export: exporter export timeout: dial tcp 127.0.0.1:4317: connect: connection refused")
	sink.handle(exportErr)
	sink.handle(exportErr)
	sink.handle(exportErr)

	now = now.Add(time.Minute)
	sink.handle(exportErr)

	mu.Lock()
	defer mu.Unlock()
	if len(notices) != 2 {
		t.Fatalf("notices = %#v, want unreachable + summary", notices)
	}
	if notices[1].Kind != ExportNoticeDegradedSummary || notices[1].Suppressed != 3 {
		t.Fatalf("summary notice = %#v", notices[1])
	}
}

func TestExportDegradedSinkLogsRestoredAfterSuccess(t *testing.T) {
	t.Parallel()
	var notices []ExportNotice
	sink := newExportDegradedSink(func(notice ExportNotice) {
		notices = append(notices, notice)
	}, time.Minute, true, false)

	exportErr := errors.New("traces export: exporter export timeout")
	sink.handle(exportErr)
	sink.recordSuccess(exportSignalTraces)

	if len(notices) != 2 {
		t.Fatalf("notices = %#v, want unreachable + restored", notices)
	}
	if notices[1].Kind != ExportNoticeRestored {
		t.Fatalf("restored notice = %#v", notices[1])
	}

	sink.handle(exportErr)
	if len(notices) != 3 || notices[2].Kind != ExportNoticeUnreachable {
		t.Fatalf("notices after second outage = %#v", notices)
	}
}

func TestExportDegradedSinkRestoresOnlyAfterAllSignalsRecover(t *testing.T) {
	t.Parallel()
	var notices []ExportNotice
	sink := newExportDegradedSink(func(notice ExportNotice) {
		notices = append(notices, notice)
	}, time.Minute, true, true)

	traceErr := errors.New("traces export: exporter export timeout")
	metricErr := errors.New("metrics export: exporter export timeout")
	sink.handle(traceErr)
	sink.handle(metricErr)
	sink.recordSuccess(exportSignalTraces)

	for _, notice := range notices {
		if notice.Kind == ExportNoticeRestored {
			t.Fatalf("restored emitted while metrics still failing: notices = %#v", notices)
		}
	}

	sink.recordSuccess(exportSignalMetrics)
	if len(notices) != 2 || notices[0].Kind != ExportNoticeUnreachable || notices[1].Kind != ExportNoticeRestored {
		t.Fatalf("notices = %#v, want unreachable then restored after both signals recover", notices)
	}
}

func TestExportDegradedSinkPassesThroughNonExportErrors(t *testing.T) {
	t.Parallel()
	var notices []ExportNotice
	sink := newExportDegradedSink(func(notice ExportNotice) {
		notices = append(notices, notice)
	}, time.Minute, true, true)

	otherErr := errors.New("duplicate metric instrument registration")
	sink.handle(otherErr)
	sink.handle(otherErr)

	if len(notices) != 2 || notices[0].Kind != ExportNoticeOther || notices[1].Kind != ExportNoticeOther {
		t.Fatalf("notices = %#v", notices)
	}
}

func TestExportDegradedSinkStartupProbeDoesNotDuplicateUnreachableNotice(t *testing.T) {
	t.Parallel()
	var notices []ExportNotice
	sink := newExportDegradedSink(func(notice ExportNotice) {
		notices = append(notices, notice)
	}, time.Minute, true, true)

	probeErr := errors.New("OTLP endpoint probe failed: dial tcp 127.0.0.1:4317: connect: connection refused")
	sink.markUnreachable(probeErr)
	sink.handle(errors.New("traces export: exporter export timeout"))

	if len(notices) != 1 || notices[0].Kind != ExportNoticeUnreachable {
		t.Fatalf("notices = %#v, want single startup unreachable notice", notices)
	}
}

func TestEndpointHostPort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		endpoint string
		protocol string
		host     string
		port     string
	}{
		{endpoint: "http://127.0.0.1:4317", protocol: "grpc", host: "127.0.0.1", port: "4317"},
		{endpoint: "http://collector:4318/base", protocol: "http/protobuf", host: "collector", port: "4318"},
		{endpoint: "", protocol: "grpc", host: "127.0.0.1", port: "4317"},
		{endpoint: "", protocol: "http/protobuf", host: "127.0.0.1", port: "4318"},
		{endpoint: "https://collector", protocol: "http/protobuf", host: "collector", port: "443"},
		{endpoint: "http://collector", protocol: "http/protobuf", host: "collector", port: "80"},
		{endpoint: "collector", protocol: "http/protobuf", host: "collector", port: "4318"},
		{endpoint: "collector:4317", protocol: "grpc", host: "collector", port: "4317"},
	}
	for _, test := range tests {
		host, port, err := endpointHostPort(test.endpoint, test.protocol)
		if err != nil {
			t.Fatalf("endpointHostPort(%q, %q) = %v", test.endpoint, test.protocol, err)
		}
		if host != test.host || port != test.port {
			t.Fatalf("endpointHostPort(%q, %q) = %q:%q, want %q:%q", test.endpoint, test.protocol, host, port, test.host, test.port)
		}
	}
}

func TestIsExportError(t *testing.T) {
	t.Parallel()
	if !isExportError(errors.New("traces export: exporter export timeout")) {
		t.Fatal("expected export error classification")
	}
	if isExportError(errors.New("duplicate metric instrument registration")) {
		t.Fatal("did not expect export error classification")
	}
}

func TestExportErrorSignals(t *testing.T) {
	t.Parallel()
	enabled := map[exportSignal]struct{}{
		exportSignalTraces:  {},
		exportSignalMetrics: {},
	}
	traceSignals := exportErrorSignals(errors.New("traces export: timeout"), enabled)
	if len(traceSignals) != 1 || traceSignals[0] != exportSignalTraces {
		t.Fatalf("trace signals = %#v", traceSignals)
	}
	metricSignals := exportErrorSignals(errors.New("metrics export: timeout"), enabled)
	if len(metricSignals) != 1 || metricSignals[0] != exportSignalMetrics {
		t.Fatalf("metric signals = %#v", metricSignals)
	}
}
