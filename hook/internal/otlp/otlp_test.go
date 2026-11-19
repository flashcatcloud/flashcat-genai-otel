package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

var t0 = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

func sampleTurn() model.Turn {
	return model.Turn{
		Tool: model.ToolClaude, SessionID: "sess-1", TurnKey: "prompt-1", ToolVersion: "2.1.311",
		UserID: "dev@example.com", Repo: "github.com/acme/app", GitBranch: "main", Cwd: "/w",
		Start: t0, End: t0.Add(10 * time.Second),
		Input: "fix the bug", Output: "done",
		Status: model.StatusOK,
		Steps: []model.Step{
			{Kind: model.StepLLM, Key: "msg_1", Start: t0.Add(time.Second), End: t0.Add(2 * time.Second), LLM: &model.LLMCall{
				Provider: "anthropic", RequestModel: "claude-opus-5-5", ResponseModel: "claude-opus-5-5", ResponseID: "msg_1",
				FinishReasons: []string{"tool_use"},
				Usage:         model.Usage{InputTokens: 1500, OutputTokens: 20, CacheReadTokens: 1000, CacheWriteTokens: 400, ReasoningTokens: 5},
				InputMessages: []model.Message{{Role: "user", Parts: []model.Part{{Type: "text", Content: "fix the bug"}}}},
				OutputMessages: []model.Message{{Role: "assistant", Parts: []model.Part{
					{Type: "reasoning", Content: "think"},
					{Type: "tool_call", ID: "toolu_1", Name: "Bash", Arguments: `{"command":"ls"}`},
				}}},
			}},
			{Kind: model.StepTool, Key: "toolu_1", Start: t0.Add(2 * time.Second), End: t0.Add(3 * time.Second), Tool: &model.ToolCall{
				Name: "Bash", CallID: "toolu_1", Arguments: `{"command":"ls"}`, Result: "a.go", Status: model.StatusOK,
			}},
			{Kind: model.StepTool, Key: "toolu_2", Start: t0.Add(3 * time.Second), End: t0.Add(8 * time.Second), Tool: &model.ToolCall{
				Name: "Agent", CallID: "toolu_2", Arguments: `{"prompt":"review"}`, Result: "ok", Status: model.StatusOK,
				Subagent: &model.Subagent{Type: "reviewer", ID: "a1", Start: t0.Add(3 * time.Second), End: t0.Add(8 * time.Second),
					Input: "review", Output: "lgtm", Status: model.StatusOK,
					Steps: []model.Step{
						{Kind: model.StepLLM, Key: "msg_s1", Start: t0.Add(4 * time.Second), End: t0.Add(5 * time.Second), LLM: &model.LLMCall{
							Provider: "anthropic", RequestModel: "claude-haiku-5-5", ResponseModel: "claude-haiku-5-5", ResponseID: "msg_s1",
							Usage: model.Usage{InputTokens: 10},
						}},
						{Kind: model.StepTool, Key: "toolu_s1", Start: t0.Add(5 * time.Second), End: t0.Add(6 * time.Second), Tool: &model.ToolCall{
							Name: "Read", CallID: "toolu_s1", Status: model.StatusError, ErrorMessage: "no such file",
						}},
					}},
			}},
			{Kind: model.StepLLM, Key: "msg_2", Start: t0.Add(9 * time.Second), End: t0.Add(10 * time.Second), LLM: &model.LLMCall{
				Provider: "anthropic", RequestModel: "claude-opus-5-5", ResponseModel: "claude-opus-5-5", ResponseID: "msg_2",
				FinishReasons:   []string{"end_turn"},
				InputMessages:   []model.Message{{Role: "tool", Parts: []model.Part{{Type: "tool_call_response", ID: "toolu_1", Response: "a.go"}}}},
				OutputMessages:  []model.Message{{Role: "assistant", Parts: []model.Part{{Type: "text", Content: "done"}}}},
				ReportedCostUSD: 0.25,
			}},
		},
	}
}

