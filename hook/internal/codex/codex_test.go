package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/model"
)

// Fixtures are sanitized real rollouts (codex-cli 0.167/0.169), see
// testdata/README.md.
const (
	fxDir     = "testdata/sessions/2026/11/"
	fxSimple  = fxDir + "16/rollout-2026-11-16T14-04-06-01a1e2f4-1390-7ddb-8d03-974ca33268c9.jsonl"
	fxAborted = fxDir + "13/rollout-2026-11-13T15-31-43-01a1d3d1-c11f-7c01-85e3-49179bfb45eb.jsonl"
	fxMCP     = fxDir + "15/rollout-2026-11-15T10-13-31-01a1dcfb-e188-73f3-84f9-5689a36c3295.jsonl"
	fxParent  = fxDir + "16/rollout-2026-11-16T19-18-09-01a1e414-29b8-7811-8025-8f2284abb7d1.jsonl"
	fxChild   = fxDir + "16/rollout-2026-11-16T19-24-00-01a1e419-7106-71a2-815d-3f74a6b1c24d.jsonl"
	fxForked  = fxDir + "13/rollout-2026-11-13T22-37-02-01a1d557-ddb5-7fff-832e-26cdb584b029.jsonl"

	parentThread = "01a1e414-29b8-7811-8025-8f2284abb7d1"
	childThread  = "01a1e419-7106-71a2-815d-3f74a6b1c24d"
	forkedThread = "01a1d557-ddb5-7fff-832e-26cdb584b029"
	forkedRoot   = "01a1d553-0c3e-72c2-a3d0-8c6b3e874fc9"
)

// ---- fixture helpers (independent of the parser) ----

type line struct {
	raw []byte
	obj map[string]any
}

func (l line) typ() string { s, _ := l.obj["type"].(string); return s }

func (l line) payload() map[string]any { m, _ := l.obj["payload"].(map[string]any); return m }

func (l line) ptype() string { s, _ := l.payload()["type"].(string); return s }

func (l line) ts(t *testing.T) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, l.obj["timestamp"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func readFixture(t *testing.T, path string) []line {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []line
	for _, raw := range bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n")) {
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, line{raw: raw, obj: obj})
	}
	return out
}

