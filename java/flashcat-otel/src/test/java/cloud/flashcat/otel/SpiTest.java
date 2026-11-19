package cloud.flashcat.otel;

import static cloud.flashcat.otel.TestSupport.env;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import cloud.flashcat.otel.testing.FakeIngest;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.autoconfigure.AutoConfiguredOpenTelemetrySdk;
import io.opentelemetry.sdk.autoconfigure.spi.internal.DefaultConfigProperties;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

/** Tests mapping FLASHCAT_* to OTel properties (contract §1) and exercises the ServiceLoader path end to end. */
class SpiTest {

    @AfterEach
    void tearDown() {
        TestSupport.reset(null);
    }

    private static Map<String, String> translate(Map<String, String> flashcatEnv, Map<String, String> otelProps) {
        TestSupport.reset(flashcatEnv);
        FlashcatAutoConfigurationCustomizer c = new FlashcatAutoConfigurationCustomizer();
        return c.properties(DefaultConfigProperties.createFromMap(otelProps));
    }

    @Test
    void translatesFlashcatEnvToPerSignalOtelProperties() throws Exception {
        Map<String, String> m = translate(env("FLASHCAT_ENDPOINT", "https://ingest.example.com/v1/traces",
                "FLASHCAT_API_KEY", "fck:s3cret+/=", "FLASHCAT_SERVICE_NAME", "kefu-agent", "FLASHCAT_CAPTURE_CONTENT", "span"), new HashMap<String, String>());
        assertEquals("kefu-agent", m.get("otel.service.name"));
        assertEquals("otlp", m.get("otel.traces.exporter"));
        assertEquals("https://ingest.example.com/v1/traces", m.get("otel.exporter.otlp.traces.endpoint"));
        assertEquals("http/protobuf", m.get("otel.exporter.otlp.traces.protocol"));
        assertEquals("gzip", m.get("otel.exporter.otlp.traces.compression"));
        assertEquals("10000", m.get("otel.exporter.otlp.traces.timeout"));
        assertEquals("https://ingest.example.com/v1/metrics", m.get("otel.exporter.otlp.metrics.endpoint"));
        assertEquals("http/protobuf", m.get("otel.exporter.otlp.metrics.protocol"));
        assertEquals("60000", m.get("otel.metric.export.interval"));
        assertEquals("none", m.get("otel.logs.exporter"));
        assertEquals("true", m.get("otel.instrumentation.genai.capture-message-content"));
        // OTel Java URL-decodes the header value to Basic base64(keyID:secret).
        String headers = m.get("otel.exporter.otlp.traces.headers");
        assertTrue(headers.startsWith("Authorization="), headers);
        String decoded = URLDecoder.decode(headers.substring("Authorization=".length()), "UTF-8");
        assertEquals("Basic " + Base64.getEncoder().encodeToString("fck:s3cret+/=".getBytes(StandardCharsets.UTF_8)), decoded);
        assertFalse(m.containsKey("otel.exporter.otlp.endpoint"), "Leave the shared endpoint alone to avoid affecting logs and other signals");
        assertFalse(m.containsKey("otel.exporter.otlp.headers"));
    }

    @Test
    void flashcatEnvBeatsOtelProperties() {
        Map<String, String> otel = new HashMap<String, String>();
        otel.put("otel.exporter.otlp.traces.endpoint", "https://other-vendor.example.com/v1/traces");
        otel.put("otel.service.name", "otel-name");
        otel.put("otel.exporter.otlp.headers", "x-tenant=t1,Authorization=Bearer%20other");
        otel.put("otel.traces.exporter", "console");
        otel.put("otel.logs.exporter", "otlp");
        Map<String, String> m = translate(env("FLASHCAT_ENDPOINT", "https://ingest.example.com/v1/traces",
                "FLASHCAT_API_KEY", "k:s", "FLASHCAT_SERVICE_NAME", "fc-name"), otel);
        assertEquals("https://ingest.example.com/v1/traces", m.get("otel.exporter.otlp.traces.endpoint"));
        assertEquals("fc-name", m.get("otel.service.name"));
        assertEquals("console,otlp", m.get("otel.traces.exporter"));
        assertNull(m.get("otel.logs.exporter"), "Preserve the explicitly configured logs exporter");
        String headers = m.get("otel.exporter.otlp.traces.headers");
        assertTrue(headers.startsWith("x-tenant=t1,Authorization=Basic%20"), headers);
    }

    @Test
    void otelOnlyConfigurationStillWorks() {
        Map<String, String> otel = new HashMap<String, String>();
        otel.put("otel.exporter.otlp.endpoint", "https://ingest.example.com");
        otel.put("otel.exporter.otlp.headers", "Authorization=Basic%20YTpi");
        otel.put("otel.service.name", "svc");
        Map<String, String> m = translate(env(), otel);
        assertEquals("https://ingest.example.com/v1/traces", m.get("otel.exporter.otlp.traces.endpoint"));
        assertEquals("Authorization=Basic%20YTpi", m.get("otel.exporter.otlp.traces.headers"));
    }

