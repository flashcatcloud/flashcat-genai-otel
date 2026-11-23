package flashcatgenai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestShutdownOTLPAbandonsLongRetryAfter(t *testing.T) {
	for _, status := range []int{429, 502, 503, 504} {
		for _, signal := range []string{"traces", "metrics"} {
			t.Run(fmt.Sprintf("%s/%d", signal, status), func(t *testing.T) {
				lifecycle := newExportLifecycle()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				lifecycle.beginShutdown(ctx)
				var requests int
				client := &http.Client{Transport: &recordingTransport{base: transportFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					deadline, ok := req.Context().Deadline()
					want, _ := ctx.Deadline()
					if !ok || deadline.After(want) {
						t.Errorf("HTTP deadline=%v, shutdown deadline=%v", deadline, want)
					}
					return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d unavailable", status),
						Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
				})}}
				var err error
				if signal == "traces" {
					inner, createErr := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL("http://localhost/v1/traces"),
						otlptracehttp.WithHTTPClient(client), otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
					if createErr != nil {
						t.Fatal(createErr)
					}
					defer inner.Shutdown(context.Background())
					exporter := &lifecycleSpanExporter{SpanExporter: inner, lifecycle: lifecycle, timeout: 10 * time.Second}
					err = exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{tracetest.SpanStub{Name: "test"}.Snapshot()})
				} else {
					inner, createErr := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL("http://localhost/v1/metrics"),
						otlpmetrichttp.WithHTTPClient(client), otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}))
					if createErr != nil {
						t.Fatal(createErr)
					}
					defer inner.Shutdown(context.Background())
					exporter := &lifecycleMetricExporter{Exporter: inner, lifecycle: lifecycle, timeout: 10 * time.Second}
					err = exporter.Export(context.Background(), &metricdata.ResourceMetrics{})
				}
				if err == nil || requests != 1 {
					t.Fatalf("requests=%d err=%v", requests, err)
				}
			})
		}
	}
}

func TestExportRetriesBeforeShutdown(t *testing.T) {
	lifecycle := newExportLifecycle()
	requests := 0
	err := lifecycle.export(context.Background(), time.Second, time.Millisecond, time.Millisecond, func(ctx context.Context) error {
		requests++
		if requests < 4 {
			ctx.Value(callRecordKey{}).(*callRecord).status.Store(503)
			return errors.New("unavailable")
		}
		return nil
	})
	if err != nil || requests != 4 {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
}

func TestExportNormalRetryDeadline(t *testing.T) {
	t.Parallel()
	lifecycle := newExportLifecycle()
	requests := 0
	started := time.Now()
	err := lifecycle.export(context.Background(), 50*time.Millisecond, time.Minute, time.Minute, func(ctx context.Context) error {
		requests++
		ctx.Value(callRecordKey{}).(*callRecord).status.Store(503)
		return errors.New("unavailable")
	})
	if !errors.Is(err, context.DeadlineExceeded) || requests != 1 || time.Since(started) > time.Second {
		t.Fatalf("requests=%d elapsed=%s err=%v", requests, time.Since(started), err)
	}
}

func TestFlushRetryLimitIsTemporary(t *testing.T) {
	t.Parallel()
	lifecycle := newExportLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release := lifecycle.limit(ctx)
	if err := lifecycle.export(context.Background(), time.Second, time.Millisecond, time.Millisecond, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	release()
	requests := 0
	err := lifecycle.export(context.Background(), time.Second, time.Millisecond, time.Millisecond, func(ctx context.Context) error {
		requests++
		if requests < 4 {
			ctx.Value(callRecordKey{}).(*callRecord).status.Store(503)
			return errors.New("unavailable")
		}
		return nil
	})
	if err != nil || requests != 4 || lifecycle.bounded() {
		t.Fatalf("requests=%d bounded=%v err=%v", requests, lifecycle.bounded(), err)
	}
}

func TestBoundedExportCancellationDuringBackoff(t *testing.T) {
	t.Parallel()
	lifecycle := newExportLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lifecycle.beginShutdown(ctx)
	requested := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- lifecycle.export(context.Background(), time.Minute, time.Minute, time.Minute, func(ctx context.Context) error {
			close(requested)
			ctx.Value(callRecordKey{}).(*callRecord).status.Store(503)
			return errors.New("unavailable")
		})
	}()
	<-requested
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry backoff ignored cancellation")
	}
}

