package cloud.flashcat.genai.agent.openai;

import com.openai.errors.OpenAIServiceException;
import com.openai.models.ChatModel;
import com.openai.models.ResponsesModel;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.api.trace.Tracer;
import java.lang.reflect.Method;
import java.net.URI;
import java.util.Arrays;
import java.util.Collections;
import java.util.HashSet;
import java.util.List;
import java.util.Optional;
import java.util.Set;
import java.util.concurrent.CompletionException;
import java.util.concurrent.ExecutionException;

/**
 * Shared openai-java instrumentation logic for attribute keys, the tracer, content capture,
 * {@code error.type}, and detection of a current inference span.
 *
 * <p>Except for the instrumentation classes, the Agent injects this package's helpers into the
 * application class loader. The helpers can only reference the OTel API loaded by the bootstrap
 * class loader, openai-java, its Jackson dependency, and the JDK.
 *
 * <ul>
 *   <li>The application classpath may already contain flashcat-genai-otel. Referencing its classes can cause name conflicts.</li>
 *   <li>The OTel SDK is hidden in the Agent's class loader. Read spans via reflection (see {@link #isInferenceSpan}).</li>
 * </ul>
 *
 * <p>Callers catch telemetry failures and the advice suppresses them so application calls continue.
 */
final class OpenAiTelemetry {
    /** Instrumentation scope name. The version comes from the system property set by the extension. */
    static final String SCOPE = "flashcat.genai.openai-java";
    /** System property set to the extension version when the extension is active. See AgentDetector in flashcat-genai-otel. */
    static final String PROP_EXTENSION = "cloud.flashcat.genai.agent.extension";
    /** The extension's resolved content capture switch, span or false. See AgentDetector in flashcat-genai-otel. */
    static final String PROP_CAPTURE_CONTENT = "cloud.flashcat.genai.agent.capture_content";
    /**
     * System property holding the first runtime instrumentation failure. InstrumentationDetector in
     * flashcat-genai-otel uses it to report {@code not_patched} under contract §8.
     */
    static final String PROP_ERROR = "cloud.flashcat.genai.agent.instrumentation.openai-java.error";

    static final AttributeKey<String> OPERATION = AttributeKey.stringKey("gen_ai.operation.name");
    static final AttributeKey<String> PROVIDER = AttributeKey.stringKey("gen_ai.provider.name");
    static final AttributeKey<String> API_TYPE = AttributeKey.stringKey("openai.api.type");
    static final AttributeKey<String> REQUEST_MODEL = AttributeKey.stringKey("gen_ai.request.model");
    static final AttributeKey<Long> REQUEST_MAX_TOKENS = AttributeKey.longKey("gen_ai.request.max_tokens");
    static final AttributeKey<Double> REQUEST_TEMPERATURE = AttributeKey.doubleKey("gen_ai.request.temperature");
    static final AttributeKey<Double> REQUEST_TOP_P = AttributeKey.doubleKey("gen_ai.request.top_p");
    static final AttributeKey<Double> REQUEST_FREQUENCY_PENALTY = AttributeKey.doubleKey("gen_ai.request.frequency_penalty");
    static final AttributeKey<Double> REQUEST_PRESENCE_PENALTY = AttributeKey.doubleKey("gen_ai.request.presence_penalty");
    static final AttributeKey<Boolean> REQUEST_STREAM = AttributeKey.booleanKey("gen_ai.request.stream");
    static final AttributeKey<String> RESPONSE_ID = AttributeKey.stringKey("gen_ai.response.id");
    static final AttributeKey<String> RESPONSE_MODEL = AttributeKey.stringKey("gen_ai.response.model");
    static final AttributeKey<List<String>> FINISH_REASONS = AttributeKey.stringArrayKey("gen_ai.response.finish_reasons");
    static final AttributeKey<Double> TIME_TO_FIRST_CHUNK = AttributeKey.doubleKey("gen_ai.response.time_to_first_chunk");
    static final AttributeKey<Long> INPUT_TOKENS = AttributeKey.longKey("gen_ai.usage.input_tokens");
    static final AttributeKey<Long> OUTPUT_TOKENS = AttributeKey.longKey("gen_ai.usage.output_tokens");
    static final AttributeKey<Long> CACHE_READ_TOKENS = AttributeKey.longKey("gen_ai.usage.cache_read.input_tokens");
    static final AttributeKey<Long> REASONING_TOKENS = AttributeKey.longKey("gen_ai.usage.reasoning.output_tokens");
    static final AttributeKey<String> SERVER_ADDRESS = AttributeKey.stringKey("server.address");
    static final AttributeKey<Long> SERVER_PORT = AttributeKey.longKey("server.port");
    static final AttributeKey<String> ERROR_TYPE = AttributeKey.stringKey("error.type");
    static final AttributeKey<String> INPUT_MESSAGES = AttributeKey.stringKey("gen_ai.input.messages");
    static final AttributeKey<String> OUTPUT_MESSAGES = AttributeKey.stringKey("gen_ai.output.messages");
    static final AttributeKey<String> SYSTEM_INSTRUCTIONS = AttributeKey.stringKey("gen_ai.system_instructions");

