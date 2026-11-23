package codex

import (
	"bufio"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/model"
)

// maxDepth bounds subagent nesting (spawned threads can spawn again).
const maxDepth = 4

// resolver finds subagent rollout files by thread id. Rollouts are named
// rollout-<local time>-<thread id>.jsonl under sessions/YYYY/MM/DD (local
// date of the spawn), so a child may sit in another day's directory.
type resolver struct {
	cache   map[string]string // thread id -> path, persisted in state
	index   map[string]string // full walk of the sessions tree, built lazily
	indexed bool
}

func (r *resolver) find(fromPath, thread string, hint time.Time) string {
	if p := r.cache[thread]; p != "" && fileExists(p) {
		return p
	}
	suffix := "-" + thread + ".jsonl"
	dir := filepath.Dir(fromPath)
	root := sessionsRoot(fromPath)
	dirs := []string{dir}
	if root != "" && !hint.IsZero() {
		for _, t := range []time.Time{hint.Local(), hint.UTC()} {
			dirs = append(dirs, filepath.Join(root, t.Format("2006"), t.Format("01"), t.Format("02")))
		}
	}
	for _, d := range dirs {
		if p := findInDir(d, suffix); p != "" {
			r.remember(thread, p)
			return p
		}
	}
	if root == "" {
		return ""
	}
	if !r.indexed {
		r.indexed = true
		r.index = map[string]string{}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if id := threadIDFromName(d.Name()); id != "" {
				if _, seen := r.index[id]; !seen {
					r.index[id] = p
				}
			}
			return nil
		})
	}
	if p := r.index[thread]; p != "" {
		r.remember(thread, p)
		return p
	}
	return ""
}

func (r *resolver) remember(thread, path string) {
	if r.cache == nil {
		r.cache = map[string]string{}
	}
	r.cache[thread] = path
}

