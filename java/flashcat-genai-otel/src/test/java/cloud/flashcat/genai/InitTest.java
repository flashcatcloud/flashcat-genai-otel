package cloud.flashcat.genai;

import static cloud.flashcat.genai.TestSupport.env;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertTrue;

import cloud.flashcat.genai.testing.FakeIngest;
import cloud.flashcat.genai.testing.MiniJson;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/** End-to-end tests of FlashcatGenAI.init against a mock ingest endpoint. Test names match Python's tests/test_sdk.py. */
class InitTest {
    private FakeIngest ingest;

    @BeforeEach
    void setUp() throws Exception {
        ingest = new FakeIngest();
    }

    @AfterEach
    void tearDown() {
        TestSupport.reset(null);
        ingest.close();
    }

    private Map<String, String> flashcatEnv(String... extra) {
        Map<String, String> m = env("FLASHCAT_ENDPOINT", ingest.tracesUrl(), "FLASHCAT_API_KEY", "fck_test:s3cret");
        m.putAll(env(extra));
        return m;
    }

    private static String basic(String key) {
        return "Basic " + Base64.getEncoder().encodeToString(key.getBytes(StandardCharsets.UTF_8));
    }

    @Test
    void situationAEndToEnd() throws Exception {
        TestSupport.reset(flashcatEnv());
        Diagnostics d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("kefu-agent").captureContent("false").build());
        assertTrue(d.isEnabled(), d.toJson());
        Map<?, ?> provider = (Map<?, ?>) d.get("provider");
        assertEquals("A", provider.get("situation"));
        assertEquals("add", provider.get("exporter_mode"));
        assertEquals("add", provider.get("metrics"), "The metrics endpoint is derived from the traces endpoint");
        assertTrue(GlobalOpenTelemetry.isSet());

        String out;
        try (Flashcat.Session s = Flashcat.session().conversationId("c-123").userId("u-9").tag("team", "kf").start()) {
            Tracer t = GlobalOpenTelemetry.getOrNoop().getTracer("fake-instrumentation");
            Span http = t.spanBuilder("GET /orders").setSpanKind(SpanKind.SERVER).startSpan();
            try (Scope ignored = http.makeCurrent()) {
                out = Flashcat.agent("kefu", () -> {
                    Flashcat.tool("lookup_order", Flashcat.SpanOptions.create().input("A-1"), () -> "shipped");
                    // Simulate an instrumented model call with a child span from a standard OTel tracer.
                    Span chat = t.spanBuilder("chat gpt-4o").setAttribute("gen_ai.operation.name", "chat")
                            .setAttribute("gen_ai.usage.input_tokens", 120L).startSpan();
                    chat.end();
                    return "您的订单已发货";
                });
            } finally {
                http.end();
            }
        }
        assertEquals("您的订单已发货", out);
        assertTrue(FlashcatGenAI.flush(Duration.ofSeconds(10)));

        List<FakeIngest.Request> reqs = ingest.requests("/v1/traces");
        assertFalse(reqs.isEmpty());
        FakeIngest.Request r = reqs.get(0);
        assertEquals(basic("fck_test:s3cret"), r.header("Authorization"));
        assertTrue(r.gzip, "Expected a gzip-compressed request body");
        assertEquals("application/x-protobuf", r.header("Content-Type"));

        List<FakeIngest.ReceivedSpan> spans = ingest.spans();
        Map<String, FakeIngest.ReceivedSpan> byOp = new HashMap<String, FakeIngest.ReceivedSpan>();
        for (FakeIngest.ReceivedSpan s : spans) {
            byOp.put(String.valueOf(s.attr("gen_ai.operation.name")), s);
        }
        FakeIngest.ReceivedSpan chat = byOp.get("chat");
        FakeIngest.ReceivedSpan agent = byOp.get("invoke_agent");
        FakeIngest.ReceivedSpan tool = byOp.get("execute_tool");
        FakeIngest.ReceivedSpan http = ingest.spanNamed("GET /orders");
        assertNotNull(http, spans.toString());
        assertEquals("c-123", http.attr("gen_ai.conversation.id"));
        assertEquals(http.spanId, agent.parentSpanId);
        assertNull(http.attr("flashcat.genai.root"));
        assertEquals(true, agent.attr("flashcat.genai.root"));
        assertEquals(false, tool.attr("flashcat.genai.root"));
        assertNotNull(chat, spans.toString());
        assertEquals(false, chat.attr("flashcat.genai.root"));
        assertEquals("kefu-agent", chat.resource.get("service.name"));
        assertEquals("prod", chat.resource.get("deployment.environment.name"));
        assertEquals("flashcat-genai-otel", chat.resource.get("telemetry.distro.name"));
        assertEquals("0.1.0", chat.resource.get("telemetry.distro.version"));
        assertEquals("java", chat.resource.get("telemetry.sdk.language"));
        for (FakeIngest.ReceivedSpan s : Arrays.asList(chat, agent, tool)) {
            assertEquals("c-123", s.attr("gen_ai.conversation.id"), s.name);
            assertEquals("u-9", s.attr("user.id"), s.name);
            assertEquals("kf", s.attr("flashcat.tag.team"), s.name);
        }
        assertEquals(120L, chat.attr("gen_ai.usage.input_tokens"));
        assertFalse(chat.attrs.containsKey("gen_ai.input.messages"), "Content capture is off by default");
        assertFalse(tool.attrs.containsKey("gen_ai.tool.call.arguments"), "Content capture is off by default");
        assertEquals(agent.spanId, chat.parentSpanId);
        assertEquals(agent.spanId, tool.parentSpanId);
        assertEquals("invoke_agent kefu", agent.name);
        assertEquals("kefu", agent.attr("gen_ai.agent.name"));
        assertEquals("flashcat.genai", agent.scope);
        assertEquals("0.1.0", agent.scopeVersion);

