package flashcatopenai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	flashcatgenai "github.com/flashcatcloud/flashcat-genai-otel/go"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type harness struct {
	rec    *tracetest.SpanRecorder
	tp     *sdktrace.TracerProvider
	fake   *fakeOpenAI
	client openai.Client
}

func newHarness(t *testing.T, capture bool, extra ...Option) *harness {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	f := newFakeOpenAI(t)
	opts := append([]Option{WithTracerProvider(tp), WithCaptureContent(capture)}, extra...)
	c := openai.NewClient(
		option.WithBaseURL(f.baseURL()),
		option.WithAPIKey("sk-test"),
		option.WithMaxRetries(0),
		option.WithMiddleware(Middleware(opts...)),
	)
	return &harness{rec: rec, tp: tp, fake: f, client: c}
}

func (h *harness) only(t *testing.T) sdktrace.ReadOnlySpan {
	t.Helper()
	ended := h.rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 span, got %d (started %d)", len(ended), len(h.rec.Started()))
	}
	return ended[0]
}

func attrs(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func has(m map[string]attribute.Value, k string) bool { return m[k].Type() != attribute.INVALID }

func noContent(t *testing.T, m map[string]attribute.Value) {
	t.Helper()
	for _, k := range []string{"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions"} {
		if has(m, k) {
			t.Fatalf("content attribute %s present with capture off", k)
		}
	}
}

func chatParams() openai.ChatCompletionNewParams {
	return openai.ChatCompletionNewParams{
		Model:       "gpt-4o",
		Messages:    []openai.ChatCompletionMessageParamUnion{openai.SystemMessage("你是客服"), openai.UserMessage("我的订单到哪了")},
		Temperature: openai.Float(0.2),
		MaxTokens:   openai.Int(256),
	}
}

func TestChatNonStreaming(t *testing.T) {
	h := newHarness(t, false)
	resp, err := h.client.Chat.Completions.New(context.Background(), chatParams())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "您的订单已发货" {
		t.Fatalf("response body altered: %q", resp.Choices[0].Message.Content)
	}
	s := h.only(t)
	m := attrs(s)
	if s.Name() != "chat gpt-4o" || s.SpanKind() != trace.SpanKindClient || s.InstrumentationScope().Name != ScopeName {
		t.Fatalf("span: %s %v %v", s.Name(), s.SpanKind(), s.InstrumentationScope())
	}
	want := map[string]any{
		"gen_ai.operation.name": "chat", "gen_ai.provider.name": "local", "gen_ai.request.model": "gpt-4o",
		"gen_ai.response.id": "chatcmpl-test", "gen_ai.response.model": "gpt-4o-2024-08-06", "openai.api.type": "chat_completions",
		"server.address": "127.0.0.1",
	}
	for k, v := range want {
		if m[k].AsString() != v {
			t.Errorf("%s = %q, want %q", k, m[k].AsString(), v)
		}
	}
	if m["gen_ai.usage.input_tokens"].AsInt64() != 120 || m["gen_ai.usage.output_tokens"].AsInt64() != 12 ||
		m["gen_ai.usage.total_tokens"].AsInt64() != 132 || m["gen_ai.usage.cache_read.input_tokens"].AsInt64() != 100 {
		t.Fatalf("usage: %v", m)
	}
	if m["gen_ai.request.temperature"].AsFloat64() != 0.2 || m["gen_ai.request.max_tokens"].AsInt64() != 256 || m["server.port"].AsInt64() == 0 {
		t.Fatalf("request attrs: %v", m)
	}
	if strings.Join(m["gen_ai.response.finish_reasons"].AsStringSlice(), ",") != "stop" {
		t.Fatalf("finish reasons: %v", m["gen_ai.response.finish_reasons"])
	}
	if has(m, "gen_ai.system") || has(m, "gen_ai.request.stream") {
		t.Fatalf("unexpected attrs: %v", m)
	}
	noContent(t, m)
	if s.Status().Code == codes.Error {
		t.Fatal("status error")
	}
}

func TestChatNonStreamingContent(t *testing.T) {
	h := newHarness(t, true)
	if _, err := h.client.Chat.Completions.New(context.Background(), chatParams()); err != nil {
		t.Fatal(err)
	}
	m := attrs(h.only(t))
	in := m["gen_ai.input.messages"].AsString()
	wantIn := `[{"role":"system","parts":[{"type":"text","content":"你是客服"}]},{"role":"user","parts":[{"type":"text","content":"我的订单到哪了"}]}]`
	if in != wantIn {
		t.Fatalf("input messages:\n got  %s\n want %s", in, wantIn)
	}
	out := m["gen_ai.output.messages"].AsString()
	wantOut := `[{"role":"assistant","parts":[{"type":"text","content":"您的订单已发货"}],"finish_reason":"stop"}]`
	if out != wantOut {
		t.Fatalf("output messages:\n got  %s\n want %s", out, wantOut)
	}
	var v any
	if json.Unmarshal([]byte(in), &v) != nil || json.Unmarshal([]byte(out), &v) != nil {
		t.Fatal("content is not valid JSON")
	}
}

func TestContentFollowsCoreSwitchByDefault(t *testing.T) {
	// Without WithCaptureContent, the middleware uses flashcatgenai.ContentCaptureEnabled().
	// This process has not called Init, so the setting comes from the environment.
	t.Setenv("FLASHCAT_CAPTURE_CONTENT", "span")
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	f := newFakeOpenAI(t)
	c := openai.NewClient(option.WithBaseURL(f.baseURL()), option.WithAPIKey("sk"), option.WithMaxRetries(0),
		option.WithMiddleware(Middleware(WithTracerProvider(tp))))
	if _, err := c.Chat.Completions.New(context.Background(), chatParams()); err != nil {
		t.Fatal(err)
	}
	if !has(attrs(rec.Ended()[0]), "gen_ai.input.messages") {
		t.Fatal("content not captured with FLASHCAT_CAPTURE_CONTENT=span")
	}
	t.Setenv("FLASHCAT_CAPTURE_CONTENT", "false")
	if _, err := c.Chat.Completions.New(context.Background(), chatParams()); err != nil {
		t.Fatal(err)
	}
	noContent(t, attrs(rec.Ended()[1]))
}

func TestChatStreaming(t *testing.T) {
	h := newHarness(t, true)
	p := chatParams()
	p.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
	stream := h.client.Chat.Completions.NewStreaming(context.Background(), p)
	var acc openai.ChatCompletionAccumulator
	for stream.Next() {
		acc.AddChunk(stream.Current())
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if acc.Choices[0].Message.Content != "您的订单已发货" {
		t.Fatalf("client saw %q", acc.Choices[0].Message.Content)
	}
	s := h.only(t)
	m := attrs(s)
	if !m["gen_ai.request.stream"].AsBool() || m["gen_ai.usage.input_tokens"].AsInt64() != 20 || m["gen_ai.usage.output_tokens"].AsInt64() != 5 ||
		m["gen_ai.response.id"].AsString() != "chatcmpl-s" || strings.Join(m["gen_ai.response.finish_reasons"].AsStringSlice(), ",") != "stop" {
		t.Fatalf("stream attrs: %v", m)
	}
	if ttfc := m["gen_ai.response.time_to_first_chunk"]; ttfc.Type() != attribute.FLOAT64 || ttfc.AsFloat64() <= 0 {
		t.Fatalf("time_to_first_chunk: %v", ttfc)
	}
	if out := m["gen_ai.output.messages"].AsString(); out != `[{"role":"assistant","parts":[{"type":"text","content":"您的订单已发货"}],"finish_reason":"stop"}]` {
		t.Fatalf("stream output: %s", out)
	}
	if s.Status().Code == codes.Error {
		t.Fatalf("status: %v", s.Status())
	}
}

func TestChatStreamingWithoutUsageRecordsNoTokens(t *testing.T) {
	h := newHarness(t, false)
	h.fake.chatSSE = sse(
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)
	stream := h.client.Chat.Completions.NewStreaming(context.Background(), chatParams())
	for stream.Next() {
	}
	_ = stream.Close()
	m := attrs(h.only(t))
	if has(m, "gen_ai.usage.input_tokens") || has(m, "gen_ai.usage.output_tokens") {
		t.Fatalf("usage recorded without usage chunk: %v", m)
	}
	noContent(t, m)
}

func TestChatStreamEarlyClose(t *testing.T) {
	h := newHarness(t, false)
	stream := h.client.Chat.Completions.NewStreaming(context.Background(), chatParams())
	if !stream.Next() {
		t.Fatal("no first chunk")
	}
	_ = stream.Close()
	s := h.only(t)
	if s.Status().Code != codes.Error || attrs(s)["error.type"].AsString() != "stream_aborted" || s.EndTime().IsZero() {
		t.Fatalf("early close: status=%v attrs=%v", s.Status(), attrs(s))
	}
}

func TestChatStreamToolCalls(t *testing.T) {
	h := newHarness(t, true)
	h.fake.chatSSE = sse(
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup_order","arguments":""}}]}}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"order_id\":"}}]}}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"A-1\"}"}}]}}]}`,
		`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	stream := h.client.Chat.Completions.NewStreaming(context.Background(), chatParams())
	for stream.Next() {
	}
	_ = stream.Close()
	out := attrs(h.only(t))["gen_ai.output.messages"].AsString()
	want := `[{"role":"assistant","parts":[{"type":"tool_call","id":"call_1","name":"lookup_order","arguments":{"order_id":"A-1"}}],"finish_reason":"tool_calls"}]`
	if out != want {
		t.Fatalf("tool call output:\n got  %s\n want %s", out, want)
	}
}

