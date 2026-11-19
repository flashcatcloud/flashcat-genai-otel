package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/config"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/otlp"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/state"
)

// maxPayload bounds what is read from stdin.
const maxPayload = 32 << 20

// hookPayload holds the common fields of the three tools' hook events.
type hookPayload struct {
	Event          string   `json:"hook_event_name"`
	SessionID      string   `json:"session_id"`
	ConversationID string   `json:"conversation_id"`
	TranscriptPath string   `json:"transcript_path"`
	Cwd            string   `json:"cwd"`
	WorkspaceRoots []string `json:"workspace_roots"`
	CursorVersion  *string  `json:"cursor_version"`
	// ParentAgentID is set by Cursor on a subagent's events (and on
	// subagentStart/subagentStop): the parent conversation id.
	ParentAgentID string `json:"parent_agent_id"`
}

// cursorDraftPrefix marks the placeholder conversation Cursor reports before
// the first prompt of a new chat; nothing in it belongs to a turn.
const cursorDraftPrefix = "draft-"

// cursorConversation is the conversation a Cursor event belongs to: the
// parent's for a subagent's events, so they share the parent's state and
// spans carry the parent conversation id (CONTRACT §5.3).
func cursorConversation(pl hookPayload) string {
	switch {
	case pl.ParentAgentID != "":
		return pl.ParentAgentID
	case pl.SessionID != "":
		return pl.SessionID
	}
	return pl.ConversationID
}

// cursorReply is what Cursor must read on stdout before anything else
// (CONTRACT §3.3); invalid output would block the action.
func cursorReply(event string) string {
	switch event {
	case "beforeSubmitPrompt":
		return `{"continue":true}`
	case "subagentStart":
		return `{"permission":"allow"}`
	}
	return `{}`
}

// cmdHook handles `hook <tool>`. It always returns 0: errors only go to the
// log, so a broken hook never blocks the coding tool.
func cmdHook(args []string, stdin io.Reader, stdout io.Writer) (code int) {
	tool := ""
	if len(args) > 0 {
		tool = args[0]
	}
	var (
		pl      hookPayload
		lg      *hookLog
		replied bool
		turns   int
		note    string
		err     error
	)
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
		if tool == "cursor" && !replied {
			fmt.Fprint(stdout, "{}")
		}
		if lg == nil {
			lg = newHookLog()
		}
		lg.summary(tool, pl.Event, turns, note, err)
		code = 0
	}()

	raw, _ := io.ReadAll(io.LimitReader(stdin, maxPayload))
	plErr := json.Unmarshal(raw, &pl)
	if tool == "cursor" {
		// Reply first, before config or anything else (CONTRACT §3.3).
		fmt.Fprint(stdout, cursorReply(pl.Event))
		if f, ok := stdout.(*os.File); ok {
			_ = f.Sync()
		}
		replied = true
	}
	lg = newHookLog()

	if plErr != nil {
		err = fmt.Errorf("invalid payload: %w", plErr)
		return 0
	}
	newParser, ok := parsers[tool]
	if !ok {
		err = fmt.Errorf("unsupported tool %q", tool)
		return 0
	}
	if tool != "cursor" && pl.CursorVersion != nil {
		note = "cursor payload ignored"
		return 0
	}
	if tool == "cursor" && (strings.HasPrefix(cursorConversation(pl), cursorDraftPrefix) ||
		strings.HasPrefix(pl.ConversationID, cursorDraftPrefix)) {
		note = "draft conversation ignored"
		return 0
	}
	cfg, cerr := config.Load()
	if cerr != nil {
		err = cerr
		return 0
	}
	if !cfg.Enabled {
		note = "disabled"
		return 0
	}
	if !cfg.Configured() {
		err = errors.New("not configured: run flashcat-hook install")
		return 0
	}
	out := handleEvent(context.Background(), tool, newParser(), pl, raw, cfg, eventOpts{})
	turns, err = out.turns, out.err
	if out.sendErr == nil {
		// Delivery works (or there was nothing to send): replay events that
		// failed earlier, in any session.
		note = retryPending(cfg, state.Store{Dir: config.StateDir()}, time.Now())
	}
	return 0
}

