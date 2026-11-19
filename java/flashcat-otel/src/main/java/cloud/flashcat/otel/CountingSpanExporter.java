package cloud.flashcat.otel;

import io.opentelemetry.exporter.otlp.http.trace.OtlpHttpSpanExporter;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SpanExporter;
import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Collection;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Wraps the OTLP exporter to count export results without retrying them (contract §3).
 *
 * <p>Reads HTTP status codes from the {@code FailedExportException$HttpExportException} returned by the OTel exporter.
 * The exception is internal to OTel, and the response type moved between 1.67 and 1.68, so this wrapper reads it via reflection.
 * If the status is unavailable, count it as {@code network} and let export proceed.
 *
 * <p>With an {@link ExitDeadline}, this wrapper also enforces the shutdown deadline (contract §9) using an exporter copy with limited retries.
 * Unfinished batches fail at the deadline and their spans count toward {@code dropped_timeout}. Ordinary failures such as connection refusal still count as {@code network}.
 */
final class CountingSpanExporter implements SpanExporter {
    private static final Logger logger = Logger.getLogger("cloud.flashcat.otel");
    private final SpanExporter inner;
    private final ExportStats stats;
    private final ExitDeadline exit;
    private final long exportTimeoutMillis;

    CountingSpanExporter(SpanExporter inner, ExportStats stats) {
        this(inner, stats, null, 0L);
    }

    CountingSpanExporter(SpanExporter inner, ExportStats stats, ExitDeadline exit, long exportTimeoutMillis) {
        this.inner = inner;
        this.stats = stats;
        this.exit = exit;
        this.exportTimeoutMillis = exportTimeoutMillis;
    }

    @Override
    public CompletableResultCode export(Collection<SpanData> spans) {
        final int n = spans.size();
        SpanExporter target = inner;
        boolean limitedByDeadline = false;
        if (exit != null && exit.started()) {
            long remaining = exit.remainingMillis();
            if (remaining <= 0) {
                stats.inc("dropped_timeout", n, "退出时限已到，放弃 " + n + " 个 span");
                return CompletableResultCode.ofFailure();
            }
            target = exitExporter(remaining);
            limitedByDeadline = exportTimeoutMillis <= 0 || remaining <= exportTimeoutMillis;
        }
        final CompletableResultCode result;
        try {
            result = target.export(spans);
        } catch (Throwable t) { // SDK failures must leave application code unaffected.
            stats.inc("network", 1, t.getClass().getSimpleName() + ": " + t.getMessage());
            return CompletableResultCode.ofFailure();
        }
        final boolean deadlineLimited = limitedByDeadline;
        if (exit != null) {
            CompletableResultCode guarded = exit.guard(result, new ExitDeadline.ResultCallback() {
                @Override
                public void accept(CompletableResultCode r) {
                    count(r, n, deadlineLimited);
                }
            }, new Runnable() {
                @Override
                public void run() {
                    stats.inc("dropped_timeout", n, "退出时限已到，放弃 " + n + " 个 span（导出未完成）");
                }
            });
            if (target != inner) {
                final SpanExporter copy = target;
                guarded.whenComplete(() -> copy.shutdown());
            }
            return guarded;
        }
        result.whenComplete(new Runnable() {
            @Override
            public void run() {
                count(result, n, false);
            }
        });
        return result;
    }

    private void count(CompletableResultCode result, int n, boolean limitedByDeadline) {
        try {
            if (result.isSuccess()) {
                stats.inc("ok", n, null);
            } else if (ExitRetry.timedOut(result, limitedByDeadline)) {
                stats.inc("dropped_timeout", n, "退出时限已到，放弃 " + n + " 个 span（请求超时）");
            } else {
                classify(result);
            }
        } catch (Throwable ignored) {
            // Counting failures must leave export unaffected.
        }
    }

    /**
     * Create a copy per batch using the time remaining so a previous flush cannot carry over its timeout. Close the copy on completion or at the deadline.
     * If the exporter cannot be copied (for example, with a non-OTLP/HTTP exporter or an older SDK), use the delegate and enforce the deadline.
     */
    private SpanExporter exitExporter(long remainingMillis) {
        try {
            if (inner instanceof OtlpHttpSpanExporter) {
                long timeout = ExitRetry.timeoutMillis(exportTimeoutMillis, remainingMillis);
                return ((OtlpHttpSpanExporter) inner).toBuilder().setRetryPolicy(ExitRetry.POLICY)
                        .setTimeout(Duration.ofMillis(timeout)).build();
            }
        } catch (Throwable t) {
            logger.log(Level.FINE, "flashcat-otel: 创建退出用的导出器失败，沿用原导出器", t);
        }
        return inner;
    }

    private void classify(CompletableResultCode result) {
        Throwable t = failureThrowable(result);
        if (t != null && t.getClass().getName().endsWith("FailedExportException$HttpExportException")) {
            Object response = call(t, "getResponse");
            if (response != null) {
                // OTel 1.68+ uses getStatusCode() / getResponseBody(). Through 1.67, including Java agent 2.33.1, it uses statusCode() / responseBody().
                Object code = call(response, "getStatusCode");
                if (code == null) {
                    code = call(response, "statusCode");
                }
                if (code instanceof Integer) {
                    int status = (Integer) code;
                    String body = "";
                    Object raw = call(response, "getResponseBody");
                    if (raw == null) {
                        raw = call(response, "responseBody");
                    }
                    if (raw instanceof byte[]) {
                        body = new String((byte[]) raw, StandardCharsets.UTF_8);
                        if (body.length() > 300) {
                            body = body.substring(0, 300);
                        }
                    }
                    stats.inc(ExportStats.bucket(status), 1, ("HTTP " + status + " " + body).trim());
                    return;
                }
            }
            Throwable cause = t.getCause();
            stats.inc("network", 1, cause != null ? describe(cause) : "export failed");
            return;
        }
        stats.inc("network", 1, t != null ? describe(t) : "export failed");
    }

    private static Throwable failureThrowable(CompletableResultCode r) {
        try {
            return r.getFailureThrowable();
        } catch (Throwable e) { // Older SDK versions lack this method.
            return null;
        }
    }

    private static String describe(Throwable t) {
        return t.getClass().getSimpleName() + ": " + t.getMessage();
    }

    private static Object call(Object target, String method) {
        try {
            Method m = target.getClass().getMethod(method);
            m.setAccessible(true);
            return m.invoke(target);
        } catch (Throwable e) {
            return null;
        }
    }

    @Override
    public CompletableResultCode flush() {
        return inner.flush();
    }

    @Override
    public CompletableResultCode shutdown() {
        return inner.shutdown();
    }

    @Override
    public String toString() {
        return "FlashcatCountingSpanExporter{" + inner + "}";
    }
}
