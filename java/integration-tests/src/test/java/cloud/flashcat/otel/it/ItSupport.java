package cloud.flashcat.otel.it;

import com.sun.net.httpserver.HttpServer;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;

/** Test subprocesses and mock model server. */
final class ItSupport {
    private ItSupport() {}

    /** Subprocess result. */
    static final class Result {
        final int exitCode;
        final String output;

        Result(int exitCode, String output) {
            this.exitCode = exitCode;
            this.output = output;
        }

        /** Returns the last line that starts with prefix, with the prefix stripped. */
        String line(String prefix) {
            String found = null;
            for (String l : output.split("\\R")) {
                if (l.startsWith(prefix)) {
                    found = l.substring(prefix.length());
                }
            }
            return found;
        }
    }

    static String prop(String name) {
        String v = System.getProperty(name);
        if (v == null) {
            throw new IllegalStateException("缺少系统属性 " + name + "（由 Gradle 传入）");
        }
        return v;
    }

    static Result runJava(List<String> jvmArgs, String classpath, String mainClass, List<String> args, Map<String, String> env, long timeoutSec)
            throws Exception {
        List<String> cmd = new ArrayList<String>();
        cmd.add(prop("it.java"));
        cmd.addAll(jvmArgs);
        cmd.add("-cp");
        cmd.add(classpath);
        cmd.add(mainClass);
        cmd.addAll(args);
        ProcessBuilder pb = new ProcessBuilder(cmd);
        pb.redirectErrorStream(true);
        Map<String, String> penv = pb.environment();
        penv.keySet().removeIf(k -> k.startsWith("OTEL_") || k.startsWith("FLASHCAT_") || k.startsWith("ANTHROPIC_") || k.toLowerCase().contains("proxy"));
        penv.putAll(env);
        File out = File.createTempFile("flashcat-it", ".log");
        out.deleteOnExit();
        pb.redirectOutput(out);
        Process p = pb.start();
        boolean done = p.waitFor(timeoutSec, TimeUnit.SECONDS);
        if (!done) {
            p.destroyForcibly();
        }
        String text = new String(java.nio.file.Files.readAllBytes(out.toPath()), StandardCharsets.UTF_8);
        if (!done) {
            throw new AssertionError("Subprocess timed out:\n" + text);
        }
        return new Result(p.exitValue(), text);
    }

    /** An ID and token counts from a mock Responses API call. Tests use the ID to find its span. */
    static final class FakeResponse {
        final String id;
        final boolean stream;
        final long inputTokens;
        final long outputTokens;
        final long cachedTokens;

        FakeResponse(String id, boolean stream, long inputTokens, long outputTokens, long cachedTokens) {
            this.id = id;
            this.stream = stream;
            this.inputTokens = inputTokens;
            this.outputTokens = outputTokens;
            this.cachedTokens = cachedTokens;
        }
    }

    /**
     * Mock OpenAI-compatible server. POST /chat/completions returns a fixed chat completion.
     * POST /responses returns a Response, or HTTP 429 if input contains [[fault:429]].
     * Both POST routes stream SSE when {@code stream=true}. For Responses SSE, wait
     * {@link #FIRST_DELTA_DELAY_MS} before the first text delta. GET returns pong.
     */
    static final class FakeModel implements AutoCloseable {
        static final long FIRST_DELTA_DELAY_MS = 120;
        final HttpServer server;
        final List<String> paths = Collections.synchronizedList(new ArrayList<String>());
        final List<FakeResponse> responses = Collections.synchronizedList(new ArrayList<FakeResponse>());
        private final java.util.concurrent.atomic.AtomicInteger seq = new java.util.concurrent.atomic.AtomicInteger();
        private final java.util.concurrent.ExecutorService executor = java.util.concurrent.Executors.newCachedThreadPool();

        FakeModel() throws IOException {
            server = cloud.flashcat.otel.testing.TestPorts.httpServer();
            // SSE responses pause between writes, so keep the server free to process other requests.
            server.setExecutor(executor);
            server.createContext("/", ex -> {
                paths.add(ex.getRequestMethod() + " " + ex.getRequestURI().getPath());
                String req = new String(readAll(ex.getRequestBody()), StandardCharsets.UTF_8);
                boolean stream = req.replace(" ", "").contains("\"stream\":true");
                if ("POST".equals(ex.getRequestMethod()) && ex.getRequestURI().getPath().endsWith("/responses")) {
                    responses(ex, req, stream);
                    return;
                }
                if ("POST".equals(ex.getRequestMethod()) && stream) {
                    chatStream(ex);
                    return;
                }
                byte[] body;
                if ("POST".equals(ex.getRequestMethod())) {
                    body = ("{\"id\":\"chatcmpl-test\",\"object\":\"chat.completion\",\"created\":1761620000,"
                            + "\"model\":\"gpt-4o-2024-08-06\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\","
                            + "\"message\":{\"role\":\"assistant\",\"content\":\"您的订单已发货\"}}],"
                            + "\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":12,\"total_tokens\":132}}")
                            .getBytes(StandardCharsets.UTF_8);
                    ex.getResponseHeaders().add("Content-Type", "application/json");
                } else {
                    body = "pong".getBytes(StandardCharsets.UTF_8);
                }
                ex.sendResponseHeaders(200, body.length);
                try (OutputStream os = ex.getResponseBody()) {
                    os.write(body);
                }
            });
            server.start();
        }

