package codex

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// maxText caps every content string kept from the rollout. The core truncates
// attributes at 20000 characters; 128 KiB is always more than that, so the
// core still sees (and flags) over-long values while memory stays bounded.
const maxText = 128 << 10

// rawRecord is one rollout line: {timestamp, type, payload, ordinal, metadata?}.
type rawRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Metadata  *struct {
		InheritedUserMessage bool `json:"inherited_user_message"`
	} `json:"metadata"`
}

type sessionMetaPayload struct {
	ID               string          `json:"id"`
	SessionID        string          `json:"session_id"`
	CLIVersion       string          `json:"cli_version"`
	Cwd              string          `json:"cwd"`
	ModelProvider    string          `json:"model_provider"`
	Source           json.RawMessage `json:"source"`
	ThreadSource     string          `json:"thread_source"`
	ParentThreadID   string          `json:"parent_thread_id"`
	AgentRole        string          `json:"agent_role"`
	AgentNickname    string          `json:"agent_nickname"`
	AgentPath        string          `json:"agent_path"`
	BaseInstructions *struct {
		Text string `json:"text"`
	} `json:"base_instructions"`
	Git *struct {
		Branch        string `json:"branch"`
		RepositoryURL string `json:"repository_url"`
	} `json:"git"`
}

type threadSpawn struct {
	ParentThreadID string `json:"parent_thread_id"`
	AgentRole      string `json:"agent_role"`
	AgentNickname  string `json:"agent_nickname"`
	AgentPath      string `json:"agent_path"`
}

