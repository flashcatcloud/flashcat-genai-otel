# flashcat-genai-otel (Java)

The Java version of the Flashcat GenAI OTel SDK provides AI agent observability on top of the OpenTelemetry Java Agent or OTel SDK.
Model call spans come from instrumentation built into the OTel Java Agent (openai-java Chat Completions and Embeddings, and Bedrock), additional openai-java instrumentation in the Flashcat extension
(the Responses API and Chat Completions on clients the Agent does not wrap, such as Spring AI), or instrumentation built into the model SDK (Anthropic Java SDK).
The SDK also handles export to Flashcat, business semantics (agents, tools, workflows, and sessions), masking and truncation, and diagnostics.

- Targets Java 8 bytecode, depends on OpenTelemetry Java 1.68.0, and has been verified with OpenTelemetry Java Agent 2.33.1.
- Version 0.1.0, Apache-2.0.

## Integration options

| Option | When to use it | Setup |
|---|---|---|
| **Java Agent extension** (recommended) | Any Java application, with no code changes | `-javaagent:opentelemetry-javaagent.jar -Dotel.javaagent.extensions=flashcat-genai-otel-agent-extension.jar` |
| Initialization in code | You cannot add JVM arguments or are running without the Java Agent | Add `flashcat-genai-otel` and call `FlashcatGenAI.init()` at the beginning of `main` |
| Spring Boot | Applications using the OTel Spring Boot Starter | Add the `flashcat-genai-otel` dependency. The SPI takes effect automatically |

All three options use the same configuration.

```bash
export OTEL_SERVICE_NAME=kefu-agent                                # or FLASHCAT_SERVICE_NAME
export FLASHCAT_ENDPOINT=https://ingest.example.com/v1/traces      # full URL including /v1/traces
export FLASHCAT_API_KEY=keyID:secret                               # the SDK handles Basic auth encoding
```

### 1. Java Agent extension (recommended)

```bash
curl -L -o opentelemetry-javaagent.jar \
  https://github.com/open-telemetry/opentelemetry-java-instrumentation/releases/download/v2.33.1/opentelemetry-javaagent.jar
# Get the extension JAR from this repository's GitHub Releases (see RELEASING.md)
curl -fL -O https://github.com/flashcatcloud/flashcat-genai-otel/releases/download/v0.1.0/flashcat-genai-otel-agent-extension-0.1.0.jar

java -javaagent:/path/to/opentelemetry-javaagent.jar \
     -Dotel.javaagent.extensions=/path/to/flashcat-genai-otel-agent-extension-0.1.0.jar \
     -jar app.jar
```

The extension JAR contains only `cloud.flashcat.genai` classes (the build checks for the absence of `io/opentelemetry/**`). The Agent provides the OTel API and SDK.
The extension translates `FLASHCAT_*` into per-signal OTel settings (trace and metric endpoints, `http/protobuf`, gzip, a Basic auth header, and a 10-second timeout).
It adds the session processor, wraps only OTLP exporters that target Flashcat to add masking, counters, and a shutdown deadline (see “Lifecycle”), and adds openai-java instrumentation (see below). `FLASHCAT_*` takes precedence over `OTEL_*`.
An explicit `otel.metrics.exporter=none` setting (via `-D` or `OTEL_METRICS_EXPORTER`) is preserved and disables metric export.

**openai-java instrumentation in the extension** (InstrumentationModule `flashcat-openai-java`) targets openai-java implementation classes, so it covers clients created with `OpenAIOkHttpClient.builder()`, `fromEnv()`,
a direct `new OpenAIClientImpl(...)`, or `withOptions(...)`. Disable it with `-Dotel.instrumentation.flashcat-openai-java.enabled=false`.

