package cursor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

// The fixture is one real Cursor 3.29.9 turn rebuilt from a shape capture
// with placeholder strings and fresh ids, see testdata/README.md.
const fxTurn = "testdata/turn_subagent.jsonl"

// t0 is the fixture's time base: received_at = t0 + seconds.
var t0 = time.Unix(1790000000, 0)

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }

type fxEvent struct {
	At  time.Time
	Raw []byte
	P   map[string]any
}

func (f fxEvent) name() string        { return f.str("hook_event_name") }
func (f fxEvent) str(k string) string { s, _ := f.P[k].(string); return s }

func readFixture(t *testing.T) []fxEvent {
	t.Helper()
	data, err := os.ReadFile(fxTurn)
	if err != nil {
		t.Fatal(err)
	}
	var out []fxEvent
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var rec struct {
			ReceivedAt float64         `json:"received_at"`
			Payload    json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		f := fxEvent{At: time.Unix(0, int64(math.Round(rec.ReceivedAt*1e6))*1000), Raw: rec.Payload}
		if err := json.Unmarshal(rec.Payload, &f.P); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

// fixture ids
type fxIDs struct {
	parent, child, gen, childGen, task, grep string
}

func ids(t *testing.T, evs []fxEvent) fxIDs {
	t.Helper()
	var x fxIDs
	for _, e := range evs {
		switch {
		case e.name() == "beforeSubmitPrompt":
			x.parent, x.gen = e.str("conversation_id"), e.str("generation_id")
		case e.name() == "subagentStop":
			x.child, x.task = e.str("child_conversation_id"), e.str("subagent_id")
		case e.name() == "postToolUse" && e.str("tool_name") == "Grep":
			x.grep = e.str("tool_use_id")
		case e.name() == "postToolUse" && e.str("parent_agent_id") != "":
			x.childGen = e.str("generation_id")
		}
	}
	if x.parent == "" || x.child == "" || x.task == "" || x.childGen == "" {
		t.Fatalf("fixture ids: %+v", x)
	}
	return x
}

// hookEvent builds what the core passes: subagent events keyed by the
// parent conversation (parent_agent_id).
func hookEvent(f fxEvent) model.HookEvent {
	sid := f.str("parent_agent_id")
	if sid == "" {
		sid = f.str("session_id")
	}
	if sid == "" {
		sid = f.str("conversation_id")
	}
	return model.HookEvent{
		Tool: model.ToolCursor, Name: f.name(), SessionID: sid, Raw: f.Raw, Now: f.At,
		Final: f.name() == "sessionEnd",
	}
}

// mkEvent builds a payload from key/value pairs on top of common fields.
func mkEvent(conv, gen, name string, sec float64, kv ...any) fxEvent {
	p := map[string]any{
		"conversation_id": conv, "session_id": conv, "generation_id": gen, "hook_event_name": name,
		"model": "grok-4.7-medium", "model_id": "grok-4.7", "cursor_version": "3.29.9",
		"workspace_roots": []string{"/tmp/redacted/project"}, "user_email": "dev@example.com",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		p[kv[i].(string)] = kv[i+1]
	}
	raw, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return fxEvent{At: at(sec), Raw: raw, P: m}
}

func (f fxEvent) with(kv ...any) fxEvent {
	p := map[string]any{}
	_ = json.Unmarshal(f.Raw, &p)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(p, kv[i].(string))
		} else {
			p[kv[i].(string)] = kv[i+1]
		}
	}
	raw, _ := json.Marshal(p)
	out := fxEvent{At: f.At, Raw: raw}
	_ = json.Unmarshal(raw, &out.P)
	return out
}

func (f fxEvent) shift(d time.Duration) fxEvent { f.At = f.At.Add(d); return f }

// apply mirrors state.Session.Apply.
func apply(prev []byte, res model.CollectResult, delivered bool) []byte {
	if len(res.Turns) == 0 || delivered {
		return res.StateOnSuccess
	}
	if res.StateOnFailure == nil {
		return prev
	}
	return res.StateOnFailure
}

