// Package codex parses Codex rollout files into model.Turns (CONTRACT §5.2).
//
// Codex writes one rollout per thread under ~/.codex/sessions/YYYY/MM/DD/
// rollout-<time>-<thread id>.jsonl. The Stop hook passes that path as
// transcript_path. The parser reads it incrementally, emits every turn that
// has finished and was not sent before, and nests subagent threads under the
// tool call that spawned them. The first Collect for a rollout exports only
// the turn the event names (or the last one it closes) and marks older turns
// as sent, so installing the hook mid-session does not replay the backlog.
package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/model"
)

// New returns the Codex parser.
func New() model.Parser { return &Parser{} }

// Parser implements model.Parser for Codex.
type Parser struct {
	// emitSubagentThreads makes Collect emit turns of spawned subagent
	// rollouts as standalone turns. Off in production: those turns are
	// nested under the parent's spawn tool call instead (tests only).
	emitSubagentThreads bool
}

type hookPayload struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TurnID         string `json:"turn_id"`
	TranscriptPath string `json:"transcript_path"`
}

const (
	stateVersion = 1
	maxSent      = 4096
)

// state is the opaque blob the core stores per session. It is keyed by
// rollout path so that sharing one blob between threads cannot mix offsets.
type state struct {
	V     int                   `json:"v"`
	Files map[string]*fileState `json:"files,omitempty"`
}

type fileState struct {
	// Resume is where the next read starts: the task_started line of the
	// oldest turn still running, or the end of the last complete line.
	Resume int64 `json:"resume"`
	// Scanned is the end of the last complete line read so far; a file
	// shorter than this was truncated or replaced.
	Scanned int64        `json:"scanned"`
	Ctx     walkCtx      `json:"ctx"`
	Meta    *sessionMeta `json:"meta,omitempty"`
	// Sent are turn ids already returned, oldest first.
	Sent []string `json:"sent,omitempty"`
	// Unknown counts record types the parser skipped ("<type>",
	// "response_item:<type>", "event_msg:<type>", "item:<type>",
	// "invalid_json"), cumulative over the file.
	Unknown map[string]int `json:"unknown,omitempty"`
	// Children caches subagent thread id -> rollout path.
	Children map[string]string `json:"children,omitempty"`
}

// UnknownCounts returns the skipped-record counters kept in a Codex state
// blob, summed over its files, for logging.
func UnknownCounts(stateBlob []byte) map[string]int {
	st := decodeState(stateBlob)
	out := map[string]int{}
	for _, fs := range st.Files {
		for k, v := range fs.Unknown {
			out[k] += v
		}
	}
	return out
}

func decodeState(b []byte) *state {
	st := &state{}
	if len(b) > 0 && json.Unmarshal(b, st) == nil && st.V == stateVersion {
		if st.Files == nil {
			st.Files = map[string]*fileState{}
		}
		return st
	}
	return &state{V: stateVersion, Files: map[string]*fileState{}}
}

func encodeState(st *state) []byte {
	b, _ := json.Marshal(st)
	return b
}