| API | Built-in instrumentation in Agent 2.33.1 | Instrumentation added by the extension |
|---|---|---|
| Responses (`responses().create / createStreaming`, synchronous and asynchronous clients) | None | `chat {model}` span with usage, cache hits, finish reasons, time to first token (TTFT) for streaming, `server.address/port`, and `openai.api.type=responses` |
| Chat Completions on a client wrapped by the Agent (`OpenAIOkHttpClient.builder().build()`) | Spans, with no TTFT for streaming | Adds `gen_ai.response.time_to_first_chunk` to the existing streaming span only |
| Chat Completions on a client the Agent does not wrap (Spring AI, direct construction with new, `withOptions`, `withRawResponse()`) | None | `chat {model}` span (`openai.api.type=chat_completions`), with the same fields as Responses |
| Embeddings | Yes, on wrapped clients only | None |

To avoid duplicating Agent spans, the extension creates a span only if there is no current inference span (a CLIENT span with a `gen_ai.operation.name` value such as chat).
A streaming span ends when the stream is fully read, closes early, or throws an exception. Closing before the end marker sets `error.type=stream_aborted`, as in Go. On error, `error.type`
follows the contract and contains the HTTP status code (such as `429`) when available, or the exception class name otherwise. Responses maps `status` to `gen_ai.response.finish_reasons` using the same rules as Go (completed → `stop`).
Flashcat export stays disabled if the endpoint or credentials are missing or the application name is invalid. In that case, if you have not configured your own OTLP export, the extension also disables the Agent's default
`localhost:4318` export, records the problem in diagnostics, and logs one line.

**To use only the semantic API without initializing it in code**, add the `flashcat-genai-otel` dependency to your application (see the coordinates below). Spans from `Flashcat.agent(...)` enter the same export pipeline through the Agent's
OTel API bridge. It is also safe to call `FlashcatGenAI.init()` in the application. When it detects the Agent and extension, it returns diagnostics only (situation C) and creates no provider.

### 2. Initialization in code

```java
import cloud.flashcat.genai.Diagnostics;
import cloud.flashcat.genai.FlashcatGenAI;

public static void main(String[] args) {
    // Put this at the beginning of main, before creating model clients
    Diagnostics d = FlashcatGenAI.init();   // Reads configuration from FLASHCAT_* / OTEL_* environment variables
    // Or pass explicit options (explicit options > FLASHCAT_* > OTEL_* > defaults)
    // FlashcatGenAI.init(FlashcatGenAI.Options.builder()
    //         .serviceName("kefu-agent")
    //         .endpoint("https://ingest.example.com/v1/traces")
    //         .apiKey("keyID:secret")
    //         .captureContent("false")      // Content capture defaults to off. Use "span" to enable it
    //         .build());
    ...
}
```

`init()` uses `AutoConfiguredOpenTelemetrySdk` and the same SPI to build and register the SDK globally. Other `OTEL_*` settings, such as sampling, still apply.
When the process already has a `GlobalOpenTelemetry` instance (situation B), Java cannot add processors after initialization. The SDK leaves the existing instance in place and describes how to use the Agent extension or Starter in `problems`.
The Agent's auto-instrumentation is unavailable in this setup. openai-java and Bedrock spans require the Java Agent. Instrumentation built into the Anthropic Java SDK works as usual.

### 3. Spring Boot (OTel Spring Boot Starter)

```xml
<dependencyManagement>
  <dependencies>
    <dependency>
      <groupId>io.opentelemetry.instrumentation</groupId>
      <artifactId>opentelemetry-instrumentation-bom</artifactId>
      <version>2.33.1</version>
      <type>pom</type>
      <scope>import</scope>
    </dependency>
  </dependencies>
</dependencyManagement>
<dependencies>
  <dependency>
    <groupId>io.opentelemetry.instrumentation</groupId>
    <artifactId>opentelemetry-spring-boot-starter</artifactId>
  </dependency>
  <dependency>
    <groupId>cloud.flashcat</groupId>
    <artifactId>flashcat-genai-otel</artifactId>
    <version>0.1.0</version>
  </dependency>
</dependencies>
```

