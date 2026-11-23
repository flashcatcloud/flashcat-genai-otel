// Package flashcateino provides OTel GenAI callbacks for CloudWeGo Eino.
//
//	callbacks.AppendGlobalHandlers(flashcateino.NewHandler()) // Register globally once at startup.
//	// Or instrument a single call: runnable.Invoke(ctx, in, compose.WithCallbacks(flashcateino.NewHandler()))
//
// ChatModel maps to chat, Tool to execute_tool, and Graph, Chain, and Workflow to invoke_workflow.
// ADK Agent maps to invoke_agent, Retriever to retrieval, and Embedding to embeddings. Other components
// (such as Lambda, ChatTemplate, and ToolsNode) produce no spans. The current span in the caller's context
// is the parent. The core SDK's session processor adds session attributes.
package flashcateino

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	flashcatgenai "github.com/flashcatcloud/flashcat-genai-otel/go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// Version is the version of this instrumentation module.
const Version = "0.1.0"

// ScopeName is the instrumentation scope name for spans created by this module.
const ScopeName = "github.com/flashcatcloud/flashcat-genai-otel/go/instrumentation/eino"

const (
	targetModule    = "github.com/cloudwego/eino"
	contentLimit    = 32 * 1024
	truncatedSuffix = "…[truncated]"
)

// Match the compose constants by string value because their type is unexported.
const (
	componentGraph    = "Graph"
	componentChain    = "Chain"
	componentWorkflow = "Workflow"
)

// Option configures NewHandler.
type Option func(*handler)

// WithTracerProvider sets the TracerProvider for new spans. By default, each callback uses the current global provider.
func WithTracerProvider(tp trace.TracerProvider) Option { return func(h *handler) { h.tp = tp } }

// WithCaptureContent overrides the content capture switch, which defaults to flashcatgenai.ContentCaptureEnabled().
func WithCaptureContent(enabled bool) Option { return func(h *handler) { h.capture = &enabled } }

type handler struct {
	tp      trace.TracerProvider
	capture *bool
}

var (
	_ callbacks.Handler       = (*handler)(nil)
	_ callbacks.TimingChecker = (*handler)(nil)
)

// NewHandler returns an Eino callbacks.Handler. The first call registers the instrumentation with the core SDK,
// which lists it in diagnostics as {"lib":"eino","source":"flashcat","status":"installed"}.
func NewHandler(opts ...Option) callbacks.Handler {
	h := &handler{}
	for _, o := range opts {
		if o != nil {
			o(h)
		}
	}
	flashcatgenai.RegisterInstrumentation("eino", targetModule)
	return h
}

type stateKey struct{}

// spanState is stored in the context returned by the handler so OnEnd and OnError can end the span.
// Components without a span store nil to prevent callbacks from ending an enclosing component's span.
type spanState struct {
	span  trace.Span
	kind  string
	start time.Time
}

func (h *handler) tracer() trace.Tracer {
	tp := h.tp
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version))
}

func (h *handler) captureOn() bool {
	if h.capture != nil {
		return *h.capture
	}
	return flashcatgenai.ContentCaptureEnabled()
}

// kindOf maps a component to a GenAI operation. An empty result means the component produces no span.
func kindOf(info *callbacks.RunInfo) string {
	if info == nil {
		return ""
	}
	switch string(info.Component) {
	case string(components.ComponentOfChatModel):
		return "chat"
	case string(components.ComponentOfTool):
		return "execute_tool"
	case componentGraph, componentChain, componentWorkflow:
		return "invoke_workflow"
	case string(adk.ComponentOfAgent):
		return "invoke_agent"
	case string(components.ComponentOfRetriever):
		return "retrieval"
	case string(components.ComponentOfEmbedding):
		return "embeddings"
	}
	return ""
}

func displayName(info *callbacks.RunInfo) string {
	for _, s := range []string{info.Name, info.Type, string(info.Component)} {
		if s != "" {
			return s
		}
	}
	return "eino"
}

// providerOf infers gen_ai.provider.name from the component implementation name (RunInfo.Type, such as "OpenAI" or "Ark").
func providerOf(typ string) string {
	t := strings.ToLower(typ)
	switch {
	case t == "":
		return "eino"
	case strings.Contains(t, "openai"):
		return "openai"
	case strings.Contains(t, "deepseek"):
		return "deepseek"
	case strings.Contains(t, "claude"), strings.Contains(t, "anthropic"):
		return "anthropic"
	case strings.Contains(t, "qwen"), strings.Contains(t, "dashscope"):
		return "dashscope"
	case strings.Contains(t, "gemini"):
		return "gcp.gemini"
	case strings.Contains(t, "ark"):
		return "ark"
	case strings.Contains(t, "ollama"):
		return "ollama"
	}
	return t
}

