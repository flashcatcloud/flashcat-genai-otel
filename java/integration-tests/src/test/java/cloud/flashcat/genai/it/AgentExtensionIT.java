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
import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;

/**
 * Runs a small app with the OpenTelemetry Java Agent (2.33.1) and flashcat-genai-otel-agent-extension.jar
 * against a mock ingest endpoint.
 *
 * <p>Verifies (a) gzip-compressed spans with Basic authentication, (b) export of application
 * spans created by {@code Flashcat.agent(...)}, and (c) inheritance of session attributes from
 * a local parent span by the Agent's HTTP client and openai-java child spans.
 */
class AgentExtensionIT {

    private ItSupport.Result runApp(FakeIngest ingest, ItSupport.FakeModel model, String classpath, Map<String, String> extraEnv) throws Exception {
        Map<String, String> env = new HashMap<String, String>();
        env.put("FLASHCAT_ENDPOINT", ingest.tracesUrl());
        env.put("FLASHCAT_API_KEY", "fck_e2e:s3cret");
        env.put("OTEL_SERVICE_NAME", "agent-e2e");
        env.put("OTEL_BSP_SCHEDULE_DELAY", "200");
        env.putAll(extraEnv);
        return ItSupport.runJava(
                list("-javaagent:" + prop("it.agentJar"), "-Dotel.javaagent.extensions=" + prop("it.extensionJar")),
                classpath, "cloud.flashcat.genai.it.AgentApp", list(model.baseUrl()), env, 180);
    }

    private static FakeIngest.ReceivedSpan find(List<FakeIngest.ReceivedSpan> spans, String name) {
        for (FakeIngest.ReceivedSpan s : spans) {
            if (s.name.equals(name)) {
                return s;
            }
        }
        return null;
    }

    private static FakeIngest.ReceivedSpan findHttp(List<FakeIngest.ReceivedSpan> spans, String pathSuffix) {
        for (FakeIngest.ReceivedSpan s : spans) {
            Object url = s.attr("url.full");
            if (url != null && url.toString().endsWith(pathSuffix)) {
                return s;
            }
        }
        return null;
    }

