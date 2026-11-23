package cloud.flashcat.genai;

import static cloud.flashcat.genai.TestSupport.env;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.opentelemetry.api.trace.Span;
import io.opentelemetry.exporter.otlp.http.trace.OtlpHttpSpanExporter;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.testing.trace.TestSpanData;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.data.StatusData;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import io.opentelemetry.api.trace.SpanKind;
import java.io.IOException;
import java.net.ServerSocket;
import java.net.Socket;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collection;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

/** Tests the shutdown deadline, bounded retries during shutdown, and dropped_timeout accounting from contract §3 and §9. */
class ExitDeadlineTest {

    @AfterEach
    void tearDown() {
        TestSupport.reset(null);
    }

    private static List<SpanData> spans(int n) {
        List<SpanData> out = new ArrayList<SpanData>();
        for (int i = 0; i < n; i++) {
            out.add(TestSpanData.builder().setName("s" + i).setKind(SpanKind.INTERNAL).setStartEpochNanos(1).setEndEpochNanos(2)
                    .setHasEnded(true).setStatus(StatusData.unset()).build());
        }
        return out;
    }

    private static long count(ExportStats stats, String key) {
        Object v = stats.snapshot().get(key);
        return v == null ? 0L : ((Number) v).longValue();
    }

    /** Exporter that never returns. */
    static final class HangingExporter implements SpanExporter {
        final List<CompletableResultCode> results = new ArrayList<CompletableResultCode>();

        @Override
        public synchronized CompletableResultCode export(Collection<SpanData> spans) {
            CompletableResultCode r = new CompletableResultCode();
            results.add(r);
            return r;
        }

        @Override
        public CompletableResultCode flush() {
            return CompletableResultCode.ofSuccess();
        }

        @Override
        public CompletableResultCode shutdown() {
            return CompletableResultCode.ofSuccess();
        }
    }

    /** Exporter that fails immediately with a connection refusal. */
    static final class FailingExporter implements SpanExporter {
        @Override
        public CompletableResultCode export(Collection<SpanData> spans) {
            return CompletableResultCode.ofExceptionalFailure(new java.net.ConnectException("Connection refused"));
        }

        @Override
        public CompletableResultCode flush() {
            return CompletableResultCode.ofSuccess();
        }

        @Override
        public CompletableResultCode shutdown() {
            return CompletableResultCode.ofSuccess();
        }
    }

    @Test
    void inFlightExportIsAbandonedAtDeadline() {
        ExportStats stats = new ExportStats();
        ExitDeadline exit = new ExitDeadline(300);
        HangingExporter inner = new HangingExporter();
        CountingSpanExporter e = new CountingSpanExporter(inner, stats, exit, 10_000);

        // A batch is already in flight before shutdown starts, for example in retry backoff.
        CompletableResultCode before = e.export(spans(3));
        assertFalse(before.isDone());
        long t0 = System.nanoTime();
        exit.start();
        CompletableResultCode during = e.export(spans(2));
        assertTrue(before.join(5, TimeUnit.SECONDS).isDone());
        assertTrue(during.join(5, TimeUnit.SECONDS).isDone());
        long ms = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - t0);
        assertTrue(ms >= 250 && ms < 2000, "Expected a return by the deadline, took " + ms + " ms");
        assertFalse(before.isSuccess());
        assertFalse(during.isSuccess());
        assertEquals(5L, count(stats, "dropped_timeout"), "Count dropped spans");
        assertEquals(0L, count(stats, "network"));
        assertEquals(0, exit.pendingCount());

        // Drop any batch submitted after the deadline.
        int sent = inner.results.size();
        CompletableResultCode late = e.export(spans(4));
        assertTrue(late.isDone());
        assertFalse(late.isSuccess());
        assertEquals(sent, inner.results.size());
        assertEquals(9L, count(stats, "dropped_timeout"));

