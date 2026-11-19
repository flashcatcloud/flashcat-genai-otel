package cloud.flashcat.otel;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * SDK runtime state for the current class loader, including configuration, shared diagnostics fields, and counters.
 *
 * <p>Programmatic initialization, the Spring Boot Starter SPI, and the Java agent extension SPI all populate this state. The extension does so in its own class loader.
 * The state stores {@code sdk} as Object so the semantic API, which depends only on opentelemetry-api, can read it safely.
 */
final class State {
    /** code / spi / agent。 */
    enum Mode {
        CODE,
        SPI,
        AGENT
    }

    static final Object LOCK = new Object();
    private static volatile State current;

    final Mode mode;
    final Settings settings;
    final List<String> problems = new ArrayList<String>();
    final List<String> notes = new ArrayList<String>();
    final Map<String, Object> content = new LinkedHashMap<String, Object>();
    volatile String situation = "A";
    volatile String exporterMode = "none";
    volatile String metricsMode = "none";
    volatile boolean enabled;
    volatile String serviceName;
    final ExportStats stats = new ExportStats();
    final SessionSpanProcessor sessionProcessor = new SessionSpanProcessor();
    /** Shared shutdown deadline (contract §9) for trace and metrics exporters wrapped by Flashcat. */
    final ExitDeadline exit;
    volatile Masker masker;
    /** OpenTelemetrySdk created by programmatic initialization or supplied by the Starter. */
    volatile Object sdk;
    /** Whether this SDK owns the provider and may close it on shutdown. */
    volatile boolean ownsSdk;
    /** In the SPI path, an invalid service.name on the resource causes the exporter wrapper to drop spans. */
    volatile boolean dropExport;

    State(Mode mode, Settings settings) {
        this.mode = mode;
        this.settings = settings;
        this.serviceName = settings != null ? settings.serviceName : null;
        this.exit = new ExitDeadline(settings != null ? settings.shutdownTimeoutMillis : ExitDeadline.DEFAULT_TIMEOUT_MILLIS, mode != Mode.SPI);
        // SDK shutdown starts the deadline. This is the only signal in the Starter path.
        final ExitDeadline e = this.exit;
        sessionProcessor.setShutdownHook(new Runnable() {
            @Override
            public void run() {
                e.start();
            }
        });
    }

    static State get() {
        return current;
    }

    static void set(State s) {
        current = s;
    }

    static void resetForTest() {
        current = null;
    }

    synchronized void problem(String p) {
        if (!problems.contains(p)) {
            problems.add(p);
        }
    }

    synchronized void note(String n) {
        if (!notes.contains(n)) {
            notes.add(n);
        }
    }

    /** Builds diagnostics in the field order defined by contract §8. */
    synchronized Map<String, Object> diagnostics(ClassLoader detectWith) {
        Map<String, Object> d = Diagnostics.skeleton();
        d.put("enabled", enabled);
        d.put("service_name", serviceName);
        d.put("environment", settings != null ? settings.environment : null);
        d.put("endpoint", settings != null ? settings.endpoint : null);
        InstrumentationDetector.Result detected = InstrumentationDetector.inspect(mode == Mode.AGENT || AgentDetector.isAgentPresent(), detectWith);
        List<String> allProblems = new ArrayList<String>(problems);
        for (String p : detected.problems) {
            if (!allProblems.contains(p)) {
                allProblems.add(p);
            }
        }
        // Contract §8 calls for one WARN per cause when a library is present but model calls will produce no spans.
        InstrumentationDetector.warnOnce(detected.problems);
        d.put("problems", allProblems);
        d.put("notes", new ArrayList<String>(notes));
        Map<String, Object> provider = new LinkedHashMap<String, Object>();
        provider.put("situation", situation);
        provider.put("exporter_mode", exporterMode);
        provider.put("metrics", metricsMode);
        d.put("provider", provider);
        d.put("content", new LinkedHashMap<String, Object>(content));
        d.put("instrumentations", detected.instrumentations);
        d.put("export", stats.snapshot());
        Map<String, Object> sem = new LinkedHashMap<String, Object>();
        sem.put("orphan_inference_spans", sessionProcessor.orphans.get());
        long nested = sessionProcessor.nested.get();
        sem.put("nested_inference_spans", nested);
        if (nested > 0 && !allProblems.contains(SessionSpanProcessor.NESTED_INFERENCE_PROBLEM)) {
            allProblems.add(SessionSpanProcessor.NESTED_INFERENCE_PROBLEM);
        }
        Masker m = masker;
        sem.put("masked_spans", m != null ? m.masked.get() : 0L);
        sem.put("truncated_spans", m != null ? m.truncated.get() : 0L);
        d.put("semantics", sem);
        return d;
    }
}
