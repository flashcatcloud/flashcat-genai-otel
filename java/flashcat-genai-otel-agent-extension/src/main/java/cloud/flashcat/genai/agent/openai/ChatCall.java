package cloud.flashcat.genai.agent.openai;

import com.openai.core.ClientOptions;
import com.openai.core.http.AsyncStreamResponse;
import com.openai.core.http.Headers;
import com.openai.core.http.HttpResponseFor;
import com.openai.core.http.StreamResponse;
import com.openai.models.chat.completions.ChatCompletion;
import com.openai.models.chat.completions.ChatCompletionChunk;
import com.openai.models.chat.completions.ChatCompletionCreateParams;
import com.openai.models.completions.CompletionUsage;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanBuilder;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.Scope;
import java.io.InputStream;
import java.util.ArrayList;
import java.util.List;
import java.util.Optional;
import java.util.TreeMap;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * Fallback span for Chat Completions. It starts only when no inference span is current, as on calls
 * that bypass the OpenTelemetry Java Agent's upstream instrumentation.
 *
 * <p>The upstream instrumentation proxies clients returned by {@code OpenAIOkHttpClient.builder().build()}.
 * The proxy intercepts only {@code create} and {@code createStreaming}. Calls bypass the proxy when
 * Spring AI 2.x constructs {@code OpenAIClientImpl} directly and calls {@code withRawResponse().create}
 * for synchronous requests, when an application constructs its own client or derives one with
 * {@code withOptions(...)}, and when any client uses {@code withRawResponse()}.
 * This instrumentation hooks the implementation classes to cover those paths. Calls through the
 * proxy already have an upstream chat span current. On streaming calls, {@link ChatFirstChunk}
 * adds only time to first token (TTFT) to that span.
 *
 * <p>Like upstream, this uses a {@code CLIENT} span named {@code chat {model}} with GenAI usage and
 * finish reasons. It also sets {@code server.address}, {@code server.port},
 * {@code openai.api.type=chat_completions}, and TTFT for streaming calls. On errors, it records the
 * HTTP status code in {@code error.type} as specified in contract §4.
 *
 * <p>The class and its entry methods are public so advice inlined into openai-java can call them
 * across packages. Application code should use the openai-java API.
 */
public final class ChatCall implements StreamObserver<ChatCompletionChunk> {
    static final String API_TYPE_CHAT = "chat_completions";
    private static final int MAX_CHOICES = 16;

    final Span span;
    private final Scope scope;
    final long startNanos;
    private final boolean capture;
    private final AtomicBoolean ended = new AtomicBoolean();
    private final AtomicBoolean scopeClosed = new AtomicBoolean();

    // The consumer thread updates state for each chunk. Another thread may end the stream.
    private volatile long firstNanos;
    private volatile String id;
    private volatile String model;
    private volatile CompletionUsage usage;
    private final TreeMap<Long, ChatContent.ChoiceAcc> choices = new TreeMap<Long, ChatContent.ChoiceAcc>();

    private ChatCall(Span span, Scope scope, long startNanos, boolean capture) {
        this.span = span;
        this.scope = scope;
        this.startNanos = startNanos;
        this.capture = capture;
    }

    // ------------------------------------------------------------------ Advice entry points

    /** Called at the start of {@code create} or {@code withRawResponse().create}. Returns null if an inference span is already current. */
    public static ChatCall start(Object params, Object clientOptions) {
        return start(params, clientOptions, false);
    }

    /**
     * Called at the start of {@code createStreaming}. Returns a {@link ChatFirstChunk} to add TTFT
     * to the current upstream chat span, or a {@code ChatCall} with a new span. Returns null on failure.
     */
    public static Object enterStreaming(Object params, Object clientOptions) {
        try {
            Span current = Span.current();
            if (OpenAiTelemetry.isInferenceSpan(current)) {
                return new ChatFirstChunk(current, System.nanoTime());
            }
            return start(params, clientOptions, true);
        } catch (Throwable t) {
            OpenAiTelemetry.recordFailure(t);
            return null;
        }
    }

