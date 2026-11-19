package flashcatotel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// app mirrors APP in Python's tests/test_sdk.py. A session contains an agent with a tool and a simulated chat span.
func app(ctx context.Context, question string) {
	ctx = WithSession(ctx, Session{ConversationID: "c-123", UserID: "u-9", Tags: map[string]string{"team": "kf"}})
	actx, agent := StartAgent(ctx, "kefu")
	agent.SetInput(question)
	_ = Tool(actx, "lookup_order", func(ctx context.Context) error {
		_, sp := StartTool(ctx, "lookup_order_detail")
		sp.SetInput(map[string]string{"order_id": "A-1"})
		sp.SetOutput(map[string]string{"status": "shipped"})
		sp.End(nil)
		return nil
	})
	// Simulate the inference span created by the OpenAI middleware to verify session attributes and masking.
	_, chat := otel.Tracer("test.instrumentation").Start(actx, "chat gpt-4o", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat"), attribute.Int("gen_ai.usage.input_tokens", 120)))
	if ContentCaptureEnabled() {
		chat.SetAttributes(attribute.String("gen_ai.input.messages", messagesJSON("user", question)))
	}
	chat.End()
	agent.SetOutput("您的订单已发货")
	agent.End(nil)
}

func init() {
	scenarios["situation_a"] = func() any {
		ctx := context.Background()
		sdk, err := Init(ctx, WithServiceName("kefu-agent"))
		if err != nil {
			panic(err)
		}
		app(ctx, "我的订单到哪了")
		// Span outside the session scope.
		_ = Trace(ctx, "outside", func(context.Context) error { return nil })
		_ = sdk.Flush(ctx)
		d := sdk.Diagnostics()
		_ = sdk.Shutdown(ctx)
		return d
	}
	scenarios["content_span"] = func() any {
		ctx := context.Background()
		sdk, _ := Init(ctx, WithServiceName("kefu-agent"), WithCaptureContent(ContentSpan), WithMetrics(false))
		app(ctx, "手机13812345678，邮箱a@b.com")
		_ = sdk.Flush(ctx)
		return sdk.Diagnostics()
	}
	scenarios["situation_c"] = func() any {
		ctx := context.Background()
		rec := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
		otel.SetTracerProvider(tp)
		sdk, _ := Init(ctx, WithServiceName("order-svc"))
		actx, agent := StartAgent(ctx, "reuse")
		_, late := tp.Tracer("late.instrumentation").Start(actx, "late child")
		late.SetAttributes(attribute.String(attrOperationName, "chat"))
		late.End()
		agent.End(nil)
		roots := map[string]bool{}
		for _, s := range rec.Ended() {
			if v, ok := attrMap(s)[attrGenAIRoot]; ok {
				roots[s.Name()] = v.AsBool()
			}
		}
		return map[string]any{"diag": sdk.Diagnostics(), "roots": roots}
	}
	scenarios["late_genai_root"] = func() any {
		ctx := context.Background()
		sdk, _ := Init(ctx, WithServiceName("x"), WithMetrics(false))
		tr := otel.Tracer("late.instrumentation")
		pctx, parent := tr.Start(ctx, "late parent")
		parent.SetAttributes(attribute.String("openinference.span.kind", "AGENT"))
		_, early := tr.Start(pctx, "early child", trace.WithAttributes(attribute.String(attrOperationName, "chat")))
		early.End()
		bctx, bridge := tr.Start(pctx, "plain bridge")
		_, late := tr.Start(bctx, "late child")
		late.SetAttributes(attribute.String(attrOperationName, "chat"))
		late.End()
		bridge.End()
		parent.End()
		_ = sdk.Flush(ctx)
		return sdk.Diagnostics()
	}
	scenarios["situation_b"] = func() any {
		// The application's provider exports elsewhere. Appending the Flashcat processor sends the same spans to Flashcat.
		tp := sdktrace.NewTracerProvider()
		otel.SetTracerProvider(tp)
		sdk, _ := Init(context.Background(), WithServiceName("x"), WithMetrics(false))
		_ = Workflow(context.Background(), "wf", func(context.Context) error { return nil })
		_ = sdk.Flush(context.Background())
		_ = sdk.Shutdown(context.Background())
		// Spans can still start and end on the application's provider after Shutdown.
		_, s := tp.Tracer("t").Start(context.Background(), "after")
		return map[string]any{"diag": sdk.Diagnostics(), "user_tp_alive": s.IsRecording()}
	}
	scenarios["unsupported"] = func() any {
		otel.SetTracerProvider(noop.NewTracerProvider())
		sdk, _ := Init(context.Background(), WithServiceName("x"))
		return sdk.Diagnostics()
	}
	scenarios["http_401"] = func() any {
		ctx := context.Background()
		sdk, _ := Init(ctx, WithServiceName("x"), WithMetrics(false))
		_ = Trace(ctx, "job", func(context.Context) error { return nil })
		_ = Workflow(ctx, "wf", func(context.Context) error { return nil })
		_ = sdk.Flush(ctx)
		return sdk.Diagnostics()
	}
	scenarios["init_only"] = func() any {
		sdk, err := Init(context.Background())
		if err != nil {
			panic(err)
		}
		return sdk.Diagnostics()
	}
	scenarios["bad_option"] = func() any {
		sdk, err := Init(context.Background(), WithServiceName("x"), WithCaptureContent("true"))
		_, err2 := Init(context.Background(), WithServiceName("x"), WithExporter("bogus"))
		// Init can succeed on retry after correcting invalid options.
		sdk3, err3 := Init(context.Background(), WithServiceName("x"), WithExporter(ExporterNone))
		return map[string]any{
			"err": errString(err), "err2": errString(err2), "err3": errString(err3),
			"sdk_nil": sdk == nil, "bad_diag": sdk.Diagnostics(), "diag3": sdk3.Diagnostics(),
			"situation_after": sdk3.Diagnostics().Provider.Situation,
		}
	}
	scenarios["idempotent"] = func() any {
		a, _ := Init(context.Background(), WithServiceName("first"), WithExporter(ExporterNone))
		tp := otel.GetTracerProvider()
		b, err := Init(context.Background(), WithServiceName("second"), WithCaptureContent("true"))
		return map[string]any{"same": a == b, "err": errString(err), "tp_same": tp == otel.GetTracerProvider(), "service": b.Diagnostics().ServiceName}
	}
	scenarios["disabled"] = func() any {
		sdk, _ := Init(context.Background(), WithServiceName("x"))
		return map[string]any{"diag": sdk.Diagnostics(), "global_default": isGlobalDefault(otel.GetTracerProvider(), "tracerProvider"),
			"semantic_ok": Agent(context.Background(), "a", func(context.Context) error { return nil }) == nil}
	}
	scenarios["new_span_processor"] = func() any {
		ctx := context.Background()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(NewSpanProcessor(WithServiceName("self-managed"))))
		otel.SetTracerProvider(tp)
		sdk, _ := Init(ctx, WithServiceName("self-managed"))
		cctx := WithSession(ctx, Session{ConversationID: "c-np"})
		_ = Agent(cctx, "a", func(ctx context.Context) error {
			_, s := tp.Tracer("late.instrumentation").Start(ctx, "late tool")
			s.SetAttributes(attribute.String(attrOperationName, "execute_tool"))
			s.End()
			return Tool(ctx, "t", func(context.Context) error { return nil })
		})
		_ = tp.ForceFlush(ctx)
		return sdk.Diagnostics()
	}
	scenarios["get_diag_before_init"] = func() any { return GetDiagnostics() }
	scenarios["content_env_fanout"] = func() any {
		sdk, _ := Init(context.Background(), WithServiceName("x"), WithCaptureContent(ContentSpan), WithExporter(ExporterNone))
		return map[string]any{"diag": sdk.Diagnostics(), "env_upstream": env("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"),
			"env_anthropic": env("ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE"), "env_oi": env("OPENINFERENCE_HIDE_INPUTS"),
			"enabled": ContentCaptureEnabled()}
	}
	scenarios["propagator"] = func() any {
		_, _ = Init(context.Background(), WithServiceName("x"), WithExporter(ExporterNone))
		ctx := WithSession(context.Background(), Session{ConversationID: "c-1", UserID: "u-1", Propagate: true})
		ctx, sp := otel.Tracer("t").Start(ctx, "root")
		defer sp.End()
		carrier := map[string]string{}
		otel.GetTextMapPropagator().Inject(ctx, mapCarrier(carrier))
		return carrier
	}
}