// sessionsRoot walks up from a rollout path to the directory named
// "sessions"; "" when there is none (then only the rollout's own directory is
// searched, never the user's real ~/.codex).
func sessionsRoot(path string) string {
	dir := filepath.Dir(path)
	for {
		if filepath.Base(dir) == "sessions" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func findInDir(dir, suffix string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "rollout-") && strings.HasSuffix(e.Name(), suffix) {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// threadIDFromName extracts the trailing 36-character uuid of a rollout file.
func threadIDFromName(name string) string {
	if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
		return ""
	}
	base := strings.TrimSuffix(name, ".jsonl")
	if len(base) < len("rollout-")+37 {
		return ""
	}
	id := base[len(base)-36:]
	if base[len(base)-37] != '-' || strings.Count(id, "-") != 4 {
		return ""
	}
	return id
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// builder turns finished turnBufs into model.Turns and nests subagents.
type builder struct {
	meta    *sessionMeta
	path    string
	res     *resolver
	depth   int
	visited map[string]bool // thread ids on the current nesting path
}

func (b *builder) turn(t *turnBuf) model.Turn {
	bt := buildSteps(t, b.meta.provider())
	steps := b.attachSubagents(t, bt)
	sortSteps(steps)
	status, msg := turnStatus(t)
	cwd := t.cwd
	if cwd == "" && b.meta != nil {
		cwd = b.meta.Cwd
	}
	turn := model.Turn{
		Tool:          model.ToolCodex,
		SessionID:     b.meta.sessionID(),
		TurnKey:       t.id,
		Cwd:           cwd,
		Start:         t.start,
		End:           t.end,
		Input:         bt.input,
		Output:        bt.output,
		Status:        status,
		StatusMessage: msg,
		Steps:         steps,
	}
	if b.meta != nil {
		turn.ToolVersion = b.meta.CLIVersion
		turn.Repo = b.meta.GitRepo
		turn.GitBranch = b.meta.GitBranch
		turn.SystemInstructions = b.meta.Instructions
	}
	return turn
}

// attachSubagents parses the child rollouts this turn talked to and hangs
// each one under the tool call that spawned it (or, failing that, the call
// that sent it work, the turn's last free tool call, or a synthetic root
// spawn_agent step).
func (b *builder) attachSubagents(t *turnBuf, bt built) []model.Step {
	steps := bt.steps
	if len(bt.links) == 0 || b.res == nil || b.depth >= maxDepth || b.meta == nil {
		return steps
	}
	for _, l := range bt.links {
		if l.thread == b.meta.ThreadID || b.visited[l.thread] ||
			(b.meta.Spawned != nil && l.thread == b.meta.Spawned.ParentThreadID) {
			continue
		}
		sa := b.subagent(l, t.start, t.end)
		if sa == nil {
			continue
		}
		target := -1
		for _, id := range append([]string{l.spawnCall}, l.interactCalls...) {
			if idx, ok := bt.calls[id]; ok && id != "" && steps[idx].Tool.Subagent == nil {
				target = idx
				break
			}
		}
		if target < 0 {
			for i := len(steps) - 1; i >= 0; i-- {
				if steps[i].Kind == model.StepTool && steps[i].Tool.Subagent == nil {
					target = i
					break
				}
			}
		}
		if target >= 0 {
			steps[target].Tool.Subagent = sa
			continue
		}
		steps = append(steps, model.Step{
			Kind:  model.StepTool,
			Key:   "spawn_agent:" + l.thread,
			Start: sa.Start,
			End:   sa.End,
			Tool:  &model.ToolCall{Name: "spawn_agent", Status: sa.Status, Subagent: sa},
		})
	}
	return steps
}

// subagent parses the child thread's rollout and keeps its own turns that
// started inside the parent turn [from, to].
func (b *builder) subagent(l *subLink, from, to time.Time) *model.Subagent {
	path := b.res.find(b.path, l.thread, l.at)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var turns []*turnBuf
	w := &walker{
		countFrom: 1<<62 - 1,
		keep: func(_ string, start time.Time) bool {
			return !start.Before(from) && !start.After(to)
		},
	}
	w.done = func(t *turnBuf) { turns = append(turns, t) }
	if err := readLines(f, 0, w.feed); err != nil && len(turns) == 0 && w.cur == nil {
		return nil
	}
	if w.cur != nil {
		w.finish(endPartial) // still running when the parent turn ended
	}
	meta := w.meta
	if meta == nil || meta.ThreadID != l.thread {
		return nil
	}
	if meta.Spawned != nil && meta.Spawned.ParentThreadID != "" && meta.Spawned.ParentThreadID != b.meta.ThreadID {
		return nil
	}
	if len(turns) == 0 {
		return nil
	}

	visited := map[string]bool{b.meta.ThreadID: true}
	for k := range b.visited {
		visited[k] = true
	}
	child := &builder{meta: meta, path: path, res: b.res, depth: b.depth + 1, visited: visited}
	sa := &model.Subagent{
		Type:   meta.subagentType(),
		ID:     l.thread,
		Start:  turns[0].start,
		End:    turns[len(turns)-1].end,
		Status: model.StatusOK,
	}
	for _, ct := range turns {
		bt := buildSteps(ct, meta.provider())
		steps := child.attachSubagents(ct, bt)
		sa.Steps = append(sa.Steps, steps...)
		if sa.Input == "" {
			sa.Input = bt.input
		}
		if bt.output != "" {
			sa.Output = bt.output
		}
		if ct.end.After(sa.End) {
			sa.End = ct.end
		}
		if st, msg := turnStatus(ct); st == model.StatusError || (st == model.StatusAborted && sa.Status == model.StatusOK) {
			sa.Status, sa.StatusMessage = st, msg
		}
	}
	sortSteps(sa.Steps)
	return sa
}

// readLinesFrom calls fn for every complete line from offset on. A trailing line
// without '\n' is not passed and not consumed. It returns the offset just
// past the last complete line.
func readLinesFrom(f *os.File, offset int64, fn func(off int64, line []byte)) (int64, error) {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	r := bufio.NewReaderSize(f, 256<<10)
	off := offset
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// io.EOF: a partial last line (or nothing) is left for next time.
			if err == io.EOF {
				return off, nil
			}
			return off, err
		}
		start := off
		off += int64(len(line))
		line = line[:len(line)-1]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		if len(line) == 0 {
			continue
		}
		fn(start, line)
	}
}

func readLines(f *os.File, offset int64, fn func(off int64, line []byte)) error {
	_, err := readLinesFrom(f, offset, fn)
	return err
}
