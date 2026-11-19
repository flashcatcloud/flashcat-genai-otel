package flashcatotel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// global holds process-wide state. The SDK initializes once per process, like the OTel global provider.
var global = newGlobalState()

type globalState struct {
	mu        sync.Mutex
	sdk       *SDK
	stats     *exportStats
	lifecycle *exportLifecycle
	orphans   atomic.Int64
	nested    atomic.Int64
	masked    atomic.Int64
	truncated atomic.Int64
	ancestry  genAIAncestry
	// content is 0 before Init (use the environment), 1 for capture off, and 2 for capture in span attributes.
	content atomic.Int32
	// processorCreated records whether NewSpanProcessor has been called in this process.
	processorCreated atomic.Bool
	// nestedWarned records whether the nested inference span warning has been logged. Each cause is logged once.
	nestedWarned atomic.Bool
}

func newGlobalState() *globalState {
	return &globalState{stats: &exportStats{}, lifecycle: newExportLifecycle()}
}

// SDK is the result of Init. Its methods are safe on a nil receiver.
type SDK struct {
	mu             sync.Mutex
	diag           Diagnostics
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	ownTracer      bool // Scenario A, where the SDK creates the provider.
	ownMeter       bool
	shutdownTO     time.Duration
	shutdownOnce   sync.Once
	lifecycle      *exportLifecycle
}

// ContentCaptureEnabled reports whether content capture is enabled. Before Init, it checks the environment
// (FLASHCAT_CAPTURE_CONTENT takes precedence over OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT).
// After Init, it uses the resolved setting. Instrumentation submodules call it on each request, so middleware can be created before Init.
func ContentCaptureEnabled() bool {
	switch global.content.Load() {
	case 1:
		return false
	case 2:
		return true
	}
	m, _, _ := contentFromEnv()
	return m == ContentSpan
}

// Init initializes the SDK and returns an SDK handle with diagnostics. Call it before creating model clients.
//
// An invalid explicit option returns an error before initialization, so callers can fix the option and retry.
// Other issues go into diagnostics, and the returned SDK remains usable. Later calls return the original SDK without repeating initialization.
// Go has no process exit hook. Defer sdk.Shutdown(ctx) in main or call sdk.Flush(ctx) before a short script exits.
func Init(ctx context.Context, opts ...Option) (*SDK, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	global.mu.Lock()
	defer global.mu.Unlock()
	if global.sdk != nil {
		slog.Warn("flashcat-otel: init 已调用过，本次忽略（OTel 不允许替换已设置的全局 TracerProvider）")
		return global.sdk, nil
	}
	s, err := resolve(opts)
	if err != nil {
		d := baseDiagnostics()
		d.Problems = append(d.Problems, err.Error())
		return &SDK{diag: d, shutdownTO: 5 * time.Second}, err
	}
	sdk := initLocked(ctx, s)
	global.sdk = sdk
	return sdk, nil
}

func baseDiagnostics() Diagnostics {
	return Diagnostics{
		SDK:      SDKInfo{Name: distroName, Version: Version, Language: "go"},
		Problems: []string{},
		Notes:    []string{},
		Provider: ProviderInfo{ExporterMode: "none", Metrics: "none"},
		Content:  map[string]string{},
	}
}

func initLocked(ctx context.Context, s *settings) *SDK {
	d := baseDiagnostics()
	d.ServiceName, d.Environment, d.Endpoint = s.serviceName, s.environment, s.endpoint
	d.Problems = append(d.Problems, s.problems...)
	sdk := &SDK{shutdownTO: s.shutdownTimeout, lifecycle: s.lifecycle}

	if s.disabled {
		d.Notes = append(d.Notes, "FLASHCAT_DISABLED / OTEL_SDK_DISABLED 已开启，未做任何事")
		sdk.diag = d
		emit(sdk.Diagnostics(), s.debug)
		return sdk
	}
	if s.serviceName == "" || strings.HasPrefix(s.serviceName, "unknown_service") {
		d.Problems = append(d.Problems, "缺少 service_name（WithServiceName 或 OTEL_SERVICE_NAME），且不能是 unknown_service*；未启用导出")
		if s.exporter == ExporterAuto || s.exporter == ExporterAdd {
			s.exporter = ExporterNone
		}
	}

	// Apply the content capture switch before instrumentation creates any spans.
	d.Content = applyContent(s)

	func() {
		defer func() {
			if r := recover(); r != nil {
				d.Problems = append(d.Problems, fmt.Sprintf("初始化 provider 失败：%T: %v", r, r))
			}
		}()
		setupTracing(ctx, s, sdk, &d)
		setupMetrics(ctx, s, sdk, &d)
	}()
	d.Enabled = d.Provider.ExporterMode != "none" || d.Provider.Situation == "C"
	sdk.diag = d
	emit(sdk.Diagnostics(), s.debug)
	return sdk
}