func truncateText(s string) string {
	if len(s) <= contentLimit {
		return s
	}
	cut := contentLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedSuffix
}

// Needed implements callbacks.TimingChecker. It selects only components that produce spans so Eino can skip copying other components' streams.
func (h *handler) Needed(_ context.Context, info *callbacks.RunInfo, _ callbacks.CallbackTiming) bool {
	return kindOf(info) != ""
}

func (h *handler) startSpan(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
	kind := kindOf(info)
	if kind == "" {
		return context.WithValue(ctx, stateKey{}, (*spanState)(nil))
	}
	name := displayName(info)
	capture := h.captureOn()
	attrs := []attribute.KeyValue{attribute.String("gen_ai.operation.name", kind)}
	spanKind := trace.SpanKindInternal
	spanName := kind + " " + name
	switch kind {
	case "chat":
		spanKind = trace.SpanKindClient
		attrs = append(attrs, attribute.String("gen_ai.provider.name", providerOf(info.Type)))
		spanName = kind
		if in := model.ConvCallbackInput(input); in != nil {
			if in.Config != nil {
				if in.Config.Model != "" {
					attrs = append(attrs, attribute.String("gen_ai.request.model", in.Config.Model))
					spanName = kind + " " + in.Config.Model
				}
				if in.Config.MaxTokens > 0 {
					attrs = append(attrs, attribute.Int("gen_ai.request.max_tokens", in.Config.MaxTokens))
				}
				if in.Config.Temperature != 0 {
					attrs = append(attrs, attribute.Float64("gen_ai.request.temperature", float64(in.Config.Temperature)))
				}
				if in.Config.TopP != 0 {
					attrs = append(attrs, attribute.Float64("gen_ai.request.top_p", float64(in.Config.TopP)))
				}
			}
			if capture {
				if s := inputMessagesJSON(in.Messages); s != "" {
					attrs = append(attrs, attribute.String("gen_ai.input.messages", s))
				}
			}
		}
	case "execute_tool":
		attrs = append(attrs, attribute.String("gen_ai.tool.name", name), attribute.String("gen_ai.tool.type", "function"))
		if in := tool.ConvCallbackInput(input); in != nil && capture && in.ArgumentsInJSON != "" {
			attrs = append(attrs, attribute.String("gen_ai.tool.call.arguments", truncateText(in.ArgumentsInJSON)))
		}
	case "invoke_workflow":
		attrs = append(attrs, attribute.String("gen_ai.workflow.name", name))
	case "invoke_agent":
		attrs = append(attrs, attribute.String("gen_ai.agent.name", name))
	case "retrieval":
		attrs = append(attrs, attribute.String("gen_ai.data_source.id", name))
		if in := retriever.ConvCallbackInput(input); in != nil {
			if in.TopK > 0 {
				attrs = append(attrs, attribute.Int("gen_ai.retrieval.top_k", in.TopK))
			}
			if capture && in.Query != "" {
				attrs = append(attrs, attribute.String("gen_ai.retrieval.query.text", truncateText(in.Query)))
			}
		}
	case "embeddings":
		spanKind = trace.SpanKindClient
		spanName = kind
		attrs = append(attrs, attribute.String("gen_ai.provider.name", providerOf(info.Type)))
		if in := embedding.ConvCallbackInput(input); in != nil && in.Config != nil && in.Config.Model != "" {
			attrs = append(attrs, attribute.String("gen_ai.request.model", in.Config.Model))
			spanName = kind + " " + in.Config.Model
		}
	}
	ctx, span := h.tracer().Start(ctx, spanName, trace.WithSpanKind(spanKind), trace.WithAttributes(attrs...))
	return context.WithValue(ctx, stateKey{}, &spanState{span: span, kind: kind, start: time.Now()})
}

func stateFrom(ctx context.Context) *spanState {
	st, _ := ctx.Value(stateKey{}).(*spanState)
	return st
}

// OnStart creates a span when a component starts.
func (h *handler) OnStart(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) (out context.Context) {
	defer func() {
		if recover() != nil { // Keep SDK failures from affecting application code.
			out = context.WithValue(ctx, stateKey{}, (*spanState)(nil))
		}
	}()
	return h.startSpan(ctx, info, input)
}

