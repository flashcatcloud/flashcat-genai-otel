package cloud.flashcat.genai;

import io.opentelemetry.sdk.common.CompletableResultCode;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * Shared shutdown deadline (contract §9) for flushing and shutting down traces and metrics from the start of shutdown.
 *
 * <p>The Java agent creates the SDK. Its shutdown hook ({@code OpenTelemetrySdk.close()} registered by autoconfigure)
 * waits synchronously for the last metrics export (up to 5 seconds), then for the final trace batch (up to 10 seconds). Exporters can retry with backoff.
 * The extension cannot remove the hook, so the exporters wrapped by Flashcat return by the shared deadline.
 *
 * <ol>
 *   <li>Shutdown starts on the first signal from the extension's shutdown hook, JVM shutdown detection during export ({@link #jvmShuttingDown()}),
 *       an explicit {@code FlashcatGenAI.shutdown(timeout)} in the programmatic initialization path, or the session processor during SDK shutdown.
 *       The Spring Boot Starter uses only SDK shutdown because Spring may close the SDK after graceful application shutdown.
 *   <li>Exports started during shutdown retry at most twice. The timeout is the lesser of the caller-supplied export timeout and the time remaining.
 *   <li>Exports still pending at the deadline fail, including requests already in flight or backing off when shutdown began. Their spans count toward {@code dropped_timeout}.
 *       Exports arriving after the deadline are skipped and their spans count toward {@code dropped_timeout} immediately.
 * </ol>
 *
 * <p>The SDK's batch processing and metric reader threads receive results by the deadline, so the agent's hook can return.
 */
final class ExitDeadline {
    private static final Logger logger = Logger.getLogger("cloud.flashcat.genai");
    static final long DEFAULT_TIMEOUT_MILLIS = 5_000L;

    private final long timeoutMillis;
    /** Whether JVM shutdown starts the deadline. This applies to the agent and programmatic initialization. With the Starter, Spring controls SDK shutdown. */
    private final boolean followJvmExit;
    /** Deadline in System.nanoTime units. Zero means shutdown has yet to start. */
    private final AtomicLong deadlineNanos = new AtomicLong();
    private final Set<Guarded> pending = ConcurrentHashMap.newKeySet();
    /** Temporary deadline for an explicit flush. Normal export resumes afterward. */
    private final Set<FlushScope> flushScopes = ConcurrentHashMap.newKeySet();
    private final AtomicBoolean hookRegistered = new AtomicBoolean();

    ExitDeadline(long timeoutMillis) {
        this(timeoutMillis, true);
    }

    ExitDeadline(long timeoutMillis, boolean followJvmExit) {
        this.timeoutMillis = timeoutMillis > 0 ? timeoutMillis : DEFAULT_TIMEOUT_MILLIS;
        this.followJvmExit = followJvmExit;
    }

    long timeoutMillis() {
        return timeoutMillis;
    }

    /** Starts shutdown with the configured shutdown_timeout. Only the first call takes effect. */
    void start() {
        start(timeoutMillis);
    }

    /** Starts shutdown with the timeout passed to shutdown(timeout). Only the first call takes effect. */
    void start(long timeout) {
        long d = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(Math.max(0L, timeout));
        if (d == 0L) {
            d = 1L;
        }
        if (!deadlineNanos.compareAndSet(0L, d)) {
            return;
        }
        schedule(d, new Runnable() {
            @Override
            public void run() {
                expireAll();
            }
        });
    }

    FlushScope beginFlush(long timeout) {
        final FlushScope scope = new FlushScope(System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(Math.max(0L, timeout)));
        flushScopes.add(scope);
        scope.timer = schedule(scope.deadline, new Runnable() {
            @Override
            public void run() {
                Guarded[] guards;
                synchronized (scope) {
                    guards = flushScopes.contains(scope) ? pending.toArray(new Guarded[0]) : new Guarded[0];
                }
                // The callback may finish another concurrent flush, so invoke it outside the scope lock.
                for (Guarded g : guards) {
                    expire(g);
                }
            }
        });
        return scope;
    }