type mapCarrier map[string]string

func (m mapCarrier) Get(k string) string { return m[k] }
func (m mapCarrier) Set(k, v string)     { m[k] = v }
func (m mapCarrier) Keys() []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func flashcatEnv(c *collector, key string) map[string]string {
	return map[string]string{"FLASHCAT_ENDPOINT": c.url() + "/v1/traces", "FLASHCAT_API_KEY": key}
}

func TestSituationAEndToEnd(t *testing.T) {
	c := newCollector(t, 200)
	d := runScenario(t, "situation_a", flashcatEnv(c, "fck_test:s3cret"))
	if path(d, "provider", "situation") != "A" || path(d, "provider", "exporter_mode") != "add" || path(d, "provider", "metrics") != "add" {
		t.Fatalf("provider: %v", d["provider"])
	}
	if num(path(d, "export", "ok")) < 4 {
		t.Fatalf("export.ok: %v", d["export"])
	}
	if path(d, "sdk", "language") != "go" || path(d, "enabled") != true {
		t.Fatalf("diag: %v", d)
	}
	reqs := c.traceRequests()
	if len(reqs) == 0 {
		t.Fatal("collector got no trace requests")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("fck_test:s3cret"))
	if got := reqs[0].Headers.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if reqs[0].Headers.Get("Content-Encoding") != "gzip" || reqs[0].Headers.Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("headers: %v", reqs[0].Headers)
	}
	spans := c.spans(t)
	ops := byOp(spans)
	chat, agent, tool := ops["chat"], ops["invoke_agent"], ops["execute_tool"]
	if chat.Resource["service.name"] != "kefu-agent" || chat.Resource["telemetry.distro.name"] != "flashcat-otel" ||
		chat.Resource["telemetry.distro.version"] != Version || chat.Resource["deployment.environment.name"] != "prod" ||
		chat.Resource["telemetry.sdk.language"] != "go" {
		t.Fatalf("resource: %v", chat.Resource)
	}
	for _, s := range []gotSpan{chat, agent, tool} {
		if s.Attrs["gen_ai.conversation.id"] != "c-123" || s.Attrs["user.id"] != "u-9" || s.Attrs["flashcat.tag.team"] != "kf" {
			t.Fatalf("session attrs missing on %s: %v", s.Name, s.Attrs)
		}
	}
	if ops["outside"].Attrs["gen_ai.conversation.id"] != nil {
		t.Fatalf("span outside session got session attrs: %v", ops["outside"].Attrs)
	}
	if chat.Attrs["gen_ai.usage.input_tokens"] != int64(120) {
		t.Fatalf("usage: %v", chat.Attrs)
	}
	for _, s := range spans {
		for k := range s.Attrs {
			if k == "gen_ai.input.messages" || k == "gen_ai.output.messages" || strings.HasPrefix(k, "gen_ai.tool.call.") {
				t.Fatalf("content capture is off by default, but %s has %s", s.Name, k)
			}
		}
	}
	if chat.Parent != agent.SpanID || tool.Parent != agent.SpanID {
		t.Fatalf("parent links wrong: chat.parent=%s tool.parent=%s agent=%s", chat.Parent, tool.Parent, agent.SpanID)
	}
	if agent.Name != "invoke_agent kefu" || agent.Attrs["gen_ai.agent.name"] != "kefu" || agent.Scope != ScopeName {
		t.Fatalf("agent span: %+v", agent)
	}
	if tool.Name != "execute_tool lookup_order" || tool.Attrs["gen_ai.tool.type"] != "function" {
		t.Fatalf("tool span: %+v", tool)
	}
}

