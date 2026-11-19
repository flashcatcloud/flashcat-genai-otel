package cursor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

// minModelGap: a gap between two tool calls shorter than this is hook
// latency between calls of one model response, not a model call. A gap
// before the first tool and after the last one is always a model call.
const minModelGap = 500 * time.Millisecond

// built is a turn plus what must be remembered once it is sent: the
// subagent generations it nested and its Task / subagent call ids, so late
// events for them are dropped.
type built struct {
	turn      model.Turn
	childGens []string
	calls     []string
}

// buildTurn assembles one generation from its events (time ordered; the
// parent conversation's and its subagents').
func buildTurn(gen string, evs []event, fb turnContext) built {
	var (
		stop, prompt   *event
		lastResp       *event
		root, children []event
		subStops       = map[string]*event{}
	)
	for i := range evs {
		e := &evs[i]
		if e.Child {
			children = append(children, *e)
			continue
		}
		root = append(root, *e)
		switch e.Name {
		case evStop:
			stop = e
		case evPrompt:
			if prompt == nil {
				prompt = e
			}
		case evResponse:
			lastResp = e
		case evSubStop:
			if e.ID != "" {
				subStops[e.ID] = e
			}
		}
	}
	ctx := func(get func(*event) string) string {
		for _, e := range []*event{stop, lastResp, prompt} {
			if e != nil && get(e) != "" {
				return get(e)
			}
		}
		return ""
	}
	t := model.Turn{
		Tool:        model.ToolCursor,
		SessionID:   firstNonEmpty(convOf(root), fb.Session),
		TurnKey:     gen,
		ToolVersion: firstNonEmpty(ctx(func(e *event) string { return e.Version }), fb.Version),
		UserID:      firstNonEmpty(ctx(func(e *event) string { return e.User }), fb.User),
		Cwd:         firstNonEmpty(ctx(func(e *event) string { return e.Cwd }), fb.Cwd),
		Status:      model.StatusOK,
	}
	if prompt != nil {
		t.Input = prompt.Text
	}
	if lastResp != nil {
		t.Output = lastResp.Text
	}
	switch {
	case stop == nil:
		t.Status, t.StatusMessage = model.StatusAborted, "aborted"
	case stop.Status == "aborted":
		t.Status, t.StatusMessage = model.StatusAborted, "aborted"
	case stop.Status == "error":
		t.Status, t.StatusMessage = model.StatusError, "error"
	}

	// Tool calls of the parent agent, one per tool_use_id.
	var tools []model.Step
	toolAt := map[string]int{}
	for _, e := range root {
		if e.Name != evTool && e.Name != evToolFailure {
			continue
		}
		if _, dup := toolAt[e.ID]; dup && e.ID != "" {
			continue
		}
		toolAt[e.ID] = len(tools)
		tools = append(tools, toolStep(e, len(tools)))
	}

	// Turn window.
	t.Start, t.End = evs[0].At, evs[len(evs)-1].At
	if prompt != nil {
		t.Start = prompt.At
	} else {
		for _, s := range tools {
			if s.Start.Before(t.Start) {
				t.Start = s.Start
			}
		}
	}
	if stop != nil {
		t.End = stop.At
	}

	// Subagents: group the children by the Task call they belong to.
	groups, order, childGens := groupChildren(children, subStops)
	for k := range subStops {
		if _, ok := groups[k]; !ok {
			groups[k] = nil
			order = append(order, k)
		}
	}
	sort.Strings(order)
	for _, k := range order {
		idx, ok := toolAt[k]
		if !ok {
			idx = len(tools)
			toolAt[k] = idx
			tools = append(tools, taskStep(k, subStops[k], groups[k], t))
		}
		s := &tools[idx]
		s.Tool.Subagent = buildSubagent(k, s, subStops[k], groups[k], t.Status)
	}
	sortSteps(tools)

	// Model calls between tool calls; the turn's token totals go on the last.
	end := t.End
	if lastResp != nil && !lastResp.At.Before(lastToolEnd(tools)) {
		end = lastResp.At
	}
	m := firstNonEmpty(ctx(func(e *event) string { return e.Model }), modelOf(root, evPrompt, evResponse, evStop, evThought))
	llm := modelCalls(t.Start, end, tools, m, t.Input, t.Output)
	var usageCall *model.LLMCall
	if n := len(llm); n > 0 {
		var u *usage
		if stop != nil && !stop.Usage.empty() {
			u = stop.Usage
		} else {
			for _, e := range root {
				if e.Name == evResponse && !e.Usage.empty() {
					u = e.Usage
				}
			}
		}
		if u != nil {
			c := llm[n-1].LLM
			c.Usage = mapUsage(u)
			c.ReportedCostUSD = u.CostCents / 100
			usageCall = c
		}
	}
	t.Steps = append(llm, tools...)
	sortSteps(t.Steps)
	var calls []string
	for _, s := range tools {
		if s.Tool.CallID != "" && (s.Tool.Subagent != nil || strings.EqualFold(s.Tool.Name, "Task")) {
			calls = append(calls, s.Tool.CallID)
		}
	}
	if usageCall != nil {
		markUsageInOtherCall(t.Steps, usageCall)
	}
	return built{turn: t, childGens: childGens, calls: calls}
}

