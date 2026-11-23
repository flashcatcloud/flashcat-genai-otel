package cloud.flashcat.genai.agent.openai;

import static io.opentelemetry.javaagent.extension.matcher.AgentElementMatchers.hasClassesNamed;

import io.opentelemetry.javaagent.extension.instrumentation.InstrumentationModule;
import io.opentelemetry.javaagent.extension.instrumentation.TypeInstrumentation;
import java.util.Arrays;
import java.util.Collections;
import java.util.List;
import net.bytebuddy.matcher.ElementMatcher;

/**
 * Additional openai-java instrumentation provided by the Flashcat extension for the OpenTelemetry Java Agent.
 *
 * <p>The OpenTelemetry Java Agent 2.33.1 openai-java-1.1 instrumentation only proxies clients
 * returned by {@code OpenAIOkHttpClient.Builder.build()}. The proxy intercepts Chat Completions
 * and Embeddings. {@code client.responses()} goes straight to the implementation without a model
 * span. The extension hooks implementation classes, covering clients created through the builder,
 * {@code fromEnv()}, or direct construction.
 *
 * <ul>
 *   <li>{@code ResponseServiceImpl} and {@code ResponseServiceAsyncImpl} start a chat span on
 *       {@code create} and {@code createStreaming} (see {@link ResponsesCall}).</li>
 *   <li>{@code ChatCompletionServiceImpl} (including {@code WithRawResponseImpl}) and
 *       {@code ChatCompletionServiceAsyncImpl} reuse the upstream span for proxied calls, adding
 *       time to first token (TTFT) for streaming calls (see {@link ChatFirstChunk}). Calls that
 *       bypass the upstream proxy, such as Spring AI, direct client construction, or
 *       {@code withRawResponse()}, get a new chat span (see {@link ChatCall}).</li>
 * </ul>
 *
 * <p>Both instrumentations check for a current inference span from upstream instrumentation or
 * application code to avoid duplicates. Set {@code -Dotel.instrumentation.flashcat-openai-java.enabled=false}
 * to disable this instrumentation.
 *
 * <p>Without the muzzle plugin, all helpers injected into the application's class loader are listed
 * in {@link #getAdditionalHelperClassNames()}. Unit tests verify that list.
 */
public final class FlashcatOpenAiInstrumentationModule extends InstrumentationModule {
    static final String NAME = "flashcat-openai-java";
    /**
     * System property set when the Agent loads the instrumentation. InstrumentationDetector in
     * flashcat-genai-otel uses it to report Responses and Chat Completions fallback instrumentation status.
     */
    static final String PROP_INSTALLED = "cloud.flashcat.genai.agent.instrumentation.openai-java";

    private static final String PKG = "cloud.flashcat.genai.agent.openai.";

    /** Inject StreamObserver before its implementations. All other superclasses and interfaces come from openai-java or the JDK. */
    static final List<String> HELPERS = Collections.unmodifiableList(Arrays.asList(
            PKG + "StreamObserver",
            PKG + "ObservedStream",
            PKG + "ObservedStream$ObservedSpliterator",
            PKG + "ObservedAsyncStream",
            PKG + "ObservedAsyncStream$ObservedHandler",
            PKG + "OpenAiTelemetry",
            PKG + "OpenAiTelemetry$ReadableSpanMethods",
            PKG + "ResponsesContent",
            PKG + "ResponsesCall",
            PKG + "ChatContent",
            PKG + "ChatContent$ChoiceAcc",
            PKG + "ChatContent$ToolAcc",
            PKG + "ChatFirstChunk",
            PKG + "ChatCall",
            PKG + "ChatCall$ParsedResponse"));

    public FlashcatOpenAiInstrumentationModule() {
        super(NAME);
    }

    @Override
    public ElementMatcher.Junction<ClassLoader> classLoaderMatcher() {
        return hasClassesNamed("com.openai.core.ClientOptions", "com.openai.core.http.StreamResponse");
    }

    @Override
    public List<TypeInstrumentation> typeInstrumentations() {
        // The Agent calls this only when instrumentation is enabled.
        try {
            System.setProperty(PROP_INSTALLED, "installed");
        } catch (SecurityException ignored) {
            // Diagnostics report the state as unknown.
        }
        return Arrays.<TypeInstrumentation>asList(
                new ResponseServiceInstrumentation(ResponseServiceInstrumentation.BLOCKING, false),
                new ResponseServiceInstrumentation(ResponseServiceInstrumentation.ASYNC, true),
                new ChatCompletionServiceInstrumentation(ChatCompletionServiceInstrumentation.BLOCKING),
                new ChatCompletionServiceInstrumentation(ChatCompletionServiceInstrumentation.BLOCKING_RAW),
                new ChatCompletionServiceInstrumentation(ChatCompletionServiceInstrumentation.ASYNC));
    }

    @Override
    public List<String> getAdditionalHelperClassNames() {
        return HELPERS;
    }

    @Override
    public boolean isHelperClass(String className) {
        return HELPERS.contains(className);
    }
}
