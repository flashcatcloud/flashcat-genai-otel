package flashcatgenai

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ExporterMode controls how the Flashcat export pipeline connects to the TracerProvider (contract §2.4).
type ExporterMode string

const (
	// ExporterAuto appends the export pipeline in scenarios A and B and reuses the existing exporter in scenario C.
	ExporterAuto ExporterMode = "auto"
	// ExporterAdd always appends the Flashcat export pipeline.
	ExporterAdd ExporterMode = "add"
	// ExporterReuse adds only session processing and sends data through the process's existing exporter.
	ExporterReuse ExporterMode = "reuse"
	// ExporterNone disables export.
	ExporterNone ExporterMode = "none"
)

// ContentMode is the content capture switch (contract §1). Go supports only ContentOff and ContentSpan.
type ContentMode string

const (
	// ContentOff disables content capture by default.
	ContentOff ContentMode = "false"
	// ContentSpan writes content to span attributes.
	ContentSpan ContentMode = "span"
)

// Matches Python's _config._CONTENT_VALUES. Event modes disable content capture in Go and record a diagnostics problem.
var contentValues = map[string]ContentMode{
	"false":      ContentOff,
	"0":          ContentOff,
	"no":         ContentOff,
	"off":        ContentOff,
	"no_content": ContentOff,
	"span":       ContentSpan,
	"span_only":  ContentSpan,
}

var eventContentValues = map[string]bool{"event": true, "event_only": true, "span_and_event": true}

// Option configures Init or NewSpanProcessor. Explicit options take precedence over FLASHCAT_* and OTEL_* environment variables.
type Option func(*options)

type options struct {
	serviceName     string
	environment     string
	endpoint        string
	apiKey          string
	exporter        *ExporterMode
	captureContent  *ContentMode
	metrics         *bool
	masking         *MaskingConfig
	resourceAttrs   map[string]string
	exportTimeout   *time.Duration
	shutdownTimeout *time.Duration
	debug           bool
}

// WithServiceName sets the application name (service.name). If empty, it falls back to FLASHCAT_SERVICE_NAME,
// OTEL_SERVICE_NAME, then service.name in OTEL_RESOURCE_ATTRIBUTES.
func WithServiceName(name string) Option { return func(o *options) { o.serviceName = name } }

// WithEnvironment sets deployment.environment.name, which defaults to prod.
func WithEnvironment(env string) Option { return func(o *options) { o.environment = env } }

// WithEndpoint sets the endpoint. Supply a full URL with a path, such as
// https://ingest.example.com/v1/traces. The SDK leaves an omitted port unset.
func WithEndpoint(endpoint string) Option { return func(o *options) { o.endpoint = endpoint } }

// WithAPIKey sets the credentials in keyID:secret format. The SDK encodes them for Basic authentication.
func WithAPIKey(key string) Option { return func(o *options) { o.apiKey = key } }

// WithExporter sets the export mode to ExporterAuto (default), ExporterAdd, ExporterReuse, or ExporterNone.
// Init returns an error for invalid values.
func WithExporter(mode ExporterMode) Option { return func(o *options) { o.exporter = &mode } }

// WithCaptureContent sets the content capture switch to ContentOff (default) or ContentSpan.
// It also accepts "off", "no_content", and "span_only".
// Event modes disable capture and record a diagnostics problem. Init returns an error for other values, such as "true".
func WithCaptureContent(mode ContentMode) Option {
	return func(o *options) { o.captureContent = &mode }
}

// WithMetrics controls metrics export, which is enabled by default.
func WithMetrics(enabled bool) Option { return func(o *options) { o.metrics = &enabled } }

// WithMasking configures masking and truncation (contract §6). Init returns an error if a custom regex fails to compile.
func WithMasking(cfg MaskingConfig) Option { return func(o *options) { o.masking = &cfg } }

// WithResourceAttributes adds resource attributes when the SDK creates a provider.
func WithResourceAttributes(attrs map[string]string) Option {
	return func(o *options) {
		if o.resourceAttrs == nil {
			o.resourceAttrs = map[string]string{}
		}
		for k, v := range attrs {
			o.resourceAttrs[k] = v
		}
	}
}

// WithExportTimeout sets the timeout for a single export. The default is 10 seconds.
func WithExportTimeout(d time.Duration) Option { return func(o *options) { o.exportTimeout = &d } }

// WithShutdownTimeout caps the total wait for Flush and Shutdown when the caller supplies no deadline.
// It takes precedence over FLASHCAT_SHUTDOWN_TIMEOUT and defaults to 5 seconds.
func WithShutdownTimeout(d time.Duration) Option {
	return func(o *options) { o.shutdownTimeout = &d }
}

// WithDebug makes Init log the full diagnostics in place of the one-line summary.
func WithDebug(enabled bool) Option { return func(o *options) { o.debug = enabled } }

