// Package install adds and removes flashcat-hook entries in the hook
// configuration of Claude Code, Codex and Cursor (CONTRACT §3). Edits are made
// with sjson on the original bytes, so other content, formatting and key order
// are kept; our entries are only appended at the end of each event array.
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Tool names as used on the command line.
const (
	Claude = "claude"
	Codex  = "codex"
	Cursor = "cursor"
)

// Tools lists the supported tools in display order.
var Tools = []string{Claude, Codex, Cursor}

type style int

const (
	styleClaude style = iota // {"hooks":{"Ev":[{"hooks":[{"type":"command","command":…}]}]}}
	styleCursor              // {"version":1,"hooks":{"ev":[{"command":…}]}}
)

// Target is one tool's hook configuration file.
type Target struct {
	Tool   string
	File   string
	Events []string
	// legacy are events earlier versions registered: Install and Uninstall
	// remove our entries from them.
	legacy []string
	style  style
}

// CursorEvents are the Cursor hook events flashcat-hook listens to.
// subagentStart is not one of them: it is a permission hook (a broken reply
// would block the subagent) and subagentStop carries everything we need.
var CursorEvents = []string{
	"beforeSubmitPrompt", "afterAgentResponse", "postToolUse", "postToolUseFailure",
	"subagentStop", "stop", "sessionEnd",
}

// cursorLegacyEvents were registered by earlier versions.
var cursorLegacyEvents = []string{"subagentStart"}

// TargetFor returns the configuration file and events of a tool:
// Claude Code ${CLAUDE_CONFIG_DIR:-~/.claude}/settings.json (Stop, SessionEnd),
// Codex ${CODEX_HOME:-~/.codex}/hooks.json (Stop), Cursor ~/.cursor/hooks.json.
func TargetFor(tool string) (Target, error) {
	home, _ := os.UserHomeDir()
	switch tool {
	case Claude:
		dir := os.Getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			dir = filepath.Join(home, ".claude")
		}
		return Target{Tool: tool, File: filepath.Join(dir, "settings.json"), Events: []string{"Stop", "SessionEnd"}, style: styleClaude}, nil
	case Codex:
		dir := os.Getenv("CODEX_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
		return Target{Tool: tool, File: filepath.Join(dir, "hooks.json"), Events: []string{"Stop"}, style: styleClaude}, nil
	case Cursor:
		return Target{Tool: tool, File: filepath.Join(home, ".cursor", "hooks.json"), Events: CursorEvents, legacy: cursorLegacyEvents, style: styleCursor}, nil
	}
	return Target{}, fmt.Errorf("unknown tool %q (want claude, codex or cursor)", tool)
}

// Command is the hook command for a tool: the absolute binary path, quoted
// when it contains spaces, followed by "hook <tool>".
func Command(bin, tool string) string {
	if strings.ContainsAny(bin, " \t") {
		bin = `"` + bin + `"`
	}
	return bin + " hook " + tool
}

// IsOurs reports whether a hook command is a flashcat-hook entry for tool
// (CONTRACT §3 rule 4: the command contains "flashcat-hook hook <tool>";
// a closing quote or ".exe" after the binary name is allowed).
func IsOurs(command, tool string) bool {
	re := regexp.MustCompile(`flashcat-hook(?:\.exe)?["']?\s+hook\s+` + regexp.QuoteMeta(tool) + `(?:$|[\s"'])`)
	return re.MatchString(command)
}

// Action is what Install or Uninstall did to a file.
type Action string

const (
	Unchanged Action = "unchanged"
	Created   Action = "created"
	Updated   Action = "updated"
)

// Result describes a change.
type Result struct {
	File   string
	Backup string // empty when nothing was backed up
	Action Action
}

// Install adds command to every event of t, idempotently.
func Install(t Target, command string, now time.Time) (Result, error) {
	path, orig, exists, err := read(t.File)
	if err != nil {
		return Result{File: t.File}, err
	}
	body, suffix := splitSpace(orig)
	if !exists || len(body) == 0 {
		out, err := fresh(t, command)
		if err != nil {
			return Result{File: t.File}, err
		}
		if err := write(path, out, 0o644); err != nil {
			return Result{File: t.File}, err
		}
		return Result{File: t.File, Action: Created}, nil
	}
	if err := checkObject(body); err != nil {
		return Result{File: t.File}, fmt.Errorf("%s: %w", t.File, err)
	}
	out, err := addEntries(t, body, command)
	if err == nil && len(t.legacy) > 0 {
		out, err = removeEntriesFrom(t, out, t.legacy)
	}
	if err != nil {
		return Result{File: t.File}, fmt.Errorf("%s: %w", t.File, err)
	}
	if bytes.Equal(out, body) {
		return Result{File: t.File, Action: Unchanged}, nil
	}
	return commit(t.File, path, orig, append(out, suffix...), now)
}

