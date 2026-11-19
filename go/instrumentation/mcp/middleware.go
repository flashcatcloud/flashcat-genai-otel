// Package flashcatmcp provides OpenTelemetry instrumentation for MCP server tool calls (github.com/modelcontextprotocol/go-sdk, contract §4.3).
//
//	server := mcp.NewServer(&mcp.Implementation{Name: "orders", Version: "1.0.0"}, nil)
//	server.AddReceivingMiddleware(flashcatmcp.Middleware())
//
// Each tools/call request creates a "tools/call {tool name}" SERVER span with mcp.method.name,
// gen_ai.operation.name=execute_tool, and gen_ai.tool.name. When content capture is enabled,
// the span includes arguments and results, which the SDK's export pipeline masks.
// If the tool returns IsError, the middleware sets error.type=tool_error and marks the span as ERROR.
// For JSON-RPC errors, it records the error code.
// If the request's _meta includes a W3C traceparent, the middleware uses the client span as the parent
// and sets flashcat.genai.root=false. It reads Trace Context only and ignores baggage.
// Other methods pass through unchanged.
package flashcatmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	flashcatotel "github.com/flashcatcloud/flashcat-otel/go"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Version is the instrumentation module version and is used as the scope version on spans.
const Version = "0.1.0"

// ScopeName is the instrumentation scope name for spans created by this module.
const ScopeName = "github.com/flashcatcloud/flashcat-otel/go/instrumentation/mcp"

const (
	targetModule  = "github.com/modelcontextprotocol/go-sdk"
	methodCall    = "tools/call"
	maxSerialized = 32 * 1024
)

// Middleware returns middleware for (*mcp.Server).AddReceivingMiddleware. It can be created before flashcatotel.Init
// because it reads the provider and content capture switch on each request. Diagnostics list it in
// instrumentations as {"lib":"mcp","source":"flashcat"}.
func Middleware(opts ...Option) mcp.Middleware {
	var cfg config
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	flashcatotel.RegisterInstrumentation("mcp", targetModule)
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != methodCall {
				return next(ctx, method, req)
			}
			return cfg.handle(ctx, method, req, next)
		}
	}
}

func (c *config) tracer() trace.Tracer {
	tp := c.tracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version))
}

func (c *config) capture() bool {
	if c.captureContent != nil {
		return *c.captureContent
	}
	return flashcatotel.ContentCaptureEnabled()
}

func (c *config) handle(ctx context.Context, method string, req mcp.Request, next mcp.MethodHandler) (res mcp.Result, err error) {
	var params *mcp.CallToolParamsRaw
	if req != nil {
		params, _ = req.GetParams().(*mcp.CallToolParamsRaw)
	}
	name := ""
	var parent trace.SpanContext
	if params != nil {
		name = params.Name
		parent = parentFromMeta(params.Meta)
		if parent.IsValid() {
			ctx = trace.ContextWithRemoteSpanContext(ctx, parent)
		}
	}
	spanName := methodCall
	attrs := []attribute.KeyValue{
		attribute.String("mcp.method.name", methodCall),
		attribute.String("gen_ai.operation.name", "execute_tool"),
	}
	if name != "" {
		spanName += " " + name
		attrs = append(attrs, attribute.String("gen_ai.tool.name", name))
	}
	ctx, span := c.tracer().Start(ctx, spanName, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
	if parent.IsValid() {
		// The parent from _meta is the client's tool call span (contract §5.1).
		span.SetAttributes(attribute.Bool("flashcat.genai.root", false))
	}
	content := c.capture()
	if content && params != nil && len(params.Arguments) > 0 {
		span.SetAttributes(attribute.String("gen_ai.tool.call.arguments", truncate(string(params.Arguments))))
	}
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprint(r)
			span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", r)))
			span.AddEvent("exception", trace.WithAttributes(attribute.String("exception.message", msg)))
			span.SetStatus(codes.Error, msg)
			span.End()
			panic(r)
		}
	}()
	res, err = next(ctx, method, req)
	if err != nil {
		var we *jsonrpc.Error
		if errors.As(err, &we) {
			span.SetAttributes(attribute.String("error.type", strconv.FormatInt(we.Code, 10)))
		} else {
			span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else if r, ok := res.(*mcp.CallToolResult); ok && r != nil {
		if content {
			span.SetAttributes(attribute.String("gen_ai.tool.call.result", resultText(r)))
		}
		if r.IsError {
			span.SetAttributes(attribute.String("error.type", "tool_error"))
			span.SetStatus(codes.Error, "")
		}
	}
	span.End()
	return res, err
}

// parentFromMeta extracts the W3C traceparent and tracestate from the request's _meta.
func parentFromMeta(meta mcp.Meta) trace.SpanContext {
	carrier := propagation.MapCarrier{}
	for _, k := range []string{"traceparent", "tracestate"} {
		if s, ok := meta[k].(string); ok && s != "" {
			carrier[k] = s
		}
	}
	ctx := propagation.TraceContext{}.Extract(context.Background(), carrier)
	return trace.SpanContextFromContext(ctx)
}

// resultText prefers a structured result, then joined text blocks, then serialization of the entire result.
func resultText(r *mcp.CallToolResult) string {
	if r.StructuredContent != nil {
		if b, err := json.Marshal(r.StructuredContent); err == nil {
			return truncate(string(b))
		}
	}
	texts := make([]string, 0, len(r.Content))
	for _, c := range r.Content {
		t, ok := c.(*mcp.TextContent)
		if !ok {
			texts = nil
			break
		}
		texts = append(texts, t.Text)
	}
	if len(texts) > 0 {
		return truncate(strings.Join(texts, "\n"))
	}
	if b, err := json.Marshal(r.Content); err == nil {
		return truncate(string(b))
	}
	return ""
}

func truncate(s string) string {
	if utf8.RuneCountInString(s) <= maxSerialized {
		return s
	}
	n := 0
	for i := range s {
		if n == maxSerialized {
			return s[:i] + "…[truncated]"
		}
		n++
	}
	return s
}