// Collect implements model.Parser.
func (p *Parser) Collect(ev model.HookEvent, prev []byte) (model.CollectResult, error) {
	keep := prev
	if len(keep) == 0 {
		keep = encodeState(&state{V: stateVersion})
	}
	unchanged := model.CollectResult{StateOnSuccess: keep}

	var hp hookPayload
	if len(ev.Raw) > 0 {
		_ = json.Unmarshal(ev.Raw, &hp)
	}
	path := ev.TranscriptPath
	if path == "" {
		path = hp.TranscriptPath
	}
	if path == "" {
		return unchanged, nil
	}
	name := ev.Name
	if name == "" {
		name = hp.HookEventName
	}
	// The Stop hook runs before Codex writes task_complete, so the turn it
	// names is finished even though the rollout does not say so yet.
	stopTurn := ""
	if strings.EqualFold(name, "Stop") {
		stopTurn = hp.TurnID
	}

	f, err := os.Open(path)
	if err != nil {
		return unchanged, fmt.Errorf("codex: open rollout: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return unchanged, fmt.Errorf("codex: stat rollout: %w", err)
	}

	st := decodeState(prev)
	fs := st.Files[path]
	// First Collect for this rollout (hook installed mid-session): export
	// only the turn this event closes and mark older turns as sent, so a long
	// backlog cannot overflow one request, time out and retry forever.
	backlog := fs == nil
	if fs == nil {
		fs = &fileState{}
	} else if info.Size() < fs.Scanned {
		// Truncated or replaced: read again from the start. Keep the sent
		// ids so turns already delivered are not delivered twice.
		fs = &fileState{Sent: fs.Sent, Children: fs.Children}
	}

	meta := fs.Meta
	if meta == nil {
		meta = peekMeta(f)
	}
	// A spawned subagent's turns are nested under the parent's spawn call
	// when the parent turn is exported; emitting them here as well would
	// count their tokens twice. Leave the (possibly shared) state alone.
	if meta != nil && meta.Spawned != nil && !p.emitSubagentThreads {
		return unchanged, nil
	}

	sent := make(map[string]bool, len(fs.Sent))
	for _, id := range fs.Sent {
		sent[id] = true
	}
	res := &resolver{cache: fs.Children}
	var (
		bld     *builder
		turns   []model.Turn
		newSent []string // emitted or skipped as backlog, in file order
		latest  *turnBuf // backlog mode without turn_id: last closed turn
		first   *turnBuf // earliest emitted turn (backlog mode failure state)
	)
	w := &walker{
		meta:      fs.Meta,
		ctx:       fs.Ctx,
		countFrom: fs.Scanned,
		unknown:   copyCounts(fs.Unknown),
		keep:      func(id string, _ time.Time) bool { return !sent[id] },
	}
	emit := func(t *turnBuf) {
		if bld == nil || bld.meta != w.meta {
			bld = &builder{meta: w.meta, path: path, res: res}
		}
		turns = append(turns, bld.turn(t))
		if first == nil || t.startOff < first.startOff {
			first = t
		}
	}
	w.done = func(t *turnBuf) {
		sent[t.id] = true
		newSent = append(newSent, t.id)
		switch {
		case !backlog:
			emit(t)
		case hp.TurnID != "":
			if t.id == hp.TurnID {
				emit(t)
			}
		default:
			latest = t // only the last turn closing on this event is exported
		}
	}

	end, err := readLinesFrom(f, fs.Resume, w.feed)
	if err != nil {
		return unchanged, fmt.Errorf("codex: read rollout: %w", err)
	}
	if w.cur != nil && ((stopTurn != "" && w.cur.id == stopTurn) || ev.Final) {
		if stopTurn != "" && w.cur.id == stopTurn {
			w.finish(endStop)
		} else {
			w.finish(endAborted)
		}
	}
	if latest != nil {
		emit(latest)
	}

	next := &fileState{
		Resume:   end,
		Scanned:  max(fs.Scanned, end),
		Ctx:      w.ctx,
		Meta:     w.meta,
		Sent:     fs.Sent,
		Unknown:  w.unknown,
		Children: res.cache,
	}
	if w.cur != nil {
		next.Resume = w.cur.startOff
		next.Ctx = w.cur.startCtx
	}
	baseSent := next.Sent
	next.Sent = capSent(append(append([]string(nil), baseSent...), newSent...))
	st.Files[path] = next
	out := model.CollectResult{Turns: turns, StateOnSuccess: encodeState(st)}

	// A failed first export must not fall back to "no state": the next
	// Collect would again be a first one and the turn would never be sent.
	// The failure state keeps the backlog marked as sent but resumes at the
	// emitted turn, so a later (non-first) Collect produces it again.
	if backlog && first != nil {
		emitted := make(map[string]bool, len(turns))
		for _, t := range turns {
			emitted[t.TurnKey] = true
		}
		failSent := append([]string(nil), baseSent...)
		for _, id := range newSent {
			if !emitted[id] {
				failSent = append(failSent, id)
			}
		}
		fail := *next
		fail.Resume = first.startOff
		fail.Ctx = first.startCtx
		fail.Sent = capSent(failSent)
		fst := decodeState(prev)
		fst.Files[path] = &fail
		out.StateOnFailure = encodeState(fst)
	}
	return out, nil
}

func capSent(ids []string) []string {
	if n := len(ids); n > maxSent {
		return append([]string(nil), ids[n-maxSent:]...)
	}
	return ids
}

// peekMeta reads the first line of a rollout and returns its session_meta.
func peekMeta(f *os.File) *sessionMeta {
	if _, err := f.Seek(0, 0); err != nil {
		return nil
	}
	line, err := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	if err != nil {
		return nil
	}
	var r rawRecord
	if json.Unmarshal(line, &r) != nil || r.Type != "session_meta" {
		return nil
	}
	var p sessionMetaPayload
	if json.Unmarshal(r.Payload, &p) != nil {
		return nil
	}
	return newSessionMeta(p)
}

func copyCounts(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
