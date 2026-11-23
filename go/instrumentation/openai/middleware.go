// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Modifications Copyright 2026 Flashcat Cloud.
// Ported from open-telemetry/opentelemetry-go-compile-instrumentation,
// instrumentation/github.com/openai/openai-go/v3/middleware.go. See UPSTREAM.md for the baseline and changes.

// Package flashcatopenai provides OTel GenAI middleware for github.com/openai/openai-go/v3.
//
//	client := openai.NewClient(option.WithMiddleware(flashcatopenai.Middleware()))
//
// It traces chat completions, embeddings, legacy completions, and the Responses API, including streaming (SSE) and non-streaming calls.
// OpenAI-compatible providers such as DeepSeek, Qwen, and Zhipu can use the same middleware with a custom base URL.
// The middleware infers gen_ai.provider.name from the hostname.
package flashcatopenai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	flashcatgenai "github.com/flashcatcloud/flashcat-genai-otel/go"
	"github.com/openai/openai-go/v3/option"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// Version is the instrumentation module version and is used as the scope version on spans.
const Version = "0.1.0"

// ScopeName is the instrumentation scope name for spans and metrics created by this module.
const ScopeName = "github.com/flashcatcloud/flashcat-genai-otel/go/instrumentation/openai"

const (
	maxRequestBodySize  = 1 << 20 // 1 MB
	maxResponseBodySize = 4 << 20 // 4 MB
	targetModule        = "github.com/openai/openai-go/v3"
)

// providerMapping uses a slice so the first keyword found in the request hostname wins deterministically.
// Prefer known values from the GenAI semantic conventions. dashscope matches the Python DashScope instrumentation.
var providerMapping = []struct {
	keyword  string
	provider string
}{
	{"openai.com", "openai"},
	{"azure.com", "azure.ai.openai"},
	{"anthropic.com", "anthropic"},
	{"dashscope.aliyuncs", "dashscope"},
	{"volces.com", "ark"},
	{"ark.cn", "ark"},
	{"hunyuan", "tencent"},
	{"tencentcloudapi", "tencent"},
	{"googleapis.com", "gcp.gen_ai"},
	{"generativelanguage", "gcp.gen_ai"},
	{"deepseek.com", "deepseek"},
	{"moonshot", "moonshot_ai"},
	{"zhipuai.cn", "zhipu"},
	{"bigmodel.cn", "zhipu"},
	{"baidu.com", "baidu"},
	{"minimax", "minimax"},
	{"siliconflow", "siliconflow"},
	{"together", "together"},
	{"mistral", "mistral"},
	{"groq.com", "groq"},
	{"ollama", "ollama"},
	{"localhost", "local"},
	{"127.0.0.1", "local"},
}

// providerName infers gen_ai.provider.name from the request hostname and falls back to "openai".
func providerName(host string) string {
	host = strings.ToLower(host)
	for _, entry := range providerMapping {
		if strings.Contains(host, entry.keyword) {
			return entry.provider
		}
	}
	return "openai"
}

type operationType int

const (
	opChat operationType = iota
	opCompletion
	opEmbedding
	opResponses
	opUnknown
)

// classifyOperation checks POST requests only. GET /chat/completions/{id}, GET /responses/{id}, and similar requests are not inference calls.
func classifyOperation(method, path string) operationType {
	if method != http.MethodPost {
		return opUnknown
	}
	p := strings.TrimSuffix(path, "/")
	switch {
	case strings.HasSuffix(p, "chat/completions"):
		return opChat
	case strings.HasSuffix(p, "completions"):
		return opCompletion
	case strings.HasSuffix(p, "embeddings"):
		return opEmbedding
	case p == "responses" || strings.HasSuffix(p, "/responses"):
		return opResponses
	}
	return opUnknown
}

func operationName(op operationType) string {
	switch op {
	case opChat, opResponses:
		return "chat"
	case opCompletion:
		return "text_completion"
	case opEmbedding:
		return "embeddings"
	}
	return ""
}

func apiType(op operationType) string {
	switch op {
	case opChat:
		return "chat_completions"
	case opResponses:
		return "responses"
	}
	return ""
}