// eventOpts distinguishes a live hook event from a replayed pending one.
type eventOpts struct {
	now         time.Time      // receive time; zero = now
	lockTimeout time.Duration  // zero = state.DefaultLockTimeout
	replay      *state.Pending // set when replaying a pending entry
}

// outcome is what handling one event did.
type outcome struct {
	turns    int
	sendErr  error // delivery was attempted and failed
	lockBusy bool  // another process holds the session; nothing was done
	err      error // anything that went wrong, for the log
}

func handleEvent(ctx context.Context, tool string, p model.Parser, pl hookPayload, raw []byte, cfg config.Config, o eventOpts) outcome {
	now := o.now
	if now.IsZero() {
		now = time.Now()
	}
	ev := model.HookEvent{
		Tool:           toolOf(tool),
		Name:           pl.Event,
		SessionID:      pl.SessionID,
		TranscriptPath: expandHome(pl.TranscriptPath),
		Cwd:            pl.Cwd,
		Raw:            raw,
		Now:            now,
	}
	if ev.SessionID == "" {
		ev.SessionID = pl.ConversationID
	}
	if tool == "cursor" {
		ev.SessionID = cursorConversation(pl)
	}
	if ev.Cwd == "" && len(pl.WorkspaceRoots) > 0 {
		ev.Cwd = pl.WorkspaceRoots[0]
	}
	ev.Final = (tool == "claude" && pl.Event == "SessionEnd") || (tool == "cursor" && pl.Event == "sessionEnd")

	key := ev.SessionID
	if key == "" {
		key = ev.TranscriptPath
	}
	if key == "" {
		return outcome{err: errors.New("payload has neither session id nor transcript_path")}
	}
	store := state.Store{Dir: config.StateDir(), LockTimeout: o.lockTimeout}
	sess, err := store.Acquire(tool, key)
	if err != nil {
		return outcome{lockBusy: true, err: err}
	}
	defer sess.Close()

	res, err := p.Collect(ev, sess.Prev())
	if err != nil {
		err = fmt.Errorf("collect: %w", err)
		if o.replay != nil {
			bumpPending(store, *o.replay)
		}
		return outcome{err: err}
	}
	var sendErr error
	if len(res.Turns) > 0 {
		id := &config.Identity{ConfigUser: cfg.User}
		resolveTurns(res.Turns, id)
		td := otlp.Build(res.Turns, resourceInfo(cfg.App), otlp.Options{
			Content:     cfg.ContentLevel(),
			TraceParent: os.Getenv("TRACEPARENT"),
		})
		sendErr = newSender(cfg).Send(ctx, td)
		label := pl.Event
		if o.replay != nil {
			label += " (retry)"
		}
		rec := exportRecord{Time: time.Now().UTC(), Tool: tool, Event: label, Turns: len(res.Turns), Spans: td.SpanCount(), OK: sendErr == nil}
		if sendErr != nil {
			rec.Error = sendErr.Error()
		}
		saveExportRecord(rec)
	}
	out := outcome{turns: len(res.Turns), sendErr: sendErr}
	if err := sess.Apply(res, sendErr); err != nil {
		out.err = fmt.Errorf("save state: %w", err)
	}

	// Pending entries change under the session lock, so a concurrent replay
	// of the same session cannot interleave.
	switch {
	case sendErr != nil && o.replay != nil:
		bumpPending(store, *o.replay)
	case sendErr != nil:
		pe := state.Pending{Tool: tool, Key: key, Received: now, Attempts: 1, Payload: raw}
		if old, _ := store.LoadPending(tool, key); old != nil {
			pe.Attempts = old.Attempts + 1
		}
		if err := store.SavePending(pe); err != nil {
			out.err = errors.Join(out.err, fmt.Errorf("save pending: %w", err))
		}
	case o.replay != nil || len(res.Turns) > 0:
		// Delivered, or a replay that has nothing left to send (a later event
		// of the session already delivered it).
		if err := store.RemovePending(tool, key); err != nil {
			out.err = errors.Join(out.err, fmt.Errorf("remove pending: %w", err))
		}
	}
	if sendErr != nil {
		out.err = errors.Join(fmt.Errorf("send: %w", sendErr), out.err)
	}
	return out
}

// expandHome turns a leading "~/" into the home directory.
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}
