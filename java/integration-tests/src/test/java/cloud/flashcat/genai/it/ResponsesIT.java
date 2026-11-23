package cloud.flashcat.genai.it;

import static cloud.flashcat.genai.it.ItSupport.list;
import static cloud.flashcat.genai.it.ItSupport.prop;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import cloud.flashcat.genai.testing.FakeIngest;
import cloud.flashcat.genai.testing.MiniJson;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;

/**
 * Verifies the extension's openai-java Responses instrumentation with OpenTelemetry Java Agent
 * 2.33.1. Synchronous, streaming (exhausted or closed early), failed, asynchronous, asynchronous
 * streaming, and {@code fromEnv()} calls each emit a chat span with IDs and token counts matching
 * the mock server. For streaming Chat Completions calls through the upstream proxy, the extension
 * adds time to first token (TTFT) to the existing span. It creates spans for calls that bypass
 * the proxy. Runs with both openai-java 2.x and 4.x.
 */
class ResponsesIT {
    private static final int SPAN_KIND_CLIENT = 3;
    private static final int STATUS_ERROR = 2;

    private ItSupport.Result runApp(FakeIngest ingest, ItSupport.FakeModel model, String classpath, Map<String, String> extraEnv) throws Exception {
        Map<String, String> env = new HashMap<String, String>();
        env.put("FLASHCAT_ENDPOINT", ingest.tracesUrl());
        env.put("FLASHCAT_API_KEY", "fck_e2e:s3cret");
        env.put("OTEL_SERVICE_NAME", "responses-e2e");
        env.put("OTEL_BSP_SCHEDULE_DELAY", "200");
        env.put("OPENAI_BASE_URL", model.baseUrl());
        env.put("OPENAI_API_KEY", "sk-test");
        env.putAll(extraEnv);
        return ItSupport.runJava(
                list("-javaagent:" + prop("it.agentJar"), "-Dotel.javaagent.extensions=" + prop("it.extensionJar")),
                classpath, "cloud.flashcat.genai.it.ResponsesApp", list(model.baseUrl()), env, 180);
    }

    private static List<FakeIngest.ReceivedSpan> responsesSpans(List<FakeIngest.ReceivedSpan> spans) {
        List<FakeIngest.ReceivedSpan> out = new ArrayList<FakeIngest.ReceivedSpan>();
        for (FakeIngest.ReceivedSpan s : spans) {
            if ("responses".equals(s.attr("openai.api.type"))) {
                out.add(s);
            }
        }
        return out;
    }

    private static FakeIngest.ReceivedSpan byResponseId(List<FakeIngest.ReceivedSpan> spans, String id) {
        for (FakeIngest.ReceivedSpan s : spans) {
            if (id.equals(s.attr("gen_ai.response.id"))) {
                return s;
            }
        }
        return null;
    }

    private static FakeIngest.ReceivedSpan named(List<FakeIngest.ReceivedSpan> spans, String name) {
        for (FakeIngest.ReceivedSpan s : spans) {
            if (s.name.equals(name)) {
                return s;
            }
        }
        return null;
    }

    @Test
    void responsesOnOpenaiJava2() throws Exception {
        verify(prop("it.agentApp.classpath"), "2.20.1");
    }

    @Test
    void responsesOnOpenaiJava4() throws Exception {
        verify(prop("it.agentAppLatestOpenai.classpath"), "4.101.0");
    }

