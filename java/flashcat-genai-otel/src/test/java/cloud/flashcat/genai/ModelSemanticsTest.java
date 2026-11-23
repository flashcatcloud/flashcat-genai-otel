package cloud.flashcat.genai;

import static cloud.flashcat.genai.TestSupport.env;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import cloud.flashcat.genai.testing.FakeIngest;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/** Tests manual model call spans (llm, embedding, retrieval) and annotate from contract §4, §4.1, and §4.2. */
class ModelSemanticsTest {
    private InMemorySpanExporter exporter;
    private SessionSpanProcessor session;

    @BeforeEach
    void setUp() {
        TestSupport.reset(null);
        exporter = InMemorySpanExporter.create();
        session = new SessionSpanProcessor();
        SdkTracerProvider tp = SdkTracerProvider.builder()
                .addSpanProcessor(session)
                .addSpanProcessor(SimpleSpanProcessor.create(exporter))
                .build();
        GlobalOpenTelemetry.set(OpenTelemetrySdk.builder().setTracerProvider(tp).build());
    }

    @AfterEach
    void tearDown() {
        TestSupport.reset(null);
    }

    private SpanData span(String name) {
        for (SpanData s : exporter.getFinishedSpanItems()) {
            if (s.getName().equals(name)) {
                return s;
            }
        }
        throw new AssertionError("Missing span " + name + ". Finished spans: " + exporter.getFinishedSpanItems());
    }

    private static String attr(SpanData s, String key) {
        return s.getAttributes().get(AttributeKey.stringKey(key));
    }

    private static Long count(SpanData s, String key) {
        return s.getAttributes().get(AttributeKey.longKey(key));
    }

    @Test
    void modelSpansAndAnnotate() throws Exception {
        ContentSwitch.setResolved(true);
        final Map<String, Object> doc = new LinkedHashMap<String, Object>();
        doc.put("id", "d1");
        doc.put("content", "订单已发货");
        Flashcat.agent("kefu", () -> {
            Flashcat.embedding("text-embedding-v3", Flashcat.SpanOptions.create().provider("dashscope"), () -> {
                Flashcat.annotate(Flashcat.Annotation.create().inputTokens(8).outputTokens(99).model("text-embedding-v3"));
            });
            Flashcat.retrieval("kb-orders", Flashcat.SpanOptions.create().input("我的订单到哪了"),
                    () -> Arrays.<Object>asList(doc, "第二段文本"));
            return Flashcat.llm("qwen-max", Flashcat.SpanOptions.create().provider("dashscope"), () -> {
                Flashcat.annotate(Flashcat.Annotation.create()
                        .input(Arrays.asList(Flashcat.Message.of("system", "你是客服"), Flashcat.Message.of("user", "我的订单到哪了")))
                        .output("已为您查询")
                        .model("qwen-max-0919")
                        .inputTokens(30).outputTokens(5).cacheReadInputTokens(10).cacheWriteInputTokens(2)
                        .finishReasons("stop")
                        .tag("route", "gateway"));
                return "annotate 写过 output，返回值不覆盖";
            });
        });

        SpanData chat = span("chat qwen-max");
        assertEquals(SpanKind.CLIENT, chat.getKind());
        assertEquals(span("invoke_agent kefu").getSpanId(), chat.getParentSpanId());
        assertEquals("chat", attr(chat, "gen_ai.operation.name"));
        assertEquals("qwen-max", attr(chat, "gen_ai.request.model"));
        assertEquals("dashscope", attr(chat, "gen_ai.provider.name"));
        assertEquals("qwen-max-0919", attr(chat, "gen_ai.response.model"));
        assertEquals(30L, count(chat, "gen_ai.usage.input_tokens"));
        assertEquals(5L, count(chat, "gen_ai.usage.output_tokens"));
        assertEquals(10L, count(chat, "gen_ai.usage.cache_read.input_tokens"));
        assertEquals(2L, count(chat, "gen_ai.usage.cache_write.input_tokens"));
        assertEquals(Collections.singletonList("stop"),
                chat.getAttributes().get(AttributeKey.stringArrayKey("gen_ai.response.finish_reasons")));
        assertEquals("gateway", attr(chat, "flashcat.tag.route"));
        assertEquals("[{\"role\":\"system\",\"parts\":[{\"type\":\"text\",\"content\":\"你是客服\"}]},"
                + "{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"我的订单到哪了\"}]}]",
                attr(chat, "gen_ai.input.messages"));
        assertEquals("[{\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"content\":\"已为您查询\"}]}]",
                attr(chat, "gen_ai.output.messages"));

        SpanData emb = span("embeddings text-embedding-v3");
        assertEquals(SpanKind.CLIENT, emb.getKind());
        assertEquals(8L, count(emb, "gen_ai.usage.input_tokens"));
        assertNull(count(emb, "gen_ai.usage.output_tokens"), "Embedding spans record input tokens only");
        assertNull(attr(emb, "gen_ai.input.messages"), "Embedding spans omit content");

        SpanData ret = span("retrieval kb-orders");
        assertEquals(SpanKind.CLIENT, ret.getKind());
        assertEquals("kb-orders", attr(ret, "gen_ai.data_source.id"));
        assertEquals("我的订单到哪了", attr(ret, "gen_ai.retrieval.query.text"));
        assertEquals("[{\"id\": \"d1\", \"content\": \"订单已发货\"}, {\"content\": \"第二段文本\"}]",
                attr(ret, "gen_ai.retrieval.documents"));
    }

