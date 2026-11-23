package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/config"
	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/state"
)

// sandbox isolates every path the binary touches.
func sandbox(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	for _, e := range []string{config.EnvEndpoint, config.EnvKey, config.EnvApp, config.EnvContent, config.EnvUser, config.EnvDisabled, "TRACEPARENT"} {
		t.Setenv(e, "")
		os.Unsetenv(e)
	}
	old := executablePath
	executablePath = func() (string, error) { return filepath.Join(home, ".flashcat", "bin", "flashcat-hook"), nil }
	t.Cleanup(func() { executablePath = old })
	return home
}

type collector struct {
	status atomic.Int32
	mu     sync.Mutex
	spans  []int
	names  []string
	traces []ptrace.Traces
	ok     []bool  // per request: answered 2xx
	plan   []int32 // statuses for the next requests; then status
}

func newCollector(t *testing.T) (*collector, *httptest.Server) {
	c := &collector{}
	c.status.Store(200)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("not gzip: %v", err)
			return
		}
		raw, _ := io.ReadAll(zr)
		req := ptraceotlp.NewExportRequest()
		_ = req.UnmarshalProto(raw)
		c.mu.Lock()
		code := c.status.Load()
		if len(c.plan) > 0 {
			code, c.plan = c.plan[0], c.plan[1:]
		}
		c.ok = append(c.ok, code >= 200 && code < 300)
		c.traces = append(c.traces, req.Traces())
		c.spans = append(c.spans, req.Traces().SpanCount())
		if req.Traces().SpanCount() > 0 {
			c.names = append(c.names, req.Traces().ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Name())
		}
		c.mu.Unlock()
		w.WriteHeader(int(code))
	}))
	t.Cleanup(s.Close)
	return c, s
}

func (c *collector) requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.spans)
}

// lastSpans returns the spans of the last request.
func (c *collector) lastSpans() []ptrace.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []ptrace.Span
	td := c.traces[len(c.traces)-1]
	for i := 0; i < td.ResourceSpans().Len(); i++ {
		ss := td.ResourceSpans().At(i).ScopeSpans()
		for j := 0; j < ss.Len(); j++ {
			for k := 0; k < ss.At(j).Spans().Len(); k++ {
				out = append(out, ss.At(j).Spans().At(k))
			}
		}
	}
	return out
}

func exec(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func installClaude(t *testing.T, url string) {
	t.Helper()
	code, out, errs := exec(t, "", "install", "claude", "--endpoint", url+"/v1/traces", "--key", "kid_t:secret", "--app", "coding-test", "--user", "dev@example.com")
	if code != 0 {
		t.Fatalf("install exit %d\n%s\n%s", code, out, errs)
	}
}

// plainTranscript copies the plain fixture into dir.
func plainTranscript(t *testing.T) (string, string) {
	t.Helper()
	src, _ := filepath.Glob("../../internal/claude/testdata/plain/*.jsonl")
	b, err := os.ReadFile(src[0])
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), filepath.Base(src[0]))
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, strings.TrimSuffix(filepath.Base(p), ".jsonl")
}

func stopPayload(sid, path string) string {
	b, _ := json.Marshal(map[string]any{"session_id": sid, "transcript_path": path, "cwd": "/work/project", "hook_event_name": "Stop", "stop_hook_active": false})
	return string(b)
}

func TestCursorRepliesFirstWithoutConfig(t *testing.T) {
	sandbox(t)
	cases := map[string]string{
		`{"hook_event_name":"beforeSubmitPrompt","conversation_id":"c"}`: `{"continue":true}`,
		`{"hook_event_name":"subagentStart"}`:                            `{"permission":"allow"}`,
		`{"hook_event_name":"stop","status":"completed"}`:                `{}`,
		`not json`: `{}`,
		``:         `{}`,
	}
	for _, ev := range []string{"afterAgentResponse", "postToolUse", "postToolUseFailure", "subagentStop", "sessionEnd",
		"sessionStart", "afterAgentThought", "afterShellExecution"} {
		cases[`{"hook_event_name":"`+ev+`","conversation_id":"c"}`] = `{}`
	}
	for in, want := range cases {
		code, out, _ := exec(t, in, "hook", "cursor")
		if code != 0 || out != want {
			t.Errorf("hook cursor %q: exit %d out %q, want %q", in, code, out, want)
		}
	}
}

