# Codex rollout fixtures

Real Codex rollouts (codex-cli 0.167.0 / 0.169.0, macOS), sanitized. They keep
the `sessions/YYYY/MM/DD/rollout-<time>-<thread id>.jsonl` layout so that
subagent lookup by thread id is exercised.

| File (thread id suffix) | What it covers |
|---|---|
| `2026/11/16/...3268c9.jsonl` | simplest case: one turn, one model call, no tools (VS Code) |
| `2026/11/13/...fb45eb.jsonl` | three turns: complete, `turn_aborted` (interrupted), complete; `exec` custom tool calls with nested `CommandExecution` items |
| `2026/11/15/...6c3295.jsonl` | one turn trimmed from a 9-turn session (lines 0 and 179–219 of the original): MCP calls (`function_call` `js` in namespace `mcp__demo_repl` + `McpToolCall` items), one of them failed (`status: failed`, `isError: true`) |
| `2026/11/16/...abb7d1.jsonl` | `codex exec` parent: one turn with `spawn_agent` + `followup_task` to a subagent |
| `2026/11/16/...b1c24d.jsonl` | its subagent (`source.subagent.thread_spawn`, nickname `Feynman`): two turns, `session_id` = parent's root session |
| `2026/11/13/...84b029.jsonl` | a *forked* subagent (`fork_turns`): starts with its own `session_meta`, then the parent's `session_meta` and replayed parent turn (`metadata.inherited_user_message`, closed by a synthetic `turn_aborted`), then `thread_settings_applied` and its own turn. Its parent rollout is not included |

## Sanitization

Every line was decoded and re-encoded with these rules (the script is not
committed; the rules below are complete):

- **Kept as is**: record and payload `type` values; ids (`id`, `turn_id`,
  `root_turn_id`, `call_id`, `response_id`, `session_id`, `thread_id`,
  `agent_thread_id`, `parent_thread_id`, `forked_from_id`, …); `timestamp`s and
  all numbers (token counts, `*_at_ms`, `create_time`, durations, exit codes);
  booleans; enum-like fields (`role`, `phase`, `status`, `name`, `namespace`,
  `server`, `tool`, `kind`, `reason`, `source`, `thread_source`, `originator`,
  `model`, `effort`, `cli_version`, `model_provider`, `agent_nickname`,
  `agent_role`); `content_item_kinds` lists; `metadata.inherited_user_message`.
  Inside `parsed_cmd`, `command`, `aggregated_output`, `result` and tool
  `description` / `parameters` / `format`, and inside JSON-encoded arguments,
  only `type` values are kept.
- **Replaced by placeholders** `[<key>:<n>]`: every other string — prompts,
  replies, reasoning summaries, developer/base instructions, AGENTS.md, tool
  arguments, tool outputs, command lines, file paths, hashes, account and user
  ids. The same original string always maps to the same placeholder, so e.g.
  `task_complete.last_agent_message` still equals the final answer's text.
  JSON-encoded `arguments` / `output` strings keep their keys; only values are
  replaced.
- **Special cases**: `cwd` and `runtime_workspace_roots` → `/tmp/redacted/project`;
  `git.repository_url` → `example.invalid/org/repo`; `git.branch` →
  `feature-branch`; `git.commit_hash` → forty zeros; `agent_path` →
  `/root/agent_<n>` (consistent across files); `encrypted_content` →
  `[encrypted]`; `FileChange.changes` keys (file paths) → placeholders;
  `world_state` payloads have every string replaced, and their
  `state.tools` map (the MCP tools available on the recording machine; the
  parser never reads `world_state`) has all entries removed, leaving `{}`;
  the only MCP server name left in the fixtures is the synthetic
  `mcp__demo_repl` / `demo_repl` used by the MCP fixture; the harness prefix of
  `exec` outputs (`Script completed`, `Script failed`,
  `Script running with cell ID …`) is kept and the rest replaced.

Checked after sanitizing: no `/Users/`, home or temp paths, `@`, `http(s)://`,
`github.com`, account ids or e-mail addresses remain.