func TestBoundedOTLPRetries(t *testing.T) {
	for _, operation := range []string{"flush", "shutdown"} {
		for _, signal := range []string{"traces", "metrics"} {
			for _, status := range []int{429, 502, 503, 504, 0} {
				for _, failures := range []int{1, 2, 3} {
					t.Run(fmt.Sprintf("%s/%s/%d/failures=%d", operation, signal, status, failures), func(t *testing.T) {
						t.Parallel()
						lifecycle := newExportLifecycle()
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if operation == "shutdown" {
							lifecycle.beginShutdown(ctx)
						} else {
							defer lifecycle.limit(ctx)()
						}
						var attempts []time.Time
						client := &http.Client{Transport: &recordingTransport{base: transportFunc(func(req *http.Request) (*http.Response, error) {
							attempts = append(attempts, time.Now())
							deadline, ok := req.Context().Deadline()
							want, _ := ctx.Deadline()
							if !ok || !deadline.Equal(want) {
								t.Errorf("HTTP deadline=%v, bounded deadline=%v", deadline, want)
							}
							code := http.StatusOK
							if len(attempts) <= failures {
								if status == 0 {
									return nil, io.EOF
								}
								code = status
							}
							return &http.Response{StatusCode: code, Status: fmt.Sprintf("%d", code), Header: http.Header{},
								Body: io.NopCloser(strings.NewReader(""))}, nil
						})}}
						var err error
						if signal == "traces" {
							inner, createErr := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL("http://localhost/v1/traces"),
								otlptracehttp.WithHTTPClient(client), otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: false}))
							if createErr != nil {
								t.Fatal(createErr)
							}
							defer inner.Shutdown(context.Background())
							exporter := &lifecycleSpanExporter{SpanExporter: inner, lifecycle: lifecycle, timeout: 10 * time.Second}
							err = exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{tracetest.SpanStub{Name: "test"}.Snapshot()})
						} else {
							inner, createErr := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL("http://localhost/v1/metrics"),
								otlpmetrichttp.WithHTTPClient(client), otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false}))
							if createErr != nil {
								t.Fatal(createErr)
							}
							defer inner.Shutdown(context.Background())
							exporter := &lifecycleMetricExporter{Exporter: inner, lifecycle: lifecycle, timeout: 10 * time.Second}
							err = exporter.Export(context.Background(), &metricdata.ResourceMetrics{})
						}
						if len(attempts) != min(failures+1, 3) || (err != nil) != (failures == 3) {
							t.Fatalf("requests=%d err=%v", len(attempts), err)
						}
						for i := 1; i < len(attempts); i++ {
							want := (200 * time.Millisecond) << (i - 1)
							if got := attempts[i].Sub(attempts[i-1]); got < want || got > want+500*time.Millisecond {
								t.Errorf("backoff %d=%s, want %s (within 500ms scheduling allowance)", i, got, want)
							}
						}
					})
				}
			}
		}
	}
}

func TestBoundedExportInsufficientTime(t *testing.T) {
	for _, operation := range []string{"flush", "shutdown"} {
		for _, tc := range []struct {
			name       string
			budget     time.Duration
			request    time.Duration
			retryAfter time.Duration
		}{
			{name: "backoff", budget: 100 * time.Millisecond},
			{name: "backoff_and_request", budget: 500 * time.Millisecond, request: 180 * time.Millisecond},
			{name: "retry_after", budget: time.Second, retryAfter: time.Minute},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				lifecycle := newExportLifecycle()
				ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
				defer cancel()
				if operation == "shutdown" {
					lifecycle.beginShutdown(ctx)
				} else {
					defer lifecycle.limit(ctx)()
				}
				requests := 0
				started := time.Now()
				err := lifecycle.export(context.Background(), time.Minute, time.Minute, time.Minute, func(ctx context.Context) error {
					requests++
					time.Sleep(tc.request)
					rec := ctx.Value(callRecordKey{}).(*callRecord)
					rec.status.Store(503)
					rec.retryAfter.Store(int64(tc.retryAfter))
					return errors.New("unavailable")
				})
				if !errors.Is(err, errExitDeadline) || requests != 1 || time.Since(started) > tc.request+150*time.Millisecond {
					t.Fatalf("requests=%d elapsed=%s err=%v", requests, time.Since(started), err)
				}
			})
		}
	}
}

