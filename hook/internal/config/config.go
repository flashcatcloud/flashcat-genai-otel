// Package config handles the hook's config file, environment overrides,
// local paths and the user / repository identity (CONTRACT §1, §2, §4.2).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

// Environment variables.
const (
	EnvEndpoint = "FLASHCAT_HOOK_ENDPOINT"
	EnvKey      = "FLASHCAT_HOOK_KEY"
	EnvApp      = "FLASHCAT_HOOK_APP"
	EnvContent  = "FLASHCAT_HOOK_CONTENT"
	EnvUser     = "FLASHCAT_HOOK_USER"
	EnvDisabled = "FLASHCAT_HOOK_DISABLED"
)

// Config is hook.json.
type Config struct {
	Endpoint string `json:"endpoint"`
	Key      string `json:"key"`
	App      string `json:"app"`
	Content  string `json:"content"`
	User     string `json:"user"`
	Enabled  bool   `json:"enabled"`
}

// Default returns an enabled config with content "none".
func Default() Config { return Config{Content: string(model.ContentNone), Enabled: true} }

// ContentLevel returns the configured content level; unknown values fall
// back to none.
func (c Config) ContentLevel() model.Content {
	switch model.Content(c.Content) {
	case model.ContentNoToolContent, model.ContentFull:
		return model.Content(c.Content)
	}
	return model.ContentNone
}

// Configured reports whether the config has what is needed to send.
func (c Config) Configured() bool {
	return c.Endpoint != "" && c.Key != "" && c.App != ""
}

// MaskedKey shows the key id and hides the secret.
func (c Config) MaskedKey() string {
	return MaskKey(c.Key)
}

// MaskKey turns "kid:secret" into "kid:****".
func MaskKey(k string) string {
	if k == "" {
		return ""
	}
	if i := strings.IndexByte(k, ':'); i >= 0 {
		return k[:i] + ":****"
	}
	if len(k) > 4 {
		return k[:4] + "****"
	}
	return "****"
}

// ValidContent reports whether s is a content level name.
func ValidContent(s string) bool {
	switch model.Content(s) {
	case model.ContentNone, model.ContentNoToolContent, model.ContentFull:
		return true
	}
	return false
}

// Path returns the config file path:
// $XDG_CONFIG_HOME/flashcat/hook.json (default ~/.config/...), or
// %APPDATA%\flashcat\hook.json on Windows.
func Path() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "flashcat", "hook.json")
		}
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "flashcat", "hook.json")
	}
	return filepath.Join(home(), ".config", "flashcat", "hook.json")
}

// StateDir returns the directory for state, locks and logs:
// $XDG_STATE_HOME/flashcat-hook (default ~/.local/state/flashcat-hook), or
// %LOCALAPPDATA%\flashcat-hook on Windows.
func StateDir() string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "flashcat-hook")
		}
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "flashcat-hook")
	}
	return filepath.Join(home(), ".local", "state", "flashcat-hook")
}

func home() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

// ErrNotFound is returned by Load when the config file does not exist.
var ErrNotFound = errors.New("config file not found")

// LoadFile reads the config file without environment overrides.
func LoadFile(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	// Missing "enabled" means enabled.
	raw := struct {
		Config
		Enabled *bool `json:"enabled"`
	}{Config: c}
	if err := json.Unmarshal(b, &raw); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	c = raw.Config
	c.Enabled = raw.Enabled == nil || *raw.Enabled
	if c.Content == "" {
		c.Content = string(model.ContentNone)
	}
	return c, nil
}

// Load reads the config file (missing is fine) and applies environment
// overrides. The returned error is only for unreadable or invalid files.
func Load() (Config, error) {
	c, err := LoadFile(Path())
	if errors.Is(err, ErrNotFound) {
		err = nil
	}
	ApplyEnv(&c)
	return c, err
}

// ApplyEnv overrides fields from FLASHCAT_HOOK_* variables.
func ApplyEnv(c *Config) {
	set := func(dst *string, env string) {
		if v, ok := os.LookupEnv(env); ok && strings.TrimSpace(v) != "" {
			*dst = strings.TrimSpace(v)
		}
	}
	set(&c.Endpoint, EnvEndpoint)
	set(&c.Key, EnvKey)
	set(&c.App, EnvApp)
	set(&c.Content, EnvContent)
	set(&c.User, EnvUser)
	if Disabled() {
		c.Enabled = false
	}
}

// Disabled reports FLASHCAT_HOOK_DISABLED set to a true value.
func Disabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvDisabled))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Save writes the config with mode 0600 (directory 0700), atomically.
func Save(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteFileAtomic(path, b, 0o600)
}

// WriteFileAtomic writes data to a temp file in the same directory and
// renames it over path. The parent directory is created with mode 0700.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
