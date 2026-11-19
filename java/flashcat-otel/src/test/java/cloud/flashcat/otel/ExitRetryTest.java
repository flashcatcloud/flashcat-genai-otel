package cloud.flashcat.otel;

import static cloud.flashcat.otel.TestSupport.env;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import cloud.flashcat.otel.testing.TestPorts;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.exporter.otlp.http.metrics.OtlpHttpMetricExporter;
import io.opentelemetry.exporter.otlp.http.trace.OtlpHttpSpanExporter;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.metrics.SdkMeterProvider;
import io.opentelemetry.sdk.metrics.data.MetricData;
import io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader;
import io.opentelemetry.sdk.testing.trace.TestSpanData;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.data.StatusData;
import java.io.IOException;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Collection;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

/** Sends real OTLP/HTTP requests to verify retry behavior during shutdown, including which exceptions the sender retries. */
class ExitRetryTest {
    private static final Collection<SpanData> SPANS = Collections.singletonList(
            TestSpanData.builder().setName("last-batch").setHasEnded(true).setKind(SpanKind.INTERNAL)
                    .setStatus(StatusData.unset()).setStartEpochNanos(1).setEndEpochNanos(2).build());

    @AfterEach
    void reset() {
        TestSupport.reset(null);
    }

    private static long count(ExportStats stats, String bucket) {
        Object value = stats.snapshot().get(bucket);
        return value == null ? 0 : ((Number) value).longValue();
    }

    @Test
    void transientHttpFailuresRetryTwiceWithBackoff() throws Exception {
        for (int failures : new int[] {1, 2, 3}) {
            try (Receiver receiver = new Receiver(503, failures, false, null)) {
                ExportStats stats = new ExportStats();
                ExitDeadline exit = new ExitDeadline(5000);
                CountingSpanExporter exporter = traces(receiver, stats, exit, 10_000);
                try {
                    exit.start();
                    CompletableResultCode result = exporter.export(SPANS).join(3, TimeUnit.SECONDS);
                    assertTrue(result.isDone());
                    assertEquals(failures < 3, result.isSuccess());
                    assertEquals(Math.min(3, failures + 1), receiver.times.size());
                    long firstGap = receiver.gapMillis(1);
                    assertTrue(firstGap >= 140 && firstGap < 600, "Expected a 200 ms backoff with OTel jitter, got " + firstGap);
                    if (failures >= 2) {
                        long secondGap = receiver.gapMillis(2);
                        assertTrue(secondGap >= 290 && secondGap < 800, "Expected a 400 ms backoff, got " + secondGap);
                    }
                    assertEquals(failures < 3 ? 1 : 0, count(stats, "ok"));
                    assertEquals(failures == 3 ? 1 : 0, count(stats, "http_5xx"));
                    assertEquals(0, count(stats, "dropped_timeout"));
                } finally {
                    exporter.shutdown();
                }
            }
        }
    }

    @Test
    void disconnectedResponseIsRetried() throws Exception {
        try (Receiver receiver = new Receiver(0, 2, true, null)) {
            ExitDeadline exit = new ExitDeadline(5000);
            CountingSpanExporter exporter = traces(receiver, new ExportStats(), exit, 10_000);
            try {
                exit.start();
                assertTrue(exporter.export(SPANS).join(3, TimeUnit.SECONDS).isSuccess());
                assertEquals(3, receiver.times.size());
            } finally {
                exporter.shutdown();
            }
        }
    }

    @Test
    void retryableHttpStatusesMatchContract() throws Exception {
        for (int status : new int[] {429, 502, 503, 504}) {
            try (Receiver receiver = new Receiver(status, 1, false, null)) {
                ExitDeadline exit = new ExitDeadline(5000);
                CountingSpanExporter exporter = traces(receiver, new ExportStats(), exit, 10_000);
                try {
                    exit.start();
                    assertTrue(exporter.export(SPANS).join(2, TimeUnit.SECONDS).isSuccess(), "HTTP " + status);
                    assertEquals(2, receiver.times.size());
                } finally {
                    exporter.shutdown();
                }
            }
        }
    }

