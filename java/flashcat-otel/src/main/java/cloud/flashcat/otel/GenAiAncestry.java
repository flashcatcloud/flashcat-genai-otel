package cloud.flashcat.otel;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.trace.SpanContext;
import io.opentelemetry.sdk.trace.ReadWriteSpan;
import io.opentelemetry.sdk.trace.ReadableSpan;
import java.util.Arrays;
import java.util.HashSet;
import java.util.Iterator;
import java.util.LinkedHashSet;
import java.util.Set;

/** Tracks spans with GenAI ancestors in the current process (contract §5.1). */
final class GenAiAncestry {
    static final AttributeKey<Boolean> ROOT = AttributeKey.booleanKey("flashcat.genai.root");
    private static final int MAX_DESCENDANTS = 65536;
    // Uses the same rule as the Flashcat backend to classify GenAI spans.
    private static final Set<String> GENAI_KEYS = new HashSet<String>(Arrays.asList(
            "gen_ai.operation.name", "gen_ai.request.model", "gen_ai.provider.name", "gen_ai.system",
            "openinference.span.kind", "llm.model_name", "traceloop.span.kind", "langfuse.observation.type",
            "flashcat.span.kind", "gen_ai.tool.name", "gen_ai.tool.call.id", "ai.operationId", "llm.request.type"));
    private static final String[] GENAI_PREFIXES = {"gen_ai.", "llm.", "openinference.", "traceloop.", "langfuse.observation."};
    private static final String LANGCHAIN4J_SCOPE = "com.arize.langchain4j";

    // Tracks only spans with GenAI ancestors and evicts the oldest entries first. Processes with only conventional APM spans never allocate this set.
    private volatile Set<String> descendants;

    void onStart(ReadWriteSpan span, ReadableSpan parent, Attributes parentAttrs) {
        SpanContext parentContext = span.getParentSpanContext();
        boolean hasAncestor = parentContext.isValid() && !parentContext.isRemote()
                && ((parent != null && isGenAi(parent, parentAttrs)) || has(parentContext));
        if (hasAncestor) {
            remember(span.getSpanContext());
        }
        if (isGenAi(span, span.getAttributes())) {
            span.setAttribute(ROOT, !hasAncestor);
        }
    }

    boolean has(SpanContext spanContext) {
        Set<String> ids = descendants;
        if (ids == null) {
            return false;
        }
        synchronized (this) {
            return ids.contains(spanContext.getSpanId());
        }
    }

    private synchronized void remember(SpanContext spanContext) {
        if (descendants == null) {
            descendants = new LinkedHashSet<String>();
        }
        descendants.add(spanContext.getSpanId());
        if (descendants.size() > MAX_DESCENDANTS) {
            Iterator<String> oldest = descendants.iterator();
            oldest.next();
            oldest.remove();
        }
    }

    private static boolean isGenAi(ReadableSpan span, Attributes attrs) {
        // OpenInference LangChain4j may write attributes after the span starts.
        String scope = span.getInstrumentationScopeInfo().getName();
        return LANGCHAIN4J_SCOPE.equals(scope) || scope.startsWith(LANGCHAIN4J_SCOPE + ".") || isGenAi(attrs);
    }

    static boolean isGenAi(Attributes attrs) {
        boolean vercelOperation = false;
        boolean vercelAttribute = false;
        for (AttributeKey<?> attr : attrs.asMap().keySet()) {
            String key = attr.getKey();
            if (GENAI_KEYS.contains(key)) {
                return true;
            }
            // The session processor also writes this attribute to ordinary spans.
            if (SessionKeys.ATTR_CONVERSATION.getKey().equals(key)) {
                continue;
            }
            for (String prefix : GENAI_PREFIXES) {
                if (key.startsWith(prefix)) {
                    return true;
                }
            }
            vercelOperation |= "operation.name".equals(key);
            vercelAttribute |= key.startsWith("ai.");
        }
        return vercelOperation && vercelAttribute;
    }
}
