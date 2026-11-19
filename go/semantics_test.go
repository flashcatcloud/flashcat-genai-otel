package flashcatotel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// useRecorder installs a global provider with a session processor and recorder.
// These in-process tests need no fresh global placeholder.
func useRecorder(t *testing.T, content bool) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sessionProcessor{}), sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	old := global.content.Load()
	if content {
		global.content.Store(2)
	} else {
		global.content.Store(1)
	}
	t.Cleanup(func() {
		global.content.Store(old)
		if !isGlobalDefault(prev, "tracerProvider") {
			otel.SetTracerProvider(prev)
		}
	})
	return rec
}

func attrMap(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	m := map[string]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func spanByName(t *testing.T, rec *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

type orderErr struct{ id string }

func (e *orderErr) Error() string { return "no such order " + e.id }

func TestSemanticSpanShapes(t *testing.T) {
	rec := useRecorder(t, false)
	ctx := context.Background()
	_ = Agent(ctx, "kefu", func(ctx context.Context) error {
		_ = Tool(ctx, "search_kb", func(context.Context) error { return nil }, WithDescription("查知识库"))
		return Workflow(ctx, "ticket", func(context.Context) error { return nil })
	}, WithDescription("客服"))
	_ = Trace(ctx, "batch_job", func(context.Context) error { return nil }, attribute.String("job.id", "j1"))

	a := spanByName(t, rec, "invoke_agent kefu")
	am := attrMap(a)
	if am["gen_ai.operation.name"].AsString() != "invoke_agent" || am["gen_ai.agent.name"].AsString() != "kefu" ||
		am["gen_ai.agent.description"].AsString() != "客服" || a.SpanKind() != trace.SpanKindInternal ||
		a.InstrumentationScope().Name != ScopeName || a.InstrumentationScope().Version != Version {
		t.Fatalf("agent: %v scope=%v", am, a.InstrumentationScope())
	}
	tm := attrMap(spanByName(t, rec, "execute_tool search_kb"))
	if tm["gen_ai.operation.name"].AsString() != "execute_tool" || tm["gen_ai.tool.name"].AsString() != "search_kb" ||
		tm["gen_ai.tool.type"].AsString() != "function" || tm["gen_ai.tool.description"].AsString() != "查知识库" {
		t.Fatalf("tool: %v", tm)
	}
	wm := attrMap(spanByName(t, rec, "invoke_workflow ticket"))
	if wm["gen_ai.operation.name"].AsString() != "invoke_workflow" || wm["gen_ai.workflow.name"].AsString() != "ticket" {
		t.Fatalf("workflow: %v", wm)
	}
	jm := attrMap(spanByName(t, rec, "batch_job"))
	if jm["job.id"].AsString() != "j1" || jm["gen_ai.operation.name"].Type() != attribute.INVALID {
		t.Fatalf("trace: %v", jm)
	}
	if spanByName(t, rec, "execute_tool search_kb").Parent().SpanID() != a.SpanContext().SpanID() {
		t.Fatal("tool not child of agent")
	}
}

func TestSemanticAttributesSetAtStart(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	var seen map[string]attribute.Value
	probe := onStartProbe(func(s sdktrace.ReadWriteSpan) { seen = attrMap(s) })
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(probe), sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(sdktrace.NewTracerProvider()) })
	_ = Tool(context.Background(), "t", func(context.Context) error { return nil })
	if seen["gen_ai.operation.name"].AsString() != "execute_tool" || seen["gen_ai.tool.name"].AsString() != "t" {
		t.Fatalf("attributes at start: %v", seen)
	}
}

type onStartProbe func(sdktrace.ReadWriteSpan)

func (p onStartProbe) OnStart(_ context.Context, s sdktrace.ReadWriteSpan) { p(s) }
func (onStartProbe) OnEnd(sdktrace.ReadOnlySpan)                           {}
func (onStartProbe) Shutdown(context.Context) error                        { return nil }
func (onStartProbe) ForceFlush(context.Context) error                      { return nil }

func TestErrorReturnedUnchanged(t *testing.T) {
	rec := useRecorder(t, false)
	want := &orderErr{id: "A-1"}
	got := Tool(context.Background(), "boom", func(context.Context) error { return want })
	if got != want { // The original error is returned unchanged.
		t.Fatalf("error changed: %#v", got)
	}
	s := spanByName(t, rec, "execute_tool boom")
	if s.Status().Code != codes.Error || attrMap(s)["error.type"].AsString() != "*flashcatotel.orderErr" {
		t.Fatalf("status=%v attrs=%v", s.Status(), attrMap(s))
	}
	if len(s.Events()) == 0 || s.Events()[0].Name != "exception" {
		t.Fatalf("exception event missing: %v", s.Events())
	}
	// Errors created with errors.New are also returned unchanged.
	sentinel := errors.New("x")
	if err := Agent(context.Background(), "a", func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) || err != sentinel {
		t.Fatal("sentinel changed")
	}
}

func TestPanicEndsSpanAndRepanics(t *testing.T) {
	rec := useRecorder(t, false)
	type myPanic struct{ code int }
	val := myPanic{code: 7}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = Workflow(context.Background(), "wf", func(context.Context) error { panic(val) })
	}()
	if recovered != val {
		t.Fatalf("re-panic value changed: %#v", recovered)
	}
	s := spanByName(t, rec, "invoke_workflow wf")
	if s.EndTime().IsZero() || s.Status().Code != codes.Error || !strings.Contains(attrMap(s)["error.type"].AsString(), "myPanic") {
		t.Fatalf("span after panic: status=%v attrs=%v", s.Status(), attrMap(s))
	}
}