    private Thread schedule(final long deadline, final Runnable expire) {
        Thread t = new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    long wait;
                    while ((wait = deadline - System.nanoTime()) > 0) {
                        TimeUnit.NANOSECONDS.sleep(wait);
                    }
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                }
                expire.run();
            }
        }, "flashcat-genai-otel-exit-deadline");
        t.setDaemon(true);
        try {
            t.start();
        } catch (Throwable e) {
            logger.log(Level.FINE, "flashcat-genai-otel: 启动退出计时线程失败", e);
        }
        return t;
    }

    /** Checks whether shutdown has started. If JVM shutdown is the first signal, the deadline starts now. */
    boolean started() {
        if (deadlineNanos.get() != 0L || !flushScopes.isEmpty()) {
            return true;
        }
        if (followJvmExit && jvmShuttingDown()) {
            start();
            return true;
        }
        return false;
    }

    /** Returns the remaining milliseconds, or Long.MAX_VALUE before shutdown starts. */
    long remainingMillis() {
        long d = deadlineNanos.get();
        for (FlushScope scope : flushScopes) {
            if (d == 0L || scope.deadline - d < 0) {
                d = scope.deadline;
            }
        }
        if (d == 0L) {
            return Long.MAX_VALUE;
        }
        return TimeUnit.NANOSECONDS.toMillis(d - System.nanoTime());
    }

    /** Registers a JVM shutdown hook that only records when shutdown begins (agent extension and Spring Boot Starter paths). */
    void registerShutdownHook() {
        if (!hookRegistered.compareAndSet(false, true)) {
            return;
        }
        try {
            Runtime.getRuntime().addShutdownHook(new Thread(new Runnable() {
                @Override
                public void run() {
                    start();
                }
            }, "flashcat-genai-otel-exit"));
        } catch (Throwable e) {
            logger.log(Level.FINE, "flashcat-genai-otel: 注册退出钩子失败", e);
        }
    }

    /**
     * Wraps an export result so it completes when the delegate finishes or the deadline expires, whichever comes first.
     *
     * @param inner result from the delegate exporter
     * @param onDone counting callback invoked with the delegate's result if it finishes first
     * @param onTimeout counting callback invoked if the delegate is still pending at the deadline
     */
    CompletableResultCode guard(final CompletableResultCode inner, final ResultCallback onDone, Runnable onTimeout) {
        final Guarded g = new Guarded(onTimeout);
        pending.add(g);
        inner.whenComplete(new Runnable() {
            @Override
            public void run() {
                if (!g.done.compareAndSet(false, true)) {
                    return;
                }
                pending.remove(g);
                try {
                    onDone.accept(inner);
                } catch (Throwable ignored) {
                    // Counting failures must leave export unaffected.
                }
                if (inner.isSuccess()) {
                    g.outer.succeed();
                } else {
                    g.outer.fail();
                }
            }
        });
        if (remainingMillis() <= 0) {
            expire(g);
        }
        return g.outer;
    }

    private void expireAll() {
        for (Guarded g : pending) {
            expire(g);
        }
    }

    private void expire(Guarded g) {
        if (!g.done.compareAndSet(false, true)) {
            return;
        }
        pending.remove(g);
        try {
            g.onTimeout.run();
        } catch (Throwable ignored) {
            // Counting failures must leave shutdown unaffected.
        }
        g.outer.fail();
    }

    /** Number of in-flight exports, for tests. */
    int pendingCount() {
        return pending.size();
    }

    /** During JVM shutdown, {@code addShutdownHook} throws IllegalStateException ("Shutdown in progress"). */
    static boolean jvmShuttingDown() {
        Thread probe = new Thread("flashcat-genai-otel-exit-probe");
        try {
            Runtime.getRuntime().addShutdownHook(probe);
        } catch (IllegalStateException e) {
            return true;
        } catch (Throwable e) {
            return false;
        }
        try {
            Runtime.getRuntime().removeShutdownHook(probe);
        } catch (Throwable ignored) {
            // The probe thread never started, so removal failures can be ignored.
        }
        return false;
    }

    /** Callback invoked when the delegate's result completes. */
    interface ResultCallback {
        void accept(CompletableResultCode result);
    }

    final class FlushScope implements AutoCloseable {
        final long deadline;
        private Thread timer;
        private CompletableResultCode completion;

        FlushScope(long deadline) {
            this.deadline = deadline;
        }

        long remainingMillis() {
            return Math.max(0L, ExitDeadline.this.remainingMillis());
        }

        void finishWhen(CompletableResultCode result) {
            completion = result;
        }

        @Override
        public void close() {
            // The SDK batch processor has already queued forceFlush. Enforce the deadline on batches that have yet to start when the caller times out.
            if (completion != null && !completion.isDone()) {
                completion.whenComplete(this::release);
                return;
            }
            release();
        }

        private void release() {
            Guarded[] guards;
            synchronized (this) {
                guards = flushScopes.contains(this) && deadline - System.nanoTime() <= 0
                        ? pending.toArray(new Guarded[0]) : new Guarded[0];
                flushScopes.remove(this);
                if (timer != null && timer != Thread.currentThread()) {
                    timer.interrupt();
                }
            }
            for (Guarded g : guards) {
                expire(g);
            }
        }
    }

    private static final class Guarded {
        final CompletableResultCode outer = new CompletableResultCode();
        final AtomicBoolean done = new AtomicBoolean();
        final Runnable onTimeout;

        Guarded(Runnable onTimeout) {
            this.onTimeout = onTimeout;
        }
    }
}
