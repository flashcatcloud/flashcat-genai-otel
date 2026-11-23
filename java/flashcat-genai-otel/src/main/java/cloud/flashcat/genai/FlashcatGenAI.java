package cloud.flashcat.genai;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;
import io.opentelemetry.sdk.common.CompletableResultCode;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import java.util.function.Function;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Initialization, diagnostics, flush, and shutdown for the Flashcat GenAI OTel SDK (contract §2, §8, and §9).
 *
 * <p>Use the OpenTelemetry Java agent with the Flashcat extension for automatic setup. If JVM arguments are unavailable, call init at the start of {@code main}.
 *
 * <pre>{@code
 * Diagnostics d = FlashcatGenAI.init();   // Read configuration from the FLASHCAT_* and OTEL_* environment variables.
 * // Or pass explicit options.
 * FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("kefu-agent").captureContent("false").build());
 * }</pre>
 *
 * <p>Initialization failures are recorded in diagnostics and trigger a fallback. Only invalid explicit arguments throw {@link ConfigException}.
 * Subsequent calls return diagnostics from the first call.
 */
public final class FlashcatGenAI {
    private static final Logger logger = Logger.getLogger("cloud.flashcat.genai");
    private static final Object LOCK = new Object();
    private static volatile Diagnostics initDiagnostics;
    private static volatile boolean initialized;
    private static volatile Thread shutdownHook;

    private FlashcatGenAI() {}

    /**
     * Initializes from environment variables (FLASHCAT_* &gt; OTEL_* &gt; defaults).
     *
     * @return initialization diagnostics
     */
    public static Diagnostics init() {
        return init(Options.builder().build());
    }

    /**
     * Initializes with explicit options (explicit options &gt; FLASHCAT_* &gt; OTEL_* &gt; defaults).
     *
     * @param options explicit initialization options
     * @return initialization diagnostics
     * @throws ConfigException if an explicit option is invalid
     */
    public static Diagnostics init(Options options) {
        synchronized (LOCK) {
            if (initialized) {
                logger.warning("flashcat-genai-otel: init 已调用过，本次忽略（OTel 不允许替换已设置的全局 OpenTelemetry）");
                return diagnostics();
            }
            // Throw for invalid explicit arguments before marking initialization complete.
            Settings s = Settings.resolve(options, new Function<String, String>() {
                @Override
                public String apply(String k) {
                    return Env.get(k);
                }
            }, new Function<String, String>() {
                @Override
                public String apply(String k) {
                    return Env.otel(k);
                }
            });
            initialized = true;
            Diagnostics d;
            try {
                d = doInit(s);
            } catch (Throwable t) {
                Map<String, Object> m = Diagnostics.skeleton();
                m.put("service_name", s.serviceName);
                m.put("environment", s.environment);
                m.put("endpoint", s.endpoint);
                List<String> problems = new ArrayList<String>(s.problems);
                problems.add("初始化 provider 失败：" + t.getClass().getSimpleName() + ": " + t.getMessage());
                m.put("problems", problems);
                d = new Diagnostics(m);
            }
            initDiagnostics = d;
            emitSummary(d, s.debug);
            return d;
        }
    }