        // Late completion of the delegate must not count the same spans again.
        for (CompletableResultCode r : inner.results) {
            r.succeed();
        }
        assertEquals(0L, count(stats, "ok"));
    }

    @Test
    void ordinaryFailureDuringExitStillCountsAsNetwork() {
        ExportStats stats = new ExportStats();
        ExitDeadline exit = new ExitDeadline(5_000);
        exit.start();
        CompletableResultCode r = new CountingSpanExporter(new FailingExporter(), stats, exit, 10_000).export(spans(2));
        assertTrue(r.isDone());
        assertFalse(r.isSuccess());
        assertEquals(1L, count(stats, "network"));
        assertEquals(0L, count(stats, "dropped_timeout"));
    }

    @Test
    void withoutExitResultsPassThrough() {
        ExportStats stats = new ExportStats();
        ExitDeadline exit = new ExitDeadline(100);
        HangingExporter inner = new HangingExporter();
        CompletableResultCode r = new CountingSpanExporter(inner, stats, exit, 10_000).export(spans(2));
        assertFalse(r.join(300, TimeUnit.MILLISECONDS).isDone(), "The deadline starts only when shutdown begins");
        inner.results.get(0).succeed();
        assertTrue(r.isSuccess());
        assertEquals(2L, count(stats, "ok"));
    }

    /** A connection refusal triggers exactly two retries, with backoffs of 200 and 400 ms. */
    @Test
    void exitExporterRetriesWithinBound() throws Exception {
        int port;
        try (ServerSocket s = cloud.flashcat.genai.testing.TestPorts.serverSocket()) {
            port = s.getLocalPort();
        }
        OtlpHttpSpanExporter otlp = OtlpHttpSpanExporter.builder().setEndpoint("http://127.0.0.1:" + port + "/v1/traces").build();
        ExportStats stats = new ExportStats();
        ExitDeadline exit = new ExitDeadline(5_000);
        CountingSpanExporter e = new CountingSpanExporter(otlp, stats, exit, 10_000);
        try {
            exit.start();
            long t0 = System.nanoTime();
            CompletableResultCode r = e.export(spans(1)).join(10, TimeUnit.SECONDS);
            long ms = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - t0);
            assertFalse(r.isSuccess());
            assertTrue(ms >= 450 && ms < 1500, "Elapsed time with bounded retries: " + ms + " ms");
            assertEquals(1L, count(stats, "network"));
        } finally {
            e.shutdown().join(5, TimeUnit.SECONDS);
        }
    }

    /**
     * When the endpoint accepts connections without responding, shutdown(timeout) on the programmatic
     * initialization path returns by the deadline and records abandoned spans in dropped_timeout.
     */
    @Test
    void shutdownWithBlackholeEndpointRespectsTotalTimeout() throws Exception {
        final ServerSocket hole = cloud.flashcat.genai.testing.TestPorts.serverSocket();
        final List<Socket> held = new ArrayList<Socket>();
        Thread acceptor = new Thread(() -> {
            try {
                while (true) {
                    held.add(hole.accept());
                }
            } catch (IOException ignored) {
                // Shut down.
            }
        });
        acceptor.setDaemon(true);
        acceptor.start();
        try {
            TestSupport.reset(env("FLASHCAT_ENDPOINT", "http://127.0.0.1:" + hole.getLocalPort() + "/v1/traces", "FLASHCAT_API_KEY", "k:s",
                    "FLASHCAT_SHUTDOWN_TIMEOUT", "1"));
            Diagnostics d = FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("exit-svc").metrics(false).build());
            assertTrue(d.isEnabled(), d.toJson());
            Span s = io.opentelemetry.api.GlobalOpenTelemetry.getTracer("t").spanBuilder("job").startSpan();
            s.end();
            long t0 = System.nanoTime();
            FlashcatGenAI.shutdown(Duration.ofSeconds(1));
            long ms = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - t0);
            assertTrue(ms < 2000, "Elapsed time for shutdown(1s): " + ms + " ms");
            // The timer or batch processor records the count at the deadline. Under load it may appear just after shutdown returns.
            Map<?, ?> export = null;
            for (int i = 0; i < 60; i++) {
                export = (Map<?, ?>) FlashcatGenAI.diagnostics().get("export");
                if (((Number) export.get("dropped_timeout")).longValue() > 0) {
                    break;
                }
                Thread.sleep(50);
            }
            assertEquals(1L, ((Number) export.get("dropped_timeout")).longValue(), export.toString());
        } finally {
            hole.close();
            for (Socket c : held) {
                c.close();
            }
        }
    }

    /** The shutdown exporter retains the endpoint, Basic auth header, and gzip compression and delivers spans to a responsive endpoint. */
    @Test
    void exitExporterKeepsEndpointAuthAndGzip() throws Exception {
        try (cloud.flashcat.genai.testing.FakeIngest ingest = new cloud.flashcat.genai.testing.FakeIngest()) {
            TestSupport.reset(env("FLASHCAT_ENDPOINT", ingest.tracesUrl(), "FLASHCAT_API_KEY", "fck:s3cret"));
            FlashcatGenAI.init(FlashcatGenAI.Options.builder().serviceName("exit-ok").metrics(false).build());
            for (int i = 0; i < 3; i++) {
                io.opentelemetry.api.GlobalOpenTelemetry.getTracer("t").spanBuilder("job-" + i).startSpan().end();
            }
            // Shutdown starts the deadline before flushing, so this batch goes through the shutdown exporter.
            FlashcatGenAI.shutdown(Duration.ofSeconds(5));
            List<cloud.flashcat.genai.testing.FakeIngest.Request> reqs = ingest.requests("/v1/traces");
            assertFalse(reqs.isEmpty());
            assertEquals("Basic " + java.util.Base64.getEncoder().encodeToString("fck:s3cret".getBytes(java.nio.charset.StandardCharsets.UTF_8)),
                    reqs.get(0).header("Authorization"));
            assertTrue(reqs.get(0).gzip);
            assertEquals(3, ingest.spans().size());
            Map<?, ?> export = (Map<?, ?>) FlashcatGenAI.diagnostics().get("export");
            assertEquals(3L, ((Number) export.get("ok")).longValue(), export.toString());
            assertEquals(0L, ((Number) export.get("dropped_timeout")).longValue(), export.toString());
        }
    }

    /** On the Starter path, SDK shutdown starts the deadline independently of JVM shutdown. Spring may close the SDK after graceful shutdown. */
    @Test
    void spiDeadlineStartsWhenSdkShutsDown() {
        State st = new State(State.Mode.SPI, null);
        assertFalse(st.exit.started());
        st.sessionProcessor.shutdown();
        assertTrue(st.exit.started());
    }

    @Test
    void jvmIsNotShuttingDownDuringTests() {
        assertFalse(ExitDeadline.jvmShuttingDown());
        ExitDeadline exit = new ExitDeadline(1000);
        assertFalse(exit.started());
        assertEquals(Long.MAX_VALUE, exit.remainingMillis());
        exit.start(200);
        exit.start(60_000);
        assertTrue(exit.started());
        assertTrue(exit.remainingMillis() <= 200, "Only the first start sets the deadline");
        assertEquals(Arrays.asList(5000L), Arrays.asList(new ExitDeadline(0).timeoutMillis()));
    }
}