// Uninstall removes our entries from t and drops event arrays and the hooks
// object when they become empty.
func Uninstall(t Target, now time.Time) (Result, error) {
	path, orig, exists, err := read(t.File)
	if err != nil || !exists {
		return Result{File: t.File, Action: Unchanged}, err
	}
	body, suffix := splitSpace(orig)
	if len(body) == 0 {
		return Result{File: t.File, Action: Unchanged}, nil
	}
	if err := checkObject(body); err != nil {
		return Result{File: t.File}, fmt.Errorf("%s: %w", t.File, err)
	}
	out, err := removeEntries(t, body)
	if err != nil {
		return Result{File: t.File}, fmt.Errorf("%s: %w", t.File, err)
	}
	if bytes.Equal(out, body) {
		return Result{File: t.File, Action: Unchanged}, nil
	}
	return commit(t.File, path, orig, append(out, suffix...), now)
}

// Registered returns the events of t that have our entry.
func Registered(t Target) ([]string, error) {
	_, orig, exists, err := read(t.File)
	if err != nil || !exists {
		return nil, err
	}
	body, _ := splitSpace(orig)
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil, nil
	}
	var evs []string
	for _, ev := range t.Events {
		if len(findOurs(t, body, ev)) > 0 {
			evs = append(evs, ev)
		}
	}
	return evs, nil
}

// read resolves symlinks (so dotfile managers keep their links) and reads the file.
func read(file string) (path string, data []byte, exists bool, err error) {
	path = file
	if p, err := filepath.EvalSymlinks(file); err == nil {
		path = p
	}
	data, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil, false, nil
	}
	return path, data, err == nil, err
}

// splitSpace separates trailing whitespace, which sjson does not keep.
func splitSpace(b []byte) (body, suffix []byte) {
	t := bytes.TrimRight(b, " \t\r\n")
	return t, b[len(t):]
}

func checkObject(body []byte) error {
	if !gjson.ValidBytes(body) {
		return errors.New("not valid JSON; fix or move the file and retry")
	}
	if !gjson.ParseBytes(body).IsObject() {
		return errors.New("top level is not a JSON object")
	}
	return nil
}

func commit(file, path string, orig, out []byte, now time.Time) (Result, error) {
	res := Result{File: file, Action: Updated}
	bak, err := backup(path, orig, now)
	if err != nil {
		return Result{File: file}, fmt.Errorf("backup %s: %w", file, err)
	}
	res.Backup = bak
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := write(path, out, mode); err != nil {
		return Result{File: file}, err
	}
	return res, nil
}

// backup copies the original bytes to <file>.bak-<YYYYmmddHHMMSS>.
func backup(path string, orig []byte, now time.Time) (string, error) {
	base := path + ".bak-" + now.Format("20060102150405")
	name := base
	for i := 1; ; i++ {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) && i < 100 {
			name = fmt.Sprintf("%s-%d", base, i)
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = io.Copy(f, bytes.NewReader(orig))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return name, err
	}
}