    @Test
    @SuppressWarnings("unchecked")
    void agentExtensionEndToEnd() throws Exception {
        try (FakeIngest ingest = new FakeIngest(); ItSupport.FakeModel model = new ItSupport.FakeModel()) {
            ItSupport.Result r = runApp(ingest, model, prop("it.agentApp.classpath"), new HashMap<String, String>());
            String out = r.output;
            assertEquals(0, r.exitCode, out);
            assertEquals("您的订单已发货", r.line("ANSWER:"), out);

            List<FakeIngest.ReceivedSpan> spans = ingest.awaitSpans(6, 10000);

            // (a) Basic authentication, gzip, and protobuf.
            List<FakeIngest.Request> reqs = ingest.requests("/v1/traces");
            assertFalse(reqs.isEmpty(), out);
            String expectedAuth = "Basic " + Base64.getEncoder().encodeToString("fck_e2e:s3cret".getBytes(StandardCharsets.UTF_8));
            for (FakeIngest.Request q : reqs) {
                assertEquals(expectedAuth, q.header("Authorization"));
                assertTrue(q.gzip, "Expected gzip compression");
                assertEquals("application/x-protobuf", q.header("Content-Type"));
            }

            // (b) Flashcat.agent(...) spans reach the endpoint through the Agent's API bridge.
            FakeIngest.ReceivedSpan agent = find(spans, "invoke_agent kefu");
            assertNotNull(agent, "spans=" + spans + "\n" + out);
            assertEquals("flashcat.genai", agent.scope);
            assertEquals("0.2.0", agent.scopeVersion);
            assertEquals("invoke_agent", agent.attr("gen_ai.operation.name"));
            assertEquals(true, agent.attr("flashcat.genai.root"));
            assertEquals("c-agent", agent.attr("gen_ai.conversation.id"));
            assertEquals("agent-e2e", agent.resource.get("service.name"));
            assertEquals("flashcat-genai-otel", agent.resource.get("telemetry.distro.name"));
            assertEquals("prod", agent.resource.get("deployment.environment.name"));

            FakeIngest.ReceivedSpan tool = find(spans, "execute_tool lookup_order");
            assertNotNull(tool);
            assertEquals(agent.spanId, tool.parentSpanId);
            assertEquals(false, tool.attr("flashcat.genai.root"));

            // (c) The Agent-instrumented HTTP client and openai-java chat spans inherit session attributes.
            FakeIngest.ReceivedSpan http = findHttp(spans, "/ping-inside");
            assertNotNull(http, "Missing HTTP child span: " + spans);
            assertEquals(agent.spanId, http.parentSpanId);
            assertEquals("c-agent", http.attr("gen_ai.conversation.id"), http.toString());
            assertEquals("u-agent", http.attr("user.id"));
            assertEquals("t1", http.attr("flashcat.tag.tenant"));
            assertNull(http.attr("flashcat.genai.root"));

            FakeIngest.ReceivedSpan manual = find(spans, "manual-child");
            assertNotNull(manual);
            assertEquals("c-agent", manual.attr("gen_ai.conversation.id"));
            assertNull(manual.attr("flashcat.genai.root"));

            FakeIngest.ReceivedSpan late = find(spans, "late-model");
            assertNotNull(late);
            assertEquals(manual.spanId, late.parentSpanId);
            assertEquals(false, late.attr("flashcat.genai.root"));

            FakeIngest.ReceivedSpan chat = null;
            for (FakeIngest.ReceivedSpan s : spans) {
                if ("chat".equals(s.attr("gen_ai.operation.name")) && agent.spanId.equals(s.parentSpanId)) {
                    chat = s;
                }
            }
            assertNotNull(chat, "Missing openai-java chat span (2.20.1 is within the Agent's instrumentation range [1.1.0,3)): " + spans);
            assertEquals(agent.spanId, chat.parentSpanId);
            assertEquals("io.opentelemetry.openai-java-1.1", chat.scope);
            assertEquals(false, chat.attr("flashcat.genai.root"));
            assertEquals("c-agent", chat.attr("gen_ai.conversation.id"));
            assertEquals("u-agent", chat.attr("user.id"));
            assertEquals(120L, chat.attr("gen_ai.usage.input_tokens"));
            assertFalse(chat.attrs.containsKey("gen_ai.input.messages"), "Content capture is off by default");

            // As documented in the README, Agent-instrumented spans need an enclosing Flashcat span to inherit session attributes.
            FakeIngest.ReceivedSpan outside = findHttp(spans, "/ping-outside");
            assertNotNull(outside);
            assertNull(outside.attr("gen_ai.conversation.id"));

            // Application init detects the Agent and extension and reuses the provider (scenario C in contract §2.4).
            Map<String, Object> init = (Map<String, Object>) MiniJson.parse(r.line("INIT:"));
            assertEquals("C", ((Map<String, Object>) init.get("provider")).get("situation"), out);
            assertEquals("reuse", ((Map<String, Object>) init.get("provider")).get("exporter_mode"));

            // Application diagnostics read live extension counters through the MBean.
            assertEquals("true", r.line("FLUSH:"), out);
            Map<String, Object> diag = (Map<String, Object>) MiniJson.parse(r.line("RESULT:"));
            assertEquals("java", ((Map<String, Object>) diag.get("sdk")).get("language"));
            assertEquals("add", ((Map<String, Object>) diag.get("provider")).get("exporter_mode"), out);
            assertTrue(((Number) ((Map<String, Object>) diag.get("export")).get("ok")).longValue() >= 5, out);
            assertTrue(((List<Object>) diag.get("notes")).toString().contains("Java Agent 里的 Flashcat 扩展"), out);
            Map<String, Object> openai = null;
            for (Object o : (List<Object>) diag.get("instrumentations")) {
                Map<String, Object> m = (Map<String, Object>) o;
                if ("openai-java".equals(m.get("lib"))) {
                    openai = m;
                }
            }
            assertEquals("installed", openai.get("status"), out);
            assertEquals("2.20.1", openai.get("lib_version"));

            // Under the Agent, annotate still finds the manual llm span in the application Context through the bridge.
            final FakeIngest.ReceivedSpan gw = find(spans, "chat gateway-model");
            assertNotNull(gw, "spans=" + spans);
            assertEquals("flashcat.genai", gw.scope);
            assertEquals("custom", gw.attr("gen_ai.provider.name"));
            assertEquals(11L, gw.attr("gen_ai.usage.input_tokens"));
            assertEquals(2L, gw.attr("gen_ai.usage.output_tokens"));
            assertEquals("gateway-model-v1", gw.attr("gen_ai.response.model"));
            assertEquals("gw", gw.attr("flashcat.tag.route"));
            boolean nestedChild = false;
            for (FakeIngest.ReceivedSpan s : spans) {
                if ("chat".equals(s.attr("gen_ai.operation.name")) && gw.spanId.equals(s.parentSpanId)) {
                    nestedChild = true;
                }
            }
            assertTrue(nestedChild, "The openai-java call should be a child of the llm span: " + spans);
            // The extension's session processor identifies nested inference from the parent span's scope and operation name.
            assertEquals(1L, ((Number) ((Map<String, Object>) diag.get("semantics")).get("nested_inference_spans")).longValue(), out);
            assertTrue(((List<Object>) diag.get("problems")).toString().contains("重复计数"), out);
        }
    }

