"""Test MCP 1.x instrumentation with in-memory transport between the client and FastMCP server and an in-memory span exporter."""

from __future__ import annotations

import json

import pytest

from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import SpanKind, StatusCode

from flashcat.instrumentation.mcp import MCPInstrumentor


@pytest.fixture
def instrumented(monkeypatch):
    """Set the content capture environment variable before instrumenting, as flashcat.genai.init does, because setup reads it once."""
    insts = []

    def make(content: str = "SPAN_ONLY") -> InMemorySpanExporter:
        monkeypatch.setenv("OTEL_SEMCONV_STABILITY_OPT_IN", "gen_ai_latest_experimental")
        monkeypatch.setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", content)
        exp = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(exp))
        inst = MCPInstrumentor()
        inst.instrument(tracer_provider=provider)
        insts.append(inst)
        return exp

    yield make
    for inst in insts:
        inst.uninstrument()


@pytest.fixture
def exporter(instrumented):
    return instrumented()


def _server():
    # Instrument before creating the server because FastMCP registers the tools/call handler during construction.
    from mcp.server.fastmcp import FastMCP

    app = FastMCP("orders")

    @app.tool()
    def lookup_order(order_id: str) -> dict:
        """Look up an order."""
        return {"order_id": order_id, "status": "shipped"}

    @app.tool()
    def broken(order_id: str) -> str:
        """Always fail."""
        raise ValueError("no such order")

    return app


async def _call(app, name: str, arguments: dict):
    from mcp.shared.memory import create_connected_server_and_client_session

    async with create_connected_server_and_client_session(app._mcp_server) as client:
        return await client.call_tool(name, arguments)


async def test_client_and_server_spans_join_one_trace(exporter):
    result = await _call(_server(), "lookup_order", {"order_id": "A-1"})
    assert result.isError is False

    spans = {s.kind: s for s in exporter.get_finished_spans() if s.name == "tools/call lookup_order"}
    client, server = spans[SpanKind.CLIENT], spans[SpanKind.SERVER]
    for s in (client, server):
        assert s.instrumentation_scope.name == "flashcat.instrumentation.mcp"
        assert s.attributes["mcp.method.name"] == "tools/call"
        assert s.attributes["gen_ai.operation.name"] == "execute_tool"
        assert s.attributes["gen_ai.tool.name"] == "lookup_order"
        assert json.loads(s.attributes["gen_ai.tool.call.arguments"]) == {"order_id": "A-1"}
        assert "shipped" in s.attributes["gen_ai.tool.call.result"]
        assert s.status.status_code != StatusCode.ERROR
    # Propagating traceparent in _meta makes the server span a child of the client span in the same trace.
    assert server.context.trace_id == client.context.trace_id
    assert server.parent.span_id == client.context.span_id
    assert server.attributes["flashcat.genai.root"] is False


async def test_tool_error_is_marked(exporter):
    result = await _call(_server(), "broken", {"order_id": "x"})
    assert result.isError is True
    for s in exporter.get_finished_spans():
        assert s.name == "tools/call broken"
        assert s.attributes["error.type"] == "tool_error"
        assert s.status.status_code == StatusCode.ERROR


async def test_content_off_records_no_arguments(instrumented):
    exporter = instrumented("NO_CONTENT")
    await _call(_server(), "lookup_order", {"order_id": "A-1"})
    assert len(exporter.get_finished_spans()) == 2
    for s in exporter.get_finished_spans():
        assert "gen_ai.tool.call.arguments" not in s.attributes
        assert "gen_ai.tool.call.result" not in s.attributes


def test_uninstrument_restores_methods():
    from mcp.client.session import ClientSession
    from mcp.server.lowlevel.server import Server

    before = (ClientSession.call_tool, Server.call_tool)
    inst = MCPInstrumentor()
    inst.instrument(tracer_provider=TracerProvider())
    assert ClientSession.call_tool is not before[0]
    inst.uninstrument()
    assert (ClientSession.call_tool, Server.call_tool) == before
