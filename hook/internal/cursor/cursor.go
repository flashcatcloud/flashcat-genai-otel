// Package cursor turns Cursor hook events into model.Turns (CONTRACT §5.3).
//
// Cursor's local transcript has no ids, times, usage or tool results, so the
// parser works from hook events only. Every event it uses is appended, in a
// compact form, to an event log kept in the conversation's state blob. `stop`
// closes the generation it names; `sessionEnd` flushes whatever is left.
//
// A subagent's events carry its own conversation id plus parent_agent_id (the
// parent conversation) and, on tool events, parent_tool_call_id (the Task
// tool call that started it). The core keys the state by parent_agent_id, so
// those events land in the parent's log and are nested under the Task step
// when the parent generation closes.
package cursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

// New returns the Cursor parser.
func New() model.Parser { return &Parser{} }

// Parser implements model.Parser for Cursor.
type Parser struct{}

const (
	stateVersion = 1
	// maxEvents and maxAge bound the log when a generation never gets its
	// stop (Cursor killed, hook installed mid-turn...).
	maxEvents = 2000
	maxAge    = 24 * time.Hour
	// maxSent is how many delivered generations are remembered to drop
	// their late events.
	maxSent = 64
	// maxField is one rune more than the core's per-attribute limit
	// (otlp.MaxAttrChars, 20000): longer text is cut here, and the core
	// still sees it as too long and marks the span truncated.
	maxField = 20001
	// maxToolContent bounds the tool arguments and results kept in the log
	// (the blob is read and rewritten on every hook call); past it new tool
	// events are kept without them.
	maxToolContent = 4 << 20
	// dupWindow: Cursor sends some events twice (afterAgentThought pairs a
	// few hundred ms apart).
	dupWindow = time.Second
)

// Event names.
const (
	evPrompt      = "beforeSubmitPrompt"
	evResponse    = "afterAgentResponse"
	evThought     = "afterAgentThought"
	evTool        = "postToolUse"
	evToolFailure = "postToolUseFailure"
	evSubStart    = "subagentStart"
	evSubStop     = "subagentStop"
	evStop        = "stop"
	evSessionEnd  = "sessionEnd"
)

// payload holds the fields of Cursor hook events the parser reads.
type payload struct {
	HookEventName       string          `json:"hook_event_name"`
	ConversationID      string          `json:"conversation_id"`
	GenerationID        string          `json:"generation_id"`
	Model               string          `json:"model"`
	ModelID             string          `json:"model_id"`
	CursorVersion       string          `json:"cursor_version"`
	WorkspaceRoots      []string        `json:"workspace_roots"`
	UserEmail           string          `json:"user_email"`
	ParentAgentID       string          `json:"parent_agent_id"`
	ParentToolCallID    string          `json:"parent_tool_call_id"`
	Prompt              string          `json:"prompt"`
	Text                string          `json:"text"`
	ToolName            string          `json:"tool_name"`
	ToolInput           json.RawMessage `json:"tool_input"`
	ToolOutput          json.RawMessage `json:"tool_output"`
	ToolUseID           string          `json:"tool_use_id"`
	Duration            float64         `json:"duration"`    // ms, fractional (tool events)
	DurationMS          float64         `json:"duration_ms"` // ms (thoughts, subagentStop)
	ErrorMessage        string          `json:"error_message"`
	FailureType         string          `json:"failure_type"`
	IsInterrupt         bool            `json:"is_interrupt"`
	SubagentID          string          `json:"subagent_id"`
	SubagentType        string          `json:"subagent_type"`
	ChildConversationID string          `json:"child_conversation_id"`
	Task                string          `json:"task"`
	Status              string          `json:"status"`
	InputTokens         float64         `json:"input_tokens"`
	OutputTokens        float64         `json:"output_tokens"`
	CacheReadTokens     float64         `json:"cache_read_tokens"`
	CacheWriteTokens    float64         `json:"cache_write_tokens"`
	EstimatedCost       *struct {
		RawCostCents float64 `json:"raw_cost_cents"`
	} `json:"estimated_cost"`
}

// state is the opaque blob the core stores per (parent) conversation.
type state struct {
	V      int     `json:"v"`
	Events []event `json:"events,omitempty"` // ordered by At
	Sent   []sent  `json:"sent,omitempty"`   // oldest first
}

