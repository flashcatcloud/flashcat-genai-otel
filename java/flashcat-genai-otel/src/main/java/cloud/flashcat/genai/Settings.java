package cloud.flashcat.genai;

import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Base64;
import java.util.Collections;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.function.Function;

/**
 * Resolves configuration in precedence order (explicit arguments &gt; FLASHCAT_* &gt; standard OTEL_* &gt; defaults), matching Python's
 * {@code _config.resolve} and contract §1.
 *
 * <p>OTEL_* values come from a lookup function. Programmatic initialization reads system properties and environment variables. The SPI uses OTel's ConfigProperties.
 * Both paths support {@code -Dotel.*} and {@code OTEL_*} settings.
 */
final class Settings {
    /** Accepted content capture values. Java supports false and span. Values such as event are reported as problems and disable capture (contract §1). */
    enum ContentMode {
        OFF,
        SPAN,
        EVENT_UNSUPPORTED,
        INVALID
    }

    String serviceName;
    String environment;
    String endpoint;
    String metricsEndpoint;
    String apiKey;
    String authHeader;
    String exporter;
    boolean captureContent;
    boolean metrics;
    Map<String, String> resourceAttributes;
    long exportTimeoutMillis;
    long shutdownTimeoutMillis;
    boolean debug;
    boolean disabled;
    List<String> knownHosts = new ArrayList<String>();
    List<String> problems = new ArrayList<String>();
    MaskingConfig masking;

    private Settings() {}

    /** keyID:secret or an Authorization value from the OTEL headers. */
    String authorization() {
        if (apiKey != null) {
            return basicAuth(apiKey);
        }
        return authHeader;
    }

    boolean serviceNameValid() {
        return isValidServiceName(serviceName);
    }

    static boolean isValidServiceName(String name) {
        return name != null && !name.trim().isEmpty() && !name.startsWith("unknown_service");
    }

    /** Encodes raw keyID:secret as a Basic auth header. Callers can pass the credentials without Base64 or URL encoding. */
    static String basicAuth(String apiKey) {
        return "Basic " + Base64.getEncoder().encodeToString(apiKey.getBytes(StandardCharsets.UTF_8));
    }

    // ---------------- Content capture switch ----------------

    static ContentMode contentMode(String raw) {
        if (raw == null) {
            return ContentMode.OFF;
        }
        String v = raw.trim().toLowerCase(Locale.ROOT);
        switch (v) {
            case "false":
            case "0":
            case "no":
            case "off":
            case "no_content":
                return ContentMode.OFF;
            case "span":
            case "span_only":
                return ContentMode.SPAN;
            case "event":
            case "event_only":
            case "span_and_event":
                return ContentMode.EVENT_UNSUPPORTED;
            default:
                return ContentMode.INVALID;
        }
    }

    /** Throws {@link ConfigException} for invalid explicit values. */
    static ContentMode parseExplicitContent(String raw) {
        if (raw == null) {
            return null;
        }
        if ("true".equalsIgnoreCase(raw.trim())) {
            throw new ConfigException("captureContent=\"true\" 有歧义，请用 \"span\"（写进 span 属性）或 \"false\"");
        }
        ContentMode m = contentMode(raw);
        if (m == ContentMode.INVALID) {
            throw new ConfigException("captureContent 只能是 \"false\" / \"span\"（同义写法 off、no_content、span_only），收到 '" + raw + "'");
        }
        return m;
    }

    static String eventProblem(String raw) {
        return "Java 版只支持把正文写进 span 属性（'span'），收到 '" + raw + "'，按不采处理";
    }

    // ---------------- Configuration parsing ----------------

