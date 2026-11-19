package cloud.flashcat.otel;

import static cloud.flashcat.otel.TestSupport.env;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.Map;
import java.util.function.Function;
import org.junit.jupiter.api.Test;

/** Tests configuration precedence and validation from contract §1. Explicit arguments &gt; FLASHCAT_* &gt; OTEL_* &gt; defaults. */
class ConfigTest {

    private static Settings resolve(FlashcatOtel.Options o, Map<String, String> flashcat, Map<String, String> otel) {
        return Settings.resolve(o, flashcat::get, new Function<String, String>() {
            @Override
            public String apply(String k) {
                return otel.get(k);
            }
        });
    }

    @Test
    void priorityExplicitOverFlashcatOverOtelOverDefault() {
        Map<String, String> fc = env("FLASHCAT_SERVICE_NAME", "fc-svc", "FLASHCAT_ENDPOINT", "https://fc.example.com/v1/traces",
                "FLASHCAT_ENVIRONMENT", "staging", "FLASHCAT_API_KEY", "fk:fs", "FLASHCAT_CAPTURE_CONTENT", "span");
        Map<String, String> otel = env("otel.service.name", "otel-svc", "otel.exporter.otlp.traces.endpoint", "https://otel.example.com/v1/traces",
                "otel.resource.attributes", "deployment.environment.name=otel-env,service.name=res-svc",
                "otel.exporter.otlp.headers", "Authorization=Basic%20b3RlbA==",
                "otel.instrumentation.genai.capture-message-content", "false");

        // Explicit arguments take precedence.
        Settings s = resolve(FlashcatOtel.Options.builder().serviceName("explicit").environment("dev")
                .endpoint("https://ex.example.com/v1/traces").apiKey("ek:es").captureContent("false").build(), fc, otel);
        assertEquals("explicit", s.serviceName);
        assertEquals("dev", s.environment);
        assertEquals("https://ex.example.com/v1/traces", s.endpoint);
        assertEquals(Settings.basicAuth("ek:es"), s.authorization());
        assertFalse(s.captureContent, "Explicit false takes precedence over FLASHCAT_CAPTURE_CONTENT=span");

        // FLASHCAT_* comes next.
        s = resolve(null, fc, otel);
        assertEquals("fc-svc", s.serviceName);
        assertEquals("staging", s.environment);
        assertEquals("https://fc.example.com/v1/traces", s.endpoint);
        assertEquals("Basic Zms6ZnM=", s.authorization());
        assertTrue(s.captureContent);

        // OTEL_* follows.
        s = resolve(null, env(), otel);
        assertEquals("otel-svc", s.serviceName);
        assertEquals("otel-env", s.environment);
        assertEquals("https://otel.example.com/v1/traces", s.endpoint);
        assertEquals("Basic b3RlbA==", s.authorization());
        assertFalse(s.captureContent);

        // Read service.name from OTEL_RESOURCE_ATTRIBUTES.
        s = resolve(null, env(), env("otel.resource.attributes", "service.name=res-svc,deployment.environment=old-env"));
        assertEquals("res-svc", s.serviceName);
        assertEquals("old-env", s.environment);

        // Apply defaults.
        s = resolve(null, env(), env());
        assertNull(s.serviceName);
        assertEquals("prod", s.environment);
        assertNull(s.endpoint);
        assertFalse(s.captureContent);
        assertTrue(s.problems.isEmpty());
    }

    @Test
    void otlpBaseEndpointGetsTracesPathAndMetricsDerived() {
        Settings s = resolve(null, env(), env("otel.exporter.otlp.endpoint", "https://otel.example.com:4318/"));
        assertEquals("https://otel.example.com:4318/v1/traces", s.endpoint);
        assertEquals("https://otel.example.com:4318/v1/metrics", s.metricsEndpoint);
        s = resolve(null, env("FLASHCAT_ENDPOINT", "https://a.example.com/v1/traces", "FLASHCAT_METRICS_ENDPOINT", "https://m.example.com/v1/metrics"), env());
        assertEquals("https://m.example.com/v1/metrics", s.metricsEndpoint);
    }

    @Test
    void endpointWithoutPathOrSchemeIsRejected() {
        Settings s = resolve(null, env("FLASHCAT_ENDPOINT", "ingest.example.com"), env());
        assertNull(s.endpoint);
        assertTrue(s.problems.get(0).contains("完整 URL"), s.problems.toString());
        s = resolve(null, env("FLASHCAT_ENDPOINT", "https://ingest.example.com"), env());
        assertNull(s.endpoint);
        assertTrue(s.problems.get(0).contains("缺少路径"), s.problems.toString());
        s = resolve(null, env("FLASHCAT_ENDPOINT", "https://ingest.example.com/"), env());
        assertNull(s.endpoint);
        s = resolve(null, env("FLASHCAT_ENDPOINT", "ftp://ingest.example.com/v1/traces"), env());
        assertNull(s.endpoint);
    }

    @Test
    void apiKeyWithoutColonIsAProblem() {
        Settings s = resolve(null, env("FLASHCAT_API_KEY", "Zm9vOmJhcg=="), env());
        assertNull(s.authorization());
        assertTrue(s.problems.get(0).contains("keyID:secret"));
    }

