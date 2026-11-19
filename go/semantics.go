package flashcatotel

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// The semantic API (contract §4) provides Agent, Tool, Workflow, LLM, Embedding, Retrieval, and Trace. Attributes are set at span start.
// Content is written only when capture is enabled. Application errors are returned unchanged, and panics resume after the span ends.

const (
	maxSerialized = 32 * 1024 // Argument and return value serialization limit in characters, matching Python.
	truncatedMark = "…[truncated]"
)

type spanKind int

const (
	kindAgent spanKind = iota
	kindTool
	kindWorkflow
	kindTrace
	kindLLM
	kindEmbedding
	kindRetrieval
)

// SpanOption configures the semantic API.
type SpanOption func(*spanConfig)

type spanConfig struct {
	description string
	provider    string
}

// WithDescription sets gen_ai.agent.description for Agent or gen_ai.tool.description for Tool. Other span types ignore it.
func WithDescription(d string) SpanOption { return func(c *spanConfig) { c.description = d } }

// WithProvider sets gen_ai.provider.name for LLM or Embedding, for example openai or dashscope. Other span types ignore it.
func WithProvider(p string) SpanOption { return func(c *spanConfig) { c.provider = p } }

// frameKey stores the innermost span created by the semantic API for Annotate and nested inference detection.
type frameKey struct{}

func tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer(ScopeName, trace.WithInstrumentationVersion(Version))
}

func startAttrs(k spanKind, name string, cfg spanConfig) (string, []attribute.KeyValue) {
	switch k {
	case kindAgent:
		attrs := []attribute.KeyValue{attribute.String(attrOperationName, "invoke_agent"), attribute.String("gen_ai.agent.name", name)}
		if cfg.description != "" {
			attrs = append(attrs, attribute.String("gen_ai.agent.description", cfg.description))
		}
		return "invoke_agent " + name, attrs
	case kindTool:
		attrs := []attribute.KeyValue{attribute.String(attrOperationName, "execute_tool"), attribute.String("gen_ai.tool.name", name),
			attribute.String("gen_ai.tool.type", "function")}
		if cfg.description != "" {
			attrs = append(attrs, attribute.String("gen_ai.tool.description", cfg.description))
		}
		return "execute_tool " + name, attrs
	case kindWorkflow:
		return "invoke_workflow " + name, []attribute.KeyValue{attribute.String(attrOperationName, "invoke_workflow"), attribute.String("gen_ai.workflow.name", name)}
	case kindLLM, kindEmbedding:
		op := "chat"
		if k == kindEmbedding {
			op = "embeddings"
		}
		attrs := []attribute.KeyValue{attribute.String(attrOperationName, op), attribute.String("gen_ai.request.model", name)}
		if cfg.provider != "" {
			attrs = append(attrs, attribute.String("gen_ai.provider.name", cfg.provider))
		}
		return op + " " + name, attrs
	case kindRetrieval:
		return "retrieval " + name, []attribute.KeyValue{attribute.String(attrOperationName, "retrieval"), attribute.String("gen_ai.data_source.id", name)}
	}
	return name, nil
}

// Span is a handle for a semantic span with explicit start and end. Its methods are no-ops on a nil handle
// and do not panic on SDK failures.
type Span struct {
	span trace.Span
	kind spanKind
	once sync.Once
}

func start(ctx context.Context, k spanKind, name string, opts []SpanOption, extra []attribute.KeyValue) (rctx context.Context, sp *Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		if recover() != nil { // If span creation fails, the caller runs the original function directly.
			rctx, sp = ctx, nil
		}
	}()
	var cfg spanConfig
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	spanName, attrs := startAttrs(k, name, cfg)
	attrs = append(attrs, extra...)
	sk := trace.SpanKindInternal
	if k == kindLLM || k == kindEmbedding || k == kindRetrieval {
		sk = trace.SpanKindClient
	}
	c, s := tracer().Start(ctx, spanName, trace.WithSpanKind(sk), trace.WithAttributes(attrs...))
	sp = &Span{span: s, kind: k}
	if k != kindTrace {
		c = context.WithValue(c, frameKey{}, sp)
	}
	return c, sp
}

// StartAgent starts an invoke_agent span and returns a context carrying the span and a handle for streaming or cross-function use.
// Call (*Span).End when finished.
func StartAgent(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Span) {
	return start(ctx, kindAgent, name, opts, nil)
}

// StartTool starts an execute_tool span.
func StartTool(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Span) {
	return start(ctx, kindTool, name, opts, nil)
}

