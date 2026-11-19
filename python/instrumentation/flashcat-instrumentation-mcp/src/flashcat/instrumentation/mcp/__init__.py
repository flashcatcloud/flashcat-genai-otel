# Copyright 2026 Flashcat Cloud
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""OpenTelemetry instrumentation for MCP Python SDK 1.x tool calls (contract §4.3).

Spans follow the OTel MCP semantic conventions. They use the name ``tools/call {tool name}`` and kind CLIENT or SERVER.
They set ``mcp.method.name``, ``gen_ai.operation.name=execute_tool``, and ``gen_ai.tool.name``. Content capture adds arguments and results.
The client injects W3C traceparent into ``_meta`` using Trace Context alone, without baggage. The server extracts it from ``_meta``
to join the client and server spans in the same trace, as in MCP 2.x's built-in instrumentation.

    from flashcat.instrumentation.mcp import MCPInstrumentor
    MCPInstrumentor().instrument()
"""

from __future__ import annotations

import inspect
import json
import logging
from typing import Any, Collection

from wrapt import wrap_function_wrapper

from opentelemetry import context as otel_context
from opentelemetry import trace
from opentelemetry.instrumentation.instrumentor import BaseInstrumentor
from opentelemetry.instrumentation.utils import unwrap
from opentelemetry.trace import SpanKind, StatusCode
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
from opentelemetry.util.genai.handler import TelemetryHandler

from .package import _instruments
from .version import __version__

logger = logging.getLogger(__name__)

SCOPE = "flashcat.instrumentation.mcp"
_CLIENT_MODULE = "mcp.client.session"
_SERVER_MODULE = "mcp.server.lowlevel.server"
_MAX_SERIALIZED = 32 * 1024
_PROPAGATOR = TraceContextTextMapPropagator()
_WRAPPED = "_flashcat_mcp_wrapped"


def _serialize(value: Any) -> str:
    try:
        if hasattr(value, "model_dump"):
            value = value.model_dump(mode="json", by_alias=True, exclude_none=True)
        s = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False, default=repr)
    except Exception:  # noqa: BLE001
        s = repr(value)
    return s[:_MAX_SERIALIZED] + "…[truncated]" if len(s) > _MAX_SERIALIZED else s


def _result_text(result: Any) -> str:
    """Prefer the structured CallToolResult, then joined text blocks, then serialization of the whole result."""
    structured = getattr(result, "structuredContent", None)
    if structured is not None:
        return _serialize(structured)
    blocks = getattr(result, "content", None)
    if isinstance(blocks, list) and blocks and all(getattr(b, "type", None) == "text" for b in blocks):
        return _serialize("\n".join(getattr(b, "text", "") for b in blocks))
    return _serialize(result)


def _attributes(name: Any) -> dict:
    attrs = {"mcp.method.name": "tools/call", "gen_ai.operation.name": "execute_tool"}
    if isinstance(name, str):
        attrs["gen_ai.tool.name"] = name
    return attrs


def _span_name(name: Any) -> str:
    return f"tools/call {name}" if isinstance(name, str) else "tools/call"


def _fail(span, exc: BaseException) -> None:
    try:
        err = getattr(exc, "error", None)  # Read the JSON-RPC error code from McpError.
        code = getattr(err, "code", None)
        span.set_attribute("error.type", str(code) if code is not None else type(exc).__qualname__)
        span.record_exception(exc)
        span.set_status(StatusCode.ERROR, str(exc))
        span.end()
    except Exception:  # noqa: BLE001
        pass


def _finish(span, result: Any, content: bool) -> None:
    """Set error.type=tool_error and status ERROR when a tool returns isError, matching MCP 2.x."""
    try:
        if content:
            span.set_attribute("gen_ai.tool.call.result", _result_text(result))
        if getattr(result, "isError", None) is True:
            span.set_attribute("error.type", "tool_error")
            span.set_status(StatusCode.ERROR)
        span.end()
    except Exception:  # noqa: BLE001
        pass


def _client_call_tool(tracer, handler: TelemetryHandler, supports_meta: bool):
    async def wrapper(wrapped, instance, args, kwargs):
        name = kwargs.get("name", args[0] if args else None)
        arguments = kwargs.get("arguments", args[1] if len(args) > 1 else None)
        try:
            span = tracer.start_span(_span_name(name), kind=SpanKind.CLIENT, attributes=_attributes(name))
            content = handler.should_capture_content()
            if content and arguments is not None:
                span.set_attribute("gen_ai.tool.call.arguments", _serialize(arguments))
            token = otel_context.attach(trace.set_span_in_context(span))
        except Exception:  # noqa: BLE001 Call the original method directly on SDK failure.
            return await wrapped(*args, **kwargs)
        try:
            if supports_meta:
                try:
                    meta = dict(kwargs.get("meta") or {})
                    _PROPAGATOR.inject(meta)
                    kwargs["meta"] = meta
                except Exception:  # noqa: BLE001
                    pass
            result = await wrapped(*args, **kwargs)
        except BaseException as e:
            otel_context.detach(token)
            _fail(span, e)
            raise
        otel_context.detach(token)
        _finish(span, result, content)
        return result

    return wrapper


def _parent_from_meta(params: Any):
    meta = getattr(params, "meta", None)
    if meta is None:
        return None
    try:
        carrier = meta.model_dump(by_alias=True, exclude_none=True) if hasattr(meta, "model_dump") else dict(meta)
        ctx = _PROPAGATOR.extract({str(k): str(v) for k, v in carrier.items()})
        return ctx if trace.get_current_span(ctx).get_span_context().is_valid else None
    except Exception:  # noqa: BLE001
        return None


def _traced_server_handler(handler_fn, tracer, handler: TelemetryHandler):
    async def traced(req):
        params = getattr(req, "params", None)
        name = getattr(params, "name", None)
        try:
            parent = _parent_from_meta(params)
            span = tracer.start_span(_span_name(name), context=parent, kind=SpanKind.SERVER, attributes=_attributes(name))
            if parent is not None:
                # The parent from _meta is the client's GenAI tool-call span, so set the server's AI root marker to false (contract §5.1).
                # The session processor would otherwise mark this as a root because it only sees in-process ancestry.
                span.set_attribute("flashcat.genai.root", False)
            content = handler.should_capture_content()
            arguments = getattr(params, "arguments", None)
            if content and arguments is not None:
                span.set_attribute("gen_ai.tool.call.arguments", _serialize(arguments))
            token = otel_context.attach(trace.set_span_in_context(span, parent))
        except Exception:  # noqa: BLE001
            return await handler_fn(req)
        try:
            result = await handler_fn(req)
        except BaseException as e:
            otel_context.detach(token)
            _fail(span, e)
            raise
        otel_context.detach(token)
        _finish(span, getattr(result, "root", result), content)  # ServerResult(root=CallToolResult)
        return result

    setattr(traced, _WRAPPED, True)
    return traced


def _server_call_tool(tracer, handler: TelemetryHandler):
    """Wrap the handler in request_handlers after the Server.call_tool() decorator registers it. FastMCP uses this path too."""

    def wrapper(wrapped, instance, args, kwargs):
        decorator = wrapped(*args, **kwargs)

        def patched(func):
            ret = decorator(func)
            try:
                from mcp import types

                current = instance.request_handlers.get(types.CallToolRequest)
                if current is not None and not getattr(current, _WRAPPED, False):
                    instance.request_handlers[types.CallToolRequest] = _traced_server_handler(current, tracer, handler)
            except Exception:  # noqa: BLE001
                logger.debug("flashcat-instrumentation-mcp: 包装服务端 handler 失败", exc_info=True)
            return ret

        return patched

    return wrapper


class MCPInstrumentor(BaseInstrumentor):
    """Instrument MCP Python SDK 1.x tool calls in ClientSession.call_tool and handlers registered by Server.call_tool."""

    def instrumentation_dependencies(self) -> Collection[str]:
        return _instruments

    def _instrument(self, **kwargs):
        tracer = trace.get_tracer(SCOPE, __version__, tracer_provider=kwargs.get("tracer_provider"))
        handler = TelemetryHandler(  # Read the content capture switch using the same rules as other GenAI instrumentation.
            tracer_provider=kwargs.get("tracer_provider"),
            meter_provider=kwargs.get("meter_provider"),
            instrumentation_scope_name=SCOPE,
            instrumentation_scope_version=__version__,
        )
        from mcp.client.session import ClientSession

        # Inject traceparent only on later 1.x versions that support the call_tool meta parameter.
        supports_meta = "meta" in inspect.signature(ClientSession.call_tool).parameters
        wrap_function_wrapper(_CLIENT_MODULE, "ClientSession.call_tool", _client_call_tool(tracer, handler, supports_meta))
        wrap_function_wrapper(_SERVER_MODULE, "Server.call_tool", _server_call_tool(tracer, handler))

    def _uninstrument(self, **kwargs):
        from mcp.client.session import ClientSession
        from mcp.server.lowlevel.server import Server

        unwrap(ClientSession, "call_tool")
        unwrap(Server, "call_tool")
