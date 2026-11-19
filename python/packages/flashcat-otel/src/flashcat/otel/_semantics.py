"""Semantic API (contract §4).

The decorators rely on util-genai's invoke_local_agent, tool, workflow,
inference, embedding, and retrieval helpers to start spans, suppress nested
spans of the same kind, set semantic convention attributes, and propagate session IDs.
"""

from __future__ import annotations

import contextlib
import functools
import inspect
import json
import logging
from typing import Any, Callable, Iterator, Mapping, TypeVar

from opentelemetry import baggage, trace as trace_api
from opentelemetry import context as otel_context
from opentelemetry.util.genai.handler import TelemetryHandler
from opentelemetry.util.genai.types import InputMessage, OutputMessage, TextPart

from ._processors import CONV_KEY, TAGS_KEY, USER_KEY

logger = logging.getLogger("flashcat.otel")
F = TypeVar("F", bound=Callable[..., Any])

SCOPE = "flashcat.otel"
_MAX_SERIALIZED = 32 * 1024  # Serialization limit for arguments and return values.
_TRUNCATED = "…[truncated]"

# Track the innermost span created by the semantic API for annotate. Upstream instrumentation spans are excluded.
_FRAME_KEY = otel_context.create_key("flashcat-otel-semantic-frame")
_UNSET: Any = object()

_handler: TelemetryHandler | None = None


def set_handler(h: TelemetryHandler | None) -> None:
    global _handler
    _handler = h


def handler() -> TelemetryHandler:
    """Create the handler lazily from the global provider so it can be used before init."""
    global _handler
    if _handler is None:
        _handler = TelemetryHandler(instrumentation_scope_name=SCOPE)
    return _handler


def _serialize(value: Any) -> str:
    """Serialize up to the length limit. Use repr for unknown objects to avoid traversing __dict__."""
    try:
        s = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False, default=repr)
    except Exception:  # noqa: BLE001
        s = repr(value)
    if len(s) > _MAX_SERIALIZED:
        s = s[:_MAX_SERIALIZED] + _TRUNCATED
    return s


def _messages(value: Any, role: str, cls: type, structured: bool) -> list:
    """In structured mode (used by annotate), turn each {role, content} item into a message. Otherwise serialize the whole value as one text message."""
    if structured and isinstance(value, (list, tuple)) and value and all(
            isinstance(m, Mapping) and "role" in m for m in value):
        out = []
        for m in value:
            c = m.get("content")
            out.append(cls(role=str(m["role"]), parts=[TextPart(content=c if isinstance(c, str) else _serialize(c))]))
        return out
    return [cls(role=role, parts=[TextPart(content=_serialize(value))])]


def _documents(value: Any) -> list[dict]:
    """Build the retrieval documents array (contract §4).

    Keep dictionaries as-is and wrap other values in {"content": …}. If the array exceeds the limit, drop items from the end and append a truncation marker.
    """
    items = list(value) if isinstance(value, (list, tuple)) else [value]
    docs = [dict(d) if isinstance(d, Mapping) else {"content": _serialize(d)} for d in items]

    def size(ds: list) -> int:
        try:
            return len(json.dumps(ds, ensure_ascii=False, default=repr))
        except Exception:  # noqa: BLE001
            return 0

    if size(docs) > _MAX_SERIALIZED:
        while docs and size(docs) > _MAX_SERIALIZED:
            docs.pop()
        docs.append({"content": _TRUNCATED})
    return docs


def _count(v: Any) -> int | None:
    return v if isinstance(v, int) and not isinstance(v, bool) and v >= 0 else None


def _call_args(fn: Callable, args: tuple, kwargs: dict) -> dict:
    try:
        bound = inspect.signature(fn).bind_partial(*args, **kwargs)
        out = dict(bound.arguments)
        out.pop("self", None)
        out.pop("cls", None)
        return out
    except Exception:  # noqa: BLE001
        return {"args": list(args), "kwargs": kwargs}


def _current_conversation() -> str | None:
    v = otel_context.get_value(CONV_KEY)
    return str(v) if v else None


# ---- Map arguments and return values to each invocation type ----


class _Frame:
    """Track an active semantic span so its arguments and return value cannot overwrite input or output set by annotate at span end."""

    __slots__ = ("inv", "kind", "output_set")

    def __init__(self, inv, kind: str) -> None:
        self.inv, self.kind = inv, kind
        self.output_set = False


def _write_input(inv, kind: str, value: Any, structured: bool) -> None:
    if kind == "tool":
        inv.arguments = _serialize(value)
    elif kind == "retrieval":
        inv.query_text = _serialize(value)
    elif kind != "embedding":
        inv.input_messages = _messages(value, "user", InputMessage, structured)