// StartWorkflow starts an invoke_workflow span.
func StartWorkflow(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Span) {
	return start(ctx, kindWorkflow, name, opts, nil)
}

// StartLLM starts a chat {model} span for an uninstrumented model call, such as a custom gateway or private SDK.
// Use (*Span).Annotate to add usage, the response model, and messages. Keep instrumented model calls outside this span
// to avoid counting the same call twice (contract §4.1).
func StartLLM(ctx context.Context, model string, opts ...SpanOption) (context.Context, *Span) {
	return start(ctx, kindLLM, model, opts, nil)
}

// StartEmbedding starts an embeddings {model} span and omits input content.
func StartEmbedding(ctx context.Context, model string, opts ...SpanOption) (context.Context, *Span) {
	return start(ctx, kindEmbedding, model, opts, nil)
}

// StartRetrieval starts a retrieval {name} span for a vector database or search engine.
func StartRetrieval(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Span) {
	return start(ctx, kindRetrieval, name, opts, nil)
}

// serialize converts a value to a bounded string. It preserves strings and tries JSON for other values, falling back to %v.
// It applies no special handling to nested values.
func serialize(v any) string {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		if b, err := json.Marshal(v); err == nil {
			s = string(b)
		} else {
			s = fmt.Sprintf("%v", v)
		}
	}
	if utf8.RuneCountInString(s) > maxSerialized {
		n := 0
		for i := range s {
			if n == maxSerialized {
				s = s[:i]
				break
			}
			n++
		}
		s += "…[truncated]"
	}
	return s
}

type msgPart struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type message struct {
	Role  string    `json:"role"`
	Parts []msgPart `json:"parts"`
}

// Message is a conversation message for Annotate. Non-string Content is serialized.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func messagesJSON(role, content string) string {
	b, err := json.Marshal([]message{{Role: role, Parts: []msgPart{{Type: "text", Content: content}}}})
	if err != nil {
		return "[]"
	}
	return string(b)
}

// messagesOf converts each element of a []Message or a []map[string]any with role keys to a message.
// It serializes other values as a single text message.
func messagesOf(v any, role string) string {
	var list []message
	add := func(r string, c any) {
		text, ok := c.(string)
		if !ok {
			text = serialize(c)
		}
		list = append(list, message{Role: r, Parts: []msgPart{{Type: "text", Content: text}}})
	}
	switch x := v.(type) {
	case []Message:
		for _, m := range x {
			add(m.Role, m.Content)
		}
	case []map[string]any:
		for _, m := range x {
			r, ok := m["role"].(string)
			if !ok {
				list = nil
				break
			}
			add(r, m["content"])
		}
	}
	if len(list) == 0 {
		return messagesJSON(role, serialize(v))
	}
	b, err := json.Marshal(list)
	if err != nil {
		return messagesJSON(role, serialize(v))
	}
	return string(b)
}

// documentsJSON builds the retrieval documents array. It preserves map elements and wraps other values as {"content": …}.
// If the array exceeds the limit, it drops items from the end and appends a truncation marker (contract §4).
func documentsJSON(v any) string {
	var items []any
	rv := reflect.ValueOf(v)
	if v != nil && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) && rv.Type().Elem().Kind() != reflect.Uint8 {
		for i := 0; i < rv.Len(); i++ {
			items = append(items, rv.Index(i).Interface())
		}
	} else {
		items = []any{v}
	}
	docs := make([]any, 0, len(items))
	for _, it := range items {
		if it != nil && reflect.TypeOf(it).Kind() == reflect.Map {
			docs = append(docs, it)
		} else {
			docs = append(docs, map[string]string{"content": serialize(it)})
		}
	}
	dump := func(ds []any) string {
		b, err := json.Marshal(ds)
		if err != nil {
			out := make([]map[string]string, len(ds))
			for i, d := range ds {
				out[i] = map[string]string{"content": serialize(d)}
			}
			b, _ = json.Marshal(out)
		}
		return string(b)
	}
	s := dump(docs)
	if utf8.RuneCountInString(s) > maxSerialized {
		for len(docs) > 0 && utf8.RuneCountInString(dump(docs)) > maxSerialized {
			docs = docs[:len(docs)-1]
		}
		docs = append(docs, map[string]string{"content": truncatedMark})
		s = dump(docs)
	}
	return s
}

