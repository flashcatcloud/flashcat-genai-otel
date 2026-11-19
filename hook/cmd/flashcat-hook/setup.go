package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/config"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/install"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
	"github.com/flashcatcloud/flashcat-otel/hook/internal/state"
)

func validTool(t string) bool {
	for _, x := range install.Tools {
		if x == t {
			return true
		}
	}
	return false
}

// splitTool takes the first argument that names a tool and returns the
// remaining (flag) arguments.
func splitTool(args []string) (string, []string) {
	for i, a := range args {
		if validTool(a) {
			return a, append(append([]string{}, args[:i]...), args[i+1:]...)
		}
	}
	return "", args
}

// executablePath returns the binary path written into hook commands;
// tests replace it.
var executablePath = executable

// executable is the absolute, symlink-resolved path of this binary.
func executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Abs(p)
}

func cmdInstall(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || !validTool(args[0]) {
		fmt.Fprintln(stderr, "用法：flashcat-hook install <claude|codex|cursor> --endpoint <url> --key <keyID:secret> --app <应用名> [--content none|no_tool_content|full] [--user <邮箱>]")
		return 2
	}
	tool := args[0]
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	endpoint := fs.String("endpoint", "", "OTLP traces 地址（FLASHCAT_HOOK_ENDPOINT）")
	key := fs.String("key", "", "接入 key，keyID:secret（FLASHCAT_HOOK_KEY）")
	app := fs.String("app", "", "应用名（FLASHCAT_HOOK_APP）")
	content := fs.String("content", "", "正文档位 none|no_tool_content|full（FLASHCAT_HOOK_CONTENT）")
	user := fs.String("user", "", "用户标识，默认取 git 邮箱（FLASHCAT_HOOK_USER）")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	cfgPath := config.Path()
	cfg, err := config.LoadFile(cfgPath)
	if err != nil && !errors.Is(err, config.ErrNotFound) {
		fmt.Fprintf(stderr, "读取配置失败：%v\n", err)
		return 1
	}
	// Flags win over environment, which wins over the existing file.
	config.ApplyEnv(&cfg)
	pick := func(dst *string, v string) {
		if v = strings.TrimSpace(v); v != "" {
			*dst = v
		}
	}
	pick(&cfg.Endpoint, *endpoint)
	pick(&cfg.Key, *key)
	pick(&cfg.App, *app)
	pick(&cfg.Content, *content)
	pick(&cfg.User, *user)
	cfg.Enabled = true
	if cfg.Content == "" {
		cfg.Content = string(model.ContentNone)
	}
	var missing []string
	if cfg.Endpoint == "" {
		missing = append(missing, "--endpoint")
	}
	if cfg.Key == "" {
		missing = append(missing, "--key")
	}
	if cfg.App == "" {
		missing = append(missing, "--app")
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "缺少参数：%s\n", strings.Join(missing, " "))
		return 2
	}
	if u, err := url.Parse(cfg.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fmt.Fprintf(stderr, "--endpoint 不是合法的 http(s) 地址：%s\n", cfg.Endpoint)
		return 2
	}
	if !strings.Contains(cfg.Key, ":") {
		fmt.Fprintln(stderr, "--key 格式应为 keyID:secret")
		return 2
	}
	if !config.ValidContent(cfg.Content) {
		fmt.Fprintf(stderr, "--content 只能是 none、no_tool_content、full：%s\n", cfg.Content)
		return 2
	}

	if err := config.Save(cfgPath, cfg); err != nil {
		fmt.Fprintf(stderr, "写配置失败：%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "配置已写入 %s（权限 600）\n", cfgPath)

	bin, err := executablePath()
	if err != nil {
		fmt.Fprintf(stderr, "找不到 flashcat-hook 自身路径：%v\n", err)
		return 1
	}
	target, _ := install.TargetFor(tool)
	res, err := install.Install(target, install.Command(bin, tool), time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "写入 hook 配置失败：%v\n", err)
		return 1
	}
	switch res.Action {
	case install.Created:
		fmt.Fprintf(stdout, "已新建 %s 并注册 hook（%s）\n", res.File, strings.Join(target.Events, "、"))
	case install.Updated:
		fmt.Fprintf(stdout, "已在 %s 注册 hook（%s），原文件备份为 %s\n", res.File, strings.Join(target.Events, "、"), res.Backup)
	default:
		fmt.Fprintf(stdout, "%s 里已有 hook，未改动\n", res.File)
	}
	_ = saveInstallRecord(tool)

	probeErr := sendProbe(cfg, toolOf(tool))
	if probeErr != nil {
		fmt.Fprintf(stdout, "连通测试失败：%v\n", probeErr)
		fmt.Fprintln(stdout, "hook 已写好；检查 endpoint、key 和网络后运行 flashcat-hook test 重试。")
	} else {
		fmt.Fprintf(stdout, "连通测试成功：已发送 probe 到 %s\n", cfg.Endpoint)
	}
	fmt.Fprintln(stdout, nextSteps(tool))
	if probeErr != nil {
		return 1
	}
	return 0
}