    /** Called when synchronous {@code create} exits. */
    public static void end(Object call, Object response, Throwable error) {
        if (!(call instanceof ChatCall)) {
            return;
        }
        ChatCall c = (ChatCall) call;
        c.closeScope();
        if (error != null) {
            c.fail(error);
        } else {
            c.finish(response instanceof ChatCompletion ? (ChatCompletion) response : null);
        }
    }

    /**
     * Wraps the value returned by {@code withRawResponse().create}. The caller parses the body
     * later, so the span ends on {@code parse()} or {@code close()}.
     */
    @SuppressWarnings("unchecked")
    public static Object endRaw(Object call, Object response, Throwable error) {
        if (!(call instanceof ChatCall)) {
            return response;
        }
        ChatCall c = (ChatCall) call;
        c.closeScope();
        if (error != null || !(response instanceof HttpResponseFor)) {
            c.fail(error != null ? error : new IllegalStateException("withRawResponse().create 没有返回响应"));
            return response;
        }
        return new ParsedResponse((HttpResponseFor<ChatCompletion>) response, c);
    }

    /** Called when asynchronous {@code create} exits. Ends the span when the original future completes. */
    @SuppressWarnings("unchecked")
    public static void endAsync(Object call, Object future, Throwable error) {
        if (!(call instanceof ChatCall)) {
            return;
        }
        final ChatCall c = (ChatCall) call;
        c.closeScope();
        if (error != null || !(future instanceof CompletableFuture)) {
            c.fail(error != null ? error : new IllegalStateException("create 没有返回 CompletableFuture"));
            return;
        }
        ((CompletableFuture<ChatCompletion>) future).whenComplete((r, t) -> {
            if (t != null) {
                c.fail(t);
            } else {
                c.finish(r);
            }
        });
    }

    /** Observes the stream returned by synchronous {@code createStreaming} using the state from {@link #enterStreaming}. */
    @SuppressWarnings("unchecked")
    public static Object exitStreaming(Object state, Object stream, Throwable error) {
        if (state instanceof ChatCall) {
            ((ChatCall) state).closeScope();
        }
        if (!(state instanceof StreamObserver)) {
            return stream;
        }
        if (error != null || !(stream instanceof StreamResponse)) {
            if (state instanceof ChatCall) {
                ((ChatCall) state).fail(error != null ? error : new IllegalStateException("createStreaming 没有返回流"));
            }
            return stream;
        }
        return new ObservedStream<ChatCompletionChunk>((StreamResponse<ChatCompletionChunk>) stream, (StreamObserver<ChatCompletionChunk>) state);
    }

    /** Called when asynchronous {@code createStreaming} exits. */
    @SuppressWarnings("unchecked")
    public static Object exitAsyncStreaming(Object state, Object stream, Throwable error) {
        if (state instanceof ChatCall) {
            ((ChatCall) state).closeScope();
        }
        if (!(state instanceof StreamObserver)) {
            return stream;
        }
        if (error != null || !(stream instanceof AsyncStreamResponse)) {
            if (state instanceof ChatCall) {
                ((ChatCall) state).fail(error != null ? error : new IllegalStateException("createStreaming 没有返回流"));
            }
            return stream;
        }
        return new ObservedAsyncStream<ChatCompletionChunk>((AsyncStreamResponse<ChatCompletionChunk>) stream,
                (StreamObserver<ChatCompletionChunk>) state);
    }

    // ------------------------------------------------------------------ span

