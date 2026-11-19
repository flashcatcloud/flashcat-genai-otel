"""Test DashScope instrumentation against a local mock HTTP service set via dashscope.base_http_api_url. Collect spans in memory."""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

import dashscope
from dashscope import AioGeneration, Generation, TextEmbedding
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import StatusCode

from flashcat.instrumentation.dashscope import DashScopeInstrumentor

USAGE = {"input_tokens": 12, "output_tokens": 5, "total_tokens": 17, "prompt_tokens_details": {"cached_tokens": 4}}


def _message_output(text: str, finish: str = "stop") -> dict:
    return {"choices": [{"finish_reason": finish, "message": {"role": "assistant", "content": text}}]}


class _Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # noqa: D401
        pass

    def _json(self, status: int, body: dict) -> None:
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):  # noqa: N802
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])) or b"{}")
        self.server.bodies.append(body)  # type: ignore[attr-defined]
        model = body.get("model")
        if "text-embedding" in self.path:
            self._json(200, {"request_id": "emb-1", "output": {"embeddings": [{"text_index": 0, "embedding": [0.1, 0.2]}]},
                             "usage": {"total_tokens": 7}})
            return
        if model == "broken":
            self._json(400, {"request_id": "err-1", "code": "InvalidParameter", "message": "model broken"})
            return
        if self.headers.get("X-DashScope-SSE") == "enable":
            incremental = body.get("parameters", {}).get("incremental_output")
            pieces = ["订单", "已发货"]
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            acc = ""
            for i, piece in enumerate(pieces):
                acc += piece
                last = i == len(pieces) - 1
                out = _message_output(piece if incremental else acc, "stop" if last else "null")
                usage = {"input_tokens": 12, "output_tokens": i + 1, "total_tokens": 13 + i}
                payload = {"request_id": "req-stream", "output": out, "usage": usage}
                self.wfile.write(f"id:{i + 1}\nevent:result\n:HTTP_STATUS/200\ndata:{json.dumps(payload)}\n\n".encode())
                self.wfile.flush()
            return
        self._json(200, {"request_id": "req-1", "output": _message_output("订单 A-1 已发货"), "usage": USAGE})


@pytest.fixture(scope="module")
def server():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
    srv.bodies = []  # type: ignore[attr-defined]
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield srv
    srv.shutdown()


@pytest.fixture
def spans(server, monkeypatch):
    for k in ("http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
        monkeypatch.delenv(k, raising=False)
    monkeypatch.setenv("NO_PROXY", "127.0.0.1")
    monkeypatch.setenv("OTEL_SEMCONV_STABILITY_OPT_IN", "gen_ai_latest_experimental")
    monkeypatch.setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "SPAN_ONLY")
    monkeypatch.setattr(dashscope, "base_http_api_url", f"http://127.0.0.1:{server.server_port}/api/v1")
    monkeypatch.setattr(dashscope, "api_key", "sk-test")
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    inst = DashScopeInstrumentor()
    inst.instrument(tracer_provider=provider)
    yield exporter
    inst.uninstrument()


def _messages(attrs, key):
    return json.loads(attrs[key])


def _texts(attrs, key):
    return [p.get("content") for m in _messages(attrs, key) for p in m["parts"]]


def _one(exporter):
    got = exporter.get_finished_spans()
    assert len(got) == 1, [s.name for s in got]
    return got[0]


def test_generation_non_stream(spans):
    resp = Generation.call(model="qwen-plus", messages=[{"role": "system", "content": "你是客服"},
                                                        {"role": "user", "content": "我的订单呢"}],
                           result_format="message", temperature=0.3, max_tokens=64)
    assert resp.status_code == 200
    s = _one(spans)
    a = s.attributes
    assert s.name == "chat qwen-plus"
    assert a["gen_ai.provider.name"] == "dashscope"
    assert a["gen_ai.request.model"] == "qwen-plus"
    assert a["gen_ai.response.model"] == "qwen-plus"
    assert a["gen_ai.response.id"] == "req-1"
    assert a["gen_ai.usage.input_tokens"] == 12
    assert a["gen_ai.usage.output_tokens"] == 5
    assert a["gen_ai.usage.cache_read.input_tokens"] == 4
    assert a["gen_ai.request.temperature"] == 0.3
    assert a["gen_ai.request.max_tokens"] == 64
    assert tuple(a["gen_ai.response.finish_reasons"]) == ("stop",)
    assert a["server.address"] == "127.0.0.1"
    assert _texts(a, "gen_ai.output.messages") == ["订单 A-1 已发货"]
    assert _messages(a, "gen_ai.output.messages")[0]["finish_reason"] == "stop"
    assert [m["role"] for m in _messages(a, "gen_ai.input.messages")] == ["system", "user"]
    assert _texts(a, "gen_ai.input.messages") == ["你是客服", "我的订单呢"]
    assert s.status.status_code != StatusCode.ERROR


