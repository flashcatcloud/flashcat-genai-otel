package flashcatgenai

import (
	"os"
	"strings"
	"testing"
	"time"
)

// clearEnv uses t.Setenv to clear environment variables that affect configuration and restore them after the test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "OTEL_") || strings.HasPrefix(k, "FLASHCAT_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
}

func mustResolve(t *testing.T, opts ...Option) *settings {
	t.Helper()
	s, err := resolve(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConfigPriority(t *testing.T) {
	clearEnv(t)
	// Default values.
	s := mustResolve(t)
	if s.serviceName != "" || s.environment != "prod" || s.endpoint != "" || s.content != ContentOff || s.exporter != ExporterAuto {
		t.Fatalf("defaults: %+v", s)
	}
	// OTEL_*
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=res-svc,deployment.environment=staging+1")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otel.example.com/")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Basic%20ab+c/d==,x=y")
	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "SPAN_ONLY")
	s = mustResolve(t)
	if s.serviceName != "res-svc" || s.environment != "staging+1" || s.endpoint != "https://otel.example.com/v1/traces" ||
		s.authHeader != "Basic ab+c/d==" || s.content != ContentSpan || s.metricsEndpoint != "https://otel.example.com/v1/metrics" {
		t.Fatalf("otel env: %+v", s)
	}
	t.Setenv("OTEL_SERVICE_NAME", "otel-svc")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://traces.example.com/v1/traces")
	s = mustResolve(t)
	if s.serviceName != "otel-svc" || s.endpoint != "https://traces.example.com/v1/traces" {
		t.Fatalf("otel specific env: %+v", s)
	}
	// FLASHCAT_* takes precedence over OTEL_*.
	t.Setenv("FLASHCAT_SERVICE_NAME", "fc-svc")
	t.Setenv("FLASHCAT_ENVIRONMENT", "test")
	t.Setenv("FLASHCAT_ENDPOINT", "https://fc.example.com/v1/traces")
	t.Setenv("FLASHCAT_API_KEY", "id:secret")
	t.Setenv("FLASHCAT_CAPTURE_CONTENT", "off")
	t.Setenv("FLASHCAT_METRICS_ENDPOINT", "https://fc.example.com/m")
	s = mustResolve(t)
	if s.serviceName != "fc-svc" || s.environment != "test" || s.endpoint != "https://fc.example.com/v1/traces" ||
		s.apiKey != "id:secret" || s.content != ContentOff || s.metricsEndpoint != "https://fc.example.com/m" {
		t.Fatalf("flashcat env: %+v", s)
	}
	if authHeader(s) != "Basic aWQ6c2VjcmV0" {
		t.Fatalf("auth: %s", authHeader(s))
	}
	// Explicit options take precedence.
	s = mustResolve(t, WithServiceName("arg-svc"), WithEnvironment("dev"), WithEndpoint("https://arg.example.com/v1/traces"),
		WithAPIKey("a:b"), WithCaptureContent(ContentSpan))
	if s.serviceName != "arg-svc" || s.environment != "dev" || s.endpoint != "https://arg.example.com/v1/traces" || s.apiKey != "a:b" || s.content != ContentSpan {
		t.Fatalf("explicit: %+v", s)
	}
	// Explicit ContentOff overrides span in the environment.
	t.Setenv("FLASHCAT_CAPTURE_CONTENT", "span")
	if s = mustResolve(t, WithCaptureContent(ContentOff)); s.content != ContentOff {
		t.Fatal("explicit off must win over env")
	}
}

func TestConfigValidation(t *testing.T) {
	clearEnv(t)
	cases := []struct {
		ep      string
		problem string
	}{
		{"ingest.example.com", "完整 URL"},
		{"ftp://ingest.example.com/v1/traces", "完整 URL"},
		{"https://ingest.example.com", "缺少路径"},
		{"https://ingest.example.com/", "缺少路径"},
	}
	for _, c := range cases {
		s := mustResolve(t, WithEndpoint(c.ep))
		if s.endpoint != "" || len(s.problems) != 1 || !strings.Contains(s.problems[0], c.problem) {
			t.Fatalf("%s: endpoint=%q problems=%v", c.ep, s.endpoint, s.problems)
		}
	}
	s := mustResolve(t, WithAPIKey("base64only"))
	if s.apiKey != "" || len(s.problems) != 1 || !strings.Contains(s.problems[0], "keyID:secret") {
		t.Fatalf("api key: %+v", s)
	}
	// Leave the port unset when the endpoint omits it.
	s = mustResolve(t, WithEndpoint("http://127.0.0.1/v1/traces"))
	if s.endpoint != "http://127.0.0.1/v1/traces" || s.knownHosts[len(s.knownHosts)-1] != "127.0.0.1" {
		t.Fatalf("endpoint: %+v", s)
	}
}

