package cloud.flashcat.genai;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanBuilder;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.ContextKey;
import java.util.ArrayList;
import java.util.Collection;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Starts and ends semantic spans (contract §4). It depends only on opentelemetry-api. In agent mode, the API bridge sends these spans through the same export pipeline.
 */
final class Semantics {
    static final AttributeKey<String> OPERATION = AttributeKey.stringKey("gen_ai.operation.name");
    static final AttributeKey<String> AGENT_NAME = AttributeKey.stringKey("gen_ai.agent.name");
    static final AttributeKey<String> AGENT_DESCRIPTION = AttributeKey.stringKey("gen_ai.agent.description");
    static final AttributeKey<String> TOOL_NAME = AttributeKey.stringKey("gen_ai.tool.name");
    static final AttributeKey<String> TOOL_TYPE = AttributeKey.stringKey("gen_ai.tool.type");
    static final AttributeKey<String> TOOL_DESCRIPTION = AttributeKey.stringKey("gen_ai.tool.description");
    static final AttributeKey<String> WORKFLOW_NAME = AttributeKey.stringKey("gen_ai.workflow.name");
    static final AttributeKey<String> INPUT_MESSAGES = AttributeKey.stringKey("gen_ai.input.messages");
    static final AttributeKey<String> OUTPUT_MESSAGES = AttributeKey.stringKey("gen_ai.output.messages");
    static final AttributeKey<String> TOOL_ARGUMENTS = AttributeKey.stringKey("gen_ai.tool.call.arguments");
    static final AttributeKey<String> TOOL_RESULT = AttributeKey.stringKey("gen_ai.tool.call.result");
    static final AttributeKey<String> ERROR_TYPE = AttributeKey.stringKey("error.type");
    static final AttributeKey<String> REQUEST_MODEL = AttributeKey.stringKey("gen_ai.request.model");
    static final AttributeKey<String> RESPONSE_MODEL = AttributeKey.stringKey("gen_ai.response.model");
    static final AttributeKey<String> PROVIDER = AttributeKey.stringKey("gen_ai.provider.name");
    static final AttributeKey<String> DATA_SOURCE = AttributeKey.stringKey("gen_ai.data_source.id");
    static final AttributeKey<String> RETRIEVAL_QUERY = AttributeKey.stringKey("gen_ai.retrieval.query.text");
    static final AttributeKey<String> RETRIEVAL_DOCUMENTS = AttributeKey.stringKey("gen_ai.retrieval.documents");
    static final AttributeKey<List<String>> FINISH_REASONS = AttributeKey.stringArrayKey("gen_ai.response.finish_reasons");
    static final AttributeKey<Long> INPUT_TOKENS = AttributeKey.longKey("gen_ai.usage.input_tokens");
    static final AttributeKey<Long> OUTPUT_TOKENS = AttributeKey.longKey("gen_ai.usage.output_tokens");
    static final AttributeKey<Long> CACHE_READ_TOKENS = AttributeKey.longKey("gen_ai.usage.cache_read.input_tokens");
    static final AttributeKey<Long> CACHE_WRITE_TOKENS = AttributeKey.longKey("gen_ai.usage.cache_write.input_tokens");

    /** Innermost span created by the semantic API in the current context, used by annotate. Spans from upstream instrumentation are excluded. */
    static final ContextKey<FlashcatSpan> FRAME = ContextKey.named("flashcat-genai-otel-semantic-frame");

    /** Serialization limit for arguments and return values, in characters. */
    static final int MAX_SERIALIZED = 32 * 1024;
    static final String TRUNCATED_SUFFIX = "…[truncated]";

    enum Kind {
        AGENT,
        TOOL,
        WORKFLOW,
        TRACE,
        LLM,
        EMBEDDING,
        RETRIEVAL;

        boolean client() {
            return this == LLM || this == EMBEDDING || this == RETRIEVAL;
        }

        boolean model() {
            return this == LLM || this == EMBEDDING;
        }
    }

    private static volatile OpenTelemetry cachedOtel;
    private static volatile Tracer cachedTracer;

    private Semantics() {}

