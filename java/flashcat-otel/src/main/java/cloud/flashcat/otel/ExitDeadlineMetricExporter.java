package cloud.flashcat.otel;

import io.opentelemetry.exporter.otlp.http.metrics.OtlpHttpMetricExporter;
import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.common.export.MemoryMode;
import io.opentelemetry.sdk.metrics.Aggregation;
import io.opentelemetry.sdk.metrics.InstrumentType;
import io.opentelemetry.sdk.metrics.data.AggregationTemporality;
import io.opentelemetry.sdk.metrics.data.MetricData;
import io.opentelemetry.sdk.metrics.export.MetricExporter;
import java.time.Duration;
import java.util.Collection;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Wraps the OTLP metrics exporter for Flashcat, limits retries during shutdown, and discards exports still pending at the deadline (contract §9).
 *
 * <p>{@code PeriodicMetricReader.shutdown()} waits synchronously for the final metrics export on the shutdown hook thread (up to 5 seconds).
 * This wrapper keeps that wait within the trace deadline. Export counters track spans only, as specified by the contract.
 * Aggregation temporality, default aggregation, and memory mode are delegated unchanged.
 */
final class ExitDeadlineMetricExporter implements MetricExporter {
    private static final Logger logger = Logger.getLogger("cloud.flashcat.otel");
    private static final ExitDeadline.ResultCallback NOOP = new ExitDeadline.ResultCallback() {
        @Override
        public void accept(CompletableResultCode result) {}
    };
    private static final Runnable NOTHING = new Runnable() {
        @Override
        public void run() {}
    };

    private final MetricExporter inner;
    private final ExitDeadline exit;
    private final long exportTimeoutMillis;

    ExitDeadlineMetricExporter(MetricExporter inner, ExitDeadline exit, long exportTimeoutMillis) {
        this.inner = inner;
        this.exit = exit;
        this.exportTimeoutMillis = exportTimeoutMillis;
    }

    @Override
    public CompletableResultCode export(Collection<MetricData> metrics) {
        MetricExporter target = inner;
        if (exit.started()) {
            long remaining = exit.remainingMillis();
            if (remaining <= 0) {
                return CompletableResultCode.ofFailure();
            }
            target = exitExporter(remaining);
        }
        CompletableResultCode result;
        try {
            result = target.export(metrics);
        } catch (Throwable t) { // SDK failures must leave application code unaffected.
            return CompletableResultCode.ofFailure();
        }
        CompletableResultCode guarded = exit.guard(result, NOOP, NOTHING);
        if (target != inner) {
            final MetricExporter copy = target;
            guarded.whenComplete(() -> copy.shutdown());
        }
        return guarded;
    }

    private MetricExporter exitExporter(long remainingMillis) {
        try {
            if (inner instanceof OtlpHttpMetricExporter) {
                long timeout = ExitRetry.timeoutMillis(exportTimeoutMillis, remainingMillis);
                return ((OtlpHttpMetricExporter) inner).toBuilder().setRetryPolicy(ExitRetry.POLICY)
                        .setTimeout(Duration.ofMillis(timeout)).build();
            }
        } catch (Throwable t) {
            logger.log(Level.FINE, "flashcat-otel: 创建退出用的指标导出器失败，沿用原导出器", t);
        }
        return inner;
    }

    @Override
    public AggregationTemporality getAggregationTemporality(InstrumentType instrumentType) {
        return inner.getAggregationTemporality(instrumentType);
    }

    @Override
    public Aggregation getDefaultAggregation(InstrumentType instrumentType) {
        return inner.getDefaultAggregation(instrumentType);
    }

    @Override
    public MemoryMode getMemoryMode() {
        return inner.getMemoryMode();
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
        return "FlashcatExitDeadlineMetricExporter{" + inner + "}";
    }
}