// readBounded reads up to limit bytes for attribute extraction and preserves the full stream for the SDK or caller.
// If the stream exceeds limit, it returns truncated=true and bodyBytes=nil to prevent parsing a partial JSON prefix.
func readBounded(body io.ReadCloser, limit int64) ([]byte, bool, io.ReadCloser, error) {
	var buf bytes.Buffer
	tee := io.TeeReader(body, &buf)
	read, err := io.ReadAll(io.LimitReader(tee, limit+1))
	reassembled := struct {
		io.Reader
		io.Closer
	}{io.MultiReader(&buf, body), body}
	if int64(len(read)) > limit {
		return nil, true, reassembled, err
	}
	return read, false, reassembled, err
}

// httpServerEndpoint returns the hostname and port, inferring the port from the scheme if omitted. Ported from upstream pkg/netutil.
func httpServerEndpoint(u *url.URL) (string, int) {
	if u == nil || u.Hostname() == "" {
		return "", 0
	}
	if p := u.Port(); p != "" {
		port, err := strconv.ParseUint(p, 10, 16)
		if err != nil || port == 0 {
			return u.Hostname(), 0
		}
		return u.Hostname(), int(port)
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return u.Hostname(), 80
	case "https":
		return u.Hostname(), 443
	}
	return u.Hostname(), 0
}

type middleware struct {
	cfg      config
	duration metric.Float64Histogram
}

// Middleware returns middleware for openai-go v3's option.WithMiddleware. The first call registers it with the core SDK,
// which lists it in diagnostics as {"lib":"openai","source":"flashcat","status":"installed"}.
// It can be created before flashcatgenai.Init because it reads the provider and content capture switch on each request.
func Middleware(opts ...Option) option.Middleware {
	m := &middleware{}
	for _, o := range opts {
		if o != nil {
			o(&m.cfg)
		}
	}
	flashcatgenai.RegisterInstrumentation("openai", targetModule)
	mp := m.cfg.meterProvider
	if mp == nil {
		mp = otel.GetMeterProvider() // The global MeterProvider hands instruments created before Init to the provider installed later.
	}
	meter := mp.Meter(ScopeName, metric.WithInstrumentationVersion(Version))
	if h, err := meter.Float64Histogram("gen_ai.client.operation.duration",
		metric.WithDescription("Duration of GenAI client operations."), metric.WithUnit("s")); err == nil {
		m.duration = h
	}
	return m.handle
}

func (m *middleware) tracer() trace.Tracer {
	tp := m.cfg.tracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version))
}

func (m *middleware) capture() bool {
	if m.cfg.captureContent != nil {
		return *m.cfg.captureContent
	}
	return flashcatgenai.ContentCaptureEnabled()
}

// requestInfo holds data parsed from the request body.
type requestInfo struct {
	model              string
	stream             bool
	attrs              []attribute.KeyValue
	inputMessages      string
	systemInstructions string
}

