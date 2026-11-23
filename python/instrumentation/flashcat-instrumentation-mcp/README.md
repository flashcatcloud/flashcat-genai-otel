# flashcat-instrumentation-mcp

OpenTelemetry instrumentation for the MCP Python SDK 1.x that creates client and server spans for tool calls (`tools/call`).
Typically installed with `pip install 'flashcat-genai-otel[mcp]'` and loaded automatically by `flashcat.genai.init()`.

```python
from flashcat.instrumentation.mcp import MCPInstrumentor
MCPInstrumentor().instrument()   # Call before creating FastMCP / Server
```

- Spans follow the OTel MCP semantic conventions. The span name is `tools/call {tool name}`. On the client, `ClientSession.call_tool` creates a CLIENT span.
  On the server, the handler registered through `Server.call_tool()` (also used by FastMCP) creates a SERVER span. Both set `mcp.method.name`,
  `gen_ai.operation.name=execute_tool`, and `gen_ai.tool.name`.
- When the content capture switch (`OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`) is on, spans include `gen_ai.tool.call.arguments` /
  `gen_ai.tool.call.result`. If the tool returns `isError`, the span gets `error.type=tool_error` and ERROR status. For JSON-RPC errors, the span records the error code.
- The client writes the W3C traceparent into the request's `_meta` using only Trace Context, without baggage. The server extracts it from `_meta`, so both sides share a trace.
  Older versions of `call_tool` without a `meta` parameter skip injection.
- This instrumentation supports mcp 1.x only. mcp 2.x has built-in OTel instrumentation, so flashcat-genai-otel only adds `gen_ai.*` attributes to its client spans and skips loading this instrumentation.