func TestHookAlwaysExitsZero(t *testing.T) {
	sandbox(t)
	for _, args := range [][]string{{"hook"}, {"hook", "vim"}, {"hook", "claude"}, {"hook", "codex"}} {
		if code, out, _ := exec(t, `{"hook_event_name":"Stop","session_id":"s","transcript_path":"/nope"}`, args...); code != 0 || out != "" {
			t.Errorf("%v: exit %d out %q", args, code, out)
		}
	}
	if code, _, _ := exec(t, "garbage", "hook", "claude"); code != 0 {
		t.Error("garbage payload")
	}
	logb, _ := os.ReadFile(logPath())
	if n := strings.Count(string(logb), "\n"); n != 5 {
		t.Errorf("want one log line per call, got %d:\n%s", n, logb)
	}
}

func TestEndToEndClaude(t *testing.T) {
	sandbox(t)
	c, srv := newCollector(t)
	installClaude(t, srv.URL)
	if c.requests() != 1 || c.names[0] != "flashcat-hook probe" {
		t.Fatalf("install probe: %v", c.names)
	}
	target := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json")
	b, _ := os.ReadFile(target)
	if !strings.Contains(string(b), " hook claude") {
		t.Fatalf("settings.json: %s", b)
	}

	path, sid := plainTranscript(t)
	if code, _, _ := exec(t, stopPayload(sid, path), "hook", "claude"); code != 0 {
		t.Fatal("hook exit code")
	}
	if c.requests() != 2 || c.spans[1] == 0 || c.names[1] != "invoke_agent claude-code" {
		t.Fatalf("hook did not send the turn: %v %v", c.spans, c.names)
	}
	// Same Stop again: nothing new.
	exec(t, stopPayload(sid, path), "hook", "claude")
	if c.requests() != 2 {
		t.Fatalf("unchanged transcript re-sent")
	}

	code, out, _ := exec(t, "", "status", "--json")
	var st statusReport
	if code != 0 || json.Unmarshal([]byte(out), &st) != nil {
		t.Fatalf("status --json: %d %s", code, out)
	}
	if !st.Tools["claude"].Registered || st.Tools["codex"].Registered || st.LastExport == nil || !st.LastExport.OK ||
		st.LastExport.Turns != 1 || st.Config.Key != "kid_t:****" || !st.Configured || strings.Contains(out, "secret") {
		t.Fatalf("status: %+v", st)
	}
	if code, out, _ := exec(t, "", "status"); code != 0 || !strings.Contains(out, "已注册 Stop、SessionEnd") {
		t.Fatalf("status text: %s", out)
	}
	logb, _ := os.ReadFile(logPath())
	if !strings.Contains(string(logb), "tool=claude event=Stop turns=1") {
		t.Fatalf("log: %s", logb)
	}
}

func TestDeliveryFailureRetries(t *testing.T) {
	sandbox(t)
	c, srv := newCollector(t)
	installClaude(t, srv.URL)
	path, sid := plainTranscript(t)
	full, _ := os.ReadFile(path)
	// Cut the transcript after its third turn (before the fourth prompt).
	var cutAt, prompts int
	for i := 0; i < len(full); {
		j := bytes.IndexByte(full[i:], '\n')
		if j < 0 {
			break
		}
		if bytes.Contains(full[i:i+j], []byte(`"promptSource":"typed"`)) {
			prompts++
			if prompts == 4 {
				cutAt = i
				break
			}
		}
		i += j + 1
	}
	if cutAt == 0 {
		t.Fatal("fixture has fewer than 4 prompts")
	}
	os.WriteFile(path, full[:cutAt], 0o644)
	c.status.Store(503)
	exec(t, stopPayload(sid, path), "hook", "claude")
	if r := loadExportRecord(); r == nil || r.OK || r.Turns != 1 || !strings.Contains(r.Error, "503") {
		t.Fatalf("export record %+v", r)
	}
	// Two more turns complete, delivery works again: the failed turn is
	// retried together with them.
	os.WriteFile(path, full, 0o644)
	c.status.Store(200)
	exec(t, stopPayload(sid, path), "hook", "claude")
	if r := loadExportRecord(); r == nil || !r.OK || r.Turns != 4 {
		t.Fatalf("retry export record %+v", r)
	}
	n := c.requests()
	exec(t, stopPayload(sid, path), "hook", "claude")
	if c.requests() != n {
		t.Fatal("re-sent after success")
	}
}

