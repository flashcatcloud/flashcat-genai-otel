package flashcatopenai

import (
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures Middleware.
type Option func(*config)

type config struct {
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
	captureContent *bool
}

// WithTracerProvider sets the TracerProvider for new spans. Each request defaults to the current global provider set by flashcatgenai.Init.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) { c.tracerProvider = tp }
}

// WithMeterProvider sets the MeterProvider for gen_ai.client.operation.duration. It defaults to the global provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) { c.meterProvider = mp }
}

// WithCaptureContent overrides the content capture switch. By default, each request calls flashcatgenai.ContentCaptureEnabled(),
// which uses the WithCaptureContent option passed to flashcatgenai.Init or FLASHCAT_CAPTURE_CONTENT.
func WithCaptureContent(enabled bool) Option {
	return func(c *config) { c.captureContent = &enabled }
}