var res = Resource{App: "coding-test", HostName: "host-1", HookVersion: "1.2.3"}

func spansByName(td ptrace.Traces) map[string]ptrace.Span {
	out := map[string]ptrace.Span{}
	forEach(td, func(_ pcommon.Resource, s ptrace.Span) { out[s.Name()] = s })
	return out
}

func forEach(td ptrace.Traces, f func(pcommon.Resource, ptrace.Span)) {
	rss := td.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		sss := rss.At(i).ScopeSpans()
		for j := 0; j < sss.Len(); j++ {
			sp := sss.At(j).Spans()
			for k := 0; k < sp.Len(); k++ {
				f(rss.At(i).Resource(), sp.At(k))
			}
		}
	}
}

func str(t *testing.T, s ptrace.Span, k string) string {
	t.Helper()
	v, ok := s.Attributes().Get(k)
	if !ok {
		t.Fatalf("%s: missing %s", s.Name(), k)
	}
	return v.AsString()
}

func has(s ptrace.Span, k string) bool {
	_, ok := s.Attributes().Get(k)
	return ok
}

func TestResourceAndCommonAttributes(t *testing.T) {
	td := Build([]model.Turn{sampleTurn()}, res, Options{})
	if td.ResourceSpans().Len() != 1 {
		t.Fatalf("resource spans = %d", td.ResourceSpans().Len())
	}
	ra := td.ResourceSpans().At(0).Resource().Attributes()
	want := map[string]string{
		"service.name": "coding-test", "service.version": "2.1.311", "telemetry.sdk.name": "flashcat-hook",
		"telemetry.sdk.version": "1.2.3", "flashcat.source": "coding_agent", "flashcat.tag.coding_tool": "claude-code",
		"host.name": "host-1",
	}
	for k, v := range want {
		got, ok := ra.Get(k)
		if !ok || got.AsString() != v {
			t.Errorf("resource %s = %v, want %s", k, got.AsString(), v)
		}
	}
	if td.SpanCount() != 8 {
		t.Fatalf("spans = %d, want 8", td.SpanCount())
	}
	forEach(td, func(_ pcommon.Resource, s ptrace.Span) {
		if str(t, s, "gen_ai.agent.name") != "claude-code" || str(t, s, "gen_ai.conversation.id") != "sess-1" ||
			str(t, s, "user.id") != "dev@example.com" || str(t, s, "flashcat.tag.repo") != "github.com/acme/app" ||
			str(t, s, "flashcat.tag.git_branch") != "main" {
			t.Errorf("%s: common attributes wrong", s.Name())
		}
		if has(s, "gen_ai.usage.total_tokens") {
			t.Errorf("%s has total_tokens", s.Name())
		}
		if !s.EndTimestamp().AsTime().After(s.StartTimestamp().AsTime()) {
			t.Errorf("%s: zero-length span", s.Name())
		}
	})
}

