package claude

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

const (
	provider       = "anthropic"
	syntheticModel = "<synthetic>"
)

// run accumulates the steps of one agent run: a main-transcript turn or a
// whole subagent file. Records are fed in file order.
type run struct {
	steps   []model.Step
	llmIdx  map[string]int // message.id → index in steps
	toolIdx map[string]int // tool_use.id → index in steps

	pendingInput []model.Message // inputs of the next model call

	input    string // first prompt of the run
	output   string // text of the last assistant message that had text
	start    time.Time
	end      time.Time
	version  string
	cwd      string
	branch   string
	session  string
	lastStop string // stop_reason of the last real assistant record
	apiError string // set when the last assistant record is a synthetic API error

	aborted  bool
	async    map[string]bool // Agent tool_use ids launched in the background
	notified map[string]bool // tool_use ids reported finished by task-notifications

	// turnDurationPending is pendingBackgroundAgentCount of a system/turn_duration
	// record seen after the last end_turn; nil when there is none yet.
	turnDurationPending *int

	// lastRelevant is the end offset of the last user/assistant record; used to
	// tell whether an emitted turn grew afterwards.
	lastRelevant int64

	// lastInput is the time of the latest tool result or task-notification;
	// a model call cannot start before it.
	lastInput time.Time
}

func newRun() *run {
	return &run{
		llmIdx:   map[string]int{},
		toolIdx:  map[string]int{},
		async:    map[string]bool{},
		notified: map[string]bool{},
	}
}

func (u *run) touch(t time.Time) {
	if t.IsZero() {
		return
	}
	if u.start.IsZero() || t.Before(u.start) {
		u.start = t
	}
	if t.After(u.end) {
		u.end = t
	}
}

// add feeds one record.
func (u *run) add(r *record) {
	if r.Version != "" {
		u.version = r.Version
	}
	if r.Cwd != "" {
		u.cwd = r.Cwd
	}
	if r.GitBranch != "" {
		u.branch = r.GitBranch
	}
	if r.SessionID != "" && u.session == "" {
		u.session = r.SessionID
	}
	switch r.Type {
	case "assistant":
		u.addAssistant(r)
	case "user":
		u.addUser(r)
	case "system":
		if r.Subtype == "turn_duration" && u.lastStop == "end_turn" {
			n := r.PendingAgents
			u.turnDurationPending = &n
		}
	}
}

func (u *run) addAssistant(r *record) {
	m := r.msg
	if m == nil {
		return
	}
	if m.Model == syntheticModel {
		// Claude Code writes synthetic assistant messages for API errors and
		// local notices; they are not model calls. Only errors matter.
		if r.IsAPIError {
			u.apiError = r.errorText()
			if u.apiError == "" {
				u.apiError = "api_error"
			}
			u.lastRelevant = r.end
			u.touch(r.ts)
		}
		return
	}
	u.lastRelevant = r.end
	u.apiError = ""
	u.touch(r.ts)
	u.turnDurationPending = nil
	if m.StopReason != "" {
		u.lastStop = m.StopReason
	}
	key := m.ID
	if key == "" {
		key = r.UUID
	}
	idx, ok := u.llmIdx[key]
	if !ok {
		step := model.Step{
			Kind:  model.StepLLM,
			Key:   key,
			Start: u.llmStart(r.ts),
			End:   r.ts,
			LLM: &model.LLMCall{
				Provider:      provider,
				RequestModel:  m.Model,
				ResponseModel: m.Model,
				ResponseID:    m.ID,
				InputMessages: u.pendingInput,
			},
		}
		u.pendingInput = nil
		u.steps = append(u.steps, step)
		idx = len(u.steps) - 1
		u.llmIdx[key] = idx
	}
	step := &u.steps[idx]
	if !r.ts.IsZero() {
		if step.Start.IsZero() {
			step.Start = r.ts
		}
		step.End = r.ts
	}
	call := step.LLM
	if m.StopReason != "" {
		call.FinishReasons = []string{m.StopReason}
	}
	if m.Usage != nil {
		// Every line of a message carries the same (or cumulative) usage:
		// the last line wins, so it is counted once.
		call.Usage = model.Usage{
			InputTokens:      m.Usage.InputTokens + m.Usage.CacheReadTokens + m.Usage.CacheCreationTokens,
			OutputTokens:     m.Usage.OutputTokens,
			CacheReadTokens:  m.Usage.CacheReadTokens,
			CacheWriteTokens: m.Usage.CacheCreationTokens,
		}
		if m.Usage.OutputDetails != nil {
			call.Usage.ReasoningTokens = m.Usage.OutputDetails.ThinkingTokens
		}
	}
	var text []string
	for _, b := range r.blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				text = append(text, b.Text)
			}
			appendPart(call, model.Part{Type: "text", Content: b.Text})
		case "thinking":
			if b.Thinking != "" {
				appendPart(call, model.Part{Type: "reasoning", Content: b.Thinking})
			}
		case "tool_use":
			args := string(b.Input)
			appendPart(call, model.Part{Type: "tool_call", ID: b.ID, Name: b.Name, Arguments: args})
			if _, dup := u.toolIdx[b.ID]; dup || b.ID == "" {
				continue
			}
			u.steps = append(u.steps, model.Step{
				Kind:  model.StepTool,
				Key:   b.ID,
				Start: r.ts,
				Tool: &model.ToolCall{
					Name:      b.Name,
					CallID:    b.ID,
					Arguments: args,
					Status:    model.StatusOK,
				},
			})
			u.toolIdx[b.ID] = len(u.steps) - 1
		}
	}
	if len(text) > 0 {
		u.output = strings.Join(text, "\n")
	}
}