The Starter uses ServiceLoader to load the SPI in `flashcat-genai-otel`. Use the same `FLASHCAT_*` environment variables for configuration. You can use `spring.application.name` as the application name.
The OTel Spring Boot Starter leaves `GlobalOpenTelemetry` unset. After the Starter builds the SDK, `flashcat-genai-otel` registers it globally if no global instance has been set.
This lets semantic APIs such as `Flashcat.agent(...)` and the Anthropic SDK's built-in instrumentation export spans. See `notes` in the diagnostics.
Explicit `otel.*` properties in `application.yml` take precedence over `FLASHCAT_*` because the Starter applies Spring configuration on top of the SPI result.

The `integration-tests` verified Spring Boot 4.1.2 with opentelemetry-spring-boot-starter 2.33.1, both with minimal dependencies and with actuator and micrometer-tracing-bridge-otel added to the classpath.
The SPI, global registration, Basic auth header, gzip, semantic API, and session attributes worked in both setups. Spring AI itself and the Micrometer Observation to OTel span path remain unverified in these tests.
(Spring Boot 4.1.2 did not create an `io.micrometer.tracing.Tracer` bean when actuator and micrometer-tracing-bridge-otel were added.)

## Dependency coordinates

Maven

```xml
<dependency>
  <groupId>cloud.flashcat</groupId>
  <artifactId>flashcat-genai-otel</artifactId>
  <version>0.1.0</version>
</dependency>
```

Gradle

```kotlin
implementation("cloud.flashcat:flashcat-genai-otel:0.1.0")
```

The Agent extension is a standalone JAR, `cloud.flashcat:flashcat-genai-otel-agent-extension:0.1.0`. Save it to disk and point `-Dotel.javaagent.extensions` to it without adding it as a dependency.
`flashcat-genai-otel` depends only on OpenTelemetry (api, sdk, sdk-extension-autoconfigure, exporter-otlp). The SDK serializes diagnostics to JSON itself and requires no JSON library.

## Configuration

| Setting | `FLASHCAT_*` | Compatible `OTEL_*` settings (also accepts `-Dotel.*`) | Default |
|---|---|---|---|
| Application name | `FLASHCAT_SERVICE_NAME` | `OTEL_SERVICE_NAME` or `service.name` in `OTEL_RESOURCE_ATTRIBUTES` | None. An empty name or a name matching `unknown_service*` disables export |
| Environment | `FLASHCAT_ENVIRONMENT` | `deployment.environment.name` / `deployment.environment` in `OTEL_RESOURCE_ATTRIBUTES` | `prod` |
| Ingest endpoint | `FLASHCAT_ENDPOINT` | `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` + `/v1/traces` | None. Requires a full HTTP(S) URL with a path. The SDK adds no port |
| Metrics endpoint | `FLASHCAT_METRICS_ENDPOINT` | `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Replaces `/v1/traces` with `/v1/metrics` |
| Credentials | `FLASHCAT_API_KEY` (`keyID:secret`) | `Authorization` in `OTEL_EXPORTER_OTLP_(TRACES_)HEADERS` | None |
| Content | `FLASHCAT_CAPTURE_CONTENT` (`false` / `span`) | `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` (`true` is treated as `span`) | Capture off |
| Disable switch | `FLASHCAT_DISABLED` | `OTEL_SDK_DISABLED` | Off |
| Flashcat host list | `FLASHCAT_KNOWN_HOSTS` | — | Host of the ingest endpoint |
| Shutdown timeout | `FLASHCAT_SHUTDOWN_TIMEOUT` (seconds, accepts fractions) | — | `5`. Invalid values record a problem and fall back to the default |

Export uses OTLP/HTTP protobuf, gzip, `Authorization: Basic base64(keyID:secret)`, a 10-second timeout, and a 60-second metrics interval.
Java treats unsupported `event`-style content capture values as capture off and records a problem. An invalid explicit option such as `captureContent("true")` causes `init` to throw `ConfigException`. Other problems appear only in diagnostics.

## Semantic API

```java
import cloud.flashcat.genai.Flashcat;

