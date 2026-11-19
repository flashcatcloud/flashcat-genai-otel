package cloud.flashcat.otel;

import java.util.Map;

/**
 * Applies the content capture switch to each instrumentation source (contract §7).
 *
 * <ul>
 *   <li>For upstream OTel instrumentation such as openai-java, the SPI sets the boolean property
 *       {@code otel.instrumentation.genai.capture-message-content}.
 *       Agent instrumentation for openai-java records content in log events. Flashcat currently reads content only from span attributes.
 *   <li>The built-in Anthropic Java SDK instrumentation reads {@code anthropic.openTelemetryTracesContentMode} from system properties,
 *       then falls back to {@code ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE}. If neither is set, the Flashcat SDK sets the system property.
 *       Only clients created with {@code fromEnv()} read these settings.
 *   <li>Spring AI uses the application's {@code spring.ai.*.observations.*} configuration. The SDK can only report it in diagnostics.
 * </ul>
 *
 * <p>The semantic API uses {@link #enabled()} to decide whether to write content attributes. It checks the configuration in this
 * class loader first, then the agent extension's system property, and finally resolves the switch from the environment.
 */
final class ContentSwitch {
    static final String ANTHROPIC_PROP = "anthropic.openTelemetryTracesContentMode";
    static final String ANTHROPIC_ENV = "ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE";
    static final String OTEL_PROP = "otel.instrumentation.genai.capture-message-content";

    /** Switch resolved by init or the SPI in this class loader. Null means it has yet to be resolved. */
    private static volatile Boolean resolved;
    private static volatile Boolean lazyFromEnv;

    private ContentSwitch() {}

    static void setResolved(Boolean on) {
        resolved = on;
    }

    static void resetForTest() {
        resolved = null;
        lazyFromEnv = null;
    }

    /** Returns whether the semantic API should write content attributes. */
    static boolean enabled() {
        Boolean r = resolved;
        if (r != null) {
            return r;
        }
        try {
            String agent = System.getProperty(AgentDetector.PROP_CAPTURE_CONTENT);
            if (agent != null) {
                return "span".equals(agent);
            }
        } catch (SecurityException ignored) {
            // Fall back to the environment if the property cannot be read.
        }
        Boolean l = lazyFromEnv;
        if (l == null) {
            boolean on;
            try {
                on = Settings.resolve(null, new java.util.function.Function<String, String>() {
                    @Override
                    public String apply(String k) {
                        return Env.get(k);
                    }
                }, new java.util.function.Function<String, String>() {
                    @Override
                    public String apply(String k) {
                        return Env.otel(k);
                    }
                }).captureContent;
            } catch (Throwable e) {
                on = false;
            }
            l = on;
            lazyFromEnv = l;
        }
        return l;
    }

    /**
     * Applies the switch to the Anthropic SDK if the user has left it unset and populates the diagnostic content field.
     *
     * @param on whether to capture content
     * @param content diagnostic content field
     */
    static void apply(boolean on, Map<String, Object> content) {
        content.put("upstream", on ? "SPAN_ONLY" : "NO_CONTENT");
        content.put("otel_java_property", OTEL_PROP + "=" + on);
        String userProp = null;
        String userEnv = null;
        try {
            userProp = Env.blankToNull(System.getProperty(ANTHROPIC_PROP));
        } catch (SecurityException ignored) {
            // Ignore failures when setting the property.
        }
        userEnv = Env.get(ANTHROPIC_ENV);
        if (userProp == null && userEnv == null) {
            String mode = on ? "content" : "metadata_only";
            try {
                System.setProperty(ANTHROPIC_PROP, mode);
                content.put("anthropic", mode);
            } catch (SecurityException e) {
                content.put("anthropic", "unchanged:security_manager");
            }
        } else {
            String v = userProp != null ? userProp : userEnv;
            content.put("anthropic", "user:" + v);
        }
        content.put("spring_ai", "app_config:spring.ai.chat.observations.log-prompt/log-completion（写应用日志），spring.ai.tools.observations.include-content（写 span）");
    }
}