def _write_output(inv, kind: str, value: Any, structured: bool) -> None:
    if kind == "tool":
        inv.tool_result = _serialize(value)
    elif kind == "retrieval":
        inv.documents = _documents(value)
    elif kind != "embedding":
        inv.output_messages = _messages(value, "assistant", OutputMessage, structured)


class _Kind:
    def __init__(self, kind: str, name: str, description: str | None = None, provider: str | None = None) -> None:
        self.kind, self.name, self.description, self.provider = kind, name, description, provider

    def start(self):
        h = handler()
        conv = _current_conversation()
        if self.kind == "llm":
            # The provider is optional. util-genai sets gen_ai.provider.name only when it has a value.
            return h.inference(self.provider, request_model=self.name, conversation_id=conv)  # type: ignore[arg-type]
        if self.kind == "embedding":
            return h.embedding(self.provider, request_model=self.name)  # type: ignore[arg-type]
        if self.kind == "retrieval":
            return h.retrieval(data_source_id=self.name)
        if self.kind == "agent":
            inv = h.invoke_local_agent(agent_name=self.name, conversation_id=conv)
            if self.description and hasattr(inv, "agent_description"):
                try:
                    inv.agent_description = self.description
                except Exception:  # noqa: BLE001
                    pass
            return inv
        if self.kind == "tool":
            inv = h.tool(self.name)
            if self.description:
                try:
                    inv.tool_description = self.description
                except Exception:  # noqa: BLE001
                    pass
            return inv
        return h.workflow(self.name, conversation_id=conv)

    def set_input(self, inv, fn, args, kwargs) -> None:
        if not inv.should_capture_content:
            return
        _write_input(inv, self.kind, _call_args(fn, args, kwargs), structured=False)

    def set_output(self, inv, result) -> None:
        if not inv.should_capture_content:
            return
        _write_output(inv, self.kind, result, structured=False)


def _decorate(kind: _Kind) -> Callable[[F], F]:
    """Wrap synchronous functions, coroutines, generators, and async generators.

    Reraise application exceptions unchanged and call the original function directly on SDK failure.
    A blanket exception-swallowing decorator would hide application errors (see OpenLLMetry #4589).
    """

    def deco(fn: F) -> F:
        def begin(args, kwargs):
            try:
                inv = kind.start()
            except Exception:  # noqa: BLE001
                logger.debug("flashcat-otel: 创建 invocation 失败", exc_info=True)
                return None
            frame = _Frame(inv, kind.kind)
            token = None
            try:
                token = otel_context.attach(otel_context.set_value(_FRAME_KEY, frame))
            except Exception:  # noqa: BLE001
                pass
            try:
                kind.set_input(inv, fn, args, kwargs)
            except Exception:  # noqa: BLE001
                pass
            return frame, token

        def finish(st, result=None, exc: BaseException | None = None) -> None:
            if st is None:
                return
            frame, token = st
            # Detach our context before the invocation detaches its own, in last-in, first-out order.
            if token is not None:
                try:
                    otel_context.detach(token)
                except Exception:  # noqa: BLE001
                    pass
            inv = frame.inv
            try:
                if exc is not None:
                    inv.fail(exc)
                else:
                    if not frame.output_set:
                        kind.set_output(inv, result)
                    inv.stop()
            except Exception:  # noqa: BLE001
                logger.debug("flashcat-otel: 结束 invocation 失败", exc_info=True)

        if inspect.isasyncgenfunction(fn):

            @functools.wraps(fn)
            async def agen_wrapper(*args, **kwargs):
                st = begin(args, kwargs)
                items = []
                try:
                    async for item in fn(*args, **kwargs):
                        items.append(item)
                        yield item
                except BaseException as e:  # Catch GeneratorExit so close() also ends the span.
                    if isinstance(e, GeneratorExit):
                        finish(st, items)
                    else:
                        finish(st, exc=e)
                    raise
                finish(st, items)

            return agen_wrapper  # type: ignore[return-value]

        if inspect.isgeneratorfunction(fn):

            @functools.wraps(fn)
            def gen_wrapper(*args, **kwargs):
                st = begin(args, kwargs)
                items = []
                try:
                    for item in fn(*args, **kwargs):
                        items.append(item)
                        yield item
                except BaseException as e:
                    if isinstance(e, GeneratorExit):
                        finish(st, items)
                    else:
                        finish(st, exc=e)
                    raise
                finish(st, items)

            return gen_wrapper  # type: ignore[return-value]

        if inspect.iscoroutinefunction(fn):

            @functools.wraps(fn)
            async def async_wrapper(*args, **kwargs):
                st = begin(args, kwargs)
                try:
                    result = await fn(*args, **kwargs)
                except BaseException as e:
                    finish(st, exc=e)
                    raise
                finish(st, result)
                return result

            return async_wrapper  # type: ignore[return-value]

        @functools.wraps(fn)
        def wrapper(*args, **kwargs):
            st = begin(args, kwargs)
            try:
                result = fn(*args, **kwargs)
            except BaseException as e:
                finish(st, exc=e)
                raise
            finish(st, result)
            return result

        return wrapper  # type: ignore[return-value]

    return deco