// sent is a delivered generation. Events for it that arrive late (another
// hook process lost the race for the lock to stop) are dropped instead of
// becoming a second, partial turn with the same trace id.
type sent struct {
	Gen   string   `json:"gen"`
	Child []string `json:"child,omitempty"` // subagent generations nested in it
	// Calls are its Task / subagent call ids. A background subagent's
	// events arrive after the turn was sent and point at one of them.
	Calls []string  `json:"calls,omitempty"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// event is one hook event, reduced to what building a turn needs.
type event struct {
	At     time.Time `json:"at"` // received time (HookEvent.Now)
	Name   string    `json:"e"`
	Conv   string    `json:"conv,omitempty"`
	Gen    string    `json:"gen,omitempty"`
	Child  bool      `json:"child,omitempty"` // a subagent's event
	Parent string    `json:"pcall,omitempty"` // parent_tool_call_id
	Model  string    `json:"model,omitempty"` // model_id, else model
	ID     string    `json:"id,omitempty"`    // tool_use_id / subagent_id
	Tool   string    `json:"tool,omitempty"`
	Dur    float64   `json:"dur,omitempty"`    // milliseconds
	Args   string    `json:"args,omitempty"`   // tool_input as JSON
	Result string    `json:"result,omitempty"` // tool_output; error_message for a failure
	Text   string    `json:"text,omitempty"`   // prompt, response text, subagent task
	Fail   string    `json:"fail,omitempty"`   // failure_type
	Intr   bool      `json:"intr,omitempty"`   // is_interrupt
	Status string    `json:"status,omitempty"`
	Type   string    `json:"stype,omitempty"` // subagent_type
	Sub    string    `json:"sub,omitempty"`   // child_conversation_id
	Usage  *usage    `json:"usage,omitempty"`

	User    string `json:"user,omitempty"`
	Version string `json:"ver,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
}

// usage is the generation's token totals (afterAgentResponse, stop).
type usage struct {
	Input      int64   `json:"in,omitempty"`
	Output     int64   `json:"out,omitempty"`
	CacheRead  int64   `json:"cr,omitempty"`
	CacheWrite int64   `json:"cw,omitempty"`
	CostCents  float64 `json:"cents,omitempty"`
}

func (u *usage) empty() bool {
	return u == nil || (u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 && u.CostCents == 0)
}

func decodeState(b []byte) *state {
	st := &state{}
	if len(b) > 0 && json.Unmarshal(b, st) == nil && st.V == stateVersion {
		return st
	}
	return &state{V: stateVersion}
}

func encodeState(st *state) []byte {
	if len(st.Events) == 0 && len(st.Sent) == 0 {
		return []byte(`{"v":1}`)
	}
	b, _ := json.Marshal(st)
	return b
}

// Collect implements model.Parser.
func (p *Parser) Collect(ev model.HookEvent, prev []byte) (model.CollectResult, error) {
	keep := model.CollectResult{StateOnSuccess: prev, StateOnFailure: prev}
	var pl payload
	if err := json.Unmarshal(ev.Raw, &pl); err != nil {
		// A field of an unexpected type is skipped; anything else is fatal.
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return keep, fmt.Errorf("cursor: payload: %w", err)
		}
	}
	name := ev.Name
	if name == "" {
		name = pl.HookEventName
	}
	now := ev.Now
	if now.IsZero() {
		now = time.Now()
	}
	st := decodeState(prev)
	st.prune(now)

	switch {
	case ev.Final || name == evSessionEnd:
		return p.close(st, ev, pl, true), nil
	case name == evStop:
		if pl.GenerationID == "" {
			log.Printf("cursor: stop without generation_id ignored")
			return keep, nil
		}
		if st.sentGen(pl.GenerationID) {
			log.Printf("cursor: stop for a generation already sent ignored")
			return keep, nil
		}
		st.add(newEvent(evStop, pl, now))
		return p.close(st, ev, pl, false), nil
	}

	e, ok := newEvent(name, pl, now), usedEvent(name)
	if !ok || (!e.Child && e.Gen == "") {
		return keep, nil
	}
	if !st.add(e) {
		return keep, nil
	}
	b := encodeState(st)
	return model.CollectResult{StateOnSuccess: b, StateOnFailure: b}, nil
}