func TestStreamWithoutTerminatorIsAborted(t *testing.T) {
	h := newHarness(t, false)
	h.fake.chatSSE = sse(`data: {"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`)
	stream := h.client.Chat.Completions.NewStreaming(context.Background(), chatParams())
	for stream.Next() {
	}
	_ = stream.Close()
	if s := h.only(t); s.Status().Code != codes.Error {
		t.Fatalf("stream without finish_reason/[DONE] should be ERROR: %v", s.Status())
	}
}

func TestEmbeddings(t *testing.T) {
	h := newHarness(t, true)
	_, err := h.client.Embeddings.New(context.Background(), openai.EmbeddingNewParams{
		Model: "text-embedding-3-small", Input: openai.EmbeddingNewParamsInputUnion{OfString: openai.String("手机13812345678")}})
	if err != nil {
		t.Fatal(err)
	}
	s := h.only(t)
	m := attrs(s)
	if s.Name() != "embeddings text-embedding-3-small" || m["gen_ai.operation.name"].AsString() != "embeddings" ||
		m["gen_ai.usage.input_tokens"].AsInt64() != 8 || m["gen_ai.response.model"].AsString() != "text-embedding-3-small" || has(m, "openai.api.type") {
		t.Fatalf("embeddings: %s %v", s.Name(), m)
	}
	noContent(t, m)
}