    static void resetForTest() {
        cachedOtel = null;
        cachedTracer = null;
    }

    /**
     * Read the global instance on each call and cache the tracer per instance so a pre-init no-op tracer can be replaced.
     * Use getOrNoop() (API 1.57+). Calling get() before a global is set registers a no-op instance and blocks later initialization.
     */
    static Tracer tracer() {
        OpenTelemetry otel;
        try {
            otel = GlobalOpenTelemetry.getOrNoop();
        } catch (NoSuchMethodError e) {
            // Fall back to get() with opentelemetry-api versions before 1.57. The Java agent still bridges the API call.
            otel = GlobalOpenTelemetry.get();
        }
        Tracer t = cachedTracer;
        if (t != null && cachedOtel == otel) {
            return t;
        }
        t = otel.tracerBuilder(Version.SCOPE).setInstrumentationVersion(Version.VERSION).build();
        cachedOtel = otel;
        cachedTracer = t;
        return t;
    }

    static String spanName(Kind kind, String name) {
        switch (kind) {
            case AGENT:
                return "invoke_agent " + name;
            case TOOL:
                return "execute_tool " + name;
            case WORKFLOW:
                return "invoke_workflow " + name;
            case LLM:
                return "chat " + name;
            case EMBEDDING:
                return "embeddings " + name;
            case RETRIEVAL:
                return "retrieval " + name;
            default:
                return name;
        }
    }

    /** Starts a span with semantic and session attributes. The initial tail-sampling filter and the session processor rely on these being set at span start. */
    static Span start(Kind kind, String name, String description, String provider, Map<String, ?> attributes, Context parent) {
        SpanBuilder b = tracer().spanBuilder(spanName(kind, name))
                .setSpanKind(kind.client() ? SpanKind.CLIENT : SpanKind.INTERNAL)
                .setParent(parent);
        switch (kind) {
            case LLM:
            case EMBEDDING:
                b.setAttribute(OPERATION, kind == Kind.LLM ? "chat" : "embeddings").setAttribute(REQUEST_MODEL, name);
                if (provider != null && !provider.isEmpty()) {
                    b.setAttribute(PROVIDER, provider);
                }
                break;
            case RETRIEVAL:
                b.setAttribute(OPERATION, "retrieval").setAttribute(DATA_SOURCE, name);
                break;
            case AGENT:
                b.setAttribute(OPERATION, "invoke_agent").setAttribute(AGENT_NAME, name);
                if (description != null) {
                    b.setAttribute(AGENT_DESCRIPTION, description);
                }
                break;
            case TOOL:
                b.setAttribute(OPERATION, "execute_tool").setAttribute(TOOL_NAME, name).setAttribute(TOOL_TYPE, "function");
                if (description != null) {
                    b.setAttribute(TOOL_DESCRIPTION, description);
                }
                break;
            case WORKFLOW:
                b.setAttribute(OPERATION, "invoke_workflow").setAttribute(WORKFLOW_NAME, name);
                break;
            default:
                if (attributes != null) {
                    for (Map.Entry<String, ?> e : attributes.entrySet()) {
                        putAttribute(b, e.getKey(), e.getValue());
                    }
                }
        }
        // Write session attributes directly to the semantic span because the Java agent extension's processor cannot read the application's private Context keys.
        String conv = parent.get(SessionKeys.CONVERSATION);
        if (conv != null) {
            b.setAttribute(SessionKeys.ATTR_CONVERSATION, conv);
        }
        String user = parent.get(SessionKeys.USER);
        if (user != null) {
            b.setAttribute(SessionKeys.ATTR_USER, user);
        }
        Map<String, String> tags = parent.get(SessionKeys.TAGS);
        if (tags != null) {
            for (Map.Entry<String, String> e : tags.entrySet()) {
                b.setAttribute(SessionKeys.TAG_PREFIX + e.getKey(), e.getValue());
            }
        }
        return b.startSpan();
    }

