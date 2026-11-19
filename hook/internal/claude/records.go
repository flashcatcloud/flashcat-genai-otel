package claude

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// record is one transcript line. Only the fields the parser needs are
// decoded; everything that may change shape between Claude Code versions is
// kept as json.RawMessage and decoded leniently.
type record struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	UUID             string          `json:"uuid"`
	PromptID         string          `json:"promptId"`
	SessionID        string          `json:"sessionId"`
	Timestamp        string          `json:"timestamp"`
	Version          string          `json:"version"`
	Cwd              string          `json:"cwd"`
	GitBranch        string          `json:"gitBranch"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	IsSidechain      bool            `json:"isSidechain"`
	IsAPIError       bool            `json:"isApiErrorMessage"`
	PromptSource     string          `json:"promptSource"`
	Origin           json.RawMessage `json:"origin"`
	Message          json.RawMessage `json:"message"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
	Error            json.RawMessage `json:"error"`
	PendingAgents    int             `json:"pendingBackgroundAgentCount"`

	// filled by the reader
	start, end int64 // byte range of the line in the file (end includes '\n')
	msg        *message
	blocks     []block
	ts         time.Time
}

type message struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	Content    json.RawMessage `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      *usage          `json:"usage"`
}

type usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	OutputDetails       *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   json.RawMessage `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// knownTypes are record types the parser reads or deliberately ignores.
// Anything else is counted as unknown and logged, to spot format changes.
var knownTypes = map[string]bool{
	"user": true, "assistant": true, "system": true, "attachment": true, "summary": true,
	"api-request": true, "api-request-blob": true, "api-request-shape": true,
	"last-prompt": true, "atis-latch": true, "mode": true, "ai-title": true, "custom-title": true,
	"permission-mode": true, "file-history-snapshot": true, "file-history-delta": true,
	"queue-operation": true, "tools-used": true, "agent-name": true, "agent-setting": true,
	"cost-state": true, "pr-link": true, "relocated": true, "worktree-state": true,
	"continued-in": true, "progress": true, "artifact-autoreact-ledger": true,
	"artifact-comment-monitor": true,
}

// decodeRecord parses one line. ok=false means the line is not valid JSON.
func decodeRecord(line []byte) (*record, bool) {
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, false
	}
	if len(r.Message) > 0 && r.Message[0] == '{' {
		var m message
		if json.Unmarshal(r.Message, &m) == nil {
			r.msg = &m
			r.blocks = decodeBlocks(m.Content)
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, r.Timestamp); err == nil {
		r.ts = t
	}
	return &r, true
}

func decodeBlocks(raw json.RawMessage) []block {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]block, 0, len(items))
	for _, it := range items {
		var b block
		if json.Unmarshal(it, &b) == nil {
			out = append(out, b)
		}
	}
	return out
}

// originKind returns origin.kind ("human", "task-notification", ...).
func (r *record) originKind() string {
	if len(r.Origin) == 0 {
		return ""
	}
	var o struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(r.Origin, &o) == nil {
		return o.Kind
	}
	var s string
	if json.Unmarshal(r.Origin, &s) == nil {
		return s
	}
	return ""
}

// isToolResultOnly reports whether a user record carries only tool results.
func (r *record) isToolResultOnly() bool {
	if len(r.blocks) == 0 {
		return false
	}
	for _, b := range r.blocks {
		if b.Type != "tool_result" {
			return false
		}
	}
	return true
}

func (r *record) hasToolResult() bool {
	for _, b := range r.blocks {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

// userText is the text of a user message: string content, or the text
// blocks of array content joined with newlines.
func (r *record) userText() string {
	if r.msg == nil {
		return ""
	}
	return contentText(r.msg.Content)
}

// contentText flattens a message or tool_result content value to text.
func contentText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	case '[':
		var parts []string
		for _, b := range decodeBlocks(raw) {
			switch b.Type {
			case "text":
				parts = append(parts, b.Text)
			case "image":
				parts = append(parts, "[image]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// isError reads tool_result.is_error; missing or non-boolean means false.
func (b block) isError() bool {
	var v bool
	if len(b.IsError) == 0 || json.Unmarshal(b.IsError, &v) != nil {
		return false
	}
	return v
}

// interruptPrefix is the fixed text Claude Code writes when the user presses Esc.
const interruptPrefix = "[Request interrupted by user"

func (r *record) isInterrupt() bool {
	if r.Type != "user" || r.hasToolResult() {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(r.userText()), interruptPrefix)
}

// toolUseResult fields the parser needs for Agent calls.
type toolUseResult struct {
	Status  string `json:"status"`
	IsAsync bool   `json:"isAsync"`
	AgentID string `json:"agentId"`
}

func (r *record) agentResult() toolUseResult {
	var t toolUseResult
	if len(r.ToolUseResult) > 0 && r.ToolUseResult[0] == '{' {
		_ = json.Unmarshal(r.ToolUseResult, &t)
	}
	return t
}

func (r *record) errorText() string {
	if len(r.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(r.Error, &s) == nil {
		return s
	}
	return string(r.Error)
}

var notifiedToolUseRe = regexp.MustCompile(`<tool-use-id>\s*([^<\s]+)\s*</tool-use-id>`)

// notifiedToolUseIDs extracts the tool-use ids a task-notification reports as
// finished (Claude Code writes them as <tool-use-id>…</tool-use-id>).
func (r *record) notifiedToolUseIDs() []string {
	var ids []string
	for _, m := range notifiedToolUseRe.FindAllStringSubmatch(r.userText(), -1) {
		ids = append(ids, m[1])
	}
	return ids
}