// markUsageInOtherCall flags every model call of the turn, subagents
// included, except the one holding the turn's token totals: Cursor reports
// usage once per turn, so the others have none of their own.
func markUsageInOtherCall(steps []model.Step, keep *model.LLMCall) {
	for _, s := range steps {
		if s.LLM != nil && s.LLM != keep {
			s.LLM.UsageInOtherCall = true
		}
		if s.Tool != nil && s.Tool.Subagent != nil {
			markUsageInOtherCall(s.Tool.Subagent.Steps, keep)
		}
	}
}

// groupChildren groups a subagent's events by the Task tool call that
// started it: parent_tool_call_id, else learned from sibling events with the
// same conversation / generation, else from subagentStop's
// child_conversation_id, else one group per child conversation.
func groupChildren(children []event, subStops map[string]*event) (map[string][]event, []string, []string) {
	byConv, byGen := map[string]string{}, map[string]string{}
	for k, s := range subStops {
		if s.Sub != "" {
			byConv[s.Sub] = k
		}
	}
	for _, e := range children {
		if e.Parent == "" {
			continue
		}
		if byConv[e.Conv] == "" {
			byConv[e.Conv] = e.Parent
		}
		if byGen[e.Gen] == "" {
			byGen[e.Gen] = e.Parent
		}
	}
	groups := map[string][]event{}
	var order, gens []string
	seenGen := map[string]bool{}
	for _, e := range children {
		k := firstNonEmpty(e.Parent, byConv[e.Conv], byGen[e.Gen])
		if k == "" {
			k = "subagent:" + e.Conv
		}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
		if e.Gen != "" && !seenGen[e.Gen] {
			seenGen[e.Gen] = true
			gens = append(gens, e.Gen)
		}
	}
	return groups, order, gens
}

// buildSubagent nests a subagent run under its Task step. Its window is the
// Task step's (subagentStop arrives after the Task call ended); its steps
// are its tool calls plus the model calls between them. Cursor reports no
// per-subagent tokens.
func buildSubagent(key string, task *model.Step, stop *event, evs []event, turnStatus model.Status) *model.Subagent {
	sa := &model.Subagent{Type: "subagent", ID: key, Start: task.Start, End: task.End, Status: model.StatusOK}
	if stop != nil {
		sa.Type = firstNonEmpty(stop.Type, sa.Type)
		sa.ID = firstNonEmpty(stop.Sub, sa.ID)
		sa.Input = stop.Text
		sa.Status, sa.StatusMessage = subagentStatus(stop.Status)
	} else {
		if c := convOf(evs); c != "" {
			sa.ID = c
		}
		switch {
		case task.Tool.Status != model.StatusOK:
			sa.Status, sa.StatusMessage = task.Tool.Status, task.Tool.ErrorMessage
		case turnStatus == model.StatusAborted:
			sa.Status, sa.StatusMessage = model.StatusAborted, "aborted"
		}
	}
	var tools []model.Step
	seen := map[string]bool{}
	for _, e := range evs {
		if e.Name != evTool && e.Name != evToolFailure {
			continue
		}
		if seen[e.ID] && e.ID != "" {
			continue
		}
		seen[e.ID] = true
		tools = append(tools, toolStep(e, len(tools)))
	}
	sortSteps(tools)
	m := modelOf(evs, evThought, evTool, evToolFailure)
	if m == "" && stop != nil {
		m = stop.Model
	}
	sa.Steps = append(modelCalls(sa.Start, sa.End, tools, m, sa.Input, ""), tools...)
	sortSteps(sa.Steps)
	return sa
}