func TestLegacyCompletions(t *testing.T) {
	h := newHarness(t, true)
	_, err := h.client.Completions.New(context.Background(), openai.CompletionNewParams{
		Model: "gpt-3.5-turbo-instruct", Prompt: openai.CompletionNewParamsPromptUnion{OfString: openai.String("Say hello")}})
	if err != nil {
		t.Fatal(err)
	}
	m := attrs(h.only(t))
	if m["gen_ai.operation.name"].AsString() != "text_completion" || m["gen_ai.usage.output_tokens"].AsInt64() != 2 ||
		m["gen_ai.input.messages"].AsString() != `[{"role":"user","parts":[{"type":"text","content":"Say hello"}]}]` ||
		!strings.Contains(m["gen_ai.output.messages"].AsString(), "Hello world") {
		t.Fatalf("completion: %v", m)
	}

	h2 := newHarness(t, true)
	stream := h2.client.Completions.NewStreaming(context.Background(), openai.CompletionNewParams{
		Model: "gpt-3.5-turbo-instruct", Prompt: openai.CompletionNewParamsPromptUnion{OfString: openai.String("Say hello")}})
	for stream.Next() {
	}
	_ = stream.Close()
	s := h2.only(t)
	m = attrs(s)
	if !m["gen_ai.request.stream"].AsBool() || !strings.Contains(m["gen_ai.output.messages"].AsString(), "Hello world") ||
		strings.Join(m["gen_ai.response.finish_reasons"].AsStringSlice(), ",") != "length" || s.Status().Code == codes.Error {
		t.Fatalf("completion stream: %v %v", m, s.Status())
	}
}

func responsesParams() responses.ResponseNewParams {
	return responses.ResponseNewParams{
		Model:        "gpt-4.1",
		Instructions: openai.String("回答要简短"),
		Input:        responses.ResponseNewParamsInputUnion{OfString: openai.String("订单到哪了")},
	}
}