func TestSpanShapes(t *testing.T) {
	turn := sampleTurn()
	td := Build([]model.Turn{turn}, res, Options{})
	sp := spansByName(td)
	tid := TraceID("claude-code", "sess-1", "prompt-1")

	root := sp["invoke_agent claude-code"]
	if root.Kind() != ptrace.SpanKindInternal || str(t, root, "gen_ai.operation.name") != "invoke_agent" ||
		!root.ParentSpanID().IsEmpty() || root.SpanID() != RootSpanID(tid) || root.TraceID() != tid {
		t.Errorf("root wrong")
	}
	if root.Status().Code() != ptrace.StatusCodeUnset {
		t.Errorf("ok turn has status %v", root.Status().Code())
	}

	chat := sp["chat claude-opus-5-5"]
	if chat.Kind() != ptrace.SpanKindClient || chat.ParentSpanID() != root.SpanID() {
		t.Errorf("chat kind/parent wrong")
	}
	checks := map[string]string{
		"gen_ai.operation.name": "chat", "gen_ai.provider.name": "anthropic", "gen_ai.request.model": "claude-opus-5-5",
		"gen_ai.response.model": "claude-opus-5-5",
	}
	for k, v := range checks {
		if str(t, chat, k) != v {
			t.Errorf("chat %s = %s", k, str(t, chat, k))
		}
	}
	// msg_2 is the later chat span with the same name; find msg_1 by response id.
	var first, second ptrace.Span
	forEach(td, func(_ pcommon.Resource, s ptrace.Span) {
		if v, ok := s.Attributes().Get("gen_ai.response.id"); ok {
			switch v.Str() {
			case "msg_1":
				first = s
			case "msg_2":
				second = s
			}
		}
	})
	ints := map[string]int64{
		"gen_ai.usage.input_tokens": 1500, "gen_ai.usage.output_tokens": 20, "gen_ai.usage.cache_read.input_tokens": 1000,
		"gen_ai.usage.cache_write.input_tokens": 400, "gen_ai.usage.reasoning.output_tokens": 5,
	}
	for k, v := range ints {
		got, ok := first.Attributes().Get(k)
		if !ok || got.Int() != v {
			t.Errorf("%s = %v, want %d", k, got.AsRaw(), v)
		}
	}
	fr, _ := first.Attributes().Get("gen_ai.response.finish_reasons")
	if fr.Type() != pcommon.ValueTypeSlice || fr.Slice().At(0).Str() != "tool_use" {
		t.Errorf("finish_reasons = %v", fr.AsRaw())
	}
	// msg_2 has no usage at all: no token attributes; cost is reported.
	for _, k := range []string{"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "gen_ai.usage.cache_read.input_tokens"} {
		if has(second, k) {
			t.Errorf("msg_2 has %s", k)
		}
	}
	if c, ok := second.Attributes().Get("llm.cost.total"); !ok || c.Double() != 0.25 {
		t.Errorf("llm.cost.total = %v", c.AsRaw())
	}
	if has(first, "llm.cost.total") {
		t.Error("cost written without a reported cost")
	}

	// Subagent chat: input 10, output 0 → both written; cache attrs omitted.
	sub := sp["chat claude-haiku-5-5"]
	in, _ := sub.Attributes().Get("gen_ai.usage.input_tokens")
	out, okOut := sub.Attributes().Get("gen_ai.usage.output_tokens")
	if in.Int() != 10 || !okOut || out.Int() != 0 || has(sub, "gen_ai.usage.cache_read.input_tokens") || has(sub, "gen_ai.usage.reasoning.output_tokens") {
		t.Errorf("zero-token rule broken: in=%v out=%v", in.AsRaw(), out.AsRaw())
	}

	bash := sp["execute_tool Bash"]
	if bash.Kind() != ptrace.SpanKindInternal || str(t, bash, "gen_ai.operation.name") != "execute_tool" ||
		str(t, bash, "gen_ai.tool.name") != "Bash" || str(t, bash, "gen_ai.tool.call.id") != "toolu_1" ||
		bash.SpanID() != StepSpanID(tid, "toolu_1") || bash.ParentSpanID() != root.SpanID() {
		t.Errorf("tool span wrong")
	}

	agentTool := sp["execute_tool Agent"]
	subSpan := sp["invoke_agent reviewer"]
	if subSpan.ParentSpanID() != agentTool.SpanID() || str(t, subSpan, "gen_ai.operation.name") != "invoke_agent" ||
		str(t, subSpan, "flashcat.tag.subagent") != "reviewer" || str(t, subSpan, "gen_ai.agent.name") != "claude-code" ||
		subSpan.SpanID() != SubagentSpanID(tid, "toolu_2") {
		t.Errorf("subagent span wrong")
	}
	if sub.ParentSpanID() != subSpan.SpanID() || sub.SpanID() != StepSpanID(tid, "toolu_2/msg_s1") {
		t.Errorf("subagent child parent/id wrong")
	}
	read := sp["execute_tool Read"]
	if read.Status().Code() != ptrace.StatusCodeError || read.Status().Message() != "no such file" {
		t.Errorf("failed tool status = %v %q", read.Status().Code(), read.Status().Message())
	}
}