// run feeds events through the parser; deliveries succeed unless fail
// returns true for the event index. It returns all turns and the state.
func run(t *testing.T, evs []fxEvent, prev []byte, fail func(int) bool) ([]model.Turn, []byte) {
	t.Helper()
	p := New()
	var turns []model.Turn
	for i, f := range evs {
		res, err := p.Collect(hookEvent(f), prev)
		if err != nil {
			t.Fatalf("event %d %s: %v", i, f.name(), err)
		}
		ok := fail == nil || !fail(i)
		if ok {
			turns = append(turns, res.Turns...)
		}
		prev = apply(prev, res, ok)
	}
	return turns, prev
}

func near(a, b time.Time) bool {
	d := a.Sub(b)
	return d > -time.Millisecond && d < time.Millisecond
}

func checkWindow(t *testing.T, what string, s model.Step, start, end float64) {
	t.Helper()
	if !near(s.Start, at(start)) || !near(s.End, at(end)) {
		t.Errorf("%s: window %s – %s, want %.3f – %.3f", what,
			s.Start.Sub(t0), s.End.Sub(t0), start, end)
	}
}

func summary(steps []model.Step) string {
	var parts []string
	for _, s := range steps {
		switch s.Kind {
		case model.StepLLM:
			parts = append(parts, s.Key)
		case model.StepTool:
			parts = append(parts, s.Tool.Name)
		}
	}
	return strings.Join(parts, ",")
}