func TestCursorPayloadIgnoredByClaude(t *testing.T) {
	sandbox(t)
	c, srv := newCollector(t)
	installClaude(t, srv.URL)
	path, sid := plainTranscript(t)
	var m map[string]any
	_ = json.Unmarshal([]byte(stopPayload(sid, path)), &m)
	m["cursor_version"] = "1.7.0"
	b, _ := json.Marshal(m)
	for _, tool := range []string{"claude", "codex"} {
		if code, out, _ := exec(t, string(b), "hook", tool); code != 0 || out != "" {
			t.Fatalf("%s: exit %d out %q", tool, code, out)
		}
	}
	if c.requests() != 1 {
		t.Fatal("cursor payload was processed")
	}
	if _, err := os.Stat(filepath.Join(config.StateDir(), "claude")); err == nil {
		t.Fatal("state written for a cursor payload")
	}
}

func TestDisabledByEnv(t *testing.T) {
	sandbox(t)
	c, srv := newCollector(t)
	installClaude(t, srv.URL)
	path, sid := plainTranscript(t)
	t.Setenv(config.EnvDisabled, "1")
	exec(t, stopPayload(sid, path), "hook", "claude")
	if c.requests() != 1 {
		t.Fatal("sent while disabled")
	}
}

func TestInstallValidationAndProbeFailure(t *testing.T) {
	sandbox(t)
	if code, _, _ := exec(t, "", "install", "claude", "--endpoint", "https://x/v1/traces", "--app", "a"); code != 2 {
		t.Error("missing key accepted")
	}
	if code, _, _ := exec(t, "", "install", "claude", "--endpoint", "https://x", "--key", "k:s", "--app", "a", "--content", "all"); code != 2 {
		t.Error("bad content accepted")
	}
	if code, _, _ := exec(t, "", "install", "vim"); code != 2 {
		t.Error("unknown tool accepted")
	}
	c, srv := newCollector(t)
	c.status.Store(401)
	code, out, _ := exec(t, "", "install", "codex", "--endpoint", srv.URL, "--key", "k:s", "--app", "coding-x")
	if code != 1 || !strings.Contains(out, "HTTP 401") || !strings.Contains(out, "/hooks") {
		t.Fatalf("probe failure: %d %s", code, out)
	}
	// Hooks are written even when the probe fails.
	b, _ := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json"))
	if !strings.Contains(string(b), " hook codex") {
		t.Fatal("codex hook not written")
	}
	fi, _ := os.Stat(config.Path())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v", fi.Mode())
	}
}

func TestTestCommand(t *testing.T) {
	sandbox(t)
	if code, _, _ := exec(t, "", "test"); code != 1 {
		t.Error("unconfigured test must fail")
	}
	c, srv := newCollector(t)
	installClaude(t, srv.URL)
	if code, out, _ := exec(t, "", "test"); code != 0 || !strings.Contains(out, "成功") {
		t.Fatalf("test: %d %s", code, out)
	}
	c.status.Store(500)
	if code, _, _ := exec(t, "", "test"); code != 1 {
		t.Fatal("failed probe must exit 1")
	}
}

func TestUninstallPurge(t *testing.T) {
	sandbox(t)
	_, srv := newCollector(t)
	installClaude(t, srv.URL)
	if code, _, _ := exec(t, "", "uninstall", "claude"); code != 0 {
		t.Fatal("uninstall")
	}
	if _, err := os.Stat(config.Path()); err != nil {
		t.Fatal("config removed without --purge")
	}
	b, _ := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"))
	if strings.Contains(string(b), "flashcat-hook") {
		t.Fatalf("entry left: %s", b)
	}
	if code, _, _ := exec(t, "", "uninstall", "--purge", "claude"); code != 0 {
		t.Fatal("purge")
	}
	if _, err := os.Stat(config.Path()); !os.IsNotExist(err) {
		t.Fatal("config kept with --purge")
	}
	if _, err := os.Stat(config.StateDir()); !os.IsNotExist(err) {
		t.Fatal("state kept with --purge")
	}
}