    private static Diagnostics doInit(Settings s) {
        State prior = State.get();
        State st = new State(State.Mode.CODE, s);
        for (String p : s.problems) {
            st.problem(p);
        }
        if (s.disabled) {
            st.note("FLASHCAT_DISABLED / OTEL_SDK_DISABLED 已开启，未做任何事");
            st.exporterMode = "none";
            return new Diagnostics(minimal(st));
        }
        State.set(st);

        // Handle the three provider situations (contract §2.4).
        if (AgentDetector.isAgentPresent()) {
            // In Java agent mode, FLASHCAT_* configures the extension's export pipeline and masking. The application cannot override content capture
            // because the semantic API could then write content while L2 masking is disabled in the extension.
            st.note("检测到 OpenTelemetry Java Agent：init 的显式参数不生效，正文开关等配置以 Agent 进程的 FLASHCAT_* 环境变量为准");
            if (AgentDetector.isExtensionActive()) {
                st.situation = "C";
                st.exporterMode = "reuse";
                st.enabled = true;
                st.note("检测到 OpenTelemetry Java Agent 与 Flashcat 扩展：数据由 Agent 导出，本次 init 不新建 provider，只返回诊断");
            } else {
                st.situation = "B";
                st.exporterMode = "none";
                st.problem("检测到 OpenTelemetry Java Agent，但没有加载 Flashcat 扩展；Java 不支持事后追加处理器。请在启动参数里加 -Dotel.javaagent.extensions=/path/to/flashcat-genai-otel-agent-extension.jar");
            }
            return new Diagnostics(merged(st));
        }
        ContentSwitch.setResolved(s.captureContent);
        // Apply the content capture switch before loading instrumentation.
        ContentSwitch.apply(s.captureContent, st.content);
        if (GlobalOpenTelemetry.isSet()) {
            boolean spi = prior != null && prior.mode == State.Mode.SPI && prior.sdk != null;
            String host = Settings.hostOf(Settings.first(Env.otel("otel.exporter.otlp.traces.endpoint"), Env.otel("otel.exporter.otlp.endpoint")));
            boolean flashcatHost = host != null && s.knownHosts.contains(host);
            if (spi) {
                st.note("GlobalOpenTelemetry 由 OTel Spring Boot Starter 创建，flashcat-genai-otel 的 SPI 已在其中生效；本次 init 不新建 provider");
            }
            if (spi || flashcatHost) {
                st.situation = "C";
                st.exporterMode = "reuse";
                st.enabled = true;
                st.note("未新增 exporter，数据经进程里已有的 exporter 发出（GenAI 数据由平台侧识别）");
                if ("add".equals(s.exporter)) {
                    st.note("进程已在往 Flashcat 发数据，显式 exporter='add' 可能产生重复上报；Java 不支持事后追加处理器，未追加");
                }
            } else {
                st.situation = "B";
                st.exporterMode = "none";
                st.problem("进程里已有 OpenTelemetry（GlobalOpenTelemetry 已设置），OTel Java 不支持事后追加处理器，未接入 Flashcat 导出。"
                        + "请改用 Java Agent 扩展（-Dotel.javaagent.extensions=flashcat-genai-otel-agent-extension.jar），"
                        + "或 OTel Spring Boot Starter（引入 cloud.flashcat:flashcat-genai-otel 后 SPI 自动生效），"
                        + "或在自己的 AutoConfiguredOpenTelemetrySdk 上让 ServiceLoader 加载 FlashcatAutoConfigurationCustomizer");
            }
            return new Diagnostics(merged(st));
        }

        // In situation A, build the SDK through autoconfigure with the same SPI and register it globally.
        AutoConfiguredOpenTelemetrySdk auto;
        final FlashcatAutoConfigurationCustomizer customizer = new FlashcatAutoConfigurationCustomizer(st);
        FlashcatAutoConfigurationCustomizer.CODE_INIT.set(Boolean.TRUE);
        try {
            auto = AutoConfiguredOpenTelemetrySdk.builder()
                    .addPropertiesCustomizer(p -> customizer.properties(p))
                    .addResourceCustomizer((r, p) -> customizer.resource(r))
                    .addTracerProviderCustomizer((b, p) -> b.addSpanProcessor(st.sessionProcessor))
                    .addSpanExporterCustomizer((e, p) -> customizer.exporter(e))
                    .addMetricExporterCustomizer((e, p) -> customizer.metricExporter(e))
                    .disableShutdownHook()
                    .build();
        } finally {
            FlashcatAutoConfigurationCustomizer.CODE_INIT.remove();
        }
        OpenTelemetrySdk sdk = auto.getOpenTelemetrySdk();
        st.sdk = sdk;
        st.ownsSdk = true;
        st.situation = "A";
        try {
            GlobalOpenTelemetry.set(sdk);
        } catch (IllegalStateException e) {
            st.situation = "B";
            st.exporterMode = "none";
            st.enabled = false;
            st.problem("GlobalOpenTelemetry 在 init 期间被其他代码设置，Flashcat 的 SDK 未生效");
            sdk.close();
            st.sdk = null;
            st.ownsSdk = false;
            return new Diagnostics(merged(st));
        }
        registerShutdownHook(s.shutdownTimeoutMillis);
        return new Diagnostics(merged(st));
    }

