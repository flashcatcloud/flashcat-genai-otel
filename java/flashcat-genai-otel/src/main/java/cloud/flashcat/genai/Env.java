package cloud.flashcat.genai;

import java.util.Locale;
import java.util.Map;

/**
 * Reads environment variables and OTel-style settings. Tests can substitute a fixed map because Java cannot change the process environment.
 */
final class Env {
    /** Replaces System.getenv in tests when non-null. */
    private static volatile Map<String, String> override;

    private Env() {}

    /** Reads an environment variable. Blank values are treated as unset, as in Python's _env. */
    static String get(String name) {
        Map<String, String> o = override;
        String v;
        try {
            v = o != null ? o.get(name) : System.getenv(name);
        } catch (SecurityException e) {
            return null;
        }
        return blankToNull(v);
    }

    /**
     * Reads an {@code otel.*} setting using OTel autoconfigure precedence. Check the system property, then the uppercased environment variable with dots and hyphens replaced by underscores.
     * Programmatic initialization uses this method. The SPI uses the ConfigProperties supplied by OTel.
     */
    static String otel(String property) {
        String v = null;
        try {
            v = blankToNull(System.getProperty(property));
        } catch (SecurityException ignored) {
            // Treat unreadable settings as unset.
        }
        if (v != null) {
            return v;
        }
        return get(property.toUpperCase(Locale.ROOT).replace('.', '_').replace('-', '_'));
    }

    static String blankToNull(String v) {
        if (v == null) {
            return null;
        }
        String t = v.trim();
        return t.isEmpty() ? null : t;
    }

    static void setOverrideForTest(Map<String, String> env) {
        override = env;
    }
}
