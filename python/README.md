# flashcat-genai-otel (Python)

uv workspace.

| Directory | Contents |
|---|---|
| `packages/flashcat-genai-otel` | Thin distribution with `init`, handling for three provider scenarios, export and counters, the content capture switch, masking and truncation, diagnostics, instrumentation catalog loading and conflict detection, and the semantic API |
| `instrumentation/flashcat-instrumentation-dashscope` | Instrumentation for the native DashScope SDK (ported from LoongSuite to the community util-genai package) |
| `instrumentation/flashcat-instrumentation-mcp` | Tool call instrumentation for the MCP Python SDK 1.x (client and server, following the OTel MCP semantic conventions). Version 2.x has built-in instrumentation and needs no separate installation |
| `tools/mock-traffic` | Demo traffic generator |

See the cross-language contract in `../spec/contract.md`.

```bash
uv sync && uv run pytest
```