func (m *middleware) handle(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	if req.Body == nil || req.URL == nil {
		return next(req)
	}
	op := classifyOperation(req.Method, req.URL.Path)
	if op == opUnknown {
		return next(req)
	}

	start := time.Now()
	provider := providerName(req.URL.Host)
	opName := operationName(op)

	// Read a bounded copy for attribute extraction and pass the full request body to the SDK.
	bodyBytes, truncated, reassembled, err := readBounded(req.Body, maxRequestBodySize)
	req.Body = reassembled
	if err != nil {
		return next(req)
	}
	capture := m.capture()
	var info requestInfo
	if !truncated {
		info = parseRequest(op, bodyBytes, capture)
		if info.model == "" {
			return next(req)
		}
	}

	spanName := opName
	baseAttrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", opName),
		attribute.String("gen_ai.provider.name", provider),
	}
	if info.model != "" {
		spanName = opName + " " + info.model
		baseAttrs = append(baseAttrs, attribute.String("gen_ai.request.model", info.model))
	}
	if addr, port := httpServerEndpoint(req.URL); addr != "" && port > 0 {
		baseAttrs = append(baseAttrs, attribute.String("server.address", addr), attribute.Int("server.port", port))
	}
	spanAttrs := slices.Concat(baseAttrs, info.attrs)
	if t := apiType(op); t != "" {
		spanAttrs = append(spanAttrs, attribute.String("openai.api.type", t))
	}
	if info.inputMessages != "" {
		spanAttrs = append(spanAttrs, attribute.String("gen_ai.input.messages", info.inputMessages))
	}
	if info.systemInstructions != "" {
		spanAttrs = append(spanAttrs, attribute.String("gen_ai.system_instructions", info.systemInstructions))
	}

	ctx, span := m.tracer().Start(req.Context(), spanName, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(spanAttrs...))
	req = req.WithContext(ctx)

	record := func(errAttrs []attribute.KeyValue) {
		if m.duration != nil {
			m.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(slices.Concat(baseAttrs, errAttrs)...))
		}
	}

	resp, err := next(req)
	if err != nil {
		errType := semconv.ErrorType(err)
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		span.SetAttributes(errType)
		span.End()
		record([]attribute.KeyValue{errType})
		return resp, err
	}
	if resp.StatusCode >= 400 {
		errType := attribute.String("error.type", strconv.Itoa(resp.StatusCode))
		span.RecordError(errors.New(resp.Status))
		span.SetStatus(codes.Error, resp.Status)
		span.SetAttributes(errType)
		span.End()
		record([]attribute.KeyValue{errType})
		return resp, nil
	}

	contentType := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	if strings.EqualFold(contentType, "text/event-stream") && resp.Body != nil {
		span.SetAttributes(attribute.Bool("gen_ai.request.stream", true))
		resp.Body = newStreamingReader(resp.Body, span, start, op, capture, func(errAttrs []attribute.KeyValue) { record(errAttrs) })
		return resp, nil
	}
	handleNonStreamingResponse(resp, span, op, capture)
	record(nil)
	return resp, nil
}

func parseRequest(op operationType, body []byte, capture bool) requestInfo {
	switch op {
	case opChat:
		return parseChatRequest(body, capture)
	case opCompletion:
		return parseCompletionRequest(body, capture)
	case opEmbedding:
		var req struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &req) != nil {
			return requestInfo{}
		}
		return requestInfo{model: req.Model}
	case opResponses:
		return parseResponsesRequest(body, capture)
	}
	return requestInfo{}
}

type samplingParams struct {
	Model               string   `json:"model"`
	MaxTokens           *int64   `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64   `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	FrequencyPenalty    *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64 `json:"presence_penalty,omitempty"`
	Stream              bool     `json:"stream,omitempty"`
}

func (p samplingParams) attrs() []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if p.MaxCompletionTokens != nil {
		attrs = append(attrs, attribute.Int64("gen_ai.request.max_tokens", *p.MaxCompletionTokens))
	} else if p.MaxTokens != nil {
		attrs = append(attrs, attribute.Int64("gen_ai.request.max_tokens", *p.MaxTokens))
	}
	if p.Temperature != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.request.temperature", *p.Temperature))
	}
	if p.TopP != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.request.top_p", *p.TopP))
	}
	if p.FrequencyPenalty != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.request.frequency_penalty", *p.FrequencyPenalty))
	}
	if p.PresencePenalty != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.request.presence_penalty", *p.PresencePenalty))
	}
	return attrs
}

func parseChatRequest(body []byte, capture bool) requestInfo {
	var p samplingParams
	if json.Unmarshal(body, &p) != nil {
		return requestInfo{}
	}
	info := requestInfo{model: p.Model, stream: p.Stream, attrs: p.attrs()}
	if capture {
		info.inputMessages = marshalMessages(chatInputMessages(body))
	}
	return info
}

func parseCompletionRequest(body []byte, capture bool) requestInfo {
	var p samplingParams
	if json.Unmarshal(body, &p) != nil {
		return requestInfo{}
	}
	p.MaxCompletionTokens = nil
	info := requestInfo{model: p.Model, stream: p.Stream, attrs: p.attrs()}
	if capture {
		info.inputMessages = marshalMessages(completionInputMessages(body))
	}
	return info
}

