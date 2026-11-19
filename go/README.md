# flashcat-otel (Go)

The Go version of the Flashcat OTel SDK provides observability for AI agents. It supports one-line initialization, export to Flashcat, business semantics (agents, tools, workflows, and sessions), masking and truncation, and diagnostics.
Model call spans come from instrumentation submodules (`openai-go` middleware and Eino callbacks) or from instrumentation built into the model SDK (Anthropic Go SDK).

Requires Go ≥ 1.25.0. Both the core and instrumentation modules depend on OTel Go v1.46.0, whose `go.mod` requires `go 1.25.0`. Projects using Go 1.24 must upgrade their toolchain even if they use an older version of Eino.

| Module | Purpose |
|---|---|
| `github.com/flashcatcloud/flashcat-otel/go` | Core module with `flashcatotel.Init`, the semantic API, sessions, masking, and diagnostics |
| `github.com/flashcatcloud/flashcat-otel/go/instrumentation/openai` | Middleware for `github.com/openai/openai-go/v3` (also works with OpenAI-compatible APIs such as DeepSeek and Qwen) |
| `github.com/flashcatcloud/flashcat-otel/go/instrumentation/eino` | Callbacks for CloudWeGo Eino |
| `github.com/flashcatcloud/flashcat-otel/go/instrumentation/mcp` | Server middleware for the MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`) |

## Installation

```bash
go get github.com/flashcatcloud/flashcat-otel/go@v0.1.0
go get github.com/flashcatcloud/flashcat-otel/go/instrumentation/openai@v0.1.0   # if you use openai-go
go get github.com/flashcatcloud/flashcat-otel/go/instrumentation/eino@v0.1.0     # if you use Eino
go get github.com/flashcatcloud/flashcat-otel/go/instrumentation/mcp@v0.1.0      # if you use the MCP Go SDK
```

```bash
export OTEL_SERVICE_NAME=kefu-agent
export FLASHCAT_ENDPOINT=https://ingest.example.com/v1/traces   # full URL including /v1/traces
export FLASHCAT_API_KEY=keyID:secret                              # the SDK encodes this for Basic authentication
```

## Initialization

```go
package main

import (
	"context"
	"log"

	flashcatotel "github.com/flashcatcloud/flashcat-otel/go"
)

func main() {
	ctx := context.Background()
	// Initialize before creating model clients.
	sdk, err := flashcatotel.Init(ctx,
		flashcatotel.WithServiceName("kefu-agent"),
		flashcatotel.WithCaptureContent(flashcatotel.ContentOff), // Content capture is off by default. Use ContentSpan to enable it.
	)
	if err != nil {
		log.Fatal(err) // Only invalid explicit options, such as WithCaptureContent("true"), reach this branch.
	}
	defer sdk.Shutdown(context.Background()) // Go has no process exit hook. Use defer to export the remaining data.

	d := sdk.Diagnostics() // Stable structure. After json.Marshal, the fields match those in the other SDKs.
	log.Printf("flashcat enabled=%v problems=%v", d.Enabled, d.Problems)
}
```

`Init` returns an error only for invalid explicit options. It records issues such as a missing endpoint, missing credentials, or an invalid application name in `problems` in the diagnostics. In those cases, the returned `*SDK` remains usable and the application keeps running normally. A second call to `Init` returns the `*SDK` from the first call without performing initialization again.

Available options are `WithServiceName`, `WithEnvironment`, `WithEndpoint`, `WithAPIKey`, `WithExporter`, `WithCaptureContent`, `WithMetrics`, `WithMasking`, `WithResourceAttributes`, `WithExportTimeout`, `WithShutdownTimeout`, and `WithDebug`.
The precedence order is explicit arguments > `FLASHCAT_*` > `OTEL_*` > defaults.

| Existing setup | `WithExporter` | Behavior |
|---|---|---|
| No TracerProvider in the process | `auto` → add | Creates a provider, exports to Flashcat, and sets the W3C propagator |
| Existing provider from another OTel SDK | `auto` → add | Reuses the provider and adds the Flashcat export pipeline (Go supports adding processors after setup) |
| Already exporting to Flashcat (`OTEL_EXPORTER_OTLP_ENDPOINT` points to Flashcat) | `auto` → reuse | Adds only session processing |

`sdk.Shutdown` shuts down providers created by the SDK and only flushes providers already in the process. Call `sdk.Flush(ctx)` before a short-lived script or serverless function exits.

`Flush` / `Shutdown` wait up to 5 seconds in total by default when the context has no deadline. Set `FLASHCAT_SHUTDOWN_TIMEOUT=2.5` to adjust the timeout in seconds (fractional values are supported),
or override it explicitly with `WithShutdownTimeout(2500*time.Millisecond)`. If the environment variable is invalid, non-positive, or outside the range of `time.Duration`, the SDK records the issue in `problems` and uses the default.
If the context already has a deadline, that deadline takes precedence. Flushing and shutting down traces and metrics share one deadline, including exports that drain the queue in the background.
Once `Shutdown` begins, retries stop. Each export can wait up to the smaller of the export timeout (10 seconds by default) and the remaining shutdown time. Data dropped due to a timeout counts as an export failure.
Go has no automatic exit hook, so use `defer sdk.Shutdown(context.Background())` in `main`. `os.Exit` skips deferred calls.

## openai-go middleware

```go
import (
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	flashcatopenai "github.com/flashcatcloud/flashcat-otel/go/instrumentation/openai"
)