        Diagnostics after = FlashcatGenAI.diagnostics();
        Map<?, ?> export = (Map<?, ?>) after.get("export");
        assertTrue(((Number) export.get("ok")).longValue() >= 3, after.toJson());
        assertEquals("NO_CONTENT", ((Map<?, ?>) after.get("content")).get("upstream"));
        assertEquals("metadata_only", System.getProperty(ContentSwitch.ANTHROPIC_PROP), "Set the Anthropic content mode when the user leaves it unset");
    }

    @Test
    void marksLateGenAiSpansOnExport() throws Exception {
        TestSupport.reset(flashcatEnv());
        FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("late-genai").build());
        Tracer tracer = GlobalOpenTelemetry.getOrNoop().getTracer("late-instrumentation");

        Span root = tracer.spanBuilder("late-root").startSpan();
        root.setAttribute("llm.request.type", "chat").end();
        Flashcat.agent("kefu", () -> Flashcat.trace("ordinary", () -> {
            Span child = tracer.spanBuilder("late-child").startSpan();
            child.setAttribute("gen_ai.usage.input_tokens", 120L).setAttribute("db.password", "secret").end();
        }));

        assertTrue(FlashcatGenAI.flush(Duration.ofSeconds(10)));
        assertEquals(true, ingest.spanNamed("late-root").attr("flashcat.genai.root"));
        assertEquals(true, ingest.spanNamed("invoke_agent kefu").attr("flashcat.genai.root"));
        assertNull(ingest.spanNamed("ordinary").attr("flashcat.genai.root"));
        FakeIngest.ReceivedSpan child = ingest.spanNamed("late-child");
        assertEquals(false, child.attr("flashcat.genai.root"));
        assertEquals("[REDACTED]", child.attr("db.password"));
    }

    @Test
    void contentSpanWithMasking() throws Exception {
        TestSupport.reset(flashcatEnv());
        FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("kefu-agent").captureContent("span").metrics(false).build());
        Flashcat.agent("kefu", Flashcat.SpanOptions.create().input("手机13812345678，邮箱a@b.com"), () -> {
            Flashcat.tool("lookup_order", Flashcat.SpanOptions.create().input(java.util.Collections.singletonMap("order_id", "A-1")),
                    () -> "shipped");
            return "已为 13900001111 查询";
        });
        FlashcatGenAI.flush(Duration.ofSeconds(10));
        FakeIngest.ReceivedSpan agent = ingest.spanNamed("invoke_agent kefu");
        String msgs = (String) agent.attr("gen_ai.input.messages");
        assertFalse(msgs.contains("13812345678"), msgs);
        assertFalse(msgs.contains("a@b.com"), msgs);
        assertTrue(msgs.contains("[REDACTED:phone]") && msgs.contains("[REDACTED:email]"), msgs);
        assertTrue(msgs.startsWith("[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":"), msgs);
        String outMsgs = (String) agent.attr("gen_ai.output.messages");
        assertTrue(outMsgs.contains("\"role\":\"assistant\"") && outMsgs.contains("[REDACTED:phone]"), outMsgs);
        FakeIngest.ReceivedSpan tool = ingest.spanNamed("execute_tool lookup_order");
        assertTrue(((String) tool.attr("gen_ai.tool.call.arguments")).contains("A-1"));
        assertEquals("shipped", tool.attr("gen_ai.tool.call.result"));
        Map<?, ?> sem = (Map<?, ?>) FlashcatGenAI.diagnostics().get("semantics");
        assertTrue(((Number) sem.get("masked_spans")).longValue() >= 1);
        assertEquals("content", System.getProperty(ContentSwitch.ANTHROPIC_PROP));
    }

    @Test
    void keyMaskingAppliesWithoutContent() throws Exception {
        TestSupport.reset(flashcatEnv());
        FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").metrics(false).build());
        Flashcat.trace("job", java.util.Collections.singletonMap("http.request.header.authorization", "Bearer sk-live"), () -> {});
        FlashcatGenAI.flush(Duration.ofSeconds(10));
        assertEquals("[REDACTED]", ingest.spanNamed("job").attr("http.request.header.authorization"));
    }

    @Test
    void http401CountedInDiagnostics() throws Exception {
        ingest.setStatus(401, "invalid api key");
        TestSupport.reset(flashcatEnv("FLASHCAT_API_KEY", "k:bad"));
        FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").metrics(false).build());
        Flashcat.trace("job", () -> {});
        Flashcat.workflow("wf", () -> 1);
        FlashcatGenAI.flush(Duration.ofSeconds(10));
        Map<?, ?> export = (Map<?, ?>) FlashcatGenAI.diagnostics().get("export");
        assertTrue(export.get("http_401") != null && ((Number) export.get("http_401")).longValue() >= 1, export.toString());
        assertTrue(String.valueOf(export.get("last_error")).contains("401"), export.toString());
        assertEquals(0L, ((Number) export.get("ok")).longValue());
    }

    @Test
    void http500CountedAs5xx() throws Exception {
        ingest.setStatus(500, "boom");
        TestSupport.reset(flashcatEnv());
        FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").metrics(false).build());
        Flashcat.trace("job", () -> {});
        FlashcatGenAI.flush(Duration.ofSeconds(10));
        Map<?, ?> export = (Map<?, ?>) FlashcatGenAI.diagnostics().get("export");
        assertTrue(export.get("http_5xx") != null && ((Number) export.get("http_5xx")).longValue() >= 1, export.toString());
    }

    @Test
    void endpointWithoutPathRejected() {
        TestSupport.reset(env("FLASHCAT_ENDPOINT", "ingest.example.com", "FLASHCAT_API_KEY", "k:s"));
        Diagnostics d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").build());
        assertTrue(d.getProblems().stream().anyMatch(p -> p.contains("完整 URL")), d.toJson());
        assertEquals("none", ((Map<?, ?>) d.get("provider")).get("exporter_mode"));
        assertFalse(d.isEnabled());
    }

    @Test
    void missingServiceName() throws Exception {
        TestSupport.reset(flashcatEnv());
        Diagnostics d = FlashcatGenAI.init();
        assertTrue(d.getProblems().stream().anyMatch(p -> p.contains("service_name")), d.toJson());
        assertEquals("none", ((Map<?, ?>) d.get("provider")).get("exporter_mode"));
        Flashcat.trace("job", () -> {});
        FlashcatGenAI.flush(Duration.ofSeconds(5));
        assertTrue(ingest.requests().isEmpty(), "Disabling export must prevent all export requests");
    }

    @Test
    void badApiKeyFormat() {
        TestSupport.reset(env("FLASHCAT_ENDPOINT", ingest.tracesUrl(), "FLASHCAT_API_KEY", "no-colon"));
        Diagnostics d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").build());
        assertTrue(d.getProblems().stream().anyMatch(p -> p.contains("keyID:secret")), d.toJson());
        assertFalse(d.isEnabled());
    }

    @Test
    void initIsIdempotent() {
        TestSupport.reset(flashcatEnv());
        Diagnostics first = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").build());
        Object global = GlobalOpenTelemetry.get();
        Diagnostics second = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("other").build());
        assertSame(global, GlobalOpenTelemetry.get());
        assertEquals(first.getServiceName(), second.getServiceName());
        assertEquals("x", second.getServiceName());
    }

    @Test
    void disabledDoesNothing() {
        TestSupport.reset(flashcatEnv("FLASHCAT_DISABLED", "true"));
        Diagnostics d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").build());
        assertFalse(d.isEnabled());
        assertTrue(d.getNotes().get(0).contains("FLASHCAT_DISABLED"));
        assertFalse(GlobalOpenTelemetry.isSet());
        TestSupport.reset(env("OTEL_SDK_DISABLED", "true"));
        d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").build());
        assertFalse(GlobalOpenTelemetry.isSet());
    }

    @Test
    void situationBExistingProviderNotExportingToFlashcat() {
        TestSupport.reset(flashcatEnv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://other-vendor.example.com:4318/v1/traces"));
        GlobalOpenTelemetry.set(OpenTelemetrySdk.builder().build());
        Diagnostics d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").build());
        Map<?, ?> provider = (Map<?, ?>) d.get("provider");
        assertEquals("B", provider.get("situation"));
        assertEquals("none", provider.get("exporter_mode"), "Java cannot add a processor after initialization, so auto falls back to none");
        assertTrue(d.getProblems().stream().anyMatch(p -> p.contains("不支持事后追加处理器") && p.contains("otel.javaagent.extensions")), d.toJson());
    }

    @Test
    void situationCAlreadyExportingToFlashcat() {
        TestSupport.reset(flashcatEnv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", ingest.tracesUrl()));
        GlobalOpenTelemetry.set(OpenTelemetrySdk.builder().build());
        Diagnostics d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("order-svc").build());
        Map<?, ?> provider = (Map<?, ?>) d.get("provider");
        assertEquals("C", provider.get("situation"));
        assertEquals("reuse", provider.get("exporter_mode"));
        assertTrue(d.isEnabled());
    }

    @Test
    void diagnosticsStructureMatchesContract() {
        TestSupport.reset(flashcatEnv());
        Diagnostics d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("kefu-agent").build());
        List<String> expected = Arrays.asList("sdk", "enabled", "service_name", "environment", "endpoint", "problems", "notes",
                "provider", "content", "instrumentations", "export", "semantics");
        assertEquals(expected, new ArrayList<String>(d.toMap().keySet()));
        @SuppressWarnings("unchecked")
        Map<String, Object> parsed = (Map<String, Object>) MiniJson.parse(d.toJson());
        assertEquals(expected, new ArrayList<String>(parsed.keySet()));
        Map<?, ?> sdk = (Map<?, ?>) parsed.get("sdk");
        assertEquals("flashcat-genai-otel", sdk.get("name"));
        assertEquals("0.1.0", sdk.get("version"));
        assertEquals("java", sdk.get("language"));
        assertEquals(Arrays.asList("situation", "exporter_mode", "metrics"), new ArrayList<Object>(((Map<?, ?>) parsed.get("provider")).keySet()));
        Map<?, ?> export = (Map<?, ?>) parsed.get("export");
        assertTrue(export.keySet().containsAll(Arrays.asList("ok", "network", "dropped_queue_full", "last_error")));
        Map<?, ?> sem = (Map<?, ?>) parsed.get("semantics");
        assertEquals(Arrays.asList("orphan_inference_spans", "nested_inference_spans", "masked_spans", "truncated_spans"), new ArrayList<Object>(sem.keySet()));
        for (Object o : (List<?>) parsed.get("instrumentations")) {
            Map<?, ?> m = (Map<?, ?>) o;
            assertTrue(Arrays.asList("upstream", "native", "openinference", "flashcat").contains(m.get("source")), m.toString());
            assertTrue(Arrays.asList("installed", "not_present", "disabled", "skipped_conflict", "skipped_version",
                    "instrumentation_not_installed", "already_instrumented", "failed").contains(m.get("status")), m.toString());
        }
    }

    @Test
    void diagnosticsBeforeInit() {
        TestSupport.reset(null);
        Diagnostics d = FlashcatGenAI.diagnostics();
        assertFalse(d.isEnabled());
        assertEquals(Arrays.asList("init 尚未调用"), d.getProblems());
    }

    @Test
    void shutdownOnlyClosesOwnProvider() {
        TestSupport.reset(flashcatEnv());
        FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").build());
        FlashcatGenAI.shutdown(Duration.ofSeconds(5));
        // The semantic API still executes the supplied callback after shutdown.
        assertEquals(42, (int) Flashcat.agent("a", () -> 42));
    }

    /**
     * Programmatic initialization honors {@code -Dotel.metrics.exporter=none}. OTel autoconfigure reads
     * {@code OTEL_METRICS_EXPORTER} into the same {@code ConfigProperties}. SpiTest covers the
     * environment variable there because Java cannot change the process environment at runtime.
     */
    @Test
    void explicitMetricsExporterNoneIsRespectedInCodeInit() {
        TestSupport.reset(flashcatEnv());
        System.setProperty("otel.metrics.exporter", "none");
        try {
            Diagnostics d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").build());
            assertEquals("disabled", ((Map<?, ?>) d.get("provider")).get("metrics"), d.toJson());
            assertTrue(d.isEnabled());
        } finally {
            System.clearProperty("otel.metrics.exporter");
        }
    }

    @Test
    void exportStatsHaveDroppedTimeout() {
        TestSupport.reset(flashcatEnv());
        Diagnostics d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").build());
        assertEquals(0L, ((Map<?, ?>) d.get("export")).get("dropped_timeout"), d.toJson());
    }
}