// usedEvent reports whether a non-terminal event goes into the log.
// afterAgentThought is not installed; when seen it is kept (deduplicated)
// only so that its generation is cleaned up with the turn. Other events
// (sessionStart, afterShellExecution, preToolUse...) are ignored.
func usedEvent(name string) bool {
	switch name {
	case evPrompt, evResponse, evThought, evTool, evToolFailure, evSubStart, evSubStop:
		return true
	}
	return false
}

// thoughtSuffix is the "-<n>-<4 chars>" Cursor appends to the generation id
// of one copy of each duplicated afterAgentThought.
var thoughtSuffix = regexp.MustCompile(`-\d+-[0-9a-z]{4}$`)

func newEvent(name string, pl payload, now time.Time) event {
	e := event{
		At:     now,
		Name:   name,
		Conv:   pl.ConversationID,
		Gen:    pl.GenerationID,
		Child:  pl.ParentAgentID != "" && pl.ConversationID != "" && pl.ParentAgentID != pl.ConversationID,
		Parent: pl.ParentToolCallID,
		Model:  pl.ModelID,
	}
	if e.Model == "" {
		e.Model = pl.Model
	}
	switch name {
	case evPrompt:
		e.Text = clip(pl.Prompt)
		e.setContext(pl)
	case evResponse:
		e.Text = clip(pl.Text)
		e.Usage = usageOf(pl)
		e.setContext(pl)
	case evStop:
		e.Status = pl.Status
		e.Usage = usageOf(pl)
		e.setContext(pl)
	case evThought:
		if g := thoughtSuffix.ReplaceAllString(e.Gen, ""); g != "" {
			e.Gen = g
		}
		e.Dur = pl.DurationMS
	case evTool, evToolFailure:
		e.ID = pl.ToolUseID
		e.Tool = pl.ToolName
		e.Dur = pl.Duration
		e.Args = clip(rawText(pl.ToolInput))
		e.Result = clip(rawText(pl.ToolOutput))
		if name == evToolFailure {
			e.Result = clip(pl.ErrorMessage)
			e.Fail = pl.FailureType
			e.Intr = pl.IsInterrupt
		}
	case evSubStart, evSubStop:
		e.ID = pl.SubagentID
		e.Type = pl.SubagentType
		e.Status = pl.Status
		e.Dur = pl.DurationMS
		e.Sub = pl.ChildConversationID
		e.Text = clip(pl.Task)
	}
	return e
}

func (e *event) setContext(pl payload) {
	e.User = pl.UserEmail
	e.Version = pl.CursorVersion
	if len(pl.WorkspaceRoots) > 0 {
		e.Cwd = pl.WorkspaceRoots[0]
	}
}

func usageOf(pl payload) *usage {
	u := &usage{
		Input:      int64(pl.InputTokens),
		Output:     int64(pl.OutputTokens),
		CacheRead:  int64(pl.CacheReadTokens),
		CacheWrite: int64(pl.CacheWriteTokens),
	}
	if pl.EstimatedCost != nil {
		u.CostCents = pl.EstimatedCost.RawCostCents
	}
	if u.empty() {
		return nil
	}
	return u
}

// rawText returns a JSON string's value, or other JSON as compact text.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func clip(s string) string {
	if len(s) <= maxField || utf8.RuneCountInString(s) <= maxField {
		return s
	}
	n := 0
	for i := range s {
		if n == maxField {
			return s[:i]
		}
		n++
	}
	return s
}

// prune drops events and sent records older than maxAge and caps the log.
func (st *state) prune(now time.Time) {
	cut := now.Add(-maxAge)
	i := sort.Search(len(st.Events), func(i int) bool { return !st.Events[i].At.Before(cut) })
	if extra := len(st.Events) - i - maxEvents; extra > 0 {
		i += extra
	}
	if i > 0 {
		log.Printf("cursor: dropped %d old events", i)
		st.Events = append([]event(nil), st.Events[i:]...)
	}
	j := 0
	for j < len(st.Sent) && st.Sent[j].End.Before(cut) {
		j++
	}
	if j > 0 {
		st.Sent = append([]sent(nil), st.Sent[j:]...)
	}
}

func (st *state) sentGen(gen string) bool {
	if gen == "" {
		return false
	}
	for _, s := range st.Sent {
		if s.Gen == gen {
			return true
		}
		for _, c := range s.Child {
			if c == gen {
				return true
			}
		}
	}
	return false
}