client := openai.NewClient(option.WithMiddleware(flashcatopenai.Middleware()))
resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
	Model:    "gpt-4o",
	Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Where is my order?")},
})
```

The middleware covers chat completions, embeddings, legacy completions, and the Responses API. Both non-streaming and streaming (SSE) requests produce spans with
`gen_ai.operation.name`, `gen_ai.provider.name`, `gen_ai.request.model`, usage, and `gen_ai.response.finish_reasons`. Streaming spans also include `gen_ai.request.stream` and the time to first chunk, `gen_ai.response.time_to_first_chunk`.
When the content capture switch is on, the middleware records `gen_ai.input.messages` / `gen_ai.output.messages` (JSON). You can create the middleware before calling `Init`.

For **OpenAI-compatible APIs such as DeepSeek and Qwen**, change the base URL. The middleware infers `gen_ai.provider.name` from the hostname (for example, `deepseek`, `dashscope`, `zhipu`, or `moonshot_ai`) and falls back to `openai` if it cannot infer the provider.

```go
deepseek := openai.NewClient(
	option.WithBaseURL("https://api.deepseek.com/v1/"),
	option.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
	option.WithMiddleware(flashcatopenai.Middleware()),
)
qwen := openai.NewClient(
	option.WithBaseURL("https://dashscope.aliyuncs.com/compatible-mode/v1/"),
	option.WithAPIKey(os.Getenv("DASHSCOPE_API_KEY")),
	option.WithMiddleware(flashcatopenai.Middleware()),
)
_, err := qwen.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
	Model:    "qwen-plus",
	Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Hello")},
})
```

## Anthropic Go SDK (built-in instrumentation, no additional installation)

`github.com/anthropics/anthropic-sdk-go/v2` (verified with v2.9.0) creates OTel spans and defaults to the global TracerProvider set by `flashcatotel.Init`.
You can use that provider without setting `option.WithTracerProvider` or installing any instrumentation.

```go
import (
	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

client := anthropic.NewClient() // Reads ANTHROPIC_API_KEY.
msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
	Model:     anthropic.ModelClaudeSonnet4_5,
	MaxTokens: 1024,
	Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Where is my order?"))},
}, option.WithOpenTelemetryConversationID("c-123")) // Optional. Adds the session ID to this call's span.
```

If `ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE` is unset, `Init` sets it to `metadata_only` when the content capture switch is off or `content` when it is on. The Anthropic SDK reads this variable on every request.
You can also set the mode explicitly on the client with `option.WithOpenTelemetryTracesContentMode(option.OpenTelemetryContentModeContent)`.
To disable Anthropic spans, use `option.WithoutOpenTelemetry()` or `ANTHROPIC_OPEN_TELEMETRY=false`.

## Eino

Supports stable Eino releases starting at v0.8.0. Versions v0.8.0, v0.9.0, v0.9.22, and v0.9.23 have been verified. The minimum dependency version is v0.8.0, so projects on v0.9.22 can stay on that version.
Stable releases v0.7.37 and earlier lack the ADK Agent callback API needed to provide the current instrumentation features. Prereleases are outside the supported range.

```go
import (
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/compose"

	flashcateino "github.com/flashcatcloud/flashcat-otel/go/instrumentation/eino"
)

callbacks.AppendGlobalHandlers(flashcateino.NewHandler()) // Call once during program initialization. Concurrent calls are unsafe.
// Or apply the handler to a single invocation:
out, err := runnable.Invoke(ctx, input, compose.WithCallbacks(flashcateino.NewHandler()))
```

ChatModel → `chat`, Tool → `execute_tool`, Graph / Chain / Workflow → `invoke_workflow`, ADK Agent → `invoke_agent`, Retriever → `retrieval`, Embedding → `embeddings`. Components such as Lambda, ChatTemplate, and ToolsNode produce no spans.
If Eino's model component uses openai-go with the middleware above, one call produces two levels of spans (one chat span from Eino and one from the HTTP layer). Keep just one if needed.

## MCP server (github.com/modelcontextprotocol/go-sdk)

```bash
go get github.com/flashcatcloud/flashcat-otel/go/instrumentation/mcp@v0.1.0
```

```go
import flashcatmcp "github.com/flashcatcloud/flashcat-otel/go/instrumentation/mcp"