func TestResponsesNonStreaming(t *testing.T) {
	h := newHarness(t, true)
	r, err := h.client.Responses.New(context.Background(), responsesParams())
	if err != nil {
		t.Fatal(err)
	}
	if r.OutputText() != "简短回答" {
		t.Fatalf("client saw %q", r.OutputText())
	}
	s := h.only(t)
	m := attrs(s)
	if s.Name() != "chat gpt-4.1" || m["openai.api.type"].AsString() != "responses" || m["gen_ai.response.id"].AsString() != "resp_1" ||
		m["gen_ai.usage.input_tokens"].AsInt64() != 30 || m["gen_ai.usage.output_tokens"].AsInt64() != 4 ||
		strings.Join(m["gen_ai.response.finish_reasons"].AsStringSlice(), ",") != "stop" {
		t.Fatalf("responses: %s %v", s.Name(), m)
	}
	if m["gen_ai.system_instructions"].AsString() != `[{"type":"text","content":"回答要简短"}]` ||
		m["gen_ai.input.messages"].AsString() != `[{"role":"user","parts":[{"type":"text","content":"订单到哪了"}]}]` ||
		m["gen_ai.output.messages"].AsString() != `[{"role":"assistant","parts":[{"type":"text","content":"简短回答"}],"finish_reason":"stop"}]` {
		t.Fatalf("responses content: %v", m)
	}
}