    @Test
    void invalidConfigWithoutUserExportTurnsDefaultExportOff() {
        Map<String, String> m = translate(env("FLASHCAT_ENDPOINT", "ingest.example.com", "FLASHCAT_API_KEY", "k:s"), new HashMap<String, String>());
        assertEquals("none", m.get("otel.traces.exporter"), "Avoid falling back to the Agent default of localhost:4318");
        assertEquals("none", m.get("otel.metrics.exporter"));
        assertEquals("none", m.get("otel.logs.exporter"));
        assertEquals("false", m.get("otel.instrumentation.genai.capture-message-content"));
        Diagnostics d = FlashcatOtel.diagnostics();
        assertTrue(d.getProblems().stream().anyMatch(p -> p.contains("完整 URL")), d.toJson());
    }

    @Test
    void invalidConfigKeepsUsersOwnOtelExport() {
        Map<String, String> otel = new HashMap<String, String>();
        otel.put("otel.exporter.otlp.endpoint", "http://collector:4318");
        Map<String, String> m = translate(env("FLASHCAT_ENDPOINT", "ingest.example.com", "FLASHCAT_API_KEY", "k:s"), otel);
        assertNull(m.get("otel.traces.exporter"));
        assertNull(m.get("otel.exporter.otlp.traces.endpoint"));
    }

    @Test
    void disabledSpiDoesNothing() {
        TestSupport.reset(env("FLASHCAT_DISABLED", "1", "FLASHCAT_ENDPOINT", "https://ingest.example.com/v1/traces", "FLASHCAT_API_KEY", "k:s"));
        AutoConfiguredOpenTelemetrySdk auto = AutoConfiguredOpenTelemetrySdk.builder()
                .addPropertiesSupplier(() -> props("otel.traces.exporter", "none", "otel.metrics.exporter", "none", "otel.logs.exporter", "none"))
                .disableShutdownHook().build();
        assertTrue(FlashcatOtel.diagnostics().getNotes().get(0).contains("FLASHCAT_DISABLED"));
        auto.getOpenTelemetrySdk().close();
    }

    /**
     * The ServiceLoader path shares the SPI with Spring Boot Starter and the Java Agent.
     * Verify SDK creation, export, masking, counters, and resource attributes.
     */
    @Test
    void serviceLoaderPathEndToEnd() throws Exception {
        try (FakeIngest ingest = new FakeIngest()) {
            TestSupport.reset(env("FLASHCAT_ENDPOINT", ingest.tracesUrl(), "FLASHCAT_API_KEY", "fck:s", "FLASHCAT_SERVICE_NAME", "spi-svc",
                    "FLASHCAT_ENVIRONMENT", "staging"));
            AutoConfiguredOpenTelemetrySdk auto = AutoConfiguredOpenTelemetrySdk.builder()
                    .addPropertiesSupplier(() -> props("otel.metrics.exporter", "none"))
                    .disableShutdownHook().build();
            OpenTelemetrySdk sdk = auto.getOpenTelemetrySdk();
            try {
                Span s = sdk.getTracer("t").spanBuilder("job").setAttribute("db.password", "hunter2").startSpan();
                s.end();
                sdk.getSdkTracerProvider().forceFlush().join(10, TimeUnit.SECONDS);
                FakeIngest.ReceivedSpan job = ingest.spanNamed("job");
                assertNotNull(job, ingest.spans().toString());
                assertEquals("[REDACTED]", job.attr("db.password"));
                assertEquals("spi-svc", job.resource.get("service.name"));
                assertEquals("staging", job.resource.get("deployment.environment.name"));
                assertEquals("flashcat-otel", job.resource.get("telemetry.distro.name"));
                assertEquals("Basic " + Base64.getEncoder().encodeToString("fck:s".getBytes(StandardCharsets.UTF_8)),
                        ingest.requests("/v1/traces").get(0).header("Authorization"));
                assertTrue(ingest.requests("/v1/traces").get(0).gzip);
                Diagnostics d = FlashcatOtel.diagnostics();
                assertEquals("add", ((Map<?, ?>) d.get("provider")).get("exporter_mode"));
                assertTrue(((Number) ((Map<?, ?>) d.get("export")).get("ok")).longValue() >= 1, d.toJson());
                // Standard autoconfigure leaves GlobalOpenTelemetry alone. Spring Boot Starter uses a separate path.
                assertFalse(GlobalOpenTelemetry.isSet());
            } finally {
                sdk.close();
            }
        }
    }

