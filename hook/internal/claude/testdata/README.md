# Claude Code parser test data

All four transcripts come from real Claude Code 2.1.311 sessions on the local machine. They were masked with `sanitize/` before being committed.

| Directory | Source | Coverage |
|---|---|---|
| `async/` | A complete session transcript and 2 subagents | Background agents (`toolUseResult.status=async_launched`), `task-notification`, `turn_duration.pendingBackgroundAgentCount`, `isMeta`, multiple lines with the same `message.id`, and `server_tool_use` |
| `sync/` | A complete session transcript and 6 subagents | Foreground agents (`status=completed`), multiple turns, tool results with `is_error=true`, and two models |
| `plain/` | A complete session transcript without subagents | 6 turns, tool calls, local `<command-name>` commands, and `suggestion_accepted` |
| `compact/` | Lines 266–408 of a session transcript | `compact_boundary` and `isCompactSummary` (no new turn) |

## Masking

```bash
go run ./internal/claude/testdata/sanitize -in ~/.claude/projects/<project>/<session>.jsonl -out internal/claude/testdata/<name> [-from N -to M] [-shift D]
```

`sanitize` parses the JSON line by line and processes each value recursively.

- **Keep the original values** for record types and structural fields (including `type`, `subtype`, `promptId`, `sessionId`, `version`, `model`, `id`, `tool_use_id`, `toolUseId`, `stop_reason`, `role`, `origin.kind`, `promptSource`, `agentId`, `agentType`, and `status`), tool names (`tool_use.name`), all numbers outside `cost-state` (usage, durations, and `pendingBackgroundAgentCount`), and booleans (such as `isMeta`, `isCompactSummary`, and `is_error`). Absent fields stay absent, for example when a tool result has no `is_error`.
- **Remap record uuids.** Every uuid-shaped value of `uuid`, `parentUuid`, `logicalParentUuid`, `leafUuid`, `sourceToolAssistantUUID`, `messageId`, and `snapshotMessageId` is replaced with a fresh random uuid. One map is shared by the main transcript and its subagent files, so the same original uuid always becomes the same new uuid and every reference still resolves (a reference to a record outside a `-from`/`-to` range stays dangling, as before).
- **Shift timestamps.** Every `timestamp` (records and `file-history-snapshot.snapshot`) is moved by one offset per run, keeping the format, order and gaps. The offset is random (40–90 days back plus odd seconds and milliseconds) unless `-shift` is given; it is not printed or stored. One run shares its uuid map and offset between the main transcript and its subagents; pass the same `-shift` if several sessions must line up.
- **Synthetic `cost-state`.** The parser ignores these records, but they hold the recorded session's spend and activity. Their numbers are replaced with fixed values (per model: 1000 input / 2000 output / 500 thinking / 30000 cache-read / 4000 cache-creation tokens and `costUSD` 0.25; `totalCostUSD` is the sum; durations 60000/120000/30000 ms; 10 lines added, 5 removed; anything else 0). `startTime` (epoch milliseconds) is shifted with the timestamps.
- **`permissionMode`** is always written as `default`.
- **Replace all other strings with placeholders.** User input, assistant replies, thinking, tool results, descriptions, and command output become `redacted` / `redacted text` / `redacted thinking`. The entire `tool_use.input` value becomes `{"redacted": true}`, `cwd` becomes `/work/project`, and `gitBranch` becomes `main`.
- **Special cases**
  - Keep `[Request interrupted by user…]` verbatim. Claude Code uses it as a hard-coded interruption marker.
  - Keep only `<task-id>`, `<tool-use-id>`, and `<status>` in `task-notification` content. Replace the summary and result with `redacted`. The parser uses `<tool-use-id>` to tell whether a background agent has reported back.
  - Replace object keys that are not identifiers, such as snapshot file paths, with `key1`, `key2`, and so on.
  - To reduce file size, keep only the top-level type, ID, and timestamp in large metadata records ignored by the parser (`api-request*` and `attachment`). Replace the other object fields with `{"redacted": true}`.
  - Keep only the subagent files that correspond to a `tool_use.id` in the selected lines. Mask `meta.json` as well, retaining `agentType` and `toolUseId`.

Before committing, we used `jq -r '.. | strings' | sort -u` to check that the only remaining strings were IDs, timestamps, model names, tool names, and enum values.

The committed fixtures were produced before the uuid, timestamp, `cost-state` and `permissionMode` rules above existed; those four rules were then applied to them in a second pass with the same semantics (one uuid map and one time offset across all four groups).