func TestFullTurnWithSubagent(t *testing.T) {
	evs := readFixture(t)
	x := ids(t, evs)
	p := New()
	var prev []byte
	var turns []model.Turn
	for i, f := range evs {
		res, err := p.Collect(hookEvent(f), prev)
		if err != nil {
			t.Fatal(err)
		}
		if f.name() != "stop" {
			if len(res.Turns) != 0 {
				t.Fatalf("event %d %s produced turns", i, f.name())
			}
			if !bytes.Equal(res.StateOnSuccess, res.StateOnFailure) {
				t.Fatalf("event %d: success and failure states differ", i)
			}
		}
		turns = append(turns, res.Turns...)
		prev = apply(prev, res, true)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %d", len(turns))
	}
	tr := turns[0]
	if tr.Tool != model.ToolCursor || tr.TurnKey != x.gen || tr.SessionID != x.parent || tr.Status != model.StatusOK ||
		tr.Input != "[prompt:58]" || tr.Output != "[text:210]" || tr.UserID != "dev@example.com" ||
		tr.ToolVersion != "3.29.9" || tr.Cwd != "/tmp/redacted/project" {
		t.Fatalf("turn fields: %+v", tr)
	}
	if !near(tr.Start, at(86.497018)) || !near(tr.End, at(113.509764)) {
		t.Errorf("turn window %s – %s", tr.Start.Sub(t0), tr.End.Sub(t0))
	}
	if got := summary(tr.Steps); got != "llm-1,Grep,llm-2,Task,Read,Read,llm-3" {
		t.Fatalf("steps %s", got)
	}
	s := tr.Steps
	checkWindow(t, "llm-1", s[0], 86.497018, 90.906560)
	checkWindow(t, "Grep", s[1], 90.906560, 90.911573)
	checkWindow(t, "llm-2", s[2], 90.911573, 99.352468)
	checkWindow(t, "Task", s[3], 99.352468, 109.551468)
	checkWindow(t, "llm-3", s[6], 109.551468, 113.441670)

	for i, st := range s {
		if st.Kind != model.StepLLM {
			continue
		}
		c := st.LLM
		if c.Provider != "xai" || c.RequestModel != "grok-4.7" || c.ResponseModel != "grok-4.7" {
			t.Errorf("%s: provider %q model %q/%q", st.Key, c.Provider, c.RequestModel, c.ResponseModel)
		}
		want := model.Usage{}
		if i == len(s)-1 {
			want = model.Usage{InputTokens: 62537, OutputTokens: 1209, CacheReadTokens: 30464}
		}
		if c.Usage != want || c.ReportedCostUSD != 0 {
			t.Errorf("%s: usage %+v cost %v", st.Key, c.Usage, c.ReportedCostUSD)
		}
	}
	if in := s[0].LLM.InputMessages; len(in) != 1 || in[0].Parts[0].Content != "[prompt:58]" {
		t.Errorf("llm-1 input %+v", in)
	}
	if out := s[0].LLM.OutputMessages; len(out) != 1 || out[0].Parts[0].Type != "tool_call" || out[0].Parts[0].Name != "Grep" {
		t.Errorf("llm-1 output %+v", out)
	}
	if out := s[6].LLM.OutputMessages; len(out) != 1 || out[0].Parts[0].Content != "[text:210]" {
		t.Errorf("llm-3 output %+v", out)
	}
	if in := s[6].LLM.InputMessages; len(in) != 3 {
		t.Errorf("llm-3 input: want the Task and both Read results, got %+v", in)
	}

	grep := s[1].Tool
	if grep.CallID != x.grep || grep.Status != model.StatusOK || grep.Result != `{"pattern":"","success":true}` ||
		!strings.Contains(grep.Arguments, `"glob":"[glob:10]"`) {
		t.Errorf("Grep %+v", grep)
	}
	for _, r := range s[4:6] {
		if r.Tool.Status != model.StatusError || r.Tool.ErrorMessage != "error" || !strings.HasPrefix(r.Tool.Result, "[error_message:") {
			t.Errorf("Read failure %+v", r.Tool)
		}
	}
	checkWindow(t, "Read", s[4], 99.703436, 99.705362)

	task := s[3].Tool
	if task.Name != "Task" || task.CallID != x.task || !strings.Contains(task.CallID, "\n") {
		t.Fatalf("Task %+v", task)
	}
	sa := task.Subagent
	if sa == nil {
		t.Fatal("Task has no subagent")
	}
	if sa.Type != "explore" || sa.ID != x.child || sa.Status != model.StatusOK || sa.Input != "[prompt:217]" ||
		!sa.Start.Equal(s[3].Start) || !sa.End.Equal(s[3].End) {
		t.Errorf("subagent %+v", sa)
	}
	if got := summary(sa.Steps); got != "llm-1,Shell,llm-2,Shell,llm-3" {
		t.Fatalf("subagent steps %s", got)
	}
	checkWindow(t, "sub llm-1", sa.Steps[0], 99.352468, 103.629806)
	checkWindow(t, "Shell 1", sa.Steps[1], 103.629806, 105.431744)
	checkWindow(t, "sub llm-2", sa.Steps[2], 105.431744, 107.579276)
	checkWindow(t, "Shell 2", sa.Steps[3], 107.579276, 107.676163)
	checkWindow(t, "sub llm-3", sa.Steps[4], 107.676163, 109.551468)
	for _, st := range sa.Steps {
		if st.Kind == model.StepLLM && (st.LLM.Provider != "cursor" || st.LLM.ResponseModel != "composer-2.5" || st.LLM.Usage != (model.Usage{})) {
			t.Errorf("subagent %s: %+v", st.Key, st.LLM)
		}
		if st.Kind == model.StepTool && (st.Tool.Status != model.StatusOK || !strings.Contains(st.Tool.Result, "exitCode")) {
			t.Errorf("subagent tool %+v", st.Tool)
		}
	}
	if in := sa.Steps[0].LLM.InputMessages; len(in) != 1 || in[0].Parts[0].Content != "[prompt:217]" {
		t.Errorf("subagent llm-1 input %+v", in)
	}

	// The turn's events are gone; the generation and its subagent
	// generation are remembered.
	st := decodeState(prev)
	if len(st.Events) != 0 || len(st.Sent) != 1 || st.Sent[0].Gen != x.gen || !reflect.DeepEqual(st.Sent[0].Child, []string{x.childGen}) {
		t.Fatalf("state after the turn: %+v", st)
	}
}