    @Test
    void reuseExporterMarksOnlyAtStart() throws Exception {
        try (FakeIngest ingest = new FakeIngest()) {
            TestSupport.reset(env("FLASHCAT_EXPORTER", "reuse"));
            AutoConfiguredOpenTelemetrySdk auto = AutoConfiguredOpenTelemetrySdk.builder()
                    .addPropertiesSupplier(() -> props("otel.traces.exporter", "otlp", "otel.exporter.otlp.protocol", "http/protobuf",
                            "otel.exporter.otlp.traces.endpoint", ingest.tracesUrl(), "otel.metrics.exporter", "none", "otel.logs.exporter", "none"))
                    .disableShutdownHook().build();
            try (OpenTelemetrySdk sdk = auto.getOpenTelemetrySdk()) {
                sdk.getTracer("model").spanBuilder("early").setAttribute("gen_ai.operation.name", "chat").startSpan().end();
                Span late = sdk.getTracer("model").spanBuilder("late").startSpan();
                late.setAttribute("gen_ai.operation.name", "chat").end();

                assertTrue(sdk.getSdkTracerProvider().forceFlush().join(10, TimeUnit.SECONDS).isSuccess());
                assertEquals(true, ingest.spanNamed("early").attr("flashcat.genai.root"));
                assertNull(ingest.spanNamed("late").attr("flashcat.genai.root"));
            }
        }
    }

    /**
     * Starter may supply service.name from spring.application.name, so the SPI validates the name
     * during resource configuration. An invalid final name prevents export.
     */
    @Test
    void invalidFinalServiceNameDropsExport() throws Exception {
        try (FakeIngest ingest = new FakeIngest()) {
            TestSupport.reset(env("FLASHCAT_ENDPOINT", ingest.tracesUrl(), "FLASHCAT_API_KEY", "fck:s"));
            AutoConfiguredOpenTelemetrySdk auto = AutoConfiguredOpenTelemetrySdk.builder()
                    .addPropertiesSupplier(() -> props("otel.metrics.exporter", "none"))
                    .disableShutdownHook().build();
            OpenTelemetrySdk sdk = auto.getOpenTelemetrySdk();
            sdk.getTracer("t").spanBuilder("job").startSpan().end();
            sdk.getSdkTracerProvider().forceFlush().join(10, TimeUnit.SECONDS);
            assertTrue(ingest.requests().isEmpty());
            Diagnostics d = FlashcatOtel.diagnostics();
            assertTrue(d.getProblems().stream().anyMatch(p -> p.contains("service_name")), d.toJson());
            sdk.close();
        }
    }

    /** The SPI reads otel.metrics.exporter=none from a system property or environment variable via ConfigProperties and preserves it. */
    @Test
    void explicitMetricsExporterNoneIsRespected() {
        Map<String, String> otel = new HashMap<String, String>();
        otel.put("otel.metrics.exporter", "none");
        Map<String, String> m = translate(env("FLASHCAT_ENDPOINT", "https://ingest.example.com/v1/traces", "FLASHCAT_API_KEY", "k:s",
                "FLASHCAT_SERVICE_NAME", "svc"), otel);
        assertFalse(m.containsKey("otel.metrics.exporter"), "Expected the user's none setting to be preserved: " + m);
        assertFalse(m.containsKey("otel.exporter.otlp.metrics.endpoint"));
        assertEquals("otlp", m.get("otel.traces.exporter"), "Trace export remains enabled");
        Diagnostics d = FlashcatOtel.diagnostics();
        assertEquals("disabled", ((Map<?, ?>) d.get("provider")).get("metrics"), d.toJson());
        assertTrue(d.getNotes().stream().anyMatch(n -> n.contains("otel.metrics.exporter=none")), d.toJson());

        // Case and whitespace variations.
        otel.put("otel.metrics.exporter", " NONE ");
        m = translate(env("FLASHCAT_ENDPOINT", "https://ingest.example.com/v1/traces", "FLASHCAT_API_KEY", "k:s", "FLASHCAT_SERVICE_NAME", "svc"), otel);
        assertFalse(m.containsKey("otel.metrics.exporter"), m.toString());

        // Append otlp if the user configured another exporter.
        otel.put("otel.metrics.exporter", "console");
        m = translate(env("FLASHCAT_ENDPOINT", "https://ingest.example.com/v1/traces", "FLASHCAT_API_KEY", "k:s", "FLASHCAT_SERVICE_NAME", "svc"), otel);
        assertEquals("console,otlp", m.get("otel.metrics.exporter"));
    }

    /** The Agent prints its startup summary before loading the extension's openai-java instrumentation. Report its state early using the Agent's enablement rules. */
    @Test
    void openAiInstrumentationEnablementFollowsAgentConfig() {
        assertTrue(FlashcatAutoConfigurationCustomizer.openAiInstrumentationEnabled(k -> null));
        assertFalse(FlashcatAutoConfigurationCustomizer.openAiInstrumentationEnabled(
                k -> k.equals("otel.instrumentation.flashcat-openai-java.enabled") ? "false" : null));
        assertFalse(FlashcatAutoConfigurationCustomizer.openAiInstrumentationEnabled(
                k -> k.equals("otel.instrumentation.common.default-enabled") ? "false" : null));
        assertTrue(FlashcatAutoConfigurationCustomizer.openAiInstrumentationEnabled(k -> k.equals("otel.instrumentation.common.default-enabled") ? "false"
                : k.equals("otel.instrumentation.flashcat-openai-java.enabled") ? "true" : null));
    }

    private static Map<String, String> props(String... kv) {
        Map<String, String> m = new HashMap<String, String>();
        for (int i = 0; i + 1 < kv.length; i += 2) {
            m.put(kv[i], kv[i + 1]);
        }
        return m;
    }
}
