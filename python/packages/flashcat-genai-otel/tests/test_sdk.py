from __future__ import annotations

import json

import pytest

from conftest import basic

APP = """
import json
from flashcat import genai
diag = genai.init(service_name="kefu-agent", capture_content=CONTENT, debug=False)
import openai
client = openai.OpenAI(base_url=OPENAI_URL, api_key="sk-test")

@genai.tool("lookup_order")
def lookup_order(order_id):
    return {"order_id": order_id, "status": "shipped"}

@genai.agent("kefu")
def answer(q):
    lookup_order("A-1")
    r = client.chat.completions.create(model="gpt-4o", messages=[{"role": "user", "content": q}])
    return r.choices[0].message.content

with genai.session(conversation_id="c-123", user_id="u-9", tags={"team": "kf"}):
    answer(QUESTION)
genai.flush()
print("RESULT:" + json.dumps(genai.diagnostics(), default=str))
"""


def app(content: str, question: str, openai_url: str) -> str:
    return APP.replace("CONTENT", content).replace("OPENAI_URL", repr(openai_url)).replace("QUESTION", repr(question))


def test_situation_a_end_to_end(collector, fake_openai, run_script):
    diag = run_script(app("False", "我的订单到哪了", fake_openai), {
        "FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "fck_test:s3cret"})
    assert diag["provider"]["situation"] == "A" and diag["provider"]["exporter_mode"] == "add"
    statuses = {i["lib"]: i["status"] for i in diag["instrumentations"]}
    assert statuses["openai"] == "installed"
    assert diag["export"]["ok"] >= 3

    trace_reqs = [r for r in collector.requests if r["path"] == "/v1/traces"]
    hdrs = {k.lower(): v for k, v in trace_reqs[0]["headers"].items()}
    assert hdrs["authorization"] == basic("fck_test:s3cret")
    spans = collector.spans()
    by_op = {s["attrs"].get("gen_ai.operation.name"): s for s in spans}
    chat, agent_span, tool_span = by_op["chat"], by_op["invoke_agent"], by_op["execute_tool"]
    assert chat["resource"]["service.name"] == "kefu-agent"
    assert chat["resource"]["telemetry.distro.name"] == "flashcat-genai-otel"
    assert chat["attrs"]["gen_ai.conversation.id"] == "c-123"
    assert chat["attrs"]["user.id"] == "u-9" and chat["attrs"]["flashcat.tag.team"] == "kf"
    assert chat["attrs"]["gen_ai.usage.input_tokens"] == 120
    assert "gen_ai.input.messages" not in chat["attrs"], "Content capture should be off by default"
    assert chat["parent"] == agent_span["span_id"] and tool_span["parent"] == agent_span["span_id"]
    # The agent is the AI root. Its tool and instrumented OpenAI model call are descendants (contract §5.1).
    assert agent_span["attrs"]["flashcat.genai.root"] is True
    assert chat["attrs"]["flashcat.genai.root"] is False and tool_span["attrs"]["flashcat.genai.root"] is False
    assert agent_span["attrs"]["gen_ai.agent.name"] == "kefu"
    assert agent_span["attrs"].get("gen_ai.conversation.id") == "c-123"
    # The SDK derives the metrics endpoint from the traces endpoint.
    diag2 = diag
    assert diag2["provider"]["metrics"] == "add"


def test_content_span_with_masking(collector, fake_openai, run_script):
    run_script(app('"span"', "手机13812345678，邮箱a@b.com", fake_openai), {
        "FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "fck_test:s3cret"})
    chat = [s for s in collector.spans() if s["attrs"].get("gen_ai.operation.name") == "chat"][0]
    msgs = chat["attrs"]["gen_ai.input.messages"]
    assert "13812345678" not in msgs and "a@b.com" not in msgs and "[REDACTED:phone]" in msgs
    tool_span = [s for s in collector.spans() if s["attrs"].get("gen_ai.operation.name") == "execute_tool"][0]
    assert "A-1" in tool_span["attrs"]["gen_ai.tool.call.arguments"]


def test_reuse_when_already_exporting_to_flashcat(collector, run_script):
    diag = run_script(
        """
        import json
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider
        trace.set_tracer_provider(TracerProvider())
        from flashcat import genai
        d = genai.init(service_name="order-svc")
        print("RESULT:" + json.dumps(d))
        """,
        {"FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "k:s",
         "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": collector.url + "/v1/traces"},
    )
    assert diag["provider"]["situation"] == "C" and diag["provider"]["exporter_mode"] == "reuse"


def test_add_to_foreign_provider(collector, run_script):
    diag = run_script(
        """
        import json
        from opentelemetry import trace
        from opentelemetry.sdk.trace import TracerProvider
        trace.set_tracer_provider(TracerProvider())
        from flashcat import genai
        d = genai.init(service_name="x")
        print("RESULT:" + json.dumps(d))
        """,
        {"FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "k:s",
         "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://other-vendor.example.com:4318/v1/traces"},
    )
    assert diag["provider"]["situation"] == "B" and diag["provider"]["exporter_mode"] == "add"


def test_401_counted_in_diagnostics(collector, run_script):
    collector.status = 401
    diag = run_script(
        """
        import json
        from flashcat import genai
        genai.init(service_name="x", metrics=False)
        with genai.trace("job"):
            pass
        @genai.workflow("wf")
        def wf(): return 1
        wf()
        genai.flush()
        print("RESULT:" + json.dumps(genai.diagnostics()))
        """,
        {"FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "k:bad"},
    )
    assert diag["export"].get("http_401", 0) >= 1 and "401" in (diag["export"]["last_error"] or "")


def test_endpoint_without_path_rejected(run_script):
    diag = run_script(
        """
        import json
        from flashcat import genai
        print("RESULT:" + json.dumps(genai.init(service_name="x")))
        """,
        {"FLASHCAT_ENDPOINT": "ingest.example.com", "FLASHCAT_API_KEY": "k:s"},
    )
    assert any("完整 URL" in p for p in diag["problems"]) and diag["provider"]["exporter_mode"] == "none"


def test_missing_service_name(run_script):
    diag = run_script(
        """
        import json
        from flashcat import genai
        print("RESULT:" + json.dumps(genai.init()))
        """,
        {"FLASHCAT_ENDPOINT": "http://127.0.0.1:1/v1/traces", "FLASHCAT_API_KEY": "k:s"},
    )
    assert any("service_name" in p for p in diag["problems"])


def test_bad_capture_content_raises():
    from flashcat.genai import _config

    with pytest.raises(_config.ConfigError):
        _config.parse_content("yes")
    with pytest.raises(_config.ConfigError):
        _config.parse_content(True)


def test_decorators_preserve_behaviour(collector, run_script):
    out = run_script(
        """
        import asyncio, inspect, json
        from flashcat import genai
        genai.init(service_name="x", metrics=False)

        @genai.tool("boom")
        def boom(a, b=2):
            raise KeyError("no such order")

        @genai.tool("gen")
        def gen():
            yield 1
            yield 2

        @genai.agent("aio")
        async def aio(x):
            return x * 2

        res = {}
        try:
            boom(1)
        except KeyError as e:
            res["raised"] = str(e)
        res["sig"] = str(inspect.signature(boom))
        g = gen(); next(g); g.close()
        res["async"] = asyncio.run(aio(21))
        genai.flush()
        print("RESULT:" + json.dumps(res))
        """,
        {"FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "k:s"},
    )
    assert out == {"raised": "'no such order'", "sig": "(a, b=2)", "async": 42}
    spans = {s["attrs"].get("gen_ai.tool.name") or s["attrs"].get("gen_ai.agent.name"): s for s in collector.spans()}
    assert spans["boom"]["status"] == 2  # ERROR
    assert spans["boom"]["attrs"]["error.type"].endswith("KeyError")
    assert "gen" in spans and "aio" in spans


def test_conflict_detection_skips_foreign_wrapper(run_script):
    out = run_script(
        """
        import json, sys, types, wrapt
        mod = types.ModuleType("openinference.instrumentation.openai")
        sys.modules["openinference.instrumentation.openai"] = mod
        def _wrapper(wrapped, instance, args, kwargs):
            return wrapped(*args, **kwargs)
        _wrapper.__module__ = "openinference.instrumentation.openai"
        import openai.resources.chat.completions as c
        wrapt.wrap_function_wrapper(c, "Completions.create", _wrapper)
        from flashcat import genai
        d = genai.init(service_name="x", exporter="none")
        print("RESULT:" + json.dumps({i["lib"]: i for i in d["instrumentations"]}))
        """,
    )
    assert out["openai"]["status"] == "skipped_conflict"
    assert out["openai"]["foreign"] == ["openinference.instrumentation.openai"]


def test_key_masking_does_not_touch_token_counts():
    from opentelemetry.sdk.trace import ReadableSpan
    from flashcat.genai._processors import MaskingConfig, MaskingWrapper

    class Sink:
        got = None

        def on_end(self, s):
            Sink.got = s

        def on_start(self, *a):
            pass

    w = MaskingWrapper(Sink(), MaskingConfig(truncate_bytes=10), content_enabled=False)
    w.on_end(ReadableSpan(name="x", attributes={"http.request.header.authorization": "Bearer abc",
                                                 "gen_ai.usage.input_tokens": 5, "big": "中文中文中文"}))
    a = Sink.got.attributes
    assert a["http.request.header.authorization"] == "[REDACTED]"
    assert a["gen_ai.usage.input_tokens"] == 5
    assert a["big"] == "中文中" and a["flashcat.content_truncated"] is True


def test_dashscope_loaded_from_catalog(collector, run_script):
    """With flashcat-genai-otel[dashscope], init() loads DashScope instrumentation from the catalog. Its spans are children of the agent span and are masked."""
    pytest.importorskip("dashscope")
    pytest.importorskip("flashcat.instrumentation.dashscope")
    from http.server import BaseHTTPRequestHandler

    from conftest import _serve

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            self.rfile.read(int(self.headers.get("Content-Length", 0)))
            b = json.dumps({"request_id": "ds-1", "usage": {"input_tokens": 30, "output_tokens": 6, "total_tokens": 36},
                            "output": {"choices": [{"finish_reason": "stop",
                                                    "message": {"role": "assistant", "content": "已为 13812345678 查询"}}]}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(b)))
            self.end_headers()
            self.wfile.write(b)

    srv = _serve(H)
    try:
        diag = run_script(f"""
            import json
            from flashcat import genai
            genai.init(service_name="bailian-agent", capture_content="span")
            import dashscope
            from dashscope import Generation
            dashscope.base_http_api_url = "http://127.0.0.1:{srv.server_address[1]}/api/v1"

            @genai.agent("kefu")
            def answer(q):
                return Generation.call(model="qwen-plus", api_key="sk-test", result_format="message",
                                       messages=[{{"role": "user", "content": q}}])

            with genai.session(conversation_id="c-ds"):
                answer("手机 13812345678 的订单")
            genai.flush()
            print("RESULT:" + json.dumps(genai.diagnostics(), default=str))
        """, {"FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "fck_test:s3cret",
              "NO_PROXY": "127.0.0.1"})
    finally:
        srv.shutdown()

    statuses = {i["lib"]: i["status"] for i in diag["instrumentations"]}
    assert statuses["dashscope"] == "installed"
    by_op = {s["attrs"].get("gen_ai.operation.name"): s for s in collector.spans()}
    chat, agent_span = by_op["chat"], by_op["invoke_agent"]
    assert chat["scope"] == "flashcat.instrumentation.dashscope"
    assert chat["attrs"]["gen_ai.provider.name"] == "dashscope"
    assert chat["attrs"]["gen_ai.usage.input_tokens"] == 30
    assert chat["attrs"]["gen_ai.conversation.id"] == "c-ds"
    assert chat["parent"] == agent_span["span_id"]
    assert agent_span["attrs"]["flashcat.genai.root"] is True and chat["attrs"]["flashcat.genai.root"] is False
    assert "13812345678" not in chat["attrs"]["gen_ai.input.messages"] + chat["attrs"]["gen_ai.output.messages"]


# ---- Cross-language contract conformance (spec/contract.md) ----


def test_masking_fixtures():
    """Use spec/fixtures/masking.json as the reference shared by all four languages."""
    import pathlib
    import re

    from flashcat.genai._processors import _SENSITIVE_KEY, DEFAULT_PATTERNS, replace_emails

    fx = json.loads((pathlib.Path(__file__).parents[4] / "spec/fixtures/masking.json").read_text())
    for c in fx["key_cases"]:
        assert bool(_SENSITIVE_KEY.search(c["key"])) is c["masked"], c["key"]
    for c in fx["content_cases"]:
        s = c["input"]
        for name, pattern, repl in DEFAULT_PATTERNS:
            s = replace_emails(s, repl) if name == "email" else re.sub(pattern, repl, s)
        assert s == c["output"], c["input"]


def test_email_rule_linear_and_identical_to_regex():
    import random
    import re
    import time

    from flashcat.genai._processors import _EMAIL_PATTERN, replace_emails

    rx = re.compile(_EMAIL_PATTERN)
    rnd = random.Random(7)
    alphabet = "ab1.@-_%+ X中\n"
    cases = ["", "@", "a@b", "a@b.c", "a@b.cn", "x a@b.co.uk.", "a@@b.cn", "a@b..cn", "a@.cn", "@b.cn", "a@b.c1", "a.b@c-d.ef@g.hi"]
    cases += ["".join(rnd.choice(alphabet) for _ in range(rnd.randint(1, 40))) for _ in range(3000)]
    for s in cases:
        assert replace_emails(s, "[E]") == rx.sub("[E]", s), repr(s)
    # The regex would take tens of seconds on this 256 KiB string without an @.
    blob = "A" * (256 * 1024)
    t0 = time.perf_counter()
    assert replace_emails(blob, "[E]") == blob
    assert replace_emails(blob + "@x.cn", "[E]") == "[E]"
    assert time.perf_counter() - t0 < 1.0


def test_masking_wrapper_uses_linear_email_rule():
    import time

    from opentelemetry.sdk.trace import ReadableSpan

    from flashcat.genai._processors import MaskingConfig, MaskingWrapper

    got = {}

    class Sink:
        def on_end(self, s):
            got["span"] = s

    w = MaskingWrapper(Sink(), MaskingConfig(), content_enabled=True)
    t0 = time.perf_counter()
    w.on_end(ReadableSpan(name="x", attributes={"gen_ai.tool.call.result": "A" * (200 * 1024) + " li@corp.com.cn"}))
    assert time.perf_counter() - t0 < 1.0
    assert got["span"].attributes["gen_ai.tool.call.result"].endswith(" [REDACTED:email]")


def test_session_covers_every_span_and_inherits_from_parent(collector, run_script):
    run_script(
        """
        from opentelemetry import trace
        from flashcat import genai
        genai.init(service_name="x", metrics=False)
        tr = trace.get_tracer("biz")
        with tr.start_as_current_span("outside"):
            pass
        with genai.session(conversation_id="c-1", user_id="u-1", tags={"team": "kf"}):
            with tr.start_as_current_span("plain"):
                pass
            with tr.start_as_current_span("keeps", attributes={"gen_ai.conversation.id": "own"}):
                pass
        # The child inherits session attributes from its parent even outside a session scope.
        with tr.start_as_current_span("parent", attributes={"gen_ai.conversation.id": "c-2", "user.id": "u-2", "flashcat.tag.k": "v"}):
            with tr.start_as_current_span("child"):
                pass
        genai.flush()
        """,
        {"FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "k:s"},
    )
    spans = {s["name"]: s["attrs"] for s in collector.spans()}
    assert "gen_ai.conversation.id" not in spans["outside"] and "user.id" not in spans["outside"]
    assert spans["plain"]["gen_ai.conversation.id"] == "c-1"
    assert spans["plain"]["user.id"] == "u-1" and spans["plain"]["flashcat.tag.team"] == "kf"
    assert spans["keeps"]["gen_ai.conversation.id"] == "own"
    assert spans["child"]["gen_ai.conversation.id"] == "c-2"
    assert spans["child"]["user.id"] == "u-2" and spans["child"]["flashcat.tag.k"] == "v"


def test_env_capture_content_invalid_is_a_problem_not_an_error(run_script):
    script = """
        import json
        from flashcat import genai
        d = genai.init(service_name="x", exporter="none", metrics=False)
        print("RESULT:" + json.dumps({"problems": d["problems"], "content": d["content"], "sdk": d["sdk"]}))
        """
    out = run_script(script, {"FLASHCAT_CAPTURE_CONTENT": "maybe"})
    assert any("FLASHCAT_CAPTURE_CONTENT" in p for p in out["problems"])
    assert out["content"]["upstream"] == "NO_CONTENT"
    assert out["sdk"]["language"] == "python"
    # Upstream instrumentation in other languages uses booleans. Treat true as capture enabled.
    out = run_script(script, {"OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": "true"})
    assert out["problems"] == [] and out["content"]["upstream"] == "SPAN_ONLY"


def test_genai_root_marks_spans_without_genai_ancestor(collector, run_script):
    run_script(
        """
        from opentelemetry import trace
        from flashcat import genai
        genai.init(service_name="x", metrics=False)
        tr = trace.get_tracer("biz")
        # The agent is still an AI root under an HTTP span, and the model call has a GenAI ancestor through the plain span.
        with tr.start_as_current_span("POST /chat"):
            with tr.start_as_current_span("invoke_agent kefu", attributes={"gen_ai.operation.name": "invoke_agent"}):
                with tr.start_as_current_span("retrieve"):
                    with tr.start_as_current_span("chat gpt-4o", attributes={"gen_ai.operation.name": "chat"}):
                        pass
        # The export pipeline uses final attributes to mark spans that become GenAI after they start.
        with tr.start_as_current_span("late") as s:
            s.set_attribute("gen_ai.operation.name", "chat")
        with tr.start_as_current_span("invoke_agent outer", attributes={"gen_ai.operation.name": "invoke_agent"}):
            with tr.start_as_current_span("late child") as s:
                s.set_attribute("gen_ai.operation.name", "chat")
        # The child sees GenAI attributes added after its parent started.
        with tr.start_as_current_span("parent late") as p:
            p.set_attribute("openinference.span.kind", "AGENT")
            with tr.start_as_current_span("child of late", attributes={"gen_ai.operation.name": "chat"}):
                pass
        # Session attributes alone do not make this plain span a GenAI span.
        with genai.session(conversation_id="c-1"):
            with tr.start_as_current_span("plain"):
                pass
        genai.flush()
        """,
        {"FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "k:s"},
    )
    spans = {s["name"]: s["attrs"] for s in collector.spans()}
    for name in ("POST /chat", "retrieve", "plain"):
        assert "flashcat.genai.root" not in spans[name], name
    assert spans["invoke_agent kefu"]["flashcat.genai.root"] is True
    assert spans["chat gpt-4o"]["flashcat.genai.root"] is False
    assert spans["late"]["flashcat.genai.root"] is True
    assert spans["invoke_agent outer"]["flashcat.genai.root"] is True
    assert spans["late child"]["flashcat.genai.root"] is False
    assert spans["parent late"]["flashcat.genai.root"] is True
    assert spans["child of late"]["flashcat.genai.root"] is False


def test_genai_root_ignores_remote_parent_for_now():
    # A GenAI span with a remote parent is treated as an AI root until ancestry propagates across processes (contract §5.1).
    from opentelemetry import trace as trace_api
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.trace import NonRecordingSpan, SpanContext, TraceFlags

    from flashcat.genai._processors import ROOT_ATTR, ContextEnricher

    provider = TracerProvider()
    enricher = ContextEnricher()
    provider.add_span_processor(enricher)
    remote = NonRecordingSpan(SpanContext(trace_id=1, span_id=2, is_remote=True, trace_flags=TraceFlags(1)))
    ctx = trace_api.set_span_in_context(remote)
    span = provider.get_tracer("t").start_span("chat", context=ctx, attributes={"gen_ai.operation.name": "chat"})
    assert span.attributes[ROOT_ATTR] is True
    span.end()


def test_genai_root_handles_instrumentation_that_sets_attributes_at_end(collector, run_script):
    # OpenInference LangChain adds attributes at span end. The instrumentation scope identifies the chain as the AI root above the model call.
    run_script(
        """
        from opentelemetry import trace
        from flashcat import genai
        genai.init(service_name="x", metrics=False)
        biz = trace.get_tracer("biz")
        oi = trace.get_tracer("openinference.instrumentation.langchain")
        with biz.start_as_current_span("POST /chat"):
            with oi.start_as_current_span("RunnableSequence") as chain:
                with oi.start_as_current_span("ChatOpenAI") as llm:
                    llm.set_attribute("openinference.span.kind", "LLM")
                chain.set_attribute("openinference.span.kind", "CHAIN")
        genai.flush()
        """,
        {"FLASHCAT_ENDPOINT": collector.url + "/v1/traces", "FLASHCAT_API_KEY": "k:s"},
    )
    spans = {s["name"]: s["attrs"] for s in collector.spans()}
    assert "flashcat.genai.root" not in spans["POST /chat"]
    assert spans["RunnableSequence"]["flashcat.genai.root"] is True
    assert spans["ChatOpenAI"]["flashcat.genai.root"] is False