func write(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	_ = f.Chmod(mode)
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// esc escapes an object key for a gjson/sjson path.
func esc(k string) string {
	r := strings.NewReplacer(`\`, `\\`, ".", `\.`, "*", `\*`, "?", `\?`, "|", `\|`, "#", `\#`, "@", `\@`)
	return r.Replace(k)
}

func entryJSON(t Target, command string) string {
	cmd, _ := json.Marshal(command)
	if t.style == styleCursor {
		return `{"command":` + string(cmd) + `}`
	}
	return `{"hooks":[{"type":"command","command":` + string(cmd) + `}]}`
}

// fresh builds a new, indented configuration file.
func fresh(t Target, command string) ([]byte, error) {
	b := []byte("{}")
	var err error
	if t.style == styleCursor {
		if b, err = sjson.SetRawBytes(b, "version", []byte("1")); err != nil {
			return nil, err
		}
	}
	if b, err = sjson.SetRawBytes(b, "hooks", []byte("{}")); err != nil {
		return nil, err
	}
	for _, ev := range t.Events {
		if b, err = sjson.SetRawBytes(b, "hooks."+esc(ev), []byte("["+entryJSON(t, command)+"]")); err != nil {
			return nil, err
		}
	}
	return []byte(gjson.GetBytes(b, "@pretty").Raw), nil
}

// ourRef locates one of our commands in an event array.
type ourRef struct {
	group   int // index in the event array
	hook    int // index in the group's "hooks" (-1 for Cursor entries)
	command string
	alone   bool // the group holds only our hooks: remove the whole group
}

func findOurs(t Target, body []byte, ev string) []ourRef {
	arr := gjson.GetBytes(body, "hooks."+esc(ev))
	if !arr.IsArray() {
		return nil
	}
	var refs []ourRef
	for gi, g := range arr.Array() {
		if t.style == styleCursor {
			if c := g.Get("command"); c.Type == gjson.String && IsOurs(c.String(), t.Tool) {
				refs = append(refs, ourRef{group: gi, hook: -1, command: c.String(), alone: true})
			}
			continue
		}
		hooks := g.Get("hooks")
		if !hooks.IsArray() {
			continue
		}
		list := hooks.Array()
		var mine []ourRef
		for hi, h := range list {
			if c := h.Get("command"); c.Type == gjson.String && IsOurs(c.String(), t.Tool) {
				mine = append(mine, ourRef{group: gi, hook: hi, command: c.String()})
			}
		}
		if len(mine) > 0 && len(mine) == len(list) {
			for i := range mine {
				mine[i].alone = true
			}
		}
		refs = append(refs, mine...)
	}
	return refs
}

func addEntries(t Target, body []byte, command string) ([]byte, error) {
	var err error
	if t.style == styleCursor && !gjson.GetBytes(body, "version").Exists() {
		if body, err = sjson.SetRawBytes(body, "version", []byte("1")); err != nil {
			return nil, err
		}
	}
	hooks := gjson.GetBytes(body, "hooks")
	if !hooks.Exists() {
		if body, err = sjson.SetRawBytes(body, "hooks", []byte("{}")); err != nil {
			return nil, err
		}
	} else if !hooks.IsObject() {
		return nil, errors.New(`"hooks" is not an object`)
	}
	for _, ev := range t.Events {
		path := "hooks." + esc(ev)
		arr := gjson.GetBytes(body, path)
		switch {
		case !arr.Exists():
			body, err = sjson.SetRawBytes(body, path, []byte("["+entryJSON(t, command)+"]"))
		case !arr.IsArray():
			return nil, fmt.Errorf("%q is not an array", "hooks."+ev)
		default:
			refs := findOurs(t, body, ev)
			exact := false
			for _, r := range refs {
				if r.command == command {
					exact = true
				}
			}
			switch {
			case exact:
			case len(refs) > 0:
				// Ours with another binary path: update in place so the entry
				// keeps its position.
				body, err = sjson.SetBytes(body, commandPath(t, ev, refs[0]), command)
			default:
				body, err = sjson.SetRawBytes(body, path+".-1", []byte(entryJSON(t, command)))
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

func commandPath(t Target, ev string, r ourRef) string {
	if t.style == styleCursor {
		return fmt.Sprintf("hooks.%s.%d.command", esc(ev), r.group)
	}
	return fmt.Sprintf("hooks.%s.%d.hooks.%d.command", esc(ev), r.group, r.hook)
}

func removeEntries(t Target, body []byte) ([]byte, error) {
	return removeEntriesFrom(t, body, append(append([]string(nil), t.Events...), t.legacy...))
}

// removeEntriesFrom removes our entries from the given events.
func removeEntriesFrom(t Target, body []byte, events []string) ([]byte, error) {
	var err error
	for _, ev := range events {
		refs := findOurs(t, body, ev)
		if len(refs) == 0 {
			continue
		}
		// Delete from the end so earlier indexes stay valid.
		for i := len(refs) - 1; i >= 0; i-- {
			r := refs[i]
			if r.alone {
				if i > 0 && refs[i-1].group == r.group {
					continue // the group is removed once, with its first ref
				}
				body, err = sjson.DeleteBytes(body, fmt.Sprintf("hooks.%s.%d", esc(ev), r.group))
			} else {
				body, err = sjson.DeleteBytes(body, fmt.Sprintf("hooks.%s.%d.hooks.%d", esc(ev), r.group, r.hook))
			}
			if err != nil {
				return nil, err
			}
		}
		path := "hooks." + esc(ev)
		if arr := gjson.GetBytes(body, path); arr.IsArray() && len(arr.Array()) == 0 {
			if body, err = sjson.DeleteBytes(body, path); err != nil {
				return nil, err
			}
		}
	}
	if h := gjson.GetBytes(body, "hooks"); h.IsObject() && len(h.Map()) == 0 {
		if body, err = sjson.DeleteBytes(body, "hooks"); err != nil {
			return nil, err
		}
	}
	return body, nil
}
