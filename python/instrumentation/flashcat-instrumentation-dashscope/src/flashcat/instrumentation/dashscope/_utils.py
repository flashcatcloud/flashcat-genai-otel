# Copyright The OpenTelemetry Authors
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

"""Convert DashScope requests and responses to community util-genai types.

Ported from utils/generation.py and utils/common.py in LoongSuite's loongsuite-instrumentation-dashscope@c27eb2a6.
See UPSTREAM.md for differences.
"""

from __future__ import annotations

from typing import Any
from urllib.parse import urlparse

from opentelemetry.util.genai.types import (
    InputMessage,
    OutputMessage,
    TextPart,
    ToolCallRequestPart,
    ToolCallResponsePart,
)

PROVIDER = "dashscope"
_MISSING = object()


def safe_get(obj: Any, key: str, default: Any = None) -> Any:
    """Read dict-like DashScope objects whose DictMixin.__getattr__ raises KeyError for missing keys."""
    if obj is None:
        return default
    if isinstance(obj, dict):
        return obj.get(key, default)
    try:
        get = getattr(obj, "get", None)
    except (AttributeError, KeyError):
        get = None
    if callable(get):
        try:
            return get(key, default)
        except (AttributeError, KeyError, TypeError):
            pass
    try:
        return getattr(obj, key)
    except (AttributeError, KeyError):
        return default


def server_address() -> tuple[str | None, int | None]:
    try:
        import dashscope  # noqa: PLC0415

        u = urlparse(dashscope.base_http_api_url)
        return u.hostname, u.port or (443 if u.scheme == "https" else 80)
    except Exception:  # noqa: BLE001
        return None, None


def _text_parts(content: Any) -> list:
    parts: list = []
    if isinstance(content, str):
        if content:
            parts.append(TextPart(content=content))
    elif isinstance(content, list):
        for part in content:
            if isinstance(part, str):
                parts.append(TextPart(content=part))
            elif isinstance(part, dict) and "text" in part:
                parts.append(TextPart(content=part["text"]))
    return parts


def _tool_call_parts(tool_calls: Any) -> list:
    parts: list = []
    for tc in tool_calls or []:
        function = safe_get(tc, "function")
        name = safe_get(function, "name")
        if name:
            parts.append(ToolCallRequestPart(arguments=safe_get(function, "arguments"), name=name, id=safe_get(tc, "id")))
    return parts


def input_messages(call_kwargs: dict) -> list[InputMessage]:
    """Convert a DashScope prompt string or a messages list that may include tool calls and results."""
    out: list[InputMessage] = []
    messages = call_kwargs.get("messages")
    if messages:
        for msg in messages:
            role = safe_get(msg, "role", _MISSING)
            if role is _MISSING:
                continue
            role = role or "user"
            content = safe_get(msg, "content", "")
            if role == "tool":
                parts = [ToolCallResponsePart(response=content, id=safe_get(msg, "tool_call_id"))]
            else:
                parts = _text_parts(content) + _tool_call_parts(safe_get(msg, "tool_calls"))
            if not parts and content:
                parts = [TextPart(content=str(content))]
            if parts:
                out.append(InputMessage(role=role, parts=parts))
    elif isinstance(call_kwargs.get("prompt"), str) and call_kwargs["prompt"]:
        out.append(InputMessage(role="user", parts=[TextPart(content=call_kwargs["prompt"])]))
    return out


