package cloud.flashcat.genai.testing;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import io.opentelemetry.proto.collector.trace.v1.ExportTraceServiceRequest;
import io.opentelemetry.proto.common.v1.AnyValue;
import io.opentelemetry.proto.common.v1.KeyValue;
import io.opentelemetry.proto.resource.v1.Resource;
import io.opentelemetry.proto.trace.v1.ResourceSpans;
import io.opentelemetry.proto.trace.v1.ScopeSpans;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;
import java.util.concurrent.Executors;
import java.util.zip.GZIPInputStream;

/**
 * Fake Flashcat ingest endpoint built on the JDK's {@code com.sun.net.httpserver.HttpServer}.
 * Records each request's path, headers (including Authorization and Content-Encoding) and decompressed body, and decodes OTLP protobuf into a list of spans.
 */
public final class FakeIngest implements AutoCloseable {
    /** One received request. */
    public static final class Request {
        public final String path;
        public final Map<String, String> headers;
        public final byte[] body;
        public final boolean gzip;
        public final int status;

        Request(String path, Map<String, String> headers, byte[] body, boolean gzip, int status) {
            this.path = path;
            this.headers = headers;
            this.body = body;
            this.gzip = gzip;
            this.status = status;
        }

        /** Header names are case-insensitive. */
        public String header(String name) {
            for (Map.Entry<String, String> e : headers.entrySet()) {
                if (e.getKey().equalsIgnoreCase(name)) {
                    return e.getValue();
                }
            }
            return null;
        }
    }

    /** One decoded span. */
    public static final class ReceivedSpan {
        public final String name;
        public final String scope;
        public final String scopeVersion;
        public final Map<String, Object> resource;
        public final String spanId;
        public final String parentSpanId;
        public final String traceId;
        public final Map<String, Object> attrs;
        public final int statusCode;
        public final int kind;
        public final List<String> eventNames;

        ReceivedSpan(String name, String scope, String scopeVersion, Map<String, Object> resource, String spanId, String parentSpanId,
                String traceId, Map<String, Object> attrs, int statusCode, int kind, List<String> eventNames) {
            this.name = name;
            this.scope = scope;
            this.scopeVersion = scopeVersion;
            this.resource = resource;
            this.spanId = spanId;
            this.parentSpanId = parentSpanId;
            this.traceId = traceId;
            this.attrs = attrs;
            this.statusCode = statusCode;
            this.kind = kind;
            this.eventNames = eventNames;
        }

        public Object attr(String key) {
            return attrs.get(key);
        }

        @Override
        public String toString() {
            return name + " " + attrs;
        }
    }

    private final HttpServer server;
    private final List<Request> requests = Collections.synchronizedList(new ArrayList<Request>());
    private volatile int status = 200;
    private volatile String responseBody = "";

    public FakeIngest() throws IOException {
        server = TestPorts.httpServer();
        server.setExecutor(Executors.newCachedThreadPool(r -> {
            Thread t = new Thread(r, "fake-ingest");
            t.setDaemon(true);
            return t;
        }));
        server.createContext("/", this::handle);
        server.start();
    }

    /** Answer all subsequent requests with this status code. */
    public void setStatus(int status, String body) {
        this.status = status;
        this.responseBody = body == null ? "" : body;
    }

    public String url() {
        return "http://127.0.0.1:" + server.getAddress().getPort();
    }

    public String tracesUrl() {
        return url() + "/v1/traces";
    }

    public List<Request> requests() {
        synchronized (requests) {
            return new ArrayList<Request>(requests);
        }
    }

    public List<Request> requests(String path) {
        List<Request> out = new ArrayList<Request>();
        for (Request r : requests()) {
            if (r.path.equals(path)) {
                out.add(r);
            }
        }
        return out;
    }

    /** Wait until a span matching the condition shows up (at most timeoutMillis). */
    public List<ReceivedSpan> awaitSpans(int atLeast, long timeoutMillis) throws InterruptedException {
        long deadline = System.currentTimeMillis() + timeoutMillis;
        List<ReceivedSpan> spans = spans();
        while (spans.size() < atLeast && System.currentTimeMillis() < deadline) {
            Thread.sleep(50);
            spans = spans();
        }
        return spans;
    }

