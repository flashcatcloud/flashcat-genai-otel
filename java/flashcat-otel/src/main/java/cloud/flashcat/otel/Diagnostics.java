package cloud.flashcat.otel;

import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Diagnostics (contract §8). {@link FlashcatOtel#init()} returns the same structure as {@link FlashcatOtel#diagnostics()},
 * with the same field names as Python, TypeScript, and Go and {@code java} as the value of {@code sdk.language}.
 *
 * <pre>{@code
 * Diagnostics d = FlashcatOtel.diagnostics();
 * if (!d.getProblems().isEmpty()) { System.err.println(d.toJson()); }
 * }</pre>
 *
 * <p>{@link #toJson()} serializes diagnostics without external JSON libraries.
 */
public final class Diagnostics {
    private final Map<String, Object> data;

    Diagnostics(Map<String, Object> data) {
        this.data = Collections.unmodifiableMap(new LinkedHashMap<String, Object>(data));
    }

    /** Fields and order from contract §8. */
    static Map<String, Object> skeleton() {
        Map<String, Object> d = new LinkedHashMap<String, Object>();
        Map<String, Object> sdk = new LinkedHashMap<String, Object>();
        sdk.put("name", Version.SDK_NAME);
        sdk.put("version", Version.VERSION);
        sdk.put("language", Version.LANGUAGE);
        d.put("sdk", sdk);
        d.put("enabled", false);
        d.put("service_name", null);
        d.put("environment", null);
        d.put("endpoint", null);
        d.put("problems", new ArrayList<String>());
        d.put("notes", new ArrayList<String>());
        return d;
    }

    /** @return whether the export pipeline is active or an existing provider already sends to Flashcat */
    public boolean isEnabled() {
        return Boolean.TRUE.equals(data.get("enabled"));
    }

    /** @return problems in Chinese, or an empty list if there are no configuration problems */
    public List<String> getProblems() {
        return stringList("problems");
    }

    /** @return notes in Chinese */
    public List<String> getNotes() {
        return stringList("notes");
    }

    /** @return the effective service.name */
    public String getServiceName() {
        Object v = data.get("service_name");
        return v == null ? null : v.toString();
    }

    /**
     * Gets a top-level field such as {@code provider}, {@code export}, or {@code semantics}.
     *
     * @param key field name
     * @return field value, with nested maps and lists containing only JDK types
     */
    public Object get(String key) {
        return data.get(key);
    }

    /** @return the complete read-only structure */
    public Map<String, Object> toMap() {
        return data;
    }

    /** @return the diagnostics structure as JSON (contract §8) */
    public String toJson() {
        return Json.write(data);
    }

    @Override
    public String toString() {
        return toJson();
    }

    private List<String> stringList(String key) {
        Object v = data.get(key);
        List<String> out = new ArrayList<String>();
        if (v instanceof List) {
            for (Object o : (List<?>) v) {
                out.add(String.valueOf(o));
            }
        }
        return Collections.unmodifiableList(out);
    }
}
