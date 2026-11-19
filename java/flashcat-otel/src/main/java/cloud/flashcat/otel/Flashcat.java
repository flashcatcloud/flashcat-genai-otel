package cloud.flashcat.otel;

import io.opentelemetry.api.baggage.Baggage;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.Scope;
import java.util.Arrays;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * Semantic API for tracing agents, tools, workflows, and sessions (contract §4 and §5).
 *
 * <pre>{@code
 * try (Flashcat.Session s = Flashcat.session().conversationId("c-123").userId("u-9").tag("tenant", "a").start()) {
 *     String out = Flashcat.agent("kefu", () -> answer(question));
 * }
 * String hits = Flashcat.tool("search_kb", () -> search(query));
 * Flashcat.workflow("ticket_flow", () -> { handle(ticket); });
 * Flashcat.trace("batch_job", () -> { runJob(); });
 * }</pre>
 *
 * <ul>
 *   <li>Callback overloads run immediately, with or without a return value. The span ends when the callback returns or throws.
 *       Callbacks can throw checked exceptions. The SDK rethrows the same exception, sets span status to ERROR, and writes {@code error.type}.
 *   <li>Methods such as {@link #startAgent(String)} return a {@link FlashcatSpan} handle for asynchronous and streaming calls.
 *   <li>The API depends only on opentelemetry-api and gets its tracer from {@code GlobalOpenTelemetry}. The Java agent bridges this API.
 *       Programmatic initialization and Spring Boot Starter use the registered global instance. With neither, the tracer is a no-op and callbacks still run.
 *   <li>If the SDK fails, it invokes the original callback directly.
 * </ul>
 */
public final class Flashcat {
    private Flashcat() {}

    /**
     * Callback that returns a value and can throw a checked exception.
     *
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     */
    @FunctionalInterface
    public interface CheckedSupplier<T, E extends Throwable> {
        /**
         * Runs the callback.
         *
         * @return the result
         * @throws E the original application exception
         */
        T get() throws E;
    }

    /**
     * Callback that can throw a checked exception and returns no value.
     *
     * @param <E> type of exception the callback can throw
     */
    @FunctionalInterface
    public interface CheckedRunnable<E extends Throwable> {
        /**
         * Runs the callback.
         *
         * @throws E the original application exception
         */
        void run() throws E;
    }

    // ------------------------------------------------------------------ agent

    /**
     * Traces an agent entry point with an {@code invoke_agent {name}} span.
     *
     * @param name agent name ({@code gen_ai.agent.name})
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T agent(String name, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.AGENT, name, null, null, fn);
    }

    /**
     * Traces an agent entry point with an optional description and input. Input is recorded only when content capture is enabled.
     *
     * @param name agent name
     * @param options description and input
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T agent(String name, SpanOptions options, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.AGENT, name, options, null, fn);
    }

    /**
     * Traces an agent entry point with a callback that returns no value.
     *
     * @param name agent name
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void agent(String name, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.AGENT, name, null, null, asSupplier(fn));
    }

    /**
     * Traces an agent entry point with a callback that returns no value and can take an optional description and input.
     *
     * @param name agent name
     * @param options description and input
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void agent(String name, SpanOptions options, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.AGENT, name, options, null, asSupplier(fn));
    }

    // ------------------------------------------------------------------ tool

    /**
     * Traces a tool call with an {@code execute_tool {name}} span and sets {@code gen_ai.tool.type=function}.
     *
     * @param name tool name ({@code gen_ai.tool.name})
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T tool(String name, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.TOOL, name, null, null, fn);
    }

    /**
     * Traces a tool call with an optional description and input. Writes {@code gen_ai.tool.call.arguments} only when content capture is enabled.
     *
     * @param name tool name
     * @param options description and input
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T tool(String name, SpanOptions options, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.TOOL, name, options, null, fn);
    }

    /**
     * Traces a tool call with a callback that returns no value.
     *
     * @param name tool name
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void tool(String name, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.TOOL, name, null, null, asSupplier(fn));
    }

    /**
     * Traces a tool call with an optional description and input and a callback that returns no value.
     *
     * @param name tool name
     * @param options description and input
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void tool(String name, SpanOptions options, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.TOOL, name, options, null, asSupplier(fn));
    }

    // ------------------------------------------------------------------ workflow

    /**
     * Traces a workflow with an {@code invoke_workflow {name}} span.
     *
     * @param name workflow name ({@code gen_ai.workflow.name})
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T workflow(String name, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.WORKFLOW, name, null, null, fn);
    }

    /**
     * Traces a workflow with optional input. Workflows have no description attribute, so the description option is ignored.
     *
     * @param name workflow name
     * @param options input
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T workflow(String name, SpanOptions options, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.WORKFLOW, name, options, null, fn);
    }

    /**
     * Traces a workflow with a callback that returns no value.
     *
     * @param name workflow name
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void workflow(String name, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.WORKFLOW, name, null, null, asSupplier(fn));
    }

    /**
     * Traces a workflow with optional input and a callback that returns no value.
     *
     * @param name workflow name
     * @param options input
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void workflow(String name, SpanOptions options, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.WORKFLOW, name, options, null, asSupplier(fn));
    }

    // ------------------------------------------------------------------ trace

    /**
     * Creates a span with the supplied name outside the GenAI semantic conventions, for use as the root span of a script or job.
     *
     * @param name span name
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T trace(String name, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.TRACE, name, null, null, fn);
    }

    /**
     * Creates an ordinary span with caller-supplied attributes. Strings, booleans, integers, and floating-point values are written by type.
     * Other values are converted to strings.
     *
     * @param name span name
     * @param attributes span attributes
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T trace(String name, Map<String, ?> attributes, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.TRACE, name, null, attributes, fn);
    }

    /**
     * Creates an ordinary span from a callback that returns no value.
     *
     * @param name span name
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void trace(String name, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.TRACE, name, null, null, asSupplier(fn));
    }

    /**
     * Creates an ordinary span with attributes and a callback that returns no value.
     *
     * @param name span name
     * @param attributes span attributes
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void trace(String name, Map<String, ?> attributes, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.TRACE, name, null, attributes, asSupplier(fn));
    }


    // ------------------------------------------------------------------ Uninstrumented model calls (contract §4 and §4.1)

    /**
     * Traces an uninstrumented model call, such as a custom gateway or private SDK, with a {@code chat {model}} span.
     * Use {@link #annotate} to add usage, the response model, and messages. Calling an instrumented model client inside the callback counts the call twice.
     *
     * @param model requested model ({@code gen_ai.request.model})
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T llm(String model, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.LLM, model, null, null, fn);
    }

    /**
     * Traces an uninstrumented model call, such as a custom gateway or private SDK, with optional provider and input.
     * Input is recorded only when content capture is enabled. Use {@link #annotate} to add usage, the response model, and messages.
     * Calling an instrumented model client inside the callback counts the call twice.
     *
     * @param model requested model ({@code gen_ai.request.model})
     * @param options provider and input
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T llm(String model, SpanOptions options, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.LLM, model, options, null, fn);
    }

    /**
     * Traces an uninstrumented model call, such as a custom gateway or private SDK, with a callback that returns no value.
     * Use {@link #annotate} to add usage, the response model, and messages. Calling an instrumented model client inside the callback counts the call twice.
     *
     * @param model requested model ({@code gen_ai.request.model})
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void llm(String model, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.LLM, model, null, null, asSupplier(fn));
    }

    /**
     * Traces an uninstrumented model call, such as a custom gateway or private SDK, with optional provider and input
     * and a callback that returns no value. Use {@link #annotate} to add usage, the response model, and messages.
     * Calling an instrumented model client inside the callback counts the call twice.
     *
     * @param model requested model ({@code gen_ai.request.model})
     * @param options provider and input
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void llm(String model, SpanOptions options, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.LLM, model, options, null, asSupplier(fn));
    }

    /**
     * Traces an uninstrumented embedding call with an {@code embeddings {model}} span. Input content is omitted.
     *
     * @param model requested model
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T embedding(String model, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.EMBEDDING, model, null, null, fn);
    }

    /**
     * Traces an uninstrumented embedding call with optional provider and input. Embedding spans omit input content.
     * The content capture switch gates input capture.
     *
     * @param model requested model
     * @param options provider and input
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T embedding(String model, SpanOptions options, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.EMBEDDING, model, options, null, fn);
    }

    /**
     * Traces an uninstrumented embedding call with a callback that returns no value. Input content is omitted.
     *
     * @param model requested model
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void embedding(String model, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.EMBEDDING, model, null, null, asSupplier(fn));
    }

    /**
     * Traces an uninstrumented embedding call with optional provider and input and a callback that returns no value. Input content is omitted.
     *
     * @param model requested model
     * @param options provider and input
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void embedding(String model, SpanOptions options, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.EMBEDDING, model, options, null, asSupplier(fn));
    }

    /**
     * Traces a retrieval call, such as a vector database or search engine query, with a {@code retrieval {name}} span.
     * The return value supplies the retrieved documents.
     *
     * @param name data source name ({@code gen_ai.data_source.id})
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T retrieval(String name, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.RETRIEVAL, name, null, null, fn);
    }

    /**
     * Traces a retrieval call, such as a vector database or search engine query, with optional provider and input.
     * The return value supplies the retrieved documents. Input is recorded only when content capture is enabled.
     *
     * @param name data source name ({@code gen_ai.data_source.id})
     * @param options provider and input
     * @param fn application callback
     * @param <T> return type
     * @param <E> type of exception the callback can throw
     * @return the callback result
     * @throws E the original exception thrown by the callback
     */
    public static <T, E extends Throwable> T retrieval(String name, SpanOptions options, CheckedSupplier<T, E> fn) throws E {
        return run(Semantics.Kind.RETRIEVAL, name, options, null, fn);
    }

    /**
     * Traces a retrieval call, such as a vector database or search engine query, with a callback that returns no value.
     *
     * @param name data source name ({@code gen_ai.data_source.id})
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void retrieval(String name, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.RETRIEVAL, name, null, null, asSupplier(fn));
    }

    /**
     * Traces a retrieval call, such as a vector database or search engine query, with optional provider and input
     * and a callback that returns no value.
     *
     * @param name data source name ({@code gen_ai.data_source.id})
     * @param options provider and input
     * @param fn application callback
     * @param <E> type of exception the callback can throw
     * @throws E the original exception thrown by the callback
     */
    public static <E extends Throwable> void retrieval(String name, SpanOptions options, CheckedRunnable<E> fn) throws E {
        run(Semantics.Kind.RETRIEVAL, name, options, null, asSupplier(fn));
    }

    /**
     * Adds data to the innermost semantic API span (agent, tool, workflow, llm, embedding, or retrieval) in the current context (contract §4.2).
     * When no such span is active, this method is a no-op. Spans from other instrumentation are left alone.
     *
     * <pre>{@code
     * String answer = Flashcat.llm("qwen-max", Flashcat.SpanOptions.create().provider("dashscope"), () -> {
     *     Resp r = gateway.chat(messages);
     *     Flashcat.annotate(Flashcat.Annotation.create()
     *             .input(messages).output(r.text()).model(r.model())
     *             .inputTokens(r.usage().in()).outputTokens(r.usage().out()).finishReasons("stop"));
     *     return r.text();
     * });
     * }</pre>
     *
     * @param annotation fields to write
     */
    public static void annotate(Annotation annotation) {
        try {
            FlashcatSpan h = Context.current().get(Semantics.FRAME);
            if (h != null) {
                h.annotate(annotation);
            }
        } catch (Throwable ignored) {
            // SDK failures must leave application code unaffected.
        }
    }

    // ------------------------------------------------------------------ Span handles

    /**
     * Starts an agent span and returns its handle. The span remains outside the current context until explicitly made current.
     *
     * @param name agent name
     * @return the span handle
     */
    public static FlashcatSpan startAgent(String name) {
        return startSpan(Semantics.Kind.AGENT, name, null);
    }

    /**
     * Starts an agent span with an optional description and input.
     *
     * @param name agent name
     * @param options description and input
     * @return the span handle
     */
    public static FlashcatSpan startAgent(String name, SpanOptions options) {
        return startSpan(Semantics.Kind.AGENT, name, options);
    }

    /**
     * Starts a tool span.
     *
     * @param name tool name
     * @return the span handle
     */
    public static FlashcatSpan startTool(String name) {
        return startSpan(Semantics.Kind.TOOL, name, null);
    }

    /**
     * Starts a tool span with an optional description and input.
     *
     * @param name tool name
     * @param options description and input
     * @return the span handle
     */
    public static FlashcatSpan startTool(String name, SpanOptions options) {
        return startSpan(Semantics.Kind.TOOL, name, options);
    }

    /**
     * Starts a workflow span.
     *
     * @param name workflow name
     * @return the span handle
     */
    public static FlashcatSpan startWorkflow(String name) {
        return startSpan(Semantics.Kind.WORKFLOW, name, null);
    }

    /**
     * Starts a workflow span with optional input.
     *
     * @param name workflow name
     * @param options input
     * @return the span handle
     */
    public static FlashcatSpan startWorkflow(String name, SpanOptions options) {
        return startSpan(Semantics.Kind.WORKFLOW, name, options);
    }


    /**
     * Starts a {@code chat {model}} span for the caller to end.
     *
     * @param model requested model
     * @return the span handle
     */
    public static FlashcatSpan startLLM(String model) {
        return startSpan(Semantics.Kind.LLM, model, null);
    }

    /**
     * Starts a {@code chat {model}} span with optional provider and input for the caller to end.
     *
     * @param model requested model
     * @param options provider and input
     * @return the span handle
     */
    public static FlashcatSpan startLLM(String model, SpanOptions options) {
        return startSpan(Semantics.Kind.LLM, model, options);
    }

    /**
     * Starts an {@code embeddings {model}} span for the caller to end.
     *
     * @param model requested model
     * @return the span handle
     */
    public static FlashcatSpan startEmbedding(String model) {
        return startSpan(Semantics.Kind.EMBEDDING, model, null);
    }

    /**
     * Starts an {@code embeddings {model}} span with optional provider and input for the caller to end.
     *
     * @param model requested model
     * @param options provider and input
     * @return the span handle
     */
    public static FlashcatSpan startEmbedding(String model, SpanOptions options) {
        return startSpan(Semantics.Kind.EMBEDDING, model, options);
    }

    /**
     * Starts a {@code retrieval {name}} span for the caller to end.
     *
     * @param name data source name
     * @return the span handle
     */
    public static FlashcatSpan startRetrieval(String name) {
        return startSpan(Semantics.Kind.RETRIEVAL, name, null);
    }

    /**
     * Starts a {@code retrieval {name}} span with optional provider and input for the caller to end.
     *
     * @param name data source name
     * @param options provider and input
     * @return the span handle
     */
    public static FlashcatSpan startRetrieval(String name, SpanOptions options) {
        return startSpan(Semantics.Kind.RETRIEVAL, name, options);
    }

    // ------------------------------------------------------------------ Sessions

    /**
     * Creates a session builder. New spans in the session scope carry {@code gen_ai.conversation.id}, {@code user.id}, and {@code flashcat.tag.*}.
     * Baggage is off by default because HTTP headers carry it downstream, including to model providers.
     *
     * @return a session builder
     */
    public static SessionBuilder session() {
        return new SessionBuilder();
    }

    // ------------------------------------------------------------------ Implementation

    private static <E extends Throwable> CheckedSupplier<Void, E> asSupplier(final CheckedRunnable<E> fn) {
        return new CheckedSupplier<Void, E>() {
            @Override
            public Void get() throws E {
                fn.run();
                return null;
            }
        };
    }

    private static <T, E extends Throwable> T run(
            Semantics.Kind kind, String name, SpanOptions options, Map<String, ?> attributes, CheckedSupplier<T, E> fn) throws E {
        Span span = null;
        Scope scope = null;
        FlashcatSpan handle;
        try {
            Context parent = Context.current();
            span = Semantics.start(kind, String.valueOf(name), options != null ? options.description : null,
                    options != null ? options.provider : null, attributes, parent);
            handle = new FlashcatSpan(span, kind, parent);
            scope = handle.context().makeCurrent();
            if (options != null && options.hasInput) {
                Semantics.setInput(span, kind, options.input);
            }
        } catch (Throwable sdkError) {
            // Clean up partial SDK state and run the original callback directly.
            closeQuietly(scope);
            endQuietly(span);
            return fn.get();
        }
        T result;
        try {
            result = fn.get();
        } catch (Throwable t) {
            closeQuietly(scope);
            try {
                Semantics.fail(span, t);
            } catch (Throwable ignored) {
                // SDK failures must leave application code unaffected.
            }
            endQuietly(span);
            throw Flashcat.<RuntimeException>sneakyThrow(t);
        }
        closeQuietly(scope);
        try {
            if (!handle.outputSet()) {
                Semantics.setOutput(span, kind, result);
            }
        } catch (Throwable ignored) {
            // SDK failures must leave application code unaffected.
        }
        endQuietly(span);
        return result;
    }

    private static FlashcatSpan startSpan(Semantics.Kind kind, String name, SpanOptions options) {
        try {
            Context parent = Context.current();
            Span span = Semantics.start(kind, String.valueOf(name), options != null ? options.description : null,
                    options != null ? options.provider : null, null, parent);
            FlashcatSpan h = new FlashcatSpan(span, kind, parent);
            if (options != null && options.hasInput) {
                h.setInput(options.input);
            }
            return h;
        } catch (Throwable e) {
            return FlashcatSpan.noop(kind);
        }
    }

    /** Rethrows the original exception object without wrapping it. */
    @SuppressWarnings("unchecked")
    private static <X extends Throwable> RuntimeException sneakyThrow(Throwable t) throws X {
        throw (X) t;
    }

    private static void closeQuietly(Scope scope) {
        if (scope == null) {
            return;
        }
        try {
            scope.close();
        } catch (Throwable ignored) {
            // Ignore scope cleanup failures.
        }
    }

    private static void endQuietly(Span span) {
        if (span == null) {
            return;
        }
        try {
            span.end();
        } catch (Throwable ignored) {
            // Ignore span cleanup failures.
        }
    }

    static void resetForTest() {
        Semantics.resetForTest();
    }

    // ------------------------------------------------------------------ Types

    /**
     * Optional semantic span settings include a description (agent or tool {@code gen_ai.*.description}) and input (recorded only when content capture is enabled).
     *
     * <pre>{@code
     * Flashcat.tool("search_kb", Flashcat.SpanOptions.create().description("Search the knowledge base").input(query), () -> search(query));
     * }</pre>
     */
    public static final class SpanOptions {
        private String description;
        private String provider;
        private Object input;
        private boolean hasInput;

        private SpanOptions() {}

        /** @return empty span options */
        public static SpanOptions create() {
            return new SpanOptions();
        }

        /**
         * Description written to {@code gen_ai.agent.description} for agents or {@code gen_ai.tool.description} for tools.
         *
         * @param description span description
         * @return this
         */
        public SpanOptions description(String description) {
            this.description = description;
            return this;
        }

        /**
         * Input strings pass through, maps, lists, arrays, and primitive values become JSON, and other objects use toString().
         * Values exceeding 32 KiB are truncated.
         *
         * @param input call arguments
         * @return this
         */
        public SpanOptions input(Object input) {
            this.input = input;
            this.hasInput = true;
            return this;
        }

        /**
         * Model provider for llm and embedding spans ({@code gen_ai.provider.name}), such as openai or dashscope. Other span types ignore it.
         *
         * @param provider model provider
         * @return this
         */
        public SpanOptions provider(String provider) {
            this.provider = provider;
            return this;
        }
    }

    /**
     * Options for {@link Flashcat#annotate} (contract §4.2). Only fields that are set and apply to the current span type are written.
     */
    public static final class Annotation {
        Object input;
        boolean hasInput;
        Object output;
        boolean hasOutput;
        String model;
        Long inputTokens;
        Long outputTokens;
        Long cacheReadInputTokens;
        Long cacheWriteInputTokens;
        List<String> finishReasons;
        final Map<String, String> tags = new LinkedHashMap<String, String>();

        private Annotation() {}

        /** @return an empty annotation */
        public static Annotation create() {
            return new Annotation();
        }

        /**
         * Agent, workflow, and llm input is a string or a list of {@link Message} objects or maps with role and content.
         * Tool input is the arguments. Retrieval input is the query text.
         *
         * @param input input to record
         * @return this
         */
        public Annotation input(Object input) {
            this.input = input;
            this.hasInput = true;
            return this;
        }

        /**
         * Agent, workflow, and llm output is a string or a list of messages. Tool output is the result. Retrieval output is a list of documents.
         * Map entries pass through and other values go into the content field.
         *
         * @param output output to record
         * @return this
         */
        public Annotation output(Object output) {
            this.output = output;
            this.hasOutput = true;
            return this;
        }

        /**
         * Response model for llm and embedding spans ({@code gen_ai.response.model}).
         *
         * @param model response model
         * @return this
         */
        public Annotation model(String model) {
            this.model = model;
            return this;
        }

        /**
         * Input tokens for llm and embedding spans. Negative counts are ignored.
         *
         * @param tokens token count
         * @return this
         */
        public Annotation inputTokens(long tokens) {
            this.inputTokens = tokens;
            return this;
        }

        /**
         * Output tokens for llm spans. Negative counts are ignored.
         *
         * @param tokens token count
         * @return this
         */
        public Annotation outputTokens(long tokens) {
            this.outputTokens = tokens;
            return this;
        }

        /**
         * Input tokens read from cache for llm spans. Negative counts are ignored.
         *
         * @param tokens token count
         * @return this
         */
        public Annotation cacheReadInputTokens(long tokens) {
            this.cacheReadInputTokens = tokens;
            return this;
        }

        /**
         * Input tokens written to cache for llm spans, corresponding to Anthropic cache creation. Negative counts are ignored.
         *
         * @param tokens token count
         * @return this
         */
        public Annotation cacheWriteInputTokens(long tokens) {
            this.cacheWriteInputTokens = tokens;
            return this;
        }

        /**
         * Finish reasons for llm spans ({@code gen_ai.response.finish_reasons}).
         *
         * @param reasons finish reasons
         * @return this
         */
        public Annotation finishReasons(String... reasons) {
            this.finishReasons = reasons == null ? null : Collections.unmodifiableList(Arrays.asList(reasons));
            return this;
        }

        /**
     * Tags apply to all span types and are written as flashcat.tag.&lt;key&gt;.
         *
         * @param key tag key
         * @param value tag value
         * @return this
         */
        public Annotation tag(String key, Object value) {
            if (key != null && value != null) {
                tags.put(key, String.valueOf(value));
            }
            return this;
        }
    }

    /** Conversation message for {@link Annotation#input} or {@link Annotation#output}. */
    public static final class Message {
        final String role;
        final Object content;

        private Message(String role, Object content) {
            this.role = role;
            this.content = content;
        }

        /**
         * Creates a conversation message.
         *
         * @param role one of system, user, assistant, or tool
         * @param content message content. Non-string values are serialized
         * @return the message
         */
        public static Message of(String role, Object content) {
            return new Message(String.valueOf(role), content);
        }
    }

    /** Session builder returned by {@link Flashcat#session()}. */
    public static final class SessionBuilder {
        private String conversationId;
        private String userId;
        private final Map<String, String> tags = new LinkedHashMap<String, String>();
        private boolean propagate;

        private SessionBuilder() {}

        /**
         * Conversation ID ({@code gen_ai.conversation.id}). An existing span value takes precedence.
         *
         * @param id conversation ID
         * @return this
         */
        public SessionBuilder conversationId(String id) {
            this.conversationId = id;
            return this;
        }

        /**
         * User ID ({@code user.id}).
         *
         * @param id user ID
         * @return this
         */
        public SessionBuilder userId(String id) {
            this.userId = id;
            return this;
        }

        /**
         * Adds a tag (flashcat.tag.&lt;key&gt;) and merges it with the enclosing session's tags.
         *
         * @param key tag key
         * @param value tag value
         * @return this
         */
        public SessionBuilder tag(String key, Object value) {
            if (key != null && value != null) {
                tags.put(key, String.valueOf(value));
            }
            return this;
        }

        /**
         * Adds multiple tags.
         *
         * @param tags tags to add
         * @return this
         */
        public SessionBuilder tags(Map<String, ?> tags) {
            if (tags != null) {
                for (Map.Entry<String, ?> e : tags.entrySet()) {
                    tag(e.getKey(), e.getValue());
                }
            }
            return this;
        }

        /**
         * Controls whether to propagate conversation and user IDs to downstream services in baggage. Defaults to false because HTTP headers carry baggage outside the process.
         *
         * @param propagate whether to propagate IDs in baggage
         * @return this
         */
        public SessionBuilder propagate(boolean propagate) {
            this.propagate = propagate;
            return this;
        }

        /**
         * Enters the session scope for try-with-resources. Close the returned session on the same thread.
         *
         * @return the session scope
         */
        public Session start() {
            try {
                Context ctx = Context.current();
                if (conversationId != null && !conversationId.isEmpty()) {
                    ctx = ctx.with(SessionKeys.CONVERSATION, conversationId);
                }
                if (userId != null && !userId.isEmpty()) {
                    ctx = ctx.with(SessionKeys.USER, userId);
                }
                if (!tags.isEmpty()) {
                    Map<String, String> merged = new LinkedHashMap<String, String>();
                    Map<String, String> outer = ctx.get(SessionKeys.TAGS);
                    if (outer != null) {
                        merged.putAll(outer);
                    }
                    merged.putAll(tags);
                    ctx = ctx.with(SessionKeys.TAGS, Collections.unmodifiableMap(merged));
                }
                if (propagate) {
                    io.opentelemetry.api.baggage.BaggageBuilder bb = Baggage.fromContext(ctx).toBuilder();
                    if (conversationId != null && !conversationId.isEmpty()) {
                        bb.put(SessionKeys.BAGGAGE_CONVERSATION, conversationId);
                    }
                    if (userId != null && !userId.isEmpty()) {
                        bb.put(SessionKeys.BAGGAGE_USER, userId);
                    }
                    ctx = ctx.with(bb.build());
                }
                return new Session(ctx.makeCurrent());
            } catch (Throwable e) {
                return new Session(Scope.noop());
            }
        }
    }

    /** Session scope exited by {@link #close()}. */
    public static final class Session implements AutoCloseable {
        private final Scope scope;
        private final AtomicBoolean closed = new AtomicBoolean();

        Session(Scope scope) {
            this.scope = scope;
        }

        /** Exits the session scope. Subsequent calls are no-ops. This method never throws. */
        @Override
        public void close() {
            if (closed.compareAndSet(false, true)) {
                closeQuietly(scope);
            }
        }
    }
}