// applyContent applies the content capture switch to each source (contract §7). It always takes effect for the SDK's semantic API
// and instrumentation submodules. Other sources are configured on a best-effort basis.
func applyContent(s *settings) map[string]string {
	on := s.content == ContentSpan
	if on {
		global.content.Store(2)
	} else {
		global.content.Store(1)
	}
	// The Go compile-time instrumentation, otelc, recognizes only "true" as enabling capture.
	_ = os.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", map[bool]string{true: "true", false: "false"}[on])
	content := map[string]string{"upstream": upstreamContentEnum(s.content)}
	// The Anthropic Go SDK's built-in instrumentation reads this variable on each request. Set it only if unset.
	if _, ok := os.LookupEnv("ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE"); !ok {
		_ = os.Setenv("ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE", map[bool]string{true: "content", false: "metadata_only"}[on])
	}
	content["anthropic"] = os.Getenv("ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE")
	for _, k := range []string{"OPENINFERENCE_HIDE_INPUTS", "OPENINFERENCE_HIDE_OUTPUTS"} {
		if _, ok := os.LookupEnv(k); !ok {
			_ = os.Setenv(k, map[bool]string{true: "false", false: "true"}[on])
		}
	}
	content["openinference"] = "best_effort:hide=" + os.Getenv("OPENINFERENCE_HIDE_INPUTS")
	return content
}

// isGlobalDefault reports whether v is the default placeholder for an OTel global API (an unexported type in internal/global).
func isGlobalDefault(v any, typeName string) bool {
	t := reflect.TypeOf(v)
	if t == nil || t.Kind() != reflect.Pointer {
		return false
	}
	t = t.Elem()
	return t.Name() == typeName && t.PkgPath() == "go.opentelemetry.io/otel/internal/global"
}