def output_messages(response: Any) -> tuple[list[OutputMessage], list[str]]:
    """Return (messages, finish_reasons) from standard output.text or message-format output.choices[]."""
    output = safe_get(response, "output")
    if not output:
        return [], []
    msgs: list[OutputMessage] = []
    reasons: list[str] = []
    choices = safe_get(output, "choices")
    if isinstance(choices, list) and choices:
        for choice in choices:
            message = safe_get(choice, "message")
            if not message:
                continue
            reason = safe_get(choice, "finish_reason") or safe_get(output, "finish_reason")
            parts = _text_parts(safe_get(message, "content")) + _tool_call_parts(safe_get(message, "tool_calls"))
            if reason == "null":
                reason = None
            if parts:
                msgs.append(OutputMessage(role=safe_get(message, "role") or "assistant", parts=parts, finish_reason=reason))
            if reason:
                reasons.append(reason)
    else:
        text = safe_get(output, "text")
        reason = safe_get(output, "finish_reason")
        if reason == "null":
            reason = None
        if text:
            msgs.append(OutputMessage(role="assistant", parts=[TextPart(content=text)], finish_reason=reason))
        if reason:
            reasons.append(reason)
    return msgs, reasons


def apply_request(invocation: Any, call_kwargs: dict) -> None:
    """Read request parameters. DashScope 2.x uses explicit keyword parameters and 1.x accepts the same names in kwargs."""
    params = call_kwargs.get("parameters") or {}

    def p(name: str):
        v = call_kwargs.get(name)
        return params.get(name) if v is None else v

    for attr, name in (("temperature", "temperature"), ("top_p", "top_p"), ("top_k", "top_k"), ("max_tokens", "max_tokens"),
                       ("presence_penalty", "presence_penalty"), ("seed", "seed")):
        v = p(name)
        if v is not None:
            try:
                setattr(invocation, attr, v)
            except Exception:  # noqa: BLE001
                pass
    stop = p("stop")
    if stop:
        invocation.stop_sequences = [stop] if isinstance(stop, str) else list(stop)
    if p("stream") is not None:
        invocation.request_stream = bool(p("stream"))
    if invocation.should_capture_content:
        invocation.input_messages = input_messages(call_kwargs)


def apply_usage(invocation: Any, usage: Any) -> None:
    if not usage:
        return
    inp = safe_get(usage, "input_tokens")
    out = safe_get(usage, "output_tokens")
    if inp is not None:
        invocation.input_tokens = int(inp)
    if out is not None:
        invocation.output_tokens = int(out)
    cached = safe_get(safe_get(usage, "prompt_tokens_details"), "cached_tokens")
    if cached:
        invocation.cache_read_input_tokens = int(cached)
    reasoning = safe_get(safe_get(usage, "output_tokens_details"), "reasoning_tokens")
    if reasoning:
        invocation.thinking_tokens = int(reasoning)


def apply_response(invocation: Any, response: Any, accumulated_text: str | None = None) -> None:
    """Use a non-streaming response or the final chunk of a stream, with accumulated text when incremental_output=True."""
    rid = safe_get(response, "request_id")
    if rid:
        invocation.response_id = rid
    model = safe_get(response, "model") or safe_get(safe_get(response, "output"), "model")
    invocation.response_model_name = model or invocation.request_model
    apply_usage(invocation, safe_get(response, "usage"))
    msgs, reasons = output_messages(response)
    if reasons:
        invocation.finish_reasons = reasons
    if invocation.should_capture_content:
        if accumulated_text:
            msgs = [OutputMessage(role="assistant", parts=[TextPart(content=accumulated_text)],
                                  finish_reason=reasons[0] if reasons else None)]
        invocation.output_messages = msgs


def response_error(response: Any) -> tuple[str, str] | None:
    """Read DashScope errors from the response. Return (code, message) for status_code != 200."""
    status = safe_get(response, "status_code")
    if status is None or status == 200:
        return None
    return str(safe_get(response, "code") or status), str(safe_get(response, "message") or "")


def chunk_text(chunk: Any) -> str:
    output = safe_get(chunk, "output")
    text = safe_get(output, "text")
    if text:
        return text
    choices = safe_get(output, "choices")
    if isinstance(choices, list) and choices:
        content = safe_get(safe_get(choices[0], "message"), "content")
        if isinstance(content, str):
            return content
    return ""