func joinLines(ls []line) []byte {
	var b bytes.Buffer
	for _, l := range ls {
		b.Write(l.raw)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func writeRollout(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func stopEvent(path, turnID string) model.HookEvent {
	raw, _ := json.Marshal(map[string]string{
		"hook_event_name": "Stop", "session_id": "s", "turn_id": turnID, "transcript_path": path, "cwd": "/tmp",
	})
	return model.HookEvent{Tool: model.ToolCodex, Name: "Stop", SessionID: "s", TranscriptPath: path, Raw: raw, Now: time.Now()}
}

func collect(t *testing.T, p *Parser, ev model.HookEvent, prev []byte) model.CollectResult {
	t.Helper()
	res, err := p.Collect(ev, prev)
	if err != nil {
		t.Fatal(err)
	}
	if res.StateOnSuccess == nil {
		t.Fatal("StateOnSuccess is nil")
	}
	return res
}

// seeded is the state of a rollout whose session started after the hook was
// installed: an entry exists, so Collect exports every finished turn.
func seeded(path string) []byte {
	return encodeState(&state{V: stateVersion, Files: map[string]*fileState{path: {}}})
}

// turnIDs returns task_started turn ids in file order.
func turnIDs(ls []line) []string {
	var ids []string
	for _, l := range ls {
		if l.typ() == "event_msg" && l.ptype() == "task_started" {
			ids = append(ids, l.payload()["turn_id"].(string))
		}
	}
	return ids
}

func find(ls []line, from int, pred func(line) bool) int {
	for i := from; i < len(ls); i++ {
		if pred(ls[i]) {
			return i
		}
	}
	return -1
}

func isEvent(kind string) func(line) bool {
	return func(l line) bool { return l.typ() == "event_msg" && l.ptype() == kind }
}

func num(m map[string]any, k string) int64 { f, _ := m[k].(float64); return int64(f) }

type record struct {
	turnID, responseID string
	usage              model.Usage
}

func usageRecords(ls []line) []record {
	var out []record
	for _, l := range ls {
		if l.typ() != "token_usage_record" {
			continue
		}
		p := l.payload()
		u := p["usage"].(map[string]any)
		out = append(out, record{
			turnID: p["turn_id"].(string), responseID: p["response_id"].(string),
			usage: model.Usage{
				InputTokens: num(u, "input_tokens"), OutputTokens: num(u, "output_tokens"),
				CacheReadTokens: num(u, "cached_input_tokens"), CacheWriteTokens: num(u, "cache_write_input_tokens"),
				ReasoningTokens: num(u, "reasoning_output_tokens"),
			},
		})
	}
	return out
}

func llmSteps(steps []model.Step) []model.Step {
	var out []model.Step
	for _, s := range steps {
		if s.Kind == model.StepLLM {
			out = append(out, s)
		}
	}
	return out
}

func toolSteps(steps []model.Step) []model.Step {
	var out []model.Step
	for _, s := range steps {
		if s.Kind == model.StepTool {
			out = append(out, s)
		}
	}
	return out
}

func stepByKey(t *testing.T, steps []model.Step, key string) model.Step {
	t.Helper()
	for _, s := range steps {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no step with key %q", key)
	return model.Step{}
}

func lastAgentMessage(ls []line, turnID string) string {
	for _, l := range ls {
		if l.typ() == "event_msg" && l.ptype() == "task_complete" && l.payload()["turn_id"] == turnID {
			s, _ := l.payload()["last_agent_message"].(string)
			return s
		}
	}
	return ""
}

func checkSorted(t *testing.T, steps []model.Step) {
	t.Helper()
	for i := 1; i < len(steps); i++ {
		if steps[i].Start.Before(steps[i-1].Start) {
			t.Fatalf("steps not in start order at %d", i)
		}
	}
	seen := map[string]bool{}
	for _, s := range steps {
		if s.End.Before(s.Start) {
			t.Fatalf("step %s ends before it starts", s.Key)
		}
		if seen[s.Key] {
			t.Fatalf("duplicate step key %q", s.Key)
		}
		seen[s.Key] = true
	}
}

// ---- tests ----

func TestSimpleTurn(t *testing.T) {
	ls := readFixture(t, fxSimple)
	ids := turnIDs(ls)
	res := collect(t, New().(*Parser), stopEvent(fxSimple, ids[0]), nil)
	if len(res.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(res.Turns))
	}
	tr := res.Turns[0]
	meta := ls[0].payload()
	want := model.Turn{
		Tool:               model.ToolCodex,
		SessionID:          meta["session_id"].(string),
		TurnKey:            ids[0],
		ToolVersion:        "0.169.0",
		Cwd:                "/tmp/redacted/project",
		Repo:               "example.invalid/org/repo",
		GitBranch:          "feature-branch",
		Start:              ls[find(ls, 0, isEvent("task_started"))].ts(t),
		End:                ls[find(ls, 0, isEvent("task_complete"))].ts(t),
		Input:              "[text:11]\n[text:12]\n[text:13]", // user.text items only; AGENTS.md/env context skipped
		Output:             lastAgentMessage(ls, ids[0]),
		SystemInstructions: meta["base_instructions"].(map[string]any)["text"].(string),
		Status:             model.StatusOK,
	}
	got := tr
	got.Steps = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("turn mismatch:\n got %+v\nwant %+v", got, want)
	}
	if want.Output == "" || want.SystemInstructions == "" {
		t.Fatal("fixture lost output or instructions")
	}
	recs := usageRecords(ls)
	llm := llmSteps(tr.Steps)
	if len(llm) != len(recs) || len(toolSteps(tr.Steps)) != 0 {
		t.Fatalf("steps = %d llm / %d tool, want %d / 0", len(llm), len(toolSteps(tr.Steps)), len(recs))
	}
	c := llm[0].LLM
	if c.Provider != "openai" || c.RequestModel != "gpt-6.1-sol" || c.ResponseModel != "gpt-6.1-sol" {
		t.Fatalf("llm call = %+v", c)
	}
	if !llm[0].Start.Equal(tr.Start) || !llm[0].End.Equal(lineOf(t, ls, "token_usage_record").ts(t)) {
		t.Fatalf("llm timing %v..%v", llm[0].Start, llm[0].End)
	}
	if len(c.InputMessages) != 1 || c.InputMessages[0].Role != "user" || c.InputMessages[0].Parts[0].Content != tr.Input {
		t.Fatalf("first call input = %+v", c.InputMessages)
	}
	if len(c.OutputMessages) != 1 || c.OutputMessages[0].Role != "assistant" {
		t.Fatalf("output messages = %+v", c.OutputMessages)
	}
}

func lineOf(t *testing.T, ls []line, typ string) line {
	t.Helper()
	i := find(ls, 0, func(l line) bool { return l.typ() == typ })
	if i < 0 {
		t.Fatalf("no %s line", typ)
	}
	return ls[i]
}

func TestTokenUsageMapping(t *testing.T) {
	for _, fx := range []string{fxSimple, fxAborted, fxMCP, fxParent} {
		ls := readFixture(t, fx)
		res := collect(t, New().(*Parser), stopEvent(fx, ""), seeded(fx))
		recs := usageRecords(ls)
		var got []model.Step
		for _, tr := range res.Turns {
			checkSorted(t, tr.Steps)
			got = append(got, llmSteps(tr.Steps)...)
		}
		if len(got) != len(recs) {
			t.Fatalf("%s: llm steps = %d, records = %d", fx, len(got), len(recs))
		}
		for i, r := range recs {
			s := got[i]
			if s.Key != r.responseID || s.LLM.ResponseID != r.responseID {
				t.Fatalf("%s step %d key %s, want %s", fx, i, s.Key, r.responseID)
			}
			// input_tokens already contains cached input: copied as is, cached
			// goes to CacheRead, reasoning stays inside output.
			if s.LLM.Usage != r.usage {
				t.Fatalf("%s step %d usage %+v, want %+v", fx, i, s.LLM.Usage, r.usage)
			}
			if r.usage.CacheReadTokens > r.usage.InputTokens || r.usage.ReasoningTokens > r.usage.OutputTokens {
				t.Fatalf("fixture semantics changed: %+v", r.usage)
			}
		}
	}
}

func TestResponseIDDedupe(t *testing.T) {
	ls := readFixture(t, fxAborted)
	var dup []line
	for _, l := range ls {
		dup = append(dup, l)
		if l.typ() == "token_usage_record" {
			dup = append(dup, l) // same response_id written twice
		}
	}
	path := writeRollout(t, t.TempDir(), "sessions/rollout-x-"+strings.Repeat("0", 8)+"-0000-0000-0000-000000000000.jsonl", joinLines(dup))
	res := collect(t, New().(*Parser), stopEvent(path, ""), seeded(path))
	n := 0
	for _, tr := range res.Turns {
		n += len(llmSteps(tr.Steps))
	}
	if want := len(usageRecords(ls)); n != want {
		t.Fatalf("llm steps = %d, want %d", n, want)
	}
}

func TestTokenCountFallback(t *testing.T) {
	ls := readFixture(t, fxAborted)
	var noRecords []line
	for _, l := range ls {
		if l.typ() != "token_usage_record" {
			noRecords = append(noRecords, l)
		}
	}
	path := writeRollout(t, t.TempDir(), "r.jsonl", joinLines(noRecords))
	res := collect(t, New().(*Parser), stopEvent(path, ""), seeded(path))
	recs := usageRecords(ls)
	var got []model.Step
	for _, tr := range res.Turns {
		got = append(got, llmSteps(tr.Steps)...)
	}
	if len(got) != len(recs) {
		t.Fatalf("fallback llm steps = %d, want %d (one per model response)", len(got), len(recs))
	}
	for i, r := range recs {
		if got[i].LLM.Usage != r.usage || got[i].LLM.ResponseID != "" {
			t.Fatalf("step %d usage %+v id %q, want %+v", i, got[i].LLM.Usage, got[i].LLM.ResponseID, r.usage)
		}
	}
}

func TestTurnBoundaries(t *testing.T) {
	ls := readFixture(t, fxAborted)
	ids := turnIDs(ls)
	res := collect(t, New().(*Parser), stopEvent(fxAborted, ids[2]), seeded(fxAborted))
	if len(res.Turns) != 3 {
		t.Fatalf("turns = %d, want 3", len(res.Turns))
	}
	wantStatus := []model.Status{model.StatusOK, model.StatusAborted, model.StatusOK}
	for i, tr := range res.Turns {
		if tr.TurnKey != ids[i] || tr.Status != wantStatus[i] {
			t.Fatalf("turn %d = %s %s, want %s %s", i, tr.TurnKey, tr.Status, ids[i], wantStatus[i])
		}
		if tr.Input == "" {
			t.Fatalf("turn %d has no input", i)
		}
	}
	ab := res.Turns[1]
	if ab.StatusMessage != "aborted" || ab.Output != "" {
		t.Fatalf("aborted turn: msg %q output %q", ab.StatusMessage, ab.Output)
	}
	if end := ls[find(ls, 0, isEvent("turn_aborted"))].ts(t); !ab.End.Equal(end) {
		t.Fatalf("aborted end %v, want %v", ab.End, end)
	}
}

func TestRunningTurnStaysPending(t *testing.T) {
	ls := readFixture(t, fxAborted)
	ids := turnIDs(ls)
	third := find(ls, 0, func(l line) bool { return isEvent("task_started")(l) && l.payload()["turn_id"] == ids[2] })
	dir := t.TempDir()
	path := writeRollout(t, dir, "r.jsonl", joinLines(ls[:third+4]))
	p := New().(*Parser)

	// A Stop for another turn (or no turn id) must not close the running turn.
	res := collect(t, p, stopEvent(path, ids[1]), seeded(path))
	if len(res.Turns) != 2 || res.Turns[0].TurnKey != ids[0] || res.Turns[1].TurnKey != ids[1] {
		t.Fatalf("turns = %v, want first two", keys(res.Turns))
	}
	st := decodeState(res.StateOnSuccess).Files[path]
	if want := int64(len(joinLines(ls[:third]))); st.Resume != want {
		t.Fatalf("resume = %d, want %d (start of the running turn)", st.Resume, want)
	}
	res2 := collect(t, p, model.HookEvent{Name: "PostToolUse", TranscriptPath: path, Raw: stopEvent(path, ids[2]).Raw}, res.StateOnSuccess)
	if len(res2.Turns) != 0 {
		t.Fatalf("non-Stop event closed a turn: %v", keys(res2.Turns))
	}

	writeRollout(t, dir, "r.jsonl", joinLines(ls))
	res3 := collect(t, p, stopEvent(path, ""), res2.StateOnSuccess)
	if len(res3.Turns) != 1 || res3.Turns[0].TurnKey != ids[2] || res3.Turns[0].Status != model.StatusOK {
		t.Fatalf("after completion turns = %v", keys(res3.Turns))
	}
}

func keys(ts []model.Turn) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.TurnKey)
	}
	return out
}

