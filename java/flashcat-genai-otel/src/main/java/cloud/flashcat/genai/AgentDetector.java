package cloud.flashcat.genai;

/**
 * Detects the OpenTelemetry Java agent and checks whether it has loaded the Flashcat extension.
 *
 * <p>Communication across class loaders uses only JDK types. At agent startup, the extension writes two string system
 * properties (extension version and content capture switch) for the application to read. Full diagnostics use a platform MBean (see {@link AgentBridge}).
 */
final class AgentDetector {
    /** System property set to the extension version when the extension loads. */
    static final String PROP_EXTENSION = "cloud.flashcat.genai.agent.extension";
    /** Content capture switch resolved by the extension ({@code span} or {@code false}). */
    static final String PROP_CAPTURE_CONTENT = "cloud.flashcat.genai.agent.capture_content";

    private static final String AGENT_BOOTSTRAP_CLASS = "io.opentelemetry.javaagent.bootstrap.AgentInitializer";

    private AgentDetector() {}

    /** The agent places {@code io.opentelemetry.javaagent.bootstrap.*} on the bootstrap class loader. */
    static boolean isAgentPresent() {
        try {
            Class.forName(AGENT_BOOTSTRAP_CLASS, false, null);
            return true;
        } catch (Throwable e) {
            return false;
        }
    }

    static boolean isExtensionActive() {
        return extensionVersion() != null;
    }

    static String extensionVersion() {
        try {
            return System.getProperty(PROP_EXTENSION);
        } catch (SecurityException e) {
            return null;
        }
    }

    /** Checks whether this class is loaded by the agent's extension class loader. */
    static boolean runningInsideAgent() {
        if (!isAgentPresent()) {
            return false;
        }
        ClassLoader cl = AgentDetector.class.getClassLoader();
        ClassLoader app;
        try {
            app = ClassLoader.getSystemClassLoader();
        } catch (Throwable e) {
            return false;
        }
        // The system class loader or one of its children (such as Spring Boot's LaunchedClassLoader) loads classes on the application classpath.
        for (ClassLoader c = app; c != null; c = c.getParent()) {
            if (c == cl) {
                return false;
            }
        }
        if (isDescendant(cl, app)) {
            return false;
        }
        return cl != null && cl.getClass().getName().startsWith("io.opentelemetry.javaagent");
    }

    private static boolean isDescendant(ClassLoader cl, ClassLoader ancestor) {
        for (ClassLoader c = cl; c != null; c = c.getParent()) {
            if (c == ancestor) {
                return true;
            }
        }
        return false;
    }
}
