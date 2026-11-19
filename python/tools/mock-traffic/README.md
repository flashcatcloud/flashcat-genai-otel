# Demo traffic (mock-traffic)

Continuously sends simulated calls to Flashcat AI agent observability. Most calls succeed, and about 3% include errors. Calls go through the flashcat-otel SDK and its instrumentation.
The model service is an in-process mock, so it makes no real model calls and incurs no charges.

| Application (service.name) | Simulated behavior | Occasional errors |
|---|---|---|
| `cs-agent` | E-commerce customer support with order lookup and FAQ tools, glm-5.3-flash / kimi-k2.6 (OpenAI-compatible SDK), and multi-turn sessions. Content includes mobile phone numbers and email addresses to show masking | 429 rate limits, 503, timeouts, order service timeouts, missing usage |
| `order-ops-agent` | Multi-agent flow (order-planner → logistics-specialist) using deepseek-flash / deepseek-v4-pro, with some streaming calls | 429, 503, timeouts, logistics gateway disconnects, missing usage |
| `kb-qa-agent` | Knowledge base Q&A (DashScope embedding → vector retrieval → qwen-plus / qwen-max) | Rate limits, 500, vector database not loaded, missing usage |

About 3% of requests are also slow (an extra 1.5–2.8 seconds of latency, with no timeout). Multi-turn conversations span 1–5 turns per session.

## Deployment

```bash
python3 -m venv venv
./venv/bin/pip install flashcat_instrumentation_dashscope-*.whl 'flashcat_otel-*.whl[openai,dashscope]' openai
cat > .env <<'ENV'
FLASHCAT_ENDPOINT=http://127.0.0.1:4320/v1/traces
FLASHCAT_API_KEY=<keyID:secret>
MOCK_INTERVAL=4        # average seconds between runs for each application
MOCK_FAULT_RATE=0.03   # fraction of runs with errors
MOCK_ENV=prod
ENV
chmod 600 .env
./ctl.sh start      # stop | restart | status | logs
```

The generator does not start at boot. To run a self-check, use `./venv/bin/python mock_traffic.py --profile kefu --once`.
