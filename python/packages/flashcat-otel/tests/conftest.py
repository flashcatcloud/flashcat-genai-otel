"""Provide a mock OTLP receiver that records requests and a mock OpenAI service. Run each scenario in its own subprocess to isolate global providers."""

from __future__ import annotations

import base64
import gzip
import json
import os
import subprocess
import sys
import textwrap
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest


def _any(v):
    kind = v.WhichOneof("value")
    if kind == "array_value":
        return [_any(x) for x in v.array_value.values]
    if kind == "kvlist_value":
        return {kv.key: _any(kv.value) for kv in v.kvlist_value.values}
    return getattr(v, kind) if kind else None


class Collector:
    def __init__(self) -> None:
        self.requests: list[dict] = []
        self.status = 200
        self.lock = threading.Lock()

    def spans(self) -> list[dict]:
        out = []
        for r in self.requests:
            if r["path"] != "/v1/traces" or r["status"] != 200:
                continue
            req = ExportTraceServiceRequest()
            req.ParseFromString(r["body"])
            for rs in req.resource_spans:
                res = {kv.key: _any(kv.value) for kv in rs.resource.attributes}
                for ss in rs.scope_spans:
                    for sp in ss.spans:
                        out.append({
                            "name": sp.name, "scope": ss.scope.name, "resource": res,
                            "span_id": sp.span_id.hex(), "parent": sp.parent_span_id.hex(),
                            "attrs": {kv.key: _any(kv.value) for kv in sp.attributes},
                            "status": sp.status.code, "kind": sp.kind,
                        })
        return out


def _serve(handler_cls):
    srv = ThreadingHTTPServer(("127.0.0.1", 0), handler_cls)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


@pytest.fixture
def collector():
    c = Collector()

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            if self.headers.get("Content-Encoding") == "gzip":
                body = gzip.decompress(body)
            with c.lock:
                c.requests.append({"path": self.path, "headers": dict(self.headers), "body": body, "status": c.status})
            self.send_response(c.status)
            self.send_header("Content-Type", "application/x-protobuf")
            self.end_headers()

    srv = _serve(H)
    c.url = f"http://127.0.0.1:{srv.server_address[1]}"
    yield c
    srv.shutdown()


@pytest.fixture
def fake_openai():
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            self.rfile.read(int(self.headers.get("Content-Length", 0)))
            payload = {
                "id": "chatcmpl-test", "object": "chat.completion", "created": 1761620000, "model": "gpt-4o-2024-08-06",
                "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "您的订单已发货"}}],
                "usage": {"prompt_tokens": 120, "completion_tokens": 12, "total_tokens": 132},
            }
            b = json.dumps(payload).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)

    srv = _serve(H)
    yield f"http://127.0.0.1:{srv.server_address[1]}/v1"
    srv.shutdown()


@pytest.fixture
def run_script():
    def run(code: str, env: dict | None = None) -> dict:
        full_env = {k: v for k, v in os.environ.items()
                    if not k.startswith(("OTEL_", "FLASHCAT_")) and "proxy" not in k.lower()}
        full_env.update(env or {})
        p = subprocess.run([sys.executable, "-c", textwrap.dedent(code)], env=full_env, capture_output=True, text=True, timeout=60)
        if p.returncode != 0:
            raise AssertionError(f"script failed:\nSTDOUT:{p.stdout}\nSTDERR:{p.stderr}")
        last = [l for l in p.stdout.splitlines() if l.startswith("RESULT:")]
        return json.loads(last[-1][len("RESULT:"):]) if last else {}

    return run


def basic(key: str) -> str:
    return "Basic " + base64.b64encode(key.encode()).decode()
