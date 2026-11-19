package cloud.flashcat.otel;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.io.File;
import java.io.IOException;
import java.net.URL;
import java.net.URLClassLoader;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

/** Contract §8 covers openai-java API instrumentation and reporting through problems and one-time warnings when model calls produce no spans. */
class InstrumentationDetectorTest {

    @AfterEach
    void tearDown() {
        TestSupport.reset(null);
        System.clearProperty(InstrumentationDetector.PROP_OPENAI_INSTALLED);
    }

    /** Class loader that exposes resources only. Detection checks resource names without loading classes. */
    private static ClassLoader withResources(String... resources) throws IOException {
        Path dir = Files.createTempDirectory("flashcat-detect");
        for (String r : resources) {
            File f = dir.resolve(r).toFile();
            f.getParentFile().mkdirs();
            Files.write(f.toPath(), new byte[0]);
        }
        return new URLClassLoader(new URL[] {dir.toUri().toURL()}, null);
    }

    private static Map<String, Object> entry(InstrumentationDetector.Result r, String lib, String api) {
        for (Map<String, Object> m : r.instrumentations) {
            if (lib.equals(m.get("lib")) && (api == null || api.equals(m.get("api")))) {
                return m;
            }
        }
        return null;
    }

    @Test
    void openaiWithoutAgentIsAProblem() throws Exception {
        InstrumentationDetector.Result r = InstrumentationDetector.inspect(false, withResources("com/openai/client/OpenAIClient.class"));
        assertEquals("instrumentation_not_installed", entry(r, "openai-java", "chat_completions, embeddings").get("status"));
        assertEquals("instrumentation_not_installed", entry(r, "openai-java", "responses").get("status"));
        assertEquals(1, r.problems.size(), r.problems.toString());
        assertTrue(r.problems.get(0).contains("-javaagent"), r.problems.toString());
    }

    @Test
    void responsesStatusUnderAgent() throws Exception {
        ClassLoader cl = withResources("com/openai/client/OpenAIClient.class");
        // The extension has loaded its Responses instrumentation.
        System.setProperty(InstrumentationDetector.PROP_OPENAI_INSTALLED, "installed");
        InstrumentationDetector.Result r = InstrumentationDetector.inspect(true, cl);
        assertEquals("installed", entry(r, "openai-java", "chat_completions, embeddings").get("status"));
        Map<String, Object> resp = entry(r, "openai-java", "responses");
        assertEquals("flashcat", resp.get("source"));
        assertEquals("installed", resp.get("status"));
        assertTrue(r.problems.isEmpty(), r.problems.toString());

        // The extension is present and reports Responses instrumentation disabled by configuration.
        System.setProperty(InstrumentationDetector.PROP_OPENAI_INSTALLED, "disabled");
        System.setProperty(AgentDetector.PROP_EXTENSION, "0.1.0");
        r = InstrumentationDetector.inspect(true, cl);
        assertEquals("disabled", entry(r, "openai-java", "responses").get("status"));
        assertTrue(r.problems.get(0).contains("Responses API"), r.problems.toString());

        // The Agent runs without the Flashcat extension and lacks built-in Responses instrumentation.
        System.clearProperty(InstrumentationDetector.PROP_OPENAI_INSTALLED);
        System.clearProperty(AgentDetector.PROP_EXTENSION);
        r = InstrumentationDetector.inspect(true, cl);
        assertEquals("instrumentation_not_installed", entry(r, "openai-java", "responses").get("status"));
        assertTrue(r.problems.get(0).contains("client.responses()"), r.problems.toString());
    }

    /** Contract §8 reports {@code not_patched} for instrumentation failures such as an incompatible openai-java version. */
    @Test
    void runtimeFailureIsNotPatched() throws Exception {
        System.setProperty(InstrumentationDetector.PROP_OPENAI_INSTALLED, "installed");
        System.setProperty(InstrumentationDetector.PROP_OPENAI_ERROR, "java.lang.NoSuchMethodError: model()");
        try {
            InstrumentationDetector.Result r = InstrumentationDetector.inspect(true, withResources("com/openai/client/OpenAIClient.class"));
            Map<String, Object> resp = entry(r, "openai-java", "responses");
            assertEquals("not_patched", resp.get("status"));
            assertTrue(r.problems.get(0).contains("NoSuchMethodError"), r.problems.toString());
        } finally {
            System.clearProperty(InstrumentationDetector.PROP_OPENAI_ERROR);
        }
    }

