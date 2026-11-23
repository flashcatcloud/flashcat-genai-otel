package cloud.flashcat.genai;

import io.opentelemetry.sdk.common.CompletableResultCode;
import io.opentelemetry.sdk.common.export.RetryPolicy;
import java.io.InterruptedIOException;
import java.time.Duration;

/** Limited retries during shutdown and explicit flush for traces and metrics. Counters reflect only the final result. */
final class ExitRetry {
    // OkHttp retries only certain IOExceptions by default. A disconnect while reading response headers may surface as a plain IOException or EOFException.
    static final RetryPolicy POLICY = RetryPolicy.builder()
            .setMaxAttempts(3)
            .setInitialBackoff(Duration.ofMillis(200))
            .setMaxBackoff(Duration.ofMillis(400))
            .setBackoffMultiplier(2)
            .setRetryExceptionPredicate(e -> true)
            .build();

    private ExitRetry() {}

    static long timeoutMillis(long exportTimeoutMillis, long remainingMillis) {
        return Math.max(1L, exportTimeoutMillis > 0 ? Math.min(exportTimeoutMillis, remainingMillis) : remainingMillis);
    }

    /** Counts only deadline-induced request timeouts as dropped_timeout. Responses such as 401 or 503 count under their actual status even near the deadline. */
    static boolean timedOut(CompletableResultCode result, boolean limitedByDeadline) {
        if (!limitedByDeadline) {
            return false;
        }
        Throwable t = result.getFailureThrowable();
        while (t != null) {
            if (t instanceof InterruptedIOException) {
                return true;
            }
            t = t.getCause();
        }
        return false;
    }
}
