package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

var now = time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)

const bin = "/Users/a/.flashcat/bin/flashcat-hook"

// sandbox points HOME and the tools' config variables at a temp dir.
func sandbox(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	return home
}

func target(t *testing.T, tool string) Target {
	t.Helper()
	tg, err := TargetFor(tool)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func backups(t *testing.T, file string) []string {
	m, _ := filepath.Glob(file + ".bak-*")
	return m
}

// Existing user configuration, formatted by hand, with user hooks on the
// events we use and on others.
var existing = map[string]string{
	Claude: `{
  "model": "opus",
  "permissions": { "allow": ["Bash(ls:*)"] },
  "hooks": {
    "PreToolUse": [
      { "matcher": "Bash", "hooks": [ { "type": "command", "command": "~/bin/guard.sh" } ] }
    ],
    "Stop": [
      { "hooks": [ { "type": "command", "command": "afplay done.aiff" } ] },
      { "matcher": "", "hooks": [ { "type": "command", "command": "other-hook hook claude" } ] }
    ]
  },
  "env": { "A": "1" }
}
`,
	Codex: `{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "notify-send codex"
          }
        ]
      }
    ]
  }
}`,
	Cursor: `{
  "version": 1,
  "hooks": {
    "stop": [ { "command": "./audit.sh" } ],
    "afterFileEdit": [ { "command": "prettier --write" } ]
  }
}
`,
}

func TestInstallMissingFile(t *testing.T) {
	for _, tool := range Tools {
		t.Run(tool, func(t *testing.T) {
			sandbox(t)
			tg := target(t, tool)
			res, err := Install(tg, Command(bin, tool), now)
			if err != nil {
				t.Fatal(err)
			}
			if res.Action != Created || res.Backup != "" {
				t.Fatalf("result %+v", res)
			}
			b := readFile(t, tg.File)
			if !gjson.Valid(b) {
				t.Fatalf("invalid JSON: %s", b)
			}
			for _, ev := range tg.Events {
				arr := gjson.Get(b, "hooks."+ev).Array()
				if len(arr) != 1 {
					t.Fatalf("%s: %d entries", ev, len(arr))
				}
				cmd := arr[0].Get("hooks.0.command").String()
				if tool == Cursor {
					cmd = arr[0].Get("command").String()
				} else if arr[0].Get("hooks.0.type").String() != "command" {
					t.Errorf("%s: type not command", ev)
				}
				if cmd != bin+" hook "+tool {
					t.Errorf("%s: command %q", ev, cmd)
				}
			}
			if tool == Cursor && gjson.Get(b, "version").Int() != 1 {
				t.Error("cursor file without version 1")
			}
			if n := len(gjson.Get(b, "hooks").Map()); n != len(tg.Events) {
				t.Errorf("events = %d, want %d", n, len(tg.Events))
			}
			evs, _ := Registered(tg)
			if len(evs) != len(tg.Events) {
				t.Errorf("registered %v", evs)
			}
		})
	}
}

func TestInstallPreservesExistingAndUninstallRestores(t *testing.T) {
	for _, tool := range Tools {
		t.Run(tool, func(t *testing.T) {
			sandbox(t)
			tg := target(t, tool)
			orig := existing[tool]
			writeFile(t, tg.File, orig)
			before := map[string][]string{}
			for _, ev := range tg.Events {
				for _, e := range gjson.Get(orig, "hooks."+ev).Array() {
					before[ev] = append(before[ev], e.Raw)
				}
			}

			res, err := Install(tg, Command(bin, tool), now)
			if err != nil {
				t.Fatal(err)
			}
			if res.Action != Updated {
				t.Fatalf("action %s", res.Action)
			}
			if bk := backups(t, tg.File); len(bk) != 1 || readFile(t, bk[0]) != orig || !strings.HasSuffix(bk[0], ".bak-20261117093000") {
				t.Fatalf("backup %v", bk)
			}
			b := readFile(t, tg.File)
			if !gjson.Valid(b) {
				t.Fatalf("invalid JSON after install:\n%s", b)
			}
			// Untouched parts are byte-identical, in order.
			for _, key := range []string{"model", "permissions", "env", "hooks.PreToolUse", "hooks.afterFileEdit", "version"} {
				if a, z := gjson.Get(orig, key).Raw, gjson.Get(b, key).Raw; a != z {
					t.Errorf("%s changed: %q -> %q", key, a, z)
				}
			}
			for _, ev := range tg.Events {
				arr := gjson.Get(b, "hooks."+ev).Array()
				if len(arr) != len(before[ev])+1 {
					t.Fatalf("%s: %d entries, want %d", ev, len(arr), len(before[ev])+1)
				}
				for i, raw := range before[ev] {
					if arr[i].Raw != raw {
						t.Errorf("%s[%d] changed: %q -> %q", ev, i, raw, arr[i].Raw)
					}
				}
				last := arr[len(arr)-1]
				cmd := last.Get("hooks.0.command").String()
				if tool == Cursor {
					cmd = last.Get("command").String()
				}
				if cmd != bin+" hook "+tool {
					t.Errorf("%s: our entry is not last: %s", ev, last.Raw)
				}
			}
			if !strings.HasSuffix(b, "}\n") && strings.HasSuffix(orig, "\n") {
				t.Error("trailing newline lost")
			}

			// Idempotent: no change, no new backup.
			res2, err := Install(tg, Command(bin, tool), now.Add(time.Second))
			if err != nil || res2.Action != Unchanged || readFile(t, tg.File) != b || len(backups(t, tg.File)) != 1 {
				t.Fatalf("re-install: %+v %v", res2, err)
			}

			// Uninstall restores the original bytes (our entries were last).
			res3, err := Uninstall(tg, now.Add(2*time.Second))
			if err != nil || res3.Action != Updated {
				t.Fatalf("uninstall: %+v %v", res3, err)
			}
			if got := readFile(t, tg.File); got != orig {
				t.Fatalf("uninstall did not restore the original:\n--- got\n%s\n--- want\n%s", got, orig)
			}
			if evs, _ := Registered(tg); len(evs) != 0 {
				t.Errorf("still registered: %v", evs)
			}
			res4, _ := Uninstall(tg, now.Add(3*time.Second))
			if res4.Action != Unchanged {
				t.Error("second uninstall changed the file")
			}
		})
	}
}

func TestUninstallRestoresFileWithoutHooksKey(t *testing.T) {
	sandbox(t)
	tg := target(t, Claude)
	orig := "{\n  \"model\": \"opus\"\n}\n"
	writeFile(t, tg.File, orig)
	if _, err := Install(tg, Command(bin, Claude), now); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(tg, now); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, tg.File); got != orig {
		t.Fatalf("got %q, want %q", got, orig)
	}
}