func TestLogRotation(t *testing.T) {
	sandbox(t)
	p := logPath()
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, bytes.Repeat([]byte("x"), maxLogBytes+1), 0o600)
	exec(t, `{}`, "hook", "claude")
	fi, err := os.Stat(p)
	if err != nil || fi.Size() > 1024 {
		t.Fatalf("hook.log not rotated: %v %v", fi, err)
	}
	if fi1, err := os.Stat(p + ".1"); err != nil || fi1.Size() <= maxLogBytes {
		t.Fatal("hook.log.1 missing")
	}
}

func TestVersion(t *testing.T) {
	if code, out, _ := exec(t, "", "version"); code != 0 || strings.TrimSpace(out) != version {
		t.Fatalf("version: %d %q", code, out)
	}
}

func installCursor(t *testing.T, url string) {
	t.Helper()
	code, out, errs := exec(t, "", "install", "cursor", "--endpoint", url+"/v1/traces", "--key", "kid_t:secret", "--app", "coding-test")
	if code != 0 {
		t.Fatalf("install exit %d\n%s\n%s", code, out, errs)
	}
}

type cursorEvent struct {
	name, payload string
	p             map[string]any
}

func (e cursorEvent) str(k string) string { s, _ := e.p[k].(string); return s }

// cursorFixture returns the payloads of the sanitized Cursor turn.
func cursorFixture(t *testing.T) []cursorEvent {
	t.Helper()
	b, err := os.ReadFile("../../internal/cursor/testdata/turn_subagent.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out []cursorEvent
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var rec struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		e := cursorEvent{payload: string(rec.Payload)}
		_ = json.Unmarshal(rec.Payload, &e.p)
		e.name = e.str("hook_event_name")
		out = append(out, e)
	}
	return out
}

func cursorStateFiles(t *testing.T) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(config.StateDir(), "cursor", "*.json"))
	return m
}

func spanAttr(sp ptrace.Span, k string) string {
	v, _ := sp.Attributes().Get(k)
	return v.AsString()
}