// The Stop hook runs before Codex writes task_complete: the named turn is
// exported from what is on disk, and task_complete later is a no-op.
func TestStopBeforeTaskComplete(t *testing.T) {
	ls := readFixture(t, fxSimple)
	ids := turnIDs(ls)
	tc := find(ls, 0, isEvent("task_complete"))
	dir := t.TempDir()
	path := writeRollout(t, dir, "r.jsonl", joinLines(ls[:tc]))
	p := New().(*Parser)

	res := collect(t, p, stopEvent(path, ids[0]), nil)
	if len(res.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(res.Turns))
	}
	tr := res.Turns[0]
	if tr.Status != model.StatusOK || !tr.End.Equal(ls[tc-1].ts(t)) {
		t.Fatalf("status %s end %v", tr.Status, tr.End)
	}
	if want := lastAgentMessage(ls, ids[0]); tr.Output != want || want == "" {
		t.Fatalf("output %q, want final_answer %q", tr.Output, want)
	}
	if len(llmSteps(tr.Steps)) != len(usageRecords(ls)) {
		t.Fatal("missing llm steps")
	}

	writeRollout(t, dir, "r.jsonl", joinLines(ls))
	res2 := collect(t, p, stopEvent(path, ids[0]), res.StateOnSuccess)
	if len(res2.Turns) != 0 {
		t.Fatalf("turn re-emitted after task_complete: %v", keys(res2.Turns))
	}
	if st := decodeState(res2.StateOnSuccess).Files[path]; st.Resume != int64(len(joinLines(ls))) {
		t.Fatalf("resume = %d, want EOF", st.Resume)
	}
}

