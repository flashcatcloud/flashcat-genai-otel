package cloud.flashcat.genai;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.common.AttributesBuilder;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizer;
import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizerProvider;
import io.opentelemetry.sdk.autoconfigure.spi.ConfigProperties;
import io.opentelemetry.sdk.autoconfigure.spi.internal.AutoConfigureListener;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.metrics.export.MetricExporter;
import io.opentelemetry.sdk.resources.Resource;
import io.opentelemetry.sdk.trace.SdkTracerProviderBuilder;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import java.util.Collection;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import java.util.function.BiFunction;
import java.util.function.Function;
import java.util.function.LongFunction;
import java.util.function.Supplier;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Flashcat's OTel autoconfigure SPI implementation (contract §2), shared by three initialization paths.
 *
 * <ul>
 *   <li>The OpenTelemetry Java agent extension ({@code -Dotel.javaagent.extensions=flashcat-genai-otel-agent-extension.jar}).
 *   <li>The OpenTelemetry Spring Boot Starter (ServiceLoader discovers the SPI when {@code flashcat-genai-otel} is a dependency).
 *   <li>{@link FlashcatGenAI#init()} when it creates the SDK through {@code AutoConfiguredOpenTelemetrySdk}.
 * </ul>
 *
 * <p>The customizers configure properties, resources, and the export pipeline.
 *
 * <ol>
 *   <li>{@code addPropertiesCustomizer} maps FLASHCAT_* to per-signal OTel settings (trace and metrics endpoints,
 *       HTTP/protobuf, gzip, Basic auth, and a 10-second timeout) and sets {@code otel.instrumentation.genai.capture-message-content}.
 *       A properties customizer gives FLASHCAT_* precedence over OTEL_*, as required by contract §1.
 *   <li>{@code addResourceCustomizer} sets {@code deployment.environment.name} and {@code telemetry.distro.*} and validates the effective service.name.
 *   <li>{@code addTracerProviderCustomizer} adds the session processor.
 *   <li>{@code addSpanExporterCustomizer} wraps only Flashcat OTLP/HTTP exporters to set the AI root marker, mask content, count export results, and enforce the shutdown deadline ({@link ExitDeadline}).
 *   <li>{@code addMetricExporterCustomizer} enforces the shutdown deadline only for Flashcat OTLP/HTTP metrics exporters.
 *   <li>Invalid configuration leaves export disabled, records the problem in diagnostics, and writes a JUL log line.
 * </ol>
 *
 * <p>This class also implements OTel's internal {@code AutoConfigureListener}. The Spring Boot Starter leaves
 * {@code GlobalOpenTelemetry} unset, so the listener registers the Starter's SDK globally for the semantic API and built-in Anthropic SDK instrumentation.
 *
 * <p>Application code can use the SDK without referencing this class directly.
 */
public final class FlashcatAutoConfigurationCustomizer implements AutoConfigurationCustomizerProvider, AutoConfigureListener {
    private static final Logger logger = Logger.getLogger("cloud.flashcat.genai");
    private static final AttributeKey<String> SERVICE_NAME = AttributeKey.stringKey("service.name");
    private static final String STARTER_CONFIG_CLASS = "io.opentelemetry.instrumentation.spring.autoconfigure.OpenTelemetryAutoConfiguration";

    /** True while FlashcatGenAI.init creates the SDK with its own customizer. The ServiceLoader instance skips customization to avoid duplication. */
    static final ThreadLocal<Boolean> CODE_INIT = new ThreadLocal<Boolean>();

    private final State codeState;
    private volatile State state;

    /** Constructor for ServiceLoader (Java agent extension and Spring Boot Starter). */
    public FlashcatAutoConfigurationCustomizer() {
        this.codeState = null;
    }

    /** Constructor for FlashcatGenAI.init, which has already resolved the explicit arguments. */
    FlashcatAutoConfigurationCustomizer(State codeState) {
        this.codeState = codeState;
        this.state = codeState;
    }

    @Override
    public void customize(AutoConfigurationCustomizer c) {
        try {
            if (codeState == null && Boolean.TRUE.equals(CODE_INIT.get())) {
                return;
            }
            if (codeState == null && Settings.truthy(Env.get("FLASHCAT_DISABLED"))) {
                State st = new State(AgentDetector.runningInsideAgent() ? State.Mode.AGENT : State.Mode.SPI, null);
                st.note("FLASHCAT_DISABLED / OTEL_SDK_DISABLED 已开启，未做任何事");
                publish(st);
                return;
            }
            c.addPropertiesCustomizer(new Function<ConfigProperties, Map<String, String>>() {
                @Override
                public Map<String, String> apply(ConfigProperties cfg) {
                    return properties(cfg);
                }
            });
            c.addResourceCustomizer(new BiFunction<Resource, ConfigProperties, Resource>() {
                @Override
                public Resource apply(Resource r, ConfigProperties cfg) {
                    return resource(r);
                }
            });
            c.addTracerProviderCustomizer(new BiFunction<SdkTracerProviderBuilder, ConfigProperties, SdkTracerProviderBuilder>() {
                @Override
                public SdkTracerProviderBuilder apply(SdkTracerProviderBuilder b, ConfigProperties cfg) {
                    State st = state;
                    return st != null ? b.addSpanProcessor(st.sessionProcessor) : b;
                }
            });
            c.addSpanExporterCustomizer(new BiFunction<SpanExporter, ConfigProperties, SpanExporter>() {
                @Override
                public SpanExporter apply(SpanExporter e, ConfigProperties cfg) {
                    return exporter(e);
                }
            });
            c.addMetricExporterCustomizer(new BiFunction<MetricExporter, ConfigProperties, MetricExporter>() {
                @Override
                public MetricExporter apply(MetricExporter e, ConfigProperties cfg) {
                    return metricExporter(e);
                }
            });
        } catch (Throwable t) {
            logger.log(Level.WARNING, "flashcat-genai-otel: SPI 初始化失败，未接入 Flashcat：" + t, t);
        }
    }

    // ------------------------------------------------------------------ properties

    Map<String, String> properties(ConfigProperties cfg) {
        Map<String, String> m = new HashMap<String, String>();
        try {
            final ConfigProperties config = cfg;
            Function<String, String> otel = new Function<String, String>() {
                @Override
                public String apply(String k) {
                    try {
                        return Env.blankToNull(config.getString(k));
                    } catch (Throwable e) {
                        return null;
                    }
                }
            };
            State st = codeState;
            if (st == null) {
                Settings s = Settings.resolve(null, new Function<String, String>() {
                    @Override
                    public String apply(String k) {
                        return Env.get(k);
                    }
                }, otel);
                st = new State(AgentDetector.runningInsideAgent() ? State.Mode.AGENT : State.Mode.SPI, s);
                for (String p : s.problems) {
                    st.problem(p);
                }
                ContentSwitch.apply(s.captureContent, st.content);
                publish(st);
            }
            state = st;
            Settings s = st.settings;
            m.put(ContentSwitch.OTEL_PROP, String.valueOf(s.captureContent));
            if (s.serviceName != null) {
                m.put("otel.service.name", s.serviceName);
            }
            if (st.mode == State.Mode.AGENT) {
                System.setProperty(AgentDetector.PROP_EXTENSION, Version.VERSION);
                System.setProperty(AgentDetector.PROP_CAPTURE_CONTENT, s.captureContent ? "span" : "false");
                // The agent loads the extension's openai-java instrumentation after SDK creation. The startup summary runs earlier, so derive the initial status from configuration.
                System.setProperty(InstrumentationDetector.PROP_OPENAI_INSTALLED, openAiInstrumentationEnabled(otel) ? "installed" : "disabled");
                installAgentBridge(st);
            }

            String mode = decideExporterMode(st, otel);
            st.exporterMode = mode;
            if ("add".equals(mode)) {
                putExportProperties(st, otel, m);
            } else if (st.mode == State.Mode.CODE || !userConfiguredExport(otel)) {
                // The SDK owns the programmatically created provider. With the agent or Starter, disable the default localhost:4318 export if the user has not configured export.
                m.put("otel.traces.exporter", "none");
                m.put("otel.metrics.exporter", "none");
                m.put("otel.logs.exporter", "none");
                st.metricsMode = s.metrics ? "none" : "disabled";
            } else {
                st.note("Flashcat 导出未启用；保留用户自己的 OTEL_* 导出配置");
                st.metricsMode = "none";
            }
            st.enabled = "add".equals(mode) || "reuse".equals(mode);
        } catch (Throwable t) {
            logger.log(Level.WARNING, "flashcat-genai-otel: 解析配置失败，未接入 Flashcat：" + t, t);
        }
        return m;
    }

    private static String decideExporterMode(State st, Function<String, String> otel) {
        Settings s = st.settings;
        String requested = s.exporter == null ? "auto" : s.exporter;
        String mode;
        if ("reuse".equals(requested) || "none".equals(requested)) {
            mode = "none";
            if ("reuse".equals(requested)) {
                st.note("未新增 exporter，数据经进程里已有的 exporter 发出（GenAI 数据由平台侧识别）");
            }
        } else {
            mode = "add";
        }
        // Agent and programmatic initialization can resolve the service name here. The Starter may supply spring.application.name, so defer validation to resource customization.
        if ("add".equals(mode) && st.mode != State.Mode.SPI && !s.serviceNameValid()) {
            st.problem("缺少 service_name（参数或 OTEL_SERVICE_NAME），且不能是 unknown_service*；未启用导出");
            mode = "none";
        }
        if ("add".equals(mode) && s.endpoint == null) {
            st.note("缺少 endpoint（FLASHCAT_ENDPOINT，完整 .../v1/traces URL），未启用导出");
            mode = "none";
        } else if ("add".equals(mode) && s.authorization() == null) {
            st.note("缺少 api_key（FLASHCAT_API_KEY=keyID:secret），未启用导出");
            mode = "none";
        }
        return mode;
    }

    private static void putExportProperties(State st, Function<String, String> otel, Map<String, String> m) {
        Settings s = st.settings;
        String timeout = String.valueOf(s.exportTimeoutMillis);
        String headers = headersWithAuth(otel, s.authorization());

        m.put("otel.traces.exporter", withOtlp(otel.apply("otel.traces.exporter")));
        m.put("otel.exporter.otlp.traces.endpoint", s.endpoint);
        m.put("otel.exporter.otlp.traces.protocol", "http/protobuf");
        m.put("otel.exporter.otlp.traces.compression", "gzip");
        m.put("otel.exporter.otlp.traces.headers", headers);
        m.put("otel.exporter.otlp.traces.timeout", timeout);

        String userMetrics = otel.apply("otel.metrics.exporter");
        if (!s.metrics) {
            m.put("otel.metrics.exporter", "none");
            st.metricsMode = "disabled";
        } else if (userMetrics != null && userMetrics.trim().equalsIgnoreCase("none")) {
            // Honor an explicit -Dotel.metrics.exporter=none or OTEL_METRICS_EXPORTER=none.
            st.metricsMode = "disabled";
            st.note("otel.metrics.exporter=none 是显式配置，gen_ai.client.* 指标不导出");
        } else if (s.metricsEndpoint == null) {
            m.put("otel.metrics.exporter", "none");
            st.metricsMode = "none";
            st.note("没有 metrics endpoint，gen_ai.client.* 指标不导出");
        } else {
            m.put("otel.metrics.exporter", withOtlp(userMetrics));
            m.put("otel.exporter.otlp.metrics.endpoint", s.metricsEndpoint);
            m.put("otel.exporter.otlp.metrics.protocol", "http/protobuf");
            m.put("otel.exporter.otlp.metrics.compression", "gzip");
            m.put("otel.exporter.otlp.metrics.headers", headers);
            m.put("otel.exporter.otlp.metrics.timeout", timeout);
            if (otel.apply("otel.metric.export.interval") == null) {
                m.put("otel.metric.export.interval", "60000");
            }
            st.metricsMode = "add";
        }
        // Content events and logs are currently outside Flashcat's scope. Disable log export unless explicitly configured to avoid sending logs to the default endpoint.
        if (otel.apply("otel.logs.exporter") == null) {
            m.put("otel.logs.exporter", "none");
        }
    }

    /** Preserves existing OTLP headers and replaces only Authorization. Percent-encode the value because OTel Java decodes it on read. */
    private static String headersWithAuth(Function<String, String> otel, String auth) {
        String raw = Settings.first(otel.apply("otel.exporter.otlp.traces.headers"), otel.apply("otel.exporter.otlp.headers"));
        Map<String, String> h = new LinkedHashMap<String, String>();
        for (Map.Entry<String, String> e : Settings.parseKvList(raw).entrySet()) {
            if (!e.getKey().equalsIgnoreCase("authorization")) {
                h.put(e.getKey(), e.getValue());
            }
        }
        h.put("Authorization", auth);
        StringBuilder sb = new StringBuilder();
        for (Map.Entry<String, String> e : h.entrySet()) {
            if (sb.length() > 0) {
                sb.append(',');
            }
            sb.append(e.getKey()).append('=').append(Settings.percentEncode(e.getValue()));
        }
        return sb.toString();
    }

    private static String withOtlp(String current) {
        if (current == null || current.trim().isEmpty() || current.trim().equalsIgnoreCase("none")) {
            return "otlp";
        }
        for (String part : current.split(",")) {
            if (part.trim().equalsIgnoreCase("otlp")) {
                return current;
            }
        }
        return current + ",otlp";
    }

    /** Follows the agent's rules for enabling instrumentation. Check otel.instrumentation.flashcat-openai-java.enabled, then common.default-enabled (true by default). */
    static boolean openAiInstrumentationEnabled(Function<String, String> otel) {
        String v = otel.apply("otel.instrumentation." + InstrumentationDetector.OPENAI_INSTRUMENTATION_NAME + ".enabled");
        if (v == null) {
            v = otel.apply("otel.instrumentation.common.default-enabled");
        }
        return v == null || !v.trim().equalsIgnoreCase("false");
    }

    private static boolean userConfiguredExport(Function<String, String> otel) {
        return otel.apply("otel.traces.exporter") != null
                || otel.apply("otel.exporter.otlp.endpoint") != null
                || otel.apply("otel.exporter.otlp.traces.endpoint") != null;
    }

    // ------------------------------------------------------------------ resource

    Resource resource(Resource r) {
        State st = state;
        if (st == null) {
            return r;
        }
        try {
            Settings s = st.settings;
            AttributesBuilder b = Attributes.builder();
            b.put("deployment.environment.name", s.environment);
            b.put("telemetry.distro.name", Version.SDK_NAME);
            b.put("telemetry.distro.version", Version.VERSION);
            for (Map.Entry<String, String> e : s.resourceAttributes.entrySet()) {
                b.put(e.getKey(), e.getValue());
            }
            Resource merged = r.merge(Resource.create(b.build()));
            String svc = merged.getAttribute(SERVICE_NAME);
            st.serviceName = svc;
            if (!Settings.isValidServiceName(svc) && "add".equals(st.exporterMode)) {
                st.problem("缺少 service_name（参数或 OTEL_SERVICE_NAME），且不能是 unknown_service*；未启用导出");
                st.dropExport = true;
                st.exporterMode = "none";
                st.enabled = false;
            }
            return merged;
        } catch (Throwable t) {
            logger.log(Level.FINE, "flashcat-genai-otel: 资源属性处理失败", t);
            return r;
        }
    }

    // ------------------------------------------------------------------ exporter

    SpanExporter exporter(SpanExporter e) {
        State st = state;
        if (st == null) {
            return e;
        }
        try {
            if (!"io.opentelemetry.exporter.otlp.http.trace.OtlpHttpSpanExporter".equals(e.getClass().getName())) {
                return e;
            }
            if (st.dropExport) {
                return new DroppingSpanExporter(e);
            }
            if (!"add".equals(st.exporterMode)) {
                return e;
            }
            Masker masker = new Masker(st.settings.masking, st.settings.captureContent);
            st.masker = masker;
            armExitHook(st);
            return new MaskingSpanExporter(
                    new CountingSpanExporter(e, st.stats, st.exit, st.settings.exportTimeoutMillis), masker, st.sessionProcessor.ancestry);
        } catch (Throwable t) {
            logger.log(Level.FINE, "flashcat-genai-otel: 包装 exporter 失败", t);
            return e;
        }
    }

    /** Wraps Flashcat OTLP/HTTP metrics exporters with a shutdown deadline. Other exporters pass through unchanged. */
    MetricExporter metricExporter(MetricExporter e) {
        State st = state;
        if (st == null) {
            return e;
        }
        try {
            if (!"io.opentelemetry.exporter.otlp.http.metrics.OtlpHttpMetricExporter".equals(e.getClass().getName())
                    || !"add".equals(st.exporterMode) || !"add".equals(st.metricsMode)) {
                return e;
            }
            armExitHook(st);
            return new ExitDeadlineMetricExporter(e, st.exit, st.settings.exportTimeoutMillis);
        } catch (Throwable t) {
            logger.log(Level.FINE, "flashcat-genai-otel: 包装指标 exporter 失败", t);
            return e;
        }
    }

    /**
     * The agent's own hook closes the SDK. Register a hook that records when shutdown begins so Flashcat exporters share a deadline from that point.
     * Skip this hook with the Starter. Spring closes the SDK when the application context closes, which may be after JVM shutdown begins.
     * The deadline then starts in the session processor's shutdown. The programmatic path calls shutdown(timeout) from FlashcatGenAI's own hook.
     */
    private static void armExitHook(State st) {
        if (st.mode == State.Mode.AGENT) {
            st.exit.registerShutdownHook();
        }
    }

    // ------------------------------------------------------------------ listener

    @Override
    public void afterAutoConfigure(OpenTelemetrySdk sdk) {
        try {
            State st = state;
            if (st == null || codeState != null || Boolean.TRUE.equals(CODE_INIT.get())) {
                return;
            }
            st.sdk = sdk;
            st.ownsSdk = false;
            if (st.mode == State.Mode.SPI && calledFromSpringStarter()) {
                if (!GlobalOpenTelemetry.isSet()) {
                    try {
                        GlobalOpenTelemetry.set(sdk);
                        st.note("OTel Spring Boot Starter 不设置 GlobalOpenTelemetry；flashcat-genai-otel 已把 Starter 创建的 OpenTelemetrySdk 注册为全局，供语义 API 与 Anthropic SDK 自带埋点使用");
                    } catch (IllegalStateException ise) {
                        st.note("GlobalOpenTelemetry 已被设置，语义 API 使用已有实例而不是 Starter 创建的 SDK");
                    }
                } else {
                    st.note("GlobalOpenTelemetry 已被设置，语义 API 使用已有实例而不是 Starter 创建的 SDK");
                }
            }
            FlashcatGenAI.emitSummary(new Diagnostics(st.diagnostics(null)), st.settings != null && st.settings.debug);
        } catch (Throwable t) {
            logger.log(Level.FINE, "flashcat-genai-otel: afterAutoConfigure 失败", t);
        }
    }

    private static boolean calledFromSpringStarter() {
        for (StackTraceElement el : new Throwable().getStackTrace()) {
            if (el.getClassName().startsWith(STARTER_CONFIG_CLASS)) {
                return true;
            }
        }
        return false;
    }

    // ------------------------------------------------------------------ helpers

    private static void publish(State st) {
        State cur = State.get();
        if (cur != null && cur.mode == State.Mode.CODE) {
            return; // Programmatic initialization takes precedence.
        }
        State.set(st);
        if (st.settings != null) {
            ContentSwitch.setResolved(st.settings.captureContent);
        }
    }

    private static void installAgentBridge(final State st) {
        final AgentBridge bridge = new AgentBridge(new Supplier<Map<String, Object>>() {
            @Override
            public Map<String, Object> get() {
                ClassLoader tccl = Thread.currentThread().getContextClassLoader();
                Map<String, Object> d = st.diagnostics(tccl);
                @SuppressWarnings("unchecked")
                java.util.List<String> notes = (java.util.List<String>) d.get("notes");
                notes.add("诊断来自 OpenTelemetry Java Agent 里的 Flashcat 扩展");
                return d;
            }
        }, new LongFunction<Boolean>() {
            @Override
            public Boolean apply(long timeoutMillis) {
                Object sdk = st.sdk;
                if (!(sdk instanceof OpenTelemetrySdk)) {
                    return Boolean.FALSE;
                }
                try (ExitDeadline.FlushScope scope = st.exit.beginFlush(timeoutMillis)) {
                    OpenTelemetrySdk otel = (OpenTelemetrySdk) sdk;
                    CompletableResultCode traces = otel.getSdkTracerProvider().forceFlush();
                    CompletableResultCode metrics = otel.getSdkMeterProvider().forceFlush();
                    CompletableResultCode all = CompletableResultCode.ofAll(java.util.Arrays.asList(traces, metrics));
                    scope.finishWhen(all);
                    return all.join(scope.remainingMillis(), TimeUnit.MILLISECONDS).isSuccess();
                }
            }
        });
        st.sessionProcessor.setFirstSpanHook(new Runnable() {
            @Override
            public void run() {
                AgentBridge.registerAsync(bridge);
                // Agent startup sees only the system classpath. Repeat detection with the application thread's context class loader when the first span starts
                // to see libraries such as those under BOOT-INF/lib in Spring Boot executable JARs. Log detected problems at WARN (contract §8).
                try {
                    ClassLoader tccl = Thread.currentThread().getContextClassLoader();
                    InstrumentationDetector.warnOnce(InstrumentationDetector.inspect(true, tccl).problems);
                } catch (Throwable ignored) {
                    // Diagnostics failures must leave application code unaffected.
                }
            }
        });
    }

    /** Replaces the OTLP exporter with a no-op when the resource's service.name is invalid, as required by the contract. */
    static final class DroppingSpanExporter implements SpanExporter {
        private final SpanExporter inner;

        DroppingSpanExporter(SpanExporter inner) {
            this.inner = inner;
        }

        @Override
        public CompletableResultCode export(Collection<SpanData> spans) {
            return CompletableResultCode.ofSuccess();
        }

        @Override
        public CompletableResultCode flush() {
            return CompletableResultCode.ofSuccess();
        }

        @Override
        public CompletableResultCode shutdown() {
            return inner.shutdown();
        }
    }
}
