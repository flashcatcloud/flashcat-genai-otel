package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/model"
)

// fixture copies testdata/<name> into a temp dir and returns the main
// transcript path and its session id.
func fixture(t *testing.T, name string) (string, string) {
	t.Helper()
	src := filepath.Join("testdata", name)
	dst := t.TempDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dst, "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("fixture %s: want one transcript, got %v", name, matches)
	}
	sid := strings.TrimSuffix(filepath.Base(matches[0]), ".jsonl")
	return matches[0], sid
}

func lines(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			out = append(out, append([]byte(nil), l...))
		}
	}
	return out
}

func writeLines(t *testing.T, path string, ls [][]byte) {
	t.Helper()
	var buf bytes.Buffer
	for _, l := range ls {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func decode(t *testing.T, l []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(l, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func ev(name, path, sid string, raw string) model.HookEvent {
	if raw == "" {
		raw = `{"hook_event_name":"` + name + `"}`
	}
	return model.HookEvent{
		Tool: model.ToolClaude, Name: name, SessionID: sid, TranscriptPath: path,
		Raw: json.RawMessage(raw), Final: name == "SessionEnd", Now: time.Now(),
	}
}

func collect(t *testing.T, e model.HookEvent, prev []byte) model.CollectResult {
	t.Helper()
	res, err := New().Collect(e, prev)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// collectFull parses the whole transcript on the first call (export mode).
func collectFull(t *testing.T, e model.HookEvent, prev []byte) model.CollectResult {
	t.Helper()
	res, err := NewBackfill().Collect(e, prev)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func countSteps(steps []model.Step, kind model.StepKind, deep bool) int {
	n := 0
	for _, s := range steps {
		if s.Kind == kind {
			n++
		}
		if deep && s.Tool != nil && s.Tool.Subagent != nil {
			n += countSteps(s.Tool.Subagent.Steps, kind, true)
		}
	}
	return n
}

func TestFixturesFinal(t *testing.T) {
	cases := []struct {
		name        string
		turns       int
		subagents   int
		deepLLM     int
		deepTools   int
		statusError int
	}{
		{"async", 1, 2, 99, 103, 0},
		{"sync", 5, 6, 61, 99, 0},
		{"plain", 6, 0, 14, 9, 0},
		{"compact", 1, 0, -1, -1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, sid := fixture(t, c.name)
			res := collectFull(t, ev("SessionEnd", path, sid, ""), nil)
			if len(res.Turns) != c.turns {
				t.Fatalf("turns = %d, want %d", len(res.Turns), c.turns)
			}
			subs, llm, tools := 0, 0, 0
			for _, tr := range res.Turns {
				if tr.Tool != model.ToolClaude || tr.SessionID != sid || tr.TurnKey == "" || tr.ToolVersion == "" {
					t.Errorf("turn header: %+v", tr)
				}
				if tr.Start.IsZero() || tr.End.Before(tr.Start) {
					t.Errorf("turn times %v..%v", tr.Start, tr.End)
				}
				if tr.Cwd != "/work/project" || tr.GitBranch != "main" {
					t.Errorf("cwd/branch = %q %q", tr.Cwd, tr.GitBranch)
				}
				llm += countSteps(tr.Steps, model.StepLLM, true)
				tools += countSteps(tr.Steps, model.StepTool, true)
				for _, s := range tr.Steps {
					if s.Tool != nil && s.Tool.Subagent != nil {
						subs++
					}
					if s.LLM != nil && s.LLM.Provider != "anthropic" {
						t.Errorf("provider %q", s.LLM.Provider)
					}
				}
			}
			if subs != c.subagents {
				t.Errorf("subagents = %d, want %d", subs, c.subagents)
			}
			if c.deepLLM >= 0 && (llm != c.deepLLM || tools != c.deepTools) {
				t.Errorf("llm/tools = %d/%d, want %d/%d", llm, tools, c.deepLLM, c.deepTools)
			}
		})
	}
}

// Every line of a multi-line message carries the same usage; the step must
// count it once.
func TestMessageIDMergeUsageOnce(t *testing.T) {
	path, sid := fixture(t, "async")
	type use struct{ in, out, cr, cw, think int64 }
	byID := map[string]use{}
	linesPerID := map[string]int{}
	for _, l := range lines(t, path) {
		r, ok := decodeRecord(l)
		if !ok || r.Type != "assistant" || r.msg == nil || r.msg.Model == syntheticModel || r.IsSidechain {
			continue
		}
		u := r.msg.Usage
		x := use{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens, 0}
		if u.OutputDetails != nil {
			x.think = u.OutputDetails.ThinkingTokens
		}
		byID[r.msg.ID] = x
		linesPerID[r.msg.ID]++
	}
	multi := 0
	for _, n := range linesPerID {
		if n > 1 {
			multi++
		}
	}
	if multi == 0 {
		t.Fatal("fixture has no multi-line message")
	}
	res := collectFull(t, ev("SessionEnd", path, sid, ""), nil)
	got := 0
	for _, s := range res.Turns[0].Steps {
		if s.Kind != model.StepLLM {
			continue
		}
		got++
		want, ok := byID[s.Key]
		if !ok {
			t.Fatalf("unexpected step %s", s.Key)
		}
		u := s.LLM.Usage
		if u.InputTokens != want.in+want.cr+want.cw || u.OutputTokens != want.out ||
			u.CacheReadTokens != want.cr || u.CacheWriteTokens != want.cw || u.ReasoningTokens != want.think {
			t.Errorf("%s usage %+v, want %+v", s.Key, u, want)
		}
		if s.LLM.ResponseID != s.Key || s.LLM.RequestModel == "" || len(s.LLM.FinishReasons) != 1 {
			t.Errorf("%s fields: %+v", s.Key, s.LLM)
		}
		if linesPerID[s.Key] > 1 && !s.End.After(s.Start) && !s.End.Equal(s.Start) {
			t.Errorf("%s times %v..%v", s.Key, s.Start, s.End)
		}
	}
	if got != len(byID) {
		t.Errorf("llm steps = %d, distinct message ids = %d", got, len(byID))
	}
}

func TestToolPairing(t *testing.T) {
	path, sid := fixture(t, "async")
	ls := lines(t, path)
	// Find tool_result blocks without is_error, and flip one result to an error.
	missing := map[string]bool{}
	var flipped string
	for i, l := range ls {
		m := decode(t, l)
		if m["type"] != "user" {
			continue
		}
		msg, _ := m["message"].(map[string]any)
		blocks, _ := msg["content"].([]any)
		for _, b := range blocks {
			bm, _ := b.(map[string]any)
			if bm["type"] != "tool_result" {
				continue
			}
			id, _ := bm["tool_use_id"].(string)
			if _, has := bm["is_error"]; !has {
				missing[id] = true
			} else if flipped == "" {
				bm["is_error"] = true
				flipped = id
				ls[i], _ = json.Marshal(m)
			}
		}
	}
	if len(missing) == 0 || flipped == "" {
		t.Fatalf("fixture lacks cases: missing=%d flipped=%q", len(missing), flipped)
	}
	writeLines(t, path, ls)
	res := collectFull(t, ev("SessionEnd", path, sid, ""), nil)
	seen := 0
	for _, s := range res.Turns[0].Steps {
		if s.Kind != model.StepTool {
			continue
		}
		tc := s.Tool
		if tc.CallID != s.Key || tc.Name == "" {
			t.Errorf("tool fields %+v", tc)
		}
		if s.End.IsZero() || s.End.Before(s.Start) {
			t.Errorf("%s not paired: %v..%v", s.Key, s.Start, s.End)
		}
		switch {
		case missing[s.Key]:
			seen++
			if tc.Status != model.StatusOK {
				t.Errorf("%s: missing is_error must mean ok, got %s", s.Key, tc.Status)
			}
		case s.Key == flipped:
			if tc.Status != model.StatusError || tc.ErrorMessage == "" {
				t.Errorf("%s: want error status, got %s %q", s.Key, tc.Status, tc.ErrorMessage)
			}
		}
	}
	if seen == 0 {
		t.Error("no tool without is_error found among steps")
	}
}

func TestSubagentNestingViaMeta(t *testing.T) {
	for _, name := range []string{"async", "sync"} {
		t.Run(name, func(t *testing.T) {
			path, sid := fixture(t, name)
			metas, _ := filepath.Glob(filepath.Join(filepath.Dir(path), sid, "subagents", "*.meta.json"))
			want := map[string]subagentMeta{}
			for _, m := range metas {
				b, _ := os.ReadFile(m)
				var meta subagentMeta
				_ = json.Unmarshal(b, &meta)
				want[meta.ToolUseID] = meta
			}
			res := collectFull(t, ev("SessionEnd", path, sid, ""), nil)
			found := 0
			for _, tr := range res.Turns {
				for _, s := range tr.Steps {
					if s.Tool == nil || s.Tool.Subagent == nil {
						continue
					}
					found++
					meta, ok := want[s.Tool.CallID]
					if !ok {
						t.Errorf("subagent on %s without meta", s.Tool.CallID)
						continue
					}
					sa := s.Tool.Subagent
					if sa.Type != meta.AgentType || sa.ID == "" || len(sa.Steps) == 0 || sa.Input == "" {
						t.Errorf("subagent %+v", sa)
					}
					if s.Tool.Name != "Agent" && s.Tool.Name != "Task" {
						t.Errorf("subagent under tool %q", s.Tool.Name)
					}
				}
			}
			if found != len(want) {
				t.Errorf("attached %d subagents, meta files %d", found, len(want))
			}
		})
	}
}

// Compact summaries, task-notifications and isMeta lines do not start turns.
func TestNonPromptUserLines(t *testing.T) {
	t.Run("compact", func(t *testing.T) {
		path, sid := fixture(t, "compact")
		compact := 0
		for _, l := range lines(t, path) {
			if r, _ := decodeRecord(l); r != nil && r.IsCompactSummary {
				compact++
				if isTurnStart(r) {
					t.Error("compact summary starts a turn")
				}
			}
		}
		if compact == 0 {
			t.Fatal("fixture has no compact summary")
		}
		res := collect(t, ev("SessionEnd", path, sid, ""), nil)
		if len(res.Turns) != 1 {
			t.Fatalf("turns = %d, want 1", len(res.Turns))
		}
	})
	t.Run("notification-and-meta", func(t *testing.T) {
		path, sid := fixture(t, "async")
		notif, meta := 0, 0
		for _, l := range lines(t, path) {
			r, _ := decodeRecord(l)
			if r == nil || r.Type != "user" {
				continue
			}
			if r.originKind() == "task-notification" {
				notif++
			}
			if r.IsMeta {
				meta++
			}
			if (r.originKind() == "task-notification" || r.IsMeta) && isTurnStart(r) {
				t.Error("notification/meta line starts a turn")
			}
		}
		if notif == 0 || meta == 0 {
			t.Fatalf("fixture lacks notification (%d) or meta (%d) lines", notif, meta)
		}
		res := collect(t, ev("SessionEnd", path, sid, ""), nil)
		if len(res.Turns) != 1 {
			t.Fatalf("turns = %d, want 1 (notification rounds belong to the prompt's turn)", len(res.Turns))
		}
	})
}

// cut writes data[:n] (or everything when n<0) of the original fixture.
func cut(t *testing.T, path string, orig []byte, n int) {
	t.Helper()
	if n < 0 || n > len(orig) {
		n = len(orig)
	}
	if err := os.WriteFile(path, orig[:n], 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalPartialLine(t *testing.T) {
	path, sid := fixture(t, "plain")
	orig, _ := os.ReadFile(path)
	full := collectFull(t, ev("SessionEnd", path, sid, ""), nil)

	// Cut in the middle of a line roughly half way through.
	mid := len(orig) / 2
	nl := bytes.IndexByte(orig[mid:], '\n')
	split := mid + nl/2
	if orig[split-1] == '\n' {
		split++
	}
	cut(t, path, orig, split)
	r1 := collectFull(t, ev("Stop", path, sid, ""), nil)
	var st state
	if err := json.Unmarshal(r1.StateOnSuccess, &st); err != nil {
		t.Fatal(err)
	}
	if st.Offset > int64(split) || (st.Offset > 0 && orig[st.Offset-1] != '\n') {
		t.Fatalf("offset %d not at a line start before the cut %d", st.Offset, split)
	}

	cut(t, path, orig, -1)
	r2 := collectFull(t, ev("Stop", path, sid, ""), r1.StateOnSuccess)
	r3 := collectFull(t, ev("SessionEnd", path, sid, ""), r2.StateOnSuccess)

	emitted := map[string]int{}
	for _, r := range []model.CollectResult{r1, r2, r3} {
		for _, tr := range r.Turns {
			emitted[tr.TurnKey]++
		}
	}
	if len(emitted) != len(full.Turns) {
		t.Fatalf("incremental turns %d, full %d", len(emitted), len(full.Turns))
	}
	for _, tr := range full.Turns {
		if emitted[tr.TurnKey] != 1 {
			t.Errorf("turn %s emitted %d times", tr.TurnKey, emitted[tr.TurnKey])
		}
	}
	if len(r1.Turns) == 0 || len(r2.Turns) == 0 {
		t.Errorf("expected turns in both Stop calls: %d, %d", len(r1.Turns), len(r2.Turns))
	}
	// A further Stop with nothing new produces nothing.
	r4 := collectFull(t, ev("Stop", path, sid, ""), r2.StateOnSuccess)
	if len(r4.Turns) != 0 {
		t.Errorf("repeat Stop produced %d turns", len(r4.Turns))
	}
}

// lineIndex returns the index of the first line matching pred.
func lineIndex(t *testing.T, ls [][]byte, pred func(*record) bool) int {
	t.Helper()
	for i, l := range ls {
		if r, ok := decodeRecord(l); ok && pred(r) {
			return i
		}
	}
	t.Fatal("line not found")
	return -1
}

func TestDeferredBackgroundAgents(t *testing.T) {
	path, sid := fixture(t, "async")
	all := lines(t, path)
	firstEnd := lineIndex(t, all, func(r *record) bool {
		return r.Type == "assistant" && r.msg != nil && r.msg.StopReason == "end_turn"
	})
	firstTD := lineIndex(t, all, func(r *record) bool {
		return r.Type == "system" && r.Subtype == "turn_duration" && r.PendingAgents > 0
	})

	// Stop right after the first end_turn: agents were launched in the
	// background and have not reported back, so the turn is deferred.
	writeLines(t, path, all[:firstEnd+1])
	if r := collect(t, ev("Stop", path, sid, ""), nil); len(r.Turns) != 0 {
		t.Fatalf("Stop with pending agents emitted %d turns", len(r.Turns))
	}

	// turn_duration says pendingBackgroundAgentCount>0: still deferred...
	writeLines(t, path, all[:firstTD+1])
	r := collect(t, ev("Stop", path, sid, ""), nil)
	if len(r.Turns) != 0 {
		t.Fatalf("deferred turn emitted on Stop")
	}
	// ...until SessionEnd flushes it.
	r = collect(t, ev("SessionEnd", path, sid, ""), r.StateOnSuccess)
	if len(r.Turns) != 1 || r.Turns[0].Status != model.StatusOK {
		t.Fatalf("SessionEnd flushed %d turns", len(r.Turns))
	}

	// With the whole file the agents have reported: the last Stop emits it.
	writeLines(t, path, all)
	r = collect(t, ev("Stop", path, sid, ""), nil)
	if len(r.Turns) != 1 {
		t.Fatalf("Stop after notifications emitted %d turns", len(r.Turns))
	}
}

// A new prompt ends a deferred turn even while agents are still running.
func TestNewPromptClosesDeferredTurn(t *testing.T) {
	path, sid := fixture(t, "async")
	all := lines(t, path)
	firstTD := lineIndex(t, all, func(r *record) bool {
		return r.Type == "system" && r.Subtype == "turn_duration" && r.PendingAgents > 0
	})
	prompt := decode(t, all[lineIndex(t, all, isTurnStart)])
	prompt["uuid"] = "00000000-0000-4000-8000-000000000001"
	prompt["promptId"] = "00000000-0000-4000-8000-000000000002"
	prompt["timestamp"] = "2099-01-01T00:00:00.000Z"
	pl, _ := json.Marshal(prompt)
	writeLines(t, path, append(append([][]byte{}, all[:firstTD+1]...), pl))
	r := collect(t, ev("Stop", path, sid, ""), nil)
	if len(r.Turns) != 1 || r.Turns[0].TurnKey == "00000000-0000-4000-8000-000000000002" {
		t.Fatalf("want the deferred turn emitted, got %d", len(r.Turns))
	}
}

func TestStopHookActiveSkipped(t *testing.T) {
	path, sid := fixture(t, "plain")
	prev := []byte(`{"v":1,"offset":0}`)
	r := collect(t, ev("Stop", path, sid, `{"hook_event_name":"Stop","stop_hook_active":true}`), prev)
	if len(r.Turns) != 0 || !bytes.Equal(r.StateOnSuccess, prev) {
		t.Fatalf("stop_hook_active not skipped: %d turns, state %s", len(r.Turns), r.StateOnSuccess)
	}
}

func TestUnknownRecordTypesTolerated(t *testing.T) {
	path, sid := fixture(t, "plain")
	base := collectFull(t, ev("SessionEnd", path, sid, ""), nil)
	ls := lines(t, path)
	var out [][]byte
	for i, l := range ls {
		out = append(out, l)
		if i%10 == 3 {
			out = append(out, []byte(`{"type":"brand-new-record","uuid":"x","payload":{"a":[1,2]}}`))
			out = append(out, []byte(`{not json`))
		}
	}
	writeLines(t, path, out)
	got := collectFull(t, ev("SessionEnd", path, sid, ""), nil)
	if len(got.Turns) != len(base.Turns) {
		t.Fatalf("turns %d with unknown records, %d without", len(got.Turns), len(base.Turns))
	}
}

// The Stop hook runs before Claude Code writes turn_duration: a transcript
// ending in end_turn is a finished turn on Stop, but not on other events.
func TestStopClosesTurnBeforeTurnDuration(t *testing.T) {
	path, sid := fixture(t, "plain")
	all := lines(t, path)
	end := lineIndex(t, all, func(r *record) bool {
		return r.Type == "assistant" && r.msg != nil && r.msg.StopReason == "end_turn"
	})
	// Include every line of that message (thinking and text are separate lines).
	r0, _ := decodeRecord(all[end])
	for end+1 < len(all) {
		r, ok := decodeRecord(all[end+1])
		if !ok || r.Type != "assistant" || r.msg == nil || r.msg.ID != r0.msg.ID {
			break
		}
		end++
	}
	writeLines(t, path, all[:end+1])
	r := collect(t, ev("Stop", path, sid, ""), nil)
	if len(r.Turns) != 1 || r.Turns[0].Status != model.StatusOK || r.Turns[0].Output == "" {
		t.Fatalf("Stop: %d turns", len(r.Turns))
	}
	// The emitted turn stays in the window; turn_duration arriving later
	// does not re-send it.
	writeLines(t, path, all[:end+3])
	r2 := collect(t, ev("Stop", path, sid, ""), r.StateOnSuccess)
	if len(r2.Turns) != 0 {
		t.Fatalf("unchanged turn re-sent")
	}
}

// A turn that grows after it was emitted (notification round before the
// next prompt) is sent again under the same key.
func TestEmittedTurnResentWhenGrown(t *testing.T) {
	path, sid := fixture(t, "async")
	all := lines(t, path)
	ends := []int{}
	for i, l := range all {
		if r, ok := decodeRecord(l); ok && r.Type == "assistant" && r.msg != nil && r.msg.StopReason == "end_turn" {
			ends = append(ends, i)
		}
	}
	if len(ends) < 2 {
		t.Fatal("need two end_turn lines")
	}
	// Pretend the agents reported: drop async markers by using SessionEnd-free
	// Stop on a prefix whose pending count is taken from turn_duration = 0.
	var td []byte
	for _, l := range all {
		if r, ok := decodeRecord(l); ok && r.Type == "system" && r.Subtype == "turn_duration" {
			m := decode(t, l)
			m["pendingBackgroundAgentCount"] = 0
			td, _ = json.Marshal(m)
			break
		}
	}
	first := append(append([][]byte{}, all[:ends[0]+1]...), td)
	writeLines(t, path, first)
	r1 := collect(t, ev("Stop", path, sid, ""), nil)
	if len(r1.Turns) != 1 {
		t.Fatalf("first emission: %d", len(r1.Turns))
	}
	grown := append(append([][]byte{}, first...), all[ends[0]+1:ends[len(ends)-1]+1]...)
	writeLines(t, path, grown)
	r2 := collect(t, ev("Stop", path, sid, ""), r1.StateOnSuccess)
	if len(r2.Turns) != 1 || r2.Turns[0].TurnKey != r1.Turns[0].TurnKey {
		t.Fatalf("grown turn not re-sent: %d", len(r2.Turns))
	}
	if len(r2.Turns[0].Steps) <= len(r1.Turns[0].Steps) {
		t.Errorf("re-sent turn did not grow: %d <= %d", len(r2.Turns[0].Steps), len(r1.Turns[0].Steps))
	}
}

func TestInterruptedTurnAborted(t *testing.T) {
	path, sid := fixture(t, "plain")
	all := lines(t, path)
	starts := []int{}
	for i, l := range all {
		if r, ok := decodeRecord(l); ok && isTurnStart(r) {
			starts = append(starts, i)
		}
	}
	// Keep the first turn up to its first tool call, then interrupt and type
	// the next prompt.
	toolLine := -1
	for i := starts[0]; i < starts[1]; i++ {
		if r, ok := decodeRecord(all[i]); ok && r.Type == "assistant" && r.msg != nil && r.msg.StopReason == "tool_use" {
			toolLine = i
			break
		}
	}
	if toolLine < 0 {
		t.Skip("first turn has no tool call")
	}
	intr := decode(t, all[starts[0]])
	delete(intr, "origin")
	delete(intr, "promptSource")
	intr["uuid"] = "00000000-0000-4000-8000-0000000000aa"
	intr["promptId"] = nil
	intr["message"] = map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}
	il, _ := json.Marshal(intr)
	ls := append(append([][]byte{}, all[:toolLine+1]...), il)
	ls = append(ls, all[starts[1]:]...)
	writeLines(t, path, ls)
	r := collectFull(t, ev("SessionEnd", path, sid, ""), nil)
	if len(r.Turns) == 0 || r.Turns[0].Status != model.StatusAborted || r.Turns[0].StatusMessage != "aborted" {
		t.Fatalf("first turn status %v", r.Turns[0].Status)
	}
}

func TestRotationResets(t *testing.T) {
	path, sid := fixture(t, "plain")
	prev, _ := json.Marshal(state{V: stateVersion, Path: path, Offset: 1 << 40, Emitted: map[string]int64{"x": 1}})
	r := collect(t, ev("SessionEnd", path, sid, ""), prev)
	if len(r.Turns) != 6 {
		t.Fatalf("after rotation got %d turns, want 6", len(r.Turns))
	}
}

func TestHeadlessSDKPromptStartsTurn(t *testing.T) {
	path, sid := fixture(t, "plain")
	all := lines(t, path)
	var out [][]byte
	for _, l := range all {
		m := decode(t, l)
		if r, _ := decodeRecord(l); r != nil && isTurnStart(r) {
			delete(m, "origin")
			m["promptSource"] = "sdk"
			l, _ = json.Marshal(m)
		}
		out = append(out, l)
	}
	writeLines(t, path, out)
	r := collectFull(t, ev("SessionEnd", path, sid, ""), nil)
	if len(r.Turns) != 6 {
		t.Fatalf("sdk prompts: %d turns, want 6", len(r.Turns))
	}
}

func TestMissingTranscript(t *testing.T) {
	_, err := New().Collect(ev("Stop", "", "s", ""), nil)
	if err == nil {
		t.Fatal("want error without transcript_path")
	}
	res, err := New().Collect(ev("Stop", filepath.Join(t.TempDir(), "nope.jsonl"), "s", ""), []byte("prev"))
	if err == nil || string(res.StateOnSuccess) != "prev" {
		t.Fatalf("missing file: err=%v state=%s", err, res.StateOnSuccess)
	}
}

// On the first Collect of a session (hook installed mid-session) only the
// most recent completed turn is sent; earlier ones count as sent.
func TestFirstCollectEmitsOnlyLatestTurn(t *testing.T) {
	path, sid := fixture(t, "plain")
	full := collectFull(t, ev("SessionEnd", path, sid, ""), nil)
	if len(full.Turns) != 6 {
		t.Fatalf("fixture turns = %d", len(full.Turns))
	}

	// Whole file, first Stop: only the last turn.
	r := collect(t, ev("Stop", path, sid, ""), nil)
	if len(r.Turns) != 1 || r.Turns[0].TurnKey != full.Turns[5].TurnKey {
		t.Fatalf("first Stop emitted %d turns", len(r.Turns))
	}
	if again := collect(t, ev("Stop", path, sid, ""), r.StateOnSuccess); len(again.Turns) != 0 {
		t.Fatalf("history re-sent: %d", len(again.Turns))
	}

	// Installed after turn 3: the first Stop sends turn 3, later Stops send 4..6.
	all := lines(t, path)
	starts := []int{}
	for i, l := range all {
		if r, ok := decodeRecord(l); ok && isTurnStart(r) {
			starts = append(starts, i)
		}
	}
	writeLines(t, path, all[:starts[3]])
	r1 := collect(t, ev("Stop", path, sid, ""), nil)
	if len(r1.Turns) != 1 || r1.Turns[0].TurnKey != full.Turns[2].TurnKey {
		t.Fatalf("mid-session first Stop: %d turns", len(r1.Turns))
	}
	writeLines(t, path, all)
	r2 := collect(t, ev("Stop", path, sid, ""), r1.StateOnSuccess)
	var keys []string
	for _, tr := range r2.Turns {
		keys = append(keys, tr.TurnKey)
	}
	want := []string{full.Turns[3].TurnKey, full.Turns[4].TurnKey, full.Turns[5].TurnKey}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("later Stop sent %v, want %v", keys, want)
	}

	// First SessionEnd without any earlier Stop: also only the latest turn.
	r3 := collect(t, ev("SessionEnd", path, sid, ""), nil)
	if len(r3.Turns) != 1 || r3.Turns[0].TurnKey != full.Turns[5].TurnKey {
		t.Fatalf("first SessionEnd emitted %d turns", len(r3.Turns))
	}

	// A corrupt state counts as a first Collect too.
	r4 := collect(t, ev("Stop", path, sid, ""), []byte("not json"))
	if len(r4.Turns) != 1 {
		t.Fatalf("corrupt state: %d turns", len(r4.Turns))
	}
}

// The first-Collect rule keeps deferral: a deferred newest turn is not sent,
// and the earlier history is not sent either.
func TestFirstCollectDeferredTurn(t *testing.T) {
	path, sid := fixture(t, "async")
	all := lines(t, path)
	firstTD := lineIndex(t, all, func(r *record) bool {
		return r.Type == "system" && r.Subtype == "turn_duration" && r.PendingAgents > 0
	})
	writeLines(t, path, all[:firstTD+1])
	r := collect(t, ev("Stop", path, sid, ""), nil)
	if len(r.Turns) != 0 {
		t.Fatalf("deferred turn sent on first Stop")
	}
	writeLines(t, path, all)
	r2 := collect(t, ev("Stop", path, sid, ""), r.StateOnSuccess)
	if len(r2.Turns) != 1 {
		t.Fatalf("deferred turn not sent once agents finished: %d", len(r2.Turns))
	}
}

// A failed first delivery retries the emitted turn (and what follows) instead
// of starting over as a first Collect that would skip it as history.
func TestFirstCollectFailureRetries(t *testing.T) {
	path, sid := fixture(t, "plain")
	full := collectFull(t, ev("SessionEnd", path, sid, ""), nil)
	all := lines(t, path)
	starts := []int{}
	for i, l := range all {
		if r, ok := decodeRecord(l); ok && isTurnStart(r) {
			starts = append(starts, i)
		}
	}
	writeLines(t, path, all[:starts[3]])
	r1 := collect(t, ev("Stop", path, sid, ""), nil)
	if len(r1.Turns) != 1 || r1.StateOnFailure == nil {
		t.Fatalf("first Collect: %d turns, failure state %s", len(r1.Turns), r1.StateOnFailure)
	}
	// Delivery failed: the core stores StateOnFailure. More turns complete.
	writeLines(t, path, all)
	r2 := collect(t, ev("Stop", path, sid, ""), r1.StateOnFailure)
	var keys []string
	for _, tr := range r2.Turns {
		keys = append(keys, tr.TurnKey)
	}
	want := []string{full.Turns[2].TurnKey, full.Turns[3].TurnKey, full.Turns[4].TurnKey, full.Turns[5].TurnKey}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("retry sent %v, want %v", keys, want)
	}
	if r2.StateOnFailure != nil {
		t.Error("later Collects keep the previous state on failure (nil)")
	}
}

// A turn ends with its last model or tool activity, not with slash-command
// echoes written before the next prompt.
func TestTurnEndIsLastActivity(t *testing.T) {
	for _, name := range []string{"async", "sync", "plain", "compact"} {
		path, sid := fixture(t, name)
		for _, tr := range collectFull(t, ev("SessionEnd", path, sid, ""), nil).Turns {
			if tr.Status == model.StatusAborted {
				continue
			}
			var last time.Time
			for _, s := range tr.Steps {
				if s.End.After(last) {
					last = s.End
				}
			}
			if tr.End.After(last) {
				t.Errorf("%s turn %s ends %v after its last step %v", name, tr.TurnKey, tr.End, last)
			}
		}
	}
}

func TestSlashCommandEchoDoesNotResend(t *testing.T) {
	path, sid := fixture(t, "plain")
	all := lines(t, path)
	r1 := collect(t, ev("Stop", path, sid, ""), nil)
	if len(r1.Turns) != 1 {
		t.Fatalf("first Stop: %d", len(r1.Turns))
	}
	echo := decode(t, all[lineIndex(t, all, isTurnStart)])
	delete(echo, "origin")
	delete(echo, "promptSource")
	delete(echo, "promptId")
	echo["uuid"] = "00000000-0000-4000-8000-0000000000ec"
	echo["timestamp"] = "2099-01-01T00:00:00.000Z"
	echo["message"] = map[string]any{"role": "user", "content": "<command-name>/cost</command-name>"}
	el, _ := json.Marshal(echo)
	writeLines(t, path, append(append([][]byte{}, all...), el))
	r2 := collect(t, ev("Stop", path, sid, ""), r1.StateOnSuccess)
	if len(r2.Turns) != 0 {
		t.Fatalf("slash-command echo re-sent the turn")
	}
	r3 := collect(t, ev("SessionEnd", path, sid, ""), r2.StateOnSuccess)
	if len(r3.Turns) != 0 {
		t.Fatalf("SessionEnd re-sent the turn after an echo")
	}
}

// A model call starts when the previous step of its run ended (the run start
// for the first one) and ends at its last line, so the span shows latency.
func TestLLMStartFollowsPreviousStep(t *testing.T) {
	lastLine := map[string]time.Time{}
	var check func(t *testing.T, where string, steps []model.Step, runStart time.Time, exact bool) (int, int)
	check = func(t *testing.T, where string, steps []model.Step, runStart time.Time, exact bool) (n, longer int) {
		var prevEnd time.Time
		for i, s := range steps {
			if s.Kind == model.StepLLM {
				n++
				want := runStart
				if i > 0 {
					want = prevEnd
				}
				switch {
				case exact && !s.Start.Equal(want):
					t.Errorf("%s step %d starts %v, want %v", where, i, s.Start, want)
				case s.Start.Before(want):
					t.Errorf("%s step %d starts %v before %v", where, i, s.Start, want)
				}
				if ll, ok := lastLine[s.Key]; ok && !s.End.Equal(ll) {
					t.Errorf("%s step %d ends %v, want last line %v", where, i, s.End, ll)
				}
				if s.End.Before(s.Start) {
					t.Errorf("%s step %d ends before it starts", where, i)
				}
				if s.End.Sub(s.Start) > 10*time.Millisecond {
					longer++
				}
			}
			if s.End.After(prevEnd) {
				prevEnd = s.End
			}
			if s.Tool != nil && s.Tool.Subagent != nil {
				sa := s.Tool.Subagent
				a, b := check(t, where+"/"+sa.ID, sa.Steps, sa.Start, true)
				n, longer = n+a, longer+b
			}
		}
		return n, longer
	}
	for _, c := range []struct {
		name  string
		exact bool // no task-notification rounds in the main transcript
	}{{"plain", true}, {"sync", true}, {"compact", true}, {"async", false}} {
		t.Run(c.name, func(t *testing.T) {
			path, sid := fixture(t, c.name)
			files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), sid, "subagents", "*.jsonl"))
			for _, f := range append(files, path) {
				for _, l := range lines(t, f) {
					if r, ok := decodeRecord(l); ok && r.Type == "assistant" && r.msg != nil && !r.ts.IsZero() {
						lastLine[r.msg.ID] = r.ts
					}
				}
			}
			n, longer := 0, 0
			for _, tr := range collectFull(t, ev("SessionEnd", path, sid, ""), nil).Turns {
				a, b := check(t, tr.TurnKey, tr.Steps, tr.Start, c.exact)
				n, longer = n+a, longer+b
			}
			if n == 0 || longer*2 < n {
				t.Errorf("only %d of %d model calls last over 10 ms", longer, n)
			}
		})
	}
}