func TestDuplicateThoughtsDeduped(t *testing.T) {
	evs := readFixture(t)
	x := ids(t, evs)
	var before []fxEvent
	thoughts := 0
	for _, f := range evs {
		if f.name() == "stop" {
			break
		}
		if f.name() == "afterAgentThought" {
			thoughts++
		}
		before = append(before, f)
	}
	if thoughts != 12 {
		t.Fatalf("fixture has %d thoughts, want 6 pairs", thoughts)
	}
	_, prev := run(t, before, nil, nil)
	st := decodeState(prev)
	var parent, child int
	for _, e := range st.Events {
		if e.Name != "afterAgentThought" {
			continue
		}
		switch e.Gen {
		case x.gen:
			parent++
		case x.childGen:
			child++
			if !e.Child {
				t.Error("child thought not marked as a subagent event")
			}
		default:
			t.Errorf("thought generation %q not normalized", e.Gen)
		}
	}
	if parent != 3 || child != 3 {
		t.Fatalf("thoughts kept: parent %d child %d, want 3 and 3", parent, child)
	}

	// Two thoughts with the same duration more than dupWindow apart are kept.
	th := evs[3]
	_, prev = run(t, []fxEvent{evs[2], th, th.shift(2 * time.Second)}, nil, nil)
	if n := len(decodeState(prev).Events); n != 3 {
		t.Fatalf("events = %d, want 3", n)
	}
	// A repeated tool event (same tool_use_id) is dropped whenever it comes.
	tool := evs[5]
	_, prev = run(t, []fxEvent{evs[2], tool, tool.shift(5 * time.Second)}, nil, nil)
	if n := len(decodeState(prev).Events); n != 2 {
		t.Fatalf("events = %d, want 2", n)
	}
}

func TestDeliveryFailureKeepsEventsAndRetriesOnce(t *testing.T) {
	evs := readFixture(t)
	x := ids(t, evs)
	stopAt := len(evs) - 1
	first, prev := run(t, evs, nil, func(i int) bool { return i == stopAt })
	if len(first) != 0 {
		t.Fatal("failed delivery counted")
	}
	st := decodeState(prev)
	if len(st.Events) == 0 || st.Events[len(st.Events)-1].Name != "stop" || len(st.Sent) != 0 {
		t.Fatalf("failure state must keep the events and the stop: %d events, sent %v", len(st.Events), st.Sent)
	}
	g2 := "00000000-0000-4000-8000-000000000002"
	next := []fxEvent{
		mkEvent(x.parent, g2, "beforeSubmitPrompt", 200, "prompt", "second"),
		mkEvent(x.parent, g2, "afterAgentResponse", 205, "text", "done", "input_tokens", 100, "output_tokens", 10),
		mkEvent(x.parent, g2, "stop", 205.1, "status", "completed", "input_tokens", 100, "output_tokens", 10),
	}
	turns, prev := run(t, next, prev, nil)
	if len(turns) != 2 || turns[0].TurnKey != x.gen || turns[1].TurnKey != g2 {
		t.Fatalf("retry turns: %d %v", len(turns), keys(turns))
	}
	// The retried turn is exactly the one a successful first delivery sends. Compare the JSON:
	// retried times come back from the state file in UTC, so with TZ=UTC their Location
	// differs from time.Local and reflect.DeepEqual fails although the instants match.
	want, _ := run(t, evs, nil, nil)
	got, _ := json.Marshal(turns[0])
	exp, _ := json.Marshal(want[0])
	if !bytes.Equal(got, exp) {
		t.Error("retried turn differs from the original")
	}
	if g := turns[1]; g.Input != "second" || g.Output != "done" || len(g.Steps) != 1 || g.Steps[0].LLM.Usage.InputTokens != 100 {
		t.Errorf("second turn %+v", g)
	}
	if st := decodeState(prev); len(st.Events) != 0 || len(st.Sent) != 2 {
		t.Fatalf("state after retry: %+v", st)
	}
	// Sent once: a repeated stop and a late event for it change nothing.
	again, prev2 := run(t, []fxEvent{next[2].shift(time.Second), next[1].shift(time.Second)}, prev, nil)
	if len(again) != 0 || len(decodeState(prev2).Events) != 0 {
		t.Fatalf("re-sent: %d turns, %d events", len(again), len(decodeState(prev2).Events))
	}
}