        private void responses(com.sun.net.httpserver.HttpExchange ex, String req, boolean stream) throws IOException {
            if (req.contains("[[fault:429]]")) {
                byte[] err = "{\"error\":{\"message\":\"fake 429\",\"type\":\"rate_limit_error\",\"code\":\"rate_limit_error\"}}"
                        .getBytes(StandardCharsets.UTF_8);
                ex.getResponseHeaders().add("Content-Type", "application/json");
                ex.sendResponseHeaders(429, err.length);
                try (OutputStream os = ex.getResponseBody()) {
                    os.write(err);
                }
                return;
            }
            int n = seq.incrementAndGet();
            String id = "resp_it_" + n;
            long in = 50 + n;
            long out = 3;
            long cached = 10 + n;
            responses.add(new FakeResponse(id, stream, in, out, cached));
            String done = responseJson(id, "completed", "[{\"id\":\"msg_" + n + "\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\","
                    + "\"content\":[{\"type\":\"output_text\",\"text\":\"订单 已 发货\",\"annotations\":[]}]}]",
                    "{\"input_tokens\":" + in + ",\"output_tokens\":" + out + ",\"total_tokens\":" + (in + out)
                            + ",\"input_tokens_details\":{\"cached_tokens\":" + cached + "},\"output_tokens_details\":{\"reasoning_tokens\":0}}");
            if (!stream) {
                byte[] body = done.getBytes(StandardCharsets.UTF_8);
                ex.getResponseHeaders().add("Content-Type", "application/json");
                ex.sendResponseHeaders(200, body.length);
                try (OutputStream os = ex.getResponseBody()) {
                    os.write(body);
                }
                return;
            }
            String pending = responseJson(id, "in_progress", "[]", "null");
            ex.getResponseHeaders().add("Content-Type", "text/event-stream");
            ex.sendResponseHeaders(200, 0);
            try (OutputStream os = ex.getResponseBody()) {
                sse(os, "{\"type\":\"response.created\",\"sequence_number\":0,\"response\":" + pending + "}");
                sse(os, "{\"type\":\"response.in_progress\",\"sequence_number\":1,\"response\":" + pending + "}");
                sse(os, "{\"type\":\"response.output_item.added\",\"sequence_number\":2,\"output_index\":0,\"item\":{\"id\":\"msg_" + n
                        + "\",\"type\":\"message\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]}}");
                sleep(FIRST_DELTA_DELAY_MS);
                int i = 3;
                for (String w : new String[] {"订单", " 已", " 发货"}) {
                    sse(os, "{\"type\":\"response.output_text.delta\",\"sequence_number\":" + (i++) + ",\"item_id\":\"msg_" + n
                            + "\",\"output_index\":0,\"content_index\":0,\"delta\":\"" + w + "\",\"logprobs\":[]}");
                    sleep(20);
                }
                sse(os, "{\"type\":\"response.completed\",\"sequence_number\":" + i + ",\"response\":" + done + "}");
            } catch (IOException e) {
                // The client closed the connection early.
            }
        }

        private static String responseJson(String id, String status, String output, String usage) {
            return "{\"id\":\"" + id + "\",\"object\":\"response\",\"created_at\":1761620000,\"status\":\"" + status + "\","
                    + "\"model\":\"gpt-4o-mini-2024-07-18\",\"output\":" + output + ",\"usage\":" + usage + ","
                    + "\"parallel_tool_calls\":true,\"tool_choice\":\"auto\",\"tools\":[],\"error\":null,\"incomplete_details\":null,"
                    + "\"instructions\":null,\"metadata\":{},\"temperature\":0.2,\"top_p\":1.0}";
        }

        private void chatStream(com.sun.net.httpserver.HttpExchange ex) throws IOException {
            ex.getResponseHeaders().add("Content-Type", "text/event-stream");
            ex.sendResponseHeaders(200, 0);
            String head = "{\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"created\":1761620000,\"model\":\"gpt-4o-mini\",\"choices\":";
            try (OutputStream os = ex.getResponseBody()) {
                sse(os, head + "[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}");
                sleep(FIRST_DELTA_DELAY_MS);
                sse(os, head + "[{\"index\":0,\"delta\":{\"content\":\"已\"},\"finish_reason\":null}]}");
                sse(os, head + "[{\"index\":0,\"delta\":{\"content\":\"发货\"},\"finish_reason\":null}]}");
                sse(os, head + "[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}");
                sse(os, "[DONE]");
            } catch (IOException e) {
                // The client closed the connection early.
            }
        }

        private static void sse(OutputStream os, String data) throws IOException {
            os.write(("data: " + data + "\n\n").getBytes(StandardCharsets.UTF_8));
            os.flush();
        }

        private static void sleep(long ms) {
            try {
                Thread.sleep(ms);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
        }

        FakeResponse response(String id) {
            synchronized (responses) {
                for (FakeResponse r : responses) {
                    if (r.id.equals(id)) {
                        return r;
                    }
                }
            }
            return null;
        }

        String baseUrl() {
            return "http://127.0.0.1:" + server.getAddress().getPort() + "/v1";
        }

        @Override
        public void close() {
            server.stop(0);
            executor.shutdownNow();
        }
    }

    static byte[] readAll(InputStream in) throws IOException {
        ByteArrayOutputStream bos = new ByteArrayOutputStream();
        byte[] buf = new byte[8192];
        int n;
        while ((n = in.read(buf)) > 0) {
            bos.write(buf, 0, n);
        }
        return bos.toByteArray();
    }

    static List<String> list(String... s) {
        return new ArrayList<String>(Arrays.asList(s));
    }
}