// A whole Cursor turn through `hook cursor`: replies, the subagent's events
// routed to the parent conversation by parent_agent_id, one export at stop.
func TestEndToEndCursor(t *testing.T) {
	home := sandbox(t)
	c, srv := newCollector(t)
	installCursor(t, srv.URL)
	hooks, _ := os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
	for _, ev := range []string{"beforeSubmitPrompt", "afterAgentResponse", "postToolUse", "postToolUseFailure", "subagentStop", "stop", "sessionEnd"} {
		if !strings.Contains(string(hooks), `"`+ev+`"`) {
			t.Errorf("hooks.json lacks %s", ev)
		}
	}
	if strings.Contains(string(hooks), "subagentStart") {
		t.Error("subagentStart registered")
	}

	var parent, child string
	for _, e := range cursorFixture(t) {
		if e.name == "subagentStop" {
			parent, child = e.str("parent_conversation_id"), e.str("child_conversation_id")
		}
		want := `{}`
		switch e.name {
		case "beforeSubmitPrompt":
			want = `{"continue":true}`
		case "subagentStart":
			want = `{"permission":"allow"}`
		}
		code, out, _ := exec(t, e.payload, "hook", "cursor")
		if code != 0 || out != want {
			t.Fatalf("%s: exit %d reply %q, want %q", e.name, code, out, want)
		}
		if e.name != "stop" && c.requests() != 1 {
			t.Fatalf("exported before stop (at %s)", e.name)
		}
	}
	if c.requests() != 2 || c.names[1] != "invoke_agent cursor" {
		t.Fatalf("requests %d %v", c.requests(), c.names)
	}
	// Events arrive here milliseconds apart (real clock), so the model-call
	// gaps collapse; the exact step layout is tested in internal/cursor.
	spans := c.lastSpans()
	names := map[string]int{}
	tokens := false
	byID := map[string]ptrace.Span{}
	for _, sp := range spans {
		names[sp.Name()]++
		if v, ok := sp.Attributes().Get("gen_ai.usage.input_tokens"); ok && v.Int() == 62537 &&
			spanAttr(sp, "gen_ai.provider.name") == "xai" && sp.Name() == "chat grok-4.7" {
			tokens = true
		}
		byID[sp.SpanID().String()] = sp
		if got := spanAttr(sp, "gen_ai.conversation.id"); got != parent {
			t.Errorf("%s: conversation %q, want the parent %q", sp.Name(), got, parent)
		}
		if spanAttr(sp, "user.id") != "dev@example.com" {
			t.Errorf("%s: user %q", sp.Name(), spanAttr(sp, "user.id"))
		}
	}
	if names["invoke_agent cursor"] != 1 || names["execute_tool Grep"] != 1 || names["execute_tool Read"] != 2 ||
		names["execute_tool Task"] != 1 || names["invoke_agent explore"] != 1 || !tokens {
		t.Fatalf("spans %v (turn tokens on a grok chat span: %v)", names, tokens)
	}
	shells := 0
	for _, sp := range spans {
		if sp.Name() != "execute_tool Shell" {
			continue
		}
		shells++
		sub := byID[sp.ParentSpanID().String()]
		if sub.Name() != "invoke_agent explore" || byID[sub.ParentSpanID().String()].Name() != "execute_tool Task" {
			t.Errorf("Shell span not under the explore subagent under Task: parent %q", sub.Name())
		}
	}
	if shells != 2 {
		t.Fatalf("Shell spans = %d", shells)
	}

	// One state file, the parent's: none for the subagent or the draft.
	files := cursorStateFiles(t)
	if len(files) != 1 || filepath.Base(files[0]) != state.Key(parent)+".json" || state.Key(child) == state.Key(parent) {
		t.Fatalf("state files %v", files)
	}
	logb, _ := os.ReadFile(logPath())
	if !strings.Contains(string(logb), "tool=cursor event=stop turns=1") || strings.Count(string(logb), `note="draft conversation ignored"`) != 1 {
		t.Fatalf("log:\n%s", logb)
	}

	// sessionEnd with nothing left clears the state.
	end, _ := json.Marshal(map[string]any{"hook_event_name": "sessionEnd", "session_id": parent, "conversation_id": parent,
		"reason": "window_close", "cursor_version": "3.29.9"})
	if _, out, _ := exec(t, string(end), "hook", "cursor"); out != `{}` || c.requests() != 2 || len(cursorStateFiles(t)) != 0 {
		t.Fatalf("sessionEnd: %q, %d requests, state %v", out, c.requests(), cursorStateFiles(t))
	}
}

// A failed export keeps the turn's events and queues the stop; the next hook
// call that can deliver (here the next prompt, which sends nothing itself)
// replays it, and the turn is delivered once.
func TestCursorDeliveryFailureRetries(t *testing.T) {
	sandbox(t)
	c, srv := newCollector(t)
	installCursor(t, srv.URL)
	var parent, gen string
	for _, e := range cursorFixture(t) {
		if e.name == "beforeSubmitPrompt" {
			parent = e.str("conversation_id")
		}
		if e.name == "stop" {
			c.status.Store(503)
			gen = e.str("generation_id")
		}
		exec(t, e.payload, "hook", "cursor")
	}
	if r := loadExportRecord(); r == nil || r.OK || r.Turns != 1 {
		t.Fatalf("export record %+v", r)
	}
	if n := (state.Store{Dir: config.StateDir()}).PendingCount(); n != 1 {
		t.Fatalf("pending = %d", n)
	}
	c.status.Store(200)
	g2 := "00000000-0000-4000-8000-000000000002"
	for i, ev := range []map[string]any{
		{"hook_event_name": "beforeSubmitPrompt", "prompt": "again"},
		{"hook_event_name": "stop", "status": "completed", "input_tokens": 10, "output_tokens": 2},
	} {
		ev["conversation_id"], ev["session_id"], ev["generation_id"], ev["cursor_version"] = parent, parent, g2, "3.29.9"
		b, _ := json.Marshal(ev)
		exec(t, string(b), "hook", "cursor")
		if i == 0 {
			if r := loadExportRecord(); r == nil || !r.OK || r.Turns != 1 || r.Event != "stop (retry)" {
				t.Fatalf("replay export record %+v", r)
			}
		}
	}
	if r := loadExportRecord(); r == nil || !r.OK || r.Turns != 1 || r.Event != "stop" {
		t.Fatalf("second turn export record %+v", r)
	}
	if got := c.deliveredRoots("gen_ai.conversation.id", parent); got != 2 {
		t.Fatalf("delivered %d turns of the conversation, want 2 (each once)", got)
	}
	n := c.requests()
	b, _ := json.Marshal(map[string]any{"hook_event_name": "stop", "status": "completed", "conversation_id": parent,
		"session_id": parent, "generation_id": g2, "cursor_version": "3.29.9"})
	exec(t, string(b), "hook", "cursor")
	if c.requests() != n || (state.Store{Dir: config.StateDir()}).PendingCount() != 0 {
		t.Fatal("re-sent after success")
	}
	_ = gen
}

