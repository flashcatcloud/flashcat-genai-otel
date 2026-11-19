"""Test automatic Flashcat instrumentation for MCP 1.x and gen_ai.* enrichment of built-in MCP 2.x client spans (contract §4.3)."""

from __future__ import annotations

import json


def env(collector) -> dict:
    return {"FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "k:s"}


def test_native_since_decides_by_version():
    from flashcat.otel._catalog import CATALOG
    from flashcat.otel._loader import is_native

    mcp = next(e for e in CATALOG if e.lib == "mcp")
    assert not is_native(mcp, "1.30.0")
    assert is_native(mcp, "2.0.0") and is_native(mcp, "2.3.0")
    assert not is_native(mcp, None)


def test_init_loads_mcp_instrumentation_end_to_end(collector, run_script):
    diag = run_script(
        """
        import asyncio, json
        from flashcat import otel
        diag = otel.init(service_name="kefu-agent", capture_content="span", metrics=False)

        from mcp.server.fastmcp import FastMCP
        from mcp.shared.memory import create_connected_server_and_client_session
        app = FastMCP("orders")

        @app.tool()
        def lookup_order(phone: str) -> dict:
            return {"phone": phone, "status": "shipped"}

        @otel.agent("kefu")
        async def handle():
            async with create_connected_server_and_client_session(app._mcp_server) as client:
                return await client.call_tool("lookup_order", {"phone": "13812345678"})

        asyncio.run(handle())
        otel.flush()
        print("RESULT:" + json.dumps(otel.diagnostics(), default=str))
        """,
        env(collector),
    )
    mcp = next(i for i in diag["instrumentations"] if i["lib"] == "mcp")
    assert mcp["source"] == "flashcat" and mcp["status"] == "installed"
    spans = collector.spans()
    agent = next(s for s in spans if s["attrs"].get("gen_ai.operation.name") == "invoke_agent")
    tools = [s for s in spans if s["name"] == "tools/call lookup_order"]
    assert {s["kind"] for s in tools} == {3, 2}, "Expected a CLIENT span and a SERVER span"
    client = next(s for s in tools if s["kind"] == 3)
    assert client["parent"] == agent["span_id"]
    for s in tools:
        assert s["attrs"]["gen_ai.tool.name"] == "lookup_order"
        assert s["attrs"]["gen_ai.operation.name"] == "execute_tool"
        args = s["attrs"]["gen_ai.tool.call.arguments"]
        assert "13812345678" not in args and "[REDACTED:phone]" in args, "Tool arguments should be masked"


def test_native_mcp2_client_span_gets_genai_attributes(collector, run_script):
    run_script(
        """
        from opentelemetry import trace
        from opentelemetry.trace import SpanKind
        from flashcat import otel
        otel.init(service_name="x", metrics=False)
        tracer = trace.get_tracer("mcp-python-sdk")
        # This MCP 2.x client span sets only mcp.method.name and jsonrpc.request.id.
        with tracer.start_as_current_span("MCP send tools/call lookup_order", kind=SpanKind.CLIENT,
                                          attributes={"mcp.method.name": "tools/call", "jsonrpc.request.id": "1"}):
            pass
        with tracer.start_as_current_span("MCP send tools/list", kind=SpanKind.CLIENT,
                                          attributes={"mcp.method.name": "tools/list"}):
            pass
        otel.flush()
        """,
        env(collector),
    )
    spans = {s["name"]: s for s in collector.spans()}
    call = spans["MCP send tools/call lookup_order"]["attrs"]
    assert call["gen_ai.operation.name"] == "execute_tool" and call["gen_ai.tool.name"] == "lookup_order"
    assert "gen_ai.operation.name" not in spans["MCP send tools/list"]["attrs"], "Only tools/call should be enriched"
    assert json.dumps(call)  # Attributes can be serialized.