    @SuppressWarnings("unchecked")
    private void verify(String classpath, String openaiVersion) throws Exception {
        try (FakeIngest ingest = new FakeIngest(); ItSupport.FakeModel model = new ItSupport.FakeModel()) {
            ItSupport.Result r = runApp(ingest, model, classpath, new HashMap<String, String>());
            String out = r.output;
            assertEquals(0, r.exitCode, out);
            assertEquals("订单 已 发货", r.line("STREAM_TEXT:"), out);
            assertEquals("已发货", r.line("CHAT_TEXT:"), out);
            assertEquals("com.openai.errors.RateLimitException", r.line("STEP:error:"), "The caller should receive the original exception: " + out);

            List<FakeIngest.ReceivedSpan> all = ingest.awaitSpans(16, 15000);
            List<FakeIngest.ReceivedSpan> spans = responsesSpans(all);
            FakeIngest.ReceivedSpan agent = named(all, "invoke_agent responses-agent");
            assertNotNull(agent, all.toString());
            assertEquals(true, agent.attr("flashcat.genai.root"));

            // Expect exactly one span for each of the seven Responses calls, including the failure.
            assertEquals(7, spans.size(), spans + "\n" + out);
            for (FakeIngest.ReceivedSpan s : spans) {
                assertEquals("flashcat.genai.openai-java", s.scope);
                assertEquals("0.2.0", s.scopeVersion);
                assertEquals("chat gpt-4o-mini", s.name);
                assertEquals(SPAN_KIND_CLIENT, s.kind);
                assertEquals(agent.spanId, s.parentSpanId, "The model span should be a child of the agent span");
                assertEquals(false, s.attr("flashcat.genai.root"));
                assertEquals("chat", s.attr("gen_ai.operation.name"));
                assertEquals("openai", s.attr("gen_ai.provider.name"));
                assertEquals("gpt-4o-mini", s.attr("gen_ai.request.model"));
                assertEquals(64L, s.attr("gen_ai.request.max_tokens"));
                assertEquals(0.2, (Double) s.attr("gen_ai.request.temperature"), 1e-9);
                assertEquals("127.0.0.1", s.attr("server.address"));
                assertEquals((long) model.server.getAddress().getPort(), s.attr("server.port"));
                // Session attributes are inherited from the local parent span.
                assertEquals("c-resp", s.attr("gen_ai.conversation.id"));
                assertEquals("u-resp", s.attr("user.id"));
                assertFalse(s.attrs.containsKey("gen_ai.input.messages"), "Content capture is off by default");
            }

            // Match each successful call's response ID and token counts to the mock server's values.
            for (String step : Arrays.asList("sync", "stream", "async", "async-stream", "from-env")) {
                String id = r.line("STEP:" + step + ":");
                ItSupport.FakeResponse fr = model.response(id);
                assertNotNull(fr, step + " id=" + id + "\n" + out);
                FakeIngest.ReceivedSpan s = byResponseId(spans, id);
                assertNotNull(s, step + " has no matching span: " + spans);
                assertEquals(fr.inputTokens, s.attr("gen_ai.usage.input_tokens"), step);
                assertEquals(fr.outputTokens, s.attr("gen_ai.usage.output_tokens"), step);
                assertEquals(fr.cachedTokens, s.attr("gen_ai.usage.cache_read.input_tokens"), step);
                assertEquals("gpt-4o-mini-2024-07-18", s.attr("gen_ai.response.model"), step);
                assertEquals(Arrays.asList("stop"), s.attr("gen_ai.response.finish_reasons"), step);
                assertEquals(fr.stream, s.attr("gen_ai.request.stream"), step);
                assertTrue(s.statusCode != STATUS_ERROR, step);
                if (fr.stream) {
                    // The server waits 120 ms after response.created before sending the first text delta.
                    double ttft = (Double) s.attr("gen_ai.response.time_to_first_chunk");
                    assertTrue(ttft >= 0.1 && ttft < 5, step + " ttft=" + ttft);
                } else {
                    assertNull(s.attr("gen_ai.response.time_to_first_chunk"), step);
                }
            }

            // Closing early still ends the span and marks the stream as stream_aborted.
            FakeIngest.ReceivedSpan early = byResponseId(spans, r.line("STEP:early:"));
            assertNotNull(early, "Closing the stream early should end the span: " + spans);
            assertEquals(STATUS_ERROR, early.statusCode);
            assertEquals("stream_aborted", early.attr("error.type"));

            // On error, set ERROR, use the HTTP status code as error.type (contract §4), and record an exception event.
            FakeIngest.ReceivedSpan error = null;
            for (FakeIngest.ReceivedSpan s : spans) {
                if ("429".equals(s.attr("error.type"))) {
                    error = s;
                }
            }
            assertNotNull(error, spans.toString());
            assertEquals(STATUS_ERROR, error.statusCode);
            assertTrue(error.eventNames.contains("exception"), error.toString());

            // OkHttp HTTP spans are children of the model span.
            FakeIngest.ReceivedSpan sync = byResponseId(spans, r.line("STEP:sync:"));
            boolean httpChild = false;
            for (FakeIngest.ReceivedSpan s : all) {
                if (sync.spanId.equals(s.parentSpanId) && s.scope.startsWith("io.opentelemetry.okhttp")) {
                    httpChild = true;
                }
            }
            assertTrue(httpChild, "The HTTP span should be a child of the model span: " + all);

            // Expect one span for each of the five Chat Completions calls. Proxied streaming and async create calls have upstream spans.
            // The extension adds TTFT to upstream streaming spans and creates spans for withOptions clients and withRawResponse() calls.
            List<FakeIngest.ReceivedSpan> chats = new ArrayList<FakeIngest.ReceivedSpan>();
            for (FakeIngest.ReceivedSpan s : all) {
                if ("chat".equals(s.attr("gen_ai.operation.name")) && !"responses".equals(s.attr("openai.api.type"))) {
                    chats.add(s);
                }
            }
            int chatRequests = 0;
            for (String p : model.paths) {
                if (p.equals("POST /v1/chat/completions")) {
                    chatRequests++;
                }
            }
            assertEquals(5, chatRequests, model.paths.toString());
            assertEquals(chatRequests, chats.size(), "Expected one span per Chat Completions call: " + chats);
            List<FakeIngest.ReceivedSpan> upstream = new ArrayList<FakeIngest.ReceivedSpan>();
            List<FakeIngest.ReceivedSpan> fallback = new ArrayList<FakeIngest.ReceivedSpan>();
            for (FakeIngest.ReceivedSpan s : chats) {
                (s.scope.equals("io.opentelemetry.openai-java-1.1") ? upstream : fallback).add(s);
            }
            assertEquals(2, upstream.size(), chats.toString());
            FakeIngest.ReceivedSpan chat = Boolean.TRUE.equals(upstream.get(0).attr("gen_ai.request.stream")) ? upstream.get(0) : upstream.get(1);
            assertEquals(true, chat.attr("gen_ai.request.stream"));
            double ttft = (Double) chat.attr("gen_ai.response.time_to_first_chunk");
            assertTrue(ttft >= 0.1 && ttft < 5, "chat ttft=" + ttft);
            assertEquals(3, fallback.size(), chats.toString());
            int fallbackStreams = 0;
            for (FakeIngest.ReceivedSpan s : fallback) {
                assertEquals("flashcat.genai.openai-java", s.scope);
                assertEquals("chat_completions", s.attr("openai.api.type"));
                assertEquals(SPAN_KIND_CLIENT, s.kind);
                assertEquals(agent.spanId, s.parentSpanId);
                assertEquals("c-resp", s.attr("gen_ai.conversation.id"));
                assertEquals(Arrays.asList("stop"), s.attr("gen_ai.response.finish_reasons"));
                if (Boolean.TRUE.equals(s.attr("gen_ai.request.stream"))) {
                    fallbackStreams++;
                    double t = (Double) s.attr("gen_ai.response.time_to_first_chunk");
                    assertTrue(t >= 0.1 && t < 5, "fallback ttft=" + t);
                } else {
                    assertEquals(120L, s.attr("gen_ai.usage.input_tokens"));
                    assertEquals("chatcmpl-test", s.attr("gen_ai.response.id"));
                }
            }
            assertEquals(1, fallbackStreams);
            assertEquals("chatcmpl-test", r.line("CHAT:raw:"));
            assertEquals("已发货", r.line("CHAT:derived-stream:"));

            // Diagnostics report the status of Responses instrumentation.
            Map<String, Object> diag = (Map<String, Object>) MiniJson.parse(r.line("RESULT:"));
            Map<String, Object> resp = null;
            for (Object o : (List<Object>) diag.get("instrumentations")) {
                Map<String, Object> m = (Map<String, Object>) o;
                if ("openai-java".equals(m.get("lib")) && "responses".equals(m.get("api"))) {
                    resp = m;
                }
            }
            assertNotNull(resp, diag.toString());
            assertEquals("flashcat", resp.get("source"));
            assertEquals("installed", resp.get("status"), out);
            assertEquals(openaiVersion, resp.get("lib_version"));
            assertTrue(((List<Object>) diag.get("problems")).isEmpty(), out);
        }
    }