// Cursor's placeholder "draft-…" conversation is ignored after the reply.
func TestCursorDraftIgnored(t *testing.T) {
	sandbox(t)
	c, srv := newCollector(t)
	installCursor(t, srv.URL)
	for _, in := range []string{
		`{"hook_event_name":"beforeSubmitPrompt","conversation_id":"draft-1","session_id":"draft-1","generation_id":"g","prompt":"p","cursor_version":"3.29.9"}`,
		`{"hook_event_name":"stop","conversation_id":"draft-1","generation_id":"g","status":"completed","cursor_version":"3.29.9"}`,
		`{"hook_event_name":"sessionEnd","session_id":"draft-1","reason":"window_close","cursor_version":"3.29.9"}`,
	} {
		want := `{}`
		if strings.Contains(in, "beforeSubmitPrompt") {
			want = `{"continue":true}`
		}
		if code, out, _ := exec(t, in, "hook", "cursor"); code != 0 || out != want {
			t.Fatalf("exit %d reply %q", code, out)
		}
	}
	if c.requests() != 1 || len(cursorStateFiles(t)) != 0 {
		t.Fatalf("draft processed: %d requests, state %v", c.requests(), cursorStateFiles(t))
	}
	logb, _ := os.ReadFile(logPath())
	if n := strings.Count(string(logb), `note="draft conversation ignored"`); n != 3 {
		t.Fatalf("draft notes = %d:\n%s", n, logb)
	}
}

// deliveredRoots counts root spans with attribute k=v in requests answered 2xx.
func (c *collector) deliveredRoots(k, v string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for i, td := range c.traces {
		if !c.ok[i] {
			continue
		}
		for r := 0; r < td.ResourceSpans().Len(); r++ {
			ss := td.ResourceSpans().At(r).ScopeSpans()
			for j := 0; j < ss.Len(); j++ {
				for x := 0; x < ss.At(j).Spans().Len(); x++ {
					sp := ss.At(j).Spans().At(x)
					if got, ok := sp.Attributes().Get(k); ok && sp.ParentSpanID().IsEmpty() && got.AsString() == v {
						n++
					}
				}
			}
		}
	}
	return n
}

// claudeSession copies the plain fixture under a new session id.
func claudeSession(t *testing.T, sid string) string {
	t.Helper()
	src, _ := filepath.Glob("../../internal/claude/testdata/plain/*.jsonl")
	b, err := os.ReadFile(src[0])
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), sid+".jsonl")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func pendingStore() state.Store { return state.Store{Dir: config.StateDir()} }