func keys(ts []model.Turn) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.TurnKey)
	}
	return out
}

func TestStopStatus(t *testing.T) {
	conv := "11111111-1111-4111-8111-111111111111"
	for status, want := range map[string]model.Status{"completed": model.StatusOK, "aborted": model.StatusAborted, "error": model.StatusError} {
		gen := "gen-" + status
		turns, _ := run(t, []fxEvent{
			mkEvent(conv, gen, "beforeSubmitPrompt", 1, "prompt", "p"),
			mkEvent(conv, gen, "postToolUseFailure", 3, "tool_name", "Shell", "tool_use_id", "t1", "duration", 500.0,
				"error_message", "boom", "failure_type", "timeout", "is_interrupt", status == "aborted"),
			mkEvent(conv, gen, "stop", 4, "status", status),
		}, nil, nil)
		if len(turns) != 1 || turns[0].Status != want {
			t.Fatalf("%s: %+v", status, turns)
		}
		tool := turns[0].Steps[1]
		switch status {
		case "aborted":
			if turns[0].StatusMessage != "aborted" || tool.Tool.Status != model.StatusAborted {
				t.Errorf("aborted: %q %+v", turns[0].StatusMessage, tool.Tool)
			}
		default:
			if tool.Tool.Status != model.StatusError || tool.Tool.ErrorMessage != "timeout" || tool.Tool.Result != "boom" {
				t.Errorf("%s tool %+v", status, tool.Tool)
			}
		}
		if status == "error" && turns[0].StatusMessage != "error" {
			t.Errorf("error message %q", turns[0].StatusMessage)
		}
		if got := summary(turns[0].Steps); got != "llm-1,Shell,llm-2" {
			t.Errorf("%s steps %s", status, got)
		}
	}
}

func TestSessionEndFlushesOpenGenerations(t *testing.T) {
	evs := readFixture(t)
	x := ids(t, evs)
	stopAt := len(evs) - 1
	open := append([]fxEvent(nil), evs[:stopAt]...)
	end := mkEvent(x.parent, "", "sessionEnd", 300, "reason", "window_close", "final_status", "completed")
	turns, prev := run(t, append(open, end), nil, nil)
	if len(turns) != 1 {
		t.Fatalf("turns = %d", len(turns))
	}
	tr := turns[0]
	if tr.TurnKey != x.gen || tr.Status != model.StatusAborted || tr.StatusMessage != "aborted" {
		t.Fatalf("flushed turn %+v", tr)
	}
	// Ends at its last event, not at sessionEnd; still has its subagent
	// and tokens from afterAgentResponse.
	if !near(tr.End, at(113.441670)) || summary(tr.Steps) != "llm-1,Grep,llm-2,Task,Read,Read,llm-3" ||
		tr.Steps[3].Tool.Subagent == nil || tr.Steps[6].LLM.Usage.InputTokens != 62537 {
		t.Fatalf("flushed turn %s %s", tr.End.Sub(t0), summary(tr.Steps))
	}
	if len(prev) != 0 {
		t.Fatalf("state not cleared: %s", prev)
	}

	// A stop whose delivery failed keeps its status at sessionEnd; a
	// generation without stop is aborted.
	g2 := "00000000-0000-4000-8000-000000000002"
	turns, prev = run(t, append(append([]fxEvent(nil), evs...),
		mkEvent(x.parent, g2, "beforeSubmitPrompt", 200, "prompt", "second"),
		end.shift(time.Minute)), nil, func(i int) bool { return i == stopAt })
	if len(turns) != 2 || turns[0].TurnKey != x.gen || turns[0].Status != model.StatusOK ||
		turns[1].TurnKey != g2 || turns[1].Status != model.StatusAborted {
		t.Fatalf("flush: %v", turns)
	}
	if len(prev) != 0 {
		t.Fatal("state not cleared")
	}

	// sessionEnd failing keeps everything for a later attempt.
	p := New()
	_, prev = run(t, open, nil, nil)
	res, err := p.Collect(hookEvent(end), prev)
	if err != nil || len(res.Turns) != 1 || len(decodeState(res.StateOnFailure).Events) != len(decodeState(prev).Events) {
		t.Fatalf("sessionEnd failure state: %v %d", err, len(res.Turns))
	}
}

