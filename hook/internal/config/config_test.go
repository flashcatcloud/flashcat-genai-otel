package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

func clearEnv(t *testing.T) {
	for _, e := range []string{EnvEndpoint, EnvKey, EnvApp, EnvContent, EnvUser, EnvDisabled} {
		t.Setenv(e, "")
		os.Unsetenv(e)
	}
}

func TestPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	if Path() != filepath.Join(home, ".config", "flashcat", "hook.json") {
		t.Errorf("default config path %s", Path())
	}
	if StateDir() != filepath.Join(home, ".local", "state", "flashcat-hook") {
		t.Errorf("default state dir %s", StateDir())
	}
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	t.Setenv("XDG_STATE_HOME", "/x/state")
	if Path() != "/x/cfg/flashcat/hook.json" || StateDir() != "/x/state/flashcat-hook" {
		t.Errorf("XDG paths %s %s", Path(), StateDir())
	}
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if !strings.HasPrefix(Path(), home) {
		t.Errorf("relative XDG_CONFIG_HOME must be ignored: %s", Path())
	}
}

func TestSaveLoadAndPermissions(t *testing.T) {
	clearEnv(t)
	p := filepath.Join(t.TempDir(), "flashcat", "hook.json")
	if _, err := LoadFile(p); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	c := Config{Endpoint: "https://x/v1/traces", Key: "kid:sec", App: "coding-a", Content: "full", User: "u@x", Enabled: true}
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	di, _ := os.Stat(filepath.Dir(p))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes file %v dir %v", fi.Mode().Perm(), di.Mode().Perm())
	}
	got, err := LoadFile(p)
	if err != nil || got != c {
		t.Fatalf("round trip %+v %v", got, err)
	}
	// Overwrite keeps 0600 (install runs again).
	if err := Save(p, Config{Endpoint: "e", Key: "k:s", App: "a"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatal("mode changed on overwrite")
	}
}

func TestEnabledDefaultAndEnv(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "hook.json")
	os.WriteFile(p, []byte(`{"endpoint":"e","key":"k:s","app":"a"}`), 0o600)
	c, _ := LoadFile(p)
	if !c.Enabled || c.Content != "none" {
		t.Fatalf("defaults: %+v", c)
	}
	os.WriteFile(p, []byte(`{"endpoint":"e","key":"k:s","app":"a","enabled":false}`), 0o600)
	if c, _ := LoadFile(p); c.Enabled {
		t.Fatal("enabled:false ignored")
	}

	t.Setenv(EnvEndpoint, "https://ci/v1/traces")
	t.Setenv(EnvKey, "ci:key")
	t.Setenv(EnvApp, "coding-ci")
	t.Setenv(EnvContent, "no_tool_content")
	t.Setenv(EnvUser, "ci@x")
	c = Default()
	ApplyEnv(&c)
	if c.Endpoint != "https://ci/v1/traces" || c.Key != "ci:key" || c.App != "coding-ci" || c.ContentLevel() != model.ContentNoToolContent || c.User != "ci@x" || !c.Configured() {
		t.Fatalf("env overrides: %+v", c)
	}
	t.Setenv(EnvDisabled, "1")
	ApplyEnv(&c)
	if c.Enabled || !Disabled() {
		t.Fatal("FLASHCAT_HOOK_DISABLED=1 ignored")
	}
}

func TestContentAndMask(t *testing.T) {
	if (Config{Content: "bogus"}).ContentLevel() != model.ContentNone {
		t.Error("unknown content must fall back to none")
	}
	if !ValidContent("full") || ValidContent("all") {
		t.Error("ValidContent")
	}
	if MaskKey("kid_1:secret") != "kid_1:****" || MaskKey("") != "" || strings.Contains(MaskKey("abcdefgh"), "efgh") {
		t.Error("MaskKey")
	}
}

type fakeGit struct {
	calls []string
	out   map[string]string
}

func (f *fakeGit) run(dir string, args ...string) (string, error) {
	k := dir + "|" + strings.Join(args, " ")
	f.calls = append(f.calls, k)
	if v, ok := f.out[k]; ok {
		return v, nil
	}
	return "", errors.New("exit 1")
}

func TestIdentityUser(t *testing.T) {
	cwd := t.TempDir()
	g := &fakeGit{out: map[string]string{cwd + "|config user.email": "repo@x", "|config user.email": "global@x"}}
	id := &Identity{Git: g.run}
	if id.User("turn@x", cwd) != "turn@x" {
		t.Error("turn user must win")
	}
	if id.User("", cwd) != "repo@x" {
		t.Error("git email in cwd")
	}
	if id.User("", filepath.Join(cwd, "gone")) != "global@x" {
		t.Error("missing cwd must fall back to the process directory")
	}
	id2 := &Identity{ConfigUser: "cfg@x", Git: g.run}
	if id2.User("", cwd) != "cfg@x" {
		t.Error("config user before git")
	}
	n := len(g.calls)
	id.User("", cwd)
	if len(g.calls) != n {
		t.Error("git lookups are not cached")
	}
}

func TestIdentityRepo(t *testing.T) {
	repo := t.TempDir()
	plain := t.TempDir()
	noRemote := t.TempDir()
	g := &fakeGit{out: map[string]string{
		repo + "|rev-parse --show-toplevel":          "/src/acme-app",
		repo + "|config --get remote.origin.url":     "git@github.com:acme/app.git",
		noRemote + "|rev-parse --show-toplevel":      "/src/local-only",
		noRemote + "|remote":                         "",
		plain + "|rev-parse --show-toplevel":         "",
		plain + "|config --get remote.origin.url":    "",
		noRemote + "|config --get remote.origin.url": "",
	}}
	id := &Identity{Git: g.run}
	if got := id.Repo("", repo); got != "github.com/acme/app" {
		t.Errorf("remote repo = %s", got)
	}
	if got := id.Repo("", noRemote); got != "local-only" {
		t.Errorf("no remote = %s", got)
	}
	if got := id.Repo("", plain); got != filepath.Base(plain) {
		t.Errorf("not a repo = %s", got)
	}
	if got := id.Repo("", "/does/not/exist/proj"); got != "proj" {
		t.Errorf("missing cwd = %s", got)
	}
	if got := id.Repo("https://github.com/acme/x.git", repo); got != "github.com/acme/x" {
		t.Errorf("turn repo = %s", got)
	}
	if id.Repo("", "") != "" {
		t.Error("empty cwd")
	}
}

func TestNormalizeRemote(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/app.git":              "github.com/acme/app",
		"https://user:token@gitlab.example.com/g/a":    "gitlab.example.com/g/a",
		"git@github.com:acme/app.git":                  "github.com/acme/app",
		"ssh://git@git.example.com:2222/team/repo.git": "git.example.com:2222/team/repo",
		"github.com/acme/app":                          "github.com/acme/app",
		"https://github.com/acme/app/":                 "github.com/acme/app",
	}
	for in, want := range cases {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}