// One-shot session A fails to deliver; an unrelated session B that delivers
// replays A, whose turn arrives exactly once.
func TestCrossSessionRetry(t *testing.T) {
	sandbox(t)
	c, srv := newCollector(t)
	installClaude(t, srv.URL)
	pathA, pathB := claudeSession(t, "session-a"), claudeSession(t, "session-b")

	c.status.Store(503)
	exec(t, stopPayload("session-a", pathA), "hook", "claude")
	pe, err := pendingStore().LoadPending("claude", "session-a")
	if err != nil || pe == nil || pe.Attempts != 1 || pe.Tool != "claude" || !strings.Contains(string(pe.Payload), pathA) {
		t.Fatalf("pending entry %+v %v", pe, err)
	}
	_, out, _ := exec(t, "", "status", "--json")
	var st statusReport
	if json.Unmarshal([]byte(out), &st) != nil || st.Pending != 1 {
		t.Fatalf("status pending: %s", out)
	}

	c.status.Store(200)
	exec(t, stopPayload("session-b", pathB), "hook", "claude")
	if a, b := c.deliveredRoots("gen_ai.conversation.id", "session-a"), c.deliveredRoots("gen_ai.conversation.id", "session-b"); a != 1 || b != 1 {
		t.Fatalf("delivered A=%d B=%d, want 1 each", a, b)
	}
	if pendingStore().PendingCount() != 0 {
		t.Fatal("pending entry left after a successful retry")
	}
	logb, _ := os.ReadFile(logPath())
	if !strings.Contains(string(logb), `note="pending: sent 1 turns"`) {
		t.Fatalf("log:\n%s", logb)
	}

	// Nothing more for A on later calls.
	n := c.requests()
	exec(t, stopPayload("session-b", pathB), "hook", "claude")
	exec(t, stopPayload("session-a", pathA), "hook", "claude")
	if c.requests() != n || c.deliveredRoots("gen_ai.conversation.id", "session-a") != 1 {
		t.Fatal("A delivered again")
	}
}

// Replays stop at the first failed delivery, back off, and respect the budget.
func TestRetryStopsOnFailureAndBudget(t *testing.T) {
	sandbox(t)
	c, srv := newCollector(t)
	installClaude(t, srv.URL)
	pathA, pathB, pathC := claudeSession(t, "session-a"), claudeSession(t, "session-b"), claudeSession(t, "session-c")

	c.status.Store(503)
	exec(t, stopPayload("session-a", pathA), "hook", "claude")
	exec(t, stopPayload("session-c", pathC), "hook", "claude")
	// A failed live delivery does not trigger replays.
	if c.requests() != 3 { // probe + A + C
		t.Fatalf("requests = %d", c.requests())
	}

	// B delivers; the replay of A (oldest) fails; C is not tried.
	c.status.Store(200)
	c.mu.Lock()
	c.plan = []int32{200, 503}
	c.mu.Unlock()
	exec(t, stopPayload("session-b", pathB), "hook", "claude")
	if c.requests() != 5 {
		t.Fatalf("requests = %d, want B + one failed replay", c.requests())
	}
	a, _ := pendingStore().LoadPending("claude", "session-a")
	cc, _ := pendingStore().LoadPending("claude", "session-c")
	if a == nil || a.Attempts != 2 || !a.NextAttempt.After(time.Now()) || cc == nil || cc.Attempts != 1 {
		t.Fatalf("after failed replay: A=%+v C=%+v", a, cc)
	}

	// Backing off: the next delivering call does not touch A, but tries C.
	exec(t, stopPayload("session-b", pathB), "hook", "claude") // nothing new for B
	if c.deliveredRoots("gen_ai.conversation.id", "session-c") != 1 || c.deliveredRoots("gen_ai.conversation.id", "session-a") != 0 {
		t.Fatal("backoff not respected")
	}

	// No budget left: nothing is replayed.
	oldBudget, oldBackoff := retryBudget, retryBackoff
	retryBudget, retryBackoff = 0, func(int) time.Duration { return 0 }
	t.Cleanup(func() { retryBudget, retryBackoff = oldBudget, oldBackoff })
	a.NextAttempt = time.Time{}
	_ = pendingStore().SavePending(*a)
	n := c.requests()
	exec(t, stopPayload("session-b", pathB), "hook", "claude")
	if c.requests() != n {
		t.Fatal("replayed without budget")
	}
	retryBudget = oldBudget
	exec(t, stopPayload("session-b", pathB), "hook", "claude")
	if c.deliveredRoots("gen_ai.conversation.id", "session-a") != 1 || pendingStore().PendingCount() != 0 {
		t.Fatal("A not delivered once the budget allows")
	}
}

