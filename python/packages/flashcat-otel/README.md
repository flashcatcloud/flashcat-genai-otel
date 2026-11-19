# flashcat-otel

The Flashcat OTel SDK for AI agent observability integrates with OpenTelemetry GenAI instrumentation in one line.

Install from this repository's GitHub Releases while PyPI publication is pending. For extras such as DashScope and MCP instrumentation, see [RELEASING.md](../../../RELEASING.md#install-from-github-releases).

```bash
pip install 'flashcat-otel[openai] @ https://github.com/flashcatcloud/flashcat-otel/releases/download/v0.1.0/flashcat_otel-0.1.0-py3-none-any.whl'
export OTEL_SERVICE_NAME=kefu-agent
export FLASHCAT_ENDPOINT=https://ingest.example.com/v1/traces   # full URL including /v1/traces
export FLASHCAT_API_KEY=keyID:secret                              # the SDK handles Basic encoding
```

```python
# Place this before importing model SDKs or frameworks
from flashcat import otel

diagnostics = otel.init(capture_content=False)   # Content capture is off by default. Use "span" to enable it

@otel.agent("kefu")
def answer(question): ...

@otel.tool("search_kb")
def search_kb(query): ...

with otel.session(conversation_id="c-123", user_id="u-9"):
    answer("Where is my order?")

otel.flush()  # Before exiting a short script
```

Use `llm` / `embedding` / `retrieval` to instrument model clients without instrumentation (custom gateways, private SDKs, or direct HTTP calls) and retrieval.
Use `annotate` to add token usage, the response model, and messages.

```python
@otel.llm("qwen-max", provider="dashscope")           # chat qwen-max
def ask(messages):
    resp = gateway.post("/chat", json={"messages": messages})
    otel.annotate(
        input=messages,                                  # [{"role": ..., "content": ...}] or a string
        output=resp["text"],
        model=resp["model"],
        usage={"input_tokens": resp["usage"]["in"], "output_tokens": resp["usage"]["out"]},
        finish_reasons=["stop"],
    )
    return resp

@otel.retrieval("kb-orders")                             # retrieval kb-orders. The return value is used as documents
def search(query): ...

@otel.embedding("text-embedding-v3", provider="dashscope")   # embeddings … without input content
def embed(texts): ...
```

- `annotate` applies only to the innermost `agent / tool / workflow / llm / embedding / retrieval` span in the current context.
  If none of these spans is active, the call has no effect. Input and output follow the content capture switch. At span end, arguments and return values only fill fields left unset by annotate.
  The usage keys are `input_tokens`, `output_tokens`, `cache_read_input_tokens`, and `cache_write_input_tokens`.
- If `llm` calls an instrumented model SDK such as openai, the inner call contributes its usage to the `llm` span without creating a separate span.
  Other instrumentation sources such as OpenInference create both spans, so the call is counted twice. Diagnostics add a `problems` entry and increment `nested_inference_spans`.

| Current setup | `exporter` | Behavior |
|---|---|---|
| No TracerProvider | `auto` → add | Create a provider and export to Flashcat |
| Another OTel provider is already present | `auto` → add | Reuse the provider and add the Flashcat export pipeline |
| Already connected to Flashcat APM | `reuse` | Send data through the existing exporter |

Parameters take precedence in this order. Explicit arguments > `FLASHCAT_*` > `OTEL_*` > defaults. `otel.diagnostics()` returns JSON with a stable structure,
including the load result for each instrumentation (installed / skipped_conflict / skipped_version / not_present) and export counters (401 / 413 / 5xx).
Root spans are explicit. Mark entry points with `@otel.agent` / `@otel.workflow` / `otel.trace()`.

At exit, traces and metrics share a 5-second deadline; after shutdown begins, a failed export is retried at most twice within the remaining time (contract §9). Set
`FLASHCAT_SHUTDOWN_TIMEOUT=2.5` to change the timeout in seconds, or pass it explicitly in `init(shutdown_timeout_s=2.5)`.
`otel.flush(timeout_s=2)` and `otel.shutdown(timeout_s=2)` also honor the total timeout passed to them. Shutdown closes only providers created by the SDK.
Spans dropped on timeout count toward `export.dropped_timeout`, and metric batches count toward `export.metrics_dropped_timeout`.

The instrumentation catalog (install with `pip install 'flashcat-otel[<name>]'`, or use `[all]` for all entries) contains `openai`, `anthropic`, `langchain`, `openai-agents`,
`llama-index`, `google-genai`, `bedrock` (upstream OTel), `dashscope`, `mcp` (Flashcat. mcp 2.x comes with instrumentation, so this instrumentation is skipped), and
`crewai` (OpenInference, which emits only Crew / Task / Agent / Tool spans. Model SDK instrumentation records model calls).

Zero-code mode (`OTEL_PYTHON_DISTRO=flashcat opentelemetry-instrument ...`) also records instrumentation load results.
If the target library is detected without its instrumentation package, the SDK adds a `problems` entry and logs a WARN line once with the install command.
`OPENAI_AGENTS_DISABLE_TRACING=1/true/yes` suppresses openai-agents framework spans while model spans continue to appear.
Diagnostics explain how to restore framework spans.
