package flashcatgenai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// basicAuth encodes keyID:secret for the Basic header. Pass the credentials without base64 or URL encoding.
func basicAuth(key string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(key))
}

func authHeader(s *settings) string {
	if s.apiKey != "" {
		return basicAuth(s.apiKey)
	}
	return s.authHeader
}

// ExportStats holds export counters (contract §3 and the export field in §8). HTTP counters use keys like
// http_400, http_401, http_403, http_413, http_429, http_5xx, and http_<other status code>. MarshalJSON flattens them.
type ExportStats struct {
	OK               int64
	Network          int64
	DroppedQueueFull int64
	DroppedTimeout   int64 // Spans dropped when the shutdown or explicit Flush deadline expires (contract §9).
	HTTP             map[string]int64
	LastError        *string
}

// MarshalJSON emits {"ok":…,"network":…,"dropped_queue_full":…,"dropped_timeout":…,"http_401":…,"last_error":…}.
func (e ExportStats) MarshalJSON() ([]byte, error) {
	m := map[string]any{"ok": e.OK, "network": e.Network, "dropped_queue_full": e.DroppedQueueFull, "dropped_timeout": e.DroppedTimeout, "last_error": e.LastError}
	for k, v := range e.HTTP {
		m[k] = v
	}
	return json.Marshal(m)
}

// exportStats holds thread-safe export counters.
type exportStats struct {
	mu        sync.Mutex
	ok        int64
	network   int64
	dropped   int64
	timeout   int64
	http      map[string]int64
	lastError *string
}

func (s *exportStats) inc(bucket string, n int64, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch bucket {
	case "ok":
		s.ok += n
	case "network":
		s.network += n
	case "dropped_timeout":
		s.timeout += n
	default:
		if s.http == nil {
			s.http = map[string]int64{}
		}
		s.http[bucket] += n
	}
	if errText != "" {
		e := errText
		s.lastError = &e
	}
}

func (s *exportStats) snapshot() ExportStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ExportStats{OK: s.ok, Network: s.network, DroppedQueueFull: s.dropped, DroppedTimeout: s.timeout, HTTP: map[string]int64{}}
	keys := make([]string, 0, len(s.http))
	for k := range s.http {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.HTTP[k] = s.http[k]
	}
	if s.lastError != nil {
		e := *s.lastError
		out.LastError = &e
	}
	return out
}

func statusBucket(code int) string {
	switch {
	case code == 400 || code == 401 || code == 403 || code == 413 || code == 429:
		return fmt.Sprintf("http_%d", code)
	case code >= 500:
		return "http_5xx"
	default:
		return fmt.Sprintf("http_%d", code)
	}
}

// callRecord tracks the HTTP result of one ExportSpans call. The context carries it to the RoundTripper to avoid races.
type callRecord struct {
	status     atomic.Int64 // Zero means no response was received (network error).
	retryAfter atomic.Int64
}

type callRecordKey struct{}

// recordingTransport records the HTTP status of OTLP requests because otlptracehttp returns only an error.
type recordingTransport struct{ base http.RoundTripper }

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if rec, ok := req.Context().Value(callRecordKey{}).(*callRecord); ok {
		if err != nil || resp == nil {
			rec.status.Store(0)
		} else {
			rec.status.Store(int64(resp.StatusCode))
			if raw := resp.Header.Get("Retry-After"); raw != "" {
				if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
					rec.retryAfter.Store(int64(time.Duration(min(seconds, int64(24*60*60))) * time.Second))
				} else if date, err := http.ParseTime(raw); err == nil {
					rec.retryAfter.Store(int64(max(time.Until(date), 0)))
				}
			}
		}
	}
	return resp, err
}

func newHTTPClient(timeout time.Duration) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{Transport: &recordingTransport{base: base}, Timeout: timeout}
}

// countingExporter wraps the OTLP exporter to record counters without retrying.
type countingExporter struct {
	inner sdktrace.SpanExporter
	stats *exportStats
}