func TestPendingExpiry(t *testing.T) {
	sandbox(t)
	c, srv := newCollector(t)
	installClaude(t, srv.URL)
	pathA, pathB, pathC := claudeSession(t, "session-a"), claudeSession(t, "session-b"), claudeSession(t, "session-c")
	st := pendingStore()
	_ = st.SavePending(state.Pending{Tool: "claude", Key: "session-a", Received: time.Now().Add(-8 * 24 * time.Hour), Attempts: 1, Payload: json.RawMessage(stopPayload("session-a", pathA))})
	_ = st.SavePending(state.Pending{Tool: "claude", Key: "session-c", Received: time.Now(), Attempts: 50, Payload: json.RawMessage(stopPayload("session-c", pathC))})
	exec(t, stopPayload("session-b", pathB), "hook", "claude")
	if st.PendingCount() != 0 {
		t.Fatal("expired entries kept")
	}
	if c.deliveredRoots("gen_ai.conversation.id", "session-a")+c.deliveredRoots("gen_ai.conversation.id", "session-c") != 0 {
		t.Fatal("expired entries were sent")
	}
	logb, _ := os.ReadFile(logPath())
	if strings.Count(string(logb), "pending: dropped a claude event") != 2 || !strings.Contains(string(logb), `note="pending: dropped 2"`) {
		t.Fatalf("log:\n%s", logb)
	}
}

// A codex exec run (one turn, no later event) is delivered by another
// session's hook call after its own delivery failed.
func TestCodexOneShotRetry(t *testing.T) {
	sandbox(t)
	c, srv := newCollector(t)
	code, out, errs := exec(t, "", "install", "codex", "--endpoint", srv.URL+"/v1/traces", "--key", "kid_t:secret", "--app", "coding-test", "--user", "dev@example.com")
	if code != 0 {
		t.Fatalf("install codex: %s %s", out, errs)
	}
	src, _ := filepath.Glob("../../internal/codex/testdata/sessions/2026/11/16/rollout-2026-11-16T14-04-06-*.jsonl")
	b, _ := os.ReadFile(src[0])
	rollout := filepath.Join(t.TempDir(), filepath.Base(src[0]))
	os.WriteFile(rollout, b, 0o644)
	var meta struct {
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(b[:bytes.IndexByte(b, '\n')], &meta)
	var turnID string
	for _, l := range bytes.Split(b, []byte("\n")) {
		var rec struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal(l, &rec) == nil && rec.Type == "event_msg" && rec.Payload["type"] == "task_started" {
			turnID, _ = rec.Payload["turn_id"].(string)
		}
	}
	stop, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": meta.Payload.ID, "turn_id": turnID,
		"transcript_path": rollout, "cwd": "/tmp", "model": "gpt-5.5"})

	c.status.Store(503)
	exec(t, string(stop), "hook", "codex")
	if pendingStore().PendingCount() != 1 {
		t.Fatal("codex failure not queued")
	}
	c.status.Store(200)
	exec(t, stopPayload("session-b", claudeSession(t, "session-b")), "hook", "claude")
	delivered := 0
	c.mu.Lock()
	for i, td := range c.traces {
		if c.ok[i] && td.SpanCount() > 0 && td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Name() == "invoke_agent codex" {
			delivered++
		}
	}
	c.mu.Unlock()
	if delivered != 1 || pendingStore().PendingCount() != 0 {
		t.Fatalf("codex turn delivered %d times, pending %d", delivered, pendingStore().PendingCount())
	}
	// Replaying the same Stop again is harmless.
	n := c.requests()
	exec(t, string(stop), "hook", "codex")
	if c.requests() != n {
		t.Fatal("codex turn re-sent")
	}
}

func TestRetryBackoff(t *testing.T) {
	if retryBackoff(2) != 15*time.Second || retryBackoff(3) != 30*time.Second || retryBackoff(40) != 15*time.Minute {
		t.Fatalf("backoff %v %v %v", retryBackoff(2), retryBackoff(3), retryBackoff(40))
	}
}