// llmStart is when a model call whose first line was written at first
// started: the latest end of the earlier steps of this run (the run start for
// the first step), but not before the latest input that triggered it (a
// task-notification arriving after the previous reply), and never after its
// own first line. This makes the span cover the model latency.
func (u *run) llmStart(first time.Time) time.Time {
	start := u.start
	for _, s := range u.steps {
		if s.End.After(start) {
			start = s.End
		}
	}
	if u.lastInput.After(start) {
		start = u.lastInput
	}
	if start.IsZero() || (!first.IsZero() && start.After(first)) {
		return first
	}
	return start
}

// appendPart adds a part to the call's single assistant output message,
// merging consecutive text parts (streamed lines of one message).
func appendPart(call *model.LLMCall, p model.Part) {
	if len(call.OutputMessages) == 0 {
		call.OutputMessages = []model.Message{{Role: "assistant"}}
	}
	msg := &call.OutputMessages[0]
	if p.Type == "text" {
		if p.Content == "" {
			return
		}
		if n := len(msg.Parts); n > 0 && msg.Parts[n-1].Type == "text" {
			msg.Parts[n-1].Content += p.Content
			return
		}
	}
	msg.Parts = append(msg.Parts, p)
}

func (u *run) addUser(r *record) {
	if u.lastStop == "end_turn" && !r.hasToolResult() && !r.isInterrupt() && r.originKind() != "task-notification" {
		// After the reply: local slash-command echoes (/model, /cost…) and
		// caveat lines belong to no model call and must not stretch the turn
		// or make an emitted turn look grown.
		return
	}
	u.lastRelevant = r.end
	u.touch(r.ts)
	if (r.hasToolResult() || r.originKind() == "task-notification") && r.ts.After(u.lastInput) {
		u.lastInput = r.ts
	}
	if r.isInterrupt() {
		u.aborted = true
		return
	}
	if r.originKind() == "task-notification" {
		for _, id := range r.notifiedToolUseIDs() {
			u.notified[id] = true
		}
	}
	if r.hasToolResult() {
		var parts []model.Part
		res := r.agentResult()
		for _, b := range r.blocks {
			if b.Type != "tool_result" {
				continue
			}
			text := contentText(b.Content)
			parts = append(parts, model.Part{Type: "tool_call_response", ID: b.ToolUseID, Response: text})
			idx, ok := u.toolIdx[b.ToolUseID]
			if !ok {
				continue
			}
			step := &u.steps[idx]
			step.End = r.ts
			step.Tool.Result = text
			if b.isError() {
				step.Tool.Status = model.StatusError
				step.Tool.ErrorMessage = firstLine(text)
			}
			if res.Status == "async_launched" || res.IsAsync {
				u.async[b.ToolUseID] = true
			}
			if res.AgentID != "" && step.Tool.Subagent == nil {
				// Placeholder carrying the agent id; replaced by the parsed
				// subagent file when one is found.
				step.Tool.Subagent = &model.Subagent{ID: res.AgentID}
			}
		}
		if len(parts) > 0 {
			u.pendingInput = append(u.pendingInput, model.Message{Role: "tool", Parts: parts})
		}
		return
	}
	text := r.userText()
	if u.input == "" && !r.IsMeta {
		u.input = text
	}
	if text != "" {
		u.pendingInput = append(u.pendingInput, model.Message{Role: "user", Parts: []model.Part{{Type: "text", Content: text}}})
	}
}

// pendingBackgroundAgents is how many background agents of this run have not
// reported back. A turn_duration record after the last end_turn is
// authoritative; without one (the Stop hook runs before Claude Code writes it)
// it is derived from async Agent launches without a matching task-notification.
func (u *run) pendingBackgroundAgents() int {
	if u.turnDurationPending != nil {
		return *u.turnDurationPending
	}
	n := 0
	for id := range u.async {
		if !u.notified[id] {
			n++
		}
	}
	return n
}

func (u *run) status() (model.Status, string) {
	switch {
	case u.apiError != "":
		return model.StatusError, u.apiError
	case u.aborted:
		return model.StatusAborted, "aborted"
	}
	return model.StatusOK, ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 512 {
		cut := 512
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}