    private static void putAttribute(SpanBuilder b, String key, Object v) {
        if (key == null || v == null) {
            return;
        }
        if (v instanceof String) {
            b.setAttribute(key, (String) v);
        } else if (v instanceof Boolean) {
            b.setAttribute(key, (Boolean) v);
        } else if (v instanceof Long || v instanceof Integer || v instanceof Short || v instanceof Byte) {
            b.setAttribute(key, ((Number) v).longValue());
        } else if (v instanceof Double || v instanceof Float) {
            b.setAttribute(key, ((Number) v).doubleValue());
        } else {
            b.setAttribute(key, String.valueOf(v));
        }
    }

    /** Writes arguments when content capture is enabled. */
    static void setInput(Span span, Kind kind, Object input) {
        if (span == null || kind == Kind.TRACE || !ContentSwitch.enabled()) {
            return;
        }
        writeInput(span, kind, input, false);
    }

    /** Writes the return value when content capture is enabled. */
    static void setOutput(Span span, Kind kind, Object output) {
        if (span == null || kind == Kind.TRACE || !ContentSwitch.enabled()) {
            return;
        }
        writeOutput(span, kind, output, false);
    }

    /** Maps content to attributes by span type (contract §4). For structured input from annotate, parse messages as a list. The caller checks the content capture switch. */
    static void writeInput(Span span, Kind kind, Object value, boolean structured) {
        switch (kind) {
            case TOOL:
                span.setAttribute(TOOL_ARGUMENTS, serialize(value));
                break;
            case RETRIEVAL:
                span.setAttribute(RETRIEVAL_QUERY, serialize(value));
                break;
            case AGENT:
            case WORKFLOW:
            case LLM:
                span.setAttribute(INPUT_MESSAGES, structured ? messagesOf(value, "user") : messages("user", serialize(value)));
                break;
            default:
                break;
        }
    }

    static void writeOutput(Span span, Kind kind, Object value, boolean structured) {
        switch (kind) {
            case TOOL:
                span.setAttribute(TOOL_RESULT, serialize(value));
                break;
            case RETRIEVAL:
                span.setAttribute(RETRIEVAL_DOCUMENTS, documents(value));
                break;
            case AGENT:
            case WORKFLOW:
            case LLM:
                span.setAttribute(OUTPUT_MESSAGES, structured ? messagesOf(value, "assistant") : messages("assistant", serialize(value)));
                break;
            default:
                break;
        }
    }

    /** Applies annotate fields by span type, ignoring fields that do not apply (contract §4.2). */
    static void annotate(FlashcatSpan h, Flashcat.Annotation a) {
        Span span = h.getSpan();
        Kind kind = h.kind();
        if (span == null || a == null || kind == Kind.TRACE) {
            return;
        }
        boolean content = ContentSwitch.enabled();
        if (a.hasInput && content) {
            writeInput(span, kind, a.input, true);
        }
        if (a.hasOutput && kind != Kind.EMBEDDING) {
            h.markOutputSet();
            if (content) {
                writeOutput(span, kind, a.output, true);
            }
        }
        if (a.model != null && kind.model()) {
            span.setAttribute(RESPONSE_MODEL, a.model);
        }
        if (kind.model()) {
            putCount(span, INPUT_TOKENS, a.inputTokens);
        }
        if (kind == Kind.LLM) {
            putCount(span, OUTPUT_TOKENS, a.outputTokens);
            putCount(span, CACHE_READ_TOKENS, a.cacheReadInputTokens);
            putCount(span, CACHE_WRITE_TOKENS, a.cacheWriteInputTokens);
            if (a.finishReasons != null) {
                span.setAttribute(FINISH_REASONS, a.finishReasons);
            }
        }
        for (Map.Entry<String, String> e : a.tags.entrySet()) {
            span.setAttribute(SessionKeys.TAG_PREFIX + e.getKey(), e.getValue());
        }
    }

    private static void putCount(Span span, AttributeKey<Long> key, Long v) {
        if (v != null && v >= 0) {
            span.setAttribute(key, v);
        }
    }