func TestIncrementalReadWithPartialLine(t *testing.T) {
	ls := readFixture(t, fxAborted)
	full := joinLines(ls)
	want := collect(t, New().(*Parser), stopEvent(fxAborted, ""), seeded(fxAborted)).Turns

	ids := turnIDs(ls)
	second := find(ls, 0, func(l line) bool { return isEvent("task_started")(l) && l.payload()["turn_id"] == ids[1] })
	cut := len(joinLines(ls[:second+5])) + len(ls[second+5].raw)/2 // middle of a line inside turn 2
	dir := t.TempDir()
	path := writeRollout(t, dir, "r.jsonl", full[:cut])
	p := New().(*Parser)

	res1 := collect(t, p, stopEvent(path, ""), seeded(path))
	if len(res1.Turns) != 1 || res1.Turns[0].TurnKey != ids[0] {
		t.Fatalf("first read turns = %v", keys(res1.Turns))
	}
	st := decodeState(res1.StateOnSuccess).Files[path]
	if st.Scanned != int64(len(joinLines(ls[:second+5]))) {
		t.Fatalf("scanned = %d: the partial line must not be consumed", st.Scanned)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(full[cut:]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	res2 := collect(t, p, stopEvent(path, ""), res1.StateOnSuccess)
	got := append(res1.Turns, res2.Turns...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental turns differ from a single read: %v vs %v", keys(got), keys(want))
	}
}

func TestUnknownTypesTolerated(t *testing.T) {
	ls := readFixture(t, fxSimple)
	ids := turnIDs(ls)
	extra := []string{
		`{"timestamp":"2026-11-16T06:04:06.300Z","type":"brand_new_record","payload":{"x":1}}`,
		`{"timestamp":"2026-11-16T06:04:06.301Z","type":"response_item","payload":{"type":"hologram","id":"h1"}}`,
		`{"timestamp":"2026-11-16T06:04:06.302Z","type":"event_msg","payload":{"type":"mystery_event"}}`,
		`{"timestamp":"2026-11-16T06:04:06.303Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"` + ids[0] + `","item":{"type":"QuantumThing","id":"q1"}}}`,
		`{not json`,
	}
	var mixed []line
	mixed = append(mixed, ls[:3]...)
	for _, e := range extra {
		mixed = append(mixed, line{raw: []byte(e)})
	}
	mixed = append(mixed, ls[3:]...)
	dir := t.TempDir()
	path := writeRollout(t, dir, "r.jsonl", joinLines(mixed[:10]))
	p := New().(*Parser)
	res1 := collect(t, p, stopEvent(path, ""), nil) // turn still running: re-read next time
	writeRollout(t, dir, "r.jsonl", joinLines(mixed))
	res2 := collect(t, p, stopEvent(path, ids[0]), res1.StateOnSuccess)
	if len(res1.Turns)+len(res2.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(res1.Turns)+len(res2.Turns))
	}
	clean := collect(t, New().(*Parser), stopEvent(fxSimple, ""), nil).Turns[0]
	if len(res2.Turns[0].Steps) != len(clean.Steps) {
		t.Fatal("unknown records changed the steps")
	}
	want := map[string]int{"brand_new_record": 1, "response_item:hologram": 1, "event_msg:mystery_event": 1, "item:QuantumThing": 1, "invalid_json": 1}
	if got := UnknownCounts(res2.StateOnSuccess); !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown counts = %v, want %v (counted once despite the re-read)", got, want)
	}
}

func TestAlreadySentTurnsNotReemitted(t *testing.T) {
	ls := readFixture(t, fxAborted)
	dir := t.TempDir()
	path := writeRollout(t, dir, "r.jsonl", joinLines(ls))
	p := New().(*Parser)
	res1 := collect(t, p, stopEvent(path, ""), seeded(path))
	if len(res1.Turns) != 3 {
		t.Fatalf("turns = %d", len(res1.Turns))
	}
	if res1.StateOnFailure != nil {
		t.Fatal("non-first collect: StateOnFailure must be nil")
	}
	if res := collect(t, p, stopEvent(path, ""), res1.StateOnSuccess); len(res.Turns) != 0 {
		t.Fatalf("re-emitted %v", keys(res.Turns))
	}
	// Shrinking resets the offsets but keeps the sent ids.
	writeRollout(t, dir, "r.jsonl", joinLines(ls[:5]))
	res2 := collect(t, p, stopEvent(path, ""), res1.StateOnSuccess)
	// The first turn is incomplete in the short file: resume at its task_started.
	if st := decodeState(res2.StateOnSuccess).Files[path]; st.Scanned != int64(len(joinLines(ls[:5]))) || st.Resume != int64(len(joinLines(ls[:1]))) {
		t.Fatalf("after shrink resume=%d scanned=%d", st.Resume, st.Scanned)
	}
	writeRollout(t, dir, "r.jsonl", joinLines(ls))
	if res := collect(t, p, stopEvent(path, ""), res2.StateOnSuccess); len(res.Turns) != 0 {
		t.Fatalf("re-emitted after reset %v", keys(res.Turns))
	}
}

func TestSupersededTurnIsAborted(t *testing.T) {
	ls := readFixture(t, fxAborted)
	var cut []line
	for _, l := range ls {
		if !isEvent("turn_aborted")(l) {
			cut = append(cut, l)
		}
	}
	path := writeRollout(t, t.TempDir(), "r.jsonl", joinLines(cut))
	res := collect(t, New().(*Parser), stopEvent(path, ""), seeded(path))
	if len(res.Turns) != 3 || res.Turns[1].Status != model.StatusAborted {
		t.Fatalf("turns = %v", keys(res.Turns))
	}
	if st := decodeState(res.StateOnSuccess).Files[path]; st.Resume != int64(len(joinLines(cut))) {
		t.Fatal("superseded turn pinned the resume offset")
	}
}