    private static Map<String, Object> minimal(State st) {
        Map<String, Object> d = Diagnostics.skeleton();
        d.put("service_name", st.serviceName);
        d.put("environment", st.settings.environment);
        d.put("endpoint", st.settings.endpoint);
        d.put("problems", new ArrayList<String>(st.problems));
        d.put("notes", new ArrayList<String>(st.notes));
        return d;
    }

    private static Map<String, Object> merged(State st) {
        return st.diagnostics(Thread.currentThread().getContextClassLoader());
    }

    /**
     * Reports whether content capture is enabled by init options, FLASHCAT_CAPTURE_CONTENT, or the Java agent extension.
     * Third-party instrumentation can use this value when configuring capture. For example, set OpenInference TraceConfig's hideInputs and hideOutputs to its inverse.
     *
     * @return true if content capture is enabled
     */
    public static boolean captureContentEnabled() {
        try {
            return ContentSwitch.enabled();
        } catch (Throwable e) {
            return false;
        }
    }

    /**
     * Returns the same diagnostics structure as init with live export counts. FlashAI and coding agents rely on stable field names.
     *
     * <p>In Java agent mode, diagnostics come from the extension's platform MBean. The extension registers it after the first span starts.
     * Until then, diagnostics indicate only that the extension is loaded.
     *
     * @return current diagnostics
     */
    public static Diagnostics diagnostics() {
        try {
            if (AgentDetector.isAgentPresent() && AgentDetector.isExtensionActive() && !AgentDetector.runningInsideAgent()) {
                AgentBridge.awaitRegistered(200, TimeUnit.MILLISECONDS);
                Map<String, Object> fromAgent = AgentBridge.fetchDiagnostics();
                if (fromAgent != null) {
                    return new Diagnostics(fromAgent);
                }
                Map<String, Object> d = Diagnostics.skeleton();
                Diagnostics own = initDiagnostics;
                if (own != null) {
                    d.putAll(own.toMap());
                }
                @SuppressWarnings("unchecked")
                List<String> notes = new ArrayList<String>((List<String>) d.get("notes"));
                notes.add("Flashcat 扩展已在 Java Agent 中加载（版本 " + AgentDetector.extensionVersion() + "），尚未产生 span，导出计数暂不可用");
                d.put("notes", notes);
                return new Diagnostics(d);
            }
            State st = State.get();
            if (st != null && st.settings != null) {
                return new Diagnostics(st.diagnostics(Thread.currentThread().getContextClassLoader()));
            }
            Diagnostics own = initDiagnostics;
            if (own != null) {
                return own;
            }
            if (st != null) {
                Map<String, Object> d = Diagnostics.skeleton();
                d.put("notes", new ArrayList<String>(st.notes));
                return new Diagnostics(d);
            }
        } catch (Throwable t) {
            logger.log(Level.FINE, "flashcat-genai-otel: 生成诊断失败", t);
        }
        Map<String, Object> d = Diagnostics.skeleton();
        List<String> problems = new ArrayList<String>();
        problems.add("init 尚未调用");
        d.put("problems", problems);
        return new Diagnostics(d);
    }