    /**
     * Resolves configuration.
     *
     * @param o explicit options, or null for the agent and Spring Boot paths
     * @param env FLASHCAT_* environment variables
     * @param otel OTel configuration lookup keyed by otel.* property names
     */
    static Settings resolve(FlashcatGenAI.Options o, Function<String, String> env, Function<String, String> otel) {
        Settings s = new Settings();
        FlashcatGenAI.Options opts = o != null ? o : FlashcatGenAI.Options.builder().build();

        // Explicit content capture settings, including "false", take precedence over environment variables.
        String content = null;
        ContentMode mode = null;
        if (opts.captureContent != null) {
            mode = parseExplicitContent(opts.captureContent);
            content = opts.captureContent;
        }
        if (mode == null) {
            String raw = env.apply("FLASHCAT_CAPTURE_CONTENT");
            if (raw != null) {
                mode = contentMode(raw);
                content = raw;
                if (mode == ContentMode.INVALID) {
                    s.problems.add("FLASHCAT_CAPTURE_CONTENT 只能是 false / span（同义写法 off、no_content、span_only），收到 '" + raw + "'，按不采处理");
                }
            }
        }
        if (mode == null) {
            String raw = otel.apply("otel.instrumentation.genai.capture-message-content");
            if (raw != null) {
                // Upstream Java instrumentation uses a boolean, so treat true in the OTEL variable as span.
                mode = "true".equalsIgnoreCase(raw) ? ContentMode.SPAN : contentMode(raw);
                content = raw;
                if (mode == ContentMode.INVALID) {
                    mode = ContentMode.OFF;
                }
            }
        }
        if (mode == ContentMode.EVENT_UNSUPPORTED) {
            s.problems.add(eventProblem(content));
        }
        s.captureContent = mode == ContentMode.SPAN;

        Map<String, String> resEnv = parseKvList(otel.apply("otel.resource.attributes"));

        s.serviceName = first(opts.serviceName, env.apply("FLASHCAT_SERVICE_NAME"), otel.apply("otel.service.name"), resEnv.get("service.name"));
        s.environment = first(opts.environment, env.apply("FLASHCAT_ENVIRONMENT"),
                resEnv.get("deployment.environment.name"), resEnv.get("deployment.environment"), "prod");

        String ep = first(opts.endpoint, env.apply("FLASHCAT_ENDPOINT"), otel.apply("otel.exporter.otlp.traces.endpoint"));
        if (ep == null) {
            String base = otel.apply("otel.exporter.otlp.endpoint");
            if (base != null) {
                ep = stripTrailingSlash(base) + "/v1/traces";
            }
        }
        if (ep != null) {
            URI uri = parseUri(ep);
            String scheme = uri == null ? null : uri.getScheme();
            if (uri == null || scheme == null || !(scheme.equalsIgnoreCase("http") || scheme.equalsIgnoreCase("https"))
                    || uri.getRawAuthority() == null || uri.getRawAuthority().isEmpty()) {
                s.problems.add("endpoint 必须是完整 URL（含 http(s):// 与 /v1/traces），收到 '" + ep + "'；不自动补端口，以免落到无鉴权入口");
                ep = null;
            } else if (uri.getRawPath() == null || uri.getRawPath().isEmpty() || uri.getRawPath().equals("/")) {
                s.problems.add("endpoint 缺少路径，应为 .../v1/traces，收到 '" + ep + "'");
                ep = null;
            }
        }
        s.endpoint = ep;

        String mep = first(env.apply("FLASHCAT_METRICS_ENDPOINT"), otel.apply("otel.exporter.otlp.metrics.endpoint"));
        if (mep == null && ep != null && ep.endsWith("/v1/traces")) {
            mep = ep.substring(0, ep.length() - "/v1/traces".length()) + "/v1/metrics";
        }
        s.metricsEndpoint = mep;

        String key = first(opts.apiKey, env.apply("FLASHCAT_API_KEY"));
        if (key == null) {
            String raw = first(otel.apply("otel.exporter.otlp.traces.headers"), otel.apply("otel.exporter.otlp.headers"));
            for (Map.Entry<String, String> e : parseKvList(raw).entrySet()) {
                if (e.getKey().equalsIgnoreCase("authorization")) {
                    s.authHeader = e.getValue();
                }
            }
        } else if (key.indexOf(':') < 0) {
            s.problems.add("api_key 应为 keyID:secret（不要传 base64，也不要传 URL 编码的 Authorization）");
            key = null;
        }
        s.apiKey = key;

        s.disabled = truthy(env.apply("FLASHCAT_DISABLED")) || truthy(otel.apply("otel.sdk.disabled"));

        String known = env.apply("FLASHCAT_KNOWN_HOSTS");
        if (known != null) {
            for (String h : known.split(",")) {
                String t = h.trim().toLowerCase(Locale.ROOT);
                if (!t.isEmpty()) {
                    s.knownHosts.add(t);
                }
            }
        }
        if (ep != null) {
            String host = hostOf(ep);
            if (host != null) {
                s.knownHosts.add(host);
            }
        }

        s.exporter = opts.exporter;
        s.metrics = opts.metrics;
        s.resourceAttributes = Collections.unmodifiableMap(new LinkedHashMap<String, String>(opts.resourceAttributes));
        s.exportTimeoutMillis = opts.exportTimeoutMillis;
        s.shutdownTimeoutMillis = opts.shutdownTimeoutMillis > 0
                ? opts.shutdownTimeoutMillis
                : shutdownTimeout(env.apply("FLASHCAT_SHUTDOWN_TIMEOUT"), s.problems);
        s.debug = opts.debug;
        s.masking = opts.masking != null ? opts.masking : MaskingConfig.defaults();
        return s;
    }