func TestToolCallPairingAndStatus(t *testing.T) {
	ls := readFixture(t, fxMCP)
	res := collect(t, New().(*Parser), stopEvent(fxMCP, ""), nil)
	if len(res.Turns) != 1 {
		t.Fatalf("turns = %d", len(res.Turns))
	}
	steps := res.Turns[0].Steps
	calls := 0
	for i, l := range ls {
		if l.typ() != "response_item" || (l.ptype() != "function_call" && l.ptype() != "custom_tool_call") {
			continue
		}
		calls++
		p := l.payload()
		callID := p["call_id"].(string)
		s := stepByKey(t, steps, callID)
		out := find(ls, i, func(o line) bool {
			return o.typ() == "response_item" && strings.HasSuffix(o.ptype(), "_output") && o.payload()["call_id"] == callID
		})
		if s.Tool.CallID != callID || out < 0 {
			t.Fatalf("call %s not paired", callID)
		}
		if l.ptype() == "custom_tool_call" {
			if s.Tool.Name != "exec" || s.Tool.Arguments != p["input"] || !s.Start.Equal(l.ts(t)) || !s.End.Equal(ls[out].ts(t)) {
				t.Fatalf("exec step %+v", s)
			}
			if !strings.HasPrefix(s.Tool.Result, "Script completed") || s.Tool.Status != model.StatusOK {
				t.Fatalf("exec result %q status %s", s.Tool.Result, s.Tool.Status)
			}
			continue
		}
		// MCP: function_call js in namespace mcp__demo_repl, McpToolCall item id == call_id.
		it := find(ls, i, func(o line) bool {
			item, _ := o.payload()["item"].(map[string]any)
			return isEvent("item_completed")(o) && item["id"] == callID
		})
		item := ls[it].payload()["item"].(map[string]any)
		if s.Tool.Name != "demo_repl.js" || s.Tool.Arguments != p["arguments"] {
			t.Fatalf("mcp step name %q", s.Tool.Name)
		}
		if !s.Start.Equal(time.UnixMilli(num(ls[it].payload(), "started_at_ms"))) || !s.End.Equal(time.UnixMilli(num(ls[it].payload(), "completed_at_ms"))) {
			t.Fatalf("mcp timing %v..%v", s.Start, s.End)
		}
		failed := item["status"] == "failed"
		if failed != (s.Tool.Status == model.StatusError) || (failed && s.Tool.ErrorMessage != "failed") {
			t.Fatalf("mcp %s status %s/%s, item status %v", callID, s.Tool.Status, s.Tool.ErrorMessage, item["status"])
		}
		if s.Tool.Result == "" {
			t.Fatal("mcp result missing")
		}
	}
	if calls == 0 || len(toolSteps(steps)) != calls {
		t.Fatalf("tool steps = %d, calls = %d", len(toolSteps(steps)), calls)
	}
	// The model call that requested a tool lists it; the next one gets its result.
	var reqIdx int
	llm := llmSteps(steps)
	for i, s := range llm {
		for _, m := range s.LLM.OutputMessages {
			for _, part := range m.Parts {
				if part.Type == "tool_call" && part.Name == "demo_repl.js" && reqIdx == 0 {
					reqIdx = i
				}
			}
		}
	}
	next := llm[reqIdx+1].LLM.InputMessages
	if len(next) == 0 || next[0].Role != "tool" || next[0].Parts[0].Type != "tool_call_response" {
		t.Fatalf("tool result not in next call input: %+v", next)
	}
}

func TestExecScriptStatus(t *testing.T) {
	ls := readFixture(t, fxAborted)
	ids := turnIDs(ls)
	// Rewrite: first exec output -> "Script failed"; add a failed nested
	// command inside the second exec (script completed) and a declined one
	// inside the third.
	var calls []string
	for _, l := range ls {
		if l.typ() == "response_item" && l.ptype() == "custom_tool_call" {
			calls = append(calls, l.payload()["call_id"].(string))
		}
	}
	var out []line
	for _, l := range ls {
		if l.typ() == "response_item" && l.ptype() == "custom_tool_call_output" {
			cid := l.payload()["call_id"].(string)
			switch cid {
			case calls[0]:
				l.raw = bytes.Replace(l.raw, []byte(`"Script completed`), []byte(`"Script failed`), 1)
			case calls[1], calls[2]:
				status, code := "failed", 1
				if cid == calls[2] {
					status, code = "declined", 0
				}
				nested, _ := json.Marshal(map[string]any{
					"timestamp": l.obj["timestamp"], "type": "event_msg",
					"payload": map[string]any{"type": "item_completed", "turn_id": ids[0], "item": map[string]any{
						"type": "CommandExecution", "id": "exec-nested-" + cid, "status": status, "exit_code": code,
					}},
				})
				out = append(out, line{raw: nested})
			}
		}
		out = append(out, l)
	}
	path := writeRollout(t, t.TempDir(), "r.jsonl", joinLines(out))
	steps := collect(t, New().(*Parser), stopEvent(path, ""), seeded(path)).Turns[0].Steps
	check := func(callID string, status model.Status, msg string) {
		t.Helper()
		s := stepByKey(t, steps, callID)
		if s.Tool.Status != status || s.Tool.ErrorMessage != msg {
			t.Fatalf("%s: %s/%q, want %s/%q", callID, s.Tool.Status, s.Tool.ErrorMessage, status, msg)
		}
	}
	check(calls[0], model.StatusError, "Script failed")
	check(calls[1], model.StatusOK, "") // nested exit != 0 inside a completed script
	check(calls[2], model.StatusError, "declined")
	check(calls[3], model.StatusOK, "")
}