func TestUninstallKeepsUserHooksInSharedGroup(t *testing.T) {
	sandbox(t)
	tg := target(t, Claude)
	cmd := Command(bin, Claude)
	writeFile(t, tg.File, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo a"},{"type":"command","command":"`+cmd+`"}]}],"SessionEnd":[{"hooks":[{"type":"command","command":"`+cmd+`"}]}]}}`)
	if _, err := Uninstall(tg, now); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, tg.File); got != `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo a"}]}]}}` {
		t.Fatalf("got %s", got)
	}
}

func TestUninstallOnlyOwnTool(t *testing.T) {
	sandbox(t)
	tg := target(t, Claude)
	// Claude Code settings that also carry a codex entry by mistake: only the
	// claude entry is ours to remove.
	writeFile(t, tg.File, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/flashcat-hook hook codex"}]},{"hooks":[{"type":"command","command":"/x/flashcat-hook hook claude"}]}]}}`)
	if _, err := Uninstall(tg, now); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, tg.File); got != `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/flashcat-hook hook codex"}]}]}}` {
		t.Fatalf("got %s", got)
	}
}

func TestInstallUpdatesMovedBinaryInPlace(t *testing.T) {
	sandbox(t)
	tg := target(t, Codex)
	writeFile(t, tg.File, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/old/flashcat-hook hook codex"}]},{"hooks":[{"type":"command","command":"echo after"}]}]}}`)
	res, err := Install(tg, Command(bin, Codex), now)
	if err != nil || res.Action != Updated {
		t.Fatalf("%+v %v", res, err)
	}
	b := readFile(t, tg.File)
	arr := gjson.Get(b, "hooks.Stop").Array()
	if len(arr) != 2 || arr[0].Get("hooks.0.command").String() != bin+" hook codex" || arr[1].Get("hooks.0.command").String() != "echo after" {
		t.Fatalf("got %s", b)
	}
}

// An entry an earlier version added to subagentStart (a blocking permission
// hook) is removed on reinstall and on uninstall; other subagentStart
// entries stay.
func TestCursorLegacySubagentStart(t *testing.T) {
	sandbox(t)
	tg := target(t, Cursor)
	cmd := Command(bin, Cursor)
	legacy := `{"version":1,"hooks":{"subagentStart":[{"command":"./gate.sh"},{"command":"` + cmd + `"}],"stop":[{"command":"` + cmd + `"}]}}`
	writeFile(t, tg.File, legacy)
	if _, err := Install(tg, cmd, now); err != nil {
		t.Fatal(err)
	}
	b := readFile(t, tg.File)
	if arr := gjson.Get(b, "hooks.subagentStart").Array(); len(arr) != 1 || arr[0].Get("command").String() != "./gate.sh" {
		t.Fatalf("legacy entry kept on install: %s", b)
	}
	if evs, _ := Registered(tg); len(evs) != len(CursorEvents) {
		t.Fatalf("registered %v", evs)
	}

	writeFile(t, tg.File, `{"version":1,"hooks":{"subagentStart":[{"command":"`+cmd+`"}]}}`)
	if _, err := Uninstall(tg, now); err != nil {
		t.Fatal(err)
	}
	if b := readFile(t, tg.File); strings.Contains(b, "flashcat-hook") || gjson.Get(b, "hooks").Exists() {
		t.Fatalf("legacy entry kept on uninstall: %s", b)
	}
}

func TestCursorAddsVersionToExistingFile(t *testing.T) {
	sandbox(t)
	tg := target(t, Cursor)
	writeFile(t, tg.File, `{"hooks":{}}`)
	if _, err := Install(tg, Command(bin, Cursor), now); err != nil {
		t.Fatal(err)
	}
	b := readFile(t, tg.File)
	if gjson.Get(b, "version").Int() != 1 || len(gjson.Get(b, "hooks").Map()) != len(CursorEvents) {
		t.Fatalf("got %s", b)
	}
}

func TestInvalidJSONUntouched(t *testing.T) {
	sandbox(t)
	tg := target(t, Claude)
	writeFile(t, tg.File, `{"hooks": {`)
	if _, err := Install(tg, Command(bin, Claude), now); err == nil {
		t.Fatal("want error")
	}
	if readFile(t, tg.File) != `{"hooks": {` || len(backups(t, tg.File)) != 0 {
		t.Fatal("invalid file was modified")
	}
	writeFile(t, tg.File, `{"hooks": []}`)
	if _, err := Install(tg, Command(bin, Claude), now); err == nil {
		t.Fatal("want error for non-object hooks")
	}
}

func TestSymlinkKept(t *testing.T) {
	home := sandbox(t)
	tg := target(t, Claude)
	real := filepath.Join(home, "dotfiles", "settings.json")
	writeFile(t, real, `{"model":"opus"}`)
	if err := os.MkdirAll(filepath.Dir(tg.File), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, tg.File); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(tg, Command(bin, Claude), now); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(tg.File); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if !strings.Contains(readFile(t, real), "flashcat-hook hook claude") {
		t.Fatal("link target not updated")
	}
}

func TestCommandAndIsOurs(t *testing.T) {
	if got := Command("/Users/a b/.flashcat/bin/flashcat-hook", "claude"); got != `"/Users/a b/.flashcat/bin/flashcat-hook" hook claude` {
		t.Errorf("quoted command %s", got)
	}
	if got := Command(bin, "codex"); got != bin+" hook codex" {
		t.Errorf("command %s", got)
	}
	yes := []string{
		bin + " hook claude",
		`"/Users/a b/.flashcat/bin/flashcat-hook" hook claude`,
		`C:\Users\a\.flashcat\bin\flashcat-hook.exe hook claude`,
		`"C:\Program Files\flashcat-hook.exe" hook claude`,
		"flashcat-hook hook claude --debug",
	}
	for _, c := range yes {
		if !IsOurs(c, "claude") {
			t.Errorf("IsOurs(%q) = false", c)
		}
	}
	no := []string{"flashcat-hook hook codex", "other-hook hook claude", "flashcat-hook hook claudex", "echo flashcat-hook", ""}
	for _, c := range no {
		if IsOurs(c, "claude") {
			t.Errorf("IsOurs(%q) = true", c)
		}
	}
}

func TestTargets(t *testing.T) {
	home := sandbox(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	if tg := target(t, Claude); tg.File != filepath.Join(home, ".claude", "settings.json") || strings.Join(tg.Events, ",") != "Stop,SessionEnd" {
		t.Errorf("claude target %+v", tg)
	}
	if tg := target(t, Codex); tg.File != filepath.Join(home, ".codex", "hooks.json") || strings.Join(tg.Events, ",") != "Stop" {
		t.Errorf("codex target %+v", tg)
	}
	if tg := target(t, Cursor); tg.File != filepath.Join(home, ".cursor", "hooks.json") ||
		strings.Join(tg.Events, ",") != "beforeSubmitPrompt,afterAgentResponse,postToolUse,postToolUseFailure,subagentStop,stop,sessionEnd" {
		t.Errorf("cursor target %+v", tg)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	if tg := target(t, Claude); tg.File != filepath.Join("/custom/claude", "settings.json") {
		t.Errorf("CLAUDE_CONFIG_DIR ignored: %s", tg.File)
	}
	if _, err := TargetFor("vim"); err == nil {
		t.Error("unknown tool accepted")
	}
}
