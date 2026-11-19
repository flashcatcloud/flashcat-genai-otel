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

"""OpenTelemetry GenAI instrumentation for the Alibaba Cloud Model Studio DashScope SDK.

Supports Generation, AioGeneration, and TextEmbedding.
Ported from LoongSuite's loongsuite-instrumentation-dashscope@c27eb2a6 to use the community opentelemetry-util-genai package.
See UPSTREAM.md for differences.

    from flashcat.instrumentation.dashscope import DashScopeInstrumentor
    DashScopeInstrumentor().instrument()
"""

from __future__ import annotations

import logging
from typing import Any, Collection

from wrapt import wrap_function_wrapper

from opentelemetry.instrumentation.instrumentor import BaseInstrumentor
from opentelemetry.instrumentation.utils import unwrap
from opentelemetry.util.genai.handler import TelemetryHandler
from opentelemetry.util.genai.types import Error

from . import _utils as u
from .package import _instruments
from .version import __version__

logger = logging.getLogger(__name__)

SCOPE = "flashcat.instrumentation.dashscope"
_MODULE_GENERATION = "dashscope.aigc.generation"
_MODULE_TEXT_EMBEDDING = "dashscope.embeddings.text_embedding"
# Limit captured embedding inputs to keep spans small when batches contain dozens of documents.
_MAX_EMBED_TEXTS = 32


def _call_kwargs(args: tuple, kwargs: dict) -> dict:
    """Read model and prompt, which Generation.call(model, prompt=None, ...) also accepts positionally."""
    out = dict(kwargs)
    if args:
        out.setdefault("model", args[0])
    if len(args) > 1:
        out.setdefault("prompt", args[1])
    return out


def _start_inference(handler: TelemetryHandler, ck: dict):
    host, port = u.server_address()
    inv = handler.inference(u.PROVIDER, request_model=ck.get("model"), server_address=host, server_port=port)
    try:
        u.apply_request(inv, ck)
    except Exception:  # noqa: BLE001  Keep instrumentation failures from affecting application code.
        logger.debug("dashscope: apply_request failed", exc_info=True)
    return inv


def _finish(inv, response, accumulated: str | None = None) -> None:
    """Set span status to ERROR when status_code != 200. The application still receives the original response."""
    try:
        err = u.response_error(response)
        if err is not None:
            inv.fail(Error(message=err[1], type=err[0]))
            return
        u.apply_response(inv, response, accumulated)
        inv.stop()
    except Exception:  # noqa: BLE001
        logger.debug("dashscope: finish failed", exc_info=True)


def _is_stream(result: Any) -> bool:
    """Distinguish a streaming generator from a non-streaming GenerationResponse with status_code."""
    return not hasattr(result, "status_code") and hasattr(result, "__iter__")


def _wrap_stream(gen, inv, incremental: bool):
    """Collect content and usage from a streaming response.

    With the default incremental_output=False, each chunk contains the full output so far. Use only the final chunk's content and usage
    (see LoongSuite patch/generation.py:70). With incremental_output=True, concatenate the text from each chunk.
    """
    last = None
    acc: list[str] = []
    try:
        for chunk in gen:
            inv.record_stream_chunk()
            last = chunk
            if incremental:
                acc.append(u.chunk_text(chunk))
            yield chunk
    except BaseException as e:
        if isinstance(e, GeneratorExit):
            _finish(inv, last, "".join(acc) if incremental else None) if last is not None else inv.stop()
        else:
            inv.fail(e)
        raise
    if last is not None:
        _finish(inv, last, "".join(acc) if incremental else None)
    else:
        inv.stop()


async def _wrap_astream(gen, inv, incremental: bool):
    last = None
    acc: list[str] = []
    try:
        async for chunk in gen:
            inv.record_stream_chunk()
            last = chunk
            if incremental:
                acc.append(u.chunk_text(chunk))
            yield chunk
    except BaseException as e:
        if isinstance(e, GeneratorExit):
            _finish(inv, last, "".join(acc) if incremental else None) if last is not None else inv.stop()
        else:
            inv.fail(e)
        raise
    if last is not None:
        _finish(inv, last, "".join(acc) if incremental else None)
    else:
        inv.stop()


def _generation_wrapper(handler: TelemetryHandler):
    def wrapper(wrapped, instance, args, kwargs):
        ck = _call_kwargs(args, kwargs)
        if not ck.get("model"):
            return wrapped(*args, **kwargs)
        try:
            inv = _start_inference(handler, ck)
        except Exception:  # noqa: BLE001
            return wrapped(*args, **kwargs)
        try:
            result = wrapped(*args, **kwargs)
        except BaseException as e:
            inv.fail(e)
            raise
        if _is_stream(result):
            return _wrap_stream(result, inv, bool(ck.get("incremental_output")))
        _finish(inv, result)
        return result

    return wrapper


