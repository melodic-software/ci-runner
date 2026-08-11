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

type exportDegradedSink struct {
	notify          func(ExportNotice)
	mu              sync.Mutex
	degraded        bool
	suppressed      uint64
	lastErr         error
	lastSummaryAt   time.Time
	summaryInterval time.Duration
	now             func() time.Time
}

func newExportDegradedSink(notify func(ExportNotice), summaryInterval time.Duration) *exportDegradedSink {
	if notify == nil {
		notify = func(ExportNotice) {}
	}
	if summaryInterval <= 0 {
		summaryInterval = defaultExportDegradedSummaryInterval
	}
	return &exportDegradedSink{
		notify:          notify,
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

	if !s.degraded {
		s.degraded = true
		s.lastErr = err
		s.suppressed = 0
		s.lastSummaryAt = s.now()
		s.notify(ExportNotice{Kind: ExportNoticeUnreachable, Err: err})
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

func (s *exportDegradedSink) recordSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.degraded {
		return
	}
	s.degraded = false
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
	if s.degraded {
		return
	}
	s.degraded = true
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
	if !strings.Contains(endpoint, "://") {
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
		switch protocol {
		case "grpc":
			port = "4317"
		default:
			port = "4318"
		}
	}
	return host, port, nil
}

type instrumentedTraceExporter struct {
	inner sdktrace.SpanExporter
	sink  *exportDegradedSink
}

func (e *instrumentedTraceExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.inner.ExportSpans(ctx, spans)
	if err == nil {
		e.sink.recordSuccess()
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
		e.sink.recordSuccess()
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