func TestStopWithoutPrompt(t *testing.T) {
	conv := "22222222-2222-4222-8222-222222222222"
	turns, _ := run(t, []fxEvent{
		mkEvent(conv, "g", "postToolUse", 10, "tool_name", "Read", "tool_use_id", "t1", "duration", 2000.0, "tool_input", map[string]any{"file_path": "a"}),
		mkEvent(conv, "g", "stop", 12, "status", "completed", "input_tokens", 10, "output_tokens", 5,
			"cache_read_tokens", 30, "cache_write_tokens", 4, "estimated_cost", map[string]any{"raw_cost_cents": 1.5},
			"model_id", "", "model", "claude-4.5-sonnet"),
	}, nil, nil)
	if len(turns) != 1 {
		t.Fatalf("turns %d", len(turns))
	}
	tr := turns[0]
	if !near(tr.Start, at(8)) || tr.Input != "" || summary(tr.Steps) != "Read,llm-1" {
		t.Fatalf("turn %s %q %s", tr.Start.Sub(t0), tr.Input, summary(tr.Steps))
	}
	c := tr.Steps[1].LLM
	// cache counts larger than input_tokens are added to it.
	if c.Usage != (model.Usage{InputTokens: 44, OutputTokens: 5, CacheReadTokens: 30, CacheWriteTokens: 4}) ||
		c.ReportedCostUSD != 0.015 || c.Provider != "anthropic" || c.ResponseModel != "claude-4.5-sonnet" {
		t.Fatalf("llm %+v", c)
	}
	if tr.Steps[0].Tool.Arguments != `{"file_path":"a"}` {
		t.Errorf("arguments %q", tr.Steps[0].Tool.Arguments)
	}
}

// The turn is interrupted while its subagent runs: no Task postToolUse and
// no subagentStop. The subagent's events still nest under a Task step.
func TestInterruptedSubagent(t *testing.T) {
	conv, child := "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	call := "call-1\nfc_1"
	sub := func(sec float64, kv ...any) fxEvent {
		return mkEvent(child, "child-gen", "postToolUse", sec, append([]any{"parent_agent_id", conv, "model", "composer-2.5", "model_id", ""}, kv...)...)
	}
	turns, prev := run(t, []fxEvent{
		mkEvent(conv, "g", "beforeSubmitPrompt", 0, "prompt", "p"),
		sub(5, "tool_name", "Shell", "tool_use_id", "s1", "duration", 1000.0, "parent_tool_call_id", call),
		sub(7, "tool_name", "Shell", "tool_use_id", "s2", "duration", 500.0, "parent_tool_call_id", call),
		mkEvent(child, "child-gen", "afterAgentThought", 7.5, "parent_agent_id", conv, "duration_ms", 300),
		mkEvent(conv, "g", "stop", 9, "status", "aborted"),
	}, nil, nil)
	if len(turns) != 1 || turns[0].Status != model.StatusAborted {
		t.Fatalf("turns %+v", turns)
	}
	tr := turns[0]
	if got := summary(tr.Steps); got != "llm-1,Task,llm-2" {
		t.Fatalf("steps %s", got)
	}
	task := tr.Steps[1]
	if task.Key != call || task.Tool.CallID != call || task.Tool.Status != model.StatusAborted {
		t.Fatalf("task %+v", task.Tool)
	}
	checkWindow(t, "Task", task, 4, 7.5)
	sa := task.Tool.Subagent
	// The Task window starts with its first tool call, so there is no model
	// call before it.
	if sa == nil || sa.ID != child || sa.Status != model.StatusAborted || summary(sa.Steps) != "Shell,llm-1,Shell,llm-2" {
		t.Fatalf("subagent %+v", sa)
	}
	if len(decodeState(prev).Events) != 0 {
		t.Fatal("subagent events left in the state")
	}
}