String out = Flashcat.agent("kefu", () -> answer(question));             // invoke_agent kefu
String hits = Flashcat.tool("search_kb", () -> search(query));           // execute_tool search_kb
Flashcat.workflow("ticket_flow", () -> { handle(ticket); });             // invoke_workflow ticket_flow
Flashcat.trace("batch_job", () -> { runJob(); });                        // Regular root span

// Description and input (input is recorded only when content capture is enabled)
Flashcat.tool("search_kb", Flashcat.SpanOptions.create().description("Knowledge base search").input(query), () -> search(query));
```

- The callback runs immediately. Overloads are available with and without a return value. A callback can throw a checked exception. The SDK rethrows it **unchanged** (the same object), sets the span status to ERROR, sets `error.type`, and records an exception event.
- The SDK sets semantic attributes when the span starts. The scope name is `flashcat.genai` and the version is `0.1.0`.
- When the content capture switch is on, agent and workflow spans get `gen_ai.input.messages` / `gen_ai.output.messages`
  (`[{"role":"user","parts":[{"type":"text","content":"…"}]}]`, with the role `assistant` for output). Tool spans get `gen_ai.tool.call.arguments` / `gen_ai.tool.call.result`.
  Strings are kept as is. Maps, lists, arrays, and primitive types are converted to JSON. Other objects use `toString()`. Values above 32 KiB are truncated and marked with `…[truncated]`.
- If the SDK itself fails, it runs the original callback directly. With no OTel configuration, instrumentation is a no-op and the callback runs as usual.

For asynchronous and streaming calls, start and end the span explicitly. The handle does not make the span current automatically.

```java
import cloud.flashcat.genai.FlashcatSpan;
import io.opentelemetry.context.Scope;

FlashcatSpan span = Flashcat.startAgent("kefu", Flashcat.SpanOptions.create().input(question));
client.chatAsync(req).whenComplete((resp, err) -> {
    if (err != null) { span.fail(err); }            // Record the error and end the span
    else { span.setOutput(resp); span.end(); }
});

// To attach child spans, use makeCurrent() on the same thread or pass span.context() across threads
try (Scope ignored = span.makeCurrent()) { ... }
```

`end()` and `close()` are idempotent, and `FlashcatSpan` implements `AutoCloseable`. For streaming output, call `end()` when the stream is exhausted, `close()` when it closes early, or `fail(e)` on error.

### Uninstrumented model calls

Use `llm` / `embedding` / `retrieval` to mark model calls and retrieval through custom gateways, private SDKs, or direct HTTP requests. Use `annotate` to add usage, the response model, and messages.

```java
String answer = Flashcat.llm("qwen-max", Flashcat.SpanOptions.create().provider("dashscope"), () -> {   // chat qwen-max
    Resp r = gateway.chat(messages);
    Flashcat.annotate(Flashcat.Annotation.create()
            .input(Arrays.asList(Flashcat.Message.of("user", question)))
            .output(r.text())
            .model(r.model())
            .inputTokens(r.usage().in()).outputTokens(r.usage().out())
            .finishReasons("stop"));
    return r.text();
});

