# Cursor hook event fixtures

`turn_subagent.jsonl` is one real turn from Cursor 3.29.9 (macOS, local IDE
agent, model `grok-4.7`), rebuilt from a capture that recorded only the
*shape* of each hook payload: field names, types, ids, enums and numbers, with
every free-text string replaced by its length. Every hook event was registered
for the capture (including ones flashcat-hook does not install, such as
`sessionStart`, `afterAgentThought`, `subagentStart` and
`afterShellExecution`).

One line per hook call: `{"received_at": <unix seconds>, "payload": {...}}`.
`received_at` is when the hook process ran; the tests use it as
`HookEvent.Now`.

## What it covers

Parent conversation P, generation G, one `explore` subagent with its own
conversation C and generation:

```
sessionStart        draft-… conversation (empty workspace)  -> ignored
sessionStart        P
beforeSubmitPrompt  P/G
afterAgentThought   ×2, duplicated (one generation id has a "-0-xxxx" suffix)
postToolUse         Grep (duration 5.013 ms)
afterAgentThought   ×2
postToolUseFailure  Read (failure_type "error")
subagentStart       subagent_id == tool_call_id == the Task tool_use_id
postToolUseFailure  Read
  child events (conversation C, parent_agent_id P, tool events carry
  parent_tool_call_id = the Task tool_use_id):
  afterAgentThought ×2, afterShellExecution, postToolUse Shell (1801.938 ms),
  afterAgentThought ×2, afterShellExecution, postToolUse Shell (96.887 ms),
  afterAgentThought ×2
postToolUse         Task (duration 10199 ms) — arrives after the child events
subagentStop        status completed, duration_ms 10206, child_conversation_id C
afterAgentThought   ×2
afterAgentResponse  text + input/output/cache tokens (62537 / 1209 / 30464 / 0)
stop                status completed, the same token totals
```

## Sanitization

Kept as captured: event order, `received_at` offsets (shifted to a base of
1790000000), all numbers and booleans (durations, token counts, loop and
message counts), enum-like values (`hook_event_name`, `model`, `model_id`,
`status`, `failure_type`, `tool_name`, `subagent_type`, `composer_mode`,
`cursor_version`, `subagent_model`) and the structure of ids, including the
`\n` inside `tool_use_id`.

Replaced:

- **ids**: every UUID was replaced by a fresh random one, with one mapping for
  the whole file, so links survive: `subagent_id` == `tool_call_id` == Task
  `tool_use_id` == child `parent_tool_call_id`; `child_conversation_id` ==
  the child events' `conversation_id`; their `parent_agent_id` == P; thought
  generation ids keep their `-<n>-<4 chars>` suffix.
- **free text** (`prompt`, `text`, `task`, `description`, `command`, `output`,
  `error_message`, tool input values, `model_params` entries) became
  `[<field>:<original length>]`, so equal-length strings of one field stay
  equal (the Task `prompt` and the subagent `task` share the
  `[prompt:217]` placeholder; a thought and its duplicate share theirs).
- **tool_output** (a JSON string in Cursor) was rebuilt from the captured key
  structure with the same placeholders; the Task output's `agentId` is set to
  the child conversation id (the capture showed only a 36-character string).
- **paths**: workspace roots and file paths → `/tmp/redacted/project…`;
  `transcript_path` → `/tmp/redacted/home/.cursor/projects/…`; `user_email`
  → `dev@example.com`.

The capture script and the sanitizer are not committed; the rules above are
complete.