// writeInput and writeOutput record content in attributes specific to each span type (contract §4).
// When structured is true, messages can be supplied as []Message.
func (s *Span) writeInput(v any, structured bool) {
	switch s.kind {
	case kindTool:
		s.span.SetAttributes(attribute.String("gen_ai.tool.call.arguments", serialize(v)))
	case kindRetrieval:
		s.span.SetAttributes(attribute.String("gen_ai.retrieval.query.text", serialize(v)))
	case kindAgent, kindWorkflow, kindLLM:
		msgs := messagesJSON("user", serialize(v))
		if structured {
			msgs = messagesOf(v, "user")
		}
		s.span.SetAttributes(attribute.String("gen_ai.input.messages", msgs))
	}
}

func (s *Span) writeOutput(v any, structured bool) {
	switch s.kind {
	case kindTool:
		s.span.SetAttributes(attribute.String("gen_ai.tool.call.result", serialize(v)))
	case kindRetrieval:
		s.span.SetAttributes(attribute.String("gen_ai.retrieval.documents", documentsJSON(v)))
	case kindAgent, kindWorkflow, kindLLM:
		msgs := messagesJSON("assistant", serialize(v))
		if structured {
			msgs = messagesOf(v, "assistant")
		}
		s.span.SetAttributes(attribute.String("gen_ai.output.messages", msgs))
	}
}

// SetInput records arguments. Tool writes gen_ai.tool.call.arguments, and Retrieval writes gen_ai.retrieval.query.text.
// Agent, Workflow, and LLM serialize the input as a single message in gen_ai.input.messages.
// Embedding omits input, and all span types omit input when content capture is off.
func (s *Span) SetInput(v any) {
	if s == nil || s.span == nil || !ContentCaptureEnabled() {
		return
	}
	defer func() { _ = recover() }()
	s.writeInput(v, false)
}

// SetOutput records the return value. Tool writes gen_ai.tool.call.result, and Retrieval writes the document array
// to gen_ai.retrieval.documents.
// Agent, Workflow, and LLM write gen_ai.output.messages. Embedding omits output, and all span types omit output when content capture is off.
func (s *Span) SetOutput(v any) {
	if s == nil || s.span == nil || !ContentCaptureEnabled() {
		return
	}
	defer func() { _ = recover() }()
	s.writeOutput(v, false)
}

// Usage holds token usage for Annotate. Zero values are treated as unset and omitted.
type Usage struct {
	InputTokens           int64
	OutputTokens          int64
	CacheReadInputTokens  int64
	CacheWriteInputTokens int64 // Corresponds to Anthropic cache creation.
}

// Annotation holds the arguments to Annotate. Zero-value fields are omitted (contract §4.2).
type Annotation struct {
	// Input is a string or []Message for Agent, Workflow, and LLM, arguments for Tool, or query text for Retrieval.
	Input any
	// Output is a string or []Message for Agent, Workflow, and LLM, a result for Tool, or a document array for Retrieval.
	Output any
	// Model sets gen_ai.response.model for LLM and Embedding.
	Model string
	// Usage records all four token counts for LLM and only InputTokens for Embedding.
	Usage *Usage
	// FinishReasons sets gen_ai.response.finish_reasons for LLM.
	FinishReasons []string
	// Tags are written as flashcat.tag.<k> for all semantic span types.
	Tags map[string]string
}

// Annotate adds data to this semantic span. Input and output follow the content capture switch. It is a no-op on a Trace handle.
func (s *Span) Annotate(a Annotation) {
	if s == nil || s.span == nil || s.kind == kindTrace {
		return
	}
	defer func() { _ = recover() }()
	content := ContentCaptureEnabled()
	if a.Input != nil && content {
		s.writeInput(a.Input, true)
	}
	if a.Output != nil && content {
		s.writeOutput(a.Output, true)
	}
	model := s.kind == kindLLM || s.kind == kindEmbedding
	var attrs []attribute.KeyValue
	if a.Model != "" && model {
		attrs = append(attrs, attribute.String("gen_ai.response.model", a.Model))
	}
	if u := a.Usage; u != nil && model {
		put := func(k string, v int64) {
			if v > 0 {
				attrs = append(attrs, attribute.Int64(k, v))
			}
		}
		put("gen_ai.usage.input_tokens", u.InputTokens)
		if s.kind == kindLLM {
			put("gen_ai.usage.output_tokens", u.OutputTokens)
			put("gen_ai.usage.cache_read.input_tokens", u.CacheReadInputTokens)
			put("gen_ai.usage.cache_write.input_tokens", u.CacheWriteInputTokens)
		}
	}
	if len(a.FinishReasons) > 0 && s.kind == kindLLM {
		attrs = append(attrs, attribute.StringSlice("gen_ai.response.finish_reasons", a.FinishReasons))
	}
	for k, v := range a.Tags {
		attrs = append(attrs, attribute.String(attrTagPrefix+k, v))
	}
	if len(attrs) > 0 {
		s.span.SetAttributes(attrs...)
	}
}