func TestUnusedEventsIgnored(t *testing.T) {
	evs := readFixture(t)
	p := New()
	for _, f := range evs {
		if f.name() != "sessionStart" && f.name() != "afterShellExecution" {
			continue
		}
		res, err := p.Collect(hookEvent(f), nil)
		if err != nil || res.StateOnSuccess != nil || res.Turns != nil {
			t.Fatalf("%s: %v %s", f.name(), err, res.StateOnSuccess)
		}
	}
	if _, err := p.Collect(model.HookEvent{Name: "stop", Raw: []byte("{"), Now: t0}, nil); err == nil {
		t.Error("invalid payload accepted")
	}
	// A field of an unexpected type is skipped, not fatal.
	res, err := p.Collect(hookEvent(mkEvent("c", "g", "beforeSubmitPrompt", 1, "duration", "slow")), nil)
	if err != nil || len(decodeState(res.StateOnSuccess).Events) != 1 {
		t.Fatalf("type mismatch: %v", err)
	}
}

func TestStateBounded(t *testing.T) {
	conv := "55555555-5555-4555-8555-555555555555"
	tool := func(gen, id string, sec float64, kv ...any) event {
		f := mkEvent(conv, gen, "postToolUse", sec, append([]any{"tool_name", "Read", "tool_use_id", id, "duration", 1.0}, kv...)...)
		var pl payload
		_ = json.Unmarshal(f.Raw, &pl)
		return newEvent("postToolUse", pl, f.At)
	}
	st := &state{V: stateVersion}
	for i := 0; i < maxEvents; i++ {
		st.add(tool("g", fmt.Sprintf("t%d", i), float64(i)/10))
	}
	prev := encodeState(st)
	p := New()
	for i := maxEvents; i < maxEvents+10; i++ {
		e := mkEvent(conv, "g", "postToolUse", float64(i)/10, "tool_name", "Read", "tool_use_id", fmt.Sprintf("t%d", i), "duration", 1.0)
		res, err := p.Collect(hookEvent(e), prev)
		if err != nil {
			t.Fatal(err)
		}
		prev = res.StateOnSuccess
	}
	st = decodeState(prev)
	if len(st.Events) != maxEvents || st.Events[0].ID != "t10" || st.Events[maxEvents-1].ID != fmt.Sprintf("t%d", maxEvents+9) {
		t.Fatalf("events %d, oldest %s", len(st.Events), st.Events[0].ID)
	}
	// A day later everything older is dropped.
	late := mkEvent(conv, "g2", "beforeSubmitPrompt", 0, "prompt", "p").shift(maxAge + time.Hour)
	res, _ := p.Collect(hookEvent(late), prev)
	if st := decodeState(res.StateOnSuccess); len(st.Events) != 1 {
		t.Fatalf("events after a day: %d", len(st.Events))
	}

	// Long text is cut a little past the core's limit.
	long := strings.Repeat("é", maxField+50)
	res, _ = p.Collect(hookEvent(mkEvent(conv, "g3", "beforeSubmitPrompt", 1, "prompt", long)), nil)
	if got := decodeState(res.StateOnSuccess).Events[0].Text; len([]rune(got)) != maxField {
		t.Fatalf("prompt kept %d runes", len([]rune(got)))
	}
	// Tool content has a total budget; past it events are kept without it.
	big := strings.Repeat("x", maxField)
	st = &state{V: stateVersion}
	n := maxToolContent/maxField + 10
	for i := 0; i < n; i++ {
		st.add(tool("g4", fmt.Sprintf("b%d", i), float64(i), "tool_output", big))
	}
	total := 0
	for _, e := range st.Events {
		total += len(e.Args) + len(e.Result)
	}
	if total > maxToolContent || len(st.Events) != n || st.Events[n-1].Result != "" || st.Events[0].Result != big {
		t.Fatalf("tool content %d bytes in %d events", total, len(st.Events))
	}
}

