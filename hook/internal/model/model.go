// Package model is the frozen contract between the per-tool parsers
// (internal/claude, internal/codex, internal/cursor) and the core
// (state, OTLP encoding, delivery). See ../../CONTRACT.md.
//
// Parsers turn hook events and local transcripts into Turns. They never
// encode OTLP, never touch the network and never write files other than
// through the state blob they return; the core does all of that.
package model

import (
	"encoding/json"
	"time"
)

// Tool is the coding tool a turn came from. The value is written to
// gen_ai.agent.name and flashcat.tag.coding_tool.
type Tool string

const (
	ToolClaude Tool = "claude-code"
	ToolCodex  Tool = "codex"
	ToolCursor Tool = "cursor"
)

// Content is how much conversation content the core writes. Parsers always
// fill content fields; the core drops them according to the configured level.
type Content string

const (
	ContentNone          Content = "none"            // structure, timing, tokens only (default)
	ContentNoToolContent Content = "no_tool_content" // + prompts, replies, model messages
	ContentFull          Content = "full"            // + tool arguments and results
)

// Status of a turn, a subagent or a tool call.
type Status string

const (
	StatusOK      Status = "ok"
	StatusError   Status = "error"
	StatusAborted Status = "aborted" // user interrupted; exported as span status ERROR
)

// HookEvent is one invocation of `flashcat-hook hook <tool>`.
type HookEvent struct {
	Tool           Tool
	Name           string          // hook_event_name as sent by the tool
	SessionID      string          // session_id / conversation_id from the payload
	TranscriptPath string          // transcript_path from the payload, may be empty
	Cwd            string          // cwd, or first workspace root for Cursor
	Raw            json.RawMessage // the full payload, for tool-specific fields
	// Final is true when the session is ending (Claude Code SessionEnd,
	// Cursor sessionEnd): deferred turns must be flushed now.
	Final bool
	Now   time.Time
}

// CollectResult is what a parser returns for one hook event.
//
// The core persists StateOnSuccess after Turns are delivered, or right away
// when Turns is empty. When delivery fails it persists StateOnFailure; nil
// means "keep the previous state unchanged" so the same turns are produced
// again on the next event. Cursor uses StateOnFailure to keep events it has
// just appended to its event log.
type CollectResult struct {
	Turns          []Turn
	StateOnSuccess []byte
	StateOnFailure []byte
}

// Parser is implemented by each tool package as `func New() model.Parser`.
type Parser interface {
	// Collect reads whatever is new since prev (the parser's own opaque
	// state for this session, nil on first call) and returns completed turns.
	// It must tolerate unknown record types and partial trailing lines.
	Collect(ev HookEvent, prev []byte) (CollectResult, error)
}

// Turn is one user prompt and everything the agent did until it stopped.
// It becomes one trace: an invoke_agent root span with Steps as children.
type Turn struct {
	Tool        Tool
	SessionID   string // gen_ai.conversation.id (Codex: root session_id, not thread id)
	TurnKey     string // stable per turn, used to derive trace_id
	ToolVersion string // CLI / app version, written to service.version
	UserID      string // optional; core falls back to config user, then git email

	Cwd       string
	Repo      string // repository name or URL if known; core may derive from Cwd
	GitBranch string

	Start time.Time
	End   time.Time

	Input              string // the user prompt
	Output             string // final assistant reply
	SystemInstructions string // Codex base instructions; empty for Claude Code

	Status        Status
	StatusMessage string

	// Steps are model calls and tool calls in start order. They are siblings
	// under the root: a tool runs after the model call that requested it.
	Steps []Step
}

// StepKind tells which of Step.LLM / Step.Tool is set.
type StepKind string

const (
	StepLLM  StepKind = "llm"
	StepTool StepKind = "tool"
)

// Step is one model call or one tool call.
type Step struct {
	Kind  StepKind
	Key   string // stable within the turn (message.id, response_id, call_id...), used to derive span_id
	Start time.Time
	End   time.Time
	LLM   *LLMCall
	Tool  *ToolCall
}

// LLMCall is one model request.
type LLMCall struct {
	Provider      string // anthropic / openai / google / cursor ... ; never empty
	RequestModel  string
	ResponseModel string
	ResponseID    string
	FinishReasons []string
	Usage         Usage

	// InputMessages is what is new in this request (the user prompt or the
	// previous tool results); OutputMessages is text, reasoning and tool calls.
	InputMessages  []Message
	OutputMessages []Message

	// ReportedCostUSD is a cost the tool itself reported (Cursor
	// estimated_cost). 0 means "not reported, price it from the table".
	ReportedCostUSD float64

	// UsageInOtherCall means this call's tokens are counted on another call
	// of the same turn (Cursor reports usage once per turn). The core writes
	// explicit zero tokens plus flashcat.usage.aggregated=true, so the call is
	// not reported as missing usage while the turn total stays right.
	UsageInOtherCall bool
}

// Usage follows OTel GenAI semantics: InputTokens INCLUDES cache reads and
// cache writes; OutputTokens includes reasoning. Set only what is known.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

// ToolCall is one tool execution.
type ToolCall struct {
	Name         string
	CallID       string
	Arguments    string // raw JSON or text
	Result       string
	Status       Status
	ErrorMessage string
	// Subagent is set when the tool spawned a subagent (Claude Code Agent/Task,
	// Codex spawn_agent, Cursor Task). Its steps become children of this span.
	Subagent *Subagent
}

// Subagent is a nested agent run under a tool call.
type Subagent struct {
	Type          string // subagent type, written to flashcat.tag.subagent
	ID            string
	Start         time.Time
	End           time.Time
	Input         string
	Output        string
	Status        Status
	StatusMessage string
	Steps         []Step
}

// Message is a gen_ai.input.messages / gen_ai.output.messages entry.
type Message struct {
	Role  string // user / assistant / system / tool
	Parts []Part
}

// Part follows the OTel GenAI message part shapes.
type Part struct {
	Type      string // text / reasoning / tool_call / tool_call_response
	Content   string // text or reasoning content
	ID        string // tool call id
	Name      string // tool name
	Arguments string // tool_call arguments (raw JSON)
	Response  string // tool_call_response result
}