def test_positional_model_and_prompt(spans):
    Generation.call("qwen-turbo", "你好")
    a = _one(spans).attributes
    assert a["gen_ai.request.model"] == "qwen-turbo"
    assert _texts(a, "gen_ai.input.messages") == ["你好"]


def test_in_band_error_marks_span_failed(spans):
    resp = Generation.call(model="broken", prompt="x")
    assert resp.status_code == 400  # The application still receives the original response.
    s = _one(spans)
    assert s.status.status_code == StatusCode.ERROR
    assert s.attributes["error.type"] == "InvalidParameter"


def test_stream_full_output_takes_last_chunk(spans):
    chunks = list(Generation.call(model="qwen-plus", prompt="物流", stream=True, result_format="message"))
    assert len(chunks) == 2
    a = _one(spans).attributes
    assert a["gen_ai.usage.output_tokens"] == 2  # Use only the final chunk's usage.
    assert _texts(a, "gen_ai.output.messages") == ["订单已发货"]
    assert _messages(a, "gen_ai.output.messages")[0]["finish_reason"] == "stop"
    assert a["gen_ai.response.id"] == "req-stream"


def test_stream_incremental_output_accumulates(spans):
    list(Generation.call(model="qwen-plus", prompt="物流", stream=True, incremental_output=True, result_format="message"))
    a = _one(spans).attributes
    assert _texts(a, "gen_ai.output.messages") == ["订单已发货"]


def test_stream_early_close_still_ends_span(spans):
    gen = Generation.call(model="qwen-plus", prompt="物流", stream=True, result_format="message")
    next(gen)
    gen.close()
    s = _one(spans)
    assert s.status.status_code != StatusCode.ERROR


async def test_aio_generation(spans):
    resp = await AioGeneration.call(model="qwen-plus", prompt="异步", result_format="message")
    assert resp.status_code == 200
    a = _one(spans).attributes
    assert a["gen_ai.usage.input_tokens"] == 12


def test_text_embedding(spans):
    TextEmbedding.call(model="text-embedding-v3", input="退货政策", dimension=2)
    s = _one(spans)
    a = s.attributes
    assert a["gen_ai.operation.name"] == "embeddings"
    assert a["gen_ai.usage.input_tokens"] == 7
    assert a["gen_ai.embeddings.dimension.count"] == 2
    assert a["embedding.embeddings.0.embedding.text"] == "退货政策"


def test_text_embedding_batch_and_inferred_dimension(spans):
    TextEmbedding.call(model="text-embedding-v3", input=["退货政策", "发票抬头"])
    a = _one(spans).attributes
    assert a["embedding.embeddings.0.embedding.text"] == "退货政策"
    assert a["embedding.embeddings.1.embedding.text"] == "发票抬头"
    assert a["gen_ai.embeddings.dimension.count"] == 2  # Infer an omitted dimension from the returned vector length.
    assert "flashcat.content_truncated" not in a


def test_text_embedding_caps_texts(spans):
    texts = [f"doc-{i}" for i in range(40)]
    TextEmbedding.call(model="text-embedding-v3", input=texts)
    a = _one(spans).attributes
    assert a["embedding.embeddings.31.embedding.text"] == "doc-31"
    assert "embedding.embeddings.32.embedding.text" not in a
    assert a["flashcat.content_truncated"] is True


def test_text_embedding_no_content_when_capture_off(spans, monkeypatch):
    # The capture mode is read during instrumentation setup, so reapply instrumentation after turning capture off.
    DashScopeInstrumentor().uninstrument()
    monkeypatch.setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "NO_CONTENT")
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    DashScopeInstrumentor().instrument(tracer_provider=provider)
    TextEmbedding.call(model="text-embedding-v3", input="退货政策")
    a = _one(exporter).attributes
    assert a["gen_ai.operation.name"] == "embeddings"
    assert not any(k.startswith("embedding.embeddings.") for k in a)


def test_uninstrument_stops_spans(server, spans):
    DashScopeInstrumentor().uninstrument()
    Generation.call(model="qwen-plus", prompt="x")
    assert spans.get_finished_spans() == ()
    DashScopeInstrumentor().instrument(tracer_provider=TracerProvider())  # Restore instrumentation for fixture teardown.