    /** Spans from all /v1/traces requests that were answered with 200. */
    public List<ReceivedSpan> spans() {
        List<ReceivedSpan> out = new ArrayList<ReceivedSpan>();
        for (Request r : requests("/v1/traces")) {
            if (r.status != 200) {
                continue;
            }
            ExportTraceServiceRequest req;
            try {
                req = ExportTraceServiceRequest.parseFrom(r.body);
            } catch (IOException e) {
                throw new AssertionError("body is not valid OTLP protobuf", e);
            }
            for (ResourceSpans rs : req.getResourceSpansList()) {
                Map<String, Object> res = attrs(rs.getResource());
                for (ScopeSpans ss : rs.getScopeSpansList()) {
                    for (io.opentelemetry.proto.trace.v1.Span sp : ss.getSpansList()) {
                        List<String> events = new ArrayList<String>();
                        for (io.opentelemetry.proto.trace.v1.Span.Event ev : sp.getEventsList()) {
                            events.add(ev.getName());
                        }
                        out.add(new ReceivedSpan(sp.getName(), ss.getScope().getName(), ss.getScope().getVersion(), res,
                                hex(sp.getSpanId().toByteArray()), hex(sp.getParentSpanId().toByteArray()), hex(sp.getTraceId().toByteArray()),
                                kvs(sp.getAttributesList()), sp.getStatus().getCodeValue(), sp.getKindValue(), events));
                    }
                }
            }
        }
        return out;
    }

    public ReceivedSpan spanNamed(String name) {
        for (ReceivedSpan s : spans()) {
            if (s.name.equals(name)) {
                return s;
            }
        }
        return null;
    }

    private static Map<String, Object> attrs(Resource r) {
        return kvs(r.getAttributesList());
    }

    private static Map<String, Object> kvs(List<KeyValue> list) {
        Map<String, Object> m = new TreeMap<String, Object>();
        for (KeyValue kv : list) {
            m.put(kv.getKey(), any(kv.getValue()));
        }
        return m;
    }

    private static Object any(AnyValue v) {
        switch (v.getValueCase()) {
            case STRING_VALUE:
                return v.getStringValue();
            case BOOL_VALUE:
                return v.getBoolValue();
            case INT_VALUE:
                return v.getIntValue();
            case DOUBLE_VALUE:
                return v.getDoubleValue();
            case ARRAY_VALUE:
                List<Object> l = new ArrayList<Object>();
                for (AnyValue x : v.getArrayValue().getValuesList()) {
                    l.add(any(x));
                }
                return l;
            default:
                return v.toString();
        }
    }

    private static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder();
        for (byte x : b) {
            sb.append(String.format("%02x", x & 0xff));
        }
        return sb.toString();
    }

    private void handle(HttpExchange ex) throws IOException {
        byte[] raw = readAll(ex.getRequestBody());
        String enc = ex.getRequestHeaders().getFirst("Content-Encoding");
        boolean gzip = "gzip".equalsIgnoreCase(enc);
        byte[] body = gzip ? readAll(new GZIPInputStream(new java.io.ByteArrayInputStream(raw))) : raw;
        Map<String, String> headers = new LinkedHashMap<String, String>();
        for (Map.Entry<String, List<String>> e : ex.getRequestHeaders().entrySet()) {
            headers.put(e.getKey(), String.join(",", e.getValue()));
        }
        int st = status;
        requests.add(new Request(ex.getRequestURI().getPath(), headers, body, gzip, st));
        byte[] resp = st == 200 ? new byte[0] : responseBody.getBytes("UTF-8");
        ex.getResponseHeaders().add("Content-Type", st == 200 ? "application/x-protobuf" : "text/plain");
        ex.sendResponseHeaders(st, resp.length == 0 ? -1 : resp.length);
        if (resp.length > 0) {
            try (OutputStream os = ex.getResponseBody()) {
                os.write(resp);
            }
        }
        ex.close();
    }

    private static byte[] readAll(InputStream in) throws IOException {
        ByteArrayOutputStream bos = new ByteArrayOutputStream();
        byte[] buf = new byte[8192];
        int n;
        while ((n = in.read(buf)) > 0) {
            bos.write(buf, 0, n);
        }
        return bos.toByteArray();
    }

    @Override
    public void close() {
        server.stop(0);
    }
}