// OnStartWithStreamInput creates a span when a component starts with streaming input. It closes the input stream without reading it.
func (h *handler) OnStartWithStreamInput(ctx context.Context, info *callbacks.RunInfo, input *schema.StreamReader[callbacks.CallbackInput]) (out context.Context) {
	if input != nil {
		input.Close()
	}
	defer func() {
		if recover() != nil {
			out = context.WithValue(ctx, stateKey{}, (*spanState)(nil))
		}
	}()
	return h.startSpan(ctx, info, nil)
}

// OnEnd writes output attributes and ends the span when the component succeeds.
// For an ADK Agent, OnEnd fires at startup and receives an event iterator. It drains its copy in the background before ending the span.
func (h *handler) OnEnd(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	st := stateFrom(ctx)
	if st == nil || st.span == nil {
		return ctx
	}
	if out := adk.ConvAgentCallbackOutput(output); out != nil && out.Events != nil {
		go drainAgentEvents(st, out.Events)
		return ctx
	}
	func() {
		defer func() { _ = recover() }()
		h.applyOutput(st, output)
	}()
	st.span.End()
	return ctx
}

// OnError marks the span as ERROR and ends it when a component fails.
func (h *handler) OnError(ctx context.Context, _ *callbacks.RunInfo, err error) context.Context {
	st := stateFrom(ctx)
	if st == nil || st.span == nil {
		return ctx
	}
	func() {
		defer func() { _ = recover() }()
		if err != nil {
			st.span.RecordError(err)
			st.span.SetStatus(codes.Error, err.Error())
			st.span.SetAttributes(semconv.ErrorType(err))
		}
	}()
	st.span.End()
	return ctx
}

// OnEndWithStreamOutput drains and closes the callback's copy of the output stream in the background.
// It accumulates output and usage, records time to first chunk, and ends the span.
func (h *handler) OnEndWithStreamOutput(ctx context.Context, _ *callbacks.RunInfo, output *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	st := stateFrom(ctx)
	if output == nil {
		if st != nil && st.span != nil {
			st.span.End()
		}
		return ctx
	}
	if st == nil || st.span == nil {
		output.Close() // Close the stream copy so Eino can release the original stream.
		return ctx
	}
	go h.drain(st, output)
	return ctx
}

func (h *handler) drain(st *spanState, output *schema.StreamReader[callbacks.CallbackOutput]) {
	defer output.Close()
	defer st.span.End()
	defer func() { _ = recover() }()
	var first time.Time
	var msgs []*schema.Message
	var last callbacks.CallbackOutput
	var usage *usageInfo
	for {
		chunk, err := output.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			st.span.RecordError(err)
			st.span.SetStatus(codes.Error, err.Error())
			st.span.SetAttributes(semconv.ErrorType(err))
			break
		}
		last = chunk
		if st.kind != "chat" {
			continue
		}
		if out := model.ConvCallbackOutput(chunk); out != nil {
			if out.Message != nil {
				if first.IsZero() && (out.Message.Content != "" || len(out.Message.ToolCalls) > 0 || out.Message.ReasoningContent != "") {
					first = time.Now()
				}
				msgs = append(msgs, out.Message)
			}
			if u := usageOf(out); u != nil {
				usage = u
			}
		}
	}
	if st.kind == "chat" {
		if !first.IsZero() {
			st.span.SetAttributes(attribute.Float64("gen_ai.response.time_to_first_chunk", first.Sub(st.start).Seconds()))
		}
		var merged *schema.Message
		if len(msgs) > 0 {
			if m, err := schema.ConcatMessages(msgs); err == nil {
				merged = m
			}
		}
		h.applyChat(st, merged, usage, "")
		return
	}
	h.applyOutput(st, last)
}

func drainAgentEvents(st *spanState, events *adk.AsyncIterator[*adk.AgentEvent]) {
	defer st.span.End()
	var lastErr error
	for {
		ev, ok := events.Next()
		if !ok {
			break
		}
		if ev != nil && ev.Err != nil {
			lastErr = ev.Err
		}
	}
	if lastErr != nil {
		func() {
			defer func() { _ = recover() }()
			st.span.RecordError(lastErr)
			st.span.SetStatus(codes.Error, lastErr.Error())
			st.span.SetAttributes(semconv.ErrorType(lastErr))
		}()
	}
}

type usageInfo struct{ in, out, total, cached int }

func usageOf(out *model.CallbackOutput) *usageInfo {
	if out.TokenUsage != nil {
		return &usageInfo{in: out.TokenUsage.PromptTokens, out: out.TokenUsage.CompletionTokens, total: out.TokenUsage.TotalTokens,
			cached: out.TokenUsage.PromptTokenDetails.CachedTokens}
	}
	if out.Message != nil && out.Message.ResponseMeta != nil && out.Message.ResponseMeta.Usage != nil {
		u := out.Message.ResponseMeta.Usage
		return &usageInfo{in: u.PromptTokens, out: u.CompletionTokens, total: u.TotalTokens, cached: u.PromptTokenDetails.CachedTokens}
	}
	return nil
}