func (e *countingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) (err error) {
	rec := &callRecord{}
	defer func() {
		if r := recover(); r != nil { // Keep SDK failures from affecting application code.
			e.stats.inc("network", 1, fmt.Sprintf("panic: %v", r))
			err = fmt.Errorf("flashcat-genai-otel: export panic: %v", r)
		}
	}()
	err = e.inner.ExportSpans(context.WithValue(ctx, callRecordKey{}, rec), spans)
	status := int(rec.status.Load())
	switch {
	case err == nil:
		e.stats.inc("ok", int64(len(spans)), "")
	case status >= 200 && status < 300:
		// A 2xx response with partial_success means the request was accepted. Record the error text only.
		e.stats.inc("ok", int64(len(spans)), err.Error())
	case errors.Is(err, errExitDeadline):
		e.stats.inc("dropped_timeout", int64(len(spans)), err.Error())
	case status == 0:
		e.stats.inc("network", 1, err.Error())
	default:
		e.stats.inc(statusBucket(status), 1, err.Error())
	}
	return err
}

func (e *countingExporter) Shutdown(ctx context.Context) error { return e.inner.Shutdown(ctx) }

type lifecycleSpanExporter struct {
	sdktrace.SpanExporter
	lifecycle *exportLifecycle
	timeout   time.Duration
}

func (e *lifecycleSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.lifecycle.export(ctx, e.timeout, time.Second, 5*time.Second, func(ctx context.Context) error {
		return e.SpanExporter.ExportSpans(ctx, spans)
	})
	// Exports canceled at the shutdown or explicit Flush deadline count as dropped_timeout separately from network errors.
	if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) && e.lifecycle.bounded() {
		return errors.Join(errExitDeadline, err)
	}
	return err
}

type lifecycleMetricExporter struct {
	sdkmetric.Exporter
	lifecycle *exportLifecycle
	timeout   time.Duration
}

func (e *lifecycleMetricExporter) Export(ctx context.Context, data *metricdata.ResourceMetrics) error {
	return e.lifecycle.export(ctx, e.timeout, 5*time.Second, 30*time.Second, func(ctx context.Context) error {
		return e.Exporter.Export(ctx, data)
	})
}

// newSpanExportProcessor builds the Flashcat export pipeline in order: set missing AI root markers, batch, mask, count, and export via OTLP.
func newSpanExportProcessor(ctx context.Context, s *settings) (sdktrace.SpanProcessor, error) {
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(s.endpoint),
		otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
		otlptracehttp.WithTimeout(s.exportTimeout),
		otlptracehttp.WithHTTPClient(newHTTPClient(s.exportTimeout)),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}),
	}
	if a := authHeader(s); a != "" {
		opts = append(opts, otlptracehttp.WithHeaders(map[string]string{"Authorization": a}))
	}
	inner, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	retrying := &lifecycleSpanExporter{SpanExporter: inner, lifecycle: s.lifecycle, timeout: s.exportTimeout}
	counted := &countingExporter{inner: retrying, stats: global.stats}
	masked := &maskingExporter{next: counted, m: s.compiled, content: s.content == ContentSpan}
	return genAIRootProcessor{sdktrace.NewBatchSpanProcessor(masked)}, nil
}

func newMetricExporter(ctx context.Context, s *settings) (sdkmetric.Exporter, error) {
	opts := []otlpmetrichttp.Option{
		otlpmetrichttp.WithEndpointURL(s.metricsEndpoint),
		otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression),
		otlpmetrichttp.WithTimeout(s.exportTimeout),
		otlpmetrichttp.WithHTTPClient(newHTTPClient(s.exportTimeout)),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}),
	}
	if a := authHeader(s); a != "" {
		opts = append(opts, otlpmetrichttp.WithHeaders(map[string]string{"Authorization": a}))
	}
	inner, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &lifecycleMetricExporter{Exporter: inner, lifecycle: s.lifecycle, timeout: s.exportTimeout}, nil
}