def agent(name: str, description: str | None = None) -> Callable[[F], F]:
    """Mark an agent entry point with an invoke_agent span."""
    return _decorate(_Kind("agent", name, description))


def tool(name: str, description: str | None = None) -> Callable[[F], F]:
    """Mark a tool function with an execute_tool span."""
    return _decorate(_Kind("tool", name, description))


def workflow(name: str) -> Callable[[F], F]:
    """Mark a workflow with an invoke_workflow span."""
    return _decorate(_Kind("workflow", name))


def llm(model: str, provider: str | None = None) -> Callable[[F], F]:
    """Mark an uninstrumented model call, such as a custom gateway or private SDK call, with a chat {model} span.

    Use annotate to add token usage, the response model, and messages. Nested calls instrumented with util-genai contribute to this span (contract §4.1).
    """
    return _decorate(_Kind("llm", model, provider=provider))


def embedding(model: str, provider: str | None = None) -> Callable[[F], F]:
    """Mark an uninstrumented embedding call with an embeddings {model} span that omits input content."""
    return _decorate(_Kind("embedding", model, provider=provider))


def retrieval(name: str) -> Callable[[F], F]:
    """Mark a retrieval call to a vector database or search engine with a retrieval {name} span and use the return value as the retrieved documents."""
    return _decorate(_Kind("retrieval", name))


def annotate(
    *,
    input: Any = _UNSET,  # noqa: A002
    output: Any = _UNSET,
    model: str | None = None,
    usage: Mapping[str, Any] | None = None,
    finish_reasons: list[str] | tuple[str, ...] | None = None,
    tags: Mapping[str, Any] | None = None,
) -> None:
    """Add data to the innermost semantic API span (contract §4.2). With no active semantic span, this is a no-op.

    The usage keys are input_tokens, output_tokens, cache_read_input_tokens, and cache_write_input_tokens.
    """
    try:
        frame = otel_context.get_value(_FRAME_KEY)
        if not isinstance(frame, _Frame):
            return
        inv, kind = frame.inv, frame.kind
        if input is not _UNSET and inv.should_capture_content:
            _write_input(inv, kind, input, structured=True)
        if output is not _UNSET and kind != "embedding":
            frame.output_set = True
            if inv.should_capture_content:
                _write_output(inv, kind, output, structured=True)
        if model is not None and kind in ("llm", "embedding"):
            inv.response_model_name = str(model)
        if usage and kind in ("llm", "embedding"):
            fields = ("input_tokens",) if kind == "embedding" else (
                "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_write_input_tokens")
            for f in fields:
                v = _count(usage.get(f))
                if v is not None:
                    setattr(inv, f, v)
        if finish_reasons is not None and kind == "llm":
            inv.finish_reasons = [str(x) for x in finish_reasons]
        if tags:
            for k, v in tags.items():
                inv.attributes[f"flashcat.tag.{k}"] = str(v)
    except Exception:  # noqa: BLE001 SDK failures must leave application code unaffected.
        logger.debug("flashcat-otel: annotate 失败", exc_info=True)


@contextlib.contextmanager
def trace(name: str, attributes: Mapping[str, Any] | None = None) -> Iterator[trace_api.Span]:
    """Create a plain root span for a script or task without GenAI semantics."""
    tracer = trace_api.get_tracer(SCOPE)
    with tracer.start_as_current_span(name, attributes=dict(attributes or {})) as span:
        yield span


@contextlib.contextmanager
def session(
    conversation_id: str | None = None,
    user_id: str | None = None,
    tags: Mapping[str, Any] | None = None,
    propagate: bool = False,
) -> Iterator[None]:
    """Set session context so ContextEnricher writes gen_ai.conversation.id, user.id, and flashcat.tag.* at span start.

    Baggage is off by default because HTTP headers would expose user_id and other data to downstream services.
    Pass propagate=True to correlate sessions across services.
    """
    ctx = otel_context.get_current()
    if conversation_id:
        ctx = otel_context.set_value(CONV_KEY, str(conversation_id), ctx)
    if user_id:
        ctx = otel_context.set_value(USER_KEY, str(user_id), ctx)
    if tags:
        merged = dict(otel_context.get_value(TAGS_KEY, ctx) or {})
        merged.update({str(k): str(v) for k, v in tags.items()})
        ctx = otel_context.set_value(TAGS_KEY, merged, ctx)
    if propagate:
        if conversation_id:
            ctx = baggage.set_baggage("gen_ai.conversation.id", str(conversation_id), ctx)
        if user_id:
            ctx = baggage.set_baggage("user.id", str(user_id), ctx)
    token = otel_context.attach(ctx)
    try:
        yield
    finally:
        otel_context.detach(token)