func handleNonStreamingResponse(resp *http.Response, span trace.Span, op operationType, capture bool) {
	defer span.End()
	if resp.Body == nil {
		return
	}
	bodyBytes, truncated, reassembled, err := readBounded(resp.Body, maxResponseBodySize)
	resp.Body = reassembled
	if err != nil || truncated {
		return
	}
	switch op {
	case opChat, opCompletion:
		parseCompletionsResponse(bodyBytes, span, op, capture)
	case opEmbedding:
		parseEmbeddingResponse(bodyBytes, span)
	case opResponses:
		var b responsesBody
		if json.Unmarshal(bodyBytes, &b) == nil {
			applyResponsesBody(span, &b, capture)
		}
	}
}

type completionsUsage struct {
	PromptTokens        *int64          `json:"prompt_tokens"`
	CompletionTokens    *int64          `json:"completion_tokens"`
	TotalTokens         *int64          `json:"total_tokens"`
	PromptTokensDetails json.RawMessage `json:"prompt_tokens_details"`
}

func (u *completionsUsage) attrs() []attribute.KeyValue {
	if u == nil {
		return nil
	}
	var attrs []attribute.KeyValue
	if u.PromptTokens != nil {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.input_tokens", *u.PromptTokens))
	}
	if u.CompletionTokens != nil {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.output_tokens", *u.CompletionTokens))
	}
	if u.TotalTokens != nil {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.total_tokens", *u.TotalTokens))
	}
	// OpenAI's prompt_tokens already includes cached tokens, so adding them to input_tokens would count them twice.
	if cached := cachedPromptTokens(u.PromptTokensDetails); cached > 0 {
		attrs = append(attrs, attribute.Int64("gen_ai.usage.cache_read.input_tokens", cached))
	}
	return attrs
}

// cachedPromptTokens returns usage.prompt_tokens_details.cached_tokens, or zero if the field is absent or malformed.
func cachedPromptTokens(details json.RawMessage) int64 {
	var d struct {
		CachedTokens int64 `json:"cached_tokens"`
	}
	if len(details) == 0 || json.Unmarshal(details, &d) != nil {
		return 0
	}
	return d.CachedTokens
}

func parseCompletionsResponse(body []byte, span trace.Span, op operationType, capture bool) {
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *completionsUsage `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return
	}
	var reasons []string
	for _, c := range resp.Choices {
		if c.FinishReason != "" {
			reasons = append(reasons, c.FinishReason)
		}
	}
	attrs := []attribute.KeyValue{}
	if resp.ID != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.id", resp.ID))
	}
	if resp.Model != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.model", resp.Model))
	}
	if len(reasons) > 0 {
		attrs = append(attrs, attribute.StringSlice("gen_ai.response.finish_reasons", reasons))
	}
	attrs = append(attrs, resp.Usage.attrs()...)
	if capture {
		var msgs []message
		if op == opChat {
			msgs = chatOutputMessages(body)
		} else {
			msgs = completionOutputMessages(body)
		}
		if out := marshalMessages(msgs); out != "" {
			attrs = append(attrs, attribute.String("gen_ai.output.messages", out))
		}
	}
	span.SetAttributes(attrs...)
}

func parseEmbeddingResponse(body []byte, span trace.Span) {
	var resp struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens *int64 `json:"prompt_tokens"`
			TotalTokens  *int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return
	}
	var attrs []attribute.KeyValue
	if resp.Model != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.model", resp.Model))
	}
	if u := resp.Usage; u != nil {
		if u.PromptTokens != nil {
			attrs = append(attrs, attribute.Int64("gen_ai.usage.input_tokens", *u.PromptTokens))
		}
		if u.TotalTokens != nil {
			attrs = append(attrs, attribute.Int64("gen_ai.usage.total_tokens", *u.TotalTokens))
		}
	}
	span.SetAttributes(attrs...)
}