func existingEndpointHost() string {
	ep := firstNonEmpty(env("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"), env("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if ep == "" {
		return ""
	}
	u, err := url.Parse(ep)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func buildResource(s *settings) *resource.Resource {
	attrs := []attribute.KeyValue{
		attribute.String("deployment.environment.name", s.environment),
		attribute.String("telemetry.distro.name", distroName),
		attribute.String("telemetry.distro.version", Version),
	}
	if s.serviceName != "" {
		attrs = append(attrs, attribute.String("service.name", s.serviceName))
	}
	for k, v := range s.resourceAttrs {
		attrs = append(attrs, attribute.String(k, v))
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		return resource.NewSchemaless(attrs...)
	}
	return res
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// setupTracing handles the three provider scenarios (contract §2.4).
func setupTracing(ctx context.Context, s *settings, sdk *SDK, d *Diagnostics) {
	current := otel.GetTracerProvider()
	sdkTP, isSDK := current.(*sdktrace.TracerProvider)
	situation := "unsupported"
	switch {
	case isGlobalDefault(current, "tracerProvider"):
		situation = "A"
	case isSDK && global.processorCreated.Load():
		situation = "C"
		d.Notes = append(d.Notes, "进程里已通过 NewSpanProcessor 接入 Flashcat 处理器，不再追加导出链和会话处理")
	case isSDK:
		if h := existingEndpointHost(); h != "" && contains(s.knownHosts, h) {
			situation = "C"
		} else {
			situation = "B"
		}
	default:
		d.Notes = append(d.Notes, fmt.Sprintf("进程里已有非 SDK 的 TracerProvider（%T），无法追加处理器", current))
	}

	mode := s.exporter
	if mode == ExporterAuto {
		mode = map[string]ExporterMode{"A": ExporterAdd, "B": ExporterAdd, "C": ExporterReuse, "unsupported": ExporterNone}[situation]
	}
	if situation == "C" && s.exporter == ExporterAdd {
		d.Notes = append(d.Notes, "进程已在往 Flashcat 发数据，显式 exporter='add' 可能产生重复上报")
	}
	if mode == ExporterReuse {
		d.Notes = append(d.Notes, "未新增 exporter，数据经进程里已有的 exporter 发出（GenAI 数据由平台侧识别）")
	}

	var tp *sdktrace.TracerProvider
	if situation == "A" {
		tp = sdktrace.NewTracerProvider(sdktrace.WithResource(buildResource(s)))
		otel.SetTracerProvider(tp)
		if isGlobalDefault(otel.GetTextMapPropagator(), "textMapPropagator") {
			otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
		}
		sdk.ownTracer = true
	} else if isSDK {
		tp = sdkTP
	}
	sdk.tracerProvider = tp

	if tp != nil {
		alreadyFlashcat := situation == "C" && global.processorCreated.Load()
		if !alreadyFlashcat {
			tp.RegisterSpanProcessor(sessionProcessor{})
		}
		if mode == ExporterAdd {
			switch {
			case s.endpoint == "":
				d.Notes = append(d.Notes, "缺少 endpoint（FLASHCAT_ENDPOINT，完整 .../v1/traces URL），未启用导出")
				mode = ExporterNone
			case authHeader(s) == "":
				d.Notes = append(d.Notes, "缺少 api_key（FLASHCAT_API_KEY=keyID:secret），未启用导出")
				mode = ExporterNone
			default:
				processor, err := newSpanExportProcessor(ctx, s)
				if err != nil {
					d.Problems = append(d.Problems, fmt.Sprintf("创建 exporter 失败：%v", err))
					mode = ExporterNone
				} else {
					tp.RegisterSpanProcessor(processor)
				}
			}
		}
	} else if mode == ExporterAdd || mode == ExporterReuse {
		mode = ExporterNone
	}
	d.Provider.Situation = situation
	d.Provider.ExporterMode = string(mode)
}

// setupMetrics handles MeterProvider similarly. Instrumentation silently drops gen_ai.client.* metrics
// when only a TracerProvider is configured.
func setupMetrics(ctx context.Context, s *settings, sdk *SDK, d *Diagnostics) {
	if !s.metrics {
		d.Provider.Metrics = "disabled"
		return
	}
	current := otel.GetMeterProvider()
	if mp, ok := current.(*sdkmetric.MeterProvider); ok {
		sdk.meterProvider = mp
		d.Provider.Metrics = "reuse"
		return
	}
	if d.Provider.ExporterMode != string(ExporterAdd) || s.metricsEndpoint == "" || !isGlobalDefault(current, "meterProvider") {
		d.Provider.Metrics = "none"
		if d.Provider.ExporterMode == string(ExporterAdd) && s.metricsEndpoint == "" {
			d.Notes = append(d.Notes, "没有 metrics endpoint，gen_ai.client.* 指标不导出")
		}
		return
	}
	exp, err := newMetricExporter(ctx, s)
	if err != nil {
		d.Problems = append(d.Problems, fmt.Sprintf("创建 metrics exporter 失败：%v", err))
		d.Provider.Metrics = "none"
		return
	}
	res := buildResource(s)
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(60*time.Second))))
	otel.SetMeterProvider(mp)
	sdk.meterProvider = mp
	sdk.ownMeter = true
	d.Provider.Metrics = "add"
}

func emit(d Diagnostics, debug bool) {
	if debug {
		b, _ := json.Marshal(d)
		slog.Info("flashcat-otel diagnostics: " + string(b))
		return
	}
	installed := []string{}
	for _, i := range d.Instrumentations {
		if i.Status == "installed" {
			installed = append(installed, i.Lib)
		}
	}
	brief := map[string]any{"service": d.ServiceName, "enabled": d.Enabled, "provider": d.Provider, "installed": installed, "problems": d.Problems}
	b, _ := json.Marshal(brief)
	slog.Info("flashcat-otel: " + string(b))
}

// Diagnostics returns the same structure as Init, with live export counters, semantics stats, and registered instrumentation.
func (s *SDK) Diagnostics() Diagnostics {
	if s == nil {
		return GetDiagnostics()
	}
	s.mu.Lock()
	d := s.diag.clone()
	s.mu.Unlock()
	fillLive(&d)
	return d
}

func fillLive(d *Diagnostics) {
	d.Instrumentations = registeredInstrumentations()
	d.Export = global.stats.snapshot()
	d.Semantics = SemanticsStats{
		OrphanInferenceSpans: global.orphans.Load(),
		NestedInferenceSpans: global.nested.Load(),
		MaskedSpans:          global.masked.Load(),
		TruncatedSpans:       global.truncated.Load(),
	}
	if d.Semantics.NestedInferenceSpans > 0 && !slices.Contains(d.Problems, nestedInferenceProblem) {
		d.Problems = append(d.Problems, nestedInferenceProblem)
	}
}

// GetDiagnostics returns diagnostics for the process's SDK. If Init has yet to run, it records this in problems.
func GetDiagnostics() Diagnostics {
	global.mu.Lock()
	sdk := global.sdk
	global.mu.Unlock()
	if sdk != nil {
		return sdk.Diagnostics()
	}
	d := baseDiagnostics()
	d.Problems = append(d.Problems, "init 尚未调用")
	fillLive(&d)
	return d
}