    /**
     * Flushes pending spans and metrics. Call before exiting a script or batch job.
     *
     * @param timeout maximum wait
     * @return true if all data was flushed successfully
     */
    public static boolean flush(Duration timeout) {
        long ms = timeout == null ? 5000L : Math.max(0L, timeout.toMillis());
        long deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(ms);
        try {
            State st = State.get();
            if (st != null && st.sdk instanceof OpenTelemetrySdk) {
                OpenTelemetrySdk sdk = (OpenTelemetrySdk) st.sdk;
                try (ExitDeadline.FlushScope scope = st.exit.beginFlush(remainingMillis(deadline))) {
                    CompletableResultCode t = sdk.getSdkTracerProvider().forceFlush();
                    CompletableResultCode m = sdk.getSdkMeterProvider().forceFlush();
                    CompletableResultCode all = CompletableResultCode.ofAll(java.util.Arrays.asList(t, m));
                    scope.finishWhen(all);
                    return all.join(scope.remainingMillis(), TimeUnit.MILLISECONDS).isSuccess();
                }
            }
            if (AgentDetector.isAgentPresent() && AgentDetector.isExtensionActive()) {
                // The extension registers the MBean in the background after the first span starts. Wait up to 2 seconds.
                AgentBridge.awaitRegistered(Math.min(ms, 2000L), TimeUnit.MILLISECONDS);
                Boolean r = AgentBridge.invokeFlush(remainingMillis(deadline));
                return r != null && r;
            }
        } catch (Throwable t) {
            logger.log(Level.FINE, "flashcat-genai-otel: flush 失败", t);
        }
        return false;
    }

    /**
     * Equivalent to {@code flush(Duration.ofSeconds(5))}.
     *
     * @return true if all data was flushed successfully
     */
    public static boolean flush() {
        return flush(Duration.ofSeconds(5));
    }

    /**
     * Flushes and closes the provider created by this SDK (situation A). Providers owned by the agent, Starter, or application are only flushed.
     *
     * <p>The timeout is shared by flush and shutdown (contract §9). Starting at this call, exports retry at most twice. Spans still pending at the deadline count toward
     * {@code dropped_timeout}。
     *
     * @param timeout total deadline for flush and shutdown
     */
    public static void shutdown(Duration timeout) {
        long ms = timeout == null ? ExitDeadline.DEFAULT_TIMEOUT_MILLIS : Math.max(0L, timeout.toMillis());
        long deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(ms);
        try {
            State st = State.get();
            if (st != null && st.ownsSdk && st.sdk instanceof OpenTelemetrySdk) {
                st.exit.start(ms);
                OpenTelemetrySdk sdk = (OpenTelemetrySdk) st.sdk;
                CompletableResultCode t = sdk.getSdkTracerProvider().forceFlush();
                CompletableResultCode m = sdk.getSdkMeterProvider().forceFlush();
                CompletableResultCode.ofAll(java.util.Arrays.asList(t, m)).join(remainingMillis(deadline), TimeUnit.MILLISECONDS);
                sdk.shutdown().join(remainingMillis(deadline), TimeUnit.MILLISECONDS);
                return;
            }
        } catch (Throwable t) {
            logger.log(Level.FINE, "flashcat-genai-otel: shutdown 失败", t);
            return;
        }
        // Only flush providers owned by other code.
        flush(Duration.ofMillis(ms));
    }

    private static long remainingMillis(long deadlineNanos) {
        return Math.max(0L, TimeUnit.NANOSECONDS.toMillis(deadlineNanos - System.nanoTime()));
    }

    /** Equivalent to {@code shutdown(Duration.ofSeconds(5))}. */
    public static void shutdown() {
        shutdown(Duration.ofSeconds(5));
    }

