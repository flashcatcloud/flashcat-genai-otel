# The flashcat-otel catalog treats MCP 2.x as native and skips this package because MCP has built-in OTel instrumentation.
_instruments = ("mcp >= 1.0, < 2",)
