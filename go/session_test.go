package flashcatgenai

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
)

func TestSessionAttributesOnEverySpanInScope(t *testing.T) {
	rec := useRecorder(t, false)
	tr := otel.Tracer("other.lib")
	ctx := WithSession(context.Background(), Session{ConversationID: "c-1", UserID: "u-1", Tags: map[string]string{"tenant": "a"}})
	ctx = WithSession(ctx, Session{Tags: map[string]string{"team": "kf"}}) // Nested sessions merge tags and inherit the conversation ID.
	_, s1 := tr.Start(ctx, "plain-no-genai")                               // Also set session attributes on spans without gen_ai.operation.name, unlike Python 0.1.0.
	s1.End()
	_, s2 := tr.Start(ctx, "preset", trace.WithAttributes(attribute.String("gen_ai.conversation.id", "native-conv")))
	s2.End()
	_, s3 := tr.Start(context.Background(), "outside")
	s3.End()

	m1 := attrMap(spanByName(t, rec, "plain-no-genai"))
	if m1["gen_ai.conversation.id"].AsString() != "c-1" || m1["user.id"].AsString() != "u-1" ||
		m1["flashcat.tag.tenant"].AsString() != "a" || m1["flashcat.tag.team"].AsString() != "kf" {
		t.Fatalf("in scope: %v", m1)
	}
	if m2 := attrMap(spanByName(t, rec, "preset")); m2["gen_ai.conversation.id"].AsString() != "native-conv" {
		t.Fatalf("existing conversation id overwritten: %v", m2)
	}
	m3 := attrMap(spanByName(t, rec, "outside"))
	for _, k := range []string{"gen_ai.conversation.id", "user.id", "flashcat.tag.tenant"} {
		if m3[k].Type() != attribute.INVALID {
			t.Fatalf("outside scope has %s", k)
		}
	}
	if baggage.FromContext(ctx).Len() != 0 {
		t.Fatal("baggage written without Propagate")
	}
}

func TestSessionInheritedFromLocalParent(t *testing.T) {
	rec := useRecorder(t, false)
	tr := otel.Tracer("x")
	ctx := WithSession(context.Background(), Session{ConversationID: "c-2", UserID: "u-2", Tags: map[string]string{"k": "v"}})
	_, parent := tr.Start(ctx, "parent")
	// Children inherit from a local parent span even if the private context key is missing,
	// such as across class loaders or when only the parent span is available.
	bare := trace.ContextWithSpan(context.Background(), parent)
	_, child := tr.Start(bare, "child")
	child.End()
	parent.End()
	m := attrMap(spanByName(t, rec, "child"))
	if m["gen_ai.conversation.id"].AsString() != "c-2" || m["user.id"].AsString() != "u-2" || m["flashcat.tag.k"].AsString() != "v" {
		t.Fatalf("not inherited: %v", m)
	}
	// A remote parent with only a SpanContext cannot supply inherited attributes.
	remote := trace.ContextWithRemoteSpanContext(context.Background(), parent.SpanContext())
	_, r := tr.Start(remote, "remote-child")
	r.End()
	if attrMap(spanByName(t, rec, "remote-child"))["gen_ai.conversation.id"].Type() != attribute.INVALID {
		t.Fatal("remote parent must not be inherited")
	}
}

func TestPropagateWritesBaggage(t *testing.T) {
	ctx := WithSession(context.Background(), Session{ConversationID: "c-3", UserID: "u-3", Propagate: true})
	b := baggage.FromContext(ctx)
	if b.Member("gen_ai.conversation.id").Value() != "c-3" || b.Member("user.id").Value() != "u-3" {
		t.Fatalf("baggage: %s", b.String())
	}
}

func TestOrphanInferenceSpans(t *testing.T) {
	_ = useRecorder(t, false)
	before := global.orphans.Load()
	tr := otel.Tracer("x")
	_, s := tr.Start(context.Background(), "chat", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	s.End()
	_ = Agent(context.Background(), "a", func(ctx context.Context) error {
		_, c := tr.Start(ctx, "chat-child", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
		c.End()
		return nil
	})
	_, e := tr.Start(context.Background(), "emb", trace.WithAttributes(attribute.String("gen_ai.operation.name", "embeddings")))
	e.End()
	if got := global.orphans.Load() - before; got != 2 {
		t.Fatalf("orphans = %d, want 2", got)
	}
}