    @Test
    void annotateRespectsContentSwitchAndScope() throws Exception {
        ContentSwitch.setResolved(false);
        Flashcat.annotate(Flashcat.Annotation.create().output("不在语义 span 里").tag("a", "b")); // No-op.
        Flashcat.annotate(null);
        Flashcat.trace("job", () -> {
            Flashcat.annotate(Flashcat.Annotation.create().tag("plain", "1")); // Trace spans are outside the scope of annotate.
            Flashcat.llm("m1", () -> {
                Flashcat.annotate(Flashcat.Annotation.create().input("secret prompt").output("secret answer").inputTokens(3)
                        .outputTokens(-1));
            });
            Flashcat.tool("t1", () -> {
                Flashcat.annotate(Flashcat.Annotation.create().output("ignored: content off"));
                return 1;
            });
        });
        SpanData chat = span("chat m1");
        assertEquals(3L, count(chat, "gen_ai.usage.input_tokens"));
        assertNull(count(chat, "gen_ai.usage.output_tokens"), "Negative token counts are ignored");
        assertNull(attr(chat, "gen_ai.input.messages"));
        assertNull(attr(chat, "gen_ai.output.messages"));
        assertNull(attr(span("execute_tool t1"), "gen_ai.tool.call.result"));
        assertNull(attr(span("job"), "flashcat.tag.plain"));
    }

    @Test
    void annotatedOutputWinsAndDocumentsStayValid() throws Exception {
        ContentSwitch.setResolved(true);
        Flashcat.tool("lookup", () -> {
            Flashcat.annotate(Flashcat.Annotation.create().output(Collections.singletonMap("status", "shipped")));
            return "raw";
        });
        Flashcat.retrieval("kb", () -> "只有一段");
        FlashcatSpan big = Flashcat.startRetrieval("big");
        List<Map<String, String>> docs = new ArrayList<Map<String, String>>();
        char[] x = new char[2000];
        Arrays.fill(x, 'x');
        for (int i = 0; i < 50; i++) {
            docs.add(Collections.singletonMap("content", new String(x)));
        }
        big.setOutput(docs);
        big.end();
        FlashcatSpan handle = Flashcat.startLLM("m2", Flashcat.SpanOptions.create().provider("openai"));
        handle.annotate(Flashcat.Annotation.create().inputTokens(1).model("m2-resp"));
        handle.end();

        assertEquals("{\"status\": \"shipped\"}", attr(span("execute_tool lookup"), "gen_ai.tool.call.result"));
        assertEquals("[{\"content\": \"只有一段\"}]", attr(span("retrieval kb"), "gen_ai.retrieval.documents"));
        String b = attr(span("retrieval big"), "gen_ai.retrieval.documents");
        assertTrue(b.length() <= Semantics.MAX_SERIALIZED + 64, String.valueOf(b.length()));
        assertTrue(b.endsWith("{\"content\": \"…[truncated]\"}]"), b.substring(b.length() - 40));
        SpanData m2 = span("chat m2");
        assertEquals("openai", attr(m2, "gen_ai.provider.name"));
        assertEquals("m2-resp", attr(m2, "gen_ai.response.model"));
        assertEquals(1L, count(m2, "gen_ai.usage.input_tokens"));
    }

    @Test
    void nestedInferenceUnderManualLlmIsCounted() throws Exception {
        final Tracer other = GlobalOpenTelemetry.getTracer("io.opentelemetry.openai-java-1.1");
        Flashcat.llm("gpt-4o", () -> {
            other.spanBuilder("chat gpt-4o").setAttribute("gen_ai.operation.name", "chat").startSpan().end();
            other.spanBuilder("ChatCompletion").setAttribute("openinference.span.kind", "LLM").startSpan().end();
            // Nested inference detection requires a direct parent.
            Span mid = other.spanBuilder("http").startSpan();
            try (io.opentelemetry.context.Scope ignored = mid.makeCurrent()) {
                other.spanBuilder("chat deeper").setAttribute("gen_ai.operation.name", "chat").startSpan().end();
            } finally {
                mid.end();
            }
        });
        other.spanBuilder("chat outside").setAttribute("gen_ai.operation.name", "chat").startSpan().end();
        assertEquals(2L, session.nested.get());
    }

    @Test
    void nestedInferenceIsReportedInDiagnosticsAndRetrievalContentIsMasked() throws Exception {
        try (FakeIngest ingest = new FakeIngest()) {
            TestSupport.reset(env("FLASHCAT_ENDPOINT", ingest.tracesUrl(), "FLASHCAT_API_KEY", "fck_test:s3cret"));
            FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("x").captureContent("span").metrics(false).build());
            final Tracer other = GlobalOpenTelemetry.getTracer("io.opentelemetry.openai-java-1.1");
            Flashcat.llm("gpt-4o", () -> {
                other.spanBuilder("chat gpt-4o").setAttribute("gen_ai.operation.name", "chat").startSpan().end();
            });
            Flashcat.retrieval("kb", () -> Collections.singletonList("联系人 13812345678"));
            FlashcatGenAI.flush(Duration.ofSeconds(10));

            Map<?, ?> sem = (Map<?, ?>) FlashcatGenAI.diagnostics().get("semantics");
            assertEquals(1L, ((Number) sem.get("nested_inference_spans")).longValue());
            @SuppressWarnings("unchecked")
            List<String> problems = (List<String>) FlashcatGenAI.diagnostics().get("problems");
            int n = 0;
            for (String p : problems) {
                if (p.contains("重复计数")) {
                    n++;
                }
            }
            assertEquals(1, n, String.valueOf(problems));
            String docs = (String) ingest.spanNamed("retrieval kb").attr("gen_ai.retrieval.documents");
            assertFalse(docs.contains("13812345678"), docs);
            assertTrue(docs.contains("[REDACTED:phone]"), docs);
        }
    }
}