    @Test
    void knownHostsIncludeEndpointHost() {
        Settings s = resolve(null, env("FLASHCAT_ENDPOINT", "https://Ingest.Example.com/v1/traces", "FLASHCAT_KNOWN_HOSTS", "a.example.com, b.example.com"), env());
        assertEquals(java.util.Arrays.asList("a.example.com", "b.example.com", "ingest.example.com"), s.knownHosts);
    }

    @Test
    void captureContentExplicitInvalidThrows() {
        assertThrows(ConfigException.class, () -> FlashcatOtel.Options.builder().captureContent("true"));
        assertThrows(ConfigException.class, () -> FlashcatOtel.Options.builder().captureContent("yes"));
        assertThrows(ConfigException.class, () -> FlashcatOtel.Options.builder().captureContent("everything"));
        assertThrows(ConfigException.class, () -> FlashcatOtel.Options.builder().exporter("sometimes"));
        assertThrows(ConfigException.class, () -> FlashcatOtel.Options.builder().exportTimeout(java.time.Duration.ZERO));
        // Accepted aliases.
        for (String v : new String[] {"false", "off", "no_content", "OFF"}) {
            assertFalse(resolve(FlashcatOtel.Options.builder().captureContent(v).build(), env(), env()).captureContent, v);
        }
        for (String v : new String[] {"span", "span_only", "SPAN_ONLY"}) {
            assertTrue(resolve(FlashcatOtel.Options.builder().captureContent(v).build(), env(), env()).captureContent, v);
        }
    }

    @Test
    void eventModesAreAProblemAndCaptureNothing() {
        Settings s = resolve(FlashcatOtel.Options.builder().captureContent("event").build(), env(), env());
        assertFalse(s.captureContent);
        assertTrue(s.problems.get(0).contains("只支持把正文写进 span 属性"));
        s = resolve(null, env("FLASHCAT_CAPTURE_CONTENT", "span_and_event"), env());
        assertFalse(s.captureContent);
        assertEquals(1, s.problems.size());
    }

    @Test
    void invalidContentFromEnvIsAProblemNotAnException() {
        Settings s = resolve(null, env("FLASHCAT_CAPTURE_CONTENT", "true"), env());
        assertFalse(s.captureContent);
        assertTrue(s.problems.get(0).contains("FLASHCAT_CAPTURE_CONTENT"));
        // Upstream Java instrumentation uses a boolean. The OTEL environment variable value true maps to span.
        assertTrue(resolve(null, env(), env("otel.instrumentation.genai.capture-message-content", "true")).captureContent);
        assertTrue(resolve(null, env(), env("otel.instrumentation.genai.capture-message-content", "SPAN_ONLY")).captureContent);
    }

    @Test
    void disabledSwitches() {
        assertTrue(resolve(null, env("FLASHCAT_DISABLED", "true"), env()).disabled);
        assertTrue(resolve(null, env(), env("otel.sdk.disabled", "1")).disabled);
        assertFalse(resolve(null, env("FLASHCAT_DISABLED", "nope"), env()).disabled);
    }

    @Test
    void headerParsingAndEncoding() {
        Map<String, String> m = Settings.parseKvList("Authorization=Basic%20YTpi,x-tenant=t1, bad");
        assertEquals("Basic YTpi", m.get("Authorization"));
        assertEquals("t1", m.get("x-tenant"));
        assertEquals("Basic%20YTpi%2B%2F%3D", Settings.percentEncode("Basic YTpi+/="));
        assertEquals("中文", Settings.percentDecode(Settings.percentEncode("中文")));
    }

    @Test
    void versionMatchesBuild() {
        assertEquals(System.getProperty("flashcat.test.expected.version"), Version.VERSION);
    }

    /**
     * Contract §1 and §9. FLASHCAT_SHUTDOWN_TIMEOUT accepts fractional seconds. Invalid values add
     * a diagnostics problem and fall back to 5 seconds. An explicit argument takes precedence.
     */
    @Test
    void shutdownTimeoutFromEnvironment() {
        assertEquals(5000L, resolve(null, env(), env()).shutdownTimeoutMillis);
        assertEquals(2000L, resolve(null, env("FLASHCAT_SHUTDOWN_TIMEOUT", "2"), env()).shutdownTimeoutMillis);
        assertEquals(500L, resolve(null, env("FLASHCAT_SHUTDOWN_TIMEOUT", "0.5"), env()).shutdownTimeoutMillis);
        for (String bad : new String[] {"abc", "0", "-1", "NaN"}) {
            Settings s = resolve(null, env("FLASHCAT_SHUTDOWN_TIMEOUT", bad), env());
            assertEquals(5000L, s.shutdownTimeoutMillis, bad);
            assertTrue(s.problems.stream().anyMatch(p -> p.contains("FLASHCAT_SHUTDOWN_TIMEOUT") && p.contains(bad)), bad + " " + s.problems);
        }
        Settings explicit = resolve(FlashcatOtel.Options.builder().shutdownTimeout(java.time.Duration.ofMillis(1500)).build(),
                env("FLASHCAT_SHUTDOWN_TIMEOUT", "2"), env());
        assertEquals(1500L, explicit.shutdownTimeoutMillis);
        assertTrue(explicit.problems.isEmpty());
    }
}