    // Older code and compatible APIs still use max_tokens, despite the deprecation of maxTokens().
    @SuppressWarnings("deprecation")
    private static ChatCall start(Object params, Object clientOptions, boolean stream) {
        try {
            if (!(params instanceof ChatCompletionCreateParams)) {
                return null;
            }
            Context parent = Context.current();
            if (OpenAiTelemetry.isInferenceSpan(Span.fromContext(parent))) {
                return null;
            }
            ChatCompletionCreateParams p = (ChatCompletionCreateParams) params;
            String model = p.model().asString();
            SpanBuilder b = OpenAiTelemetry.tracer()
                    .spanBuilder(model == null ? "chat" : "chat " + model)
                    .setSpanKind(SpanKind.CLIENT)
                    .setParent(parent)
                    .setAttribute(OpenAiTelemetry.OPERATION, "chat")
                    .setAttribute(OpenAiTelemetry.PROVIDER, "openai")
                    .setAttribute(OpenAiTelemetry.API_TYPE, API_TYPE_CHAT)
                    .setAttribute(OpenAiTelemetry.REQUEST_STREAM, stream);
            if (model != null) {
                b.setAttribute(OpenAiTelemetry.REQUEST_MODEL, model);
            }
            Optional<Long> max = p.maxCompletionTokens();
            if (!max.isPresent()) {
                max = p.maxTokens();
            }
            if (max.isPresent()) {
                b.setAttribute(OpenAiTelemetry.REQUEST_MAX_TOKENS, max.get());
            }
            if (p.temperature().isPresent()) {
                b.setAttribute(OpenAiTelemetry.REQUEST_TEMPERATURE, p.temperature().get());
            }
            if (p.topP().isPresent()) {
                b.setAttribute(OpenAiTelemetry.REQUEST_TOP_P, p.topP().get());
            }
            if (p.frequencyPenalty().isPresent()) {
                b.setAttribute(OpenAiTelemetry.REQUEST_FREQUENCY_PENALTY, p.frequencyPenalty().get());
            }
            if (p.presencePenalty().isPresent()) {
                b.setAttribute(OpenAiTelemetry.REQUEST_PRESENCE_PENALTY, p.presencePenalty().get());
            }
            if (clientOptions instanceof ClientOptions) {
                OpenAiTelemetry.setServer(b, ((ClientOptions) clientOptions).baseUrl());
            }
            boolean capture = OpenAiTelemetry.captureContent();
            if (capture) {
                ChatContent.request(b, p);
            }
            Span span = b.startSpan();
            Scope scope = parent.with(span).makeCurrent();
            return new ChatCall(span, scope, System.nanoTime(), capture);
        } catch (Throwable t) {
            OpenAiTelemetry.recordFailure(t);
            return null;
        }
    }

    private void closeScope() {
        if (scopeClosed.compareAndSet(false, true)) {
            try {
                scope.close();
            } catch (Throwable ignored) {
                // Ignore failures while closing the scope.
            }
        }
    }

    void fail(Throwable error) {
        if (!ended.compareAndSet(false, true)) {
            return;
        }
        try {
            OpenAiTelemetry.recordError(span, error);
        } catch (Throwable ignored) {
            // Telemetry failures leave the application call unaffected.
        } finally {
            span.end();
        }
    }

    void finish(ChatCompletion r) {
        if (!ended.compareAndSet(false, true)) {
            return;
        }
        try {
            if (r != null) {
                span.setAttribute(OpenAiTelemetry.RESPONSE_ID, r.id());
                span.setAttribute(OpenAiTelemetry.RESPONSE_MODEL, r.model());
                List<String> reasons = new ArrayList<String>();
                for (ChatCompletion.Choice c : r.choices()) {
                    reasons.add(c.finishReason().asString());
                }
                if (!reasons.isEmpty()) {
                    span.setAttribute(OpenAiTelemetry.FINISH_REASONS, reasons);
                }
                applyUsage(r.usage().orElse(null));
                if (capture) {
                    ChatContent.response(span, r);
                }
            }
        } catch (Throwable ignored) {
            // Telemetry failures leave the application call unaffected.
        } finally {
            span.end();
        }
    }

    private void applyUsage(CompletionUsage u) {
        if (u == null) {
            return;
        }
        span.setAttribute(OpenAiTelemetry.INPUT_TOKENS, u.promptTokens());
        span.setAttribute(OpenAiTelemetry.OUTPUT_TOKENS, u.completionTokens());
        // Cache hits are included in prompt_tokens.
        if (u.promptTokensDetails().isPresent() && u.promptTokensDetails().get().cachedTokens().isPresent()) {
            span.setAttribute(OpenAiTelemetry.CACHE_READ_TOKENS, u.promptTokensDetails().get().cachedTokens().get());
        }
        if (u.completionTokensDetails().isPresent() && u.completionTokensDetails().get().reasoningTokens().isPresent()) {
            span.setAttribute(OpenAiTelemetry.REASONING_TOKENS, u.completionTokensDetails().get().reasoningTokens().get());
        }
    }

