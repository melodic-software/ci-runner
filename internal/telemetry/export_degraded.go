package telemetry

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const defaultExportDegradedSummaryInterval = 5 * time.Minute

// ExportNoticeKind classifies OTLP export logging events for graceful degradation.
type ExportNoticeKind int

const (
	ExportNoticeUnreachable ExportNoticeKind = iota
	ExportNoticeDegradedSummary
	ExportNoticeRestored
	ExportNoticeOther
)

// ExportNotice is emitted instead of forwarding every export-cycle failure to the
// controller log.
type ExportNotice struct {
	Kind       ExportNoticeKind
	Err        error
	Suppressed uint64
}

type exportSignal int

const (
	exportSignalTraces exportSignal = iota
	exportSignalMetrics
)

type exportDegradedSink struct {
	notify          func(ExportNotice)
	mu              sync.Mutex
	enabled         map[exportSignal]struct{}
	degraded        map[exportSignal]struct{}
	suppressed      uint64
	lastErr         error
	lastSummaryAt   time.Time
	summaryInterval time.Duration
	now             func() time.Time
}

func newExportDegradedSink(notify func(ExportNotice), summaryInterval time.Duration, tracesEnabled, metricsEnabled bool) *exportDegradedSink {
	if notify == nil {
		notify = func(ExportNotice) {}
	}
	if summaryInterval <= 0 {
		summaryInterval = defaultExportDegradedSummaryInterval
	}
	enabled := make(map[exportSignal]struct{})
	if tracesEnabled {
		enabled[exportSignalTraces] = struct{}{}
	}
	if metricsEnabled {
		enabled[exportSignalMetrics] = struct{}{}
	}
	return &exportDegradedSink{
		notify:          notify,
		enabled:         enabled,
		degraded:        make(map[exportSignal]struct{}),
		summaryInterval: summaryInterval,
		now:             time.Now,
	}
}

func (s *exportDegradedSink) handle(err error) {
	if err == nil {
		return
	}
	if !isExportError(err) {
		s.notify(ExportNotice{Kind: ExportNoticeOther, Err: err})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	signals := exportErrorSignals(err, s.enabled)
	if len(signals) == 0 {
		return
	}

	wasDegraded := len(s.degraded) > 0
	newlyDegraded := false
	for _, signal := range signals {
		if _, already := s.degraded[signal]; already {
			continue
		}
		s.degraded[signal] = struct{}{}
		newlyDegraded = true
	}

	if !wasDegraded && newlyDegraded {
		s.lastErr = err
		s.suppressed = 0
		s.lastSummaryAt = s.now()
		s.notify(ExportNotice{Kind: ExportNoticeUnreachable, Err: err})
		return
	}

	if !wasDegraded {
		return
	}

	s.suppressed++
	if s.lastErr == nil {
		s.lastErr = err
	}
	if s.suppressed > 0 && s.now().Sub(s.lastSummaryAt) >= s.summaryInterval {
		s.emitSummaryLocked()
	}
}

func (s *exportDegradedSink) recordSuccess(signal exportSignal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, enabled := s.enabled[signal]; !enabled {
		return
	}
	if _, degraded := s.degraded[signal]; !degraded {
		return
	}
	delete(s.degraded, signal)
	if len(s.degraded) > 0 {
		return
	}
	s.suppressed = 0
	s.lastErr = nil
	s.notify(ExportNotice{Kind: ExportNoticeRestored})
}

func (s *exportDegradedSink) markUnreachable(err error) {
	if err == nil {
		err = errors.New("OTLP endpoint unreachable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.degraded) > 0 {
		return
	}
	for signal := range s.enabled {
		s.degraded[signal] = struct{}{}
	}
	s.lastErr = err
	s.suppressed = 0
	s.lastSummaryAt = s.now()
	s.notify(ExportNotice{Kind: ExportNoticeUnreachable, Err: err})
}

func (s *exportDegradedSink) emitSummaryLocked() {
	if s.suppressed == 0 {
		return
	}
	s.notify(ExportNotice{
		Kind:       ExportNoticeDegradedSummary,
		Err:        s.lastErr,
		Suppressed: s.suppressed,
	})
	s.suppressed = 0
	s.lastSummaryAt = s.now()
}

func exportErrorSignals(err error, enabled map[exportSignal]struct{}) []exportSignal {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "traces export:"):
		if _, ok := enabled[exportSignalTraces]; ok {
			return []exportSignal{exportSignalTraces}
		}
	case strings.Contains(message, "metrics export:"):
		if _, ok := enabled[exportSignalMetrics]; ok {
			return []exportSignal{exportSignalMetrics}
		}
	}
	signals := make([]exportSignal, 0, len(enabled))
	for signal := range enabled {
		signals = append(signals, signal)
	}
	return signals
}

func isExportError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, " export:") ||
		strings.Contains(message, "export timeout") ||
		strings.Contains(message, "failed to export") ||
		strings.Contains(message, "exporter export")
}

func probeEndpointReachable(endpoint, protocol string) error {
	host, port, err := endpointHostPort(endpoint, protocol)
	if err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(context.Background(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	return conn.Close()
}

func endpointHostPort(endpoint, protocol string) (string, string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		switch protocol {
		case "grpc":
			return "127.0.0.1", "4317", nil
		default:
			return "127.0.0.1", "4318", nil
		}
	}
	explicitScheme := strings.Contains(endpoint, "://")
	if !explicitScheme {
		endpoint = "http://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", "", err
	}
	host := parsed.Hostname()
	if host == "" {
		return "", "", errors.New("telemetry endpoint must include a host")
	}
	port := parsed.Port()
	if port == "" {
		if explicitScheme {
			switch parsed.Scheme {
			case "https":
				port = "443"
			case "http":
				port = "80"
			default:
				port = defaultOTLPPort(protocol)
			}
		} else {
			port = defaultOTLPPort(protocol)
		}
	}
	return host, port, nil
}

func defaultOTLPPort(protocol string) string {
	if protocol == "grpc" {
		return "4317"
	}
	return "4318"
}

type instrumentedTraceExporter struct {
	inner sdktrace.SpanExporter
	sink  *exportDegradedSink
}

func (e *instrumentedTraceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.inner.ExportSpans(ctx, spans)
	if err == nil {
		e.sink.recordSuccess(exportSignalTraces)
	}
	return err
}

func (e *instrumentedTraceExporter) Shutdown(ctx context.Context) error {
	return e.inner.Shutdown(ctx)
}

type instrumentedMetricExporter struct {
	inner metric.Exporter
	sink  *exportDegradedSink
}

func (e *instrumentedMetricExporter) Temporality(kind metric.InstrumentKind) metricdata.Temporality {
	return e.inner.Temporality(kind)
}

func (e *instrumentedMetricExporter) Aggregation(kind metric.InstrumentKind) metric.Aggregation {
	return e.inner.Aggregation(kind)
}

func (e *instrumentedMetricExporter) Export(ctx context.Context, metrics *metricdata.ResourceMetrics) error {
	err := e.inner.Export(ctx, metrics)
	if err == nil {
		e.sink.recordSuccess(exportSignalMetrics)
	}
	return err
}

func (e *instrumentedMetricExporter) ForceFlush(ctx context.Context) error {
	if flusher, ok := e.inner.(interface{ ForceFlush(context.Context) error }); ok {
		return flusher.ForceFlush(ctx)
	}
	return nil
}

func (e *instrumentedMetricExporter) Shutdown(ctx context.Context) error {
	return e.inner.Shutdown(ctx)
}
