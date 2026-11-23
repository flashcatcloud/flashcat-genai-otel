# flashcat-hook

Send Claude Code, Codex, and Cursor sessions to Flashcat for AI agent observability. At the end of each turn, a hook reads the coding tool's local session transcript (or hook events for Cursor), creates spans using OTel GenAI semantic conventions, and sends them to the Flashcat ingest gateway over OTLP. The standalone binary runs without Node.js or Python.

> The Claude Code, Codex, and Cursor parsers are currently integrated. Cursor capture is limited to agents running in the local IDE or CLI because cloud and background agents skip user hooks.

## Installation

Copy the installation command from “Connect Claude Code / Codex / Cursor” on the onboarding page. It looks like this.

```bash
curl -fsSL https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/hook-v<version>/install.sh | sh -s -- \
  claude --endpoint https://<gateway>/v1/traces --key <keyID:secret> --app coding-<name>
```

- The first argument selects the tool, `claude`, `codex`, or `cursor`. All three tools can share an application and are distinguished by `agent_name`.
- `--content none|no_tool_content|full` sets the content level. The default is `none` (see below).
- `--user <email>` sets the user identifier. If omitted, it defaults to `git config user.email` (the signed-in email for Cursor).
- `--base-url <url>` sets the **unversioned base URL** for downloads. The script appends `/hook-v<version>/`. For networks in China or internal networks, an administrator can configure this in settings and the onboarding page will include it automatically.

The script detects the OS and CPU (macOS / Linux, x86_64 / arm64), downloads the binary and `SHA256SUMS`, verifies the checksum, places the binary at `~/.flashcat/bin/flashcat-hook`, and runs `flashcat-hook install <tool> ...`.

Windows (**untested on a Windows machine**)

```powershell
& ([scriptblock]::Create((irm 'https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/hook-v<version>/install.ps1'))) `
  -Tool claude -Endpoint https://<gateway>/v1/traces -Key <keyID:secret> -App coding-<name>