func TestCaptureContentParsing(t *testing.T) {
	clearEnv(t)
	for _, bad := range []ContentMode{"true", "yes", "1", "", "SPANS"} {
		if _, err := resolve([]Option{WithCaptureContent(bad)}); err == nil {
			t.Fatalf("explicit %q should return error", bad)
		}
	}
	for in, want := range map[ContentMode]ContentMode{"off": ContentOff, "no_content": ContentOff, "FALSE": ContentOff, "span_only": ContentSpan, "Span": ContentSpan} {
		if s := mustResolve(t, WithCaptureContent(in)); s.content != want || len(s.problems) != 0 {
			t.Fatalf("%q → %q problems=%v", in, s.content, s.problems)
		}
	}
	// Event modes disable capture and record a diagnostics problem without returning an error.
	s := mustResolve(t, WithCaptureContent("span_and_event"))
	if s.content != ContentOff || len(s.problems) != 1 || !strings.Contains(s.problems[0], "不支持") {
		t.Fatalf("event: %+v", s)
	}
	// Invalid environment values record diagnostics problems without returning an error.
	t.Setenv("FLASHCAT_CAPTURE_CONTENT", "yes")
	s = mustResolve(t)
	if s.content != ContentOff || len(s.problems) != 1 {
		t.Fatalf("env invalid: %+v", s)
	}
	os.Unsetenv("FLASHCAT_CAPTURE_CONTENT")
	// Go instrumentation expects a boolean in the OTEL_* variable.
	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "true")
	if s = mustResolve(t); s.content != ContentSpan {
		t.Fatalf("OTEL true: %+v", s)
	}
	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "EVENT_ONLY")
	if s = mustResolve(t); s.content != ContentOff || len(s.problems) != 1 {
		t.Fatalf("OTEL event: %+v", s)
	}
}

func TestExplicitOptionErrors(t *testing.T) {
	clearEnv(t)
	if _, err := resolve([]Option{WithExporter("bogus")}); err == nil {
		t.Fatal("bad exporter should error")
	}
	if _, err := resolve([]Option{WithExportTimeout(0)}); err == nil {
		t.Fatal("zero timeout should error")
	}
	if _, err := resolve([]Option{WithShutdownTimeout(-1)}); err == nil {
		t.Fatal("negative timeout should error")
	}
}

func TestShutdownTimeoutEnv(t *testing.T) {
	clearEnv(t)
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		problem bool
	}{
		{"", 5 * time.Second, false},
		{"2", 2 * time.Second, false},
		{" 0.125 ", 125 * time.Millisecond, false},
		{"1e-3", time.Millisecond, false},
		{"bad", 5 * time.Second, true},
		{"0", 5 * time.Second, true},
		{"-2", 5 * time.Second, true},
		{"NaN", 5 * time.Second, true},
		{"Inf", 5 * time.Second, true},
		{"1e100", 5 * time.Second, true},
		{"1e-100", 5 * time.Second, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("FLASHCAT_SHUTDOWN_TIMEOUT", tc.raw)
			s := mustResolve(t)
			if s.shutdownTimeout != tc.want || (len(s.problems) != 0) != tc.problem {
				t.Fatalf("timeout=%s problems=%v", s.shutdownTimeout, s.problems)
			}
			if tc.problem && !strings.Contains(s.problems[0], "FLASHCAT_SHUTDOWN_TIMEOUT") {
				t.Fatalf("problems=%v", s.problems)
			}
			// An explicit option overrides an invalid environment variable without recording a problem.
			s = mustResolve(t, WithShutdownTimeout(3*time.Second))
			if s.shutdownTimeout != 3*time.Second || len(s.problems) != 0 {
				t.Fatalf("explicit timeout=%s problems=%v", s.shutdownTimeout, s.problems)
			}
		})
	}
}

func TestDisabledAndKnownHosts(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_SDK_DISABLED", "true")
	t.Setenv("FLASHCAT_KNOWN_HOSTS", "Ingest.Flashcat.cloud, other.example.com")
	s := mustResolve(t, WithEndpoint("https://fc.example.com/v1/traces"))
	if !s.disabled || strings.Join(s.knownHosts, ",") != "ingest.flashcat.cloud,other.example.com,fc.example.com" {
		t.Fatalf("%+v", s)
	}
}