func (s *SDK) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	to := s.shutdownTO
	if to <= 0 {
		to = 5 * time.Second
	}
	return context.WithTimeout(ctx, to)
}

// Flush immediately exports finished spans and metrics. Call it before a short script or serverless function exits.
// It waits up to the shutdown timeout when ctx has no deadline.
func (s *SDK) Flush(ctx context.Context) error {
	if s == nil {
		return nil
	}
	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	if s.lifecycle != nil {
		release := s.lifecycle.limit(ctx)
		defer release()
	}
	var errs []error
	if s.tracerProvider != nil {
		errs = append(errs, s.tracerProvider.ForceFlush(ctx))
	}
	if s.meterProvider != nil {
		errs = append(errs, s.meterProvider.ForceFlush(ctx))
	}
	return errors.Join(errs...)
}

// Shutdown flushes telemetry and then shuts down providers created by the SDK (scenario A).
// Existing providers are flushed and remain open.
func (s *SDK) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var err error
	s.shutdownOnce.Do(func() {
		ctx, cancel := s.withTimeout(ctx)
		defer cancel()
		if s.lifecycle != nil {
			s.lifecycle.beginShutdown(ctx)
		}
		errs := []error{s.Flush(ctx)}
		if s.ownTracer && s.tracerProvider != nil {
			errs = append(errs, s.tracerProvider.Shutdown(ctx))
		}
		if s.ownMeter && s.meterProvider != nil {
			errs = append(errs, s.meterProvider.Shutdown(ctx))
		}
		err = errors.Join(errs...)
	})
	return err
}

// ---- For callers who manage their own provider ----

// flashcatProcessor combines session processing with the Flashcat export pipeline.
type flashcatProcessor struct {
	session    sessionProcessor
	batch      sdktrace.SpanProcessor // Nil with incomplete configuration, which enables only session processing.
	lifecycle  *exportLifecycle
	shutdownTO time.Duration
}

func (p *flashcatProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	p.session.OnStart(ctx, s)
	if p.batch != nil {
		p.batch.OnStart(ctx, s)
	}
}

func (p *flashcatProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	if p.batch != nil {
		p.batch.OnEnd(s)
	}
}

func (p *flashcatProcessor) Shutdown(ctx context.Context) error {
	if p.batch != nil {
		ctx, cancel := (&SDK{shutdownTO: p.shutdownTO}).withTimeout(ctx)
		defer cancel()
		p.lifecycle.beginShutdown(ctx)
		return p.batch.Shutdown(ctx)
	}
	return nil
}

func (p *flashcatProcessor) ForceFlush(ctx context.Context) error {
	if p.batch != nil {
		ctx, cancel := (&SDK{shutdownTO: p.shutdownTO}).withTimeout(ctx)
		defer cancel()
		release := p.lifecycle.limit(ctx)
		defer release()
		return p.batch.ForceFlush(ctx)
	}
	return nil
}

// NewSpanProcessor returns the Flashcat SpanProcessor for a caller-managed TracerProvider. It provides
// session processing, masking, batching, and OTLP export. Install it with
// sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(flashcatotel.NewSpanProcessor(...))).
// Configuration follows the same rules as Init. Invalid options or a missing endpoint or credentials
// leave only session processing enabled and produce a one-line slog warning.
// A later Init call uses scenario C and avoids adding another export pipeline. GetDiagnostics reports the export counters.
func NewSpanProcessor(opts ...Option) sdktrace.SpanProcessor {
	global.processorCreated.Store(true)
	p := &flashcatProcessor{}
	s, err := resolve(opts)
	if err != nil {
		slog.Warn("flashcat-otel: NewSpanProcessor 参数非法，只做会话处理", "error", err.Error())
		return p
	}
	p.lifecycle, p.shutdownTO = s.lifecycle, s.shutdownTimeout
	for _, problem := range s.problems {
		slog.Warn("flashcat-otel: " + problem)
	}
	switch {
	case s.endpoint == "":
		slog.Warn("flashcat-otel: 缺少 endpoint（FLASHCAT_ENDPOINT，完整 .../v1/traces URL），未启用导出")
		return p
	case authHeader(s) == "":
		slog.Warn("flashcat-otel: 缺少 api_key（FLASHCAT_API_KEY=keyID:secret），未启用导出")
		return p
	}
	processor, err := newSpanExportProcessor(context.Background(), s)
	if err != nil {
		slog.Warn("flashcat-otel: 创建 exporter 失败", "error", err.Error())
		return p
	}
	p.batch = processor
	return p
}
