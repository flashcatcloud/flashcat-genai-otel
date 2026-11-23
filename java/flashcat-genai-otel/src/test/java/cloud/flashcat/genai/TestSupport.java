package cloud.flashcat.genai;

import io.opentelemetry.api.GlobalOpenTelemetry;
import java.util.HashMap;
import java.util.Map;

/** Resets SDK and OTel global state before and after each test. A stub map stands in for the process environment, which Java cannot modify at runtime. */
final class TestSupport {
    private TestSupport() {}

    static void reset(Map<String, String> env) {
        FlashcatGenAI.resetForTest();
        GlobalOpenTelemetry.resetForTest();
        Env.setOverrideForTest(env == null ? new HashMap<String, String>() : env);
        System.clearProperty(ContentSwitch.ANTHROPIC_PROP);
        System.clearProperty(AgentDetector.PROP_EXTENSION);
        System.clearProperty(AgentDetector.PROP_CAPTURE_CONTENT);
        System.clearProperty(InstrumentationDetector.PROP_OPENAI_INSTALLED);
        System.clearProperty(InstrumentationDetector.PROP_OPENAI_ERROR);
        for (String k : new String[] {"otel.service.name", "otel.exporter.otlp.endpoint", "otel.exporter.otlp.traces.endpoint",
                "otel.instrumentation.genai.capture-message-content", "otel.resource.attributes", "otel.sdk.disabled"}) {
            System.clearProperty(k);
        }
    }

    static Map<String, String> env(String... kv) {
        Map<String, String> m = new HashMap<String, String>();
        for (int i = 0; i + 1 < kv.length; i += 2) {
            m.put(kv[i], kv[i + 1]);
        }
        return m;
    }
}
