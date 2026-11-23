# Flashcat GenAI OTel SDK cross-language contract

The Python, TypeScript, Go, and Java implementations must all follow this contract. If an implementation differs from this document, this document takes precedence.

Conformance test cases are in `fixtures/`. The Python implementation generates `masking.json`, the reference for masking. Go's regex engine lacks lookaround support, so Go needs equivalent patterns that produce the same test results.



## 1. Configuration

Precedence follows this order. **Explicit arguments > `FLASHCAT_*` > standard `OTEL_*` > defaults**.

| Setting | `FLASHCAT_*` | Compatible `OTEL_*` | Default | Validation |
|---|---|---|---|---|
| Application name | `FLASHCAT_SERVICE_NAME` | `OTEL_SERVICE_NAME` or `service.name` in `OTEL_RESOURCE_ATTRIBUTES` | None | An empty name or one starting with `unknown_service` disables export and adds an entry to `problems` |
| Environment | `FLASHCAT_ENVIRONMENT` | `deployment.environment.name` / `deployment.environment` | `prod` | — |
| Ingest endpoint | `FLASHCAT_ENDPOINT` | `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` + `/v1/traces` | None | Must be a full `http(s)` URL with a path. The SDK does not add a port automatically |
| Metrics endpoint | `FLASHCAT_METRICS_ENDPOINT` | `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Replace `/v1/traces` with `/v1/metrics` | — |
| Credentials | `FLASHCAT_API_KEY` (`keyID:secret`) | `Authorization` in `OTEL_EXPORTER_OTLP_(TRACES_)HEADERS` | None | A missing `:` is reported as a problem. The SDK handles Basic encoding |
| Content | `FLASHCAT_CAPTURE_CONTENT` | `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` | Capture off | TypeScript, Go, and Java support only `false` / `span` (and the synonyms `off`, `no_content`, and `span_only`). Values such as `event` are reported as a problem and disable capture. Invalid environment variable values always log a problem and disable capture. The environment value `true` is treated as `span`. Only invalid explicit arguments raise an exception |
| Disable switch | `FLASHCAT_DISABLED` | `OTEL_SDK_DISABLED` | Off | When set, init only returns diagnostics |
| Flashcat host list | `FLASHCAT_KNOWN_HOSTS` | — | Ingest endpoint host | Determines whether the process is already sending data to Flashcat |
| Shutdown timeout | `FLASHCAT_SHUTDOWN_TIMEOUT` (seconds, fractional values accepted) | — | `5` | A nonnumeric value or a value ≤ 0 is reported as a problem and falls back to the default. See §9 |

## 2. Initialization

1. **Application code receives no exceptions.** If a step fails, the SDK records diagnostics and falls back. The only exception is an incorrect type for an explicit argument (such as passing `true` for the content capture switch), which should surface during development.
2. **Idempotent.** The second call returns the diagnostics from the first without taking any other action.
3. **Order.** Parse configuration → set the content capture switch (before loading instrumentation) → configure the provider → configure metrics → load instrumentation → register an exit flush → log a one-line diagnostics summary.
4. **Three provider scenarios** apply. The `exporter` modes are `auto / add / reuse / none`.

| Scenario | `auto` behavior |
|---|---|
| A. The process has no provider yet | Create a provider and add the Flashcat export pipeline |
| B. A provider exists and sends no data to Flashcat | Append the processor in languages that support it (Python and Go). In TypeScript and Java, fall back to `none` and include instructions in `problems` for adding the Flashcat processor to your SDK configuration |
| C. A provider exists and already sends data to Flashcat | `reuse` installs only instrumentation and session processing, with no additional exporter |

5. **Resource attributes** for scenario A are `service.name`, `deployment.environment.name`, `telemetry.distro.name=flashcat-genai-otel`, and `telemetry.distro.version=<SDK version>`. The OTel SDK's own `telemetry.sdk.language` identifies the language.

## 3. Export

Export uses OTLP/HTTP protobuf, gzip, `Authorization: Basic base64(keyID:secret)`, a 10-second timeout, and OTel's default batching settings. Metrics are exported every 60 seconds. Once the process starts shutting down (§9), failed exports can be retried up to twice (with backoffs of 0.2 and 0.4 seconds). Retries are limited to the same retryable errors as during normal operation (network errors, 429, 502, 503, and 504). Retries stop if the remaining time cannot fit both the backoff and one request, or if `Retry-After` exceeds the remaining time. The timeout for each request is the shorter of the export timeout and the time remaining under the shutdown deadline. A counting layer wraps the exporter and tracks `ok` (span count), `http_400 / 401 / 403 / 413 / 429`, `http_5xx`, `http_<code>` for other status codes, `network`, `dropped_queue_full`, `dropped_timeout` (the number of spans dropped when the shutdown or explicit flush deadline expires, see §9), and `last_error`. The counting layer performs no retries.

## 4. Semantic spans

Spans created by the SDK use the scope name `flashcat.genai` and the SDK version as the scope version. Attributes follow the OTel GenAI semantic conventions.

| API | Span name | Kind | Attributes |
|---|---|---|---|
| agent | `invoke_agent {name}` | INTERNAL | `gen_ai.operation.name=invoke_agent`, `gen_ai.agent.name`, optional `gen_ai.agent.description` |
| tool | `execute_tool {name}` | INTERNAL | `gen_ai.operation.name=execute_tool`, `gen_ai.tool.name`, `gen_ai.tool.type=function`, optional `gen_ai.tool.description` |
| workflow | `invoke_workflow {name}` | INTERNAL | `gen_ai.operation.name=invoke_workflow`, `gen_ai.workflow.name` |
| llm | `chat {model}` | CLIENT | `gen_ai.operation.name=chat`, `gen_ai.request.model`, optional `gen_ai.provider.name` |
| embedding | `embeddings {model}` | CLIENT | `gen_ai.operation.name=embeddings`, `gen_ai.request.model`, optional `gen_ai.provider.name` |
| retrieval | `retrieval {name}` | CLIENT | `gen_ai.operation.name=retrieval`, `gen_ai.data_source.id={name}` |
| trace | `{name}` | INTERNAL | Caller-supplied attributes outside the GenAI semantic conventions |

- These attributes are set **when the span starts**. The initial backend tail-sampling filter and session processing both rely on this.
- **Content is written only when capture is enabled.** Tool writes `gen_ai.tool.call.arguments` / `gen_ai.tool.call.result`. Agent, workflow, and llm write `gen_ai.input.messages` / `gen_ai.output.messages` in the format `[{"role":"user","parts":[{"type":"text","content":"…"}]}]` (the output role is `assistant`). Retrieval writes `gen_ai.retrieval.query.text` / `gen_ai.retrieval.documents` (a JSON array). Embedding omits content, in line with upstream instrumentation, because the GenAI conventions define no embedding input content attribute. Where annotate has supplied no value, the SDK serializes the arguments together as input and uses the return value as output (a list returned by retrieval becomes the documents array). Serialization is limited to 32 KiB. Longer values are truncated and suffixed with `…[truncated]`. The SDK skips traversing object internals and falls back to the language's built-in string representation when serialization fails.
- **On error,** set the span status to ERROR, write `error.type` (the exception class name), record an exception event, and **rethrow the original exception unchanged**.
- **`error.type` for model calls.** Flashcat's own model instrumentation (such as the Go middleware and Java Responses instrumentation) writes the HTTP status code as a string (such as `429`) if the error carries one. Otherwise, it writes the exception class name. The SDK leaves values from upstream instrumentation intact. The backend exporter normalizes errors across languages (HTTP status errors normalize to the status code, see the exporter's `genai` mapping).
- **SDK failures leave application code unaffected.** If creating or ending the span fails, run the original function directly.
- **Support synchronous, asynchronous, and streaming calls (generators / iterators).** Streaming spans must end on exhaustion, early close, or an exception.
- **Retrieval documents array.** For a list or array, keep dictionary elements (maps or object literals) as-is and write other elements as `{"content": "<serialized value>"}`. Treat a non-list value as a single document. If the serialized array exceeds 32 KiB, discard documents from the end until it fits, then append `{"content":"…[truncated]"}` so the result remains valid JSON.

### 4.1 Manual model calls vs. instrumented calls

Use llm / embedding for **uninstrumented** model clients (custom gateways, private SDKs, or direct HTTP calls). The following rules apply when they contain an instrumented inference span (where `gen_ai.operation.name` is `chat / text_completion / generate_content / embeddings`).

- In Python, the semantic API is built on util-genai. Inner instrumentation built on util-genai contributes usage, the response model, and content to the outer span without creating its own span (existing util-genai behavior). Other instrumentation sources such as OpenInference still create a span and follow the next rule.
- In all other cases (TypeScript, Go, and Java, plus Python instrumentation outside util-genai), both layers create a span and the backend counts the same call twice. When the inner span starts, if its parent is an llm / embedding span created by the semantic API in the same process, the session processor increments `semantics.nested_inference_spans`, adds a `problems` entry, and emits a WARN line (once per cause) with the message `手工 llm / embedding span 里又有被插桩的模型调用，同一次调用会重复计数：只用 llm / embedding 标没有被插桩的模型客户端，或去掉外层的手工 span` (An instrumented model call inside a manual llm / embedding span will be counted twice. Use llm / embedding only for uninstrumented model clients, or remove the outer manual span.)

### 4.2 annotate

`annotate(input, output, model, usage, finish_reasons, tags)` adds data to the **innermost semantic API span** in the current context (agent / tool / workflow / llm / embedding / retrieval, excluding trace). With no semantic API span in the context, the call is a no-op and reports no error. It leaves spans from upstream instrumentation alone.

| Field | agent / workflow | tool | llm | embedding | retrieval |
|---|---|---|---|---|---|
| input | `gen_ai.input.messages` | `gen_ai.tool.call.arguments` | `gen_ai.input.messages` | Ignored | `gen_ai.retrieval.query.text` |
| output | `gen_ai.output.messages` | `gen_ai.tool.call.result` | `gen_ai.output.messages` | Ignored | `gen_ai.retrieval.documents` |
| model | Ignored | Ignored | `gen_ai.response.model` | `gen_ai.response.model` | Ignored |
| usage | Ignored | Ignored | `gen_ai.usage.input_tokens`, `output_tokens`, `cache_read.input_tokens`, `cache_write.input_tokens` | `gen_ai.usage.input_tokens` | Ignored |
| finish_reasons | Ignored | Ignored | `gen_ai.response.finish_reasons` (array of strings) | Ignored | Ignored |
| tags | `flashcat.tag.<k>` (all types) | Same | Same | Same | Same |

- Input and output follow the content capture switch. When the span ends, arguments and return values never overwrite input or output written by annotate.
- A string produces one text message (the input role is `user` and the output role is `assistant`). A `[{role, content}]` list produces one message per item, serializing any non-string `content`. Other values are serialized and treated as strings. Tool input and output and retrieval input are serialized before being written. Retrieval output follows the documents array rules above.
- Usage accepts only non-negative integers. Other values are ignored. `cache_write` corresponds to Anthropic cache creation.
- Repeated calls overwrite only the supplied fields.

### 4.3 MCP tool calls

The SDK's MCP instrumentation covers only tool calls (`tools/call`) and follows the OTel MCP semantic conventions.

| Side | Span name | Kind | Attributes |
|---|---|---|---|
| Client | `tools/call {tool name}` | CLIENT | `mcp.method.name=tools/call`, `gen_ai.operation.name=execute_tool`, `gen_ai.tool.name` |
| Server | `tools/call {tool name}` | SERVER | Same as above |

- When the content capture switch is on, write `gen_ai.tool.call.arguments` / `gen_ai.tool.call.result`, preferring structured results and otherwise joining the text blocks. Apply masking as described in §6.
- If a tool returns `isError`, write `error.type=tool_error` and set the span status to ERROR. For JSON-RPC errors, write the error code. For other exceptions, write the exception class name.
- The client writes the W3C traceparent into the request's `_meta` using only the Trace Context propagator, without baggage. The server extracts it from `_meta` and uses it as the parent span, so both sides are in the same trace. When the server gets a parent span, it sets `flashcat.genai.root=false` (the parent is the client's tool call span, see §5.1).
- For MCP SDK versions with built-in instrumentation, skip installing instrumentation and report `source=native` in diagnostics. When a built-in client span sets only `mcp.method.name`, the session processor adds `gen_ai.operation.name=execute_tool` and extracts `gen_ai.tool.name` from the span name so the call appears in AI observability.

Coverage is Python client and server (Flashcat instrumentation for mcp 1.x, built-in instrumentation in 2.x), TypeScript client (`Client.callTool` from `@modelcontextprotocol/sdk`), and Go server (middleware for `github.com/modelcontextprotocol/go-sdk`).

## 5. Sessions

`session(conversation_id, user_id, tags, propagate=false)` stores values under private keys in the OTel Context. **At span start**, the session processor writes them to **every new span** within the session scope.

- The attributes are `gen_ai.conversation.id` (preserve an existing value on the span), `user.id`, and `flashcat.tag.<k>`.
- Child spans also inherit these three kinds of attributes when the parent span is in the same process and carries them. This works in environments such as Java Agent, where the application and processor use different class loaders and private Context keys cannot pass between them.
- **Baggage is off by default** because it travels downstream in HTTP headers, including to model providers. Set `propagate=true` to write baggage with the keys `gen_ai.conversation.id` and `user.id`.
- `orphan_inference_spans` counts spans with no parent and with `gen_ai.operation.name` set to `chat / text_completion / generate_content / embeddings` at span start.

### 5.1 AI root marker

The platform's trace list is based on AI roots, following Datadog's behavior of showing a trace only after its root span arrives. The SDK marks the AI root so the server does not need to guess.

- **GenAI span.** Uses the same rule the Flashcat backend uses to decide whether a span is a GenAI span. A span qualifies if it carries one of `gen_ai.operation.name / gen_ai.request.model / gen_ai.provider.name / gen_ai.system / openinference.span.kind / llm.model_name / traceloop.span.kind / langfuse.observation.type / flashcat.span.kind / gen_ai.tool.name / gen_ai.tool.call.id / ai.operationId / llm.request.type`, has a key with one of the prefixes `gen_ai. / llm. / openinference. / traceloop. / langfuse.observation.` (except `gen_ai.conversation.id`), or has a key with the `ai.` prefix when `ai.operationId / operation.name` is present.
- **AI root.** A GenAI span with no GenAI ancestor. An agent under an ordinary span such as an HTTP span can still be an AI root. A model call with a GenAI ancestor is excluded even if an ordinary span sits between them.
- **When each span starts**, the session processor checks whether the span has a GenAI ancestor in the same process. It does when the parent is in the same process and either qualifies as a GenAI span (based on the parent's current attributes) or already has a GenAI ancestor of its own. The processor tracks only spans with a GenAI ancestor and retains the most recent 65,536.
- For instrumentation libraries that write attributes only at span end, the instrumentation scope name identifies GenAI spans. In Python, the scope starts with `openinference.instrumentation.` (OpenInference LangChain sets no attributes at span start). In Node.js, it starts with `@arizeai/openinference-instrumentation-`, which includes `@arizeai/openinference-instrumentation-openai` and `@arizeai/openinference-instrumentation-langchain`. In Java, OpenInference LangChain4j uses `com.arize.langchain4j` and names with the `com.arize.langchain4j.` prefix. Spans from these scopes qualify as GenAI spans at start. The Go semantic API, Eino callbacks, openai-go middleware, and MCP server set `gen_ai.operation.name` when they create spans, so those spans are identified by their attributes.
- GenAI spans get `flashcat.genai.root` (a boolean, with `true` indicating an AI root). The session processor sets it for spans that already qualify as GenAI at start. For spans that gain GenAI attributes after starting, the Flashcat export pipeline sets it after span end based on the final attributes. When reusing an existing exporter that bypasses the Flashcat export pipeline, only the span-start check applies.
- A span with a remote parent (in another process) is currently treated as having no GenAI ancestor. If an agent in service A calls service B, the model call in B is marked as a root. Once the parent span is stored, the platform corrects the marker based on the trace structure. Propagating GenAI ancestry across processes is future work.
- On the platform, every GenAI span with an empty `parent_span_id` is a root. For spans with a parent but no AI root marker (reported by other SDKs), the platform uses the trace structure. A span is treated as a root only if its parent is absent from storage and three minutes have passed since the span ended.

## 6. Masking and truncation

Masking and truncation apply only in Flashcat's own export pipeline, after spans end and before batching.

| Layer | Rule |
|---|---|
| L1 Key name | Replace string values with `[REDACTED]` when the final segment of the attribute key (split on `.`, `_`, or `-`) matches `authorization / api_key / apikey / password / passwd / secret / client_secret / token / access_token / refresh_token / auth_token / cookie / set_cookie / private_key` (an `_` in a pattern can be replaced with `-` or omitted). Keys such as `gen_ai.usage.input_tokens` must remain unaffected |
| L2 Content | When content capture is enabled, apply the following replacements in order to the first 256 KiB of `gen_ai.input.messages / gen_ai.output.messages / gen_ai.system_instructions / gen_ai.tool.call.arguments / gen_ai.tool.call.result / gen_ai.retrieval.query.text / gen_ai.retrieval.documents`. Email addresses → `[REDACTED:email]`, national ID numbers → `[REDACTED:id_card]`, bank card numbers → `[REDACTED:bank_card]`, mobile phone numbers → `[REDACTED:phone]`. The regular expressions and their order match `DEFAULT_PATTERNS` in Python's `_processors.py` exactly |
| L3 Truncation | If a single string attribute exceeds 512 KiB in UTF-8 bytes, truncate it at a valid character boundary and set `flashcat.content_truncated=true` |

Additional custom rules can specify a name, regular expression, and replacement text. Diagnostics include the `masked_spans` and `truncated_spans` counters.

## 7. Content capture per source

| Source | Setting |
|---|---|
| Upstream OTel instrumentation | Use `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`. Pass the setting directly as a constructor argument when supported. Accepted values differ by package (Python accepts `SPAN_ONLY`, JavaScript `genai-util` accepts `span_only`, and Java uses a boolean). Each language adapts the setting for each package |
| Built into the Anthropic SDK | `ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE=content / metadata_only` (set only if the user has not set it) |
| Vercel AI SDK | Pass the input and output recording options when registering the `OpenTelemetry` integration |
| OpenInference | `OPENINFERENCE_HIDE_INPUTS / OPENINFERENCE_HIDE_OUTPUTS` (set only if the user has not set them) |
| Spring AI | Controlled by the application's own `spring.ai.*.observations.*` configuration. The SDK can only document and report this in diagnostics |

The SDK guarantees that the switch takes effect for sources it loads. For other sources it applies the switch on a best-effort basis. Diagnostics list the effective state for each source.

## 8. Diagnostics

The return value of `init()` has the same structure as `diagnostics()`, with the same field names across all four languages.

```json
{
  "sdk": {"name": "flashcat-genai-otel", "version": "0.2.0", "language": "nodejs"},
  "enabled": true,
  "service_name": "kefu-agent",
  "environment": "prod",
  "endpoint": "https://ingest.example.com/v1/traces",
  "problems": [],
  "notes": [],
  "provider": {"situation": "A", "exporter_mode": "add", "metrics": "add"},
  "content": {"upstream": "NO_CONTENT", "anthropic": "metadata_only"},
  "instrumentations": [
    {"lib": "openai", "source": "upstream", "status": "installed", "lib_version": "6.9.0"},
    {"lib": "anthropic", "source": "native", "status": "installed", "lib_version": "1.8.0"}
  ],
  "export": {"ok": 120, "network": 0, "dropped_queue_full": 0, "dropped_timeout": 0, "last_error": null},
  "semantics": {"orphan_inference_spans": 0, "nested_inference_spans": 0, "masked_spans": 0, "truncated_spans": 0}
}
```

- `sdk.language` is one of `python / nodejs / go / java`.
- `instrumentations[].source` is one of `upstream / native / openinference / flashcat`. When different sources instrument different APIs in the same library, report separate entries and distinguish them with the optional `api` field (in Java, the Java Agent instruments openai-java's `chat_completions` and `embeddings`, while the Flashcat extension instruments `responses`). The `status` values are `installed / not_present / disabled / skipped_conflict / skipped_version / instrumentation_not_installed / already_instrumented / failed`, plus `loaded_before_init` (the target library in Node.js loaded before initialization, so it could not be patched) and `not_patched` (runtime confirms the library is in use but the patch did not take effect, for example when Next.js, webpack, or esbuild bundled it in Node.js. The status changes from `installed` to `not_patched` at runtime).
- In the following cases, the application can run normally and diagnostics can look normal while model data is missing. Add a `problems` entry and log one WARN line per cause.
  - The target library is installed but its instrumentation package is missing (`instrumentation_not_installed`), such as a Node.js project using openai 7.x without the OpenInference package.
  - `not_patched`.
  - The detected model SDK or API is outside instrumentation coverage (for example, LangChain4j in Java or openai-java without the Java Agent, see each language's implementation notes).
- When the framework's own tracing switch is off, add an entry to `notes` explaining that framework spans will be missing (in Python, when `OPENAI_AGENTS_DISABLE_TRACING` is true and openai-agents instrumentation is loaded).
- Zero-code entry points (Python `opentelemetry-instrument`, Node.js `--import`, and Java Agent) must also list actual load results in `instrumentations`, which must contain at least one entry.
- Write `problems` and `notes` messages in Chinese, consistent with Python.
- Log a one-line INFO summary at init. When `debug` is enabled, log the full diagnostics.

## 9. Lifecycle

The SDK provides `flush(timeout)` and `shutdown(timeout)`. When the process exits, the SDK automatically flushes and shuts down within `shutdown_timeout` (5 seconds by default, configurable with `FLASHCAT_SHUTDOWN_TIMEOUT`). `shutdown` closes only providers created by the SDK.

`shutdown_timeout` is a **total deadline** shared by all steps.

1. From the start of shutdown, both traces and metrics must flush and shut down by the same deadline. When it expires, drop any remaining data (count spans in `export.dropped_timeout`) and stop waiting.
2. Once shutdown begins, retries for both traces and metrics are limited to two, with backoffs of 0.2 and 0.4 seconds and subject to the remaining time (§3). This lets the final batch reach the ingest endpoint after an isolated failure and lets the process exit on time if the endpoint is unreachable. The timeout for each request is the shorter of the export timeout and the remaining time.
3. If anything else in the process registers an exit hook for a provider the SDK created (such as the atexit hook registered by Python `opentelemetry-instrument` or the OTel Java Agent's own SDK shutdown hook), those hooks must also return by the same deadline. The SDK must take over and disable duplicate hooks, or ensure export is already closed when they start so they return immediately. If a hook cannot be controlled, document the actual upper bound and the reason in that language's implementation notes.
4. Explicit calls to `flush(timeout)` / `shutdown(timeout)` also honor the timeout supplied by the caller. A supplied timeout (the context deadline in Go) takes precedence. `FLASHCAT_SHUTDOWN_TIMEOUT` and the initialization argument apply only to automatic shutdown and calls without a timeout.

The benchmark program must exit no more than `shutdown_timeout + 1` seconds after the application finishes when the ingest endpoint refuses connections or blackholes requests (accepts connections without responding). All data must reach a healthy endpoint. All data must also arrive when the first request fails (with a 503 or a dropped connection) and the endpoint then recovers.


