# flashcat-hook contract

`flashcat-hook` is a command-line program that runs as a hook in Claude Code, Codex, and Cursor. It reads local session transcripts or hook events, creates spans using OTel GenAI semantic conventions, and sends them to the Flashcat ingest gateway over OTLP.

If the implementation differs from this document, this document takes precedence.

## 0. Directory layout and responsibilities

```
hook/
  CONTRACT.md            This document (frozen)
  go.mod                 module github.com/flashcatcloud/flashcat-genai-otel/hook
  cmd/flashcat-hook/     main dispatches subcommands
  internal/model/        Data structures shared by the parsers and core (frozen)
  internal/config/       Configuration file, environment variables, identity (Git email)
  internal/state/        Per-session state files and file locks
  internal/otlp/         Turn → ptrace, ID derivation, content levels, truncation, sending, probe
  internal/install/      Adds or removes hook configuration for the three tools
  internal/claude/       Claude Code parser (model.Parser)
  internal/codex/        Codex parser (model.Parser)
  internal/cursor/       Cursor parser (model.Parser)
  scripts/               release.sh, install.sh.tmpl, install.ps1.tmpl
  README.md              User documentation
```

The parsers depend only on the standard library and `internal/model`. They make no network requests and write no files directly. They return state through `CollectResult`.

## 1. Command line

| Subcommand | Description |
|---|---|
| `install <claude\|codex\|cursor> --endpoint <url> --key <keyID:secret> --app <app name> [--content none\|no_tool_content\|full] [--user <email>]` | Writes the configuration file (permissions 600), merges hook entries into the tool's configuration, then sends a probe and prints the result to the terminal (success, or the HTTP status code and reason). Rerunning it overwrites the configuration without adding duplicate hooks |
| `uninstall <tool>` | Removes only the Flashcat hook entries. The configuration file is kept because the other two tools may still use it. Add `--purge` to delete the configuration and state |
| `status [--json]` | Shows the configuration (with the key masked), hook registration for each of the three tools, the time and result of the last export, and the log path |
| `test` | Sends a probe using the current configuration and prints the result. Exits with 0 on success or 1 on failure |
| `hook <tool>` | Called by the tool. Reads one JSON event from stdin. **Always exits with 0.** Errors go only to the log. Cursor requires a response on stdout first (§3.3) |
| `version` | Shows the version |

The `FLASHCAT_HOOK_ENDPOINT`, `FLASHCAT_HOOK_KEY`, `FLASHCAT_HOOK_APP`, `FLASHCAT_HOOK_CONTENT`, and `FLASHCAT_HOOK_USER` environment variables can supply `install` arguments. They also override the configuration file at runtime, which is useful in CI. `FLASHCAT_HOOK_DISABLED=1` disables export.

## 2. Local files

| Purpose | macOS / Linux | Windows |
|---|---|---|
| Configuration | `$XDG_CONFIG_HOME/flashcat/hook.json`, defaults to `~/.config/flashcat/hook.json` | `%APPDATA%\flashcat\hook.json` |
| State, locks, and logs | `$XDG_STATE_HOME/flashcat-hook/`, defaults to `~/.local/state/flashcat-hook/` | `%LOCALAPPDATA%\flashcat-hook\` |
| Binary (placed by install.sh) | `~/.flashcat/bin/flashcat-hook` | `%USERPROFILE%\.flashcat\bin\flashcat-hook.exe` |

Configuration file

```json
{ "endpoint": "https://ingest.example.com/v1/traces", "key": "kid_xxx:secret", "app": "coding-platform",
  "content": "none", "user": "", "enabled": true }
