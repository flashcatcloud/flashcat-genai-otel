package cloud.flashcat.otel;

import java.util.LinkedHashMap;
import java.util.Map;

/** Export counts by outcome for diagnostics (the export field in contract §3 and §8). */
final class ExportStats {
    private final Map<String, Long> counts = new LinkedHashMap<String, Long>();
    private String lastError;

    ExportStats() {
        counts.put("ok", 0L);
        counts.put("network", 0L);
        counts.put("dropped_queue_full", 0L);
        counts.put("dropped_timeout", 0L);
    }

    synchronized void inc(String key, long n, String error) {
        Long cur = counts.get(key);
        counts.put(key, (cur == null ? 0L : cur) + n);
        if (error != null) {
            lastError = error;
        }
    }

    synchronized Map<String, Object> snapshot() {
        Map<String, Object> out = new LinkedHashMap<String, Object>(counts);
        out.put("last_error", lastError);
        return out;
    }

    /** Counts HTTP 400, 401, 403, 413, and 429 separately, groups 5xx, and uses http_&lt;code&gt; for other status codes. */
    static String bucket(int status) {
        if (status == 400 || status == 401 || status == 403 || status == 413 || status == 429) {
            return "http_" + status;
        }
        if (status >= 500) {
            return "http_5xx";
        }
        return "http_" + status;
    }
}