func TestGoexitEndsSpan(t *testing.T) {
	rec := useRecorder(t, false)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = Agent(context.Background(), "exit", func(context.Context) error {
			runtimeGoexit()
			return nil
		})
	}()
	wg.Wait()
	s := spanByName(t, rec, "invoke_agent exit")
	if s.EndTime().IsZero() || s.Status().Code == codes.Error {
		t.Fatalf("span after Goexit: %v", s.Status())
	}
}

func TestAsyncGoroutines(t *testing.T) {
	rec := useRecorder(t, false)
	err := Agent(context.Background(), "parallel", func(ctx context.Context) error {
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs <- Tool(ctx, fmt.Sprintf("t%d", i), func(context.Context) error { return nil })
			}(i)
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			if e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := spanByName(t, rec, "invoke_agent parallel").SpanContext().SpanID()
	for i := 0; i < 8; i++ {
		if spanByName(t, rec, fmt.Sprintf("execute_tool t%d", i)).Parent().SpanID() != parent {
			t.Fatal("goroutine span lost parent")
		}
	}
}

// Streaming spans started explicitly end on exhaustion, early close, or error.
func stream(ctx context.Context, n int, failAt int) (<-chan int, func(), *Span) {
	_, sp := StartAgent(ctx, "stream")
	ch := make(chan int)
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	go func() {
		defer close(ch)
		var got []int
		for i := 0; i < n; i++ {
			if i == failAt {
				sp.End(fmt.Errorf("upstream broke at %d", i))
				return
			}
			select {
			case ch <- i:
				got = append(got, i)
			case <-done:
				sp.SetOutput(got)
				sp.End(nil)
				return
			}
		}
		sp.SetOutput(got)
		sp.End(nil)
	}()
	return ch, stop, sp
}

func TestStreamingStartEnd(t *testing.T) {
	rec := useRecorder(t, true)
	ctx := context.Background()

	ch, stop, _ := stream(ctx, 3, -1) // Exhaustion.
	for range ch {
	}
	stop()
	ch, stop, _ = stream(ctx, 100, -1) // Early close.
	<-ch
	stop()
	for range ch {
	}
	ch, stop, _ = stream(ctx, 5, 2) // Stream error.
	for range ch {
	}
	stop()

	ended := rec.Ended()
	if len(ended) != 3 {
		t.Fatalf("expected 3 ended spans, got %d", len(ended))
	}
	out0 := attrMap(ended[0])["gen_ai.output.messages"].AsString()
	if !strings.Contains(out0, `"role":"assistant"`) || !strings.Contains(out0, "[0,1,2]") {
		t.Fatalf("exhausted output: %q", out0)
	}
	var errSpans int
	for _, s := range ended {
		if s.Status().Code == codes.Error {
			errSpans++
		}
	}
	if errSpans != 1 {
		t.Fatalf("error spans = %d", errSpans)
	}
}

func TestContentOffWritesNothing(t *testing.T) {
	rec := useRecorder(t, false)
	ctx, sp := StartTool(context.Background(), "t")
	sp.SetInput(map[string]string{"q": "x"})
	sp.SetOutput("y")
	sp.End(nil)
	_, ag := StartAgent(ctx, "a")
	ag.SetInput("q")
	ag.SetOutput("a")
	ag.End(nil)
	for _, s := range rec.Ended() {
		for k := range attrMap(s) {
			if strings.HasPrefix(k, "gen_ai.tool.call.") || strings.HasSuffix(k, ".messages") {
				t.Fatalf("content attr %s written with switch off", k)
			}
		}
	}
}

func TestContentOnSerialization(t *testing.T) {
	rec := useRecorder(t, true)
	_, sp := StartTool(context.Background(), "t")
	sp.SetInput(map[string]any{"order_id": "A-1"})
	sp.SetOutput(make(chan int)) // Fall back to %v because JSON cannot encode a channel.
	sp.End(nil)
	_, ag := StartWorkflow(context.Background(), "w")
	ag.SetInput(strings.Repeat("长", maxSerialized+10))
	ag.End(nil)
	tm := attrMap(spanByName(t, rec, "execute_tool t"))
	if tm["gen_ai.tool.call.arguments"].AsString() != `{"order_id":"A-1"}` || !strings.HasPrefix(tm["gen_ai.tool.call.result"].AsString(), "0x") {
		t.Fatalf("tool content: %v", tm)
	}
	in := attrMap(spanByName(t, rec, "invoke_workflow w"))["gen_ai.input.messages"].AsString()
	if !strings.HasPrefix(in, `[{"role":"user","parts":[{"type":"text","content":"长长`) || !strings.Contains(in, "…[truncated]") {
		t.Fatalf("workflow input prefix: %.80q", in)
	}
}

func TestNilSpanIsSafe(t *testing.T) {
	var s *Span
	s.SetInput(1)
	s.SetOutput(2)
	s.End(errors.New("x"))
	s.End(nil)
}

func TestSDKErrorRunsFunctionOnce(t *testing.T) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(onStartProbe(func(sdktrace.ReadWriteSpan) { panic("processor bug") })))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(sdktrace.NewTracerProvider()) })
	calls := 0
	want := errors.New("biz")
	err := Agent(context.Background(), "a", func(context.Context) error { calls++; return want })
	if calls != 1 || err != want {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestEndIsIdempotent(t *testing.T) {
	rec := useRecorder(t, false)
	_, sp := StartTool(context.Background(), "t")
	sp.End(nil)
	sp.End(errors.New("late"))
	s := spanByName(t, rec, "execute_tool t")
	if s.Status().Code == codes.Error {
		t.Fatal("second End changed the span")
	}
}