func TestRootStatus(t *testing.T) {
	for _, c := range []struct {
		st   model.Status
		msg  string
		want string
	}{{model.StatusAborted, "", "aborted"}, {model.StatusError, "rate_limit", "rate_limit"}} {
		turn := sampleTurn()
		turn.Status, turn.StatusMessage = c.st, c.msg
		root := spansByName(Build([]model.Turn{turn}, res, Options{}))["invoke_agent claude-code"]
		if root.Status().Code() != ptrace.StatusCodeError || root.Status().Message() != c.want {
			t.Errorf("%s: status %v %q", c.st, root.Status().Code(), root.Status().Message())
		}
	}
}

func TestContentLevels(t *testing.T) {
	contentKeys := []string{"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "gen_ai.system_instructions"}
	t.Run("none", func(t *testing.T) {
		forEach(Build([]model.Turn{sampleTurn()}, res, Options{Content: model.ContentNone}), func(_ pcommon.Resource, s ptrace.Span) {
			for _, k := range contentKeys {
				if has(s, k) {
					t.Errorf("%s has %s at none", s.Name(), k)
				}
			}
		})
	})
	t.Run("no_tool_content", func(t *testing.T) {
		turn := sampleTurn()
		turn.SystemInstructions = "be brief"
		sp := spansByName(Build([]model.Turn{turn}, res, Options{Content: model.ContentNoToolContent}))
		root := sp["invoke_agent claude-code"]
		if str(t, root, "gen_ai.input.messages") != `[{"role":"user","parts":[{"type":"text","content":"fix the bug"}]}]` {
			t.Errorf("root input = %s", str(t, root, "gen_ai.input.messages"))
		}
		if !strings.Contains(str(t, root, "gen_ai.output.messages"), `"content":"done"`) {
			t.Errorf("root output = %s", str(t, root, "gen_ai.output.messages"))
		}
		var first ptrace.Span
		forEach(Build([]model.Turn{turn}, res, Options{Content: model.ContentNoToolContent}), func(_ pcommon.Resource, s ptrace.Span) {
			if v, ok := s.Attributes().Get("gen_ai.response.id"); ok && v.Str() == "msg_1" {
				first = s
			}
		})
		out := str(t, first, "gen_ai.output.messages")
		if strings.Contains(out, "arguments") || !strings.Contains(out, `"name":"Bash"`) || !strings.Contains(out, `"id":"toolu_1"`) {
			t.Errorf("chat output at no_tool_content = %s", out)
		}
		if str(t, first, "gen_ai.system_instructions") != `[{"type":"text","content":"be brief"}]` {
			t.Errorf("system instructions = %s", str(t, first, "gen_ai.system_instructions"))
		}
		forEach(Build([]model.Turn{turn}, res, Options{Content: model.ContentNoToolContent}), func(_ pcommon.Resource, s ptrace.Span) {
			if v, ok := s.Attributes().Get("gen_ai.response.id"); ok && v.Str() == "msg_2" {
				in := str(t, s, "gen_ai.input.messages")
				if strings.Contains(in, `"response":`) || !strings.Contains(in, `"tool_call_response"`) {
					t.Errorf("tool response kept at no_tool_content: %s", in)
				}
				if has(s, "gen_ai.system_instructions") {
					t.Error("system instructions on a later chat span")
				}
			}
			if has(s, "gen_ai.tool.call.arguments") || has(s, "gen_ai.tool.call.result") {
				t.Errorf("%s has tool content at no_tool_content", s.Name())
			}
		})
		subSpan := sp["invoke_agent reviewer"]
		if !strings.Contains(str(t, subSpan, "gen_ai.input.messages"), "review") {
			t.Error("subagent input missing")
		}
	})
	t.Run("full", func(t *testing.T) {
		td := Build([]model.Turn{sampleTurn()}, res, Options{Content: model.ContentFull})
		sp := spansByName(td)
		bash := sp["execute_tool Bash"]
		if str(t, bash, "gen_ai.tool.call.arguments") != `{"command":"ls"}` || str(t, bash, "gen_ai.tool.call.result") != "a.go" {
			t.Error("tool content missing at full")
		}
		forEach(td, func(_ pcommon.Resource, s ptrace.Span) {
			if v, ok := s.Attributes().Get("gen_ai.response.id"); ok && v.Str() == "msg_1" {
				out := str(t, s, "gen_ai.output.messages")
				if !strings.Contains(out, `"arguments":{"command":"ls"}`) {
					t.Errorf("tool_call arguments not embedded as JSON: %s", out)
				}
				var msgs []map[string]any
				if err := json.Unmarshal([]byte(out), &msgs); err != nil || msgs[0]["finish_reason"] != "tool_use" {
					t.Errorf("output messages %s: %v", out, err)
				}
			}
			if v, ok := s.Attributes().Get("gen_ai.response.id"); ok && v.Str() == "msg_2" {
				if !strings.Contains(str(t, s, "gen_ai.input.messages"), `"response":"a.go"`) {
					t.Error("tool response missing at full")
				}
			}
		})
	})
}

func TestTruncation(t *testing.T) {
	turn := sampleTurn()
	long := strings.Repeat("长文本", 400) // 1200 chars
	turn.Input = long
	turn.Steps[0].LLM.InputMessages[0].Parts[0].Content = long
	turn.Steps[0].LLM.OutputMessages[0].Parts[1].Arguments = `{"command":"` + strings.Repeat("x", 1000) + `"}`
	turn.Steps[1].Tool.Result = long
	const limit = 300
	td := Build([]model.Turn{turn}, res, Options{Content: model.ContentFull, MaxAttrChars: limit})
	truncated := 0
	forEach(td, func(_ pcommon.Resource, s ptrace.Span) {
		cut := false
		s.Attributes().Range(func(k string, v pcommon.Value) bool {
			if v.Type() != pcommon.ValueTypeStr {
				return true
			}
			if n := utf8.RuneCountInString(v.Str()); n > limit {
				t.Errorf("%s %s has %d chars", s.Name(), k, n)
			}
			if strings.HasSuffix(k, ".messages") && !json.Valid([]byte(v.Str())) {
				t.Errorf("%s %s is not valid JSON after truncation", s.Name(), k)
			}
			if strings.Contains(v.Str(), truncMark) {
				cut = true
			}
			return true
		})
		if cut {
			truncated++
			if v, ok := s.Attributes().Get("flashcat.content_truncated"); !ok || !v.Bool() {
				t.Errorf("%s truncated without flag", s.Name())
			}
		} else if has(s, "flashcat.content_truncated") {
			t.Errorf("%s flagged without truncation", s.Name())
		}
	})
	if truncated < 3 {
		t.Errorf("only %d spans truncated", truncated)
	}
}

func TestDeterministicIDs(t *testing.T) {
	ids := func(opt Options) []string {
		var out []string
		forEach(Build([]model.Turn{sampleTurn()}, res, opt), func(_ pcommon.Resource, s ptrace.Span) {
			out = append(out, s.TraceID().String()+"/"+s.SpanID().String()+"/"+s.ParentSpanID().String())
		})
		return out
	}
	a, b := ids(Options{}), ids(Options{Content: model.ContentFull})
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatal("ids differ between runs")
	}
	h := sha256.Sum256([]byte("claude-code|sess-1|prompt-1"))
	tid := TraceID("claude-code", "sess-1", "prompt-1")
	if !bytes.Equal(tid[:], h[:16]) {
		t.Fatal("trace id formula")
	}
	rh := sha256.Sum256(append(append([]byte{}, tid[:]...), "root"...))
	rid := RootSpanID(tid)
	if !bytes.Equal(rid[:], rh[:8]) {
		t.Fatal("root span id formula")
	}
	other := sampleTurn()
	other.TurnKey = "prompt-2"
	var otherTrace pcommon.TraceID
	forEach(Build([]model.Turn{other}, res, Options{}), func(_ pcommon.Resource, s ptrace.Span) { otherTrace = s.TraceID() })
	if otherTrace == tid {
		t.Fatal("different turns share a trace id")
	}
	seen := map[string]bool{}
	for _, x := range a {
		span := strings.Split(x, "/")[1]
		if seen[span] {
			t.Fatalf("duplicate span id %s", span)
		}
		seen[span] = true
	}
}

