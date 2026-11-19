package flashcateino

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/embedding"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	flashcatotel "github.com/flashcatcloud/flashcat-otel/go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// fakeChatModel makes no network requests. The compose node injects its callbacks, which receive inputs as []*schema.Message.
type fakeChatModel struct{ err error }

func (f *fakeChatModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &schema.Message{Role: schema.Assistant, Content: "您的订单已发货",
		ResponseMeta: &schema.ResponseMeta{FinishReason: "stop", Usage: &schema.TokenUsage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17}}}, nil
}

func (f *fakeChatModel) Stream(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if f.err != nil {
		return nil, f.err
	}
	return schema.StreamReaderFromArray([]*schema.Message{
		{Role: schema.Assistant, Content: ""},
		{Role: schema.Assistant, Content: "您的"},
		{Role: schema.Assistant, Content: "订单已发货"},
		{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{FinishReason: "stop", Usage: &schema.TokenUsage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17}}},
	}), nil
}

func setup(t *testing.T, capture bool) (*tracetest.SpanRecorder, callbacks.Handler) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	return rec, NewHandler(WithTracerProvider(tp), WithCaptureContent(capture))
}

func attrs(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func byName(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	var names []string
	for _, s := range rec.Ended() {
		names = append(names, s.Name())
	}
	t.Fatalf("span %q not found; ended: %v", name, names)
	return nil
}

func chain(t *testing.T, m model.BaseChatModel) compose.Runnable[[]*schema.Message, *schema.Message] {
	t.Helper()
	c := compose.NewChain[[]*schema.Message, *schema.Message]()
	c.AppendLambda(compose.InvokableLambda(func(_ context.Context, in []*schema.Message) ([]*schema.Message, error) { return in, nil }))
	c.AppendChatModel(m, compose.WithNodeName("kefu_model"))
	r, err := c.Compile(context.Background(), compose.WithGraphName("kefu_chain"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var question = []*schema.Message{schema.SystemMessage("你是客服"), schema.UserMessage("我的订单到哪了")}

func TestChainInvoke(t *testing.T) {
	rec, h := setup(t, false)
	out, err := chain(t, &fakeChatModel{}).Invoke(context.Background(), question, compose.WithCallbacks(h))
	if err != nil || out.Content != "您的订单已发货" {
		t.Fatalf("invoke: %v %v", out, err)
	}
	wf := byName(t, rec, "invoke_workflow kefu_chain")
	chat := byName(t, rec, "chat")
	if chat.Parent().SpanID() != wf.SpanContext().SpanID() {
		t.Fatal("chat span is not a child of the chain span")
	}
	if len(rec.Ended()) != 2 {
		t.Fatalf("lambda must not produce a span: %d spans", len(rec.Ended()))
	}
	m := attrs(chat)
	if m["gen_ai.operation.name"].AsString() != "chat" || chat.SpanKind() != trace.SpanKindClient || m["gen_ai.usage.input_tokens"].AsInt64() != 12 ||
		m["gen_ai.usage.output_tokens"].AsInt64() != 5 || strings.Join(m["gen_ai.response.finish_reasons"].AsStringSlice(), ",") != "stop" {
		t.Fatalf("chat attrs: %v", m)
	}
	if _, ok := m["gen_ai.input.messages"]; ok {
		t.Fatal("content captured with switch off")
	}
	if w := attrs(wf); w["gen_ai.operation.name"].AsString() != "invoke_workflow" || w["gen_ai.workflow.name"].AsString() != "kefu_chain" {
		t.Fatalf("workflow attrs: %v", w)
	}
}

func TestChainInvokeContent(t *testing.T) {
	rec, h := setup(t, true)
	if _, err := chain(t, &fakeChatModel{}).Invoke(context.Background(), question, compose.WithCallbacks(h)); err != nil {
		t.Fatal(err)
	}
	m := attrs(byName(t, rec, "chat"))
	if in := m["gen_ai.input.messages"].AsString(); in != `[{"role":"system","parts":[{"type":"text","content":"你是客服"}]},{"role":"user","parts":[{"type":"text","content":"我的订单到哪了"}]}]` {
		t.Fatalf("input: %s", in)
	}
	if out := m["gen_ai.output.messages"].AsString(); out != `[{"role":"assistant","parts":[{"type":"text","content":"您的订单已发货"}],"finish_reason":"stop"}]` {
		t.Fatalf("output: %s", out)
	}
}

func TestChainStream(t *testing.T) {
	rec, h := setup(t, true)
	sr, err := chain(t, &fakeChatModel{}).Stream(context.Background(), question, compose.WithCallbacks(h))
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for {
		m, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got.WriteString(m.Content)
	}
	sr.Close()
	if got.String() != "您的订单已发货" {
		t.Fatalf("caller saw %q", got.String())
	}
	// The callback drains its stream copy in the background before ending the span.
	waitEnded(t, rec, 2)
	m := attrs(byName(t, rec, "chat"))
	if m["gen_ai.usage.input_tokens"].AsInt64() != 12 || m["gen_ai.response.time_to_first_chunk"].AsFloat64() <= 0 ||
		!strings.Contains(m["gen_ai.output.messages"].AsString(), "您的订单已发货") {
		t.Fatalf("stream chat attrs: %v", m)
	}
}

func waitEnded(t *testing.T, rec *tracetest.SpanRecorder, n int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if len(rec.Ended()) >= n {
			return
		}
		select {
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("only %d spans ended, want %d", len(rec.Ended()), n)
}

func TestChainError(t *testing.T) {
	rec, h := setup(t, false)
	want := errors.New("model down")
	_, err := chain(t, &fakeChatModel{err: want}).Invoke(context.Background(), question, compose.WithCallbacks(h))
	if !errors.Is(err, want) {
		t.Fatalf("error: %v", err)
	}
	for _, name := range []string{"chat", "invoke_workflow kefu_chain"} {
		if s := byName(t, rec, name); s.Status().Code != codes.Error {
			t.Fatalf("%s status: %v", name, s.Status())
		}
	}
}

// Invoke callbacks directly to cover Tool, Retriever, Embedding, and Agent, and verify that components with no span leave the enclosing span open.
func TestComponentKinds(t *testing.T) {
	rec, h := setup(t, true)
	ctx := context.Background()

	actx := h.OnStart(ctx, &callbacks.RunInfo{Name: "kefu", Component: "Agent"}, nil)

	tctx := h.OnStart(actx, &callbacks.RunInfo{Name: "lookup_order", Type: "InferTool", Component: components.ComponentOfTool},
		&tool.CallbackInput{ArgumentsInJSON: `{"order_id":"A-1"}`})
	h.OnEnd(tctx, &callbacks.RunInfo{Name: "lookup_order", Component: components.ComponentOfTool}, &tool.CallbackOutput{Response: `{"status":"shipped"}`})

	lctx := h.OnStart(actx, &callbacks.RunInfo{Name: "fmt", Component: "Lambda"}, "x")
	h.OnEnd(lctx, &callbacks.RunInfo{Name: "fmt", Component: "Lambda"}, "y") // The Lambda must leave the agent span open.

	rctx := h.OnStart(actx, &callbacks.RunInfo{Name: "kb", Component: components.ComponentOfRetriever}, &retriever.CallbackInput{Query: "退货政策", TopK: 3})
	doc := &schema.Document{ID: "d1"}
	doc.WithScore(0.9)
	h.OnEnd(rctx, &callbacks.RunInfo{Name: "kb", Component: components.ComponentOfRetriever}, &retriever.CallbackOutput{Docs: []*schema.Document{doc}})

	ectx := h.OnStart(actx, &callbacks.RunInfo{Name: "emb", Type: "OpenAI", Component: components.ComponentOfEmbedding},
		&embedding.CallbackInput{Texts: []string{"a"}, Config: &embedding.Config{Model: "text-embedding-3-small"}})
	h.OnEnd(ectx, &callbacks.RunInfo{Component: components.ComponentOfEmbedding}, &embedding.CallbackOutput{TokenUsage: &embedding.TokenUsage{PromptTokens: 4}})

	if len(rec.Ended()) != 3 {
		t.Fatalf("agent ended early or extra spans: %d", len(rec.Ended()))
	}
	h.OnEnd(actx, &callbacks.RunInfo{Name: "kefu", Component: "Agent"}, nil)

	agent := byName(t, rec, "invoke_agent kefu")
	if a := attrs(agent); a["gen_ai.operation.name"].AsString() != "invoke_agent" || a["gen_ai.agent.name"].AsString() != "kefu" {
		t.Fatalf("agent: %v", a)
	}
	ts := byName(t, rec, "execute_tool lookup_order")
	tm := attrs(ts)
	if ts.Parent().SpanID() != agent.SpanContext().SpanID() || tm["gen_ai.tool.type"].AsString() != "function" ||
		tm["gen_ai.tool.call.arguments"].AsString() != `{"order_id":"A-1"}` || tm["gen_ai.tool.call.result"].AsString() != `{"status":"shipped"}` {
		t.Fatalf("tool: %v", tm)
	}
	rm := attrs(byName(t, rec, "retrieval kb"))
	if rm["gen_ai.retrieval.top_k"].AsInt64() != 3 || rm["gen_ai.retrieval.query.text"].AsString() != "退货政策" ||
		rm["gen_ai.retrieval.documents"].AsString() != `[{"id":"d1","score":0.9}]` {
		t.Fatalf("retrieval: %v", rm)
	}
	em := attrs(byName(t, rec, "embeddings text-embedding-3-small"))
	if em["gen_ai.provider.name"].AsString() != "openai" || em["gen_ai.usage.input_tokens"].AsInt64() != 4 {
		t.Fatalf("embeddings: %v", em)
	}
}

func TestStreamOutputForNonSpanComponentIsClosed(t *testing.T) {
	_, h := setup(t, false)
	ctx := h.OnStart(context.Background(), &callbacks.RunInfo{Component: "Lambda"}, nil)
	sr, sw := schema.Pipe[callbacks.CallbackOutput](1)
	h.OnEndWithStreamOutput(ctx, &callbacks.RunInfo{Component: "Lambda"}, sr)
	// Send on the writer returns closed=true once the reader closes.
	if closed := sw.Send("x", nil); !closed {
		t.Fatal("handler did not close the copied stream")
	}
	sw.Close()
}

func TestNeeded(t *testing.T) {
	_, h := setup(t, false)
	tc := h.(callbacks.TimingChecker)
	if tc.Needed(context.Background(), &callbacks.RunInfo{Component: "Lambda"}, callbacks.TimingOnStart) ||
		!tc.Needed(context.Background(), &callbacks.RunInfo{Component: components.ComponentOfChatModel}, callbacks.TimingOnEndWithStreamOutput) {
		t.Fatal("Needed")
	}
}

func TestSessionAndGlobalProvider(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(flashcatotel.NewSpanProcessor()), sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	h := NewHandler(WithCaptureContent(false))
	ctx := flashcatotel.WithSession(context.Background(), flashcatotel.Session{ConversationID: "c-e", UserID: "u-e"})
	if _, err := chain(t, &fakeChatModel{}).Invoke(ctx, question, compose.WithCallbacks(h)); err != nil {
		t.Fatal(err)
	}
	m := attrs(byName(t, rec, "chat"))
	if m["gen_ai.conversation.id"].AsString() != "c-e" || m["user.id"].AsString() != "u-e" {
		t.Fatalf("session: %v", m)
	}
	workflow := attrs(byName(t, rec, "invoke_workflow kefu_chain"))
	if workflow["flashcat.genai.root"] != attribute.BoolValue(true) || m["flashcat.genai.root"] != attribute.BoolValue(false) {
		t.Fatalf("AI roots: workflow=%v chat=%v", workflow, m)
	}
	found := false
	for _, it := range flashcatotel.GetDiagnostics().Instrumentations {
		if it.Lib == "eino" && it.Status == "installed" && it.Source == "flashcat" {
			found = true
		}
	}
	if !found {
		t.Fatal("eino not registered in diagnostics")
	}
}

// fakeTool is a local test tool. ToolsNode injects its callbacks.
type fakeTool struct{}

func (fakeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "lookup_order", Desc: "查订单"}, nil
}

func (fakeTool) InvokableRun(_ context.Context, args string, _ ...tool.Option) (string, error) {
	return `{"status":"shipped"}`, nil
}

func TestToolsNodeInGraph(t *testing.T) {
	rec, h := setup(t, true)
	ctx := context.Background()
	tn, err := compose.NewToolNode(ctx, &compose.ToolsNodeConfig{Tools: []tool.BaseTool{fakeTool{}}})
	if err != nil {
		t.Fatal(err)
	}
	c := compose.NewChain[*schema.Message, []*schema.Message]()
	c.AppendToolsNode(tn)
	r, err := c.Compile(ctx, compose.WithGraphName("tools_chain"))
	if err != nil {
		t.Fatal(err)
	}
	in := &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "call_1", Function: schema.FunctionCall{Name: "lookup_order", Arguments: `{"order_id":"A-1"}`}}}}
	out, err := r.Invoke(ctx, in, compose.WithCallbacks(h))
	if err != nil || len(out) != 1 || out[0].Content != `{"status":"shipped"}` {
		t.Fatalf("invoke: %v %v", out, err)
	}
	ts := byName(t, rec, "execute_tool lookup_order")
	m := attrs(ts)
	if m["gen_ai.tool.name"].AsString() != "lookup_order" || m["gen_ai.tool.call.arguments"].AsString() != `{"order_id":"A-1"}` ||
		m["gen_ai.tool.call.result"].AsString() != `{"status":"shipped"}` {
		t.Fatalf("tool attrs: %v", m)
	}
	if ts.Parent().SpanID() != byName(t, rec, "invoke_workflow tools_chain").SpanContext().SpanID() {
		t.Fatal("tool span not under the chain span")
	}
}

func TestADKAgent(t *testing.T) {
	rec, h := setup(t, false)
	ctx := context.Background()
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{Name: "kefu", Description: "客服", Instruction: "你是客服", Model: &fakeChatModel{}})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent})
	iter := runner.Query(ctx, "我的订单到哪了", adk.WithCallbacks(h))
	var final string
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			t.Fatal(ev.Err)
		}
		if ev.Output != nil && ev.Output.MessageOutput != nil && ev.Output.MessageOutput.Message != nil {
			final = ev.Output.MessageOutput.Message.Content
		}
	}
	if final != "您的订单已发货" {
		t.Fatalf("agent output %q", final)
	}
	// ChatModelAgent runs a compose graph with the same name. The agent span contains invoke_workflow kefu, which contains chat.
	waitEnded(t, rec, 3)
	ag := byName(t, rec, "invoke_agent kefu")
	wf := byName(t, rec, "invoke_workflow kefu")
	chat := byName(t, rec, "chat")
	if attrs(ag)["gen_ai.agent.name"].AsString() != "kefu" || wf.Parent().SpanID() != ag.SpanContext().SpanID() ||
		chat.Parent().SpanID() != wf.SpanContext().SpanID() {
		t.Fatalf("agent span tree wrong: agent=%v", attrs(ag))
	}
	// The agent span ends when the event stream finishes, after the full model call.
	if ag.EndTime().Before(chat.EndTime()) {
		t.Fatal("agent span ended before its chat span")
	}
}