func TestContentSpanWithMasking(t *testing.T) {
	c := newCollector(t, 200)
	d := runScenario(t, "content_span", flashcatEnv(c, "fck_test:s3cret"))
	ops := byOp(c.spans(t))
	msgs, _ := ops["chat"].Attrs["gen_ai.input.messages"].(string)
	if msgs == "" || strings.Contains(msgs, "13812345678") || strings.Contains(msgs, "a@b.com") || !strings.Contains(msgs, "[REDACTED:phone]") {
		t.Fatalf("chat input messages not masked: %q", msgs)
	}
	agentIn, _ := ops["invoke_agent"].Attrs["gen_ai.input.messages"].(string)
	if !strings.Contains(agentIn, `"role":"user"`) || !strings.Contains(agentIn, "[REDACTED:email]") {
		t.Fatalf("agent input: %q", agentIn)
	}
	agentOut, _ := ops["invoke_agent"].Attrs["gen_ai.output.messages"].(string)
	if !strings.Contains(agentOut, `"role":"assistant"`) || !strings.Contains(agentOut, "您的订单已发货") {
		t.Fatalf("agent output: %q", agentOut)
	}
	var detail gotSpan
	for _, s := range c.spans(t) {
		if s.Name == "execute_tool lookup_order_detail" {
			detail = s
		}
	}
	if args, _ := detail.Attrs["gen_ai.tool.call.arguments"].(string); !strings.Contains(args, "A-1") {
		t.Fatalf("tool arguments: %v", detail.Attrs)
	}
	if res, _ := detail.Attrs["gen_ai.tool.call.result"].(string); !strings.Contains(res, "shipped") {
		t.Fatalf("tool result: %v", detail.Attrs)
	}
	if num(path(d, "semantics", "masked_spans")) < 2 || path(d, "content", "upstream") != "SPAN_ONLY" {
		t.Fatalf("diag: semantics=%v content=%v", d["semantics"], d["content"])
	}
}