func TestResponsesStreaming(t *testing.T) {
	h := newHarness(t, false)
	stream := h.client.Responses.NewStreaming(context.Background(), responsesParams())
	var text strings.Builder
	for stream.Next() {
		if ev := stream.Current(); ev.Type == "response.output_text.delta" {
			text.WriteString(ev.Delta)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if text.String() != "简短回答" {
		t.Fatalf("client saw %q", text.String())
	}
	s := h.only(t)
	m := attrs(s)
	if s.Status().Code == codes.Error {
		t.Fatalf("Responses stream (no [DONE]) marked error: %v %v", s.Status(), m)
	}
	if !m["gen_ai.request.stream"].AsBool() || m["gen_ai.response.id"].AsString() != "resp_s" || m["gen_ai.usage.input_tokens"].AsInt64() != 30 ||
		m["gen_ai.response.time_to_first_chunk"].AsFloat64() <= 0 {
		t.Fatalf("responses stream attrs: %v", m)
	}
	noContent(t, m)
}

func TestHTTPErrorStatus(t *testing.T) {
	h := newHarness(t, false)
	h.fake.status = 401
	_, err := h.client.Chat.Completions.New(context.Background(), chatParams())
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("client error: %v", err)
	}
	s := h.only(t)
	if s.Status().Code != codes.Error || attrs(s)["error.type"].AsString() != "401" {
		t.Fatalf("status=%v attrs=%v", s.Status(), attrs(s))
	}
}

func TestNetworkError(t *testing.T) {
	h := newHarness(t, false)
	h.fake.srv.Close()
	if _, err := h.client.Chat.Completions.New(context.Background(), chatParams()); err == nil {
		t.Fatal("expected error")
	}
	s := h.only(t)
	if s.Status().Code != codes.Error || !has(attrs(s), "error.type") {
		t.Fatalf("status=%v attrs=%v", s.Status(), attrs(s))
	}
}

func TestNonInferencePassThrough(t *testing.T) {
	h := newHarness(t, false)
	_, _ = h.client.Models.List(context.Background())
	if n := len(h.rec.Started()); n != 0 {
		t.Fatalf("GET /models produced %d spans", n)
	}
}

func TestProviderName(t *testing.T) {
	cases := map[string]string{
		"api.openai.com":                    "openai",
		"api.deepseek.com":                  "deepseek",
		"dashscope.aliyuncs.com":            "dashscope",
		"open.bigmodel.cn":                  "zhipu",
		"api.moonshot.cn":                   "moonshot_ai",
		"my-res.openai.azure.com":           "azure.ai.openai",
		"ark.cn-beijing.volces.com":         "ark",
		"llm-gateway.internal.example.com":  "openai",
		"127.0.0.1:8080":                    "local",
		"generativelanguage.googleapis.com": "gcp.gen_ai",
	}
	for host, want := range cases {
		if got := providerName(host); got != want {
			t.Errorf("%s → %s, want %s", host, got, want)
		}
	}
}

func TestDurationMetric(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	h := newHarness(t, false, WithMeterProvider(mp))
	if _, err := h.client.Chat.Completions.New(context.Background(), chatParams()); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "gen_ai.client.operation.duration" {
				if h, ok := m.Data.(metricdata.Histogram[float64]); ok && len(h.DataPoints) == 1 && h.DataPoints[0].Count == 1 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("duration histogram not recorded: %+v", rm)
	}
}

func TestRegisteredInDiagnostics(t *testing.T) {
	_ = Middleware()
	var got *flashcatgenai.InstrumentationStatus
	for _, it := range flashcatgenai.GetDiagnostics().Instrumentations {
		if it.Lib == "openai" {
			it := it
			got = &it
		}
	}
	// The test binary's build info has an empty debug.ReadBuildInfo().Deps. lib_version is available only in normally built binaries.
	if got == nil || got.Source != "flashcat" || got.Status != "installed" || (got.LibVersion != "" && !strings.HasPrefix(got.LibVersion, "v3.")) {
		t.Fatalf("registration: %+v", got)
	}
}

func TestParentSpanAndSession(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	// The core SDK's NewSpanProcessor only processes sessions when no endpoint is configured. Use it to verify session attributes.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(flashcatgenai.NewSpanProcessor()), sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	f := newFakeOpenAI(t)
	c := openai.NewClient(option.WithBaseURL(f.baseURL()), option.WithAPIKey("sk"), option.WithMaxRetries(0),
		option.WithMiddleware(Middleware(WithCaptureContent(false))))
	ctx := flashcatgenai.WithSession(context.Background(), flashcatgenai.Session{ConversationID: "c-123", UserID: "u-9"})
	err := flashcatgenai.Agent(ctx, "kefu", func(ctx context.Context) error {
		_, err := c.Chat.Completions.New(ctx, chatParams())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var chat, agent sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		switch s.Name() {
		case "chat gpt-4o":
			chat = s
		case "invoke_agent kefu":
			agent = s
		}
	}
	if chat == nil || agent == nil || chat.Parent().SpanID() != agent.SpanContext().SpanID() {
		t.Fatal("chat span is not a child of the agent span")
	}
	if m := attrs(chat); m["gen_ai.conversation.id"].AsString() != "c-123" || m["user.id"].AsString() != "u-9" {
		t.Fatalf("session attrs: %v", m)
	}
	if attrs(agent)["flashcat.genai.root"] != attribute.BoolValue(true) || attrs(chat)["flashcat.genai.root"] != attribute.BoolValue(false) {
		t.Fatalf("AI roots: agent=%v chat=%v", attrs(agent), attrs(chat))
	}
}

func TestParsersEdgeCases(t *testing.T) {
	in := chatInputMessages([]byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:..."}}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"42"}]}`))
	got := marshalMessages(in)
	want := `[{"role":"user","parts":[{"type":"text","content":"看图"}]},{"role":"assistant","parts":[{"type":"tool_call","id":"call_1","name":"f","arguments":{"a":1}}]},{"role":"tool","parts":[{"type":"tool_call_response","id":"call_1","response":"42"}]}]`
	if got != want {
		t.Fatalf("chat input:\n got  %s\n want %s", got, want)
	}
	items := responsesInputMessages(json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"not json"},{"type":"function_call_output","call_id":"c1","output":"ok"}]`))
	got = marshalMessages(items)
	want = `[{"role":"user","parts":[{"type":"text","content":"hi"}]},{"role":"assistant","parts":[{"type":"tool_call","id":"c1","name":"f","arguments":"not json"}]},{"role":"tool","parts":[{"type":"tool_call_response","id":"c1","response":"ok"}]}]`
	if got != want {
		t.Fatalf("responses input:\n got  %s\n want %s", got, want)
	}
	long := strings.Repeat("长", contentLimit)
	if tt := truncateText(long); len(tt) > contentLimit+len(truncatedSuffix) || !strings.HasSuffix(tt, truncatedSuffix) {
		t.Fatalf("truncate: len=%d", len(tt))
	}
	if classifyOperation("GET", "/v1/chat/completions") != opUnknown || classifyOperation("POST", "/v1/responses/compact") != opUnknown ||
		classifyOperation("POST", "/v1/responses") != opResponses || classifyOperation("POST", "/compatible-mode/v1/chat/completions") != opChat {
		t.Fatal("classify")
	}
}