// sentCall returns the sent turn whose Task / subagent call id is id.
func (st *state) sentCall(id string) *sent {
	if id == "" {
		return nil
	}
	for i := range st.Sent {
		for _, c := range st.Sent[i].Calls {
			if c == id {
				return &st.Sent[i]
			}
		}
	}
	return nil
}

// add inserts e in time order unless it duplicates a logged event or
// belongs to a turn already sent. It reports whether the state changed.
func (st *state) add(e event) bool {
	if st.sentGen(e.Gen) {
		log.Printf("cursor: late %s for a sent generation dropped", e.Name)
		return false
	}
	// A subagent still running after its parent turn was sent (background
	// Task): its events cannot be added to the delivered trace, and must
	// not be attributed to the next turn either. Remember its generation so
	// its events without parent_tool_call_id are dropped too.
	if s := st.sentCall(e.Parent); s != nil && e.Child {
		log.Printf("cursor: %s of a subagent whose turn was sent dropped", e.Name)
		if e.Gen != "" && !st.sentGen(e.Gen) {
			s.Child = append(s.Child, e.Gen)
			return true
		}
		return false
	}
	if (e.Name == evSubStart || e.Name == evSubStop) && st.sentCall(e.ID) != nil {
		log.Printf("cursor: late %s for a sent turn dropped", e.Name)
		return false
	}
	for i := len(st.Events) - 1; i >= 0; i-- {
		if duplicate(st.Events[i], e) {
			return false
		}
	}
	if e.Args != "" || e.Result != "" {
		total := 0
		for _, o := range st.Events {
			total += len(o.Args) + len(o.Result)
		}
		if total+len(e.Args)+len(e.Result) > maxToolContent {
			log.Printf("cursor: tool content budget reached, %s kept without arguments and result", e.Name)
			e.Args, e.Result = "", ""
		}
	}
	i := sort.Search(len(st.Events), func(i int) bool { return st.Events[i].At.After(e.At) })
	st.Events = append(st.Events, event{})
	copy(st.Events[i+1:], st.Events[i:])
	st.Events[i] = e
	if n := len(st.Events) - maxEvents; n > 0 {
		log.Printf("cursor: event log full, dropped %d oldest events", n)
		st.Events = append([]event(nil), st.Events[n:]...)
	}
	return true
}

// duplicate: one prompt and one stop per generation; one event per tool
// call or subagent id; anything else identical within dupWindow.
func duplicate(a, b event) bool {
	if a.Name != b.Name || a.Gen != b.Gen || a.Conv != b.Conv {
		return false
	}
	switch b.Name {
	case evPrompt, evStop:
		return true
	case evTool, evToolFailure, evSubStart, evSubStop:
		if b.ID != "" {
			return a.ID == b.ID
		}
	}
	d := a.At.Sub(b.At)
	if d < 0 {
		d = -d
	}
	return d <= dupWindow && a.ID == b.ID && a.Dur == b.Dur && a.Text == b.Text && a.Status == b.Status
}

func (st *state) markSent(s sent) {
	st.Sent = append(st.Sent, s)
	if n := len(st.Sent) - maxSent; n > 0 {
		st.Sent = append([]sent(nil), st.Sent[n:]...)
	}
}

// close emits the generations that are done: those with a stop record, or
// on sessionEnd (final) every generation left, stop-less ones as aborted.
// StateOnFailure keeps the log including the new stop, so the next stop or
// sessionEnd emits the same turns again.
func (p *Parser) close(st *state, ev model.HookEvent, pl payload, final bool) model.CollectResult {
	failure := encodeState(st)
	own := st.owners()
	var gens []string
	seen := map[string]bool{}
	for i := range st.Events {
		g := own[i]
		if g == "" || g == dropped || seen[g] {
			continue
		}
		if final || st.hasStop(g, own) {
			seen[g] = true
			gens = append(gens, g)
		}
	}
	fallback := turnContext{Session: ev.SessionID, Cwd: ev.Cwd, User: pl.UserEmail, Version: pl.CursorVersion}
	if fallback.Cwd == "" && len(pl.WorkspaceRoots) > 0 {
		fallback.Cwd = pl.WorkspaceRoots[0]
	}
	var turns []model.Turn
	emitted := map[string]bool{}
	for _, g := range gens {
		var evs []event
		for i, e := range st.Events {
			if own[i] == g {
				evs = append(evs, e)
			}
		}
		b := buildTurn(g, evs, fallback)
		turns = append(turns, b.turn)
		emitted[g] = true
		st.markSent(sent{Gen: g, Child: b.childGens, Calls: b.calls, Start: b.turn.Start, End: b.turn.End})
	}
	if final {
		return model.CollectResult{Turns: turns, StateOnSuccess: nil, StateOnFailure: failure}
	}
	rest := st.Events[:0:0]
	for i, e := range st.Events {
		if !emitted[own[i]] && own[i] != dropped {
			rest = append(rest, e)
		}
	}
	st.Events = rest
	return model.CollectResult{Turns: turns, StateOnSuccess: encodeState(st), StateOnFailure: failure}
}

