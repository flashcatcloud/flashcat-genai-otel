package flashcatgenai

import (
	"context"
	"encoding/binary"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestGenAIRootMarksLocalAncestry(t *testing.T) {
	rec := useRecorder(t, false)
	tr := otel.Tracer("biz")
	ctx := WithSession(context.Background(), Session{ConversationID: "c-1"})
	hctx, http := tr.Start(ctx, "POST /chat", trace.WithSpanKind(trace.SpanKindServer))
	actx, agent := StartAgent(hctx, "kefu")
	_, direct := StartLLM(actx, "direct")
	direct.End(nil)
	_, tool := StartTool(actx, "lookup")
	tool.End(nil)
	pctx, plain := tr.Start(actx, "prepare")
	_, indirect := StartLLM(pctx, "indirect")
	indirect.End(nil)
	_, newTrace := tr.Start(pctx, "new trace", trace.WithNewRoot(), trace.WithAttributes(attribute.String(attrOperationName, "chat")))
	newTrace.End()
	plain.End()
	agent.End(nil)
	http.End()

	for name, want := range map[string]bool{
		"invoke_agent kefu": true, "chat direct": false, "execute_tool lookup": false,
		"chat indirect": false, "new trace": true,
	} {
		if got := attrMap(spanByName(t, rec, name))[attrGenAIRoot]; got != attribute.BoolValue(want) {
			t.Errorf("%s: %s = %v, want %v", name, attrGenAIRoot, got, want)
		}
	}
	for _, name := range []string{"POST /chat", "prepare"} {
		attrs := attrMap(spanByName(t, rec, name))
		if _, ok := attrs[attrGenAIRoot]; ok || attrs[attrConversationID].AsString() != "c-1" {
			t.Errorf("%s: attrs = %v", name, attrs)
		}
	}
}

func TestGenAIRootIgnoresRemoteParent(t *testing.T) {
	rec := useRecorder(t, false)
	actx, agent := StartAgent(context.Background(), "upstream")
	pctx, plain := otel.Tracer("biz").Start(actx, "upstream child")
	// Simulate cross-process propagation with a span whose ancestry is already recorded. Ignore the remote parent ID even if cached.
	propagator := propagation.TraceContext{}
	carrier := propagation.MapCarrier{}
	propagator.Inject(pctx, carrier)
	remote := propagator.Extract(context.Background(), carrier)
	_, model := StartLLM(remote, "downstream")
	model.End(nil)
	plain.End()
	agent.End(nil)

	s := spanByName(t, rec, "chat downstream")
	if !s.Parent().IsRemote() || s.Parent().SpanID() != plain.SpanContext().SpanID() {
		t.Fatalf("remote parent: %v", s.Parent())
	}
	if got := attrMap(s)[attrGenAIRoot]; got != attribute.BoolValue(true) {
		t.Fatalf("%s = %v, want true", attrGenAIRoot, got)
	}
}

func TestGenAIRootUsesInstrumentationScope(t *testing.T) {
	rec := useRecorder(t, false)
	oi := otel.Tracer("openinference.instrumentation.langchain")
	ctx, chain := oi.Start(context.Background(), "RunnableSequence")
	_, model := oi.Start(ctx, "ChatOpenAI")
	model.SetAttributes(attribute.String("openinference.span.kind", "LLM"))
	model.End()
	chain.SetAttributes(attribute.String("openinference.span.kind", "CHAIN"))
	chain.End()

	for name, want := range map[string]bool{"RunnableSequence": true, "ChatOpenAI": false} {
		if got := attrMap(spanByName(t, rec, name))[attrGenAIRoot]; got != attribute.BoolValue(want) {
			t.Errorf("%s: %s = %v, want %v", name, attrGenAIRoot, got, want)
		}
	}
}

func TestIsGenAI(t *testing.T) {
	for _, tt := range []struct {
		keys []string
		want bool
	}{
		{[]string{attrConversationID, attrGenAIRoot, "http.request.method"}, false},
		{[]string{"gen_ai.usage.input_tokens"}, true},
		{[]string{"llm.request.type"}, true},
		{[]string{"openinference.span.kind"}, true},
		{[]string{"traceloop.span.kind"}, true},
		{[]string{"langfuse.observation.type"}, true},
		{[]string{"flashcat.span.kind"}, true},
		{[]string{"ai.operationId"}, true},
		{[]string{"ai.model.id"}, false},
		{[]string{"operation.name"}, false},
		{[]string{"ai.model.id", "operation.name"}, true},
		{[]string{"operation.name", "ai.model.id"}, true},
	} {
		attrs := make([]attribute.KeyValue, 0, len(tt.keys))
		for _, key := range tt.keys {
			attrs = append(attrs, attribute.String(key, ""))
		}
		if got := isGenAI(attrs); got != tt.want {
			t.Errorf("isGenAI(%v) = %v, want %v", tt.keys, got, tt.want)
		}
	}
}

func TestGenAIAncestryEvictsOldest(t *testing.T) {
	sc := func(n int) trace.SpanContext {
		var id trace.SpanID
		binary.BigEndian.PutUint64(id[:], uint64(n))
		return trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: id})
	}
	var ancestry genAIAncestry
	if ancestry.has(sc(1)) || ancestry.ids != nil || ancestry.order != nil {
		t.Fatal("empty ancestry should not allocate storage")
	}
	for i := 1; i <= genAIAncestryLimit; i++ {
		ancestry.remember(sc(i))
	}
	ancestry.remember(sc(1))
	ancestry.remember(sc(genAIAncestryLimit + 1))
	ancestry.remember(sc(genAIAncestryLimit + 2))
	if len(ancestry.ids) != genAIAncestryLimit || ancestry.has(sc(1)) || ancestry.has(sc(2)) ||
		!ancestry.has(sc(3)) || !ancestry.has(sc(genAIAncestryLimit+2)) {
		t.Fatal("ancestry should retain the most recent 65536 spans")
	}
	if ancestry.has(sc(3).WithTraceID(trace.TraceID{2})) {
		t.Fatal("ancestry must match both trace and span IDs")
	}
}
