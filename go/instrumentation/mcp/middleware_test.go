package flashcatmcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	flashcatgenai "github.com/flashcatcloud/flashcat-genai-otel/go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type orderArgs struct {
	Phone string `json:"phone"`
}

// connect creates a pair of in-memory transports, installs the server middleware, registers two tools, and returns the client session.
func connect(t *testing.T, opts ...Option) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "orders", Version: "1.0.0"}, nil)
	server.AddReceivingMiddleware(Middleware(opts...))
	mcp.AddTool(server, &mcp.Tool{Name: "lookup_order", Description: "查订单"},
		func(ctx context.Context, req *mcp.CallToolRequest, in orderArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "shipped " + in.Phone}}}, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "broken", Description: "总是失败"},
		func(ctx context.Context, req *mcp.CallToolRequest, in orderArgs) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("no such order")
		})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "kefu", Version: "1.0.0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func spanNamed(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func attrs(s sdktrace.ReadOnlySpan) map[string]string {
	m := map[string]string{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

func TestToolCallSpanJoinsClientTrace(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(flashcatgenai.NewSpanProcessor()), sdktrace.WithSpanProcessor(rec))
	cs := connect(t, WithTracerProvider(tp), WithCaptureContent(true))

	// Client instrumentation sends traceparent in _meta.
	ctx, client := tp.Tracer("client").Start(context.Background(), "tools/call lookup_order", trace.WithSpanKind(trace.SpanKindClient))
	meta := mcp.Meta{}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	for k, v := range carrier {
		meta[k] = v
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "lookup_order", Arguments: map[string]any{"phone": "13812345678"}, Meta: meta})
	client.End()
	if err != nil || res.IsError {
		t.Fatalf("res=%+v err=%v", res, err)
	}

	var server sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "tools/call lookup_order" && s.SpanKind() == trace.SpanKindServer {
			server = s
		}
	}
	if server == nil {
		t.Fatal("no SERVER span found")
	}
	if server.InstrumentationScope().Name != ScopeName {
		t.Errorf("scope = %s", server.InstrumentationScope().Name)
	}
	if server.Parent().SpanID() != client.SpanContext().SpanID() || server.SpanContext().TraceID() != client.SpanContext().TraceID() {
		t.Error("the server span should be a child of the client span from traceparent in _meta")
	}
	if !server.Parent().IsRemote() || !slices.Contains(server.Attributes(), attribute.Bool("flashcat.genai.root", false)) {
		t.Errorf("a remote parent in _meta must set flashcat.genai.root=false: %v", server.Attributes())
	}
	a := attrs(server)
	for k, want := range map[string]string{
		"mcp.method.name": "tools/call", "gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": "lookup_order",
		"gen_ai.tool.call.arguments": `{"phone":"13812345678"}`, "gen_ai.tool.call.result": "shipped 13812345678",
	} {
		if a[k] != want {
			t.Errorf("%s = %q, want %q", k, a[k], want)
		}
	}
	if server.Status().Code == codes.Error {
		t.Error("a successful call should not have ERROR status")
	}
}

func TestToolCallWithInvalidMetaKeepsRemoteParent(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(flashcatgenai.NewSpanProcessor()), sdktrace.WithSpanProcessor(rec))
	handler := Middleware(WithTracerProvider(tp))(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil
	})
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{
		"traceparent": "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "lookup_order", Meta: mcp.Meta{"traceparent": "invalid"}}}
	if _, err := handler(ctx, "tools/call", req); err != nil {
		t.Fatal(err)
	}
	s := spanNamed(t, rec, "tools/call lookup_order")
	if !s.Parent().Equal(trace.SpanContextFromContext(ctx)) || !slices.Contains(s.Attributes(), attribute.Bool("flashcat.genai.root", true)) {
		t.Errorf("a remote parent from outside _meta should be preserved and the tool call marked as an AI root: parent=%v attrs=%v", s.Parent(), s.Attributes())
	}
}

func TestToolErrorAndContentOff(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	cs := connect(t, WithTracerProvider(tp), WithCaptureContent(false))

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "broken", Arguments: map[string]any{"phone": "x"}})
	if err != nil || !res.IsError {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	s := spanNamed(t, rec, "tools/call broken")
	a := attrs(s)
	if a["error.type"] != "tool_error" || s.Status().Code != codes.Error {
		t.Errorf("error.type=%q status=%v", a["error.type"], s.Status())
	}
	if _, ok := a["gen_ai.tool.call.arguments"]; ok {
		t.Error("tool arguments should be omitted when content capture is off")
	}
	for _, sp := range rec.Ended() {
		if strings.HasPrefix(sp.Name(), "tools/list") {
			t.Error("only tools/call should be instrumented")
		}
	}
}