func TestSubagentNestedUnderSpawn(t *testing.T) {
	pls := readFixture(t, fxParent)
	cls := readFixture(t, fxChild)
	res := collect(t, New().(*Parser), stopEvent(fxParent, turnIDs(pls)[0]), nil)
	if len(res.Turns) != 1 {
		t.Fatalf("turns = %d", len(res.Turns))
	}
	tr := res.Turns[0]
	if tr.SessionID != parentThread {
		t.Fatalf("session = %s", tr.SessionID)
	}
	spawn := find(pls, 0, func(l line) bool { return l.ptype() == "function_call" && l.payload()["name"] == "spawn_agent" })
	spawnID := pls[spawn].payload()["call_id"].(string)
	var nested []model.Step
	for _, s := range tr.Steps {
		if s.Kind == model.StepTool && s.Tool.Subagent != nil {
			nested = append(nested, s)
		}
	}
	if len(nested) != 1 || nested[0].Key != spawnID || nested[0].Tool.Name != "spawn_agent" {
		t.Fatalf("subagent attached to %v, want spawn call %s", nested, spawnID)
	}
	sa := nested[0].Tool.Subagent
	cids := turnIDs(cls)
	if sa.ID != childThread || sa.Type != "Feynman" || sa.Status != model.StatusOK {
		t.Fatalf("subagent = %s %s %s", sa.ID, sa.Type, sa.Status)
	}
	// Both child turns (spawn + followup_task) ran inside the parent turn.
	if len(cids) != 2 || sa.Output != lastAgentMessage(cls, cids[1]) || sa.Input == "" {
		t.Fatalf("subagent input %q output %q", sa.Input, sa.Output)
	}
	if !sa.Start.Equal(cls[find(cls, 0, isEvent("task_started"))].ts(t)) {
		t.Fatalf("subagent start %v", sa.Start)
	}
	checkSorted(t, sa.Steps)
	recs := usageRecords(cls)
	llm := llmSteps(sa.Steps)
	if len(llm) != len(recs) {
		t.Fatalf("subagent llm steps = %d, want %d", len(llm), len(recs))
	}
	for i, r := range recs {
		if llm[i].LLM.Usage != r.usage || llm[i].Key != r.responseID {
			t.Fatalf("subagent step %d usage mismatch", i)
		}
	}
	// Parent usage is the parent's own records only.
	if n := len(llmSteps(tr.Steps)); n != len(usageRecords(pls)) {
		t.Fatalf("parent llm steps = %d", n)
	}
	if st := decodeState(res.StateOnSuccess).Files[fxParent]; st.Children[childThread] != fxChild {
		t.Fatalf("child path not cached: %v", st.Children)
	}
}

func TestSubagentLookupAcrossDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "codex", "sessions")
	parentData, _ := os.ReadFile(fxParent)
	childData, _ := os.ReadFile(fxChild)
	parent := writeRollout(t, root, "2026/11/16/"+filepath.Base(fxParent), parentData)
	writeRollout(t, root, "2026/10/01/"+filepath.Base(fxChild), childData)
	res := collect(t, New().(*Parser), stopEvent(parent, ""), nil)
	if sa := spawnSubagent(res.Turns[0]); sa == nil || sa.ID != childThread {
		t.Fatal("child in another day directory not found")
	}

	// Without a sessions/ ancestor only the rollout's own directory is searched.
	other := t.TempDir()
	parent2 := writeRollout(t, other, "a/"+filepath.Base(fxParent), parentData)
	writeRollout(t, other, "b/"+filepath.Base(fxChild), childData)
	res2 := collect(t, New().(*Parser), stopEvent(parent2, ""), nil)
	if sa := spawnSubagent(res2.Turns[0]); sa != nil {
		t.Fatal("searched outside a sessions tree")
	}
	if len(toolSteps(res2.Turns[0].Steps)) != len(toolSteps(res.Turns[0].Steps)) {
		t.Fatal("missing child changed the tool steps")
	}
}

func spawnSubagent(tr model.Turn) *model.Subagent {
	for _, s := range tr.Steps {
		if s.Kind == model.StepTool && s.Tool.Subagent != nil {
			return s.Tool.Subagent
		}
	}
	return nil
}

func TestSubagentRolloutNotEmittedOnItsOwn(t *testing.T) {
	prev := collect(t, New().(*Parser), stopEvent(fxParent, ""), nil).StateOnSuccess
	res := collect(t, New().(*Parser), stopEvent(fxChild, turnIDs(readFixture(t, fxChild))[1]), prev)
	if len(res.Turns) != 0 {
		t.Fatalf("subagent rollout emitted %v", keys(res.Turns))
	}
	if !bytes.Equal(res.StateOnSuccess, prev) {
		t.Fatal("subagent rollout changed the (possibly shared) state")
	}
}

func TestSubagentRolloutUsesRootSessionID(t *testing.T) {
	cls := readFixture(t, fxChild)
	p := &Parser{emitSubagentThreads: true}
	res := collect(t, p, stopEvent(fxChild, ""), seeded(fxChild))
	if !reflect.DeepEqual(keys(res.Turns), turnIDs(cls)) {
		t.Fatalf("turns = %v", keys(res.Turns))
	}
	for _, tr := range res.Turns {
		if tr.SessionID != parentThread || tr.SessionID == childThread {
			t.Fatalf("session id = %s, want root %s", tr.SessionID, parentThread)
		}
	}
	// The child's prompt is the parent's agent_message, not AGENTS.md.
	if res.Turns[0].Input == "" || llmSteps(res.Turns[0].Steps)[0].LLM.InputMessages[0].Parts[0].Content != res.Turns[0].Input {
		t.Fatalf("child input %q", res.Turns[0].Input)
	}
}