def _aio_generation_wrapper(handler: TelemetryHandler):
    async def wrapper(wrapped, instance, args, kwargs):
        ck = _call_kwargs(args, kwargs)
        if not ck.get("model"):
            return await wrapped(*args, **kwargs)
        try:
            inv = _start_inference(handler, ck)
        except Exception:  # noqa: BLE001
            return await wrapped(*args, **kwargs)
        try:
            result = await wrapped(*args, **kwargs)
        except BaseException as e:
            inv.fail(e)
            raise
        if hasattr(result, "__aiter__"):
            return _wrap_astream(result, inv, bool(ck.get("incremental_output")))
        _finish(inv, result)
        return result

    return wrapper


def _embedding_texts(inv, texts: Any) -> None:
    """Record embedding content using OpenInference's embedding.embeddings.{i}.embedding.text.

    The OTel GenAI conventions define no embedding content attribute. Phoenix and the Flashcat backend read this attribute.
    Write it only when content capture to spans is enabled, using the same switch as Generation's input_messages.
    """
    if not getattr(inv, "_should_capture_content_on_span", False):
        return
    if isinstance(texts, str):
        texts = [texts]
    if not isinstance(texts, (list, tuple)):
        return  # Skip inputs such as files and URLs.
    texts = [t for t in texts if isinstance(t, str)]
    for i, t in enumerate(texts[:_MAX_EMBED_TEXTS]):
        inv.span.set_attribute(f"embedding.embeddings.{i}.embedding.text", t)
    if len(texts) > _MAX_EMBED_TEXTS:
        inv.span.set_attribute("flashcat.content_truncated", True)


def _embedding_wrapper(handler: TelemetryHandler):
    def wrapper(wrapped, instance, args, kwargs):
        model = kwargs.get("model") or (args[0] if args else None)
        if not model:
            return wrapped(*args, **kwargs)
        try:
            host, port = u.server_address()
            inv = handler.embedding(u.PROVIDER, request_model=model, server_address=host, server_port=port)
            dim = kwargs.get("dimension")
            if dim:
                inv.dimension_count = int(dim)
        except Exception:  # noqa: BLE001
            return wrapped(*args, **kwargs)
        try:
            _embedding_texts(inv, kwargs.get("input", args[1] if len(args) > 1 else None))
        except Exception:  # noqa: BLE001
            logger.debug("dashscope: embedding content capture failed", exc_info=True)
        try:
            result = wrapped(*args, **kwargs)
        except BaseException as e:
            inv.fail(e)
            raise
        try:
            err = u.response_error(result)
            if err is not None:
                inv.fail(Error(message=err[1], type=err[0]))
                return result
            usage = u.safe_get(result, "usage")
            tokens = u.safe_get(usage, "total_tokens") or u.safe_get(usage, "input_tokens")
            if tokens is not None:
                inv.input_tokens = int(tokens)
            inv.response_model_name = model
            if not getattr(inv, "dimension_count", None):
                # Infer the dimension from the returned vector length when omitted, for the output summary in the list view.
                embs = u.safe_get(u.safe_get(result, "output"), "embeddings") or []
                vec = u.safe_get(embs[0], "embedding") if embs else None
                if vec:
                    inv.dimension_count = len(vec)
            inv.stop()
        except Exception:  # noqa: BLE001
            logger.debug("dashscope: embedding finish failed", exc_info=True)
        return result

    return wrapper


class DashScopeInstrumentor(BaseInstrumentor):
    """Instrument the DashScope SDK. Use the custom provider name dashscope, which Flashcat recognizes and the semantic conventions omit."""

    def instrumentation_dependencies(self) -> Collection[str]:
        return _instruments

    def _instrument(self, **kwargs):
        handler = TelemetryHandler(
            tracer_provider=kwargs.get("tracer_provider"),
            meter_provider=kwargs.get("meter_provider"),
            logger_provider=kwargs.get("logger_provider"),
            instrumentation_scope_name=SCOPE,
            instrumentation_scope_version=__version__,
        )
        wrap_function_wrapper(_MODULE_GENERATION, "Generation.call", _generation_wrapper(handler))
        wrap_function_wrapper(_MODULE_GENERATION, "AioGeneration.call", _aio_generation_wrapper(handler))
        wrap_function_wrapper(_MODULE_TEXT_EMBEDDING, "TextEmbedding.call", _embedding_wrapper(handler))

    def _uninstrument(self, **kwargs):
        import dashscope.aigc.generation as gen  # noqa: PLC0415
        import dashscope.embeddings.text_embedding as emb  # noqa: PLC0415

        unwrap(gen.Generation, "call")
        unwrap(gen.AioGeneration, "call")
        unwrap(emb.TextEmbedding, "call")


__all__ = ["DashScopeInstrumentor", "__version__"]