func TestDuplicateKeysGetDistinctIDs(t *testing.T) {
	turn := sampleTurn()
	turn.Steps[1].Key = "dup"
	turn.Steps[2].Key = "dup"
	seen := map[pcommon.SpanID]bool{}
	forEach(Build([]model.Turn{turn}, res, Options{}), func(_ pcommon.Resource, s ptrace.Span) {
		if seen[s.SpanID()] {
			t.Fatalf("duplicate span id for %s", s.Name())
		}
		seen[s.SpanID()] = true
	})
}

func TestTraceParent(t *testing.T) {
	tp := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	plain := spansByName(Build([]model.Turn{sampleTurn()}, res, Options{}))
	with := spansByName(Build([]model.Turn{sampleTurn()}, res, Options{TraceParent: tp}))
	root := with["invoke_agent claude-code"]
	if root.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" || root.ParentSpanID().String() != "00f067aa0ba902b7" {
		t.Fatalf("root not attached to TRACEPARENT: %s %s", root.TraceID(), root.ParentSpanID())
	}
	for name, s := range with {
		if s.TraceID() != root.TraceID() {
			t.Errorf("%s not in the external trace", name)
		}
		if s.SpanID() != plain[name].SpanID() {
			t.Errorf("%s span id changed with TRACEPARENT", name)
		}
	}
	for _, bad := range []string{"", "garbage", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01"} {
		if _, ok := ParseTraceParent(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestTimeInference(t *testing.T) {
	turn := model.Turn{Tool: model.ToolCodex, SessionID: "s", TurnKey: "k", Start: t0, End: t0.Add(time.Second), Status: model.StatusOK,
		Steps: []model.Step{
			{Kind: model.StepLLM, Key: "a", Start: t0.Add(100 * time.Millisecond), LLM: &model.LLMCall{Provider: "openai"}},
			{Kind: model.StepTool, Key: "b", Tool: &model.ToolCall{Name: "x"}},
			{Kind: model.StepLLM, Key: "c", Start: t0.Add(2 * time.Second), End: t0.Add(2 * time.Second), LLM: &model.LLMCall{Provider: "openai"}},
		}}
	sp := map[string]ptrace.Span{}
	var root ptrace.Span
	forEach(Build([]model.Turn{turn}, res, Options{}), func(_ pcommon.Resource, s ptrace.Span) {
		if s.ParentSpanID().IsEmpty() {
			root = s
			return
		}
		sp[s.Name()] = s
	})
	for name, s := range sp {
		d := s.EndTimestamp().AsTime().Sub(s.StartTimestamp().AsTime())
		if d < time.Millisecond {
			t.Errorf("%s lasts %v", name, d)
		}
		if s.StartTimestamp() < root.StartTimestamp() || s.EndTimestamp() > root.EndTimestamp() {
			t.Errorf("%s outside root", name)
		}
	}
	tool := sp["execute_tool x"]
	if !tool.StartTimestamp().AsTime().Equal(t0.Add(100*time.Millisecond + time.Millisecond)) {
		t.Errorf("tool start inferred as %v", tool.StartTimestamp().AsTime())
	}
	if root.EndTimestamp().AsTime().Before(t0.Add(2*time.Second + time.Millisecond)) {
		t.Errorf("root does not cover children: %v", root.EndTimestamp().AsTime())
	}
}

func TestProbe(t *testing.T) {
	td := Probe(res, model.ToolCursor, t0)
	if td.SpanCount() != 1 {
		t.Fatal("probe span count")
	}
	ra := td.ResourceSpans().At(0).Resource().Attributes()
	if v, _ := ra.Get("flashcat.source"); v.Str() != "coding_agent" {
		t.Error("probe resource lacks flashcat.source")
	}
	if v, _ := ra.Get("service.name"); v.Str() != "coding-test" {
		t.Error("probe service.name")
	}
	s := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	if s.Name() != "flashcat-hook probe" || str(t, s, "flashcat.span.kind") != "onboarding_probe" || str(t, s, "gen_ai.agent.name") != "cursor" {
		t.Error("probe span attributes")
	}
}

// --- sender ---

type captured struct {
	mu    sync.Mutex
	reqs  []*http.Request
	spans []int
}

func server(t *testing.T, c *captured, handler func(w http.ResponseWriter, r *http.Request, td ptrace.Traces)) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("body not gzip: %v", err)
			return
		}
		raw, _ := io.ReadAll(zr)
		req := ptraceotlp.NewExportRequest()
		if err := req.UnmarshalProto(raw); err != nil {
			t.Errorf("body not OTLP protobuf: %v", err)
		}
		c.mu.Lock()
		c.reqs = append(c.reqs, r)
		c.spans = append(c.spans, req.Traces().SpanCount())
		c.mu.Unlock()
		handler(w, r, req.Traces())
	}))
	t.Cleanup(s.Close)
	return s
}