func TestForkedSubagentSkipsReplayedHistory(t *testing.T) {
	ls := readFixture(t, fxForked)
	ids := turnIDs(ls) // [replayed parent turn, own turn]
	if len(ids) != 2 {
		t.Fatalf("fixture turns = %v", ids)
	}
	p := &Parser{emitSubagentThreads: true}
	res := collect(t, p, stopEvent(fxForked, ids[1]), nil)
	if !reflect.DeepEqual(keys(res.Turns), ids[1:]) {
		t.Fatalf("turns = %v, want only own turn %s", keys(res.Turns), ids[1])
	}
	tr := res.Turns[0]
	if tr.SessionID != forkedRoot || tr.Input == "" {
		t.Fatalf("session %s input %q", tr.SessionID, tr.Input)
	}
	if len(llmSteps(tr.Steps)) != len(usageRecords(ls)) {
		t.Fatal("replayed history leaked into the own turn")
	}
	if res := collect(t, New().(*Parser), stopEvent(fxForked, ids[1]), nil); len(res.Turns) != 0 {
		t.Fatal("forked subagent rollout emitted by default")
	}

	// Nested under a parent: same own turn only.
	b := &builder{meta: &sessionMeta{ThreadID: forkedRoot}, path: fxForked, res: &resolver{}}
	sa := b.subagent(&subLink{thread: forkedThread}, ls[0].ts(t).Add(-time.Hour), ls[len(ls)-1].ts(t))
	if sa == nil || sa.Type != "Anscombe" || len(llmSteps(sa.Steps)) != len(usageRecords(ls)) || sa.Output != lastAgentMessage(ls, ids[1]) {
		t.Fatalf("nested forked subagent = %+v", sa)
	}
}