    private static final Set<String> INFERENCE_OPS = Collections.unmodifiableSet(
            new HashSet<String>(Arrays.asList("chat", "text_completion", "generate_content", "embeddings")));
    private static final String FALLBACK_VERSION = "0.1.0";
    private static final ReadableSpanMethods READABLE_SPAN = new ReadableSpanMethods();

    private static volatile OpenTelemetry cachedOtel;
    private static volatile Tracer cachedTracer;

    private OpenAiTelemetry() {}

    /** Reads the current global instance and caches its tracer. Tests may replace the instance. */
    static Tracer tracer() {
        OpenTelemetry otel = GlobalOpenTelemetry.get();
        Tracer t = cachedTracer;
        if (t == null || otel != cachedOtel) {
            String version = null;
            try {
                version = System.getProperty(PROP_EXTENSION);
            } catch (SecurityException ignored) {
                // Fall back to the built-in version if the property is unavailable.
            }
            t = otel.getTracer(SCOPE, version != null ? version : FALLBACK_VERSION);
            cachedTracer = t;
            cachedOtel = otel;
        }
        return t;
    }

    /** Reads the content capture switch from the extension's system property, based on the Agent process's FLASHCAT_CAPTURE_CONTENT. */
    static boolean captureContent() {
        try {
            return "span".equals(System.getProperty(PROP_CAPTURE_CONTENT));
        } catch (SecurityException e) {
            return false;
        }
    }

    /**
     * Checks whether the current span is a CLIENT span with an inference operation such as chat
     * in {@code gen_ai.operation.name}, from other instrumentation or application code.
     *
     * <p>The OTel API exposes neither span kind nor attributes. Reflect on the Agent SDK's
     * {@code ReadableSpan}, which the application class loader cannot access directly. An SDK on
     * the application's classpath would have a different copy of the interface, making
     * {@code instanceof} unusable. If reflection fails, treat the span as non-inference.
     */
    static boolean isInferenceSpan(Span span) {
        try {
            if (span == null || !span.getSpanContext().isValid()) {
                return false;
            }
            Method[] m = READABLE_SPAN.get(span.getClass());
            if (m == null) {
                return false;
            }
            if (m[0].invoke(span) != SpanKind.CLIENT) {
                return false;
            }
            Object op = m[1].invoke(span, OPERATION);
            return op instanceof String && INFERENCE_OPS.contains(op);
        } catch (Throwable e) {
            return false;
        }
    }

    /**
     * Without muzzle checks, incompatible openai-java versions can fail during span creation
     * with an error such as LinkageError. Application calls continue. Record only the first
     * failure, log one warning, and report {@code not_patched} in diagnostics (contract §8).
     */
    static void recordFailure(Throwable t) {
        try {
            if (System.getProperty(PROP_ERROR) != null) {
                return;
            }
            String msg = t.getClass().getName() + ": " + t.getMessage();
            System.setProperty(PROP_ERROR, msg);
            java.util.logging.Logger.getLogger("cloud.flashcat.genai").warning("flashcat-genai-otel: openai-java 插桩出错，模型调用可能没有 span：" + msg);
        } catch (Throwable ignored) {
            // Keep the application call running even if failure reporting fails.
        }
    }