```

- State is stored at `<state>/<tool>/<sha256(session or transcript_path)[:16]>.json`, using the session or transcript path as the hash input. The file contains opaque bytes returned by the parser. A `.lock` file in the same directory provides locking (`github.com/gofrs/flock`).
- Pending replay. When a session's export fails, save the hook event to `<state>/pending/<tool>-<sha256(key)[:16]>.json`, keeping only the latest event per session. Any subsequent hook invocation that processes its current event and sends it successfully (or has nothing to send) replays these events in chronological order. The total budget is about 2 seconds, and replay stops on failure. The backoff for a failed replay starts at 15 seconds and doubles up to 15 minutes. Drop and log events older than 7 days or with 50 failed attempts. This preserves turns from one-shot sessions such as `codex exec` and `claude -p` when the network is down. `status` shows the number of pending events.
- Logs go to `<state>/hook.log`, which rotates to `hook.log.1` above 5 MB. Each invocation logs one summary line with the tool, event name, number of turns exported, duration, and error.

## 3. Writing tool configuration

General rules

1. **Merge changes and preserve formatting.** Use `tidwall/sjson` to append or remove entries from the original JSON without reformatting the whole file. Create the file if it is missing.
2. **Back up first.** Copy the original to `<file>.bak-<YYYYmmddHHMMSS>` before editing.
3. **Append only at the end of the array and preserve the order of existing entries.** Codex tracks trust by array position (`…:hooks/hooks.json:stop:0:0`). Inserting entries before existing ones causes the user's hooks to lose trust. Uninstall removes only Flashcat entries.
4. **Make writes idempotent.** Skip adding the command string if it is already present. Identify Flashcat entries by `flashcat-hook hook <tool>` in the command.
5. Commands use the absolute path to the binary. Quote the path if it contains spaces.

### 3.1 Claude Code

The file is `${CLAUDE_CONFIG_DIR:-~/.claude}/settings.json`. The events are `Stop` and `SessionEnd`.

```json
{ "hooks": { "Stop": [ { "hooks": [ { "type": "command", "command": "/Users/a/.flashcat/bin/flashcat-hook hook claude" } ] } ],
             "SessionEnd": [ { "hooks": [ { "type": "command", "command": "/Users/a/.flashcat/bin/flashcat-hook hook claude" } ] } ] } }
```

### 3.2 Codex

The file is `${CODEX_HOME:-~/.codex}/hooks.json`, with the same format as above, and the event is `Stop`. At the end of `install`, the user is prompted to run `/hooks` in Codex to trust the Flashcat hook and to trust it again after an upgrade. `codex exec` silently skips untrusted hooks. **Installation leaves `config.toml` intact.**

### 3.3 Cursor

The file is `~/.cursor/hooks.json`.

```json
{ "version": 1, "hooks": {
    "beforeSubmitPrompt": [ { "command": "/Users/a/.flashcat/bin/flashcat-hook hook cursor" } ],
    "afterAgentResponse": [ ... ], "postToolUse": [ ... ], "postToolUseFailure": [ ... ],
    "subagentStop": [ ... ], "stop": [ ... ], "sessionEnd": [ ... ] } }