List<Doc> docs = Flashcat.retrieval("kb-orders", Flashcat.SpanOptions.create().input(query), () -> search(query)); // Use the return value as documents
```

- These methods create CLIENT spans named `chat {model}`, `embeddings {model}` (input content is omitted), and `retrieval {name}`. To manage span lifetime explicitly, use `startLLM` / `startEmbedding` / `startRetrieval`. The handles also expose `annotate`.
- `Flashcat.annotate` applies only to the innermost `agent / tool / workflow / llm / embedding / retrieval` span in the current context. Outside those spans it is a no-op. Input and output follow the content capture switch. An explicitly annotated output takes precedence over the callback's return value. Negative token counts are ignored, and embedding spans record only input tokens.
- For retrieval documents, Map elements are kept as is and other values are written as `{"content": …}`. Above 32 KiB, trailing documents are dropped and a truncation marker is appended.
- Avoid calling instrumented model clients (such as openai-java with the Java Agent) inside `llm` / `embedding`. Both spans are exported, counting the same model call twice. Diagnostics count these cases in `nested_inference_spans` and report them in `problems`.

## Sessions

```java
try (Flashcat.Session s = Flashcat.session()
        .conversationId("c-123").userId("u-9").tag("tenant", "a")
        .start()) {
    String out = Flashcat.agent("kefu", () -> answer(question));
}
```

Spans started within the session scope carry `gen_ai.conversation.id` (preserving any existing value on the span), `user.id`, and `flashcat.tag.<k>`.
Baggage is off by default because HTTP headers carry it to downstream services, including model providers. Use `.propagate(true)` for cross-service correlation.

GenAI spans have the boolean AI root marker `flashcat.genai.root`. It is `true` when the span has no GenAI ancestor in the same process. The ancestry check also traverses non-GenAI spans.
The session processor sets the marker when a span starts. The Flashcat export pipeline adds it for spans that acquire GenAI attributes later. When an existing exporter is reused, only the registered session processor marks spans, at span start.

**Java Agent limitation.** The Agent extension cannot see the application's private Context keys. Session attributes reach auto-instrumented spans in two steps.
Spans created by `Flashcat.agent / tool / workflow / trace` carry session attributes directly. The extension's session processor then passes the attributes from local parent spans to their children.
With the Agent, **put model calls inside `Flashcat.agent(...)` (or workflow / trace)**. Model calls within a session need an enclosing Flashcat span to receive session attributes.
Initialization in code and Spring Boot work without this constraint.

## Content capture by source

| Source | Span source | Content capture switch |
|---|---|---|
| openai-java | Chat Completions / Embeddings use the OTel Java Agent's built-in instrumentation (`openai-java-1.1`, which only wraps clients created with `OpenAIOkHttpClient.builder()`). Both 2.20.1 and 4.101.0 produced `chat` spans in tests with Agent 2.33.1. Responses and Chat Completions on unwrapped clients use the Flashcat extension (tested with 2.20.1, 4.101.0, and 4.105.0) | The SDK sets `otel.instrumentation.genai.capture-message-content`. The Agent's openai-java instrumentation writes content to **log events** and leaves it out of span attributes. The SDK defaults to `otel.logs.exporter=none`, and the initial Flashcat release reads content only from span attributes. With `FLASHCAT_CAPTURE_CONTENT=span`, the Flashcat extension writes content to the span attributes it produces in the format defined by the contract (`gen_ai.input.messages` / `gen_ai.output.messages` / `gen_ai.system_instructions`). The export pipeline masks the content |
| Bedrock | The OTel Java Agent's aws-sdk instrumentation | Same as above |
| Spring AI | For OpenAI model calls, Spring AI 2.x creates the client directly with `new OpenAIClientImpl` and uses `withRawResponse().create` for synchronous calls. The built-in Agent instrumentation misses these calls, so the Flashcat extension creates the `chat` spans (tested with Spring AI 2.1.0-RC2). The framework's built-in Micrometer Observation (ChatClient, advisors, tools) produces OTel spans through Micrometer Tracing (`micrometer-tracing-bridge-otel`, with an `io.micrometer.tracing.Tracer` bean in the application). The Java Agent does not bridge Micrometer traces. **This path is unverified in this release** | The application controls these settings. `spring.ai.chat.observations.log-prompt` / `log-completion` and `spring.ai.chat.client.observations.log-prompt` / `log-completion` write to **application logs**. `spring.ai.tools.observations.include-content` writes tool arguments and results to span attributes. The SDK cannot change these settings and explains this in diagnostics |
| Anthropic Java SDK | Built into the SDK (uses `GlobalOpenTelemetry`) | The `anthropic.openTelemetryTracesContentMode` system property takes precedence over the `ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE` environment variable (values `content` / `metadata_only`). If both are unset, the SDK sets the system property according to the content capture switch. Only clients built with `AnthropicOkHttpClient.fromEnv()` (or with `fromEnv()` on the builder) read these settings |
| LangChain4j | OpenInference's LangChain4j instrumentation (`com.arize:openinference-instrumentation-langchain4j`). Attach the listener yourself (see the next section) | Use OpenInference's `TraceConfig` to set `hideInputs` / `hideOutputs` based on `FlashcatGenAI.captureContentEnabled()` |


## LangChain4j (OpenInference)

LangChain4j lacks a global registration entry point for automatic SDK setup. Add OpenInference's LangChain4j instrumentation and attach the listeners it creates to the model builder and AiServices.
Pass the content capture switch explicitly to OpenInference's `TraceConfig`. By default, OpenInference records conversation content, and Flashcat masking does not cover those attributes.

```xml
<dependency>
  <groupId>com.arize</groupId>
  <artifactId>openinference-instrumentation-langchain4j</artifactId>
  <version>0.1.9</version>
