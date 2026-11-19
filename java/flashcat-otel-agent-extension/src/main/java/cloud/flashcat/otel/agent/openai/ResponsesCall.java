package cloud.flashcat.otel.agent.openai;

import com.openai.core.ClientOptions;
import com.openai.core.http.AsyncStreamResponse;
import com.openai.core.http.StreamResponse;
import com.openai.models.responses.Response;
import com.openai.models.responses.ResponseCreateParams;
import com.openai.models.responses.ResponseStatus;
import com.openai.models.responses.ResponseStreamEvent;
import com.openai.models.responses.ResponseUsage;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanBuilder;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.Scope;
import java.util.Collections;
import java.util.Optional;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * Span for a single Responses API call to {@code client.responses().create} or
 * {@code client.responses().createStreaming}, on synchronous or asynchronous clients.
 *
 * <p>Uses the OTel GenAI conventions for a CLIENT span named {@code chat {model}}. Request
 * attributes are set at start for the session processor and orphan inference span counting.
 * Response attributes are set at end. The model span is current during the call, so OkHttp
 * HTTP spans are its children.
 *
 * <p>As in the Go middleware, time to first token (TTFT) uses the first event with output
 * (a text, refusal, tool argument, reasoning summary, or audio delta, or a new {@code function_call}
 * item). A completed, incomplete, or failed response event supplies the full response for span
 * attributes. Exhaustion, early close, and exceptions all end the span. Closing without a
 * terminal event is marked {@code stream_aborted}.
 *
 * <p>The class and its entry methods are public so advice inlined into openai-java can call them
 * across packages. Application code should use the openai-java API.
 */
public final class ResponsesCall implements StreamObserver<ResponseStreamEvent> {
    static final String API_TYPE_RESPONSES = "responses";

    final Span span;
    private final Scope scope;
    final long startNanos;
    private final boolean capture;
    private final AtomicBoolean ended = new AtomicBoolean();
    private final AtomicBoolean scopeClosed = new AtomicBoolean();

    // The consumer thread receives events one at a time. close() may end the stream on another thread, so the fields are volatile.
    private volatile long firstNanos;
    private volatile Response terminal;
    private volatile Response created;
    private volatile String failed;

    private ResponsesCall(Span span, Scope scope, long startNanos, boolean capture) {
        this.span = span;
        this.scope = scope;
        this.startNanos = startNanos;
        this.capture = capture;
    }

    // ------------------------------------------------------------------ Advice entry points

    /**
     * Called at the start of {@code create} or {@code createStreaming}. Returns null if another
     * instrumentation or the application has already made an inference span current.
     */
    public static ResponsesCall start(Object params, Object clientOptions, boolean stream) {
        try {
            if (!(params instanceof ResponseCreateParams)) {
                return null;
            }
            Context parent = Context.current();
            if (OpenAiTelemetry.isInferenceSpan(Span.fromContext(parent))) {
                return null;
            }
            ResponseCreateParams p = (ResponseCreateParams) params;
            String model = OpenAiTelemetry.model(p.model().orElse(null));
            SpanBuilder b = OpenAiTelemetry.tracer()
                    .spanBuilder(model == null ? "chat" : "chat " + model)
                    .setSpanKind(SpanKind.CLIENT)
                    .setParent(parent)
                    .setAttribute(OpenAiTelemetry.OPERATION, "chat")
                    .setAttribute(OpenAiTelemetry.PROVIDER, "openai")
                    .setAttribute(OpenAiTelemetry.API_TYPE, API_TYPE_RESPONSES)
                    .setAttribute(OpenAiTelemetry.REQUEST_STREAM, stream);
            if (model != null) {
                b.setAttribute(OpenAiTelemetry.REQUEST_MODEL, model);
            }
            requestAttributes(b, p);
            if (clientOptions instanceof ClientOptions) {
                OpenAiTelemetry.setServer(b, ((ClientOptions) clientOptions).baseUrl());
            }
            boolean capture = OpenAiTelemetry.captureContent();
            if (capture) {
                ResponsesContent.request(b, p);
            }
            Span span = b.startSpan();
            Scope scope = parent.with(span).makeCurrent();
            return new ResponsesCall(span, scope, System.nanoTime(), capture);
        } catch (Throwable t) {
            OpenAiTelemetry.recordFailure(t);
            return null;
        }
    }

    /** Called when synchronous {@code create} exits. */
    public static void end(Object call, Object response, Throwable error) {
        if (!(call instanceof ResponsesCall)) {
            return;
        }
        ResponsesCall c = (ResponsesCall) call;
        c.closeScope();
        if (error != null) {
            c.fail(error);
        } else {
            c.finish(response instanceof Response ? (Response) response : null);
        }
    }