func nextSteps(tool string) string {
	switch tool {
	case install.Codex:
		return "下一步：在 Codex 里运行 /hooks，信任 Flashcat hook。升级 flashcat-hook 后要重新信任；未信任时 codex exec 会静默跳过这个 hook。"
	case install.Cursor:
		return "下一步：Cursor 会自动重新加载 ~/.cursor/hooks.json；没有生效时完全退出并重启 Cursor。"
	}
	return "下一步：重启 Claude Code（已打开的会话不会加载新 hook）。"
}

func cmdUninstall(args []string, stdout, stderr io.Writer) int {
	tool, rest := splitTool(args)
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	purge := fs.Bool("purge", false, "同时删除配置和状态")
	if err := fs.Parse(rest); err != nil || !validTool(tool) {
		fmt.Fprintln(stderr, "用法：flashcat-hook uninstall <claude|codex|cursor> [--purge]")
		return 2
	}
	target, _ := install.TargetFor(tool)
	res, err := install.Uninstall(target, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "移除 hook 失败：%v\n", err)
		return 1
	}
	if res.Action == install.Updated {
		fmt.Fprintf(stdout, "已从 %s 移除 flashcat-hook，原文件备份为 %s\n", res.File, res.Backup)
	} else {
		fmt.Fprintf(stdout, "%s 里没有 flashcat-hook 的条目\n", res.File)
	}
	if *purge {
		code := 0
		for _, p := range []string{config.Path(), config.StateDir()} {
			if err := os.RemoveAll(p); err != nil {
				fmt.Fprintf(stderr, "删除 %s 失败：%v\n", p, err)
				code = 1
			} else {
				fmt.Fprintf(stdout, "已删除 %s\n", p)
			}
		}
		return code
	}
	fmt.Fprintf(stdout, "配置 %s 已保留（其他工具可能还在用）；加 --purge 可一并删除配置和状态。\n", config.Path())
	return 0
}

// statusReport is `status --json`.
type statusReport struct {
	Version      string                `json:"version"`
	ConfigFile   string                `json:"config_file"`
	ConfigExists bool                  `json:"config_exists"`
	ConfigError  string                `json:"config_error,omitempty"`
	Config       statusConfig          `json:"config"`
	EnvOverrides []string              `json:"env_overrides"`
	Configured   bool                  `json:"configured"`
	Tools        map[string]toolStatus `json:"tools"`
	LastExport   *exportRecord         `json:"last_export"`
	Pending      int                   `json:"pending"` // failed events waiting for a retry
	StateDir     string                `json:"state_dir"`
	LogFile      string                `json:"log_file"`
}

type statusConfig struct {
	Endpoint string `json:"endpoint"`
	Key      string `json:"key"`
	App      string `json:"app"`
	Content  string `json:"content"`
	User     string `json:"user"`
	Enabled  bool   `json:"enabled"`
}

type toolStatus struct {
	File       string   `json:"file"`
	Registered bool     `json:"registered"`
	Events     []string `json:"events"`
	Error      string   `json:"error,omitempty"`
}