</dependency>
```

```java
boolean content = FlashcatGenAI.captureContentEnabled();
LangChain4jInstrumentor oi = LangChain4jInstrumentor.instrument(
        TraceConfig.builder().hideInputs(!content).hideOutputs(!content).build());

ChatModel model = OpenAiChatModel.builder()
        .apiKey(System.getenv("OPENAI_API_KEY"))
        .modelName("gpt-4o")
        .listeners(List.of(oi.createModelListener()))          // One LLM span per model call
        .build();

Assistant assistant = AiServices.builder(Assistant.class)
        .chatModel(model)
        .registerListeners(oi.createAiServiceListeners())       // Spans for AI Service calls and tool execution
        .build();
```

- Spans use the global OpenTelemetry instance (bridged by the Java Agent or registered by the SDK during initialization in code). Session processing works as usual.
- In diagnostics, `langchain4j` has `status` set to `instrumentation_not_installed` when the OpenInference package is absent (also recorded in `problems`) or `installed` when present. The SDK cannot detect whether the listener is attached, and `hint` notes this.
- Using `langchain4j-open-ai-official` (which uses openai-java) with the Java Agent counts both the openai-java span and the LangChain4j LLM span. Choose one instrumentation source.

## Masking and truncation

These rules apply only to Flashcat's own export pipeline, which wraps the OTLP exporter.

| Layer | Rule |
|---|---|
| L1 Key name | If the last segment of an attribute key is authorization, api_key, password, secret, token, cookie, private_key, or another matching name, string values are replaced with `[REDACTED]`. Keys such as `gen_ai.usage.input_tokens` are unaffected |
| L2 Content | When content capture is enabled, the first 256 KiB of each content attribute is masked for email addresses, national ID numbers, bank card numbers, and mobile phone numbers, in that order. The rules match the Python version exactly and pass all cases in `spec/fixtures/masking.json` |
| L3 Truncation | An individual string attribute longer than 512 KiB (in UTF-8 bytes) is truncated at a valid character boundary, and `flashcat.content_truncated=true` is set |

Define custom rules with `FlashcatGenAI.Options.builder().masking(MaskingConfig.builder().addPattern("order", "ORD-\\d{8}", "[REDACTED:order]").build())`.
The SDK probes custom regular expressions for backtracking when they are constructed and throws `ConfigException` if they show significant backtracking.

## Diagnostics

```java
Diagnostics d = FlashcatGenAI.diagnostics();   // Same structure as the return value of init()
System.out.println(d.toJson());
```

The fields match the other languages (`sdk.language` is `java`). They include `enabled`, `service_name`, `environment`, `endpoint`, `problems`, `notes`,
`provider` (`situation` A / B / C, `exporter_mode`, and `metrics`), `content`, `instrumentations`, `export` (`ok`, `http_401` and similar counters, `network`, and `last_error`),
and `semantics` (`orphan_inference_spans`, `nested_inference_spans`, `masked_spans`, and `truncated_spans`).

- In Java Agent mode, the application's `FlashcatGenAI.diagnostics()` reads live counters from the extension through a platform MBean (`cloud.flashcat.genai:type=AgentExtension`).
  The extension registers this MBean only after the first span starts. Until then, diagnostics can only say that the extension is loaded.
- `instrumentations` lists known libraries detected on the classpath (openai-java, bedrock, anthropic-java, spring-ai, langchain4j) and their instrumentation sources.
  openai-java has two entries. The built-in Agent instrumentation has `source=upstream` (`api` is Chat Completions / Embeddings), and the extension instrumentation has `source=flashcat` (`api=responses`).
  The extension uses Agent configuration to determine whether the instrumentation is enabled. A runtime error changes the status to `not_patched`.
- If a library is present but its model calls do not produce spans (openai-java or Bedrock without the Agent, disabled or failed openai-java instrumentation in the extension, Spring AI without the Agent, or LangChain4j),
  diagnostics record the reason in `problems` and log one WARN line per reason. In Agent mode, the extension checks again when the first span starts, using the application thread's class loader.
  This also detects libraries inside Spring Boot executable JARs.
- Initialization logs an INFO summary (JUL logger `cloud.flashcat.genai`). `debug(true)` logs the full diagnostics.

| Current setup | `exporter` | Behavior |
|---|---|---|
| No OpenTelemetry (initialization in code) | `auto` → add | Creates an SDK, exports to Flashcat, and registers the SDK globally |
| Java Agent + Flashcat extension | — | The extension builds the export pipeline in the Agent. Application-side `init()` returns situation C |
| Existing `GlobalOpenTelemetry` from another source | `auto` → none | Java cannot add processors after initialization. `problems` explains how to use the Agent extension or the Starter |
| Already exporting to Flashcat (`OTEL_EXPORTER_OTLP_*` points to a Flashcat host) | `reuse` | Reuses the exporter |

## Lifecycle

Use `FlashcatGenAI.flush(Duration)` and `FlashcatGenAI.shutdown(Duration)`. Initialization in code registers a shutdown hook that flushes and closes the SDK when the process exits.
`shutdown` closes only providers that the SDK created.

The shutdown timeout is a **total deadline** (contract §9, 5 seconds by default, configurable with `FLASHCAT_SHUTDOWN_TIMEOUT` or `shutdownTimeout(...)`). Flushing and closing traces and metrics share a single deadline measured from the start of shutdown.
Once shutdown starts, export switches to a copy of the exporter with limited retries. It makes up to 3 attempts, with base backoff delays of 200 and 400 ms (OTel applies ±20% jitter).
It retries only network errors and HTTP 429 / 502 / 503 / 504. The timeout is the smaller of the export timeout and the remaining time. Backoff and `Retry-After` also share the same deadline.
Spans still unsent at the deadline are dropped and counted in `export.dropped_timeout` in diagnostics. An explicit `flush(timeout)` uses a temporary deadline and allows export to continue afterward.
Under the Java Agent, the Agent's own shutdown hook closes the SDK, and the extension cannot remove that hook. The extension ensures that the trace and metric exporters wrapped by Flashcat return by the deadline, allowing the Agent's hook to return.
When connections are refused, the limited retries typically add about 0.6 seconds. A network black hole waits at most for the total shutdown timeout. If the ingest endpoint recovers after one or two intermittent failures, the final batch can still be sent.
For the Spring Boot Starter, Spring closes the SDK. The deadline starts when the SDK begins to close.

## Build and test

```bash
cd java
./gradlew build                     # Compile (--release 8), run unit tests, check the extension JAR, and run end-to-end tests
./gradlew :integration-tests:test   # End-to-end tests only: real Java Agent + extension, and Spring Boot Starter
```

The end-to-end tests use a local mock ingest endpoint (JDK `HttpServer` that decodes OTLP protobuf and checks the Basic auth header and gzip) and a mock OpenAI service. They make no real model requests and require no API key.