// Annotate adds data to the innermost semantic API span in ctx (Agent, Tool, Workflow, LLM, Embedding, or Retrieval).
// With no semantic API span, it is a no-op. Use it inside closure-based calls such as LLM or Retrieval to record usage and output.
func Annotate(ctx context.Context, a Annotation) {
	if ctx == nil {
		return
	}
	if sp, ok := ctx.Value(frameKey{}).(*Span); ok {
		sp.Annotate(a)
	}
}

// End ends the span. If err is non-nil, it sets ERROR status, writes error.type, and records an exception event.
// Only the first call takes effect.
func (s *Span) End(err error) {
	if s == nil || s.span == nil {
		return
	}
	s.once.Do(func() {
		defer func() { _ = recover() }()
		if err != nil {
			s.span.RecordError(err)
			s.span.SetStatus(codes.Error, err.Error())
			s.span.SetAttributes(semconv.ErrorType(err))
		}
		s.span.End()
	})
}

func (s *Span) endPanic(r any) {
	if s == nil || s.span == nil {
		return
	}
	s.once.Do(func() {
		defer func() { _ = recover() }()
		typ := fmt.Sprintf("%T", r)
		msg := fmt.Sprint(r)
		s.span.AddEvent("exception", trace.WithAttributes(
			attribute.String("exception.type", typ),
			attribute.String("exception.message", msg),
		))
		s.span.SetStatus(codes.Error, msg)
		s.span.SetAttributes(attribute.String("error.type", typ))
		s.span.End()
	})
}

func run(ctx context.Context, k spanKind, name string, fn func(context.Context) error, opts []SpanOption, extra []attribute.KeyValue) (err error) {
	sctx, sp := start(ctx, k, name, opts, extra)
	if sp == nil {
		return fn(ctx)
	}
	finished := false
	defer func() {
		if finished {
			return
		}
		if r := recover(); r != nil {
			sp.endPanic(r)
			panic(r)
		}
		sp.End(nil) // runtime.Goexit (for example, from t.FailNow).
	}()
	err = fn(sctx)
	finished = true
	sp.End(err)
	return err
}

// Agent runs fn within an invoke_agent span. It records and returns any error unchanged. If fn panics,
// the panic continues after the span ends.
func Agent(ctx context.Context, name string, fn func(context.Context) error, opts ...SpanOption) error {
	return run(ctx, kindAgent, name, fn, opts, nil)
}

// Tool runs fn within an execute_tool span with the same behavior as Agent.
func Tool(ctx context.Context, name string, fn func(context.Context) error, opts ...SpanOption) error {
	return run(ctx, kindTool, name, fn, opts, nil)
}

// Workflow runs fn within an invoke_workflow span with the same behavior as Agent.
func Workflow(ctx context.Context, name string, fn func(context.Context) error, opts ...SpanOption) error {
	return run(ctx, kindWorkflow, name, fn, opts, nil)
}

// LLM runs fn within a chat {model} span with the same behavior as Agent.
// Use Annotate(ctx, …) inside fn to record usage, the response model, and messages.
func LLM(ctx context.Context, model string, fn func(context.Context) error, opts ...SpanOption) error {
	return run(ctx, kindLLM, model, fn, opts, nil)
}

// Embedding runs fn within an embeddings {model} span with the same behavior as Agent and omits input content.
func Embedding(ctx context.Context, model string, fn func(context.Context) error, opts ...SpanOption) error {
	return run(ctx, kindEmbedding, model, fn, opts, nil)
}

// Retrieval runs fn within a retrieval {name} span with the same behavior as Agent.
// Use Annotate(ctx, …) to record the query and documents.
func Retrieval(ctx context.Context, name string, fn func(context.Context) error, opts ...SpanOption) error {
	return run(ctx, kindRetrieval, name, fn, opts, nil)
}

// Trace runs fn within a regular, non-GenAI span called name. Use it for root spans of scripts and jobs.
func Trace(ctx context.Context, name string, fn func(context.Context) error, attrs ...attribute.KeyValue) error {
	return run(ctx, kindTrace, name, fn, nil, attrs)
}