    @Test
    void nonRetryableHttpFailureNearDeadlineKeepsItsBucket() throws Exception {
        for (int status : new int[] {400, 401, 403, 413, 500}) {
            try (Receiver receiver = new Receiver(status, 10, false, null)) {
                ExportStats stats = new ExportStats();
                ExitDeadline exit = new ExitDeadline(100);
                CountingSpanExporter exporter = traces(receiver, stats, exit, 10_000);
                try {
                    exit.start();
                    assertFalse(exporter.export(SPANS).join(1, TimeUnit.SECONDS).isSuccess());
                    assertEquals(1, receiver.times.size());
                    assertEquals(1, count(stats, ExportStats.bucket(status)));
                    assertEquals(0, count(stats, "dropped_timeout"), "HTTP " + status + " near the deadline must not be counted as a timeout");
                } finally {
                    exporter.shutdown();
                }
            }
        }
    }

    @Test
    void insufficientBackoffBudgetDoesNotSendAnotherRequest() throws Exception {
        try (Receiver receiver = new Receiver(503, 10, false, null)) {
            ExportStats stats = new ExportStats();
            ExitDeadline exit = new ExitDeadline(100);
            CountingSpanExporter exporter = traces(receiver, stats, exit, 10_000);
            try {
                exit.start();
                long before = System.nanoTime();
                assertFalse(exporter.export(SPANS).join(1, TimeUnit.SECONDS).isSuccess());
                assertTrue(elapsed(before) < 500);
                assertEquals(1, receiver.times.size());
                assertEquals(1, count(stats, "dropped_timeout"));
            } finally {
                exporter.shutdown();
            }
        }
    }

    @Test
    void retryAfterBeyondBudgetDoesNotSendAnotherRequest() throws Exception {
        try (Receiver receiver = new Receiver(503, 10, false, "60")) {
            ExportStats stats = new ExportStats();
            ExitDeadline exit = new ExitDeadline(300);
            CountingSpanExporter exporter = traces(receiver, stats, exit, 10_000);
            try {
                exit.start();
                long before = System.nanoTime();
                assertFalse(exporter.export(SPANS).join(1, TimeUnit.SECONDS).isSuccess());
                assertTrue(elapsed(before) < 700, "The retry must not wait the 60 seconds specified by Retry-After");
                assertEquals(1, receiver.times.size());
                assertEquals(1, count(stats, "dropped_timeout"));
            } finally {
                exporter.shutdown();
            }
        }
    }

    @Test
    void metricsRetryHttpAndNetworkFailures() throws Exception {
        InMemoryMetricReader reader = InMemoryMetricReader.create();
        SdkMeterProvider provider = SdkMeterProvider.builder().registerMetricReader(reader).build();
        provider.get("test").counterBuilder("jobs").build().add(1, Attributes.empty());
        Collection<MetricData> metrics = reader.collectAllMetrics();
        provider.close();
        for (boolean reset : new boolean[] {false, true}) {
            try (Receiver receiver = new Receiver(503, 2, reset, null)) {
                ExitDeadline exit = new ExitDeadline(5000);
                ExitDeadlineMetricExporter exporter = new ExitDeadlineMetricExporter(OtlpHttpMetricExporter.builder()
                        .setEndpoint(receiver.url("metrics")).build(), exit, 10_000);
                try {
                    exit.start();
                    assertTrue(exporter.export(metrics).join(3, TimeUnit.SECONDS).isSuccess());
                    assertEquals(3, receiver.times.size());
                } finally {
                    exporter.shutdown();
                }
            }
        }
    }