```

If you already have the binary, run `flashcat-hook install claude --endpoint ... --key ... --app ...` directly. Rerunning the command overwrites the configuration without adding duplicate hooks. When installing for a second tool, you can omit arguments you already configured.

### After installation

| Tool | Action |
|---|---|
| Claude Code | Restart Claude Code. Only newly opened sessions load the new hooks |
| Codex | Run `/hooks` in Codex and trust the Flashcat hook. **Trust the hook again after upgrading flashcat-hook.** `codex exec` silently skips untrusted hooks |
| Cursor | Cursor watches `~/.cursor/hooks.json` and reloads it automatically. If the hook fails to load, quit Cursor completely and reopen it |

## Installation changes

| File | Changes |
|---|---|
| `~/.config/flashcat/hook.json` (Windows: `%APPDATA%\flashcat\hook.json`) | Endpoint, key, application name, content level, user, and enable switch. Permissions are 600 |
| `${CLAUDE_CONFIG_DIR:-~/.claude}/settings.json` | Appends a `<absolute path to binary> hook claude` entry **to the end** of each of the `hooks.Stop` and `hooks.SessionEnd` arrays |
| `${CODEX_HOME:-~/.codex}/hooks.json` | Appends a `... hook codex` entry to `hooks.Stop`. It leaves `config.toml` intact |
| `~/.cursor/hooks.json` | Appends a `{"command": "... hook cursor"}` entry to each of `beforeSubmitPrompt`, `afterAgentResponse`, `postToolUse`, `postToolUseFailure`, `subagentStop`, `stop`, and `sessionEnd`. If the file is missing, it creates it with `"version": 1` |

- Installation appends entries to the original file, preserving its formatting and existing hooks in their original order. Codex tracks trust by array position. Before editing, installation copies the original file to `<file>.bak-<YYYYmmddHHMMSS>`.
- If the file is a symbolic link (for example, when managed by a dotfiles setup), installation edits the target and preserves the link.
- If the binary path changes, for example after reinstalling elsewhere, installation updates the Flashcat hook command in place.
- Installation writes the key only to `hook.json`.

## Content levels

| Level | Content sent |
|---|---|
| `none` (default) | Structure, duration, model, token counts, tool names, and success or failure only |
| `no_tool_content` | Also includes user input, final replies, and model call input and output messages with tool arguments and results removed |
| `full` | Also includes tool arguments and results |

Individual attributes longer than 20,000 characters are truncated, and the span includes `flashcat.content_truncated=true`.

## Exported data

Each turn produces one trace. Under the root span `invoke_agent claude-code` are model calls (`chat <model>`, with token counts, cached token counts, and provider) and tool calls (`execute_tool <tool name>`). A subagent appears as `invoke_agent <type>` under the corresponding tool span. Each span carries the session ID, `user.id`, repository (`flashcat.tag.repo`, taken from the Git remote URL), and branch. Trace and span IDs are derived deterministically from the session and turn, so resending a turn produces no duplicates.

- **When turns are sent.** Claude Code sends each turn when it ends (`Stop`). If background agents are still running, it waits until all have reported back or sends the turn when the session ends (`SessionEnd`).
- **Installation during an active session.** The first hook invocation sends only the most recent turn. Earlier turns are skipped.
- **Retries.** A send failure leaves the session's progress unchanged. The next hook invocation for that session resends the failed turn along with any new turns. Failed events are also saved in `pending/` under the state directory. After a successful send, a hook invocation from **any** session replays these events in chronological order for up to about 2 seconds. Replay stops on failure, and consecutive failures increase the retry delay. This preserves single-turn sessions such as `codex exec` and `claude -p`. Events older than 7 days or with 50 failed attempts are dropped and logged. `status` shows the number of pending events.
- **Effect on coding tools.** The `hook` subcommand always exits with code 0 and logs any errors. Its total timeout is 3 seconds.

## Commands

| Command | Description |
|---|---|
| `flashcat-hook status [--json]` | Shows the configuration (with the key masked), hook registration for each of the three tools, the time and result of the last export, and the log path |
| `flashcat-hook test` | Sends a test probe using the current configuration. Exits with 0 on success or 1 on failure. A successful probe lights up “Ingest gateway” on the onboarding page |
| `flashcat-hook uninstall <tool> [--purge]` | Removes only the Flashcat hook entries. The configuration is kept because the other two tools may still use it. `--purge` also deletes the configuration and state directory |
| `flashcat-hook version` | Shows the version |

## Environment variables

`FLASHCAT_HOOK_ENDPOINT`, `FLASHCAT_HOOK_KEY`, `FLASHCAT_HOOK_APP`, `FLASHCAT_HOOK_CONTENT`, and `FLASHCAT_HOOK_USER` can supply `install` arguments and also override the configuration file at runtime, which is useful in CI. `FLASHCAT_HOOK_DISABLED=1` disables export. When the `TRACEPARENT` environment variable is valid, the root span of each turn is attached to the caller's trace.

## Local files

| Purpose | macOS / Linux | Windows |
|---|---|---|
| Configuration | `$XDG_CONFIG_HOME/flashcat/hook.json`, defaults to `~/.config/flashcat/hook.json` | `%APPDATA%\flashcat\hook.json` |
| State, locks, and logs | `$XDG_STATE_HOME/flashcat-hook/`, defaults to `~/.local/state/flashcat-hook/` | `%LOCALAPPDATA%\flashcat-hook\` |
| Binary | `~/.flashcat/bin/flashcat-hook` | `%USERPROFILE%\.flashcat\bin\flashcat-hook.exe` |

`hook.log` records one line per hook invocation with the tool, event, number of turns sent, duration, and error. It rotates to `hook.log.1` above 5 MB.

## Troubleshooting

1. **Start with `flashcat-hook status`.** Check whether the hook is registered, the configuration is complete, and the last export succeeded.
2. **The connectivity test fails.** `flashcat-hook test` prints the HTTP status code and reason. For 401 or 403, check the key. For connection failures, check the endpoint and network (proxy or internal network).
3. **The turn ended but no data appeared in Flashcat.**
   - Look for the invocation in `hook.log`. A missing entry means the tool skipped the hook. Restart Claude Code or fully restart Cursor. In Codex, use `/hooks` to trust the hook, and do so again after an upgrade. `codex exec` silently skips untrusted hooks.
   - If there is an entry with `turns=0`, the turn is still waiting for a background agent or contained no model call (for example, a local slash command).
   - If there is an `error=` field, follow the error message. An error starting with `send:` indicates a send failure and is retried automatically on the next invocation.
4. **user_id is empty.** Set `git config --global user.email` or pass `--user` at installation time.
5. **Temporarily disable export** with `export FLASHCAT_HOOK_DISABLED=1`. To remove the hook entirely, use `flashcat-hook uninstall <tool>`.
6. **Windows.** The binary and `install.ps1` are available. Testing on a Windows machine is still pending. Paths containing spaces are quoted in the hook command. How the tools execute that command remains unverified.

## Development

```bash
cd hook
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...

# Convert a session transcript to the OTLP JSON that would be sent (hidden development command, sends nothing).
go run ./cmd/flashcat-hook export claude --transcript ~/.claude/projects/<project>/<session>.jsonl --content none --out /tmp/out.json

# Release artifacts in dist/hook-v<version>/ (6 binaries, install.sh, install.ps1, SHA256SUMS)
sh scripts/release.sh 0.1.0
```

When a `hook-vX.Y.Z` tag is pushed, `.github/workflows/hook-release.yml` runs `release.sh` and uploads the release assets. See [CONTRACT.md](CONTRACT.md) for data structures and parsing rules and [internal/claude/testdata/README.md](internal/claude/testdata/README.md) for how the Claude Code test data was masked.
