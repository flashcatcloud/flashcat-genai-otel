package cloud.flashcat.otel;

import io.opentelemetry.api.trace.Span;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.Scope;
import java.util.concurrent.atomic.AtomicBoolean;

/**
 * Semantic span handle for asynchronous, streaming, and cross-method use (returned by {@link Flashcat#startAgent(String)} and similar methods).
 *
 * <pre>{@code
 * FlashcatSpan span = Flashcat.startAgent("kefu");
 * span.setInput(question);
 * client.chatAsync(req).whenComplete((resp, err) -> {
 *     if (err != null) span.fail(err); else { span.setOutput(resp.text()); span.end(); }
 * });
 * }</pre>
 *
 * <ul>
 *   <li>Creating a handle leaves the current span unchanged. To parent new spans, use {@link #makeCurrent()}
 *       and close the returned Scope on the same thread, or pass {@link #context()} to asynchronous tasks.
 *   <li>{@link #end()} and {@link #close()} are idempotent. {@link #fail(Throwable)} records the error and ends the span.
 *   <li>These methods never throw to the caller. setInput and setOutput are no-ops with content capture disabled.
 * </ul>
 */
public final class FlashcatSpan implements AutoCloseable {
    private final Span span;
    private final Semantics.Kind kind;
    private final Context context;
    private final AtomicBoolean ended = new AtomicBoolean();
    private volatile boolean outputSet;

    FlashcatSpan(Span span, Semantics.Kind kind, Context parent) {
        this.span = span;
        this.kind = kind;
        // Semantic spans add themselves to the Context for Flashcat.annotate. Spans created by trace are excluded.
        Context c = parent.with(span);
        this.context = kind == Semantics.Kind.TRACE ? c : c.with(Semantics.FRAME, this);
    }

    /** No-op handle returned on SDK failure. */
    static FlashcatSpan noop(Semantics.Kind kind) {
        return new FlashcatSpan(Span.getInvalid(), kind, Context.root());
    }

    /**
     * Records input in {@code gen_ai.input.messages} or {@code gen_ai.tool.call.arguments} when content capture is enabled.
     *
     * @param input arguments. Strings pass through, maps, lists, arrays, and primitive values become JSON, and other objects use toString()
     * @return this
     */
    public FlashcatSpan setInput(Object input) {
        try {
            Semantics.setInput(span, kind, input);
        } catch (Throwable ignored) {
            // SDK failures must leave application code unaffected.
        }
        return this;
    }

    /**
     * Records the return value in {@code gen_ai.output.messages} or {@code gen_ai.tool.call.result} when content capture is enabled.
     *
     * @param output return value
     * @return this
     */
    public FlashcatSpan setOutput(Object output) {
        try {
            Semantics.setOutput(span, kind, output);
        } catch (Throwable ignored) {
            // SDK failures must leave application code unaffected.
        }
        return this;
    }

    /**
     * Adds messages, response model, token usage, finish reasons, and tags (contract §4.2). Fields that do not apply to this span type are ignored.
     * Input and output follow the content capture switch. Once output is set, the callback's return value cannot overwrite it.
     *
     * @param annotation fields to write
     * @return this
     */
    public FlashcatSpan annotate(Flashcat.Annotation annotation) {
        try {
            Semantics.annotate(this, annotation);
        } catch (Throwable ignored) {
            // SDK failures must leave application code unaffected.
        }
        return this;
    }

    Semantics.Kind kind() {
        return kind;
    }

    void markOutputSet() {
        outputSet = true;
    }

    boolean outputSet() {
        return outputSet;
    }

    /**
     * Records an error (ERROR status, {@code error.type}, and an exception event) and ends the span.
     *
     * @param error exception to record
     */
    public void fail(Throwable error) {
        if (!ended.compareAndSet(false, true)) {
            return;
        }
        try {
            Semantics.fail(span, error);
            span.end();
        } catch (Throwable ignored) {
            // SDK failures must leave application code unaffected.
        }
    }

    /** Ends the span. Subsequent calls are no-ops. */
    public void end() {
        if (!ended.compareAndSet(false, true)) {
            return;
        }
        try {
            span.end();
        } catch (Throwable ignored) {
            // SDK failures must leave application code unaffected.
        }
    }

    /** Equivalent to {@link #end()} for try-with-resources. */
    @Override
    public void close() {
        end();
    }

    /**
     * Makes this span current so new spans on the same thread become its children. Close the returned Scope on that thread.
     *
     * @return Scope
     */
    public Scope makeCurrent() {
        try {
            return context.makeCurrent();
        } catch (Throwable e) {
            return Scope.noop();
        }
    }

    /** @return the Context with this span as current, which can be passed to another thread */
    public Context context() {
        return context;
    }

    /** @return the underlying OTel Span for setting additional attributes */
    public Span getSpan() {
        return span;
    }

    /** @return whether the span has ended */
    public boolean isEnded() {
        return ended.get();
    }
}