// configError is the error Init returns for invalid explicit options, the sole exception in contract §2.1.
type configError struct{ msg string }

func (e *configError) Error() string { return "flashcat-genai-otel: " + e.msg }

// settings holds the resolved configuration in precedence order (explicit options, FLASHCAT_*, OTEL_*, defaults).
type settings struct {
	serviceName     string
	environment     string
	endpoint        string
	metricsEndpoint string
	apiKey          string
	authHeader      string
	exporter        ExporterMode
	content         ContentMode
	metrics         bool
	masking         MaskingConfig
	compiled        *masker
	resourceAttrs   map[string]string
	exportTimeout   time.Duration
	shutdownTimeout time.Duration
	debug           bool
	disabled        bool
	knownHosts      []string
	problems        []string
	lifecycle       *exportLifecycle
}

func env(name string) string { return strings.TrimSpace(os.Getenv(name)) }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseKVList parses the k=v,k=v format of OTEL_RESOURCE_ATTRIBUTES and OTEL_EXPORTER_OTLP_HEADERS.
// Like the OTel SDK, it percent-decodes values with PathUnescape to preserve "+" in base64.
func parseKVList(raw string) map[string]string {
	out := map[string]string{}
	if raw == "" {
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if dv, err := url.PathUnescape(strings.TrimSpace(v)); err == nil {
			v = dv
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// parseContentExplicit parses the explicit content capture switch and returns an error for invalid values.
func parseContentExplicit(v ContentMode) (ContentMode, string, error) {
	raw := strings.ToLower(strings.TrimSpace(string(v)))
	if m, ok := contentValues[raw]; ok {
		return m, "", nil
	}
	if eventContentValues[raw] {
		return ContentOff, eventProblem(raw), nil
	}
	if raw == "true" {
		return "", "", &configError{msg: `capture_content="true" 有歧义，请用 flashcatgenai.ContentSpan（写进 span 属性）或 ContentOff`}
	}
	return "", "", &configError{msg: fmt.Sprintf("capture_content 只能是 ContentOff / ContentSpan（或 off、no_content、span_only），收到 %q", string(v))}
}

func eventProblem(raw string) string {
	return fmt.Sprintf("capture_content=%q 在 Go 版不支持（正文事件需要 LoggerProvider；只支持 false / span），已按不采正文处理", raw)
}

// contentFromEnv checks FLASHCAT_CAPTURE_CONTENT before OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT.
// Invalid environment values produce a diagnostics problem. Init returns errors only for explicit options (contract §2.1).
// The OTEL_* variable also accepts true and false for Go instrumentation such as otelc that expects a boolean.
func contentFromEnv() (mode ContentMode, set bool, problem string) {
	if raw := env("FLASHCAT_CAPTURE_CONTENT"); raw != "" {
		return parseContentEnv("FLASHCAT_CAPTURE_CONTENT", raw, false)
	}
	if raw := env("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"); raw != "" {
		return parseContentEnv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", raw, true)
	}
	return ContentOff, false, ""
}

func parseContentEnv(name, raw string, boolOK bool) (ContentMode, bool, string) {
	low := strings.ToLower(raw)
	if m, ok := contentValues[low]; ok {
		return m, true, ""
	}
	if boolOK && low == "true" {
		return ContentSpan, true, ""
	}
	if eventContentValues[low] {
		return ContentOff, true, eventProblem(low)
	}
	return ContentOff, true, fmt.Sprintf("%s=%q 无法识别（只支持 false / span），已按不采正文处理", name, raw)
}

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// resolve parses configuration. Only invalid explicit options return an error. Other issues go into problems.
func resolve(opts []Option) (*settings, error) {
	o := &options{}
	for _, fn := range opts {
		if fn != nil {
			fn(o)
		}
	}
	s := &settings{exporter: ExporterAuto, metrics: true, exportTimeout: 10 * time.Second, shutdownTimeout: 5 * time.Second, debug: o.debug,
		lifecycle: global.lifecycle}

	if o.exporter != nil {
		switch *o.exporter {
		case ExporterAuto, ExporterAdd, ExporterReuse, ExporterNone:
			s.exporter = *o.exporter
		default:
			return nil, &configError{msg: fmt.Sprintf(`exporter 只能是 "auto" / "add" / "reuse" / "none"，收到 %q`, string(*o.exporter))}
		}
	}
	// The explicit content capture switch takes precedence over environment variables.
	if o.captureContent != nil {
		m, problem, err := parseContentExplicit(*o.captureContent)
		if err != nil {
			return nil, err
		}
		s.content = m
		if problem != "" {
			s.problems = append(s.problems, problem)
		}
	} else {
		m, _, problem := contentFromEnv()
		s.content = m
		if problem != "" {
			s.problems = append(s.problems, problem)
		}
	}
	if o.masking != nil {
		s.masking = *o.masking
	}
	mk, err := newMasker(s.masking)
	if err != nil {
		return nil, err
	}
	s.compiled = mk
	if o.metrics != nil {
		s.metrics = *o.metrics
	}
	if o.exportTimeout != nil {
		if *o.exportTimeout <= 0 {
			return nil, &configError{msg: fmt.Sprintf("export timeout 必须大于 0，收到 %s", *o.exportTimeout)}
		}
		s.exportTimeout = *o.exportTimeout
	}
	if o.shutdownTimeout != nil {
		if *o.shutdownTimeout <= 0 {
			return nil, &configError{msg: fmt.Sprintf("shutdown timeout 必须大于 0，收到 %s", *o.shutdownTimeout)}
		}
		s.shutdownTimeout = *o.shutdownTimeout
	} else if raw := env("FLASHCAT_SHUTDOWN_TIMEOUT"); raw != "" {
		seconds, err := strconv.ParseFloat(raw, 64)
		nanos := seconds * float64(time.Second)
		if err != nil || math.IsNaN(nanos) || math.IsInf(nanos, 0) || nanos < 1 || nanos >= float64(math.MaxInt64) {
			s.problems = append(s.problems, "FLASHCAT_SHUTDOWN_TIMEOUT 必须是大于 0 且可表示的秒数，已使用默认值 5 秒")
		} else {
			s.shutdownTimeout = time.Duration(nanos)
		}
	}
	s.resourceAttrs = o.resourceAttrs

	resEnv := parseKVList(env("OTEL_RESOURCE_ATTRIBUTES"))
	s.serviceName = firstNonEmpty(strings.TrimSpace(o.serviceName), env("FLASHCAT_SERVICE_NAME"), env("OTEL_SERVICE_NAME"), resEnv["service.name"])
	s.environment = firstNonEmpty(strings.TrimSpace(o.environment), env("FLASHCAT_ENVIRONMENT"),
		resEnv["deployment.environment.name"], resEnv["deployment.environment"], "prod")

	ep := firstNonEmpty(strings.TrimSpace(o.endpoint), env("FLASHCAT_ENDPOINT"), env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	if ep == "" {
		if base := env("OTEL_EXPORTER_OTLP_ENDPOINT"); base != "" {
			ep = strings.TrimRight(base, "/") + "/v1/traces"
		}
	}
	if ep != "" {
		u, perr := url.Parse(ep)
		switch {
		case perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			s.problems = append(s.problems, fmt.Sprintf("endpoint 必须是完整 URL（含 http(s):// 与 /v1/traces），收到 '%s'；不自动补端口，以免落到无鉴权入口", ep))
			ep = ""
		case u.Path == "" || u.Path == "/":
			s.problems = append(s.problems, fmt.Sprintf("endpoint 缺少路径，应为 .../v1/traces，收到 '%s'", ep))
			ep = ""
		}
	}
	s.endpoint = ep

	s.metricsEndpoint = firstNonEmpty(env("FLASHCAT_METRICS_ENDPOINT"), env("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"))
	if s.metricsEndpoint == "" && strings.HasSuffix(ep, "/v1/traces") {
		s.metricsEndpoint = strings.TrimSuffix(ep, "/v1/traces") + "/v1/metrics"
	}

	key := firstNonEmpty(strings.TrimSpace(o.apiKey), env("FLASHCAT_API_KEY"))
	if key == "" {
		headers := parseKVList(firstNonEmpty(env("OTEL_EXPORTER_OTLP_TRACES_HEADERS"), env("OTEL_EXPORTER_OTLP_HEADERS")))
		for k, v := range headers {
			if strings.EqualFold(k, "authorization") {
				s.authHeader = v
			}
		}
	} else if !strings.Contains(key, ":") {
		s.problems = append(s.problems, "api_key 应为 keyID:secret（不要传 base64，也不要传 URL 编码的 Authorization）")
		key = ""
	}
	s.apiKey = key

	s.disabled = truthy(env("FLASHCAT_DISABLED")) || truthy(env("OTEL_SDK_DISABLED"))

	for _, h := range strings.Split(env("FLASHCAT_KNOWN_HOSTS"), ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			s.knownHosts = append(s.knownHosts, h)
		}
	}
	if ep != "" {
		if u, err := url.Parse(ep); err == nil && u.Hostname() != "" {
			s.knownHosts = append(s.knownHosts, strings.ToLower(u.Hostname()))
		}
	}
	return s, nil
}

// upstreamContentEnum matches Python's UPSTREAM_CONTENT_ENUM and feeds content.upstream in diagnostics.
func upstreamContentEnum(m ContentMode) string {
	if m == ContentSpan {
		return "SPAN_ONLY"
	}
	return "NO_CONTENT"
}