    /** Sets ERROR status and error.type (the HTTP status code when available, per contract §4) and records the exception event. */
    static void recordError(Span span, Throwable error) {
        Throwable t = unwrap(error);
        span.setStatus(StatusCode.ERROR, t.getMessage() != null ? t.getMessage() : t.getClass().getName());
        span.setAttribute(ERROR_TYPE, errorType(t));
        span.recordException(t);
    }

    /** Contract §4 uses the HTTP status code as a string (such as 429) when available, or the exception class name otherwise. */
    static String errorType(Throwable error) {
        Throwable t = unwrap(error);
        if (t instanceof OpenAIServiceException) {
            return String.valueOf(((OpenAIServiceException) t).statusCode());
        }
        return t.getClass().getName();
    }

    /** Unwraps asynchronous exceptions from CompletionException or ExecutionException. */
    static Throwable unwrap(Throwable t) {
        Throwable cur = t;
        while ((cur instanceof CompletionException || cur instanceof ExecutionException) && cur.getCause() != null) {
            cur = cur.getCause();
        }
        return cur;
    }

    /**
     * Reads the active variant of the ResponsesModel union (string, ChatModel, or Responses-specific
     * model). {@code asString()} throws for either model variant.
     */
    static String model(ResponsesModel m) {
        if (m == null) {
            return null;
        }
        Optional<String> s = m.string();
        if (s.isPresent()) {
            return s.get();
        }
        Optional<ChatModel> chat = m.chat();
        if (chat.isPresent()) {
            return chat.get().asString();
        }
        Optional<ResponsesModel.ResponsesOnlyModel> only = m.only();
        if (only.isPresent()) {
            return only.get().asString();
        }
        return null;
    }

    /** Maps the client baseUrl to server.address and server.port, inferring the port from the scheme if omitted. */
    static void setServer(io.opentelemetry.api.trace.SpanBuilder b, String baseUrl) {
        if (baseUrl == null) {
            return;
        }
        try {
            URI u = new URI(baseUrl);
            String host = u.getHost();
            if (host == null || host.isEmpty()) {
                return;
            }
            int port = u.getPort();
            if (port <= 0) {
                if ("https".equalsIgnoreCase(u.getScheme())) {
                    port = 443;
                } else if ("http".equalsIgnoreCase(u.getScheme())) {
                    port = 80;
                }
            }
            b.setAttribute(SERVER_ADDRESS, host);
            if (port > 0) {
                b.setAttribute(SERVER_PORT, (long) port);
            }
        } catch (Exception ignored) {
            // Omit these attributes if URL parsing fails.
        }
    }

    /** Caches getKind and getAttribute from the SDK's {@code ReadableSpan} interface by concrete span class. */
    static final class ReadableSpanMethods extends ClassValue<Method[]> {
        private static final String READABLE_SPAN = "io.opentelemetry.sdk.trace.ReadableSpan";

        @Override
        protected Method[] computeValue(Class<?> type) {
            Class<?> iface = find(type);
            if (iface == null) {
                return null;
            }
            try {
                // Use methods from the public interface. The SDK's non-public implementation class would deny reflective access.
                return new Method[] {iface.getMethod("getKind"), iface.getMethod("getAttribute", AttributeKey.class)};
            } catch (Throwable e) {
                return null;
            }
        }

        private static Class<?> find(Class<?> type) {
            for (Class<?> c = type; c != null; c = c.getSuperclass()) {
                for (Class<?> i : c.getInterfaces()) {
                    Class<?> hit = findInterface(i);
                    if (hit != null) {
                        return hit;
                    }
                }
            }
            return null;
        }

        private static Class<?> findInterface(Class<?> i) {
            if (READABLE_SPAN.equals(i.getName())) {
                return i;
            }
            for (Class<?> p : i.getInterfaces()) {
                Class<?> hit = findInterface(p);
                if (hit != null) {
                    return hit;
                }
            }
            return null;
        }
    }
}