    @Test
    void explicitFlushUsesFiniteRetriesAndThenAllowsMoreExports() throws Exception {
        try (Receiver receiver = new Receiver(503, 2, false, null)) {
            TestSupport.reset(env("FLASHCAT_ENDPOINT", receiver.url("traces"), "FLASHCAT_API_KEY", "exitcheck:secret"));
            FlashcatOtel.init(FlashcatOtel.Options.builder().serviceName("flush-retry").metrics(false).build());
            GlobalOpenTelemetry.getTracer("test").spanBuilder("first").startSpan().end();
            assertTrue(FlashcatOtel.flush(Duration.ofSeconds(2)));
            assertEquals(3, receiver.times.size());
            assertFalse(State.get().exit.started(), "Normal export resumes after flush completes");
            GlobalOpenTelemetry.getTracer("test").spanBuilder("second").startSpan().end();
            assertTrue(FlashcatOtel.flush(Duration.ofSeconds(2)));
            Map<?, ?> stats = (Map<?, ?>) FlashcatOtel.diagnostics().get("export");
            assertEquals(2L, ((Number) stats.get("ok")).longValue());
            assertEquals(4, receiver.times.size());
        }
    }

    @Test
    void explicitShutdownRetriesLastBatch() throws Exception {
        try (Receiver receiver = new Receiver(503, 2, false, null)) {
            TestSupport.reset(env("FLASHCAT_ENDPOINT", receiver.url("traces"), "FLASHCAT_API_KEY", "exitcheck:secret"));
            FlashcatOtel.init(FlashcatOtel.Options.builder().serviceName("shutdown-retry").metrics(false).build());
            GlobalOpenTelemetry.getTracer("test").spanBuilder("last").startSpan().end();
            FlashcatOtel.shutdown(Duration.ofSeconds(2));
            assertEquals(3, receiver.times.size());
            assertEquals(1, count(State.get().stats, "ok"));
        }
    }

    @Test
    void flushTimeoutDoesNotBecomePermanentExit() {
        ExitDeadline exit = new ExitDeadline(5000);
        ExitDeadlineTest.HangingExporter inner = new ExitDeadlineTest.HangingExporter();
        ExportStats stats = new ExportStats();
        CountingSpanExporter exporter = new CountingSpanExporter(inner, stats, exit, 10_000);
        try (ExitDeadline.FlushScope scope = exit.beginFlush(50)) {
            assertFalse(exporter.export(SPANS).join(1, TimeUnit.SECONDS).isSuccess());
            assertEquals(0, scope.remainingMillis());
            assertEquals(1, count(stats, "dropped_timeout"));
        }
        assertFalse(exit.started());
        CompletableResultCode next = exporter.export(SPANS);
        inner.results.get(1).succeed();
        assertTrue(next.isSuccess());
    }

    private static CountingSpanExporter traces(Receiver receiver, ExportStats stats, ExitDeadline exit, long timeout) {
        return new CountingSpanExporter(OtlpHttpSpanExporter.builder().setEndpoint(receiver.url("traces")).build(), stats, exit, timeout);
    }

    private static long elapsed(long before) {
        return TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - before);
    }

    private static final class Receiver implements AutoCloseable {
        final HttpServer server = TestPorts.httpServer();
        final List<Long> times = Collections.synchronizedList(new ArrayList<Long>());
        final int status;
        final int failures;
        final boolean reset;
        final String retryAfter;

        Receiver(int status, int failures, boolean reset, String retryAfter) throws IOException {
            this.status = status;
            this.failures = failures;
            this.reset = reset;
            this.retryAfter = retryAfter;
            server.createContext("/", this::handle);
            server.start();
        }

        String url(String signal) {
            return "http://127.0.0.1:" + server.getAddress().getPort() + "/v1/" + signal;
        }

        private void handle(HttpExchange exchange) throws IOException {
            while (exchange.getRequestBody().read(new byte[4096]) != -1) {
                // Read the entire request before closing the connection to simulate a failure on the receiving end.
            }
            times.add(System.nanoTime());
            if (times.size() <= failures && reset) {
                exchange.close();
                return;
            }
            if (retryAfter != null) {
                exchange.getResponseHeaders().set("Retry-After", retryAfter);
            }
            exchange.sendResponseHeaders(times.size() <= failures ? status : 200, -1);
            exchange.close();
        }

        long gapMillis(int index) {
            return TimeUnit.NANOSECONDS.toMillis(times.get(index) - times.get(index - 1));
        }

        @Override
        public void close() {
            server.stop(0);
        }
    }
}