```

If the file is missing, create it with `"version": 1`. Cursor watches the file and reloads it automatically when it changes.

Skip registering `subagentStart` because it is a permissions hook and an incorrect response blocks the subagent. `subagentStop` supplies the required type and ID. Remove any entries older versions added to `subagentStart` during `install` or `uninstall`.

**Response. `hook cursor` must write to stdout before any other processing. Invalid JSON blocks the action.**

| Event | stdout |
|---|---|
| `beforeSubmitPrompt` | `{"continue":true}` |
| `subagentStart` | `{"permission":"allow"}` |
| Other | `{}` |

### 3.4 Identifying the event source

Cursor also runs hooks configured in the Claude Code format. If the event JSON contains `cursor_version`, only `hook cursor` processes it. `hook claude` and `hook codex` exit immediately with 0.

## 4. OTLP output

Use pdata to build `ptrace.Traces`, convert it to `ptraceotlp.ExportRequest` protobuf, gzip it, and send it with `POST <endpoint>` and the headers `Content-Type: application/x-protobuf`, `Content-Encoding: gzip`, and `Authorization: Basic base64(key)`. The total timeout is 3 seconds. Success requires a 2xx response and `partial_success.rejected_spans == 0`. State advances only on success. All turns from one hook invocation go into a single request unless it exceeds 4 MiB, in which case it is split.

### 4.1 Resource (same for every request)

| Attribute | Value |
|---|---|
| `service.name` | The configured `app` (such as `coding-platform`). The page generates this value without hard-coding the tool name |
| `service.version` | Tool version (Claude Code `version`, Codex `cli_version`, Cursor `cursor_version`) |
| `telemetry.sdk.name` / `telemetry.sdk.version` | `flashcat-hook` / hook version |
| `flashcat.source` | `coding_agent` marks the data as coming from a coding agent. Probes also require it |
| `flashcat.tag.coding_tool` | `claude-code` / `codex` / `cursor` |
| `host.name` | Hostname |

### 4.2 Attributes on every span (set these on each span, not on the resource)

| Attribute | Value |
|---|---|
| `gen_ai.agent.name` | `claude-code` / `codex` / `cursor` (also used in subagent subtrees) |
| `gen_ai.conversation.id` | `Turn.SessionID` |
| `user.id` | `Turn.UserID` → configured `user` → `git config user.email` (run in `Turn.Cwd`) → empty |
| `flashcat.tag.repo` | `Turn.Repo`. If empty, use the remote URL of the Git repository at `Cwd` with the protocol and `.git` removed. If there is no remote, use the directory name |
| `flashcat.tag.git_branch` | `Turn.GitBranch` |

### 4.3 Span types

| Span | Name | Kind | Attributes |
|---|---|---|---|
| Turn (root) | `invoke_agent <tool>` | INTERNAL | `gen_ai.operation.name=invoke_agent`. `StatusError` and `StatusAborted` set status to ERROR and use `StatusMessage` as the message (`aborted` for an interruption) |
| Model call | `chat <model>` | CLIENT | `gen_ai.operation.name=chat`, `gen_ai.provider.name` (required), `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.response.id`, `gen_ai.response.finish_reasons`, `gen_ai.usage.input_tokens` (**includes cache reads and writes**), `gen_ai.usage.output_tokens`, `gen_ai.usage.cache_read.input_tokens`, `gen_ai.usage.cache_write.input_tokens`, `gen_ai.usage.reasoning.output_tokens`. **Omit `gen_ai.usage.total_tokens`**. When `ReportedCostUSD>0`, set `llm.cost.total` (in USD) |
| Tool call | `execute_tool <name>` | INTERNAL | `gen_ai.operation.name=execute_tool`, `gen_ai.tool.name`, `gen_ai.tool.call.id`. Set status to ERROR on failure |
| Subagent | `invoke_agent <type>` | INTERNAL | The corresponding tool span is the parent. Set `gen_ai.operation.name=invoke_agent` and `flashcat.tag.subagent=<type>`. Its Steps are its child spans |

Omit token attributes with a value of 0 (unknown ≠ 0). If either `input_tokens` or `output_tokens` is nonzero, write both. When `LLMCall.UsageInOtherCall=true`, Cursor reports tokens per turn and records the total usage on the last model call, leaving the other calls without their own usage. In this case, explicitly set `input_tokens=0`, `output_tokens=0`, and `flashcat.usage.aggregated=true`. This marks the call's usage as known and already included in the turn total on the last model call, rather than missing.

### 4.4 Content levels

| Level | What gets written |
|---|---|
| `none` (default) | No content |
| `no_tool_content` | `gen_ai.input.messages` (user input) and `gen_ai.output.messages` (final reply) on root and subagent spans, model call input and output messages (with arguments removed from tool_call and responses removed from tool_call_response), and `gen_ai.system_instructions` |
| `full` | Everything above plus tool arguments and results in model calls and `gen_ai.tool.call.arguments` and `gen_ai.tool.call.result` on tool spans |

Serialize messages as OTel GenAI JSON, `[{"role":"user","parts":[{"type":"text","content":"..."}]}]`. Truncate individual attribute values longer than 20,000 characters and set `flashcat.content_truncated=true` on the span.

### 4.5 IDs and timing

- `trace_id = sha256("<tool>|<SessionID>|<TurnKey>")[:16]`
- For `span_id`, the root uses `sha256(trace_id || "root")[:8]` and a Step uses `sha256(trace_id || "step|" + path)[:8]`. The path is the chain of Keys from the root to the Step (prefix Steps inside a subagent with the parent tool's Key). Resending the same turn produces the same IDs so the backend can deduplicate them.
- If the `TRACEPARENT` environment variable is valid, use its context as the root span's parent and use its trace ID.
- If timing is missing, infer a Step's timing from adjacent Steps with a minimum duration of 1 ms. The root span covers all child spans.

### 4.6 Probe (`test` and the end of `install`)

Send one span named `flashcat-hook probe` with `flashcat.span.kind=onboarding_probe` and `gen_ai.agent.name=<most recently installed tool, or claude-code>`. It uses the resource in §4.1, including `flashcat.source=coding_agent`. The probe verifies connectivity to the ingest gateway and that the key is accepted.

### 4.7 Provider

Claude Code uses `anthropic`. Codex uses `openai` unless `session_meta.model_provider` is set to a nonempty value other than openai, in which case it uses that value. Cursor maps model names as follows. `claude*` → `anthropic`, `gpt*`, `o1*`, `o3*`, `o4*`, and `codex*` → `openai`, `gemini*` → `google`, `grok*` → `xai`, `deepseek*` → `deepseek`, and all others → `cursor`.

## 5. Parsers

### 5.0 Rules shared by all three parsers

- **The first Collect skips historical turns.** When a session or rollout triggers a hook for the first time and has no entry in state, export only the turn that the current hook invocation ended. Mark earlier turns as sent and skip them. Without this rule, installing mid-session could put hundreds of turns in one request, which would keep timing out after 3 seconds and retrying without advancing state. If the initial export fails, `StateOnFailure` marks only the earlier turns as sent so the next invocation retries the current turn.
- **Stop ends the turn.** Claude Code writes `turn_duration` and Codex writes `task_complete` **after** the Stop hook runs. End the turn at Stop without waiting for the end record. Waiting would delay every turn by one hook invocation and leave the final turn unsent.
- Parsers write detailed messages to Go's standard-library `log`. The core routes them to `hook.log`.
- Keep the most recently exported turn in the read window. If more data arrives for it later (notification turns or partially written messages), resend it with the same IDs to replace the original. Cursor discards late events for turns already sent, so a reconstructed partial turn cannot overwrite a complete one (see §5.3).

### 5.1 Claude Code (`internal/claude`)

- Events are `Stop` (skip if `stop_hook_active=true`) and `SessionEnd` (`Final=true`).
- Read `transcript_path` incrementally by byte offset. Leave an incomplete last line for the next read. Treat a shorter file as rotation and reset.
- A turn starts with a `type=user` record containing human input (`origin.kind=="human"`, `promptSource=="typed"`, or `promptSource=="sdk"` for headless `claude -p` or the Agent SDK). Exclude `isCompactSummary` and `isMeta` records and records whose `message.content` consists entirely of `tool_result` entries. Set `TurnKey` to that line's `uuid` unless `promptId` is present, in which case use `promptId`.
- At Stop, a transcript ending in `end_turn` (or an API error) ends the turn (§5.0). If `turn_duration` has yet to be written, check for an agent call that returned `async_launched` and has no corresponding `task-notification`. If one exists, a background agent is still running. Defer export until Stop after the background agent completes or until `Final`. Mark interrupted turns and turns that end without `end_turn` as Aborted. Mark API errors as Error. Skip turns with neither model calls nor tool calls, such as `/model`.
- Merge lines with the same `message.id` into one model call Step (Key=`message.id`) and take usage once. Use `input = input_tokens + cache_read_input_tokens + cache_creation_input_tokens` and `reasoning` = `output_tokens_details.thinking_tokens` if present. The model is `message.model`, and the response ID is `message.id`. The end time is the last line's `timestamp`. The start time is the latest end time of any previous step in the same turn or subagent, including tool results and task-notification. For the first call, use the turn start time. The start time must be no later than the first line.
- Pair the assistant's `tool_use{id,name,input}` with a later user line containing `tool_result{tool_use_id,is_error,content}` (a missing `is_error` is treated as false). Start and End are the times on the tool_use and tool_result lines, respectively.
- Subagent files are `<transcript dir>/<sessionId>/subagents/agent-<id>.jsonl` and `agent-<id>.meta.json`. `meta.toolUseId` matches the parent agent tool call's `tool_use.id`. Parse each subagent file into Steps using the same rules. The entire file is one subagent run. Type is `meta.agentType`.
- Map `version` to `ToolVersion` and `cwd` and `gitBranch` to Turn.
- Skip unknown `type` values and log the count.

### 5.2 Codex (`internal/codex`)

- The event is `Stop`. Its payload contains `turn_id` and `transcript_path`, which points to the rollout file. At Stop, `task_complete` has yet to be written. End the turn using the `turn_id` from Stop and take Output from the last `final_answer`. Ignore the later `task_complete` event, which means `task_complete.error` is unavailable for these turns.
- Read the rollout (`{timestamp,type,payload}` per line) incrementally by byte offset.
- For the session, use `session_meta.payload.session_id` (the root session) as `SessionID`. `id` is the thread ID. Map `cli_version` to `ToolVersion`, `git.branch` / `git.repository_url` and `cwd` to Turn, and `base_instructions.text` to `SystemInstructions`.
- A turn runs from `event_msg/task_started` to `event_msg/task_complete` (OK) or `turn_aborted` (Aborted). `TurnKey` = `turn_id`, and model = the turn's `turn_context.payload.model`. Output = `task_complete.last_agent_message`, and Input is the first user message in the turn with a content type beginning with `user.` (the first `role=user` usually contains AGENTS.md or environment context, so it does not qualify as user input). Normalize `Repo` to `host/org/repo` by removing credentials, the protocol, and `.git`.
- Each `token_usage_record` (deduplicated by `payload.response_id`) produces one model call Step (Key = response_id). Take usage from `payload.usage`, including `input_tokens` (already includes cache tokens) and `output_tokens`. Map `cached_input_tokens` to cache_read, `cache_write_input_tokens` to cache_write, and `reasoning_output_tokens` to reasoning. The Step runs from the end of the previous model call (or turn start) to the record's `timestamp`. Older versions without `token_usage_record` fall back to `event_msg/token_count.info.last_token_usage`.
- Pair `response_item/function_call` and `custom_tool_call` with the corresponding `*_output` by `call_id`. Use `event_msg/item_completed` records for `CommandExecution` (exit_code, status, duration) and `McpToolCall` (`item.id == call_id`, `result.isError`) to fill in duration and outcome. Status `failed`/`declined` or exit_code≠0 means Error. A `custom_tool_call(exec)` runs a script that can execute many commands. Each exec call is one Step, timed from the call to its output. Count it as failed only when the output begins with `Script failed`, regardless of nonzero exit codes from individual commands. Tool errors include only the status word and omit tool output.
- For subagents, `SubAgentActivity(kind=started).id == spawn_agent.call_id`, and `agent_thread_id` identifies `*-<thread_id>.jsonl` in the same directory tree. Attach only subturns that **start within the parent turn's time window** to that spawn call. Attach subturns that start during a later parent turn to that turn's `send_message` / `followup_task` call. A Stop on the child rollout skips standalone export to avoid counting tokens twice. Forked subagents replay the parent session history (the parent's `session_meta`, in-progress parent turn, and `inherited_user_message`). Skip all of it.
- Skip unknown types and count them.

### 5.3 Cursor (`internal/cursor`)

The parser follows events observed in Cursor 3.29.9 (see `internal/cursor/testdata/` for field shapes). Session transcripts lack IDs, timing, usage, and tool results, so **the parser relies entirely on hook events**. Cloud and background agents skip user hooks, so their events are unavailable.

- **Routing (core).** If an event has `parent_agent_id` (subagent events and `subagentStart`/`subagentStop` on the parent session), use it as the state key and `SessionID`. Store subagent events in the parent session's state and put the parent session ID on the spans. Respond to events whose `conversation_id` / `session_id` starts with `draft-` (placeholder sessions for new conversations), then ignore them.
- For **nonterminal events** (`beforeSubmitPrompt`, `afterAgentResponse`, `postToolUse`, `postToolUseFailure`, and `subagentStop`, plus `subagentStart` and `afterAgentThought` if received), retain only the fields needed for parsing and append them to the stored event list in order of receipt (`Now`). Return the updated state for both outcomes (`StateOnSuccess = StateOnFailure = <state after appending>`) and produce no Turn. Ignore other events, such as `sessionStart` and `afterShellExecution`.
- **Deduplication.** Keep one `beforeSubmitPrompt` and one `stop` per generation, and one event per `tool_use_id` / `subagent_id`. For the remaining events, discard identical content received within 1 second. Each `afterAgentThought` arrives twice (one copy has a generation_id with a `-<n>-<4 chars>` suffix and a different model). Strip the suffix before comparing.
- **Turn assignment.** Assign parent session events to turn G by `generation_id`. Subagent events (`parent_agent_id != conversation_id`, with their own session and generation) belong to G when `parent_tool_call_id` matches the Task call's `tool_use_id` in G (= `subagentStop.subagent_id`). For events without `parent_tool_call_id` (thoughts), use a sibling event in the same session or generation, or `subagentStop.child_conversation_id`. If none matches (the turn was interrupted and neither Task's `postToolUse` nor `subagentStop` arrived), assign the event to the turn running at the time.
- **`stop`** ends the turn with `generation_id` = G and retries stored turns that have a stop event and whose last send failed. `TurnKey = generation_id`. Start is the `beforeSubmitPrompt` time. If absent, use the earliest event or tool start time and export as usual. End is the `stop` time. Status mappings are `completed` → OK, `aborted` → Aborted, and `error` → Error. Input = `prompt` and Output = the last `afterAgentResponse.text`. Map `user_email` to `UserID`, `cursor_version` to `ToolVersion`, and `workspace_roots[0]` to `Cwd`.
- **Tools.** Each `postToolUse` / `postToolUseFailure` produces one Step, with Key = CallID = `tool_use_id` (preserve embedded newlines). End is the receipt time and Start = End − `duration` (milliseconds, including fractions). Arguments = `tool_input` (JSON) and Result = `tool_output`. On failure, set Status to Error (Aborted if `is_interrupt=true`) and ErrorMessage to `failure_type` (a status word emitted at every content level). Put `error_message` in Result, which is emitted only at the `full` level.
- **Subagents** are attached to the Task call's Step. Type = `subagentStop.subagent_type` (default `subagent`), ID = `child_conversation_id` (default `subagent_id`), and Input = `task`. Use the Task Step's time window because `subagentStop` arrives after Task's `postToolUse`. Take Status from `subagentStop.status`. Steps are the subagent's tool and model call Steps. If the Task's `postToolUse` is missing after an interruption, synthesize a `Task` Step using timing from `subagentStop` (receipt time − `duration_ms`) or the subagent's events.
- **Model calls.** Cursor lacks per-call data, so split at tool boundaries. Create a Step with Key = `llm-<n>` for each of the following intervals. [Turn start → first tool start], each gap between adjacent tool blocks, and [last tool end → last `afterAgentResponse` (or `stop` if absent)]. Treat overlapping parallel tools and tools less than 500 ms apart as one block, since the gap is hook latency. Split subagent calls the same way within their own time windows. For the model, use `model_id` (or `model` if empty). For the parent turn, take it from `stop` / `afterAgentResponse` / `beforeSubmitPrompt` because the parent session's Task `postToolUse` and `subagentStop` carry the subagent's model. For a subagent, take it from its own events. Determine provider as in §4.7. The first call's input is the prompt (the task for a subagent). Later calls use the previous tool block's results. Output contains the next block's tool calls, with the reply as the last call's output.
- **Tokens.** The `input_tokens`, `output_tokens`, `cache_read_tokens`, and `cache_write_tokens` in `stop` are **totals for the whole turn** (fall back to `afterAgentResponse` if absent from `stop`). Attach them only to the final model call of the parent turn. Treat `input_tokens` as already including cache tokens and add `cache_read + cache_write` only if that sum is greater than the input token count. Map `estimated_cost.raw_cost_cents` to `ReportedCostUSD = cents/100` (this field is absent in 3.29.9). Subagent model calls have no token counts.
- **After sending.** On success, remove the turn's events, including subagent events, from state and record its generation, child generations, and Task call IDs for up to 64 turns. Discard later events for the same generation and duplicate `stop` events to prevent an incomplete turn with the same trace ID from overwriting the original. Also discard events from background subagents (Task returns immediately, `isBackground`) that occur after the parent turn is sent, so they are excluded from the next turn. On failure, `StateOnFailure` holds the event list with the stop event. The next `stop` or `sessionEnd` retries the failed turn along with new turns.
- **`sessionEnd` (`Final`).** Export turns with a stop event using their status and turns without one as Aborted (End is the time of their last event). Then delete the state file. Keep state if sending fails.
- **State limits.** Drop events older than 24 hours and keep at most 2,000. Truncate text fields to 20,001 characters (the core then truncates to 20,000 and marks the truncation). Once tool arguments and results exceed a combined 4 MiB, omit those fields from new events.
- The first Collect rule (§5.0) follows from how state is populated. Events from before installation are absent.

## 6. Releasing

- Use the tag `hook-vX.Y.Z`. The repository contains multiple languages, so releases do not use `releases/latest`.
- `scripts/release.sh <version>` cross-compiles for `darwin/{amd64,arm64}`, `linux/{amd64,arm64}`, and `windows/{amd64,arm64}` (`CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<version>"`). It names the artifacts `flashcat-hook_<os>_<arch>[.exe]`, generates `SHA256SUMS`, and generates `install.sh` / `install.ps1` from templates with the default URL `https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/hook-v<version>`. Output goes to `dist/hook-v<version>/`.
- `install.sh <tool> [install args...] [--base-url <base url>]` accepts an **unversioned** base URL in `--base-url` and appends `/hook-v<version>/`. It detects the platform with `uname -s/-m` (Darwin/Linux, x86_64/amd64/arm64/aarch64), downloads the binary and `SHA256SUMS`, and verifies the checksum (`shasum -a 256` or `sha256sum`). It places the binary in `~/.flashcat/bin/`, runs `chmod +x`, then runs `exec flashcat-hook install <tool> ...`. It uses only POSIX sh.
- `install.ps1` follows the same logic and accepts `-Tool -Endpoint -Key -App [-Content] [-User] [-BaseUrl]`. The page calls it with `& ([scriptblock]::Create((irm '<url>/install.ps1'))) -Tool claude -Endpoint ...`. **Untested on a Windows machine.**
- `.github/workflows/hook-release.yml` runs `release.sh` and uploads the release assets when a `hook-v*` tag is pushed.