    /** With content capture on, span attributes use the format from contract §4 and are masked by the Flashcat export pipeline. */
    @Test
    void contentCaptureWritesMessagesAndIsMasked() throws Exception {
        try (FakeIngest ingest = new FakeIngest(); ItSupport.FakeModel model = new ItSupport.FakeModel()) {
            Map<String, String> env = new HashMap<String, String>();
            env.put("FLASHCAT_CAPTURE_CONTENT", "span");
            ItSupport.Result r = runApp(ingest, model, prop("it.agentAppLatestOpenai.classpath"), env);
            assertEquals(0, r.exitCode, r.output);
            List<FakeIngest.ReceivedSpan> spans = responsesSpans(ingest.awaitSpans(12, 15000));
            FakeIngest.ReceivedSpan sync = byResponseId(spans, r.line("STEP:sync:"));
            assertNotNull(sync, spans.toString());
            String input = (String) sync.attr("gen_ai.input.messages");
            assertEquals("[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"sync 我的订单到哪了 [REDACTED:phone]\"}]}]", input);
            assertEquals("[{\"type\":\"text\",\"content\":\"回答要简短\"}]", sync.attr("gen_ai.system_instructions"));
            assertEquals("[{\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"content\":\"订单 已 发货\"}],\"finish_reason\":\"stop\"}]",
                    sync.attr("gen_ai.output.messages"));
            FakeIngest.ReceivedSpan stream = byResponseId(spans, r.line("STEP:stream:"));
            assertEquals("[{\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"content\":\"订单 已 发货\"}],\"finish_reason\":\"stop\"}]",
                    stream.attr("gen_ai.output.messages"));
        }
    }
}
