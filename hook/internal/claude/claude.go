// Package claude parses Claude Code transcripts into model.Turns
// (CONTRACT.md §5.1). It only reads files; state goes back to the core
// through model.CollectResult.
package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/model"
)

const (
	stateVersion = 1
	// maxTurnsPerCollect bounds the work of one event when many turns piled
	// up; the rest follows on later events. Final (SessionEnd) ignores it.
	maxTurnsPerCollect = 50
)

// Parser implements model.Parser for Claude Code.
type Parser struct {
	maxTurns int
	backfill bool
}

// New returns a Claude Code parser.
//
// On the first Collect of a session (prev == nil, e.g. the hook was installed
// in the middle of a long session) it emits only the most recent completed
// turn and treats every earlier turn as already sent, so one Stop never tries
// to deliver hundreds of turns within the 3 s budget.
func New() model.Parser { return &Parser{maxTurns: maxTurnsPerCollect} }

// NewBackfill returns a parser that emits every turn of the transcript on
// the first Collect too. Used by the `export` developer command.
func NewBackfill() model.Parser { return &Parser{maxTurns: maxTurnsPerCollect, backfill: true} }

// state is the parser's opaque per-session state.
type state struct {
	V    int    `json:"v"`
	Path string `json:"path,omitempty"`
	// Offset is where the next read starts: the first line of the earliest
	// turn that may still change (an open, deferred or just-emitted turn), or
	// the end of the consumed data when there is none. Always at a line start.
	Offset int64 `json:"offset"`
	// Emitted maps turn keys emitted at or after Offset to the end offset of
	// their last user/assistant record at emission time, so an unchanged turn
	// is not sent again and a grown one is re-sent with the same IDs.
	Emitted map[string]int64 `json:"emitted,omitempty"`
}

type payload struct {
	StopHookActive bool `json:"stop_hook_active"`
}

// Collect implements model.Parser.
func (p *Parser) Collect(ev model.HookEvent, prev []byte) (model.CollectResult, error) {
	keep := model.CollectResult{StateOnSuccess: prev}
	final := ev.Final || ev.Name == "SessionEnd"
	switch ev.Name {
	case "Stop":
		var pl payload
		if len(ev.Raw) > 0 {
			_ = json.Unmarshal(ev.Raw, &pl)
		}
		if pl.StopHookActive {
			return keep, nil
		}
	case "SessionEnd":
	default:
		if !final {
			return keep, nil
		}
	}
	if ev.TranscriptPath == "" {
		return keep, errors.New("claude: payload has no transcript_path")
	}

	st := state{V: stateVersion}
	first := len(prev) == 0
	if !first {
		if err := json.Unmarshal(prev, &st); err != nil || st.V != stateVersion {
			st = state{V: stateVersion}
			first = true
		}
	}
	if st.Path != "" && st.Path != ev.TranscriptPath {
		st = state{V: stateVersion}
	}
	st.Path = ev.TranscriptPath

	data, base, err := readFrom(ev.TranscriptPath, st.Offset)
	if err != nil {
		return keep, err
	}
	if base == 0 && st.Offset != 0 {
		// The file shrank: treat it as rotated and start over.
		st.Offset, st.Emitted = 0, nil
	}
	records, consumed := parseLines(data, base)

	turns := p.group(records, ev, final)
	sessionDir := subagentDir(ev.TranscriptPath, ev.SessionID)

	var out []model.Turn
	next := state{V: stateVersion, Path: st.Path, Offset: base + consumed, Emitted: map[string]int64{}}
	limit := p.maxTurns
	if final || limit <= 0 {
		limit = int(^uint(0) >> 1)
	}
	var firstEmittedStart int64
	skipBefore := 0
	if first && !p.backfill {
		skipBefore = latestCompleted(turns)
	}
	for i, t := range turns {
		last := i == len(turns)-1
		if !t.complete {
			// Open or deferred: re-read from its first line next time.
			next.Offset = t.startOffset
			break
		}
		if len(out) >= limit {
			next.Offset = t.startOffset
			break
		}
		prevEnd, emitted := st.Emitted[t.key]
		changed := !emitted || t.run.lastRelevant > prevEnd
		if i < skipBefore {
			changed = false // first sight of the session: history counts as sent
		}
		if changed && len(t.run.steps) > 0 {
			if len(out) == 0 {
				firstEmittedStart = t.startOffset
			}
			out = append(out, buildTurn(t, ev, sessionDir))
		}
		if last && !final {
			// The last turn can still grow (notification rounds after a Stop):
			// keep it in the window and remember what was sent.
			next.Offset = t.startOffset
			next.Emitted[t.key] = t.run.lastRelevant
		}
	}
	if len(next.Emitted) == 0 {
		next.Emitted = nil
	}
	b, _ := json.Marshal(next)
	res := model.CollectResult{Turns: out, StateOnSuccess: b}
	if first && !p.backfill && len(out) > 0 {
		// A failed first delivery must not let the next call start over as a
		// "first" Collect (which would skip this turn as history): retry from
		// the emitted turn, still skipping everything before it.
		fb, _ := json.Marshal(state{V: stateVersion, Path: st.Path, Offset: firstEmittedStart})
		res.StateOnFailure = fb
	}
	return res, nil
}