func TestReuseWhenAlreadyExportingToFlashcat(t *testing.T) {
	c := newCollector(t, 200)
	e := flashcatEnv(c, "k:s")
	e["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = c.url() + "/v1/traces"
	out := runScenario(t, "situation_c", e)
	d := out["diag"].(map[string]any)
	if path(d, "provider", "situation") != "C" || path(d, "provider", "exporter_mode") != "reuse" || d["enabled"] != true {
		t.Fatalf("provider: %v", d["provider"])
	}
	if roots := out["roots"].(map[string]any); len(roots) != 1 || roots["invoke_agent reuse"] != true {
		t.Fatalf("reuse should mark only the early GenAI span: %v", roots)
	}
}

func TestGenAIRootFillsLateAttributesOnExport(t *testing.T) {
	c := newCollector(t, 200)
	d := runScenario(t, "late_genai_root", flashcatEnv(c, "k:s"))
	spans := map[string]gotSpan{}
	for _, s := range c.spans(t) {
		spans[s.Name] = s
	}
	for name, want := range map[string]bool{"late parent": true, "early child": false, "late child": false} {
		if got := spans[name].Attrs[attrGenAIRoot]; got != want {
			t.Errorf("%s: %s = %v, want %v", name, attrGenAIRoot, got, want)
		}
	}
	if bridge, ok := spans["plain bridge"]; !ok || bridge.Attrs[attrGenAIRoot] != nil {
		t.Errorf("plain bridge: %+v", bridge)
	}
	if num(path(d, "semantics", "masked_spans")) != 0 || num(path(d, "semantics", "truncated_spans")) != 0 {
		t.Errorf("root marking affected masking diagnostics: %v", d["semantics"])
	}
}

func TestAddToForeignProvider(t *testing.T) {
	c := newCollector(t, 200)
	e := flashcatEnv(c, "k:s")
	e["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = "http://other-vendor.example.com:4318/v1/traces"
	out := runScenario(t, "situation_b", e)
	d := out["diag"].(map[string]any)
	if path(d, "provider", "situation") != "B" || path(d, "provider", "exporter_mode") != "add" {
		t.Fatalf("provider: %v", d["provider"])
	}
	if ops := byOp(c.spans(t)); ops["invoke_workflow"].Name != "invoke_workflow wf" {
		t.Fatalf("appended processor did not export: %v", c.spans(t))
	}
	if out["user_tp_alive"] != true {
		t.Fatal("Shutdown closed the application's provider")
	}
}

func TestUnsupportedProvider(t *testing.T) {
	c := newCollector(t, 200)
	d := runScenario(t, "unsupported", flashcatEnv(c, "k:s"))
	if path(d, "provider", "situation") != "unsupported" || path(d, "provider", "exporter_mode") != "none" {
		t.Fatalf("provider: %v", d["provider"])
	}
}

func Test401CountedInDiagnostics(t *testing.T) {
	c := newCollector(t, 401)
	d := runScenario(t, "http_401", flashcatEnv(c, "k:bad"))
	last, _ := path(d, "export", "last_error").(string)
	if num(path(d, "export", "http_401")) < 1 || !strings.Contains(last, "401") {
		t.Fatalf("export: %v", d["export"])
	}
	if num(path(d, "export", "ok")) != 0 {
		t.Fatalf("export.ok should be 0: %v", d["export"])
	}
}

func TestNetworkErrorCounted(t *testing.T) {
	c := newCollector(t, 200)
	url := c.url()
	c.srv.Close() // The port is no longer listening.
	d := runScenario(t, "http_401", map[string]string{"FLASHCAT_ENDPOINT": url + "/v1/traces", "FLASHCAT_API_KEY": "k:s"})
	if num(path(d, "export", "network")) < 1 || path(d, "export", "last_error") == nil {
		t.Fatalf("export: %v", d["export"])
	}
}

func TestEndpointWithoutPathRejected(t *testing.T) {
	for _, ep := range []string{"ingest.example.com", "https://ingest.example.com", "https://ingest.example.com/"} {
		d := runScenario(t, "init_only", map[string]string{"FLASHCAT_ENDPOINT": ep, "FLASHCAT_API_KEY": "k:s", "OTEL_SERVICE_NAME": "x"})
		problems, _ := json.Marshal(d["problems"])
		if !strings.Contains(string(problems), "完整 URL") && !strings.Contains(string(problems), "缺少路径") {
			t.Fatalf("%s: problems=%s", ep, problems)
		}
		if path(d, "provider", "exporter_mode") != "none" || d["endpoint"] != nil {
			t.Fatalf("%s: provider=%v endpoint=%v", ep, d["provider"], d["endpoint"])
		}
	}
}

func TestMissingServiceName(t *testing.T) {
	for _, svc := range []string{"", "unknown_service:foo"} {
		e := map[string]string{"FLASHCAT_ENDPOINT": "http://127.0.0.1:1/v1/traces", "FLASHCAT_API_KEY": "k:s"}
		if svc != "" {
			e["OTEL_SERVICE_NAME"] = svc
		}
		d := runScenario(t, "init_only", e)
		problems, _ := json.Marshal(d["problems"])
		if !strings.Contains(string(problems), "service_name") || path(d, "provider", "exporter_mode") != "none" || d["enabled"] != false {
			t.Fatalf("svc=%q problems=%s provider=%v", svc, problems, d["provider"])
		}
	}
}

func TestBadAPIKeyFormat(t *testing.T) {
	d := runScenario(t, "init_only", map[string]string{"FLASHCAT_ENDPOINT": "http://127.0.0.1:1/v1/traces", "FLASHCAT_API_KEY": "nocolon", "OTEL_SERVICE_NAME": "x"})
	problems, _ := json.Marshal(d["problems"])
	notes, _ := json.Marshal(d["notes"])
	if !strings.Contains(string(problems), "keyID:secret") || path(d, "provider", "exporter_mode") != "none" || !strings.Contains(string(notes), "api_key") {
		t.Fatalf("problems=%s notes=%s provider=%v", problems, notes, d["provider"])
	}
}

func TestBadCaptureContentReturnsError(t *testing.T) {
	out := runScenario(t, "bad_option", nil)
	if !strings.Contains(out["err"].(string), "capture_content") || !strings.Contains(out["err2"].(string), "exporter") || out["err3"] != "" {
		t.Fatalf("errors: %v / %v / %v", out["err"], out["err2"], out["err3"])
	}
	if out["sdk_nil"] != false || out["situation_after"] != "A" {
		t.Fatalf("out: %v", out)
	}
}

func TestIdempotentInit(t *testing.T) {
	out := runScenario(t, "idempotent", nil)
	if out["same"] != true || out["err"] != "" || out["tp_same"] != true || out["service"] != "first" {
		t.Fatalf("out: %v", out)
	}
}

func TestDisabled(t *testing.T) {
	for _, k := range []string{"FLASHCAT_DISABLED", "OTEL_SDK_DISABLED"} {
		out := runScenario(t, "disabled", map[string]string{k: "true"})
		d := out["diag"].(map[string]any)
		notes, _ := json.Marshal(d["notes"])
		if d["enabled"] != false || !strings.Contains(string(notes), "FLASHCAT_DISABLED") || out["global_default"] != true || out["semantic_ok"] != true {
			t.Fatalf("%s: %v", k, out)
		}
	}
}

func TestNewSpanProcessorThenInitIsSituationC(t *testing.T) {
	c := newCollector(t, 200)
	d := runScenario(t, "new_span_processor", flashcatEnv(c, "k:s"))
	if path(d, "provider", "situation") != "C" || path(d, "provider", "exporter_mode") != "reuse" {
		t.Fatalf("provider: %v", d["provider"])
	}
	spans := c.spans(t)
	ids := map[string]int{}
	roots := map[string]any{}
	for _, s := range spans {
		ids[s.SpanID]++
		roots[s.Name] = s.Attrs[attrGenAIRoot]
		if s.Attrs["gen_ai.conversation.id"] != "c-np" {
			t.Fatalf("session attr missing: %v", s.Attrs)
		}
	}
	if len(spans) != 3 || len(ids) != 3 {
		t.Fatalf("expected 3 spans exported once each, got %d spans %v", len(spans), ids)
	}
	if roots["invoke_agent a"] != true || roots["execute_tool t"] != false || roots["late tool"] != false {
		t.Fatalf("AI roots: %v", roots)
	}
	if num(path(d, "export", "ok")) != 3 {
		t.Fatalf("export: %v", d["export"])
	}
}

// Check the diagnostics JSON keys against the example in contract §8 because the repository has no diagnostics JSON schema.
func TestDiagnosticsShape(t *testing.T) {
	d := runScenario(t, "get_diag_before_init", nil)
	delete(d, "_stderr")
	var keys []string
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := "content,enabled,endpoint,environment,export,instrumentations,notes,problems,provider,sdk,semantics,service_name"
	if strings.Join(keys, ",") != want {
		t.Fatalf("keys = %s", strings.Join(keys, ","))
	}
	if path(d, "sdk", "name") != "flashcat-otel" || path(d, "sdk", "version") != Version || path(d, "sdk", "language") != "go" {
		t.Fatalf("sdk: %v", d["sdk"])
	}
	if _, ok := d["instrumentations"].([]any); !ok {
		t.Fatalf("instrumentations must be [] not null: %v", d["instrumentations"])
	}
	exp := d["export"].(map[string]any)
	for _, k := range []string{"ok", "network", "dropped_queue_full", "last_error"} {
		if _, ok := exp[k]; !ok {
			t.Fatalf("export missing %s: %v", k, exp)
		}
	}
	if exp["last_error"] != nil {
		t.Fatalf("last_error should be null: %v", exp)
	}
	for _, k := range []string{"orphan_inference_spans", "masked_spans", "truncated_spans"} {
		if _, ok := path(d, "semantics").(map[string]any)[k]; !ok {
			t.Fatalf("semantics missing %s", k)
		}
	}
	for _, k := range []string{"situation", "exporter_mode", "metrics"} {
		if _, ok := path(d, "provider").(map[string]any)[k]; !ok {
			t.Fatalf("provider missing %s", k)
		}
	}
	problems, _ := json.Marshal(d["problems"])
	if !strings.Contains(string(problems), "init 尚未调用") {
		t.Fatalf("problems: %s", problems)
	}
}

func TestContentFanOut(t *testing.T) {
	out := runScenario(t, "content_env_fanout", nil)
	if out["env_upstream"] != "true" || out["env_anthropic"] != "content" || out["env_oi"] != "false" || out["enabled"] != true {
		t.Fatalf("out: %v", out)
	}
	d := out["diag"].(map[string]any)
	if path(d, "content", "anthropic") != "content" || path(d, "content", "upstream") != "SPAN_ONLY" {
		t.Fatalf("content: %v", d["content"])
	}
	// Preserve values already set by the application.
	out = runScenario(t, "content_env_fanout", map[string]string{"ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE": "metadata_only"})
	if out["env_anthropic"] != "metadata_only" {
		t.Fatalf("user env overwritten: %v", out)
	}
}

func TestPropagatorAndBaggage(t *testing.T) {
	out := runScenario(t, "propagator", nil)
	if out["traceparent"] == nil {
		t.Fatalf("W3C propagator not installed: %v", out)
	}
	bag, _ := out["baggage"].(string)
	if !strings.Contains(bag, "gen_ai.conversation.id=c-1") || !strings.Contains(bag, "user.id=u-1") {
		t.Fatalf("baggage: %q", bag)
	}
}

// Verify that baggage is off by default without relying on the global provider.
func TestSessionNoBaggageByDefault(t *testing.T) {
	ctx := WithSession(context.Background(), Session{ConversationID: "c", UserID: "u"})
	if n := baggage.FromContext(ctx).Len(); n != 0 {
		t.Fatalf("baggage should be empty, got %d members", n)
	}
}