    /** Writes one message per item if every list element is a {@link Flashcat.Message} or a map with a role. Otherwise, serializes the whole value as one text message. */
    static String messagesOf(Object value, String role) {
        Collection<?> items = value instanceof Collection ? (Collection<?>) value
                : value instanceof Object[] ? java.util.Arrays.asList((Object[]) value) : null;
        if (items != null && !items.isEmpty()) {
            StringBuilder sb = new StringBuilder("[");
            boolean ok = true;
            for (Object m : items) {
                String r;
                Object c;
                if (m instanceof Flashcat.Message) {
                    r = ((Flashcat.Message) m).role;
                    c = ((Flashcat.Message) m).content;
                } else if (m instanceof Map && ((Map<?, ?>) m).get("role") != null) {
                    r = String.valueOf(((Map<?, ?>) m).get("role"));
                    c = ((Map<?, ?>) m).get("content");
                } else {
                    ok = false;
                    break;
                }
                if (sb.length() > 1) {
                    sb.append(',');
                }
                String one = messages(r, c instanceof String ? (String) c : serialize(c));
                sb.append(one, 1, one.length() - 1);
            }
            if (ok) {
                return sb.append(']').toString();
            }
        }
        return messages(role, serialize(value));
    }

    /**
     * Builds the retrieval documents array (contract §4). Keep map elements as-is, wrap other elements in {@code {"content": …}}, and treat a non-collection, non-array value as one document.
     * When the array exceeds the limit, drop entries from the end and append a truncation marker to keep the JSON valid.
     */
    static String documents(Object value) {
        List<Object> items = new ArrayList<Object>();
        if (value instanceof Collection) {
            items.addAll((Collection<?>) value);
        } else if (value != null && value.getClass().isArray() && !(value instanceof byte[])) {
            int n = java.lang.reflect.Array.getLength(value);
            for (int i = 0; i < n; i++) {
                items.add(java.lang.reflect.Array.get(value, i));
            }
        } else {
            items.add(value);
        }
        List<Object> docs = new ArrayList<Object>(items.size());
        for (Object it : items) {
            docs.add(it instanceof Map ? it : Collections.singletonMap("content", serialize(it)));
        }
        String s = Json.write(docs);
        if (s.length() > MAX_SERIALIZED) {
            while (!docs.isEmpty() && Json.write(docs).length() > MAX_SERIALIZED) {
                docs.remove(docs.size() - 1);
            }
            Map<String, String> mark = new LinkedHashMap<String, String>();
            mark.put("content", TRUNCATED_SUFFIX);
            docs.add(mark);
            s = Json.write(docs);
        }
        return s;
    }

    /** Sets ERROR status, writes the exception class name to error.type, and records an exception event. */
    static void fail(Span span, Throwable t) {
        if (span == null || t == null) {
            return;
        }
        span.setStatus(StatusCode.ERROR, t.getMessage() == null ? "" : t.getMessage());
        span.setAttribute(ERROR_TYPE, t.getClass().getName());
        span.recordException(t);
    }

    /** {@code [{"role":"user","parts":[{"type":"text","content":"…"}]}]}。 */
    static String messages(String role, String content) {
        StringBuilder sb = new StringBuilder();
        sb.append("[{\"role\":");
        Json.quote(sb, role);
        sb.append(",\"parts\":[{\"type\":\"text\",\"content\":");
        Json.quote(sb, content);
        sb.append("}]}]");
        return sb.toString();
    }

    /**
     * Serializes up to the length limit. Strings pass through, JDK containers and primitive values become JSON, and other objects use toString() without traversing their internals.
     */
    static String serialize(Object value) {
        String s;
        try {
            if (value instanceof String) {
                s = (String) value;
            } else if (value == null || value instanceof Number || value instanceof Boolean
                    || value instanceof Map || value instanceof java.util.Collection || value.getClass().isArray()) {
                s = Json.write(value);
            } else {
                s = String.valueOf(value);
            }
        } catch (Throwable e) {
            s = value.getClass().getName() + "@" + Integer.toHexString(System.identityHashCode(value));
        }
        if (s.length() > MAX_SERIALIZED) {
            int cut = MAX_SERIALIZED;
            if (Character.isHighSurrogate(s.charAt(cut - 1))) {
                cut--;
            }
            s = s.substring(0, cut) + TRUNCATED_SUFFIX;
        }
        return s;
    }
}
