package cloud.flashcat.otel;

/** SDK version, verified by tests to match Gradle's project.version. */
final class Version {
    static final String VERSION = "0.1.0";
    static final String SDK_NAME = "flashcat-otel";
    static final String LANGUAGE = "java";
    /** Instrumentation scope name for spans created by the SDK (contract §4). */
    static final String SCOPE = "flashcat.otel";

    private Version() {}
}