// sessionMeta is what the parser keeps from the thread's own session_meta.
type sessionMeta struct {
	ThreadID     string `json:"thread_id"`
	SessionID    string `json:"session_id,omitempty"`
	CLIVersion   string `json:"cli_version,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	GitBranch    string `json:"git_branch,omitempty"`
	GitRepo      string `json:"git_repo,omitempty"`
	// Spawned is set for threads started by another thread (spawn_agent).
	Spawned *threadSpawn `json:"spawned,omitempty"`
	// Subagent is true for any subagent source, spawned or not (review...).
	Subagent bool `json:"subagent,omitempty"`
}

func newSessionMeta(p sessionMetaPayload) *sessionMeta {
	m := &sessionMeta{
		ThreadID:   p.ID,
		SessionID:  p.SessionID,
		CLIVersion: p.CLIVersion,
		Cwd:        p.Cwd,
		Provider:   p.ModelProvider,
	}
	if p.BaseInstructions != nil {
		m.Instructions = capText(p.BaseInstructions.Text)
	}
	if p.Git != nil {
		m.GitBranch = p.Git.Branch
		m.GitRepo = normalizeRepo(p.Git.RepositoryURL)
	}
	// source: "exec" | "vscode" | {"subagent": "review" | {"thread_spawn": {...}} | {...}}
	var src struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	if len(p.Source) > 0 && p.Source[0] == '{' && json.Unmarshal(p.Source, &src) == nil && len(src.Subagent) > 0 && string(src.Subagent) != "null" {
		m.Subagent = true
		var sub struct {
			ThreadSpawn *threadSpawn `json:"thread_spawn"`
		}
		if src.Subagent[0] == '{' && json.Unmarshal(src.Subagent, &sub) == nil && sub.ThreadSpawn != nil {
			m.Spawned = sub.ThreadSpawn
		}
	}
	if p.ThreadSource == "subagent" {
		m.Subagent = true
	}
	if m.Subagent && m.Spawned == nil && p.ParentThreadID != "" {
		m.Spawned = &threadSpawn{ParentThreadID: p.ParentThreadID}
	}
	if m.Spawned != nil {
		if m.Spawned.ParentThreadID == "" {
			m.Spawned.ParentThreadID = p.ParentThreadID
		}
		if m.Spawned.AgentRole == "" {
			m.Spawned.AgentRole = p.AgentRole
		}
		if m.Spawned.AgentNickname == "" {
			m.Spawned.AgentNickname = p.AgentNickname
		}
		if m.Spawned.AgentPath == "" {
			m.Spawned.AgentPath = p.AgentPath
		}
	}
	return m
}

// normalizeRepo turns a git remote into host/org/repo: no scheme, no
// credentials (flashcat.tag.repo is written at every content level), no
// trailing .git, scp form (git@host:org/repo) converted.
func normalizeRepo(remote string) string {
	r := strings.TrimSpace(remote)
	if r == "" {
		return ""
	}
	if i := strings.Index(r, "://"); i >= 0 {
		if u, err := url.Parse(r); err == nil {
			r = u.Hostname() + u.Path
		} else {
			r = r[i+3:]
			if at := strings.LastIndex(r, "@"); at >= 0 && at < strings.IndexByte(r+"/", '/') {
				r = r[at+1:]
			}
		}
	} else if colon := strings.IndexByte(r, ':'); colon > 0 && !strings.Contains(r[:colon], "/") {
		host := r[:colon]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		r = host + "/" + strings.TrimPrefix(r[colon+1:], "/")
	}
	r = strings.TrimRight(r, "/")
	r = strings.TrimSuffix(r, ".git")
	return strings.Trim(r, "/")
}

// provider follows CONTRACT §4.7: openai unless model_provider says otherwise.
func (m *sessionMeta) provider() string {
	if m == nil {
		return "openai"
	}
	p := strings.ToLower(strings.TrimSpace(m.Provider))
	if p == "" {
		return "openai"
	}
	return p
}

// sessionID is the ROOT session id (equal to the thread id for root threads).
func (m *sessionMeta) sessionID() string {
	if m == nil {
		return ""
	}
	if m.SessionID != "" {
		return m.SessionID
	}
	return m.ThreadID
}

func (m *sessionMeta) subagentType() string {
	if m != nil && m.Spawned != nil {
		if m.Spawned.AgentRole != "" {
			return m.Spawned.AgentRole
		}
		if m.Spawned.AgentNickname != "" {
			return m.Spawned.AgentNickname
		}
	}
	return "subagent"
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responseItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Phase     string          `json:"phase"`
	Content   json.RawMessage `json:"content"`
	Summary   []contentItem   `json:"summary"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Meta      *struct {
		ContentItemKinds []string `json:"content_item_kinds"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

type usage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

type eventMsg struct {
	Type             string          `json:"type"`
	TurnID           string          `json:"turn_id"`
	ThreadID         string          `json:"thread_id"`
	LastAgentMessage string          `json:"last_agent_message"`
	Error            json.RawMessage `json:"error"`
	Reason           string          `json:"reason"`
	Info             *struct {
		LastTokenUsage  *usage `json:"last_token_usage"`
		TotalTokenUsage *usage `json:"total_token_usage"`
	} `json:"info"`
	Item          json.RawMessage `json:"item"`
	StartedAtMs   int64           `json:"started_at_ms"`
	CompletedAtMs int64           `json:"completed_at_ms"`
}

// item is an event_msg/item_completed item; only the fields the parser uses.
type item struct {
	Type              string          `json:"type"`
	ID                string          `json:"id"`
	Status            string          `json:"status"`
	ExitCode          *int64          `json:"exit_code"`
	Server            string          `json:"server"`
	Tool              string          `json:"tool"`
	Result            json.RawMessage `json:"result"`
	Kind              string          `json:"kind"`
	AgentThreadID     string          `json:"agent_thread_id"`
	ReceiverThreadIDs []string        `json:"receiver_thread_ids"`
	ReceiverAgents    []struct {
		ThreadID string `json:"thread_id"`
	} `json:"receiver_agents"`

	startedAt, completedAt time.Time
}

// isError reports McpToolCall result.isError (also tolerates {"Err": ...}).
func (it *item) isError() bool {
	if len(it.Result) == 0 || it.Result[0] != '{' {
		return false
	}
	var r struct {
		IsError bool            `json:"isError"`
		Err     json.RawMessage `json:"Err"`
	}
	if json.Unmarshal(it.Result, &r) != nil {
		return false
	}
	return r.IsError || (len(r.Err) > 0 && string(r.Err) != "null")
}

type tokenUsageRecord struct {
	ResponseID string `json:"response_id"`
	TurnID     string `json:"turn_id"`
	Usage      usage  `json:"usage"`
}

type turnContext struct {
	TurnID string `json:"turn_id"`
	Model  string `json:"model"`
	Cwd    string `json:"cwd"`
}

// Known types: anything else is skipped and counted.
var (
	knownTop = set("session_meta", "turn_context", "response_item", "event_msg",
		"token_usage_record", "world_state", "inter_agent_communication_metadata", "compacted")
	knownResponse = set("message", "reasoning", "function_call", "function_call_output",
		"custom_tool_call", "custom_tool_call_output", "local_shell_call", "agent_message",
		"additional_tools", "configuration_update", "web_search_call", "tool_search_call",
		"tool_search_output", "image_generation_call", "ghost_snapshot", "compaction", "compaction_summary")
	knownEvent = set("task_started", "task_complete", "turn_complete", "turn_aborted", "token_count",
		"item_completed", "item_started", "thread_settings_applied", "thread_goal_updated",
		"user_message", "agent_message", "agent_message_delta", "agent_reasoning", "agent_reasoning_delta",
		"agent_reasoning_raw_content", "agent_reasoning_section_break", "exec_command_begin",
		"exec_command_end", "exec_command_output_delta", "mcp_tool_call_begin", "mcp_tool_call_end",
		"patch_apply_begin", "patch_apply_end", "web_search_begin", "web_search_end",
		"view_image_tool_call", "stream_error", "error", "warning", "turn_diff", "plan_update",
		"context_compacted", "entered_review_mode", "exited_review_mode", "background_event",
		"collab_agent_spawn_end", "sub_agent_activity")
	knownItem = set("UserMessage", "AgentMessage", "Reasoning", "CommandExecution", "FileChange",
		"McpToolCall", "SubAgentActivity", "CollabAgentToolCall", "Extension", "ImageView",
		"ContextCompaction", "FunctionCallOutput", "DynamicToolCall", "WebSearch", "Plan", "TodoList", "Error")
)

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// walkCtx is the parse context that must survive between Collect calls
// because the next read starts in the middle of the file.
type walkCtx struct {
	// Foreign is true inside history replayed from another thread: a forked
	// subagent rollout copies its parent's session_meta and turns.
	Foreign bool `json:"foreign,omitempty"`
	// LastTotal is the last token_count total, to drop repeated token_count
	// events in the fallback path.
	LastTotal int64 `json:"last_total,omitempty"`
}

type evKind uint8

const (
	evUser evKind = iota
	evAgentMsg
	evAssistant
	evReasoning
	evCall
	evOutput
	evItem
	evRecord
	evTokenCount
)

// event is one turn-relevant rollout record, in file order.
type event struct {
	kind       evKind
	ts         time.Time
	text       string
	phase      string
	callID     string
	name       string
	namespace  string
	args       string
	custom     bool
	failed     bool // custom tool output reported "Script failed"
	item       *item
	usage      usage
	responseID string
	dup        bool // token_count repeating the previous total
}

type endKind uint8

const (
	endOpen       endKind = iota
	endComplete           // task_complete
	endAborted            // turn_aborted
	endSuperseded         // a new task_started arrived first
	endStop               // closed by the Stop hook before task_complete was written
	endPartial            // still running (only used for nested subagent turns)
)

// turnBuf collects one turn while walking.
type turnBuf struct {
	id       string
	startOff int64
	startCtx walkCtx
	start    time.Time
	end      time.Time
	lastTS   time.Time
	model    string
	cwd      string

	foreign   bool // started inside replayed history
	inherited bool // has a response_item flagged inherited_user_message
	ownWork   bool // has records of its own (non-inherited items, usage records)
	skip      bool // already sent or filtered: do not buffer events

	ended        endKind
	lastAgentMsg string
	errText      string
	hasRecords   bool
	sawCtx       bool
	events       []event
}

func (t *turnBuf) add(e event) {
	if !t.skip {
		t.events = append(t.events, e)
	}
}

// walker turns rollout lines into turnBufs.
type walker struct {
	meta      *sessionMeta
	ctx       walkCtx
	countFrom int64 // only lines at or after this offset are counted as unknown
	unknown   map[string]int
	cur       *turnBuf
	pendCtx   *turnContext // turn_context seen outside a turn

	// keep decides whether a starting turn is buffered (false = skip).
	keep func(id string, start time.Time) bool
	// done receives every finished turn that is not inherited and not skipped.
	done func(t *turnBuf)
}

func (w *walker) count(off int64, key string) {
	if off < w.countFrom {
		return
	}
	if w.unknown == nil {
		w.unknown = map[string]int{}
	}
	w.unknown[key]++
}

func (w *walker) feed(off int64, line []byte) {
	var r rawRecord
	if err := json.Unmarshal(line, &r); err != nil {
		w.count(off, "invalid_json")
		return
	}
	ts := parseTS(r.Timestamp)
	if w.cur != nil && !ts.IsZero() {
		w.cur.lastTS = ts
	}
	switch r.Type {
	case "session_meta":
		w.onSessionMeta(r.Payload)
	case "turn_context":
		var tc turnContext
		if json.Unmarshal(r.Payload, &tc) != nil {
			return
		}
		if w.cur != nil && (tc.TurnID == "" || tc.TurnID == w.cur.id) {
			w.applyCtx(w.cur, &tc)
		} else {
			w.pendCtx = &tc
		}
	case "response_item":
		w.onResponseItem(off, ts, r)
	case "event_msg":
		w.onEvent(off, ts, r.Payload)
	case "token_usage_record":
		var rec tokenUsageRecord
		if json.Unmarshal(r.Payload, &rec) != nil || w.cur == nil {
			return
		}
		w.cur.hasRecords, w.cur.ownWork = true, true
		w.cur.add(event{kind: evRecord, ts: ts, responseID: rec.ResponseID, usage: rec.Usage})
	default:
		if !knownTop[r.Type] {
			w.count(off, r.Type)
		}
	}
}

func (w *walker) onSessionMeta(raw json.RawMessage) {
	var p sessionMetaPayload
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	if w.meta == nil {
		w.meta = newSessionMeta(p)
		return
	}
	// A second session_meta with another id starts history replayed from the
	// thread this one was forked from; our own id ends it.
	if p.ID != "" {
		w.ctx.Foreign = p.ID != w.meta.ThreadID
	}
}

func (w *walker) applyCtx(t *turnBuf, tc *turnContext) {
	if tc.Model != "" {
		t.model = tc.Model
	}
	if tc.Cwd != "" {
		t.cwd = tc.Cwd
	}
	t.sawCtx = true
}

func (w *walker) onResponseItem(off int64, ts time.Time, r rawRecord) {
	var p responseItem
	if json.Unmarshal(r.Payload, &p) != nil {
		return
	}
	if !knownResponse[p.Type] {
		w.count(off, "response_item:"+p.Type)
	}
	t := w.cur
	if t == nil {
		return
	}
	if r.Metadata != nil && r.Metadata.InheritedUserMessage {
		t.inherited = true
		return
	}
	t.ownWork = true
	switch p.Type {
	case "message":
		switch p.Role {
		case "user":
			if text, ok := userText(p, t.sawCtx); ok {
				t.add(event{kind: evUser, ts: ts, text: text})
			}
		case "assistant":
			t.add(event{kind: evAssistant, ts: ts, text: joinText(p.Content, "output_text", "text"), phase: p.Phase})
		}
	case "agent_message":
		if text := joinText(p.Content, "input_text", "text"); text != "" {
			t.add(event{kind: evAgentMsg, ts: ts, text: text})
		}
	case "reasoning":
		var parts []string
		for _, s := range p.Summary {
			if s.Text != "" {
				parts = append(parts, s.Text)
			}
		}
		if rc := joinText(p.Content, "reasoning_text", "text"); rc != "" {
			parts = append(parts, rc)
		}
		if len(parts) > 0 {
			t.add(event{kind: evReasoning, ts: ts, text: capText(strings.Join(parts, "\n"))})
		}
	case "function_call":
		t.add(event{kind: evCall, ts: ts, callID: p.CallID, name: p.Name, namespace: p.Namespace, args: capText(p.Arguments)})
	case "custom_tool_call":
		t.add(event{kind: evCall, ts: ts, callID: p.CallID, name: p.Name, namespace: p.Namespace, args: capText(p.Input), custom: true})
	case "local_shell_call":
		t.add(event{kind: evCall, ts: ts, callID: p.CallID, name: "local_shell", args: capText(string(p.Action))})
	case "function_call_output", "custom_tool_call_output":
		text := outputText(p.Output)
		t.add(event{kind: evOutput, ts: ts, callID: p.CallID, text: text,
			failed: p.Type == "custom_tool_call_output" && strings.HasPrefix(text, "Script failed")})
	}
}

func (w *walker) onEvent(off int64, ts time.Time, raw json.RawMessage) {
	var e eventMsg
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	if !knownEvent[e.Type] {
		w.count(off, "event_msg:"+e.Type)
	}
	t := w.cur
	switch e.Type {
	case "task_started":
		if t != nil {
			w.finish(endSuperseded)
		}
		nt := &turnBuf{id: e.TurnID, startOff: off, startCtx: w.ctx, start: ts, lastTS: ts, foreign: w.ctx.Foreign}
		if w.keep != nil && !w.keep(nt.id, ts) {
			nt.skip = true
		}
		if w.pendCtx != nil && (w.pendCtx.TurnID == "" || w.pendCtx.TurnID == nt.id) {
			w.applyCtx(nt, w.pendCtx)
		}
		w.pendCtx = nil
		w.cur = nt
	case "task_complete", "turn_complete":
		if t != nil && (e.TurnID == "" || e.TurnID == t.id) {
			t.end = ts
			t.lastAgentMsg = capText(e.LastAgentMessage)
			t.errText = errorText(e.Error)
			w.finish(endComplete)
		}
	case "turn_aborted":
		if t != nil && (e.TurnID == "" || e.TurnID == t.id) {
			t.end = ts
			w.finish(endAborted)
		}
	case "token_count":
		if e.Info == nil || e.Info.LastTokenUsage == nil {
			return
		}
		var total int64
		if e.Info.TotalTokenUsage != nil {
			total = e.Info.TotalTokenUsage.TotalTokens
		}
		dup := total != 0 && total == w.ctx.LastTotal
		w.ctx.LastTotal = total
		if t != nil {
			t.add(event{kind: evTokenCount, ts: ts, usage: *e.Info.LastTokenUsage, dup: dup})
		}
	case "item_completed":
		var it item
		if json.Unmarshal(e.Item, &it) != nil {
			return
		}
		if !knownItem[it.Type] {
			w.count(off, "item:"+it.Type)
		}
		if t == nil {
			return
		}
		if e.StartedAtMs > 0 {
			it.startedAt = time.UnixMilli(e.StartedAtMs).UTC()
		}
		if e.CompletedAtMs > 0 {
			it.completedAt = time.UnixMilli(e.CompletedAtMs).UTC()
		}
		t.add(event{kind: evItem, ts: ts, item: &it})
	case "thread_settings_applied":
		if w.meta != nil && (e.ThreadID == "" || e.ThreadID == w.meta.ThreadID) {
			w.ctx.Foreign = false
		}
	}
}

// finish closes the current turn and hands it to done unless it is skipped,
// replayed from another thread, or has no id.
func (w *walker) finish(how endKind) {
	t := w.cur
	w.cur = nil
	if t == nil {
		return
	}
	t.ended = how
	if t.end.IsZero() {
		t.end = t.lastTS
	}
	if t.skip || t.id == "" || t.inherited || (t.foreign && !t.ownWork) {
		return
	}
	if w.done != nil {
		w.done(t)
	}
}

// userText returns the real user prompt carried by a role=user message.
// Codex also sends AGENTS.md, environment context and skill instructions as
// user messages; content_item_kinds tells them apart: user.text, user.image,
// and user.heartbeat (the prompt of a scheduled automation) are user input.
func userText(p responseItem, afterCtx bool) (string, bool) {
	var items []contentItem
	if len(p.Content) > 0 && p.Content[0] == '[' {
		_ = json.Unmarshal(p.Content, &items)
	}
	var kinds []string
	if p.Meta != nil {
		kinds = p.Meta.ContentItemKinds
	}
	if len(kinds) > 0 {
		real := false
		var parts []string
		for i, k := range kinds {
			if !strings.HasPrefix(k, "user.") {
				continue
			}
			real = true
			if len(kinds) == len(items) && items[i].Text != "" {
				parts = append(parts, items[i].Text)
			}
		}
		if !real {
			return "", false
		}
		if len(kinds) != len(items) {
			return capText(joinText(p.Content, "input_text", "text")), true
		}
		return capText(strings.Join(parts, "\n")), true
	}
	// No kinds (older rollouts): the first user message after turn_context
	// that does not look like harness context.
	text := joinText(p.Content, "input_text", "text")
	if !afterCtx || looksLikeHarness(text) {
		return "", false
	}
	return capText(text), true
}

func looksLikeHarness(s string) bool {
	s = strings.TrimSpace(s)
	for _, p := range []string{"<environment_context", "<user_instructions", "# AGENTS.md", "<INSTRUCTIONS>", "<skill", "<turn_aborted", "<user_shell_command"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// joinText joins the text of content items whose type is one of types.
func joinText(raw json.RawMessage, types ...string) string {
	if len(raw) == 0 || raw[0] != '[' {
		return ""
	}
	var items []contentItem
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, it := range items {
		for _, t := range types {
			if it.Type == t && it.Text != "" {
				parts = append(parts, it.Text)
				break
			}
		}
	}
	return capText(strings.Join(parts, "\n"))
}

// outputText flattens a tool output: a string, or a list of content items.
func outputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		return capText(s)
	case '[':
		return joinText(raw, "input_text", "output_text", "text")
	case '{':
		// Older shapes: {"content": "...", "success": bool} or {"output": "..."}.
		var o struct {
			Content string `json:"content"`
			Output  string `json:"output"`
		}
		if json.Unmarshal(raw, &o) == nil && (o.Content != "" || o.Output != "") {
			if o.Content != "" {
				return capText(o.Content)
			}
			return capText(o.Output)
		}
		return capText(string(raw))
	}
	return ""
}

// errorText renders task_complete.error (null, string or object).
func errorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return truncate(s, 1000)
	}
	var o struct {
		Message string `json:"message"`
	}
	if raw[0] == '{' && json.Unmarshal(raw, &o) == nil && o.Message != "" {
		return truncate(o.Message, 1000)
	}
	return truncate(string(raw), 1000)
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func capText(s string) string { return truncate(s, maxText) }

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
