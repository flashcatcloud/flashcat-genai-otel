package cloud.flashcat.otel.it;

import static cloud.flashcat.otel.it.ItSupport.list;
import static cloud.flashcat.otel.it.ItSupport.prop;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import cloud.flashcat.otel.testing.FakeIngest;
import cloud.flashcat.otel.testing.MiniJson;
import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;

/**
 * Exercises Spring Boot 4.1 with OpenTelemetry Spring Boot Starter 2.33.1 and flashcat-otel,
 * without the Java Agent. The Starter's ComponentLoader loads the SPI, and flashcat-otel sets
 * GlobalOpenTelemetry on the Starter's behalf. Runs with a minimal spring-boot-starter classpath
 * and with actuator and micrometer-tracing-bridge-otel added.
 */
class SpringStarterIT {

    @ParameterizedTest
    @ValueSource(strings = {"it.springApp.classpath", "it.springAppActuator.classpath"})
    @SuppressWarnings("unchecked")
    void springBootStarterPath(String classpathProp) throws Exception {
        try (FakeIngest ingest = new FakeIngest()) {
            Map<String, String> env = new HashMap<String, String>();
            env.put("FLASHCAT_ENDPOINT", ingest.tracesUrl());
            env.put("FLASHCAT_API_KEY", "fck_spring:s3cret");
            env.put("OTEL_BSP_SCHEDULE_DELAY", "200");
            ItSupport.Result r = ItSupport.runJava(list(), prop(classpathProp), "cloud.flashcat.otel.it.spring.SpringApp",
                    list("--spring.application.name=spring-kefu"), env, 180);
            String out = r.output;
            assertEquals(0, r.exitCode, out);
            System.out.println(classpathProp + " BEAN=" + r.line("BEAN:") + " MICROMETER_TRACER=" + r.line("MICROMETER_TRACER:"));
            assertEquals("io.opentelemetry.instrumentation.spring.autoconfigure.SpringOpenTelemetrySdk", r.line("BEAN:"), out);
            assertEquals("true", r.line("GLOBAL_SET:"), "flashcat-otel should register the Starter's SDK globally:\n" + out);

            List<FakeIngest.ReceivedSpan> spans = ingest.awaitSpans(4, 10000);
            List<FakeIngest.Request> reqs = ingest.requests("/v1/traces");
            assertTrue(!reqs.isEmpty(), out);
            String expectedAuth = "Basic " + Base64.getEncoder().encodeToString("fck_spring:s3cret".getBytes(StandardCharsets.UTF_8));
            assertEquals(expectedAuth, reqs.get(0).header("Authorization"));
            assertTrue(reqs.get(0).gzip);

            FakeIngest.ReceivedSpan agent = ingest.spanNamed("invoke_agent kefu");
            assertNotNull(agent, spans + "\n" + out);
            assertEquals("c-spring", agent.attr("gen_ai.conversation.id"));
            assertEquals("spring-kefu", agent.resource.get("service.name"), "service.name should come from spring.application.name");
            assertEquals("flashcat-otel", agent.resource.get("telemetry.distro.name"));

            FakeIngest.ReceivedSpan manual = ingest.spanNamed("manual-child");
            assertNotNull(manual);
            assertEquals(agent.spanId, manual.parentSpanId);
            assertEquals("c-spring", manual.attr("gen_ai.conversation.id"));
            assertEquals("u-spring", manual.attr("user.id"));
            assertEquals("t2", manual.attr("flashcat.tag.tenant"));

            FakeIngest.ReceivedSpan outside = ingest.spanNamed("outside-flashcat-span");
            assertNotNull(outside);
            assertEquals("c-spring", outside.attr("gen_ai.conversation.id"), "The Starter's session processor can read private Context keys");

            assertEquals("true", r.line("FLUSH:"), out);
            Map<String, Object> diag = (Map<String, Object>) MiniJson.parse(r.line("RESULT:"));
            assertEquals("add", ((Map<String, Object>) diag.get("provider")).get("exporter_mode"), out);
            assertEquals("spring-kefu", diag.get("service_name"));
            assertTrue(((List<Object>) diag.get("notes")).toString().contains("Spring Boot Starter 不设置 GlobalOpenTelemetry"), out);
            assertTrue(((Number) ((Map<String, Object>) diag.get("export")).get("ok")).longValue() >= 3, out);
        }
    }
}