    private static void registerShutdownHook(final long timeoutMillis) {
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                // Flush and shut down automatically on process exit within shutdown_timeout (contract §9).
                shutdown(Duration.ofMillis(timeoutMillis));
            }
        }, "flashcat-genai-otel-shutdown");
        try {
            Runtime.getRuntime().addShutdownHook(t);
            shutdownHook = t;
        } catch (Throwable e) {
            logger.log(Level.FINE, "flashcat-genai-otel: 注册退出钩子失败", e);
        }
    }

    /** Logs a one-line INFO summary at init and full diagnostics when debug is enabled (contract §8). */
    static void emitSummary(Diagnostics d, boolean debug) {
        try {
            if (debug) {
                logger.info("flashcat-genai-otel diagnostics: " + d.toJson());
                return;
            }
            Map<String, Object> brief = new LinkedHashMap<String, Object>();
            brief.put("service", d.get("service_name"));
            brief.put("enabled", d.get("enabled"));
            brief.put("provider", d.get("provider"));
            List<String> installed = new ArrayList<String>();
            Object insts = d.get("instrumentations");
            if (insts instanceof List) {
                for (Object o : (List<?>) insts) {
                    if (o instanceof Map && "installed".equals(((Map<?, ?>) o).get("status"))) {
                        Map<?, ?> m = (Map<?, ?>) o;
                        // Include the API when a library has multiple entries, such as upstream and Flashcat instrumentation for openai-java.
                        installed.add(m.get("api") != null ? m.get("lib") + "[" + m.get("api") + "]" : String.valueOf(m.get("lib")));
                    }
                }
            }
            brief.put("installed", installed);
            brief.put("problems", d.get("problems"));
            logger.info("flashcat-genai-otel: " + Json.write(brief));
        } catch (Throwable ignored) {
            // Logging failures must leave application code unaffected.
        }
    }

    /** Clears all static state for tests. */
    static void resetForTest() {
        synchronized (LOCK) {
            State st = State.get();
            if (st != null && st.ownsSdk && st.sdk instanceof OpenTelemetrySdk) {
                try {
                    ((OpenTelemetrySdk) st.sdk).shutdown().join(5, TimeUnit.SECONDS);
                } catch (Throwable ignored) {
                    // Ignore failures during test cleanup.
                }
            }
            Thread hook = shutdownHook;
            if (hook != null) {
                try {
                    Runtime.getRuntime().removeShutdownHook(hook);
                } catch (Throwable ignored) {
                    // Ignore failures during test cleanup.
                }
                shutdownHook = null;
            }
            initialized = false;
            initDiagnostics = null;
            State.resetForTest();
            ContentSwitch.resetForTest();
            InstrumentationDetector.resetForTest();
            Flashcat.resetForTest();
        }
    }

    /**
     * Explicit arguments to {@link #init(Options)}. Names follow contract §2.1 in Java camel case. Unset options follow FLASHCAT_* &gt; OTEL_* &gt; defaults.
     *
     * <pre>{@code
     * FlashcatGenAI.Options.builder()
     *     .serviceName("kefu-agent")
     *     .endpoint("https://ingest.example.com/v1/traces")
     *     .apiKey("keyID:secret")
     *     .captureContent("false")
     *     .build();
     * }</pre>
     */
    public static final class Options {
        final String serviceName;
        final String environment;
        final String endpoint;
        final String apiKey;
        final String exporter;
        final String captureContent;
        final boolean metrics;
        final MaskingConfig masking;
        final Map<String, String> resourceAttributes;
        final long exportTimeoutMillis;
        final long shutdownTimeoutMillis;
        final boolean debug;

        private Options(Builder b) {
            this.serviceName = b.serviceName;
            this.environment = b.environment;
            this.endpoint = b.endpoint;
            this.apiKey = b.apiKey;
            this.exporter = b.exporter;
            this.captureContent = b.captureContent;
            this.metrics = b.metrics;
            this.masking = b.masking;
            this.resourceAttributes = Collections.unmodifiableMap(new LinkedHashMap<String, String>(b.resourceAttributes));
            this.exportTimeoutMillis = b.exportTimeoutMillis;
            this.shutdownTimeoutMillis = b.shutdownTimeoutMillis;
            this.debug = b.debug;
        }

        /** @return a builder */
        public static Builder builder() {
            return new Builder();
        }

        /** Builder for {@link Options}. Setters or {@link #build()} throw {@link ConfigException} for invalid values. */
        public static final class Builder {
            private String serviceName;
            private String environment;
            private String endpoint;
            private String apiKey;
            private String exporter = "auto";
            private String captureContent;
            private boolean metrics = true;
            private MaskingConfig masking;
            private final Map<String, String> resourceAttributes = new LinkedHashMap<String, String>();
            private long exportTimeoutMillis = 10_000L;
            /** Zero leaves the timeout unset. Fall back to FLASHCAT_SHUTDOWN_TIMEOUT, then 5 seconds. */
            private long shutdownTimeoutMillis;
            private boolean debug;

            private Builder() {}

            /**
             * Application name (service.name). An empty name or a name starting with unknown_service disables export.
             *
             * @param v application name
             * @return this
             */
            public Builder serviceName(String v) {
                this.serviceName = v;
                return this;
            }

            /**
             * Environment (deployment.environment.name), defaulting to prod.
             *
             * @param v environment
             * @return this
             */
            public Builder environment(String v) {
                this.environment = v;
                return this;
            }

            /**
             * Ingest endpoint as a full HTTP(S) URL with a path, such as {@code https://ingest.example.com/v1/traces}. Supply a port if needed.
             *
             * @param v ingest endpoint
             * @return this
             */
            public Builder endpoint(String v) {
                this.endpoint = v;
                return this;
            }

            /**
             * Credentials in {@code keyID:secret} format. The SDK handles Basic encoding.
             *
             * @param v credentials
             * @return this
             */
            public Builder apiKey(String v) {
                this.apiKey = v;
                return this;
            }

            /**
             * Selects auto (the default), add, reuse, or none for provider handling.
             *
             * @param v provider handling mode
             * @return this
             * @throws ConfigException if the mode is invalid
             */
            public Builder exporter(String v) {
                String t = v == null ? null : v.trim().toLowerCase(Locale.ROOT);
                if (t == null || !(t.equals("auto") || t.equals("add") || t.equals("reuse") || t.equals("none"))) {
                    throw new ConfigException("exporter 只能是 \"auto\" / \"add\" / \"reuse\" / \"none\"，收到 '" + v + "'");
                }
                this.exporter = t;
                return this;
            }

            /**
             * Content capture switch. Use {@code "false"} (default, capture off) or {@code "span"} (write to span attributes). Aliases are off, no_content, and span_only.
             * {@code "true"} is ambiguous and throws. Java reports unsupported values such as event as a problem and disables capture.
             *
             * @param v content capture mode
             * @return this
             * @throws ConfigException if the value is invalid
             */
            public Builder captureContent(String v) {
                Settings.parseExplicitContent(v);
                this.captureContent = v;
                return this;
            }

            /**
             * Controls metrics export (enabled by default, every 60 seconds).
             *
             * @param v whether to export metrics
             * @return this
             */
            public Builder metrics(boolean v) {
                this.metrics = v;
                return this;
            }

            /**
             * Masking and truncation settings (default {@link MaskingConfig#defaults()}).
             *
             * @param v masking and truncation settings
             * @return this
             */
            public Builder masking(MaskingConfig v) {
                this.masking = v;
                return this;
            }

            /**
             * Adds a resource attribute in provider situation A.
             *
             * @param key attribute key
             * @param value attribute value
             * @return this
             */
            public Builder resourceAttribute(String key, String value) {
                if (key == null || value == null) {
                    throw new ConfigException("resourceAttribute 的键和值都不能为 null");
                }
                this.resourceAttributes.put(key, value);
                return this;
            }

            /**
             * Export timeout (default 10 seconds).
             *
             * @param v positive export timeout
             * @return this
             */
            public Builder exportTimeout(Duration v) {
                if (v == null || v.isNegative() || v.isZero()) {
                    throw new ConfigException("exportTimeout 必须大于 0，收到 " + v);
                }
                this.exportTimeoutMillis = v.toMillis();
                return this;
            }

            /**
             * Total deadline for flushing and closing on process exit (default 5 seconds). This option takes precedence over FLASHCAT_SHUTDOWN_TIMEOUT.
             *
             * @param v positive shutdown timeout
             * @return this
             */
            public Builder shutdownTimeout(Duration v) {
                if (v == null || v.isNegative() || v.isZero()) {
                    throw new ConfigException("shutdownTimeout 必须大于 0，收到 " + v);
                }
                this.shutdownTimeoutMillis = v.toMillis();
                return this;
            }

            /**
             * Enables full diagnostics logging at init.
             *
             * @param v whether to log full diagnostics at init
             * @return this
             */
            public Builder debug(boolean v) {
                this.debug = v;
                return this;
            }

            /** @return initialization options */
            public Options build() {
                return new Options(this);
            }
        }
    }
}