func TestProvider(t *testing.T) {
	for in, want := range map[string]string{"": "openai", "openai": "openai", "Azure": "azure", " ollama ": "ollama"} {
		if got := (&sessionMeta{Provider: in}).provider(); got != want {
			t.Fatalf("provider(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoTranscriptPath(t *testing.T) {
	res := collect(t, New().(*Parser), model.HookEvent{Name: "Stop"}, []byte(`{"v":1}`))
	if len(res.Turns) != 0 || string(res.StateOnSuccess) != `{"v":1}` {
		t.Fatalf("got %+v", res)
	}
	if _, err := New().Collect(stopEvent(filepath.Join(t.TempDir(), "missing.jsonl"), ""), nil); err == nil {
		t.Fatal("missing rollout: want error")
	}
}

func TestUserTextSelection(t *testing.T) {
	msg := func(kinds []string, texts ...string) responseItem {
		var items []contentItem
		for _, x := range texts {
			items = append(items, contentItem{Type: "input_text", Text: x})
		}
		raw, _ := json.Marshal(items)
		r := responseItem{Type: "message", Role: "user", Content: raw}
		if kinds != nil {
			r.Meta = &struct {
				ContentItemKinds []string `json:"content_item_kinds"`
			}{kinds}
		}
		return r
	}
	cases := []struct {
		name     string
		in       responseItem
		afterCtx bool
		want     string
		ok       bool
	}{
		{"harness kinds", msg([]string{"agents_md.instructions", "environments.environment_context"}, "a", "b"), true, "", false},
		{"user text only", msg([]string{"skills.selected_skill_instructions", "user.text"}, "skill", "prompt"), true, "prompt", true},
		{"heartbeat", msg([]string{"user.heartbeat"}, "tick"), true, "tick", true},
		{"no kinds after ctx", msg(nil, "prompt"), true, "prompt", true},
		{"no kinds env context", msg(nil, "<environment_context>x</environment_context>"), true, "", false},
		{"no kinds before ctx", msg(nil, "prompt"), false, "", false},
	}
	for _, c := range cases {
		got, ok := userText(c.in, c.afterCtx)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestNormalizeRepo(t *testing.T) {
	for in, want := range map[string]string{
		"":                                     "",
		"https://github.com/org/repo.git":      "github.com/org/repo",
		"https://user:tok@github.com/org/repo": "github.com/org/repo",
		"https://ghp_x@github.com:8443/org/repo/":   "github.com/org/repo",
		"git@github.com:org/repo.git":               "github.com/org/repo",
		"ssh://git@gitlab.example.com:2222/a/b.git": "gitlab.example.com/a/b",
		"github.com/org/repo":                       "github.com/org/repo",
		"example.invalid/org/repo":                  "example.invalid/org/repo",
	} {
		if got := normalizeRepo(in); got != want {
			t.Errorf("normalizeRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

// Without usage records the fallback keys must stay unique across the child
// turns concatenated into one subagent (they become span ids).
func TestSyntheticKeysUniqueInSubagent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	parentData, _ := os.ReadFile(fxParent)
	parent := writeRollout(t, root, "2026/11/16/"+filepath.Base(fxParent), parentData)
	var noRecords []line
	cls := readFixture(t, fxChild)
	for _, l := range cls {
		if l.typ() != "token_usage_record" {
			noRecords = append(noRecords, l)
		}
	}
	writeRollout(t, root, "2026/11/16/"+filepath.Base(fxChild), joinLines(noRecords))
	sa := spawnSubagent(collect(t, New().(*Parser), stopEvent(parent, ""), nil).Turns[0])
	if sa == nil {
		t.Fatal("no subagent")
	}
	if len(llmSteps(sa.Steps)) != len(usageRecords(cls)) {
		t.Fatalf("fallback llm steps = %d, want %d", len(llmSteps(sa.Steps)), len(usageRecords(cls)))
	}
	seen := map[string]bool{}
	for _, s := range sa.Steps {
		if seen[s.Key] {
			t.Fatalf("duplicate step key %q", s.Key)
		}
		seen[s.Key] = true
	}
}

// Installing the hook in the middle of a session must not export the whole
// backlog: the first Collect exports only the turn the event names.
func TestFirstCollectSkipsBacklog(t *testing.T) {
	ls := readFixture(t, fxAborted) // three finished turns
	ids := turnIDs(ls)
	dir := t.TempDir()
	path := writeRollout(t, dir, "r.jsonl", joinLines(ls))
	p := New().(*Parser)

	res := collect(t, p, stopEvent(path, ids[2]), nil)
	if !reflect.DeepEqual(keys(res.Turns), ids[2:]) {
		t.Fatalf("first collect turns = %v, want only %s", keys(res.Turns), ids[2])
	}
	if got := decodeState(res.StateOnSuccess).Files[path].Sent; !reflect.DeepEqual(got, ids) {
		t.Fatalf("sent = %v, want all three", got)
	}

	// A fourth turn (the simple fixture's turn, without its session_meta).
	fourth := readFixture(t, fxSimple)[1:]
	fid := turnIDs(fourth)[0]
	writeRollout(t, dir, "r.jsonl", joinLines(append(append([]line(nil), ls...), fourth...)))
	res2 := collect(t, p, stopEvent(path, fid), res.StateOnSuccess)
	if !reflect.DeepEqual(keys(res2.Turns), []string{fid}) {
		t.Fatalf("second collect turns = %v, want only %s", keys(res2.Turns), fid)
	}

	// Without a turn id: only the last turn that closes on this event.
	res3 := collect(t, New().(*Parser), stopEvent(fxAborted, ""), nil)
	if !reflect.DeepEqual(keys(res3.Turns), ids[2:]) {
		t.Fatalf("no turn id: turns = %v, want %s", keys(res3.Turns), ids[2])
	}

	// The named turn is still running (Stop before task_complete): it is the
	// one exported; a turn still open and not named stays pending.
	tc := find(ls, 0, func(l line) bool { return isEvent("task_complete")(l) && l.payload()["turn_id"] == ids[2] })
	cut := writeRollout(t, dir, "cut.jsonl", joinLines(ls[:tc]))
	res4 := collect(t, New().(*Parser), stopEvent(cut, ids[2]), nil)
	if !reflect.DeepEqual(keys(res4.Turns), ids[2:]) || res4.Turns[0].Status != model.StatusOK {
		t.Fatalf("stop-closed first collect = %v", keys(res4.Turns))
	}
	res5 := collect(t, New().(*Parser), stopEvent(cut, "other-turn"), nil)
	if len(res5.Turns) != 0 {
		t.Fatalf("unnamed turns exported: %v", keys(res5.Turns))
	}
	st := decodeState(res5.StateOnSuccess).Files[cut]
	third := find(ls, 0, func(l line) bool { return isEvent("task_started")(l) && l.payload()["turn_id"] == ids[2] })
	if st.Resume != int64(len(joinLines(ls[:third]))) || len(st.Sent) != 2 {
		t.Fatalf("resume %d sent %v: the open third turn must stay pending", st.Resume, st.Sent)
	}
	res6 := collect(t, New().(*Parser), stopEvent(path, ids[2]), res5.StateOnSuccess) // different path entry
	if !reflect.DeepEqual(keys(res6.Turns), ids[2:]) {
		t.Fatalf("other rollout in a shared state: %v", keys(res6.Turns))
	}
}

// A failed first export: the core persists StateOnFailure. The next Collect
// is not a first one and emits the pending turn again, exactly once, with
// the new turn; the backlog stays skipped.
func TestFirstCollectFailureReemitsTurn(t *testing.T) {
	ls := readFixture(t, fxAborted)
	ids := turnIDs(ls)
	fourth := readFixture(t, fxSimple)[1:]
	fid := turnIDs(fourth)[0]
	withFourth := joinLines(append(append([]line(nil), ls...), fourth...))
	tc := find(ls, 0, func(l line) bool { return isEvent("task_complete")(l) && l.payload()["turn_id"] == ids[2] })

	for _, c := range []struct {
		name  string
		first []byte // rollout at the first Stop
	}{
		{"complete", joinLines(ls)},
		{"before task_complete", joinLines(ls[:tc])},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeRollout(t, dir, "r.jsonl", c.first)
			p := New().(*Parser)
			res := collect(t, p, stopEvent(path, ids[2]), nil)
			if !reflect.DeepEqual(keys(res.Turns), ids[2:]) || res.StateOnFailure == nil {
				t.Fatalf("first collect turns %v, failure state %v", keys(res.Turns), res.StateOnFailure != nil)
			}
			fail := decodeState(res.StateOnFailure).Files[path]
			if !reflect.DeepEqual(fail.Sent, ids[:2]) {
				t.Fatalf("failure state sent = %v, want backlog only", fail.Sent)
			}

			writeRollout(t, dir, "r.jsonl", withFourth)
			res2 := collect(t, p, stopEvent(path, fid), res.StateOnFailure)
			if !reflect.DeepEqual(keys(res2.Turns), []string{ids[2], fid}) {
				t.Fatalf("after failure turns = %v, want %s then %s", keys(res2.Turns), ids[2], fid)
			}
			if res2.StateOnFailure != nil {
				t.Fatal("non-first collect: StateOnFailure must be nil")
			}
			if res2.Turns[0].Status != model.StatusOK || len(llmSteps(res2.Turns[0].Steps)) != len(llmSteps(res.Turns[0].Steps)) {
				t.Fatal("re-emitted turn differs")
			}
			if res3 := collect(t, p, stopEvent(path, fid), res2.StateOnSuccess); len(res3.Turns) != 0 {
				t.Fatalf("emitted again: %v", keys(res3.Turns))
			}
		})
	}
}