func (st *state) hasStop(gen string, own []string) bool {
	for i, e := range st.Events {
		if own[i] == gen && e.Name == evStop && !e.Child {
			return true
		}
	}
	return false
}

// dropped marks an event that belongs to a generation already sent.
const dropped = "\x00sent"

// owners returns, for each logged event, the parent generation it belongs
// to: its own generation for the parent conversation's events; for a
// subagent's events the generation whose tool call (parent_tool_call_id,
// subagentStop.subagent_id) or child conversation it belongs to, learned
// from sibling events; failing that, the generation running when it
// arrived. "" means not known yet, dropped a generation already sent.
func (st *state) owners() []string {
	own := make([]string, len(st.Events))
	byCall := map[string]string{}
	byConv := map[string]string{}
	byGen := map[string]string{}
	for i, e := range st.Events {
		if e.Child || e.Gen == "" {
			continue
		}
		own[i] = e.Gen
		switch e.Name {
		case evTool, evToolFailure, evSubStart, evSubStop:
			if e.ID != "" {
				byCall[e.ID] = e.Gen
			}
			if e.Sub != "" {
				byConv[e.Sub] = e.Gen
			}
		}
	}
	// Subagent events of a turn already sent (see add): dropped.
	for i, e := range st.Events {
		if e.Child && (st.sentCall(e.Parent) != nil || st.sentGen(e.Gen)) {
			own[i] = dropped
		}
	}
	link := func(i int, g string) {
		own[i] = g
		e := st.Events[i]
		if e.Conv != "" && byConv[e.Conv] == "" {
			byConv[e.Conv] = g
		}
		if e.Gen != "" && byGen[e.Gen] == "" {
			byGen[e.Gen] = g
		}
	}
	for i, e := range st.Events {
		if e.Child && e.Parent != "" && byCall[e.Parent] != "" {
			link(i, byCall[e.Parent])
		}
	}
	for i, e := range st.Events {
		if !e.Child || own[i] != "" {
			continue
		}
		if g := byConv[e.Conv]; g != "" {
			link(i, g)
		} else if g := byGen[e.Gen]; g != "" {
			link(i, g)
		}
	}
	// Still unlinked (the Task call and subagentStop never came, e.g. the
	// turn was interrupted): attribute by time to the generation that was
	// running. A conversation runs one generation at a time.
	type window struct {
		gen        string
		start, end time.Time // end zero: still open
	}
	var wins []window
	idx := map[string]int{}
	for i, e := range st.Events {
		if e.Child || own[i] == "" {
			continue
		}
		k, ok := idx[e.Gen]
		if !ok {
			k = len(wins)
			idx[e.Gen] = k
			wins = append(wins, window{gen: e.Gen, start: e.At})
		}
		if e.Name == evStop {
			wins[k].end = e.At
		}
	}
	for _, s := range st.Sent {
		wins = append(wins, window{gen: dropped, start: s.Start, end: s.End})
	}
	for i, e := range st.Events {
		if !e.Child || own[i] != "" {
			continue
		}
		best := -1
		for k, w := range wins {
			if e.At.Before(w.start) || (!w.end.IsZero() && e.At.After(w.end)) {
				continue
			}
			if best < 0 || w.start.After(wins[best].start) {
				best = k
			}
		}
		if best >= 0 {
			own[i] = wins[best].gen
		}
	}
	return own
}

// turnContext is what a turn falls back to when its own events lack it.
type turnContext struct {
	Session, Cwd, User, Version string
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