// taskStep stands in for a Task call whose postToolUse never came (the turn
// was interrupted): its window is subagentStop's, else its events'.
func taskStep(key string, stop *event, evs []event, t model.Turn) model.Step {
	s := model.Step{Kind: model.StepTool, Key: key, Tool: &model.ToolCall{Name: "Task", Status: model.StatusOK}}
	if !strings.HasPrefix(key, "subagent:") {
		s.Tool.CallID = key
	}
	switch {
	case stop != nil:
		s.End = stop.At
		s.Start = stop.At.Add(-millis(stop.Dur))
	case len(evs) > 0:
		s.Start, s.End = evs[0].At, evs[len(evs)-1].At
		for _, e := range evs {
			if st := e.At.Add(-millis(e.Dur)); (e.Name == evTool || e.Name == evToolFailure) && st.Before(s.Start) {
				s.Start = st
			}
		}
	default:
		s.Start, s.End = t.Start, t.End
	}
	status, msg := model.StatusOK, ""
	if stop != nil {
		status, msg = subagentStatus(stop.Status)
	} else if t.Status == model.StatusAborted {
		status, msg = model.StatusAborted, "aborted"
	}
	s.Tool.Status, s.Tool.ErrorMessage = status, msg
	return s
}

func subagentStatus(s string) (model.Status, string) {
	switch strings.ToLower(s) {
	case "", "completed", "success", "succeeded":
		return model.StatusOK, ""
	case "aborted", "cancelled", "canceled", "interrupted":
		return model.StatusAborted, "aborted"
	}
	return model.StatusError, s
}

// toolStep: End is when the hook ran, Start is End minus Cursor's duration.
// A failure's message is failure_type (a status word, safe at every content
// level); error_message is the tool result.
func toolStep(e event, n int) model.Step {
	key := e.ID
	if key == "" {
		key = fmt.Sprintf("tool-%d", n+1)
	}
	tc := &model.ToolCall{
		Name:      firstNonEmpty(e.Tool, "unknown"),
		CallID:    e.ID,
		Arguments: e.Args,
		Result:    e.Result,
		Status:    model.StatusOK,
	}
	if e.Name == evToolFailure {
		if e.Intr {
			tc.Status, tc.ErrorMessage = model.StatusAborted, "aborted"
		} else {
			tc.Status, tc.ErrorMessage = model.StatusError, firstNonEmpty(e.Fail, "error")
		}
	}
	return model.Step{Kind: model.StepTool, Key: key, Start: e.At.Add(-millis(e.Dur)), End: e.At, Tool: tc}
}