    @Test
    @SuppressWarnings("unchecked")
    void contentSwitchReachesAgentAndAnthropicProperty() throws Exception {
        try (FakeIngest ingest = new FakeIngest(); ItSupport.FakeModel model = new ItSupport.FakeModel()) {
            Map<String, String> env = new HashMap<String, String>();
            env.put("FLASHCAT_CAPTURE_CONTENT", "span");
            ItSupport.Result r = runApp(ingest, model, prop("it.agentApp.classpath"), env);
            assertEquals(0, r.exitCode, r.output);
            Map<String, Object> diag = (Map<String, Object>) MiniJson.parse(r.line("RESULT:"));
            Map<String, Object> content = (Map<String, Object>) diag.get("content");
            assertEquals("SPAN_ONLY", content.get("upstream"), r.output);
            assertEquals("content", content.get("anthropic"));
            ingest.awaitSpans(6, 10000);
            FakeIngest.ReceivedSpan tool = find(ingest.spans(), "execute_tool lookup_order");
            assertEquals("shipped", tool.attr("gen_ai.tool.call.result"), "The application semantic API reads the content capture switch from the extension's system property");
        }
    }

    /**
     * Under the Agent, application init ignores explicit content capture settings. This keeps
     * the semantic API from writing content when the extension's L2 masking is off.
     */
    @Test
    @SuppressWarnings("unchecked")
    void explicitContentOptionIsIgnoredUnderAgent() throws Exception {
        try (FakeIngest ingest = new FakeIngest(); ItSupport.FakeModel model = new ItSupport.FakeModel()) {
            Map<String, String> env = new HashMap<String, String>();
            env.put("IT_EXPLICIT_CONTENT", "span");
            ItSupport.Result r = runApp(ingest, model, prop("it.agentApp.classpath"), env);
            assertEquals(0, r.exitCode, r.output);
            Map<String, Object> init = (Map<String, Object>) MiniJson.parse(r.line("INIT:"));
            assertTrue(init.get("notes").toString().contains("显式参数不生效"), r.output);
            ingest.awaitSpans(6, 10000);
            FakeIngest.ReceivedSpan tool = find(ingest.spans(), "execute_tool lookup_order");
            assertNotNull(tool);
            assertNull(tool.attr("gen_ai.tool.call.result"), "The application omits content when capture is disabled in the Agent process");
        }
    }

    /**
     * Agent 2.33.1 declares a muzzle range of [1.1.0,3) without {@code assertInverse}.
     * The test verifies that openai-java 4.101.0 still emits a chat span and inherits session attributes.
     */
    @Test
    @SuppressWarnings("unchecked")
    void openaiJavaLatestMajorIsInstrumented() throws Exception {
        try (FakeIngest ingest = new FakeIngest(); ItSupport.FakeModel model = new ItSupport.FakeModel()) {
            ItSupport.Result r = runApp(ingest, model, prop("it.agentAppLatestOpenai.classpath"), new HashMap<String, String>());
            assertEquals(0, r.exitCode, r.output);
            List<FakeIngest.ReceivedSpan> spans = ingest.awaitSpans(5, 10000);
            boolean hasChat = false;
            for (FakeIngest.ReceivedSpan s : spans) {
                if ("chat".equals(s.attr("gen_ai.operation.name"))) {
                    hasChat = true;
                }
            }
            System.out.println("openai-java 4.x: chat span=" + hasChat + " ANSWER=" + r.line("ANSWER:"));
            Map<String, Object> diag = (Map<String, Object>) MiniJson.parse(r.line("RESULT:"));
            Map<String, Object> openai = null;
            for (Object o : (List<Object>) diag.get("instrumentations")) {
                Map<String, Object> m = (Map<String, Object>) o;
                if ("openai-java".equals(m.get("lib"))) {
                    openai = m;
                }
            }
            assertTrue(hasChat, "Missing openai-java 4.x chat span: " + spans + "\n" + r.output);
            assertEquals("您的订单已发货", r.line("ANSWER:"));
            assertEquals("installed", openai.get("status"), r.output);
            assertEquals("4.101.0", openai.get("lib_version"));
        }
    }

    @Test
    void http401UnderAgentIsCounted() throws Exception {
        try (FakeIngest ingest = new FakeIngest(); ItSupport.FakeModel model = new ItSupport.FakeModel()) {
            ingest.setStatus(401, "invalid api key");
            ItSupport.Result r = runApp(ingest, model, prop("it.agentApp.classpath"), new HashMap<String, String>());
            assertEquals(0, r.exitCode, r.output);
            String diag = r.line("RESULT:");
            assertTrue(diag.contains("\"http_401\""), r.output);
            assertTrue(diag.contains("HTTP 401"), r.output);
        }
    }
}