// latestCompleted is the index of the most recent complete turn that has
// steps; turns before it are skipped on a session's first Collect. It returns
// len(turns) when no turn is complete yet, so nothing earlier is sent either.
func latestCompleted(turns []*pendingTurn) int {
	idx := len(turns)
	for i, t := range turns {
		if !t.complete {
			break
		}
		if len(t.run.steps) > 0 {
			idx = i
		}
	}
	return idx
}

// readFrom returns the bytes from offset to EOF. When the file is shorter
// than offset it reads from 0 and returns base 0.
func readFrom(path string, offset int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if fi.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(f)
	return data, offset, err
}

// parseLines decodes complete lines; a trailing line without '\n' is left
// for the next call. consumed is the number of bytes of complete lines.
func parseLines(data []byte, base int64) ([]*record, int64) {
	var out []*record
	unknown := map[string]int{}
	malformed := 0
	pos := 0
	for {
		nl := bytes.IndexByte(data[pos:], '\n')
		if nl < 0 {
			break
		}
		line := bytes.TrimSpace(data[pos : pos+nl])
		start := int64(pos)
		pos += nl + 1
		if len(line) == 0 {
			continue
		}
		r, ok := decodeRecord(line)
		if !ok {
			malformed++
			continue
		}
		if !knownTypes[r.Type] {
			unknown[r.Type]++
			continue
		}
		r.start, r.end = base+start, base+int64(pos)
		out = append(out, r)
	}
	if len(unknown) > 0 || malformed > 0 {
		keys := make([]string, 0, len(unknown))
		for k := range unknown {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, unknown[k]))
		}
		log.Printf("claude: skipped unknown record types [%s], malformed lines %d", strings.Join(parts, " "), malformed)
	}
	return out, int64(pos)
}

// pendingTurn is a turn being grouped.
type pendingTurn struct {
	key         string
	startOffset int64
	run         *run
	complete    bool
}

// isTurnStart reports whether a record is a real user prompt.
//
// CONTRACT §5.1 names origin.kind=="human" and promptSource=="typed".
// promptSource=="sdk" (headless `claude -p` and the Agent SDK, which carry no
// origin) is accepted too so CI runs produce turns.
func isTurnStart(r *record) bool {
	if r.Type != "user" || r.IsMeta || r.IsCompactSummary || r.IsSidechain {
		return false
	}
	if r.msg == nil || r.isToolResultOnly() {
		return false
	}
	return r.originKind() == "human" || r.PromptSource == "typed" || r.PromptSource == "sdk"
}

func turnKey(r *record) string {
	if r.PromptID != "" {
		return r.PromptID
	}
	return r.UUID
}

// group splits records into turns and decides which are complete.
func (p *Parser) group(records []*record, ev model.HookEvent, final bool) []*pendingTurn {
	var turns []*pendingTurn
	var cur *pendingTurn
	for _, r := range records {
		if r.IsSidechain {
			continue // old transcripts inlined subagent records
		}
		if isTurnStart(r) {
			if cur != nil && turnKey(r) == cur.key {
				cur.run.add(r) // same prompt id: still the same turn
				continue
			}
			if cur != nil {
				// The next prompt ends the previous turn, whatever is pending.
				cur.complete = true
				turns = append(turns, cur)
			}
			cur = &pendingTurn{key: turnKey(r), startOffset: r.start, run: newRun()}
			cur.run.add(r)
			continue
		}
		if cur != nil {
			cur.run.add(r)
		}
	}
	if cur != nil {
		cur.complete = final || lastTurnDone(cur.run, ev)
		turns = append(turns, cur)
	}
	return turns
}

// lastTurnDone decides whether the newest turn has ended.
//
// CONTRACT §5.1: end_turn followed by system/turn_duration. Claude Code runs
// Stop hooks before writing turn_duration, so a Stop whose transcript ends in
// an end_turn (or a synthetic API error) is taken as the end of the turn too.
// Background agents still running defer the turn.
func lastTurnDone(u *run, ev model.HookEvent) bool {
	ended := false
	switch {
	case u.lastStop == "end_turn" && u.turnDurationPending != nil:
		ended = true
	case ev.Name == "Stop" && (u.lastStop == "end_turn" || u.apiError != ""):
		ended = true
	}
	if !ended {
		return false
	}
	return u.pendingBackgroundAgents() == 0
}

// buildTurn converts a grouped turn into a model.Turn, attaching subagents.
func buildTurn(t *pendingTurn, ev model.HookEvent, dir string) model.Turn {
	u := t.run
	sid := ev.SessionID
	if sid == "" {
		sid = u.session
	}
	if dir == "" && sid != "" {
		dir = subagentDir(ev.TranscriptPath, sid)
	}
	attachSubagents(u.steps, loadSubagentIndex(dir), dir, 0)
	status, msg := u.status()
	if status == model.StatusOK && u.lastStop != "end_turn" {
		// Closed by the next prompt or by SessionEnd without finishing.
		status, msg = model.StatusAborted, "aborted"
	}
	return model.Turn{
		Tool:          model.ToolClaude,
		SessionID:     sid,
		TurnKey:       t.key,
		ToolVersion:   u.version,
		Cwd:           u.cwd,
		GitBranch:     u.branch,
		Start:         u.start,
		End:           u.end,
		Input:         u.input,
		Output:        u.output,
		Status:        status,
		StatusMessage: msg,
		Steps:         u.steps,
	}
}

// subagentDir is <transcript dir>/<sessionId>/subagents.
func subagentDir(transcript, sessionID string) string {
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(transcript), filepath.Ext(transcript))
	}
	if sessionID == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(transcript), sessionID, "subagents")
}
