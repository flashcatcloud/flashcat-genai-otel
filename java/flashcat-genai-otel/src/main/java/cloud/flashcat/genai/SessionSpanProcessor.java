package cloud.flashcat.genai;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.context.Context;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.trace.ReadWriteSpan;
import io.opentelemetry.sdk.trace.ReadableSpan;
import io.opentelemetry.sdk.trace.SpanProcessor;
import java.util.Arrays;
import java.util.Collections;
import java.util.HashSet;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Session processor that adds session attributes to every span started within a session (contract §5).
 *
 * <ol>
 *   <li>Reads the conversation ID, user ID, and tags from private Context keys when the application and processor share a class loader (programmatic initialization and Spring Boot).
 *   <li>Child spans inherit these attributes from an in-process parent. In Java agent mode, the extension cannot see the application's private Context keys,
 *       so inheritance carries session attributes from semantic agent, tool, and workflow spans to auto-instrumented children such as model calls and HTTP spans.
 * </ol>
 *
 * <p>Also sets the AI root marker (contract §5.1) and counts orphan inference spans in orphan_inference_spans.
 */
final class SessionSpanProcessor implements SpanProcessor {
    static final AttributeKey<String> OPERATION = AttributeKey.stringKey("gen_ai.operation.name");
    private static final Set<String> INFERENCE_OPS = Collections.unmodifiableSet(
            new HashSet<String>(Arrays.asList("chat", "text_completion", "generate_content", "embeddings")));

    static final AttributeKey<String> OPENINFERENCE_KIND = AttributeKey.stringKey("openinference.span.kind");
    static final String NESTED_INFERENCE_PROBLEM = "手工 llm / embedding span 里又有被插桩的模型调用，同一次调用会重复计数："
            + "只用 llm / embedding 标没有被插桩的模型客户端，或去掉外层的手工 span";
    private static final java.util.logging.Logger logger = java.util.logging.Logger.getLogger("cloud.flashcat.genai");

    final AtomicLong orphans = new AtomicLong();
    /** Number of inference spans whose parent is an llm or embedding span created by the semantic API (contract §4.1). */
    final AtomicLong nested = new AtomicLong();
    final GenAiAncestry ancestry = new GenAiAncestry();
    private final java.util.concurrent.atomic.AtomicBoolean nestedWarned = new java.util.concurrent.atomic.AtomicBoolean();
    private volatile Runnable firstSpanHook;
    private volatile Runnable shutdownHook;

    /** Invoked once when the first span starts in agent mode. This defers MBean registration to avoid initializing JMX in premain. */
    void setFirstSpanHook(Runnable hook) {
        this.firstSpanHook = hook;
    }

    /** Invoked when the SDK shuts down via provider.shutdown. The shared shutdown deadline starts here (see {@link ExitDeadline}). */
    void setShutdownHook(Runnable hook) {
        this.shutdownHook = hook;
    }

    @Override
    public void onStart(Context parentContext, ReadWriteSpan span) {
        Runnable hook = firstSpanHook;
        if (hook != null) {
            firstSpanHook = null;
            try {
                hook.run();
            } catch (Throwable ignored) {
                // Diagnostics channel failures must leave application code unaffected.
            }
        }
        try {
            enrich(parentContext, span);
        } catch (Throwable ignored) {
            // SDK failures must leave application code unaffected.
        }
    }

    private void enrich(Context parentContext, ReadWriteSpan span) {
        Context ctx = parentContext != null ? parentContext : Context.current();

        // 1. Read session data from private Context keys.
        String conv = ctx.get(SessionKeys.CONVERSATION);
        if (conv != null && span.getAttribute(SessionKeys.ATTR_CONVERSATION) == null) {
            span.setAttribute(SessionKeys.ATTR_CONVERSATION, conv);
        }
        String user = ctx.get(SessionKeys.USER);
        if (user != null) {
            span.setAttribute(SessionKeys.ATTR_USER, user);
        }
        Map<String, String> tags = ctx.get(SessionKeys.TAGS);
        if (tags != null) {
            for (Map.Entry<String, String> e : tags.entrySet()) {
                span.setAttribute(SessionKeys.TAG_PREFIX + e.getKey(), e.getValue());
            }
        }

        // 2. Fill in missing session attributes from the in-process parent span.
        Span parent = Span.fromContext(ctx);
        SpanContext psc = parent.getSpanContext();
        boolean hasParent = psc.isValid();
        ReadableSpan localParent = hasParent && !psc.isRemote() && parent instanceof ReadableSpan ? (ReadableSpan) parent : null;
        Attributes parentAttrs = Attributes.empty();
        if (localParent != null) {
            String pconv = localParent.getAttribute(SessionKeys.ATTR_CONVERSATION);
            if (pconv != null && span.getAttribute(SessionKeys.ATTR_CONVERSATION) == null) {
                span.setAttribute(SessionKeys.ATTR_CONVERSATION, pconv);
            }
            String puser = localParent.getAttribute(SessionKeys.ATTR_USER);
            if (puser != null && span.getAttribute(SessionKeys.ATTR_USER) == null) {
                span.setAttribute(SessionKeys.ATTR_USER, puser);
            }
            parentAttrs = localParent.getAttributes();
            for (Map.Entry<AttributeKey<?>, Object> e : parentAttrs.asMap().entrySet()) {
                String k = e.getKey().getKey();
                if (k.startsWith(SessionKeys.TAG_PREFIX) && e.getValue() instanceof String) {
                    AttributeKey<String> tk = AttributeKey.stringKey(k);
                    if (span.getAttribute(tk) == null) {
                        span.setAttribute(tk, (String) e.getValue());
                    }
                }
            }
        }

        ancestry.onStart(span, localParent, parentAttrs);

        // 3. Count orphan inference spans.
        String op = span.getAttribute(OPERATION);
        boolean inference = op != null && INFERENCE_OPS.contains(op);
        if (!hasParent && inference) {
            orphans.incrementAndGet();
        }

        // 4. An llm or embedding span created by the semantic API as parent causes the call to count twice (contract §4.1).
        // Use the parent's scope and operation name because the Java agent cannot read the application's private Context keys.
        String oi = span.getAttribute(OPENINFERENCE_KIND);
        if ((inference || "LLM".equals(oi) || "EMBEDDING".equals(oi)) && localParent != null) {
            String pop = localParent.getAttribute(OPERATION);
            if (("chat".equals(pop) || "embeddings".equals(pop))
                    && Version.SCOPE.equals(localParent.getInstrumentationScopeInfo().getName())) {
                nested.incrementAndGet();
                if (nestedWarned.compareAndSet(false, true)) {
                    logger.warning("flashcat-genai-otel: " + NESTED_INFERENCE_PROBLEM);
                }
            }
        }
    }

    @Override
    public boolean isStartRequired() {
        return true;
    }

    @Override
    public void onEnd(ReadableSpan span) {}

    @Override
    public boolean isEndRequired() {
        return false;
    }

    @Override
    public CompletableResultCode shutdown() {
        Runnable hook = shutdownHook;
        if (hook != null) {
            try {
                hook.run();
            } catch (Throwable ignored) {
                // Timer failures must leave shutdown unaffected.
            }
        }
        return CompletableResultCode.ofSuccess();
    }

    @Override
    public String toString() {
        return "FlashcatSessionSpanProcessor";
    }
}