func TestProvider(t *testing.T) {
	for m, want := range map[string]string{
		"grok-4.7": "xai", "claude-4.5-opus-high-thinking": "anthropic", "gpt-5.2": "openai", "o3": "openai",
		"codex-5": "openai", "gemini-3-pro": "google", "deepseek-v4": "deepseek", "composer-2.5": "cursor",
		"auto": "cursor", "": "cursor", "Grok-Code": "xai",
	} {
		if got := provider(m); got != want {
			t.Errorf("provider(%q) = %q, want %q", m, got, want)
		}
	}
}

// A background Task returns at once: its turn is sent before the subagent
// does anything. The subagent's later events must not turn up in the next
// turn as a Task it never called.
func TestBackgroundSubagentAfterTurnSent(t *testing.T) {
	conv, child := "66666666-6666-4666-8666-666666666666", "77777777-7777-4777-8777-777777777777"
	call := "call-bg\nfc_bg"
	sub := func(name string, sec float64, kv ...any) fxEvent {
		return mkEvent(child, "child-gen", name, sec, append([]any{"parent_agent_id", conv, "model", "composer-2.5", "model_id", ""}, kv...)...)
	}
	first, prev := run(t, []fxEvent{
		mkEvent(conv, "g1", "beforeSubmitPrompt", 0, "prompt", "p1"),
		mkEvent(conv, "g1", "postToolUse", 3, "tool_name", "Task", "tool_use_id", call, "duration", 20.0,
			"tool_output", `{"status":"success","isBackground":true}`),
		mkEvent(conv, "g1", "stop", 5, "status", "completed"),
	}, nil, nil)
	if len(first) != 1 || summary(first[0].Steps) != "llm-1,Task,llm-2" || first[0].Steps[1].Tool.Subagent != nil {
		t.Fatalf("first turn %v", first)
	}
	if s := decodeState(prev).Sent; len(s) != 1 || !reflect.DeepEqual(s[0].Calls, []string{call}) {
		t.Fatalf("sent %+v", s)
	}
	turns, prev := run(t, []fxEvent{
		mkEvent(conv, "g2", "beforeSubmitPrompt", 10, "prompt", "p2"),
		sub("afterAgentThought", 11, "duration_ms", 400),
		sub("postToolUse", 12, "tool_name", "Shell", "tool_use_id", "s1", "duration", 500.0, "parent_tool_call_id", call),
		sub("postToolUse", 14, "tool_name", "Shell", "tool_use_id", "s2", "duration", 500.0, "parent_tool_call_id", call),
		mkEvent(conv, "g1", "subagentStop", 15, "subagent_id", call, "subagent_type", "explore", "status", "completed",
			"child_conversation_id", child, "parent_agent_id", conv),
		mkEvent(conv, "g2", "stop", 16, "status", "completed"),
	}, prev, nil)
	if len(turns) != 1 || turns[0].TurnKey != "g2" || summary(turns[0].Steps) != "llm-1" {
		t.Fatalf("second turn %v", turns)
	}
	st := decodeState(prev)
	if len(st.Events) != 0 || !reflect.DeepEqual(st.Sent[0].Child, []string{"child-gen"}) {
		t.Fatalf("state %+v", st)
	}
}

func TestUsageOnlyOnLastCallOthersMarked(t *testing.T) {
	turns, _ := run(t, readFixture(t), nil, nil)
	if len(turns) != 1 {
		t.Fatalf("turns = %d", len(turns))
	}
	var withUsage, marked, unmarked int
	var walk func([]model.Step)
	walk = func(steps []model.Step) {
		for _, s := range steps {
			if s.LLM != nil {
				switch {
				case s.LLM.Usage.InputTokens > 0:
					withUsage++
					if s.LLM.UsageInOtherCall {
						t.Errorf("call with the turn's usage is marked")
					}
				case s.LLM.UsageInOtherCall:
					marked++
				default:
					unmarked++
				}
			}
			if s.Tool != nil && s.Tool.Subagent != nil {
				walk(s.Tool.Subagent.Steps)
			}
		}
	}
	walk(turns[0].Steps)
	if withUsage != 1 || marked == 0 || unmarked != 0 {
		t.Fatalf("withUsage=%d marked=%d unmarked=%d", withUsage, marked, unmarked)
	}
}