    /** Wraps the stream returned by synchronous {@code createStreaming} to end the span when the stream ends. */
    @SuppressWarnings("unchecked")
    public static Object endStreaming(Object call, Object stream, Throwable error) {
        if (!(call instanceof ResponsesCall)) {
            return stream;
        }
        ResponsesCall c = (ResponsesCall) call;
        c.closeScope();
        if (error != null || !(stream instanceof StreamResponse)) {
            c.fail(error != null ? error : new IllegalStateException("createStreaming 没有返回流"));
            return stream;
        }
        return new ObservedStream<ResponseStreamEvent>((StreamResponse<ResponseStreamEvent>) stream, c);
    }

    /** Called when asynchronous {@code create} exits. Ends the span when the original future completes. */
    @SuppressWarnings("unchecked")
    public static void endAsync(Object call, Object future, Throwable error) {
        if (!(call instanceof ResponsesCall)) {
            return;
        }
        final ResponsesCall c = (ResponsesCall) call;
        c.closeScope();
        if (error != null || !(future instanceof CompletableFuture)) {
            c.fail(error != null ? error : new IllegalStateException("create 没有返回 CompletableFuture"));
            return;
        }
        ((CompletableFuture<Response>) future).whenComplete((r, t) -> {
            if (t != null) {
                c.fail(t);
            } else {
                c.finish(r);
            }
        });
    }

    /** Wraps the stream returned by asynchronous {@code createStreaming} to end the span when the handler receives completion. */
    @SuppressWarnings("unchecked")
    public static Object endAsyncStreaming(Object call, Object stream, Throwable error) {
        if (!(call instanceof ResponsesCall)) {
            return stream;
        }
        ResponsesCall c = (ResponsesCall) call;
        c.closeScope();
        if (error != null || !(stream instanceof AsyncStreamResponse)) {
            c.fail(error != null ? error : new IllegalStateException("createStreaming 没有返回流"));
            return stream;
        }
        return new ObservedAsyncStream<ResponseStreamEvent>((AsyncStreamResponse<ResponseStreamEvent>) stream, c);
    }

    // ------------------------------------------------------------------ Span completion

    private void closeScope() {
        if (scopeClosed.compareAndSet(false, true)) {
            try {
                scope.close();
            } catch (Throwable ignored) {
                // Ignore failures while closing the scope.
            }
        }
    }

    /** Records ERROR and error.type for an exception and ends the span. */
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

    /** Handles the full non-streaming response. */
    void finish(Response r) {
        if (!ended.compareAndSet(false, true)) {
            return;
        }
        try {
            if (r != null) {
                applyResponse(r);
            }
        } catch (Throwable ignored) {
            // Telemetry failures leave the application call unaffected.
        } finally {
            span.end();
        }
    }

    /** Handles each stream event on the consumer thread. */
    @Override
    public void onItem(ResponseStreamEvent e) {
        try {
            if (firstNanos == 0L && isFirstOutput(e)) {
                firstNanos = System.nanoTime();
            }
            if (e.isCompleted()) {
                terminal = e.asCompleted().response();
            } else if (e.isIncomplete()) {
                terminal = e.asIncomplete().response();
            } else if (e.isFailed()) {
                Response r = e.asFailed().response();
                terminal = r;
                String msg = r.error().isPresent() ? r.error().get().message() : null;
                failed = msg != null && !msg.isEmpty() ? msg : "response failed";
            } else if (e.isError()) {
                String msg = e.asError().message();
                failed = msg != null && !msg.isEmpty() ? msg : "error event";
            } else if (e.isCreated()) {
                created = e.asCreated().response();
            }
        } catch (Throwable ignored) {
            // Keep processing later events if parsing this one fails.
        }
    }