func TestBoundedExportHonorsRetryAfter(t *testing.T) {
	t.Parallel()
	lifecycle := newExportLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lifecycle.beginShutdown(ctx)
	var attempts []time.Time
	err := lifecycle.export(context.Background(), time.Second, time.Minute, time.Minute, func(ctx context.Context) error {
		attempts = append(attempts, time.Now())
		if len(attempts) == 1 {
			rec := ctx.Value(callRecordKey{}).(*callRecord)
			rec.status.Store(429)
			rec.retryAfter.Store(int64(300 * time.Millisecond))
			return errors.New("rate limited")
		}
		return nil
	})
	if err != nil || len(attempts) != 2 || attempts[1].Sub(attempts[0]) < 300*time.Millisecond {
		t.Fatalf("attempts=%v err=%v", attempts, err)
	}
}

func TestBoundedExportDoesNotRetryPermanentErrors(t *testing.T) {
	for _, status := range []int{0, 400, 401, 403, 413, 500, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			lifecycle := newExportLifecycle()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			lifecycle.beginShutdown(ctx)
			requests := 0
			want := errors.New("permanent failure")
			err := lifecycle.export(context.Background(), time.Minute, time.Millisecond, time.Millisecond, func(ctx context.Context) error {
				requests++
				ctx.Value(callRecordKey{}).(*callRecord).status.Store(int64(status))
				return want
			})
			if !errors.Is(err, want) || requests != 1 {
				t.Fatalf("requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestBoundedExportRequestTimeout(t *testing.T) {
	t.Parallel()
	lifecycle := newExportLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lifecycle.beginShutdown(ctx)
	requests := 0
	err := lifecycle.export(context.Background(), 50*time.Millisecond, time.Minute, time.Minute, func(ctx context.Context) error {
		requests++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 50*time.Millisecond {
			t.Errorf("request timeout exceeds export timeout: %v", deadline)
		}
		if requests == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	if err != nil || requests != 2 {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
}

func TestRetryableExportNetworkErrors(t *testing.T) {
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE,
		&net.DNSError{IsTemporary: true}, context.DeadlineExceeded} {
		wrapped := &url.Error{Op: "Post", URL: "http://localhost/v1/traces", Err: fmt.Errorf("wrapped: %w", err)}
		if !retryableExport(wrapped, 0) {
			t.Errorf("not retryable: %v", wrapped)
		}
		if retryableExport(wrapped, 401) {
			t.Errorf("HTTP 401 must not retry: %v", wrapped)
		}
	}
	for _, err := range []error{context.Canceled, errors.New("bad payload"), &net.DNSError{IsNotFound: true}} {
		if retryableExport(err, 0) {
			t.Errorf("unexpected retry: %v", err)
		}
	}
}

func TestBoundedInterruptsRetryBackoff(t *testing.T) {
	for _, operation := range []string{"flush", "shutdown"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			lifecycle := newExportLifecycle()
			requested := make(chan struct{})
			done := make(chan error, 1)
			var requests atomic.Int64
			var retryDeadlines []time.Time
			go func() {
				done <- lifecycle.export(context.Background(), time.Minute, time.Minute, time.Minute, func(ctx context.Context) error {
					if requests.Add(1) == 1 {
						close(requested)
					} else {
						deadline, _ := ctx.Deadline()
						retryDeadlines = append(retryDeadlines, deadline)
					}
					ctx.Value(callRecordKey{}).(*callRecord).status.Store(503)
					return errors.New("unavailable")
				})
			}()
			<-requested
			// Give the export time to enter its longer backoff during normal operation.
			time.Sleep(20 * time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if operation == "shutdown" {
				lifecycle.beginShutdown(ctx)
			} else {
				defer lifecycle.limit(ctx)()
			}
			started := time.Now()
			select {
			case err := <-done:
				if err == nil || requests.Load() != 3 || time.Since(started) < 600*time.Millisecond {
					t.Fatalf("requests=%d elapsed=%s err=%v", requests.Load(), time.Since(started), err)
				}
				want, _ := ctx.Deadline()
				for _, got := range retryDeadlines {
					if !got.Equal(want) {
						t.Errorf("retry deadline=%v, want %v", got, want)
					}
				}
			case <-time.After(1500 * time.Millisecond):
				t.Fatal("bounded export did not interrupt retry backoff")
			}
		})
	}
}

func TestShutdownBoundsInFlightExport(t *testing.T) {
	lifecycle := newExportLifecycle()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- lifecycle.export(context.Background(), time.Minute, time.Second, time.Second, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	lifecycle.beginShutdown(ctx)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("in-flight export was not canceled")
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight export outlived shutdown deadline")
	}
	// The batch processor must stop sending requests even when it drains with context.Background.
	err := lifecycle.export(context.Background(), time.Minute, time.Second, time.Second, func(context.Context) error {
		t.Error("export started after shutdown deadline")
		return nil
	})
	if err == nil {
		t.Fatal("expected shutdown deadline error")
	}
}

// Batches canceled at the shutdown deadline count toward dropped_timeout by span count and are excluded from network errors.
func TestShutdownDeadlineCountsDroppedTimeout(t *testing.T) {
	lifecycle := newExportLifecycle()
	stats := &exportStats{}
	started := make(chan struct{})
	exporter := &countingExporter{stats: stats, inner: &lifecycleSpanExporter{lifecycle: lifecycle, timeout: time.Minute, SpanExporter: deadlineSpanExporter{export: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}}}
	spans := tracetest.SpanStubs{{Name: "a"}, {Name: "b"}, {Name: "c"}}.Snapshots()
	done := make(chan error, 1)
	go func() { done <- exporter.ExportSpans(context.Background(), spans) }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	lifecycle.beginShutdown(ctx)
	if err := <-done; err == nil {
		t.Fatal("expected deadline error")
	}
	got := stats.snapshot()
	if got.DroppedTimeout != 3 || got.Network != 0 {
		t.Fatalf("dropped_timeout=%d network=%d, want 3 / 0", got.DroppedTimeout, got.Network)
	}
}

type deadlineSpanExporter struct{ export func(context.Context) error }

func (e deadlineSpanExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	return e.export(ctx)
}
func (e deadlineSpanExporter) Shutdown(context.Context) error { return nil }

type deadlineMetricExporter struct {
	sdkmetric.Exporter
	export func(context.Context) error
}

func (e deadlineMetricExporter) Export(ctx context.Context, _ *metricdata.ResourceMetrics) error {
	return e.export(ctx)
}
func (e deadlineMetricExporter) Temporality(kind sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(kind)
}
func (e deadlineMetricExporter) Aggregation(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(kind)
}
func (e deadlineMetricExporter) ForceFlush(context.Context) error { return nil }
func (e deadlineMetricExporter) Shutdown(context.Context) error   { return nil }

func TestSDKFlushAndShutdownShareDeadline(t *testing.T) {
	for _, operation := range []string{"flush", "shutdown"} {
		t.Run(operation, func(t *testing.T) {
			lifecycle := newExportLifecycle()
			var traceDeadline time.Time
			tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(&lifecycleSpanExporter{lifecycle: lifecycle, timeout: time.Minute, SpanExporter: deadlineSpanExporter{export: func(ctx context.Context) error {
				traceDeadline, _ = ctx.Deadline()
				select {
				case <-time.After(20 * time.Millisecond):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}}}))
			var metricRequests atomic.Int64
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(&lifecycleMetricExporter{lifecycle: lifecycle, timeout: time.Minute, Exporter: deadlineMetricExporter{export: func(ctx context.Context) error {
				if metricRequests.Add(1) > 1 {
					return nil
				}
				deadline, _ := ctx.Deadline()
				if !deadline.Equal(traceDeadline) {
					t.Errorf("traces deadline=%v metrics deadline=%v", traceDeadline, deadline)
				}
				<-ctx.Done()
				return ctx.Err()
			}}}, sdkmetric.WithInterval(time.Hour))))
			defer tp.Shutdown(context.Background())
			defer mp.Shutdown(context.Background())
			_, span := tp.Tracer("test").Start(context.Background(), "test")
			span.End()
			sdk := &SDK{tracerProvider: tp, meterProvider: mp, ownTracer: true, ownMeter: true,
				shutdownTO: time.Minute, lifecycle: lifecycle}
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			started := time.Now()
			var err error
			if operation == "shutdown" {
				err = sdk.Shutdown(ctx)
			} else {
				err = sdk.Flush(ctx)
			}
			if !errors.Is(err, context.DeadlineExceeded) || metricRequests.Load() != 1 || time.Since(started) > time.Second {
				t.Fatalf("elapsed=%s metrics=%d err=%v", time.Since(started), metricRequests.Load(), err)
			}
		})
	}
}
