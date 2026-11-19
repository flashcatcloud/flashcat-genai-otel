package config

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// gitTimeout bounds each git invocation; the hook must stay fast.
const gitTimeout = 1500 * time.Millisecond

// Identity resolves user.id and flashcat.tag.repo for turns, caching git
// lookups per directory for the life of the process.
type Identity struct {
	ConfigUser string
	// Git runs git with args in dir and returns trimmed stdout; nil uses the
	// git binary. Tests replace it.
	Git func(dir string, args ...string) (string, error)

	mu    sync.Mutex
	cache map[string]string
}

func (id *Identity) git(dir string, args ...string) string {
	key := dir + "\x00" + strings.Join(args, "\x00")
	id.mu.Lock()
	if v, ok := id.cache[key]; ok {
		id.mu.Unlock()
		return v
	}
	id.mu.Unlock()
	run := id.Git
	if run == nil {
		run = runGit
	}
	v, err := run(dir, args...)
	if err != nil {
		v = ""
	}
	id.mu.Lock()
	if id.cache == nil {
		id.cache = map[string]string{}
	}
	id.cache[key] = v
	id.mu.Unlock()
	return v
}

func runGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func dirExists(d string) bool {
	if d == "" {
		return false
	}
	fi, err := os.Stat(d)
	return err == nil && fi.IsDir()
}

// User returns turnUser, else the configured user, else
// `git config user.email` run in cwd (in the process directory when cwd no
// longer exists, so the global git config still applies), else "".
func (id *Identity) User(turnUser, cwd string) string {
	if s := strings.TrimSpace(turnUser); s != "" {
		return s
	}
	if s := strings.TrimSpace(id.ConfigUser); s != "" {
		return s
	}
	dir := cwd
	if !dirExists(dir) {
		dir = ""
	}
	return id.git(dir, "config", "user.email")
}

// Repo returns turnRepo, else the remote URL of the git repository holding
// cwd without scheme, credentials and ".git", else the repository (or cwd)
// directory name.
func (id *Identity) Repo(turnRepo, cwd string) string {
	if s := strings.TrimSpace(turnRepo); s != "" {
		return NormalizeRemote(s)
	}
	if cwd == "" {
		return ""
	}
	if !dirExists(cwd) {
		return filepath.Base(filepath.Clean(cwd))
	}
	top := id.git(cwd, "rev-parse", "--show-toplevel")
	if top == "" {
		return filepath.Base(filepath.Clean(cwd))
	}
	remote := id.git(cwd, "config", "--get", "remote.origin.url")
	if remote == "" {
		if names := id.git(cwd, "remote"); names != "" {
			first := strings.Fields(names)[0]
			remote = id.git(cwd, "config", "--get", "remote."+first+".url")
		}
	}
	if remote != "" {
		return NormalizeRemote(remote)
	}
	return filepath.Base(filepath.Clean(top))
}

// NormalizeRemote turns a git remote into host/path: it drops the scheme,
// any credentials and a trailing ".git", and rewrites scp-like
// "git@host:org/repo" as "host/org/repo".
func NormalizeRemote(remote string) string {
	r := strings.TrimSpace(remote)
	if r == "" {
		return ""
	}
	if strings.Contains(r, "://") {
		if u, err := url.Parse(r); err == nil && u.Host != "" {
			r = u.Host + u.Path
		} else {
			r = r[strings.Index(r, "://")+3:]
			if at := strings.LastIndexByte(r, '@'); at >= 0 {
				r = r[at+1:]
			}
		}
	} else if at := strings.IndexByte(r, '@'); at >= 0 && strings.Contains(r[at:], ":") {
		// scp-like: user@host:path
		r = r[at+1:]
		if i := strings.IndexByte(r, ':'); i >= 0 {
			r = r[:i] + "/" + strings.TrimPrefix(r[i+1:], "/")
		}
	}
	r = strings.TrimSuffix(r, "/")
	r = strings.TrimSuffix(r, ".git")
	return r
}