func buildStatus() statusReport {
	r := statusReport{
		Version:    version,
		ConfigFile: config.Path(),
		Tools:      map[string]toolStatus{},
		StateDir:   config.StateDir(),
		LogFile:    logPath(),
		LastExport: loadExportRecord(),
		Pending:    state.Store{Dir: config.StateDir()}.PendingCount(),
	}
	cfg, err := config.LoadFile(r.ConfigFile)
	switch {
	case err == nil:
		r.ConfigExists = true
	case !errors.Is(err, config.ErrNotFound):
		r.ConfigExists = true
		r.ConfigError = err.Error()
	}
	for _, e := range []string{config.EnvEndpoint, config.EnvKey, config.EnvApp, config.EnvContent, config.EnvUser, config.EnvDisabled} {
		if v, ok := os.LookupEnv(e); ok && v != "" {
			r.EnvOverrides = append(r.EnvOverrides, e)
		}
	}
	if r.EnvOverrides == nil {
		r.EnvOverrides = []string{}
	}
	config.ApplyEnv(&cfg)
	r.Configured = cfg.Configured()
	r.Config = statusConfig{Endpoint: cfg.Endpoint, Key: cfg.MaskedKey(), App: cfg.App, Content: string(cfg.ContentLevel()), User: cfg.User, Enabled: cfg.Enabled}
	for _, t := range install.Tools {
		target, _ := install.TargetFor(t)
		ts := toolStatus{File: target.File, Events: []string{}}
		evs, err := install.Registered(target)
		if err != nil {
			ts.Error = err.Error()
		}
		if len(evs) > 0 {
			ts.Events = evs
			ts.Registered = true
		}
		r.Tools[t] = ts
	}
	return r
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	r := buildStatus()
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return 0
	}
	fmt.Fprintf(stdout, "flashcat-hook %s\n\n", r.Version)
	fmt.Fprintf(stdout, "配置文件  %s", r.ConfigFile)
	switch {
	case r.ConfigError != "":
		fmt.Fprintf(stdout, "（读取失败：%s）\n", r.ConfigError)
	case !r.ConfigExists:
		fmt.Fprintln(stdout, "（不存在，先运行 flashcat-hook install）")
	default:
		fmt.Fprintln(stdout)
	}
	c := r.Config
	fmt.Fprintf(stdout, "  endpoint  %s\n  key       %s\n  app       %s\n  content   %s\n  user      %s\n  enabled   %v\n",
		orDash(c.Endpoint), orDash(c.Key), orDash(c.App), c.Content, orDash(c.User), c.Enabled)
	if len(r.EnvOverrides) > 0 {
		fmt.Fprintf(stdout, "  环境变量覆盖：%s\n", strings.Join(r.EnvOverrides, " "))
	}
	fmt.Fprintln(stdout, "\nhook 注册")
	for _, t := range install.Tools {
		ts := r.Tools[t]
		mark := "未注册"
		if ts.Registered {
			mark = "已注册 " + strings.Join(ts.Events, "、")
		}
		if ts.Error != "" {
			mark += "（" + ts.Error + "）"
		}
		fmt.Fprintf(stdout, "  %-7s %s  %s\n", t, mark, ts.File)
	}
	fmt.Fprintln(stdout, "\n最近一次上报")
	if e := r.LastExport; e != nil {
		result := "成功"
		if !e.OK {
			result = "失败：" + e.Error
		}
		fmt.Fprintf(stdout, "  %s  %s %s  %d 轮 %d 个 span  %s\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Tool, e.Event, e.Turns, e.Spans, result)
	} else {
		fmt.Fprintln(stdout, "  还没有上报过")
	}
	if r.Pending > 0 {
		fmt.Fprintf(stdout, "  待重发 %d 个事件（发送失败，下次任一 hook 调用时自动重发）\n", r.Pending)
	}
	fmt.Fprintf(stdout, "\n状态目录  %s\n日志      %s\n", r.StateDir, r.LogFile)
	return 0
}

func cmdTest(args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "读取配置失败：%v\n", err)
		return 1
	}
	if !cfg.Configured() {
		fmt.Fprintln(stderr, "还没有配置：先运行 flashcat-hook install <tool> --endpoint ... --key ... --app ...")
		return 1
	}
	if err := sendProbe(cfg, lastInstalledTool()); err != nil {
		fmt.Fprintf(stdout, "连通测试失败：%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "连通测试成功：已发送 probe 到 %s（应用 %s）\n", cfg.Endpoint, cfg.App)
	return 0
}
