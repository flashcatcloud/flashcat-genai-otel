"""Test manual llm, embedding, and retrieval spans and annotate (contract §4, §4.1, and §4.2)."""

from __future__ import annotations

import json

ENV = {"FLASHCAT_API_KEY": "k:s"}
SPAN_KIND_CLIENT = 3  # OTLP Span.SpanKind.SPAN_KIND_CLIENT


def env(collector) -> dict:
    return {**ENV, "FLASHCAT_ENDPOINT": collector.url + "/v1/traces"}


def by_op(spans: list[dict]) -> dict:
    return {s["attrs"].get("gen_ai.operation.name"): s for s in spans}


def test_llm_embedding_retrieval_with_annotate(collector, run_script):
    run_script(
        """
        import json
        from flashcat import genai
        genai.init(service_name="x", capture_content="span", metrics=False)

        @genai.llm("qwen-max", provider="dashscope")
        def ask(prompt):
            genai.annotate(
                input=[{"role": "system", "content": "你是客服"}, {"role": "user", "content": prompt}],
                output="已为您查询",
                model="qwen-max-0919",
                usage={"input_tokens": 30, "output_tokens": 5, "cache_read_input_tokens": 10,
                       "cache_write_input_tokens": 2, "bogus": 1},
                finish_reasons=["stop"],
                tags={"route": "gateway"},
            )
            return {"raw": "ignored because annotate set output"}

        @genai.embedding("text-embedding-v3", provider="dashscope")
        def embed(texts):
            genai.annotate(usage={"input_tokens": 8, "output_tokens": 99}, model="text-embedding-v3")
            return [[0.1, 0.2]]

        @genai.retrieval("kb-orders")
        def search(query):
            return [{"id": "d1", "content": "订单联系人 13812345678", "score": 0.9}, "第二段文本"]

        @genai.agent("kefu")
        def handle(q):
            embed([q])
            search(q)
            return ask(q)

        handle("我的订单到哪了")
        genai.flush()
        print("RESULT:" + json.dumps(genai.diagnostics()["semantics"]))
        """,
        env(collector),
    )
    ops = by_op(collector.spans())
    chat, emb, ret, agent = ops["chat"], ops["embeddings"], ops["retrieval"], ops["invoke_agent"]

    assert chat["name"] == "chat qwen-max" and chat["kind"] == SPAN_KIND_CLIENT
    a = chat["attrs"]
    assert a["gen_ai.request.model"] == "qwen-max" and a["gen_ai.provider.name"] == "dashscope"
    assert a["gen_ai.response.model"] == "qwen-max-0919"
    assert a["gen_ai.usage.input_tokens"] == 30 and a["gen_ai.usage.output_tokens"] == 5
    assert a["gen_ai.usage.cache_read.input_tokens"] == 10
    assert a["gen_ai.usage.cache_write.input_tokens"] == 2
    assert a["gen_ai.response.finish_reasons"] == ["stop"]
    assert a["flashcat.tag.route"] == "gateway"
    msgs = json.loads(a["gen_ai.input.messages"])
    assert [m["role"] for m in msgs] == ["system", "user"]
    assert msgs[1]["parts"][0]["content"] == "我的订单到哪了"
    out = json.loads(a["gen_ai.output.messages"])
    assert out[0]["role"] == "assistant" and out[0]["parts"][0]["content"] == "已为您查询"
    assert chat["parent"] == agent["span_id"]

    assert emb["name"] == "embeddings text-embedding-v3" and emb["kind"] == SPAN_KIND_CLIENT
    assert emb["attrs"]["gen_ai.usage.input_tokens"] == 8
    assert "gen_ai.usage.output_tokens" not in emb["attrs"], "Embedding spans should record only input tokens"
    assert "gen_ai.input.messages" not in emb["attrs"], "Embedding spans should omit content"

    assert ret["name"] == "retrieval kb-orders" and ret["attrs"]["gen_ai.data_source.id"] == "kb-orders"
    assert "我的订单到哪了" in ret["attrs"]["gen_ai.retrieval.query.text"]
    docs = json.loads(ret["attrs"]["gen_ai.retrieval.documents"])
    assert docs[0]["id"] == "d1" and docs[1] == {"content": "第二段文本"}
    assert "13812345678" not in ret["attrs"]["gen_ai.retrieval.documents"], "Retrieval content should also be masked"
    assert "[REDACTED:phone]" in ret["attrs"]["gen_ai.retrieval.documents"]


