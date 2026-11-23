# @flashcat/genai-otel

The Node.js version of the Flashcat GenAI OTel SDK provides observability for AI agents with a one-line setup on top of OpenTelemetry GenAI instrumentation. It handles initialization, export to Flashcat, business semantics (agents, tools, workflows, and sessions), masking and truncation, and diagnostics. Model call spans come from upstream instrumentation or instrumentation built into the model SDK.

Requires **Node.js ≥ 22.15.0** to meet the requirements of the current generation of OTel JS. Support is limited to server-side Node.js. Browser and Edge runtimes are unsupported.

## Installation

The package has yet to be published to npm. Install it from this repository's GitHub Releases (see [RELEASING.md](../RELEASING.md#install-from-github-releases)).

```bash
npm install https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/v0.1.0/flashcat-genai-otel-0.1.0.tgz
```

The package includes instrumentation for openai (4.19 through 6.x), the Anthropic SDK, and the OpenAI Agents SDK. For openai 7, LangChain, the Vercel AI SDK, and Bedrock, install the corresponding packages listed under [Instrumentation sources](#instrumentation-sources).

## Zero-code startup

<!-- snippet:zero-code -->
```bash
export OTEL_SERVICE_NAME=kefu-agent
export FLASHCAT_ENDPOINT=https://ingest.example.com/v1/traces   # full URL including /v1/traces
export FLASHCAT_API_KEY=keyID:secret                              # the SDK encodes this for Basic authentication
node --import @flashcat/genai-otel/register app.js
```

Use the same command for ESM and CommonJS applications. `--import` registers the ESM loader hook before calling `init()`, so it can also patch model SDKs loaded with `import` in ESM applications.

## Initialization in code

Use `init()` to pass options in code. Call it before loading the model SDK.

For CommonJS, put the initialization at the top of the entry file.

<!-- snippet:init-cjs -->
```js
const otel = require('@flashcat/genai-otel');
otel.init({ serviceName: 'kefu-agent', captureContent: false });   // Content capture is off by default. Use 'span' to enable it.

const { OpenAI } = require('openai');   // Load the model SDK after init.
```

In ESM, `import` declarations are hoisted to the top of the file. Put the initialization in a separate file, register the ESM loader hook there, and load that file first with `--import`.

<!-- snippet:init-esm -->
```js
// instrumentation.mjs
import { register } from 'node:module';
import * as otel from '@flashcat/genai-otel';

register('@opentelemetry/instrumentation/hook.mjs', import.meta.resolve('@flashcat/genai-otel'));
otel.init({ serviceName: 'kefu-agent', captureContent: false });
```

<!-- snippet:init-esm-run -->
```bash
node --import ./instrumentation.mjs app.mjs
```

openai 7 and LangChain use OpenInference instrumentation. In ESM applications, patching these libraries requires the zero-code entry point `--import @flashcat/genai-otel/register` (see [Known limitations](#known-limitations)).

The precedence order is explicit arguments > `FLASHCAT_*` > `OTEL_*` > defaults. `init()` is synchronous. A second call returns the diagnostics from the first call without performing initialization again. Only invalid explicit arguments (such as `captureContent: true`) cause `ConfigError`. Other issues are recorded in the diagnostics and the application continues running normally.

| Option | Environment variable | Default |
|---|---|---|
| `serviceName` | `FLASHCAT_SERVICE_NAME`, `OTEL_SERVICE_NAME` | None (required) |
| `environment` | `FLASHCAT_ENVIRONMENT` | `prod` |
| `endpoint` | `FLASHCAT_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | None |
| `apiKey` | `FLASHCAT_API_KEY` (`keyID:secret`) | None |
| `captureContent` | `FLASHCAT_CAPTURE_CONTENT`, `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` | `false` |
| `exporter` | — | `auto` |
| `metrics` | — | `true` |
| `instrumentations` / `disabledInstrumentations` | `OTEL_NODE_DISABLED_INSTRUMENTATIONS` | Load all |
| `shutdownTimeoutMs` | `FLASHCAT_SHUTDOWN_TIMEOUT` (seconds, including fractional values. Invalid values are recorded in `problems` and the default is used.) | `5000` |
| `masking`, `resourceAttributes`, `exportTimeoutMs` (10000), `debug`, `prefer` | — | — |

Set `FLASHCAT_DISABLED=true` or `OTEL_SDK_DISABLED=true` to disable the SDK. In this mode, `init()` only returns diagnostics.

## Business semantics

<!-- snippet:semantic -->
```js
import * as otel from '@flashcat/genai-otel';

const searchKb = otel.tool('search_kb', async (query) => {
  return ['Return policy'];   // Existing logic
});

const answer = otel.agent('kefu', async (question) => {
  const docs = await searchKb(question);
  return `References: ${docs.join(', ')}`;   // Existing logic. Model call spans are children of the agent span.
});

await otel.session({ conversationId: 'c-123', userId: 'u-9', tags: { tenant: 'a' } }, () => answer('Where is my order?'));
await otel.flush();   // Call before a short-lived script exits. Long-running services can skip this.
```

| API | Span produced |
|---|---|
| `otel.agent(name, fn, { description })` | `invoke_agent {name}` |
| `otel.tool(name, fn, { description })` | `execute_tool {name}` |
| `otel.workflow(name, fn)` | `invoke_workflow {name}` |
| `otel.llm(model, fn, { provider })` | `chat {model}` for uninstrumented model calls |
| `otel.embedding(model, fn, { provider })` | `embeddings {model}` without input content |
| `otel.retrieval(name, fn)` | `retrieval {name}`, with the return value as the retrieved documents |
| `otel.annotate({ input, output, model, usage, finishReasons, tags })` | No new span. Adds data to the current innermost semantic span. |
| `otel.trace(name, (span) => …, attributes)` | `{name}`, a regular span used as the root span for scripts and tasks |
| `otel.session({ conversationId, userId, tags, propagate }, fn)` | No new span. Spans started in the session scope carry `gen_ai.conversation.id`, `user.id`, and `flashcat.tag.*`. |

- `agent / tool / workflow / llm / embedding / retrieval` return wrapped functions that preserve their arguments, `this`, and return values. They support synchronous functions, promises, async iterators (including async generators), and generators. For streaming results, the span ends when the stream is exhausted, ends early, or throws an error.
- The SDK rethrows exceptions from the wrapped function as is, sets the span status to ERROR, and records `error.type`.
- Use `agent / workflow / trace` to mark entry points. The SDK never creates implicit root spans.
- By default, the SDK leaves session data out of baggage because baggage travels in HTTP headers to downstream services, including model providers. Pass `propagate: true` to correlate spans across services.
- The session processor sets the boolean `flashcat.genai.root` attribute on GenAI spans. It is `true` if the span has no GenAI ancestor in the same process. The Flashcat export pipeline adds the marker for spans that gain GenAI attributes between span start and span end.

### Uninstrumented model calls

Use `llm / embedding / retrieval` to mark model calls and retrievals made through custom gateways, private SDKs, or direct HTTP requests. Then use `annotate` to add usage, the response model, and messages.

```js
const ask = otel.llm('qwen-max', async (messages) => {
  const resp = await gateway.chat(messages);
  otel.annotate({
    input: messages,                      // [{ role, content }] or a string
    output: resp.text,
    model: resp.model,
    usage: { inputTokens: resp.usage.in, outputTokens: resp.usage.out },
    finishReasons: ['stop'],
  });
  return resp;
}, { provider: 'dashscope' });
```

- `annotate` applies only to the innermost `agent / tool / workflow / llm / embedding / retrieval` span in the current context and has no effect outside those spans. Input and output follow the content capture switch. Explicitly recorded output takes precedence over the return value when the span ends.
- The `usage` fields are `inputTokens`, `outputTokens`, `cacheReadInputTokens`, and `cacheWriteInputTokens` (Anthropic cache creation). Embedding only uses `inputTokens`.
- Avoid calling an instrumented model SDK inside `llm / embedding`. The SDK reports both span levels and counts the same call twice. When this happens, the diagnostics increment `nested_inference_spans` and record the issue in `problems`.

## Applications already using OpenTelemetry

OTel JS does not support adding processors to an existing TracerProvider. If the process already has a `NodeSDK`, add the Flashcat processor (session processing, masking, batching, and export) to your SDK configuration. Then use `init({ exporter: 'reuse' })` to install only the instrumentation.

<!-- snippet:existing-otel -->
```js
// instrumentation.mjs
import { register } from 'node:module';
import { NodeSDK } from '@opentelemetry/sdk-node';
import * as otel from '@flashcat/genai-otel';

register('@opentelemetry/instrumentation/hook.mjs', import.meta.resolve('@flashcat/genai-otel'));   // Required for ESM applications.

const sdk = new NodeSDK({
  serviceName: 'kefu-agent',
  spanProcessors: [otel.spanProcessor()],
});
sdk.start();
otel.init({ exporter: 'reuse' });
```

```bash
node --import ./instrumentation.mjs app.mjs
```

`otel.spanProcessor()` reads the endpoint and credentials the same way as `init()` (from `FLASHCAT_ENDPOINT` and `FLASHCAT_API_KEY`, or from arguments). NodeSDK continues exporting metrics and logs according to its own configuration. Its MeterProvider receives `gen_ai.client.*` metrics. For CommonJS applications, follow the same setup using `require`, omit `register`, and load the file with `node --require ./instrumentation.cjs app.js`.

Existing exporters receive the AI root marker that `otel.spanProcessor()` adds at span start. Markers added at span end are sent only through the Flashcat export pipeline.

| Existing setup in the process | Behavior with `exporter: 'auto'` |
|---|---|
| No TracerProvider | Creates a provider and exports to Flashcat |
| Existing provider that does not export to Flashcat | Falls back to `none`, adds the setup instructions above to `problems`, and loads instrumentation as usual. Uses `reuse` if `spanProcessor()` is already in use. |
| Existing provider that already exports to Flashcat | Uses `reuse` and installs only instrumentation |

## Instrumentation sources

The SDK has no automatic discovery. `init()` checks each target library in the table for installation and version range. Each library uses only one instrumentation source.

| lib | Target library and version | Source | Additional installation |
|---|---|---|---|
| `openai` | `openai` ≥4.19 and <7 | Upstream `@opentelemetry/instrumentation-openai` | None |
| `openai` | `openai` 7.x | OpenInference `@arizeai/openinference-instrumentation-openai` | `npm install @arizeai/openinference-instrumentation-openai` |
| `anthropic` | `@anthropic-ai/sdk` ≥0.134 | Built into the Anthropic SDK (no separate instrumentation) | None |
| `anthropic` | `@anthropic-ai/sdk` ≥0.65 and <0.134 | Upstream `@opentelemetry/instrumentation-anthropic` | None |
| `openai-agents` | `@openai/agents` ≥0.14 and <1 | Upstream `@opentelemetry/instrumentation-openai-agents` | None |
| `langchain` | `@langchain/core` 0.2 / 0.3 / 1.x | OpenInference `@arizeai/openinference-instrumentation-langchain` | `npm install @arizeai/openinference-instrumentation-langchain` |
| `ai` | `ai` (Vercel AI SDK) ≥7 | Built into the Vercel AI SDK via `registerTelemetry(new OpenTelemetry())` | `npm install @ai-sdk/otel` |
| `bedrock` | `@aws-sdk/client-bedrock-runtime` | Upstream `@opentelemetry/instrumentation-aws-sdk` (produces spans for all AWS SDK v3 clients, including Bedrock) | `npm install @opentelemetry/instrumentation-aws-sdk` |
| `google-genai` | `@google/genai` ≥1 (Gemini API / Vertex AI) | Built into the SDK. Each actual request produces one `generate_content {model}` / `embeddings {model}` span. | None |
| `mcp` | `@modelcontextprotocol/sdk` 1.x | Built into the SDK. `Client.callTool` produces a `tools/call {tool name}` CLIENT span and includes traceparent in `_meta`. | None |

The `status` for each library in the diagnostics can be `installed`, `not_present` (the target library is absent), `disabled`, `skipped_version`, `skipped_conflict` (another instrumentation has already wrapped the library), `instrumentation_not_installed` (`hint` gives the install command), `already_instrumented`, `loaded_before_init` (CommonJS loaded the target library with `require` before `init()`, so the patch may fail), `not_patched` (the SDK has observed model requests from the library at runtime without a working patch, usually because the library was bundled, see [Bundlers](#bundlers-nextjs--webpack--esbuild)), or `failed`.

The application can keep running without producing model data in three cases. The SDK records them in `problems` and writes one WARN line per cause to stderr. The cases are `instrumentation_not_installed` (with the package to install), an openai or Anthropic SDK version outside the instrumentation's supported range (`skipped_version`), and `not_patched`.

Usage for `@google/genai` follows the exporter's conventions. `gen_ai.usage.input_tokens` takes `promptTokenCount`, which includes cache hits. `output_tokens` is the sum of candidate and reasoning tokens. The SDK also records `cache_read.input_tokens` and `reasoning.output_tokens`. The provider is `gcp.gemini` or `gcp.vertex_ai`. Streaming spans record time to first token (TTFT). When automatic function calling makes multiple requests, each request gets its own span.

MCP client spans record `mcp.method.name`, `gen_ai.operation.name=execute_tool`, and `gen_ai.tool.name`, following the OTel MCP semantic conventions. They record arguments and results when the content capture switch is on. If the tool returns `isError`, the span records `error.type=tool_error`. The SDK adds W3C traceparent (without baggage) to the request's `_meta` so the server can join the same trace. It leaves the caller's argument object intact.

For openai 4.19 through 6.x, the upstream instrumentation omits cached token counts, time to first token (TTFT), and whether the request streams. The SDK adds `gen_ai.usage.cache_read.input_tokens`, `gen_ai.response.time_to_first_chunk` (seconds), and `gen_ai.request.stream` to the existing span without creating another span. openai 7 uses OpenInference, which records cached tokens in `llm.token_count.prompt_details.cache_read`. The SDK only adds TTFT for streaming requests.

### Bundlers (Next.js / webpack / esbuild)

Runtime require and import hooks can intercept only model SDKs loaded from `node_modules`. When a bundler includes a library such as `openai` in the server bundle, instrumentation silently fails. The diagnostics initially show `installed`. On the first model request, the status changes to `not_patched` and the SDK records the issue in `problems`. Exclude the SDK and model libraries from the bundle, then rebuild.

- For Next.js 13–14, add `experimental: { serverComponentsExternalPackages: ['@flashcat/genai-otel', 'openai'] }` to `next.config.js`.
- For Next.js 15 and later, set `serverExternalPackages: ['@flashcat/genai-otel', 'openai']` at the top level.
- For webpack, exclude these packages using `externals`. For esbuild, use `--external:openai` (or `packages: 'external'`).

## Content capture and masking

- Content capture is off by default. The SDK applies the content capture switch, enabled with `captureContent: 'span'`, to each source. It sets environment variables to `true` / `false` for upstream instrumentation, uses `ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE` for the Anthropic SDK and `OPENINFERENCE_HIDE_INPUTS / OUTPUTS` for OpenInference, and forces input and output recording off for each Vercel AI SDK call when content capture is off. Environment variables you set take precedence.
- Upstream `instrumentation-openai` (openai <7) records model call content as log events. The SDK copies those events to span attributes on the same call, applies masking as usual, and lets the original logging pipeline receive the events. In the diagnostics, `content.openai` is `log_events_bridged`.
- Upstream `instrumentation-anthropic` (Anthropic SDK <0.134) omits both token usage and content. The SDK adds token usage (including cache hits), the response model, and finish reasons. Content capture requires an upgrade to 0.134 or later. The diagnostics describe this in `notes`.
- Masking and truncation apply only to Flashcat's own export pipeline and run after the span ends, before batching.
  - Masking by key name replaces values for keys such as `authorization`, `api_key`, `password`, `token`, and `cookie` with `[REDACTED]` and preserves counters such as `gen_ai.usage.input_tokens`.
  - Content masking runs only when content capture is on and replaces email addresses, identity card numbers, bank card numbers, and mobile phone numbers.
  - Truncation shortens individual attributes larger than 512 KiB and sets `flashcat.content_truncated=true`.
- Add custom rules with `init({ masking: { extraPatterns: [{ name: 'order', pattern: /ORD-\d{6}/, replacement: '[REDACTED:order]' }] } })`.

## Diagnostics

`init()` and `otel.diagnostics()` return JSON with the same structure. The field names match the Python, Go, and Java SDKs.

```json
{
  "sdk": { "name": "flashcat-genai-otel", "version": "0.1.0", "language": "nodejs" },
  "enabled": true,
  "service_name": "kefu-agent",
  "environment": "prod",
  "endpoint": "https://ingest.example.com/v1/traces",
  "problems": [],
  "notes": [],
  "provider": { "situation": "A", "exporter_mode": "add", "metrics": "add" },
  "content": { "upstream": "NO_CONTENT", "anthropic": "metadata_only", "openinference": "best_effort:hide=true", "vercel": "recordInputs=false,recordOutputs=false" },
  "instrumentations": [{ "lib": "openai", "source": "upstream", "status": "installed", "lib_version": "6.49.0" }],
  "export": { "ok": 120, "network": 0, "dropped_queue_full": 0, "dropped_timeout": 0, "last_error": null },
  "semantics": { "orphan_inference_spans": 0, "nested_inference_spans": 0, "masked_spans": 0, "truncated_spans": 0 }
}
```

`export` also includes `http_400 / http_401 / http_403 / http_413 / http_429 / http_5xx / http_<code>` counters for status codes that have occurred. `dropped_timeout` counts spans dropped when the deadline for exit or an explicit flush expires. `network` counts ordinary failures such as connection refusals. `init()` writes a one-line summary to stderr, or the full diagnostics when `debug: true` is set.

## Lifecycle

- `await otel.flush(timeoutMs = shutdownTimeoutMs)` exports completed spans and metrics and returns whether it succeeded. The SDK aborts unfinished requests at the deadline, including any that would otherwise continue in the background.
- `await otel.shutdown(timeoutMs = shutdownTimeoutMs)` flushes first, then shuts down only providers created by the SDK. Subsequent calls return the result of the first call.
- `timeoutMs` is the total time limit. Traces and metrics share a deadline, each export request is limited to the time remaining, and spans dropped at the deadline count toward `export.dropped_timeout`. An explicit `timeoutMs` takes precedence over `shutdownTimeoutMs` / `FLASHCAT_SHUTDOWN_TIMEOUT`.
- On process exit, the SDK automatically flushes and shuts down, waiting up to `shutdownTimeoutMs`. The triggers are `beforeExit` and `SIGTERM`. On `SIGTERM`, if you have no listener for the signal, the process exits with the default behavior after shutdown. If you have a listener, the SDK only flushes, and your handler decides when to exit. The time limit still applies if the endpoint refuses the connection or fails to respond.

## Known limitations

- Start ESM applications with `--import` using the zero-code entry point or your own `instrumentation.mjs`. By the time application code calls `init()`, a model SDK loaded by a static `import` is already loaded and cannot be patched.
- OpenInference instrumentation depends on an older version of `@opentelemetry/instrumentation` (0.46) that this package's ESM loader hook cannot handle. The zero-code entry point first uses `import` to load the target library's ESM build, then calls OpenInference's `manuallyInstrument()` to apply the patch. This step is absent when you write your own `instrumentation.mjs`.
- With a custom TracerProvider (situation B), the SDK adds session attributes only to spans from the semantic API and spans that go through `spanProcessor()`.
- The zero-code entry point initializes only on the main thread. Call `init()` yourself in `worker_threads` when needed.
- The exporter skips retries for 429 / 5xx responses and counts failures by status code. Metrics exports also skip retries. Metrics are cumulative by default, so the next export still includes the totals.
- `process.exit()` exits immediately without giving asynchronous export a chance to finish. Call `await otel.shutdown()` first when needed.