    @Test
    void springAiCoverage() throws Exception {
        String[] springAi = {"org/springframework/ai/chat/model/ChatModel.class", "com/openai/client/OpenAIClient.class"};
        // Without the Agent, model calls produce no spans.
        InstrumentationDetector.Result r = InstrumentationDetector.inspect(false, withResources(springAi));
        assertEquals("instrumentation_not_installed", entry(r, "spring-ai", null).get("status"));
        assertTrue(r.problems.stream().anyMatch(p -> p.contains("Spring AI") && p.contains("OpenAIClientImpl")), r.problems.toString());

        // The Agent and Flashcat extension provide openai-java instrumentation, so diagnostics report no problem.
        System.setProperty(InstrumentationDetector.PROP_OPENAI_INSTALLED, "installed");
        r = InstrumentationDetector.inspect(true, withResources(springAi));
        Map<String, Object> spring = entry(r, "spring-ai", null);
        assertNotNull(spring);
        assertEquals("installed", spring.get("status"));
        assertEquals("flashcat", spring.get("source"));
        assertTrue(r.problems.isEmpty(), r.problems.toString());

        // With only the Micrometer bridge, count the framework's built-in instrumentation.
        System.clearProperty(InstrumentationDetector.PROP_OPENAI_INSTALLED);
        r = InstrumentationDetector.inspect(false, withResources(
                "org/springframework/ai/chat/model/ChatModel.class", "io/micrometer/tracing/otel/bridge/OtelTracer.class"));
        assertEquals("installed", entry(r, "spring-ai", null).get("status"));
        assertTrue(r.problems.isEmpty(), r.problems.toString());
    }

    @Test
    void langchain4jNeedsOpenInference() throws Exception {
        // Without OpenInference, model calls produce no spans. Diagnostics add a problems entry.
        InstrumentationDetector.Result r = InstrumentationDetector.inspect(false, withResources("dev/langchain4j/model/chat/ChatModel.class"));
        Map<String, Object> lc4j = entry(r, "langchain4j", null);
        assertEquals("instrumentation_not_installed", lc4j.get("status"));
        assertEquals("openinference", lc4j.get("source"));
        assertTrue(r.problems.stream().anyMatch(p -> p.contains("com.arize:openinference-instrumentation-langchain4j")), r.problems.toString());

        // With OpenInference installed, the hint explains how to attach the listener and set TraceConfig for content capture.
        r = InstrumentationDetector.inspect(false, withResources("dev/langchain4j/model/chat/ChatModel.class",
                "com/arize/instrumentation/langchain4j/LangChain4jInstrumentor.class"));
        lc4j = entry(r, "langchain4j", null);
        assertEquals("installed", lc4j.get("status"));
        assertTrue(String.valueOf(lc4j.get("hint")).contains("captureContentEnabled"), String.valueOf(lc4j.get("hint")));
        assertTrue(r.problems.isEmpty(), r.problems.toString());
    }

    @Test
    void problemsAreInDiagnosticsAndWarnedOnce() throws Exception {
        final List<LogRecord> warnings = new ArrayList<LogRecord>();
        Handler h = new Handler() {
            @Override
            public void publish(LogRecord record) {
                if (record.getLevel() == Level.WARNING) {
                    warnings.add(record);
                }
            }

            @Override
            public void flush() {}

            @Override
            public void close() {}
        };
        Logger logger = Logger.getLogger("cloud.flashcat.otel");
        logger.addHandler(h);
        try {
            TestSupport.reset(TestSupport.env("FLASHCAT_ENDPOINT", "https://ingest.example.com/v1/traces", "FLASHCAT_API_KEY", "k:s"));
            FlashcatOtel.init(FlashcatOtel.Options.builder().serviceName("x").metrics(false).build());
            ClassLoader cl = withResources("com/openai/client/OpenAIClient.class");
            Thread t = Thread.currentThread();
            ClassLoader prev = t.getContextClassLoader();
            t.setContextClassLoader(cl);
            try {
                Diagnostics d1 = FlashcatOtel.diagnostics();
                Diagnostics d2 = FlashcatOtel.diagnostics();
                assertTrue(d1.getProblems().stream().anyMatch(p -> p.contains("openai-java")), d1.toJson());
                assertEquals(d1.getProblems(), d2.getProblems());
            } finally {
                t.setContextClassLoader(prev);
            }
            long openaiWarnings = warnings.stream().filter(w -> w.getMessage().contains("openai-java")).count();
            assertEquals(1L, openaiWarnings, "Expected one warning per cause");
        } finally {
            logger.removeHandler(h);
        }
    }
}
