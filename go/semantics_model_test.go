package flashcatotel

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Manual model call spans (LLM, Embedding, and Retrieval) and Annotate (contract §4, §4.1, and §4.2).

func TestModelSpansAndAnnotate(t *testing.T) {
	rec := useRecorder(t, true)
	ctx := context.Background()
	err := Agent(ctx, "kefu", func(ctx context.Context) error {
		_ = Embedding(ctx, "text-embedding-v3", func(ctx context.Context) error {
			Annotate(ctx, Annotation{Usage: &Usage{InputTokens: 8, OutputTokens: 99}, Model: "text-embedding-v3"})
			return nil
		}, WithProvider("dashscope"))
		_ = Retrieval(ctx, "kb-orders", func(ctx context.Context) error {
			Annotate(ctx, Annotation{
				Input:  "我的订单到哪了",
				Output: []any{map[string]any{"id": "d1", "content": "订单已发货", "score": 0.9}, "第二段文本"},
			})
			return nil
		})
		return LLM(ctx, "qwen-max", func(ctx context.Context) error {
			Annotate(ctx, Annotation{
				Input:         []Message{{Role: "system", Content: "你是客服"}, {Role: "user", Content: "我的订单到哪了"}},
				Output:        "已为您查询",
				Model:         "qwen-max-0919",
				Usage:         &Usage{InputTokens: 30, OutputTokens: 5, CacheReadInputTokens: 10, CacheWriteInputTokens: 2},
				FinishReasons: []string{"stop"},
				Tags:          map[string]string{"route": "gateway"},
			})
			return nil
		}, WithProvider("dashscope"))
	})
	if err != nil {
		t.Fatal(err)
	}

	chat := spanByName(t, rec, "chat qwen-max")
	agent := spanByName(t, rec, "invoke_agent kefu")
	if chat.SpanKind() != trace.SpanKindClient {
		t.Fatalf("chat kind = %v", chat.SpanKind())
	}
	if chat.Parent().SpanID() != agent.SpanContext().SpanID() {
		t.Fatal("the chat span should be a child of the agent span")
	}
	a := attrMap(chat)
	for k, want := range map[string]any{
		"gen_ai.operation.name": "chat", "gen_ai.request.model": "qwen-max", "gen_ai.provider.name": "dashscope",
		"gen_ai.response.model": "qwen-max-0919", "flashcat.tag.route": "gateway",
	} {
		if got := a[k].AsString(); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for k, want := range map[string]int64{
		"gen_ai.usage.input_tokens": 30, "gen_ai.usage.output_tokens": 5,
		"gen_ai.usage.cache_read.input_tokens": 10, "gen_ai.usage.cache_write.input_tokens": 2,
	} {
		if got := a[k].AsInt64(); got != want {
			t.Errorf("%s = %d, want %d", k, got, want)
		}
	}
	if got := a["gen_ai.response.finish_reasons"].AsStringSlice(); !slices.Equal(got, []string{"stop"}) {
		t.Errorf("finish_reasons = %v", got)
	}
	var in []message
	if err := json.Unmarshal([]byte(a["gen_ai.input.messages"].AsString()), &in); err != nil || len(in) != 2 ||
		in[0].Role != "system" || in[1].Parts[0].Content != "我的订单到哪了" {
		t.Errorf("input messages = %s", a["gen_ai.input.messages"].AsString())
	}
	if !strings.Contains(a["gen_ai.output.messages"].AsString(), `"role":"assistant"`) {
		t.Errorf("output messages = %s", a["gen_ai.output.messages"].AsString())
	}

	emb := attrMap(spanByName(t, rec, "embeddings text-embedding-v3"))
	if emb["gen_ai.usage.input_tokens"].AsInt64() != 8 {
		t.Error("embedding input_tokens")
	}
	if _, ok := emb["gen_ai.usage.output_tokens"]; ok {
		t.Error("the embedding span should record only input tokens")
	}

	ret := spanByName(t, rec, "retrieval kb-orders")
	r := attrMap(ret)
	if ret.SpanKind() != trace.SpanKindClient || r["gen_ai.data_source.id"].AsString() != "kb-orders" ||
		r["gen_ai.retrieval.query.text"].AsString() != "我的订单到哪了" {
		t.Errorf("retrieval attrs = %v", r)
	}
	var docs []map[string]any
	if err := json.Unmarshal([]byte(r["gen_ai.retrieval.documents"].AsString()), &docs); err != nil ||
		len(docs) != 2 || docs[0]["id"] != "d1" || docs[1]["content"] != "第二段文本" {
		t.Errorf("documents = %s", r["gen_ai.retrieval.documents"].AsString())
	}
}

func TestAnnotateRespectsContentSwitchAndScope(t *testing.T) {
	rec := useRecorder(t, false)
	ctx := context.Background()
	Annotate(ctx, Annotation{Output: "不在语义 span 里", Tags: map[string]string{"a": "b"}}) // No effect without a semantic API span.
	Annotate(nil, Annotation{Output: "x"})                                              //nolint:staticcheck // A nil context is safe.
	_ = Trace(ctx, "job", func(ctx context.Context) error {
		Annotate(ctx, Annotation{Tags: map[string]string{"plain": "1"}}) // Trace creates a plain span.
		return LLM(ctx, "m1", func(ctx context.Context) error {
			Annotate(ctx, Annotation{Input: "secret prompt", Output: "secret answer", Usage: &Usage{InputTokens: 3}})
			return nil
		})
	})
	chat := attrMap(spanByName(t, rec, "chat m1"))
	if chat["gen_ai.usage.input_tokens"].AsInt64() != 3 {
		t.Error("usage should be recorded with content capture off")
	}
	for _, k := range []string{"gen_ai.input.messages", "gen_ai.output.messages"} {
		if _, ok := chat[k]; ok {
			t.Errorf("%s should be omitted when content capture is off", k)
		}
	}
	if _, ok := attrMap(spanByName(t, rec, "job"))["flashcat.tag.plain"]; ok {
		t.Error("Annotate should leave Trace spans unchanged")
	}
}

func TestRetrievalDocumentsStayValidJSONWhenTruncated(t *testing.T) {
	rec := useRecorder(t, true)
	_, sp := StartRetrieval(context.Background(), "big")
	docs := make([]map[string]string, 50)
	for i := range docs {
		docs[i] = map[string]string{"id": "d", "content": strings.Repeat("x", 2000)}
	}
	sp.SetOutput(docs)
	sp.End(nil)
	_, single := StartRetrieval(context.Background(), "single")
	single.SetOutput("只有一段")
	single.End(nil)

	var got []map[string]any
	if err := json.Unmarshal([]byte(attrMap(spanByName(t, rec, "retrieval big"))["gen_ai.retrieval.documents"].AsString()), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) >= 50 || got[len(got)-1]["content"] != truncatedMark {
		t.Errorf("len=%d last=%v", len(got), got[len(got)-1])
	}
	if s := attrMap(spanByName(t, rec, "retrieval single"))["gen_ai.retrieval.documents"].AsString(); s != `[{"content":"只有一段"}]` {
		t.Errorf("single = %s", s)
	}
}

func TestNestedInferenceUnderManualLLMIsReported(t *testing.T) {
	useRecorder(t, false)
	before := global.nested.Load()
	t.Cleanup(func() { global.nested.Store(before) })
	tr := otel.Tracer("flashcat-otel/go/instrumentation/openai")

	_ = LLM(context.Background(), "gpt-4o", func(ctx context.Context) error {
		_, s := tr.Start(ctx, "chat gpt-4o", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
		s.End()
		_, s = tr.Start(ctx, "ChatCompletion", trace.WithAttributes(attribute.String("openinference.span.kind", "LLM")))
		s.End()
		return nil
	})
	// Count only inference spans whose direct parent is a manual LLM span.
	_, s := tr.Start(context.Background(), "chat outside", trace.WithAttributes(attribute.String("gen_ai.operation.name", "chat")))
	s.End()

	if got := global.nested.Load() - before; got != 2 {
		t.Fatalf("nested = %d, want 2", got)
	}
	d := GetDiagnostics()
	n := 0
	for _, p := range d.Problems {
		if strings.Contains(p, "重复计数") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("problems = %v", d.Problems)
	}
}