func TestSendHeadersAndSuccess(t *testing.T) {
	c := &captured{}
	s := server(t, c, func(w http.ResponseWriter, r *http.Request, _ ptrace.Traces) { w.WriteHeader(200) })
	snd := &Sender{Endpoint: s.URL + "/v1/traces", Key: "kid:secret", UserAgent: "flashcat-hook/test"}
	td := Build([]model.Turn{sampleTurn()}, res, Options{})
	if err := snd.Send(context.Background(), td); err != nil {
		t.Fatal(err)
	}
	r := c.reqs[0]
	if r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("Content-Encoding") != "gzip" ||
		r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("kid:secret")) || r.Method != http.MethodPost {
		t.Errorf("headers: %v", r.Header)
	}
	if c.spans[0] != 8 {
		t.Errorf("server got %d spans", c.spans[0])
	}
}

func TestSendFailures(t *testing.T) {
	td := Build([]model.Turn{sampleTurn()}, res, Options{})
	t.Run("http status", func(t *testing.T) {
		s := server(t, &captured{}, func(w http.ResponseWriter, r *http.Request, _ ptrace.Traces) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(401)
			_, _ = w.Write([]byte("invalid key"))
		})
		err := (&Sender{Endpoint: s.URL, Key: "k:s"}).Send(context.Background(), td)
		var he *HTTPError
		if err == nil || !asHTTP(err, &he) || he.Status != 401 || !strings.Contains(err.Error(), "invalid key") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("partial success", func(t *testing.T) {
		s := server(t, &captured{}, func(w http.ResponseWriter, r *http.Request, _ ptrace.Traces) {
			resp := ptraceotlp.NewExportResponse()
			resp.PartialSuccess().SetRejectedSpans(2)
			resp.PartialSuccess().SetErrorMessage("bad spans")
			b, _ := resp.MarshalProto()
			w.Header().Set("Content-Type", "application/x-protobuf")
			_, _ = w.Write(b)
		})
		err := (&Sender{Endpoint: s.URL, Key: "k:s"}).Send(context.Background(), td)
		if err == nil || !strings.Contains(err.Error(), "rejected 2") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		s := server(t, &captured{}, func(w http.ResponseWriter, r *http.Request, _ ptrace.Traces) {
			time.Sleep(300 * time.Millisecond)
		})
		start := time.Now()
		err := (&Sender{Endpoint: s.URL, Key: "k:s", Timeout: 50 * time.Millisecond}).Send(context.Background(), td)
		if err == nil || time.Since(start) > 250*time.Millisecond {
			t.Fatalf("err = %v after %v", err, time.Since(start))
		}
	})
}

func asHTTP(err error, target **HTTPError) bool {
	he, ok := err.(*HTTPError)
	if ok {
		*target = he
	}
	return ok
}

func TestSendSplitsLargeRequests(t *testing.T) {
	var turns []model.Turn
	for i := 0; i < 20; i++ {
		tr := sampleTurn()
		tr.TurnKey = strings.Repeat("k", i+1)
		turns = append(turns, tr)
	}
	td := Build(turns, res, Options{Content: model.ContentFull})
	c := &captured{}
	s := server(t, c, func(w http.ResponseWriter, r *http.Request, _ ptrace.Traces) { w.WriteHeader(200) })
	var sizer ptrace.ProtoMarshaler
	max := sizer.TracesSize(td) / 5
	if err := (&Sender{Endpoint: s.URL, Key: "k:s", MaxBytes: max}).Send(context.Background(), td); err != nil {
		t.Fatal(err)
	}
	if len(c.spans) < 5 {
		t.Fatalf("requests = %d, want >= 5", len(c.spans))
	}
	total := 0
	for _, n := range c.spans {
		total += n
	}
	if total != td.SpanCount() {
		t.Fatalf("spans delivered %d, built %d", total, td.SpanCount())
	}
	for _, part := range Split(td, max) {
		if sizer.TracesSize(part) > max && part.SpanCount() > 1 {
			t.Errorf("part of %d bytes over %d", sizer.TracesSize(part), max)
		}
		// Each part keeps the resource.
		if v, _ := part.ResourceSpans().At(0).Resource().Attributes().Get("flashcat.source"); v.Str() != "coding_agent" {
			t.Error("split part lost its resource")
		}
	}
}

func TestUsageInOtherCallWritesExplicitZeros(t *testing.T) {
	turn := sampleTurn()
	turn.Steps[0].LLM.Usage = model.Usage{}
	turn.Steps[0].LLM.UsageInOtherCall = true
	td := Build([]model.Turn{turn}, res, Options{})
	var flagged ptrace.Span
	forEach(td, func(_ pcommon.Resource, s ptrace.Span) {
		if s.SpanID() == StepSpanID(TraceID("claude-code", "sess-1", "prompt-1"), turn.Steps[0].Key) {
			flagged = s
		}
	})
	in, ok1 := flagged.Attributes().Get("gen_ai.usage.input_tokens")
	out, ok2 := flagged.Attributes().Get("gen_ai.usage.output_tokens")
	agg, ok3 := flagged.Attributes().Get("flashcat.usage.aggregated")
	if !ok1 || !ok2 || !ok3 || in.Int() != 0 || out.Int() != 0 || !agg.Bool() {
		t.Fatalf("explicit zero usage not written: %v", flagged.Attributes().AsRaw())
	}
	// a call with its own usage keeps it and is not marked
	other := spansByName(td)["chat claude-opus-5-5"]
	if has(other, "flashcat.usage.aggregated") && other.SpanID() != flagged.SpanID() {
		t.Fatalf("call with usage marked aggregated")
	}
}