server := mcp.NewServer(&mcp.Implementation{Name: "orders", Version: "1.0.0"}, nil)
server.AddReceivingMiddleware(flashcatmcp.Middleware())
```

- Each `tools/call` produces a `tools/call {tool name}` SERVER span with `mcp.method.name`, `gen_ai.operation.name=execute_tool`, and `gen_ai.tool.name`, following the OTel MCP semantic conventions. Other methods, such as initialize and tools/list, produce no spans.
- When the content capture switch is on, the middleware records `gen_ai.tool.call.arguments` / `gen_ai.tool.call.result`. When the tool returns `IsError`, it records `error.type=tool_error` and sets the span status to ERROR. For JSON-RPC errors, it records the error code.
- When the client includes a W3C traceparent in the request's `_meta`, the client span becomes the parent of the server span. Flashcat's Python and Node.js client instrumentation and mcp Python 2.x include it.
- You can create the middleware before calling `flashcatotel.Init` and override the defaults with `WithTracerProvider` and `WithCaptureContent`. The diagnostics list `{"lib":"mcp","source":"flashcat"}` in `instrumentations`.
- Supports go-sdk v1.8. Support for mark3labs/mcp-go is currently unavailable.

## Semantic API for business logic

```go
err := flashcatotel.Agent(ctx, "kefu", func(ctx context.Context) error {
	return flashcatotel.Tool(ctx, "search_kb", func(ctx context.Context) error {
		/* Existing business logic */
		return nil
	}, flashcatotel.WithDescription("Search the knowledge base"))
})
```

- `Agent` / `Tool` / `Workflow` run the function inside an `invoke_agent {name}` / `execute_tool {name}` / `invoke_workflow {name}` span, respectively. If the function returns an error, the SDK returns the same error value without wrapping it and sets the span status to ERROR. If the function panics, the SDK ends the span before the panic continues.
- `Trace(ctx, name, fn, attrs...)` creates a regular root span for scripts and tasks.
- Use explicit start and end calls for streaming or spans that cross function boundaries.

```go
ctx, span := flashcatotel.StartAgent(ctx, "kefu")
span.SetInput(question) // Records input only when the content capture switch is on.
go func() {
	var answer strings.Builder
	defer func() { span.SetOutput(answer.String()); span.End(nil) }()
	/* Write to answer as it is generated. */
}()
```

`StartTool` and `StartWorkflow` work the same way. Only the first call to `End(err)` takes effect. If `err` is non-nil, it sets the span status to ERROR.

Use `Agent` / `Workflow` / `Trace` at the entry point to give model call spans a parent. The SDK never creates implicit root spans.

### Uninstrumented model calls

Use `LLM` / `Embedding` / `Retrieval` to mark model calls and retrievals made through custom gateways, private SDKs, or direct HTTP requests. Then use `Annotate` to add usage, the response model, and messages.

```go
err := flashcatotel.LLM(ctx, "qwen-max", func(ctx context.Context) error {
	resp, err := gateway.Chat(ctx, msgs)
	if err != nil {
		return err
	}
	flashcatotel.Annotate(ctx, flashcatotel.Annotation{
		Input:         []flashcatotel.Message{{Role: "user", Content: question}},
		Output:        resp.Text,
		Model:         resp.Model,
		Usage:         &flashcatotel.Usage{InputTokens: resp.Usage.In, OutputTokens: resp.Usage.Out},
		FinishReasons: []string{"stop"},
	})
	return nil
}, flashcatotel.WithProvider("dashscope"))