    /**
     * FLASHCAT_SHUTDOWN_TIMEOUT accepts fractional seconds. Nonnumeric or nonpositive values are reported as problems and use the 5-second default (contract §1 and §9).
     *
     * @return the shutdown timeout in milliseconds
     */
    static long shutdownTimeout(String raw, List<String> problems) {
        if (raw == null) {
            return ExitDeadline.DEFAULT_TIMEOUT_MILLIS;
        }
        double seconds;
        try {
            seconds = Double.parseDouble(raw.trim());
        } catch (NumberFormatException e) {
            seconds = Double.NaN;
        }
        if (Double.isNaN(seconds) || Double.isInfinite(seconds) || seconds <= 0) {
            problems.add("FLASHCAT_SHUTDOWN_TIMEOUT 应为大于 0 的秒数（可带小数），收到 '" + raw + "'，使用默认值 5 秒");
            return ExitDeadline.DEFAULT_TIMEOUT_MILLIS;
        }
        return Math.max(1L, Math.round(seconds * 1000));
    }

    // ---------------- Helpers ----------------

    static String first(String... vs) {
        for (String v : vs) {
            String t = Env.blankToNull(v);
            if (t != null) {
                return t;
            }
        }
        return null;
    }

    static boolean truthy(String v) {
        if (v == null) {
            return false;
        }
        String t = v.trim().toLowerCase(Locale.ROOT);
        return t.equals("1") || t.equals("true") || t.equals("yes") || t.equals("on");
    }

    static String stripTrailingSlash(String s) {
        String r = s;
        while (r.endsWith("/")) {
            r = r.substring(0, r.length() - 1);
        }
        return r;
    }

    static URI parseUri(String s) {
        try {
            return new URI(s);
        } catch (Exception e) {
            return null;
        }
    }

    static String hostOf(String url) {
        if (url == null) {
            return null;
        }
        URI u = parseUri(url);
        if (u == null || u.getHost() == null) {
            return null;
        }
        String h = u.getHost().toLowerCase(Locale.ROOT);
        return h.isEmpty() ? null : h;
    }

    /** Parses k=v,k=v in OTEL_RESOURCE_ATTRIBUTES and OTEL_EXPORTER_OTLP_HEADERS. Percent-decodes values while preserving literal plus signs. */
    static Map<String, String> parseKvList(String raw) {
        Map<String, String> out = new LinkedHashMap<String, String>();
        if (raw == null) {
            return out;
        }
        for (String part : raw.split(",")) {
            int i = part.indexOf('=');
            if (i < 0) {
                continue;
            }
            out.put(part.substring(0, i).trim(), percentDecode(part.substring(i + 1).trim()));
        }
        return out;
    }

    static String percentDecode(String s) {
        if (s.indexOf('%') < 0) {
            return s;
        }
        byte[] buf = new byte[s.length()];
        int n = 0;
        StringBuilder out = new StringBuilder();
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            if (c == '%' && isHex(s, i + 1) && isHex(s, i + 2)) {
                buf[n++] = (byte) Integer.parseInt(s.substring(i + 1, i + 3), 16);
                i += 2;
                continue;
            }
            if (n > 0) {
                out.append(new String(buf, 0, n, StandardCharsets.UTF_8));
                n = 0;
            }
            out.append(c);
        }
        if (n > 0) {
            out.append(new String(buf, 0, n, StandardCharsets.UTF_8));
        }
        return out.toString();
    }

    private static boolean isHex(String s, int i) {
        if (i >= s.length()) {
            return false;
        }
        char c = s.charAt(i);
        return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F');
    }

    /** Percent-encodes values for otel.exporter.otlp.*.headers, which OTel Java URL-decodes when reading. */
    static String percentEncode(String s) {
        StringBuilder sb = new StringBuilder();
        for (byte b : s.getBytes(StandardCharsets.UTF_8)) {
            int c = b & 0xff;
            if ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~') {
                sb.append((char) c);
            } else {
                sb.append('%').append(String.format("%02X", c));
            }
        }
        return sb.toString();
    }

    /** Snapshot for tests and diagnostics. */
    Map<String, Object> describe() {
        Map<String, Object> m = new HashMap<String, Object>();
        m.put("service_name", serviceName);
        m.put("environment", environment);
        m.put("endpoint", endpoint);
        m.put("metrics_endpoint", metricsEndpoint);
        m.put("capture_content", captureContent);
        m.put("disabled", disabled);
        return m;
    }
}
