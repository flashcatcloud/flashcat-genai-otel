// Command flashcat-hook sends Claude Code, Codex and Cursor sessions to
// Flashcat AI Agent observability. See ../../CONTRACT.md.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/config"
	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/model"
	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/otlp"
)

// version is set by scripts/release.sh via -ldflags.
var version = "dev"

const usage = `flashcat-hook：把 Claude Code / Codex / Cursor 的会话发到 Flashcat AI Agent 可观测

用法：
  flashcat-hook install <claude|codex|cursor> --endpoint <url> --key <keyID:secret> --app <应用名>
                        [--content none|no_tool_content|full] [--user <邮箱>]
  flashcat-hook uninstall <claude|codex|cursor> [--purge]
  flashcat-hook status [--json]
  flashcat-hook test
  flashcat-hook hook <claude|codex|cursor>      （由编码工具调用，从 stdin 读事件）
  flashcat-hook version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "hook":
		return cmdHook(args[1:], stdin, stdout)
	case "install":
		return cmdInstall(args[1:], stdout, stderr)
	case "uninstall":
		return cmdUninstall(args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "test":
		return cmdTest(args[1:], stdout, stderr)
	case "export":
		return cmdExport(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "未知子命令 %q\n\n%s", args[0], usage)
	return 2
}

// toolOf maps the CLI tool name to model.Tool.
func toolOf(name string) model.Tool {
	switch name {
	case "codex":
		return model.ToolCodex
	case "cursor":
		return model.ToolCursor
	}
	return model.ToolClaude
}

func resourceInfo(app string) otlp.Resource {
	host, _ := os.Hostname()
	return otlp.Resource{App: app, HostName: host, HookVersion: version}
}

func newSender(cfg config.Config) *otlp.Sender {
	return &otlp.Sender{Endpoint: cfg.Endpoint, Key: cfg.Key, UserAgent: "flashcat-hook/" + version}
}

// resolveTurns fills UserID and Repo (CONTRACT §4.2).
func resolveTurns(turns []model.Turn, id *config.Identity) {
	for i := range turns {
		t := &turns[i]
		t.UserID = id.User(t.UserID, t.Cwd)
		t.Repo = id.Repo(t.Repo, t.Cwd)
	}
}

// sendProbe sends the onboarding probe with the given agent name.
func sendProbe(cfg config.Config, tool model.Tool) error {
	td := otlp.Probe(resourceInfo(cfg.App), tool, time.Now())
	return newSender(cfg).Send(context.Background(), td)
}

// installRecord remembers the most recently installed tool for the probe's
// gen_ai.agent.name (CONTRACT §4.6).
type installRecord struct {
	Tool string    `json:"tool"`
	Time time.Time `json:"time"`
}

func installRecordPath() string { return filepath.Join(config.StateDir(), "install.json") }

func lastInstalledTool() model.Tool {
	var r installRecord
	if b, err := os.ReadFile(installRecordPath()); err == nil && json.Unmarshal(b, &r) == nil && r.Tool != "" {
		return toolOf(r.Tool)
	}
	return model.ToolClaude
}

func saveInstallRecord(tool string) error {
	b, _ := json.Marshal(installRecord{Tool: tool, Time: time.Now().UTC()})
	return config.WriteFileAtomic(installRecordPath(), b, 0o600)
}

// exportRecord is the last delivery attempt, shown by status.
type exportRecord struct {
	Time  time.Time `json:"time"`
	Tool  string    `json:"tool"`
	Event string    `json:"event"`
	Turns int       `json:"turns"`
	Spans int       `json:"spans"`
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
}

func exportRecordPath() string { return filepath.Join(config.StateDir(), "last_export.json") }

func saveExportRecord(r exportRecord) {
	b, _ := json.Marshal(r)
	_ = config.WriteFileAtomic(exportRecordPath(), b, 0o600)
}

func loadExportRecord() *exportRecord {
	var r exportRecord
	b, err := os.ReadFile(exportRecordPath())
	if err != nil || json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}