    // ------------------------------------------------------------------ Streaming

    @Override
    public void onItem(ChatCompletionChunk chunk) {
        try {
            if (firstNanos == 0L && ChatFirstChunk.hasOutput(chunk)) {
                firstNanos = System.nanoTime();
            }
            id = chunk.id();
            model = chunk.model();
            if (chunk.usage().isPresent()) {
                usage = chunk.usage().get();
            }
            synchronized (choices) {
                for (ChatCompletionChunk.Choice c : chunk.choices()) {
                    ChatContent.ChoiceAcc acc = choices.get(c.index());
                    if (acc == null) {
                        if (choices.size() >= MAX_CHOICES) {
                            continue;
                        }
                        acc = new ChatContent.ChoiceAcc();
                        choices.put(c.index(), acc);
                    }
                    if (c.finishReason().isPresent()) {
                        acc.finishReason = c.finishReason().get().asString();
                    }
                    if (capture) {
                        acc.append(c.delta());
                    }
                }
            }
        } catch (Throwable ignored) {
            // Keep processing subsequent chunks if parsing this one fails.
        }
    }

    /**
     * Ends the span on exhaustion, early close, or error. A finish reason marks completion.
     * A stream without one is marked {@code stream_aborted} if no exception occurred. Idempotent.
     */
    @Override
    public void onEnd(Throwable error) {
        if (!ended.compareAndSet(false, true)) {
            return;
        }
        try {
            List<String> reasons = new ArrayList<String>();
            List<ChatContent.ChoiceAcc> accs;
            synchronized (choices) {
                accs = new ArrayList<ChatContent.ChoiceAcc>(choices.values());
            }
            for (ChatContent.ChoiceAcc a : accs) {
                if (a.finishReason != null) {
                    reasons.add(a.finishReason);
                }
            }
            if (error != null) {
                OpenAiTelemetry.recordError(span, error);
            } else if (reasons.isEmpty()) {
                span.setStatus(StatusCode.ERROR, "stream aborted");
                span.setAttribute(OpenAiTelemetry.ERROR_TYPE, "stream_aborted");
            }
            if (reasons.isEmpty()) {
                reasons.add("error");
            }
            span.setAttribute(OpenAiTelemetry.FINISH_REASONS, reasons);
            if (id != null) {
                span.setAttribute(OpenAiTelemetry.RESPONSE_ID, id);
            }
            if (model != null) {
                span.setAttribute(OpenAiTelemetry.RESPONSE_MODEL, model);
            }
            applyUsage(usage);
            if (capture) {
                ChatContent.streamed(span, accs);
            }
            long first = firstNanos;
            if (first != 0L) {
                span.setAttribute(OpenAiTelemetry.TIME_TO_FIRST_CHUNK, (first - startNanos) / 1e9);
            }
        } catch (Throwable ignored) {
            // Telemetry failures leave the application call unaffected.
        } finally {
            span.end();
        }
    }

    /**
     * Wraps the value returned by {@code withRawResponse().create}. Records response attributes
     * and ends the span when {@code parse()} yields a ChatCompletion. The span also ends on {@code close()}.
     */
    static final class ParsedResponse implements HttpResponseFor<ChatCompletion> {
        private final HttpResponseFor<ChatCompletion> delegate;
        private final ChatCall call;

        ParsedResponse(HttpResponseFor<ChatCompletion> delegate, ChatCall call) {
            this.delegate = delegate;
            this.call = call;
        }

        @Override
        public ChatCompletion parse() {
            ChatCompletion r;
            try {
                r = delegate.parse();
            } catch (RuntimeException | Error e) {
                call.fail(e);
                throw e;
            }
            call.finish(r);
            return r;
        }

        @Override
        public int statusCode() {
            return delegate.statusCode();
        }

        @Override
        public Headers headers() {
            return delegate.headers();
        }

        @Override
        public Optional<String> requestId() {
            return delegate.requestId();
        }

        @Override
        public InputStream body() {
            return delegate.body();
        }

        @Override
        public void close() {
            try {
                call.finish(null);
            } finally {
                delegate.close();
            }
        }
    }
}