def test_annotate_respects_content_switch_and_scope(collector, run_script):
    out = run_script(
        """
        import json
        from flashcat import genai
        genai.init(service_name="x", metrics=False)  # Content capture is off by default.

        genai.annotate(output="不在语义 span 里", tags={"a": "b"})  # A no-op that should succeed.

        @genai.llm("m1")
        def ask(prompt):
            genai.annotate(input="secret prompt", output="secret answer", usage={"input_tokens": 3})
            return "x"

        @genai.tool("t1")
        def tool_fn(x):
            genai.annotate(output="ignored: content off")
            return x

        with genai.trace("job"):
            genai.annotate(tags={"plain": "1"})  # trace creates a plain span outside the semantic API.
            ask("p")
            tool_fn(1)
        genai.flush()
        print("RESULT:" + json.dumps({"ok": True}))
        """,
        env(collector),
    )
    assert out == {"ok": True}
    spans = collector.spans()
    ops = by_op(spans)
    chat = ops["chat"]
    assert chat["attrs"]["gen_ai.usage.input_tokens"] == 3
    for k in ("gen_ai.input.messages", "gen_ai.output.messages"):
        assert k not in chat["attrs"]
    assert "gen_ai.tool.call.result" not in ops["execute_tool"]["attrs"]
    job = [s for s in spans if s["name"] == "job"][0]
    assert "flashcat.tag.plain" not in job["attrs"]


def test_annotate_output_wins_over_return_value(collector, run_script):
    run_script(
        """
        from flashcat import genai
        genai.init(service_name="x", capture_content="span", metrics=False)

        @genai.tool("lookup")
        def lookup(order_id):
            genai.annotate(output={"status": "shipped"})
            return "raw-object-repr"

        @genai.retrieval("kb")
        def search(q):
            return "只有一段"

        lookup("A-1")
        search("q")
        genai.flush()
        """,
        env(collector),
    )
    ops = by_op(collector.spans())
    assert json.loads(ops["execute_tool"]["attrs"]["gen_ai.tool.call.result"]) == {"status": "shipped"}
    assert json.loads(ops["retrieval"]["attrs"]["gen_ai.retrieval.documents"]) == [{"content": "只有一段"}]


def test_manual_llm_merges_instrumented_inner_call(collector, fake_openai, run_script):
    """Verify that util-genai suppresses the nested OpenAI span and adds its usage to the outer manual llm span."""
    sem = run_script(
        f"""
        import json
        from flashcat import genai
        genai.init(service_name="x", metrics=False)
        import openai
        client = openai.OpenAI(base_url={fake_openai!r}, api_key="sk-test")

        @genai.llm("gpt-4o", provider="openai")
        def ask(q):
            return client.chat.completions.create(model="gpt-4o", messages=[{{"role": "user", "content": q}}])

        ask("hi")
        genai.flush()
        print("RESULT:" + json.dumps(genai.diagnostics()["semantics"]))
        """,
        env(collector),
    )
    chats = [s for s in collector.spans() if s["attrs"].get("gen_ai.operation.name") == "chat"]
    assert len(chats) == 1, [c["scope"] for c in chats]
    assert chats[0]["scope"] == "flashcat.genai"
    assert chats[0]["attrs"]["gen_ai.usage.input_tokens"] == 120
    assert sem["nested_inference_spans"] == 0


def test_nested_inference_from_other_sources_is_reported(collector, run_script):
    diag = run_script(
        """
        import json
        from opentelemetry import trace
        from flashcat import genai
        genai.init(service_name="x", metrics=False)
        tracer = trace.get_tracer("openinference.instrumentation.fake")

        @genai.llm("gpt-4o")
        def ask(q):
            # Simulate instrumentation outside util-genai, such as OpenInference, that only sets openinference.span.kind at span start.
            with tracer.start_as_current_span("ChatCompletion", attributes={"openinference.span.kind": "LLM"}):
                pass
            with tracer.start_as_current_span("ChatCompletion", attributes={"openinference.span.kind": "LLM"}):
                pass

        ask("hi")
        genai.flush()
        print("RESULT:" + json.dumps(genai.diagnostics()))
        """,
        env(collector),
    )
    assert diag["semantics"]["nested_inference_spans"] == 2
    assert sum("重复计数" in p for p in diag["problems"]) == 1