_ = flashcatotel.Retrieval(ctx, "kb-orders", func(ctx context.Context) error {
	docs := search(query)
	flashcatotel.Annotate(ctx, flashcatotel.Annotation{Input: query, Output: docs}) // Array of documents.
	return nil
})
```

- These produce `chat {model}`, `embeddings {model}` (without input content), and `retrieval {name}` spans, all of kind CLIENT. For explicit start and end calls, use `StartLLM` / `StartEmbedding` / `StartRetrieval`. The returned handles also provide `Annotate`.
- `Annotate(ctx, …)` applies only to the innermost `Agent / Tool / Workflow / LLM / Embedding / Retrieval` span in ctx and has no effect if there is no such span. Input and Output follow the content capture switch. A zero value in `Usage` is treated as unset. Embedding records only `InputTokens`.
- Avoid calling instrumented model clients (such as openai-go with the middleware) inside `LLM` / `Embedding`. The SDK reports both span levels and counts the same call twice. When this happens, the diagnostics increment `nested_inference_spans` and record the issue in `problems`.

## Sessions

```go
ctx = flashcatotel.WithSession(ctx, flashcatotel.Session{
	ConversationID: "c-123",
	UserID:         "u-9",
	Tags:           map[string]string{"tenant": "a"},
})
_ = flashcatotel.Agent(ctx, "kefu", answer)
```

**Every** span started in this context or a context derived from it carries `gen_ai.conversation.id` (any value already on the span takes precedence), `user.id`, and `flashcat.tag.<k>`.
Child spans also inherit these three kinds of attributes from parent spans in the same process. By default, the SDK leaves session data out of baggage because baggage travels in HTTP headers to downstream services, including model providers. Set `Propagate: true` to correlate spans across services.

GenAI spans carry a boolean `flashcat.genai.root` attribute. It is `true` when the span has no GenAI ancestor in the same process. The Flashcat export pipeline adds the marker to spans that gain GenAI attributes later. When reusing an external exporter, the SDK applies the marker only at span start. On the MCP server, the marker is `false` when the middleware gets a parent span from the request's `_meta`.

## Masking and truncation

These rules apply only to Flashcat's own export pipeline. It replaces values with `[REDACTED]` when attribute keys look like credentials (such as `authorization`, `api_key`, or `token`). When the content capture switch is on, it masks email addresses, identity card numbers, bank card numbers, and mobile phone numbers in content attributes. It truncates individual string attributes larger than 512 KiB and sets `flashcat.content_truncated=true`.
To add custom rules, use the following configuration.

```go
flashcatotel.WithMasking(flashcatotel.MaskingConfig{
	ExtraPatterns: []flashcatotel.MaskingPattern{{Name: "order", Pattern: `ORD-\d+`, Replacement: "[REDACTED:order]"}},
})
```

## Using an existing OTel setup

- If you already registered an SDK provider using `otel.SetTracerProvider`, call `flashcatotel.Init` as usual. The SDK adds the Flashcat export pipeline to your provider (situation B).
- To manage the provider yourself, add the Flashcat processor to it before calling `Init`. The SDK recognizes this as situation C and skips adding a duplicate pipeline.

```go
tp := sdktrace.NewTracerProvider(
	sdktrace.WithResource(myResource),
	sdktrace.WithSpanProcessor(flashcatotel.NewSpanProcessor()), // Session processing + masking + batching + OTLP export
)
otel.SetTracerProvider(tp)
sdk, _ := flashcatotel.Init(ctx, flashcatotel.WithServiceName("kefu-agent"))
```

`NewSpanProcessor` reads configuration using the same rules as `Init`. If the endpoint or credentials are missing, it only processes sessions and logs one warning line.

## Diagnostics

`sdk.Diagnostics()` / `flashcatotel.GetDiagnostics()` return a struct with JSON tags. After `json.Marshal`, the fields match the Python, Node.js, and Java SDKs.
`sdk.language` is `go`. The `provider` field reports situation A, B, or C. The `instrumentations` field lists instrumentation you have configured (the Go SDK does not scan directories, so unconfigured libraries are absent). The `export` field reports export counts (`ok`, `http_401`, `http_5xx`, `network`, `last_error`). The `semantics` field reports counts for orphan inference spans, masking, and truncation.
During `Init`, the SDK logs a one-line INFO summary through `log/slog`. Use `WithDebug(true)` to log the full diagnostics.

## Zero-code instrumentation at compile time (without this SDK)

To keep your application code as is, use compile-time instrumentation to inject OTel code during `go build`, then use standard OTel environment variables to point to Flashcat.

- With the official OTel tool `otelc` (opentelemetry-go-compile-instrumentation, requires Go 1.26+), run `otelc go build -o app .`
- With LoongSuite Go (`alibaba/loongsuite-go`), run `otel go build -o app .` This covers more libraries, including eino, langchaingo, and mcp.

```bash
export OTEL_SERVICE_NAME=kefu-agent
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://ingest.example.com/v1/traces
export OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic%20$(printf '%s' 'keyID:secret' | base64 | tr -d '\n')"
export OTEL_EXPORTER_OTLP_COMPRESSION=gzip
export OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=false   # otelc captures content only when set to true
./app
```

This approach runs without the SDK's masking, session attributes, and diagnostics. To avoid duplicate spans, use only one form of instrumentation per library, either compile-time instrumentation or the middleware and callbacks above.