func (h *handler) applyOutput(st *spanState, output callbacks.CallbackOutput) {
	capture := h.captureOn()
	switch st.kind {
	case "chat":
		if out := model.ConvCallbackOutput(output); out != nil {
			respModel := ""
			if out.Config != nil {
				respModel = out.Config.Model
			}
			h.applyChat(st, out.Message, usageOf(out), respModel)
		}
	case "execute_tool":
		if out := tool.ConvCallbackOutput(output); out != nil && capture && out.Response != "" {
			st.span.SetAttributes(attribute.String("gen_ai.tool.call.result", truncateText(out.Response)))
		}
	case "embeddings":
		if out := embedding.ConvCallbackOutput(output); out != nil && out.TokenUsage != nil {
			st.span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", out.TokenUsage.PromptTokens))
		}
	case "retrieval":
		if out := retriever.ConvCallbackOutput(output); out != nil && capture && len(out.Docs) > 0 {
			type doc struct {
				ID    string  `json:"id"`
				Score float64 `json:"score"`
			}
			docs := make([]doc, 0, len(out.Docs))
			for _, d := range out.Docs {
				if d != nil {
					docs = append(docs, doc{ID: d.ID, Score: d.Score()})
				}
			}
			if b, err := json.Marshal(docs); err == nil {
				st.span.SetAttributes(attribute.String("gen_ai.retrieval.documents", string(b)))
			}
		}
	}
}

func (h *handler) applyChat(st *spanState, msg *schema.Message, usage *usageInfo, respModel string) {
	var attrs []attribute.KeyValue
	if respModel != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.model", respModel))
	}
	if usage != nil {
		attrs = append(attrs, attribute.Int("gen_ai.usage.input_tokens", usage.in), attribute.Int("gen_ai.usage.output_tokens", usage.out))
		if usage.total > 0 {
			attrs = append(attrs, attribute.Int("gen_ai.usage.total_tokens", usage.total))
		}
		if usage.cached > 0 {
			attrs = append(attrs, attribute.Int("gen_ai.usage.cache_read.input_tokens", usage.cached))
		}
	}
	if msg != nil {
		if msg.ResponseMeta != nil && msg.ResponseMeta.FinishReason != "" {
			attrs = append(attrs, attribute.StringSlice("gen_ai.response.finish_reasons", []string{msg.ResponseMeta.FinishReason}))
		}
		if h.captureOn() {
			if s := outputMessageJSON(msg); s != "" {
				attrs = append(attrs, attribute.String("gen_ai.output.messages", s))
			}
		}
	}
	st.span.SetAttributes(attrs...)
}

// ---- Content JSON in the GenAI messages format ----

type part struct {
	Type      string `json:"type"`
	Content   string `json:"content,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments any    `json:"arguments,omitempty"`
	Response  any    `json:"response,omitempty"`
}

type message struct {
	Role         string `json:"role"`
	Parts        []part `json:"parts"`
	FinishReason string `json:"finish_reason,omitempty"`
}

func toolArguments(s string) any {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil
	}
	if len(t) <= contentLimit && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	return truncateText(s)
}

func convert(m *schema.Message) message {
	out := message{Role: string(m.Role), Parts: []part{}}
	if m.Role == schema.Tool {
		out.Parts = append(out.Parts, part{Type: "tool_call_response", ID: m.ToolCallID, Response: truncateText(m.Content)})
		return out
	}
	if m.Content != "" {
		out.Parts = append(out.Parts, part{Type: "text", Content: truncateText(m.Content)})
	}
	for _, tc := range m.ToolCalls {
		out.Parts = append(out.Parts, part{Type: "tool_call", ID: tc.ID, Name: tc.Function.Name, Arguments: toolArguments(tc.Function.Arguments)})
	}
	return out
}

func inputMessagesJSON(msgs []*schema.Message) string {
	out := make([]message, 0, len(msgs))
	for _, m := range msgs {
		if m != nil {
			out = append(out, convert(m))
		}
	}
	if len(out) == 0 {
		return ""
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

func outputMessageJSON(m *schema.Message) string {
	msg := convert(m)
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	if m.ResponseMeta != nil {
		msg.FinishReason = m.ResponseMeta.FinishReason
	}
	b, err := json.Marshal([]message{msg})
	if err != nil {
		return ""
	}
	return string(b)
}