    /**
     * Ends the span idempotently on exhaustion, early close, or a read failure. The error is null
     * for exhaustion and close. Closing before a terminal event is marked {@code stream_aborted}.
     */
    @Override
    public void onEnd(Throwable error) {
        if (!ended.compareAndSet(false, true)) {
            return;
        }
        try {
            Response r = terminal;
            if (error != null) {
                OpenAiTelemetry.recordError(span, error);
            } else if (failed != null) {
                span.setStatus(StatusCode.ERROR, failed);
                span.setAttribute(OpenAiTelemetry.ERROR_TYPE, "response_failed");
            } else if (r == null) {
                span.setStatus(StatusCode.ERROR, "stream aborted");
                span.setAttribute(OpenAiTelemetry.ERROR_TYPE, "stream_aborted");
            }
            if (r != null) {
                applyResponse(r);
            } else {
                Response c = created;
                if (c != null) {
                    span.setAttribute(OpenAiTelemetry.RESPONSE_ID, c.id());
                    String model = OpenAiTelemetry.model(c.model());
                    if (model != null) {
                        span.setAttribute(OpenAiTelemetry.RESPONSE_MODEL, model);
                    }
                }
                span.setAttribute(OpenAiTelemetry.FINISH_REASONS, Collections.singletonList("error"));
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

    // ------------------------------------------------------------------ Span attributes

    private static void requestAttributes(SpanBuilder b, ResponseCreateParams p) {
        Optional<Long> max = p.maxOutputTokens();
        if (max.isPresent()) {
            b.setAttribute(OpenAiTelemetry.REQUEST_MAX_TOKENS, max.get());
        }
        Optional<Double> temperature = p.temperature();
        if (temperature.isPresent()) {
            b.setAttribute(OpenAiTelemetry.REQUEST_TEMPERATURE, temperature.get());
        }
        Optional<Double> topP = p.topP();
        if (topP.isPresent()) {
            b.setAttribute(OpenAiTelemetry.REQUEST_TOP_P, topP.get());
        }
    }

    /** Records attributes from a full Response, either non-streaming or from a streaming terminal event. */
    private void applyResponse(Response r) {
        span.setAttribute(OpenAiTelemetry.RESPONSE_ID, r.id());
        String model = OpenAiTelemetry.model(r.model());
        if (model != null) {
            span.setAttribute(OpenAiTelemetry.RESPONSE_MODEL, model);
        }
        String reason = finishReason(r);
        if (reason != null) {
            span.setAttribute(OpenAiTelemetry.FINISH_REASONS, Collections.singletonList(reason));
        }
        Optional<ResponseUsage> usage = r.usage();
        if (usage.isPresent()) {
            ResponseUsage u = usage.get();
            span.setAttribute(OpenAiTelemetry.INPUT_TOKENS, u.inputTokens());
            span.setAttribute(OpenAiTelemetry.OUTPUT_TOKENS, u.outputTokens());
            // OpenAI includes cached tokens in input_tokens.
            try {
                span.setAttribute(OpenAiTelemetry.CACHE_READ_TOKENS, u.inputTokensDetails().cachedTokens());
            } catch (RuntimeException ignored) {
                // Compatible APIs may omit input_tokens_details.
            }
            try {
                span.setAttribute(OpenAiTelemetry.REASONING_TOKENS, u.outputTokensDetails().reasoningTokens());
            } catch (RuntimeException ignored) {
                // Compatible APIs may omit output_tokens_details.
            }
        }
        if (isFailed(r) && failed == null) {
            String msg = r.error().isPresent() ? r.error().get().message() : null;
            span.setStatus(StatusCode.ERROR, msg != null && !msg.isEmpty() ? msg : "response failed");
            span.setAttribute(OpenAiTelemetry.ERROR_TYPE, "response_failed");
        }
        if (capture) {
            ResponsesContent.response(span, r, reason);
        }
    }

    private static boolean isFailed(Response r) {
        Optional<ResponseStatus> status = r.status();
        return status.isPresent() && ResponseStatus.FAILED.equals(status.get());
    }

    /**
     * Matches the Go middleware's finish reasons. Completed maps to stop, incomplete maps to
     * incomplete_details.reason (or incomplete if absent), and failed maps to error.
     */
    static String finishReason(Response r) {
        Optional<ResponseStatus> status = r.status();
        if (!status.isPresent()) {
            return null;
        }
        String s = status.get().asString();
        if ("completed".equals(s)) {
            return "stop";
        }
        if ("incomplete".equals(s)) {
            try {
                if (r.incompleteDetails().isPresent() && r.incompleteDetails().get().reason().isPresent()) {
                    return r.incompleteDetails().get().reason().get().asString();
                }
            } catch (RuntimeException ignored) {
                // Fall back to incomplete if the reason is unavailable.
            }
            return "incomplete";
        }
        if ("failed".equals(s)) {
            return "error";
        }
        return s;
    }

    /** Detects the first event with output using the same rule as Go's markFirst. */
    private static boolean isFirstOutput(ResponseStreamEvent e) {
        if (e.isOutputTextDelta()) {
            return notEmpty(e.asOutputTextDelta().delta());
        }
        if (e.isRefusalDelta()) {
            return notEmpty(e.asRefusalDelta().delta());
        }
        if (e.isFunctionCallArgumentsDelta()) {
            return notEmpty(e.asFunctionCallArgumentsDelta().delta());
        }
        if (e.isReasoningSummaryTextDelta()) {
            return notEmpty(e.asReasoningSummaryTextDelta().delta());
        }
        if (e.isAudioDelta()) {
            return notEmpty(e.asAudioDelta().delta());
        }
        if (e.isOutputItemAdded()) {
            return e.asOutputItemAdded().item().isFunctionCall();
        }
        return false;
    }

    private static boolean notEmpty(String s) {
        return s != null && !s.isEmpty();
    }
}