// modelCalls fills [start, end] with model calls: one before the first tool
// call, one in each gap between tool calls (overlapping calls, and calls
// less than minModelGap apart, form one block), and one after the last
// tool call. Cursor has no per-call data, so this is inferred from timing.
// The first call's input is the prompt; each call's output is the tool
// calls of the block after it, and the last call's output is the reply.
func modelCalls(start, end time.Time, tools []model.Step, mdl, input, output string) []model.Step {
	type block struct {
		start, end time.Time
		steps      []*model.Step
	}
	var blocks []*block
	for i := range tools {
		s := &tools[i]
		if n := len(blocks); n > 0 && s.Start.Before(blocks[n-1].end.Add(minModelGap)) {
			b := blocks[n-1]
			if s.End.After(b.end) {
				b.end = s.End
			}
			b.steps = append(b.steps, s)
			continue
		}
		blocks = append(blocks, &block{start: s.Start, end: s.End, steps: []*model.Step{s}})
	}

	var out []model.Step
	add := func(s, e time.Time, in []model.Message, outMsgs []model.Message) {
		if e.Before(s) {
			e = s
		}
		out = append(out, model.Step{
			Kind:  model.StepLLM,
			Key:   fmt.Sprintf("llm-%d", len(out)+1),
			Start: s,
			End:   e,
			LLM: &model.LLMCall{
				Provider:       provider(mdl),
				RequestModel:   mdl,
				ResponseModel:  mdl,
				InputMessages:  in,
				OutputMessages: outMsgs,
			},
		})
	}
	in := textMessages("user", input)
	from := start
	for _, b := range blocks {
		if b.start.After(from) {
			add(from, b.start, in, toolCalls(b.steps))
			in = toolResults(b.steps)
		} else {
			in = append(in, toolResults(b.steps)...)
		}
		if b.end.After(from) {
			from = b.end
		}
	}
	add(from, end, in, textMessages("assistant", output))
	return out
}

func toolCalls(steps []*model.Step) []model.Message {
	parts := make([]model.Part, 0, len(steps))
	for _, s := range steps {
		parts = append(parts, model.Part{Type: "tool_call", ID: s.Tool.CallID, Name: s.Tool.Name, Arguments: s.Tool.Arguments})
	}
	return []model.Message{{Role: "assistant", Parts: parts}}
}

func toolResults(steps []*model.Step) []model.Message {
	var msgs []model.Message
	for _, s := range steps {
		msgs = append(msgs, model.Message{Role: "tool", Parts: []model.Part{{Type: "tool_call_response", ID: s.Tool.CallID, Response: s.Tool.Result}}})
	}
	return msgs
}

func textMessages(role, text string) []model.Message {
	if text == "" {
		return nil
	}
	return []model.Message{{Role: role, Parts: []model.Part{{Type: "text", Content: text}}}}
}

// mapUsage: Cursor's input_tokens already include cache reads and writes;
// if the cache counts exceed it they cannot be included, so add them.
func mapUsage(u *usage) model.Usage {
	in := u.Input
	if u.CacheRead+u.CacheWrite > in {
		in += u.CacheRead + u.CacheWrite
	}
	return model.Usage{InputTokens: in, OutputTokens: u.Output, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite}
}

// provider maps a Cursor model name to gen_ai.provider.name (CONTRACT §4.7).
func provider(m string) string {
	s := strings.ToLower(strings.TrimSpace(m))
	switch {
	case strings.HasPrefix(s, "claude"):
		return "anthropic"
	case strings.HasPrefix(s, "gpt"), strings.HasPrefix(s, "o1"), strings.HasPrefix(s, "o3"),
		strings.HasPrefix(s, "o4"), strings.HasPrefix(s, "codex"):
		return "openai"
	case strings.HasPrefix(s, "gemini"):
		return "google"
	case strings.HasPrefix(s, "grok"):
		return "xai"
	case strings.HasPrefix(s, "deepseek"):
		return "deepseek"
	}
	return "cursor"
}

// modelOf is the first model named by an event of the given kinds.
func modelOf(evs []event, names ...string) string {
	for _, n := range names {
		for _, e := range evs {
			if e.Name == n && e.Model != "" {
				return e.Model
			}
		}
	}
	return ""
}

func convOf(evs []event) string {
	for _, e := range evs {
		if e.Conv != "" {
			return e.Conv
		}
	}
	return ""
}

func lastToolEnd(steps []model.Step) time.Time {
	var t time.Time
	for _, s := range steps {
		if s.End.After(t) {
			t = s.End
		}
	}
	return t
}

func millis(ms float64) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms * float64(time.Millisecond))
}

func sortSteps(steps []model.Step) {
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Start.Before(steps[j].Start) })
}
