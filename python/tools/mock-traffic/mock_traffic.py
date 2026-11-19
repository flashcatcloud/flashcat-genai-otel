"""Demo traffic for Flashcat AI agent observability: continuously simulates normal calls of an AI application, with occasional errors.

Goes through the real flashcat-otel SDK and instrumentations (OpenAI-compatible, DashScope). The model service is an
in-process fake, so no real model is called and nothing is billed. One process simulates one application (OTel's
service.name is per process):

    python mock_traffic.py --profile kefu        # customer service: OpenAI-compatible SDK calling Zhipu / Kimi, multi-turn sessions, content with phone numbers / emails (to see masking)
    python mock_traffic.py --profile order       # orders: multi-agent collaboration, DeepSeek, partly streaming
    python mock_traffic.py --profile rag         # knowledge-base Q&A: native DashScope SDK, embedding -> retrieval -> generation

Environment variables:
    FLASHCAT_ENDPOINT / FLASHCAT_API_KEY   ingest endpoint and key (required)
    MOCK_INTERVAL     average seconds between runs, default 4
    MOCK_FAULT_RATE   share of runs with injected faults, default 0.03 (plus a few slow requests, +1.5-2.8 s, no timeouts)
    MOCK_ENV          deployment.environment.name, default prod
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import random
import signal
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

log = logging.getLogger("mock")

# ---------------------------------------------------------------------------
# Call arm() before the model call. The mock model server calls take() when it handles the request.
# The server runs on a separate thread, so protect the shared fault slot with a lock. Each process has one caller thread.
# ---------------------------------------------------------------------------

_pending_fault: dict[str, str | None] = {"kind": None}
_pending_lock = threading.Lock()


def arm(kind: str | None) -> None:
    with _pending_lock:
        _pending_fault["kind"] = kind


def take() -> str | None:
    with _pending_lock:
        k = _pending_fault["kind"]
        _pending_fault["kind"] = None
        return k


def tokens_for(text: str) -> int:
    # Estimate one token per Chinese character and one per four English characters.
    return max(8, int(sum(1 if ord(c) > 127 else 0.25 for c in text)))


# ---------------------------------------------------------------------------
# Mock model server for OpenAI-compatible and DashScope requests.
# ---------------------------------------------------------------------------


class FakeModels(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _json(self, status: int, body: dict, headers: dict | None = None) -> None:
        b = json.dumps(body, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(b)

    def do_POST(self):  # noqa: N802
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        fault = take()
        if "/chat/completions" in self.path:
            self._openai(body, fault)
        elif "text-embedding" in self.path:
            self._ds_embedding(body, fault)
        else:
            self._ds_generation(body, fault)

    # Use log-normal latency with occasional slow requests capped below the client's four-second timeout.
    # Only injected timeouts can exceed the limit, so MOCK_FAULT_RATE controls the error rate.
    @staticmethod
    def _latency(base: float, fault: str | None) -> None:
        if fault == "timeout":
            time.sleep(6)
            return
        d = random.lognormvariate(0, 0.35) * base
        if fault == "slow" or random.random() < 0.01:
            d += random.uniform(1.5, 2.8)
        time.sleep(min(d, 3.6))

    def _openai(self, body: dict, fault: str | None) -> None:
        model = body.get("model", "glm-5.3-flash")
        self._latency(0.6 if "flash" in model else 1.1, fault)
        if fault == "rate_limit":
            return self._json(429, {"error": {"message": "Rate limit reached for requests", "type": "requests", "code": "rate_limit_exceeded"}}, {"retry-after": "1"})
        if fault == "server_error":
            return self._json(503, {"error": {"message": "The server is overloaded, please try again later", "type": "server_error"}})
        prompt = "".join(m.get("content") or "" for m in body.get("messages", []) if isinstance(m.get("content"), str))
        reply = random.choice(REPLIES.get(body.get("_mock_topic", ""), REPLIES["default"]))
        prompt_tokens = tokens_for(prompt) + random.randint(60, 240)
        completion_tokens = tokens_for(reply) + random.randint(0, 40)
        cached = int(prompt_tokens * random.uniform(0.3, 0.7)) if random.random() < 0.6 else 0
        resp_model = model
        usage = None if fault == "no_usage" else {
            "prompt_tokens": prompt_tokens, "completion_tokens": completion_tokens, "total_tokens": prompt_tokens + completion_tokens,
            "prompt_tokens_details": {"cached_tokens": cached},
        }
        rid = f"chatcmpl-{uuid.uuid4().hex[:24]}"
        if body.get("stream"):
            return self._openai_stream(rid, resp_model, reply, usage if (body.get("stream_options") or {}).get("include_usage") else None)
        payload = {"id": rid, "object": "chat.completion", "created": int(time.time()), "model": resp_model,
                   "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": reply}}]}
        if usage:
            payload["usage"] = usage
        self._json(200, payload)

    def _openai_stream(self, rid: str, model: str, reply: str, usage: dict | None) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        def send(obj: dict | str) -> None:
            data = obj if isinstance(obj, str) else json.dumps(obj, ensure_ascii=False)
            chunk = f"data: {data}\n\n".encode()
            self.wfile.write(f"{len(chunk):x}\r\n".encode() + chunk + b"\r\n")
            self.wfile.flush()

        base = {"id": rid, "object": "chat.completion.chunk", "created": int(time.time()), "model": model}
        for i in range(0, len(reply), 6):
            send({**base, "choices": [{"index": 0, "delta": {"content": reply[i:i + 6]}, "finish_reason": None}]})
            time.sleep(0.03)
        send({**base, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
        if usage:
            send({**base, "choices": [], "usage": usage})
        send("[DONE]")
        self.wfile.write(b"0\r\n\r\n")

    def _ds_generation(self, body: dict, fault: str | None) -> None:
        self._latency(0.9, fault)
        rid = str(uuid.uuid4())
        if fault == "rate_limit":
            return self._json(429, {"request_id": rid, "code": "Throttling.RateQuota", "message": "Requests rate limit exceeded, please try again later."})
        if fault == "server_error":
            return self._json(500, {"request_id": rid, "code": "InternalError.Algo", "message": "An internal error has occured, please try again later."})
        msgs = (body.get("input") or {}).get("messages") or []
        prompt = "".join(str(m.get("content", "")) for m in msgs)
        reply = random.choice(REPLIES["rag"])
        it, ot = tokens_for(prompt) + random.randint(200, 900), tokens_for(reply) + random.randint(0, 60)
        usage = None if fault == "no_usage" else {"input_tokens": it, "output_tokens": ot, "total_tokens": it + ot}
        out = {"request_id": rid, "output": {"choices": [{"finish_reason": "stop", "message": {"role": "assistant", "content": reply}}]}}
        if usage:
            out["usage"] = usage
        self._json(200, out)

    def _ds_embedding(self, body: dict, fault: str | None) -> None:
        self._latency(0.15, fault)
        texts = (body.get("input") or {}).get("texts") or [""]
        if isinstance(texts, str):
            texts = [texts]
        self._json(200, {"request_id": str(uuid.uuid4()), "output": {"embeddings": [{"text_index": i, "embedding": [0.01] * 8} for i in range(len(texts))]},
                         "usage": {"total_tokens": sum(tokens_for(t) for t in texts)}})


class QuietServer(ThreadingHTTPServer):
    """Suppress tracebacks for expected connection resets when the client times out before the mock server writes its response."""

    daemon_threads = True

    def handle_error(self, request, client_address):
        import sys

        if isinstance(sys.exc_info()[1], (ConnectionResetError, BrokenPipeError)):
            return
        super().handle_error(request, client_address)


REPLIES = {
    "default": ["好的，已经为您处理。", "收到，我来帮您看一下。"],
    "order": [
        "您的订单已从上海仓发出，预计明天下午送达，物流单号 SF1048832291。",
        "订单还在打包中，今天 18 点前会发货，发货后会短信通知您。",
        "这笔订单已签收，如果没有收到包裹，可以申请人工客服核实。",
    ],
    "refund": [
        "未拆封商品 7 天内可以无理由退货，运费由我们承担。请在订单详情页点“申请退货”。",
        "退款会在仓库收到退货后 1–3 个工作日原路退回。",
    ],
    "invoice": ["电子发票会在确认收货后 24 小时内发送到您的邮箱。", "发票抬头可以在订单详情页修改，修改后会重新开具。"],
    "plan": ['{"steps": ["query_logistics", "estimate_eta", "notify_user"], "priority": "normal"}'],
    "logistics": ["包裹目前在杭州转运中心，受天气影响预计晚到半天，已为您申请延误补偿券。", "包裹已到达派送站点，快递员会在 2 小时内联系您。"],
    "rag": [
        "根据《员工差旅报销制度》第 4 条，一线城市住宿标准为每晚 600 元，超出部分需部门负责人审批。",
        "根据知识库文档，生产环境变更需要在变更平台提交工单，并在每周二、四的变更窗口内执行。",
        "VPN 账号可在 IT 服务台自助开通，首次登录需绑定手机令牌。",
    ],
}

# ---------------------------------------------------------------------------
# Simulated application workloads.
# ---------------------------------------------------------------------------

PHONES = ["13812345678", "13987654321", "15011112222", "18600001111"]
EMAILS = ["zhang.wei@example.com", "li.na@example.com", "wang.fang@example.com"]
SURNAMES = "张王李赵刘陈杨黄周吴"


class Sessions:
    """Maintain a pool of active sessions. Replace each session after one to five turns."""

    def __init__(self, prefix: str, size: int = 6) -> None:
        self.prefix, self.size, self.pool = prefix, size, []

    def pick(self) -> dict:
        self.pool = [s for s in self.pool if s["left"] > 0]
        if len(self.pool) < self.size and (not self.pool or random.random() < 0.35):
            self.pool.append({"id": f"{self.prefix}-{uuid.uuid4().hex[:10]}", "user": f"u{random.randint(1000, 1099)}", "left": random.randint(1, 5)})
        s = random.choice(self.pool)
        s["left"] -= 1
        return s


def pick_fault(rate: float, kinds: list[tuple[str, float]]) -> str | None:
    if random.random() >= rate:
        return "slow" if random.random() < 0.02 else None
    total = sum(w for _, w in kinds)
    r = random.uniform(0, total)
    for k, w in kinds:
        r -= w
        if r <= 0:
            return k
    return kinds[-1][0]


def build_kefu(otel, base_url: str):
    import openai

    client = openai.OpenAI(base_url=f"{base_url}/v1", api_key="sk-mock", timeout=4, max_retries=0)
    sessions = Sessions("kefu")
    faults = [("rate_limit", 3), ("server_error", 2), ("timeout", 1), ("tool_error", 3), ("no_usage", 1)]

    @otel.tool("lookup_order")
    def lookup_order(order_id: str, fault: str | None):
        time.sleep(random.uniform(0.02, 0.12))
        if fault == "tool_error":
            raise TimeoutError(f"order-service timeout after 3000ms (order_id={order_id})")
        return {"order_id": order_id, "status": random.choice(["shipped", "packing", "delivered"]), "warehouse": "SH-02"}

    @otel.tool("search_faq")
    def search_faq(query: str):
        time.sleep(random.uniform(0.01, 0.05))
        return {"hits": random.randint(1, 5), "top": "退换货政策"}

    @otel.agent("kefu")
    def answer(question: str, topic: str, fault: str | None):
        # Use only Chinese models in the demo. Match names in the bundled pricing catalog, which covers Chinese providers and OpenRouter as of 2026-11-18.
        model = "kimi-k2.6" if random.random() < 0.25 else "glm-5.3-flash"
        context = ""
        if topic == "order":
            try:
                context = json.dumps(lookup_order(f"A{random.randint(10000, 99999)}", fault), ensure_ascii=False)
            except TimeoutError:
                context = "订单服务暂时不可用"
        else:
            context = json.dumps(search_faq(question), ensure_ascii=False)
        arm(fault if fault in ("rate_limit", "server_error", "timeout", "no_usage") else None)
        r = client.chat.completions.create(
            model=model,
            messages=[{"role": "system", "content": "你是电商客服助手，回答简洁，不要泄露用户隐私。"},
                      {"role": "user", "content": f"{question}\n\n参考信息：{context}"}],
            extra_body={"_mock_topic": topic},
        )
        return r.choices[0].message.content

    questions = {
        "order": ["我的订单到哪了？手机号 {phone}", "订单 A{num} 怎么还没发货", "帮我查下物流，收件人{name}，电话 {phone}"],
        "refund": ["买的耳机不想要了能退吗", "退款多久到账？我的邮箱是 {email}", "拆封了还能退货吗"],
        "invoice": ["怎么开发票", "发票抬头写错了，邮箱 {email}，能重开吗"],
    }

    def run_once(fault_rate: float) -> None:
        s = sessions.pick()
        fault = pick_fault(fault_rate, faults)
        # Only order lookups can raise tool errors, so force the order topic when injecting one.
        topic = "order" if fault == "tool_error" else random.choice(["order", "order", "refund", "invoice"])
        q = random.choice(questions[topic]).format(phone=random.choice(PHONES), email=random.choice(EMAILS), num=random.randint(10000, 99999), name=random.choice(SURNAMES) + "先生")
        with otel.session(conversation_id=s["id"], user_id=s["user"], tags={"channel": random.choice(["app", "web", "miniprogram"]), "topic": topic}):
            answer(q, topic, fault)

    return run_once


def build_order(otel, base_url: str):
    import openai

    client = openai.OpenAI(base_url=f"{base_url}/v1", api_key="sk-mock", timeout=4, max_retries=0)
    sessions = Sessions("order", size=4)
    faults = [("rate_limit", 2), ("server_error", 3), ("timeout", 1), ("tool_error", 4), ("no_usage", 1)]

    def chat(topic: str, prompt: str, fault: str | None, stream: bool = False) -> str:
        arm(fault)
        kwargs = dict(model="deepseek-v4-pro" if random.random() < 0.2 else "deepseek-flash", messages=[{"role": "user", "content": prompt}], extra_body={"_mock_topic": topic})
        if stream:
            out = []
            for ch in client.chat.completions.create(stream=True, stream_options={"include_usage": True}, **kwargs):
                if ch.choices and ch.choices[0].delta.content:
                    out.append(ch.choices[0].delta.content)
            return "".join(out)
        return client.chat.completions.create(**kwargs).choices[0].message.content

    @otel.tool("query_logistics")
    def query_logistics(order_id: str, fault: str | None):
        time.sleep(random.uniform(0.05, 0.3))
        if fault == "tool_error":
            raise ConnectionError("logistics-gateway: upstream connect error or disconnect/reset before headers")
        return {"order_id": order_id, "node": random.choice(["杭州转运中心", "上海分拨中心", "派送站点"])}

    @otel.tool("notify_user")
    def notify_user(user: str, text: str):
        time.sleep(random.uniform(0.01, 0.04))
        return {"sent": True}

    @otel.agent("logistics-specialist")
    def logistics(order_id: str, fault: str | None):
        info = query_logistics(order_id, fault)
        return chat("logistics", f"根据物流信息给用户写一句说明：{json.dumps(info, ensure_ascii=False)}", fault if fault in ("rate_limit", "server_error", "timeout") else None, stream=random.random() < 0.5)

    @otel.agent("order-planner")
    def plan_and_run(order_id: str, user: str, fault: str | None):
        plan_fault = fault if fault == "no_usage" else None
        chat("plan", f"用户询问订单 {order_id} 的配送进度，给出处理步骤", plan_fault)
        msg = logistics(order_id, fault)
        notify_user(user, msg)
        return msg

    def run_once(fault_rate: float) -> None:
        s = sessions.pick()
        fault = pick_fault(fault_rate, faults)
        with otel.session(conversation_id=s["id"], user_id=s["user"], tags={"channel": "app", "tier": random.choice(["vip", "normal", "normal"])}):
            plan_and_run(f"A{random.randint(10000, 99999)}", s["user"], fault)

    return run_once


def build_rag(otel, base_url: str):
    import dashscope
    from dashscope import Generation, TextEmbedding

    dashscope.base_http_api_url = f"{base_url}/api/v1"
    dashscope.api_key = "sk-mock"
    sessions = Sessions("rag", size=5)
    faults = [("rate_limit", 4), ("server_error", 2), ("tool_error", 3), ("no_usage", 1)]
    docs = ["员工差旅报销制度.pdf", "生产变更管理规范.md", "IT 服务台 FAQ.md", "新人入职指南.docx", "信息安全管理办法.pdf"]

    @otel.tool("vector_search")
    def vector_search(query: str, fault: str | None):
        time.sleep(random.uniform(0.03, 0.2))
        if fault == "tool_error":
            raise RuntimeError("milvus: collection kb_v3 not loaded")
        return [{"doc": d, "score": round(random.uniform(0.62, 0.93), 3)} for d in random.sample(docs, 3)]

    @otel.agent("kb-assistant")
    def ask(question: str, fault: str | None):
        TextEmbedding.call(model="text-embedding-v3", input=question)
        try:
            hits = vector_search(question, fault)
        except RuntimeError:
            hits = []
        arm(fault if fault in ("rate_limit", "server_error", "no_usage") else None)
        model = "qwen-max" if random.random() < 0.2 else "qwen-plus"
        r = Generation.call(model=model, result_format="message", messages=[
            {"role": "system", "content": "你是企业知识库助手，只根据检索到的文档回答，并注明出处。"},
            {"role": "user", "content": f"{question}\n\n检索结果：{json.dumps(hits, ensure_ascii=False)}"},
        ])
        if r.status_code != 200:
            raise RuntimeError(f"dashscope {r.code}: {r.message}")
        return r.output.choices[0].message.content

    questions = ["出差住宿标准是多少", "生产环境变更要走什么流程", "VPN 怎么开通", "新人入职第一周要做什么", "公司电脑丢了怎么处理"]

    def run_once(fault_rate: float) -> None:
        s = sessions.pick()
        fault = pick_fault(fault_rate, faults)
        with otel.session(conversation_id=s["id"], user_id=s["user"], tags={"dept": random.choice(["研发", "销售", "行政"])}):
            ask(random.choice(questions), fault)

    return run_once


PROFILES = {
    "kefu": ("cs-agent", build_kefu),
    "order": ("order-ops-agent", build_order),
    "rag": ("kb-qa-agent", build_rag),
}


def export_summary(otel) -> str:
    return json.dumps({"flashcat": otel.diagnostics().get("export")}, default=str)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--profile", choices=sorted(PROFILES), required=True)
    ap.add_argument("--app", help="override the default application name (service.name)")
    ap.add_argument("--once", action="store_true", help="run once and exit (self-check)")
    args = ap.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")

    srv = QuietServer(("127.0.0.1", 0), FakeModels)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    base_url = f"http://127.0.0.1:{srv.server_address[1]}"

    default_app, build = PROFILES[args.profile]
    os.environ.setdefault("OTEL_RESOURCE_ATTRIBUTES", f"deployment.environment.name={os.environ.get('MOCK_ENV', 'prod')}")
    from flashcat import otel

    diag = otel.init(service_name=args.app or default_app, capture_content="span")
    log.info("flashcat-otel 初始化：%s", json.dumps({k: diag.get(k) for k in ("provider", "instrumentations")}, ensure_ascii=False, default=str))

    run_once = build(otel, base_url)
    interval = float(os.environ.get("MOCK_INTERVAL", "4"))
    fault_rate = float(os.environ.get("MOCK_FAULT_RATE", "0.03"))

    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())

    runs = errors = 0
    last_report = time.time()
    while not stop.is_set():
        runs += 1
        try:
            run_once(fault_rate)
        except Exception as e:  # noqa: BLE001  Log simulated errors and keep running.
            errors += 1
            log.debug("run failed: %s: %s", type(e).__name__, e)
        if args.once:
            break
        if time.time() - last_report > 300:
            log.info("runs=%d errors=%d export=%s", runs, errors, export_summary(otel))
            last_report = time.time()
        stop.wait(random.expovariate(1 / interval))

    otel.flush(10)
    log.info("退出：runs=%d errors=%d export=%s", runs, errors, export_summary(otel))


if __name__ == "__main__":
    main()
