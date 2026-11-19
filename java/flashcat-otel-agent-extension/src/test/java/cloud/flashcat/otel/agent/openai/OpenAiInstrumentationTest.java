package cloud.flashcat.otel.agent.openai;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.openai.core.ObjectMappers;
import com.openai.core.http.AsyncStreamResponse;
import com.openai.core.http.Headers;
import com.openai.core.http.StreamResponse;
import com.openai.errors.RateLimitException;
import com.openai.models.chat.completions.ChatCompletionChunk;
import com.openai.models.responses.Response;
import com.openai.models.responses.ResponseCreateParams;
import com.openai.models.responses.ResponseInputItem;
import com.openai.models.responses.ResponseStreamEvent;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.context.Scope;
import io.opentelemetry.sdk.OpenTelemetrySdk;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.io.File;
import java.net.URI;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.HashSet;
import java.util.List;
import java.util.Optional;
import java.util.Set;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionException;
import java.util.concurrent.Executor;
import java.util.stream.Collectors;
import java.util.stream.Stream;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/** Tests openai-java instrumentation helpers by calling the advice entry points directly, without the Agent. */
class OpenAiInstrumentationTest {
    private InMemorySpanExporter exporter;
    private OpenTelemetrySdk sdk;

    @BeforeEach
    void setUp() {
        GlobalOpenTelemetry.resetForTest();
        exporter = InMemorySpanExporter.create();
        sdk = OpenTelemetrySdk.builder()
                .setTracerProvider(SdkTracerProvider.builder().addSpanProcessor(SimpleSpanProcessor.create(exporter)).build())
                .build();
        GlobalOpenTelemetry.set(sdk);
        System.clearProperty(OpenAiTelemetry.PROP_CAPTURE_CONTENT);
    }

    @AfterEach
    void tearDown() {
        sdk.close();
        GlobalOpenTelemetry.resetForTest();
        System.clearProperty(OpenAiTelemetry.PROP_CAPTURE_CONTENT);
    }

    // ------------------------------------------------------------------ openai-java test objects

    private static ResponseCreateParams params() {
        return ResponseCreateParams.builder().model("gpt-4o-mini").input("你好").maxOutputTokens(64L).temperature(0.5).topP(0.9).build();
    }

    private static String responseJson(String id, String status, String output, String extra) {
        return "{\"id\":\"" + id + "\",\"object\":\"response\",\"created_at\":1,\"status\":\"" + status + "\",\"model\":\"gpt-4o-mini-2024-07-18\","
                + "\"output\":" + output + ",\"usage\":{\"input_tokens\":120,\"output_tokens\":7,\"total_tokens\":127,"
                + "\"input_tokens_details\":{\"cached_tokens\":64},\"output_tokens_details\":{\"reasoning_tokens\":2}},"
                + "\"parallel_tool_calls\":true,\"tool_choice\":\"auto\",\"tools\":[],\"error\":null,\"metadata\":{},"
                + "\"temperature\":1.0,\"top_p\":1.0" + extra + "}";
    }

    private static final String TEXT_OUTPUT = "[{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"role\":\"assistant\","
            + "\"content\":[{\"type\":\"output_text\",\"text\":\"已发货\",\"annotations\":[]}]}]";

    private static Response response(String id) throws Exception {
        return ObjectMappers.jsonMapper().readValue(responseJson(id, "completed", TEXT_OUTPUT, ",\"incomplete_details\":null"), Response.class);
    }

    private static ResponseStreamEvent event(String json) throws Exception {
        return ObjectMappers.jsonMapper().readValue(json, ResponseStreamEvent.class);
    }

    private static List<ResponseStreamEvent> events(String id) throws Exception {
        String pending = responseJson(id, "in_progress", "[]", ",\"incomplete_details\":null");
        return Arrays.asList(
                event("{\"type\":\"response.created\",\"sequence_number\":0,\"response\":" + pending + "}"),
                event("{\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,"
                        + "\"delta\":\"已\",\"logprobs\":[]}"),
                event("{\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,"
                        + "\"delta\":\"发货\",\"logprobs\":[]}"),
                event("{\"type\":\"response.completed\",\"sequence_number\":3,\"response\":" + responseJson(id, "completed", TEXT_OUTPUT,
                        ",\"incomplete_details\":null") + "}"));
    }

    private SpanData onlySpan() {
        List<SpanData> spans = exporter.getFinishedSpanItems();
        assertEquals(1, spans.size(), spans.toString());
        return spans.get(0);
    }

    private static <T> T attr(SpanData s, AttributeKey<T> k) {
        return s.getAttributes().get(k);
    }

    // ------------------------------------------------------------------ Synchronous calls

    @Test
    void syncCreateWritesGenAiAttributes() throws Exception {
        Span agent = GlobalOpenTelemetry.getTracer("t").spanBuilder("invoke_agent kefu").startSpan();
        ResponsesCall call;
        try (Scope ignored = agent.makeCurrent()) {
            call = ResponsesCall.start(params(), null, false);
            assertNotNull(call);
            assertSame(call.span, Span.current(), "The model span is current during the call");
            ResponsesCall.end(call, response("resp_1"), null);
            assertSame(agent, Span.current(), "The outer span is restored when the call ends");
        }
        agent.end();
        SpanData s = exporter.getFinishedSpanItems().get(0);
        assertEquals("chat gpt-4o-mini", s.getName());
        assertEquals(SpanKind.CLIENT, s.getKind());
        assertEquals(agent.getSpanContext().getSpanId(), s.getParentSpanId());
        assertEquals("flashcat.otel.openai-java", s.getInstrumentationScopeInfo().getName());
        assertEquals("chat", attr(s, OpenAiTelemetry.OPERATION));
        assertEquals("openai", attr(s, OpenAiTelemetry.PROVIDER));
        assertEquals("responses", attr(s, OpenAiTelemetry.API_TYPE));
        assertEquals("gpt-4o-mini", attr(s, OpenAiTelemetry.REQUEST_MODEL));
        assertEquals(64L, (long) attr(s, OpenAiTelemetry.REQUEST_MAX_TOKENS));
        assertEquals(0.5, attr(s, OpenAiTelemetry.REQUEST_TEMPERATURE), 1e-9);
        assertEquals(0.9, attr(s, OpenAiTelemetry.REQUEST_TOP_P), 1e-9);
        assertEquals(false, attr(s, OpenAiTelemetry.REQUEST_STREAM));
        assertEquals("resp_1", attr(s, OpenAiTelemetry.RESPONSE_ID));
        assertEquals("gpt-4o-mini-2024-07-18", attr(s, OpenAiTelemetry.RESPONSE_MODEL));
        assertEquals(Collections.singletonList("stop"), attr(s, OpenAiTelemetry.FINISH_REASONS));
        assertEquals(120L, (long) attr(s, OpenAiTelemetry.INPUT_TOKENS));
        assertEquals(7L, (long) attr(s, OpenAiTelemetry.OUTPUT_TOKENS));
        assertEquals(64L, (long) attr(s, OpenAiTelemetry.CACHE_READ_TOKENS));
        assertEquals(2L, (long) attr(s, OpenAiTelemetry.REASONING_TOKENS));
        assertNull(attr(s, OpenAiTelemetry.TIME_TO_FIRST_CHUNK));
        assertNull(attr(s, OpenAiTelemetry.INPUT_MESSAGES), "Content capture is off by default");
        assertEquals(StatusCode.UNSET, s.getStatus().getStatusCode());
    }

    @Test
    void modelVariantsAreReadWithoutThrowing() {
        // Read the ChatModel variant through chat(). Calling asString() would throw.
        ResponseCreateParams p = ResponseCreateParams.builder().model(com.openai.models.ChatModel.GPT_3_5_TURBO).input("x").build();
        ResponsesCall.end(ResponsesCall.start(p, null, false), null, null);
        assertEquals("chat gpt-3.5-turbo", onlySpan().getName());
    }

    @Test
    void errorWithHttpStatusUsesStatusCode() {
        RateLimitException e = RateLimitException.builder().headers(Headers.builder().build()).build();
        ResponsesCall.end(ResponsesCall.start(params(), null, false), null, e);
        SpanData s = onlySpan();
        assertEquals(StatusCode.ERROR, s.getStatus().getStatusCode());
        assertEquals("429", attr(s, OpenAiTelemetry.ERROR_TYPE));
        assertEquals("exception", s.getEvents().get(0).getName());
    }

    @Test
    void errorWithoutStatusUsesClassName() {
        assertEquals("java.io.IOException", OpenAiTelemetry.errorType(new CompletionException(new java.io.IOException("x"))));
        ResponsesCall.end(ResponsesCall.start(params(), null, false), null, new IllegalStateException("boom"));
        assertEquals("java.lang.IllegalStateException", attr(onlySpan(), OpenAiTelemetry.ERROR_TYPE));
    }

    @Test
    void skipsWhenAnotherInferenceSpanIsCurrent() {
        Span upstream = GlobalOpenTelemetry.getTracer("io.opentelemetry.openai-java-1.1").spanBuilder("chat gpt-4o")
                .setSpanKind(SpanKind.CLIENT).setAttribute("gen_ai.operation.name", "chat").startSpan();
        try (Scope ignored = upstream.makeCurrent()) {
            assertTrue(OpenAiTelemetry.isInferenceSpan(Span.current()));
            assertNull(ResponsesCall.start(params(), null, false), "Reuse the current inference span");
        }
        upstream.end();
        Span agent = GlobalOpenTelemetry.getTracer("t").spanBuilder("invoke_agent").setAttribute("gen_ai.operation.name", "invoke_agent").startSpan();
        try (Scope ignored = agent.makeCurrent()) {
            assertFalse(OpenAiTelemetry.isInferenceSpan(Span.current()));
        }
        agent.end();
        assertFalse(OpenAiTelemetry.isInferenceSpan(Span.getInvalid()));
    }

    @Test
    void serverAddressFromBaseUrl() {
        Span s = null;
        io.opentelemetry.api.trace.SpanBuilder b = GlobalOpenTelemetry.getTracer("t").spanBuilder("x");
        OpenAiTelemetry.setServer(b, "https://api.openai.com/v1");
        s = b.startSpan();
        s.end();
        SpanData d = onlySpan();
        assertEquals("api.openai.com", attr(d, OpenAiTelemetry.SERVER_ADDRESS));
        assertEquals(443L, (long) attr(d, OpenAiTelemetry.SERVER_PORT));
    }

    @Test
    void finishReasonFollowsGoMapping() throws Exception {
        assertEquals("stop", ResponsesCall.finishReason(response("r")));
        Response incomplete = ObjectMappers.jsonMapper().readValue(
                responseJson("r", "incomplete", "[]", ",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}"), Response.class);
        assertEquals("max_output_tokens", ResponsesCall.finishReason(incomplete));
        Response failed = ObjectMappers.jsonMapper().readValue(responseJson("r", "failed", "[]", ",\"incomplete_details\":null"), Response.class);
        assertEquals("error", ResponsesCall.finishReason(failed));
    }

    // ------------------------------------------------------------------ Streaming

    @SuppressWarnings("unchecked")
    @Test
    void streamReadToEndRecordsFirstChunkAndEnds() throws Exception {
        FakeStream<ResponseStreamEvent> raw = new FakeStream<ResponseStreamEvent>(events("resp_s"), -1);
        ResponsesCall call = ResponsesCall.start(params(), null, true);
        Thread.sleep(20);
        StreamResponse<ResponseStreamEvent> wrapped = (StreamResponse<ResponseStreamEvent>) ResponsesCall.endStreaming(call, raw, null);
        assertTrue(exporter.getFinishedSpanItems().isEmpty(), "The span must stay open while the stream is being consumed");
        try (StreamResponse<ResponseStreamEvent> st = wrapped) {
            assertEquals(4, st.stream().count());
        }
        SpanData s = onlySpan();
        assertEquals(true, attr(s, OpenAiTelemetry.REQUEST_STREAM));
        assertEquals("resp_s", attr(s, OpenAiTelemetry.RESPONSE_ID));
        assertEquals(120L, (long) attr(s, OpenAiTelemetry.INPUT_TOKENS));
        double ttft = attr(s, OpenAiTelemetry.TIME_TO_FIRST_CHUNK);
        assertTrue(ttft >= 0.015 && ttft < 5, "ttft=" + ttft);
        assertEquals(StatusCode.UNSET, s.getStatus().getStatusCode());
        assertTrue(raw.closed);
    }

    @SuppressWarnings("unchecked")
    @Test
    void streamClosedEarlyEndsAsAborted() throws Exception {
        FakeStream<ResponseStreamEvent> raw = new FakeStream<ResponseStreamEvent>(events("resp_e"), -1);
        StreamResponse<ResponseStreamEvent> st = (StreamResponse<ResponseStreamEvent>) ResponsesCall.endStreaming(
                ResponsesCall.start(params(), null, true), raw, null);
        assertTrue(st.stream().findFirst().isPresent());
        st.close();
        st.close();
        SpanData s = onlySpan();
        assertEquals(StatusCode.ERROR, s.getStatus().getStatusCode());
        assertEquals("stream_aborted", attr(s, OpenAiTelemetry.ERROR_TYPE));
        assertEquals("resp_e", attr(s, OpenAiTelemetry.RESPONSE_ID), "Use the response.created ID when there is no terminal event");
        assertEquals(Collections.singletonList("error"), attr(s, OpenAiTelemetry.FINISH_REASONS));
        assertTrue(raw.closed);
    }

    @SuppressWarnings("unchecked")
    @Test
    void streamExceptionEndsWithErrorAndRethrows() throws Exception {
        FakeStream<ResponseStreamEvent> raw = new FakeStream<ResponseStreamEvent>(events("resp_x"), 2);
        StreamResponse<ResponseStreamEvent> st = (StreamResponse<ResponseStreamEvent>) ResponsesCall.endStreaming(
                ResponsesCall.start(params(), null, true), raw, null);
        IllegalStateException e = assertThrows(IllegalStateException.class, () -> st.stream().forEach(x -> { }));
        assertEquals("连接断开", e.getMessage());
        SpanData s = onlySpan();
        assertEquals(StatusCode.ERROR, s.getStatus().getStatusCode());
        assertEquals("java.lang.IllegalStateException", attr(s, OpenAiTelemetry.ERROR_TYPE));
        assertNotNull(attr(s, OpenAiTelemetry.TIME_TO_FIRST_CHUNK), "The first delta arrived before the error");
    }

    @SuppressWarnings("unchecked")
    @Test
    void failedEventMarksResponseFailed() throws Exception {
        String failed = responseJson("resp_f", "failed", "[]", ",\"incomplete_details\":null")
                .replace("\"error\":null", "\"error\":{\"code\":\"server_error\",\"message\":\"上游出错\"}");
        List<ResponseStreamEvent> evs = Arrays.asList(event("{\"type\":\"response.failed\",\"sequence_number\":0,\"response\":" + failed + "}"));
        StreamResponse<ResponseStreamEvent> st = (StreamResponse<ResponseStreamEvent>) ResponsesCall.endStreaming(
                ResponsesCall.start(params(), null, true), new FakeStream<ResponseStreamEvent>(evs, -1), null);
        st.stream().forEach(x -> { });
        SpanData s = onlySpan();
        assertEquals(StatusCode.ERROR, s.getStatus().getStatusCode());
        assertEquals("上游出错", s.getStatus().getDescription());
        assertEquals("response_failed", attr(s, OpenAiTelemetry.ERROR_TYPE));
        assertEquals(Collections.singletonList("error"), attr(s, OpenAiTelemetry.FINISH_REASONS));
    }

    @Test
    void streamingCallThatThrowsEndsSpan() {
        Object r = ResponsesCall.endStreaming(ResponsesCall.start(params(), null, true), null, new IllegalStateException("x"));
        assertNull(r);
        assertEquals(StatusCode.ERROR, onlySpan().getStatus().getStatusCode());
    }

    // ------------------------------------------------------------------ Asynchronous calls

    @Test
    void asyncCreateEndsWhenFutureCompletes() throws Exception {
        CompletableFuture<Response> f = new CompletableFuture<Response>();
        ResponsesCall.endAsync(ResponsesCall.start(params(), null, false), f, null);
        assertTrue(exporter.getFinishedSpanItems().isEmpty());
        f.complete(response("resp_a"));
        assertEquals("resp_a", attr(onlySpan(), OpenAiTelemetry.RESPONSE_ID));

        exporter.reset();
        CompletableFuture<Response> g = new CompletableFuture<Response>();
        ResponsesCall.endAsync(ResponsesCall.start(params(), null, false), g, null);
        g.completeExceptionally(new CompletionException(RateLimitException.builder().headers(Headers.builder().build()).build()));
        assertEquals("429", attr(onlySpan(), OpenAiTelemetry.ERROR_TYPE));
    }

    @SuppressWarnings("unchecked")
    @Test
    void asyncStreamEndsOnComplete() throws Exception {
        FakeAsyncStream<ResponseStreamEvent> raw = new FakeAsyncStream<ResponseStreamEvent>(events("resp_as"));
        AsyncStreamResponse<ResponseStreamEvent> st = (AsyncStreamResponse<ResponseStreamEvent>) ResponsesCall.endAsyncStreaming(
                ResponsesCall.start(params(), null, true), raw, null);
        List<ResponseStreamEvent> got = new ArrayList<ResponseStreamEvent>();
        st.subscribe(got::add);
        assertEquals(4, got.size());
        SpanData s = onlySpan();
        assertEquals("resp_as", attr(s, OpenAiTelemetry.RESPONSE_ID));
        assertNotNull(attr(s, OpenAiTelemetry.TIME_TO_FIRST_CHUNK));
        st.close();
        assertEquals(1, exporter.getFinishedSpanItems().size(), "Closing the stream must end the span only once");
    }

    // ------------------------------------------------------------------ Content capture

    @Test
    void contentCaptureFormatsMessages() throws Exception {
        System.setProperty(OpenAiTelemetry.PROP_CAPTURE_CONTENT, "span");
        List<ResponseInputItem> items = new ArrayList<ResponseInputItem>();
        items.add(ResponseInputItem.ofMessage(ResponseInputItem.Message.builder().addInputTextContent("查订单")
                .role(ResponseInputItem.Message.Role.USER).build()));
        items.add(ResponseInputItem.ofFunctionCallOutput(ResponseInputItem.FunctionCallOutput.builder().callId("call_1").output("shipped").build()));
        ResponseCreateParams p = ResponseCreateParams.builder().model("gpt-4o-mini").instructions("简短")
                .input(ResponseCreateParams.Input.ofResponse(items)).build();
        String toolOutput = "[{\"id\":\"fc_1\",\"type\":\"function_call\",\"status\":\"completed\",\"call_id\":\"call_2\","
                + "\"name\":\"lookup\",\"arguments\":\"{\\\"id\\\":7}\"}]";
        Response r = ObjectMappers.jsonMapper().readValue(responseJson("resp_c", "completed", toolOutput, ",\"incomplete_details\":null"), Response.class);
        ResponsesCall.end(ResponsesCall.start(p, null, false), r, null);
        SpanData s = onlySpan();
        assertEquals("[{\"type\":\"text\",\"content\":\"简短\"}]", attr(s, OpenAiTelemetry.SYSTEM_INSTRUCTIONS));
        assertEquals("[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"查订单\"}]},"
                + "{\"role\":\"tool\",\"parts\":[{\"type\":\"tool_call_response\",\"id\":\"call_1\",\"response\":\"shipped\"}]}]",
                attr(s, OpenAiTelemetry.INPUT_MESSAGES));
        assertEquals("[{\"role\":\"assistant\",\"parts\":[{\"type\":\"tool_call\",\"id\":\"call_2\",\"name\":\"lookup\",\"arguments\":{\"id\":7}}],"
                + "\"finish_reason\":\"stop\"}]", attr(s, OpenAiTelemetry.OUTPUT_MESSAGES));
    }

    @Test
    void contentTruncatesLongText() {
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < ResponsesContent.LIMIT + 10; i++) {
            sb.append('a');
        }
        String t = ResponsesContent.truncate(sb.toString());
        assertEquals(ResponsesContent.LIMIT + ResponsesContent.TRUNCATED_SUFFIX.length(), t.length());
        assertTrue(t.endsWith(ResponsesContent.TRUNCATED_SUFFIX));
    }

    // ------------------------------------------------------------------ Chat Completions

    private static ChatCompletionChunk chunk(String choicesJson) throws Exception {
        return ObjectMappers.jsonMapper().readValue("{\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,"
                + "\"model\":\"gpt-4o-2024-08-06\",\"choices\":" + choicesJson + "}", ChatCompletionChunk.class);
    }

    private static List<ChatCompletionChunk> chatChunks() throws Exception {
        return Arrays.asList(
                chunk("[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]"),
                chunk("[{\"index\":0,\"delta\":{\"content\":\"好\"},\"finish_reason\":null}]"),
                chunk("[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]"),
                ObjectMappers.jsonMapper().readValue("{\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,"
                        + "\"model\":\"gpt-4o-2024-08-06\",\"choices\":[],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":2,"
                        + "\"total_tokens\":32,\"prompt_tokens_details\":{\"cached_tokens\":8}}}", ChatCompletionChunk.class));
    }

    private static com.openai.models.chat.completions.ChatCompletionCreateParams chatParams() {
        return com.openai.models.chat.completions.ChatCompletionCreateParams.builder().model("gpt-4o").addUserMessage("你好")
                .maxCompletionTokens(32L).build();
    }

    @SuppressWarnings("unchecked")
    @Test
    void chatFirstChunkIsWrittenOnUpstreamSpan() throws Exception {
        Span upstream = GlobalOpenTelemetry.getTracer("io.opentelemetry.openai-java-1.1").spanBuilder("chat gpt-4o")
                .setSpanKind(SpanKind.CLIENT).setAttribute("gen_ai.operation.name", "chat").startSpan();
        Object state;
        try (Scope ignored = upstream.makeCurrent()) {
            state = ChatCall.enterStreaming(chatParams(), null);
        }
        assertTrue(state instanceof ChatFirstChunk, "Calls through the upstream proxy only add time to first token (TTFT) to the existing span");
        StreamResponse<ChatCompletionChunk> st = (StreamResponse<ChatCompletionChunk>) ChatCall.exitStreaming(
                state, new FakeStream<ChatCompletionChunk>(chatChunks(), -1), null);
        Thread.sleep(15);
        assertEquals(4, st.stream().collect(Collectors.toList()).size());
        st.close();
        assertTrue(exporter.getFinishedSpanItems().isEmpty(), "Upstream instrumentation ends its span");
        upstream.end();
        SpanData s = onlySpan();
        double ttft = attr(s, OpenAiTelemetry.TIME_TO_FIRST_CHUNK);
        assertTrue(ttft >= 0.01 && ttft < 5, "ttft=" + ttft);
    }

    /** Starts a chat span for clients that bypass the upstream proxy, such as Spring AI and directly constructed clients. */
    @SuppressWarnings("unchecked")
    @Test
    void chatStreamWithoutUpstreamGetsOwnSpan() throws Exception {
        Span agent = GlobalOpenTelemetry.getTracer("t").spanBuilder("invoke_agent").startSpan();
        Object state;
        try (Scope ignored = agent.makeCurrent()) {
            state = ChatCall.enterStreaming(chatParams(), null);
        }
        assertTrue(state instanceof ChatCall);
        StreamResponse<ChatCompletionChunk> st = (StreamResponse<ChatCompletionChunk>) ChatCall.exitStreaming(
                state, new FakeStream<ChatCompletionChunk>(chatChunks(), -1), null);
        st.stream().forEach(c -> { });
        agent.end();
        SpanData s = exporter.getFinishedSpanItems().get(0);
        assertEquals("chat gpt-4o", s.getName());
        assertEquals(SpanKind.CLIENT, s.getKind());
        assertEquals(agent.getSpanContext().getSpanId(), s.getParentSpanId());
        assertEquals("chat_completions", attr(s, OpenAiTelemetry.API_TYPE));
        assertEquals(true, attr(s, OpenAiTelemetry.REQUEST_STREAM));
        assertEquals(32L, (long) attr(s, OpenAiTelemetry.REQUEST_MAX_TOKENS));
        assertEquals("chatcmpl-1", attr(s, OpenAiTelemetry.RESPONSE_ID));
        assertEquals("gpt-4o-2024-08-06", attr(s, OpenAiTelemetry.RESPONSE_MODEL));
        assertEquals(Collections.singletonList("stop"), attr(s, OpenAiTelemetry.FINISH_REASONS));
        assertEquals(30L, (long) attr(s, OpenAiTelemetry.INPUT_TOKENS));
        assertEquals(8L, (long) attr(s, OpenAiTelemetry.CACHE_READ_TOKENS));
        assertNotNull(attr(s, OpenAiTelemetry.TIME_TO_FIRST_CHUNK));
        assertEquals(StatusCode.UNSET, s.getStatus().getStatusCode());
    }

    @Test
    void chatCreateWithoutUpstreamAndRawResponse() throws Exception {
        String json = "{\"id\":\"chatcmpl-2\",\"object\":\"chat.completion\",\"created\":1,\"model\":\"gpt-4o-2024-08-06\","
                + "\"choices\":[{\"index\":0,\"finish_reason\":\"tool_calls\",\"message\":{\"role\":\"assistant\",\"content\":null,"
                + "\"tool_calls\":[{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}]}}],"
                + "\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":9,\"total_tokens\":49}}";
        final com.openai.models.chat.completions.ChatCompletion cc =
                ObjectMappers.jsonMapper().readValue(json, com.openai.models.chat.completions.ChatCompletion.class);
        System.setProperty(OpenAiTelemetry.PROP_CAPTURE_CONTENT, "span");

        // A nested withRawResponse().create call reuses the span created by the outer create call.
        ChatCall outer = ChatCall.start(chatParams(), null);
        assertNotNull(outer);
        assertNull(ChatCall.start(chatParams(), null), "The current span is already a chat span");
        ChatCall.end(outer, cc, null);
        SpanData s = onlySpan();
        assertEquals(Collections.singletonList("tool_calls"), attr(s, OpenAiTelemetry.FINISH_REASONS));
        assertEquals(40L, (long) attr(s, OpenAiTelemetry.INPUT_TOKENS));
        assertEquals("[{\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"content\":\"你好\"}]}]", attr(s, OpenAiTelemetry.INPUT_MESSAGES));
        assertEquals("[{\"role\":\"assistant\",\"parts\":[{\"type\":\"tool_call\",\"id\":\"call_1\",\"name\":\"weather\","
                + "\"arguments\":{\"city\":\"Paris\"}}],\"finish_reason\":\"tool_calls\"}]", attr(s, OpenAiTelemetry.OUTPUT_MESSAGES));

        // Spring AI's synchronous path uses withRawResponse().create. parse() records attributes and ends the span.
        exporter.reset();
        final boolean[] closed = new boolean[1];
        com.openai.core.http.HttpResponseFor<com.openai.models.chat.completions.ChatCompletion> raw =
                new com.openai.core.http.HttpResponseFor<com.openai.models.chat.completions.ChatCompletion>() {
                    @Override
                    public com.openai.models.chat.completions.ChatCompletion parse() {
                        return cc;
                    }

                    @Override
                    public int statusCode() {
                        return 200;
                    }

                    @Override
                    public Headers headers() {
                        return Headers.builder().build();
                    }

                    @Override
                    public java.io.InputStream body() {
                        return new java.io.ByteArrayInputStream(new byte[0]);
                    }

                    @Override
                    public void close() {
                        closed[0] = true;
                    }
                };
        @SuppressWarnings("unchecked")
        com.openai.core.http.HttpResponseFor<com.openai.models.chat.completions.ChatCompletion> wrapped =
                (com.openai.core.http.HttpResponseFor<com.openai.models.chat.completions.ChatCompletion>) ChatCall.endRaw(
                        ChatCall.start(chatParams(), null), raw, null);
        assertTrue(exporter.getFinishedSpanItems().isEmpty(), "The span must stay open until parse");
        assertEquals(200, wrapped.statusCode());
        assertSame(cc, wrapped.parse());
        assertSame(cc, wrapped.parse());
        wrapped.close();
        assertTrue(closed[0]);
        assertEquals("chatcmpl-2", attr(onlySpan(), OpenAiTelemetry.RESPONSE_ID));
    }

    @Test
    void chatCreateSkippedUnderUpstreamSpan() {
        Span upstream = GlobalOpenTelemetry.getTracer("io.opentelemetry.openai-java-1.1").spanBuilder("chat gpt-4o")
                .setSpanKind(SpanKind.CLIENT).setAttribute("gen_ai.operation.name", "chat").startSpan();
        try (Scope ignored = upstream.makeCurrent()) {
            assertNull(ChatCall.start(chatParams(), null));
        }
        upstream.end();
        assertEquals(1, exporter.getFinishedSpanItems().size());
    }

    @Test
    void firstRuntimeFailureIsRecordedOnce() {
        System.clearProperty(OpenAiTelemetry.PROP_ERROR);
        try {
            OpenAiTelemetry.recordFailure(new NoSuchMethodError("model()"));
            OpenAiTelemetry.recordFailure(new IllegalStateException("second"));
            assertEquals("java.lang.NoSuchMethodError: model()", System.getProperty(OpenAiTelemetry.PROP_ERROR));
        } finally {
            System.clearProperty(OpenAiTelemetry.PROP_ERROR);
        }
    }

    // ------------------------------------------------------------------ Injected helpers

    /**
     * Without muzzle, the module must list every helper injected into the application class loader.
     * Helpers must avoid dependencies on the flashcat-otel library and the OTel SDK.
     */
    @Test
    void helperListCoversAllHelperClasses() throws Exception {
        URI root = FlashcatOpenAiInstrumentationModule.class.getProtectionDomain().getCodeSource().getLocation().toURI();
        File dir = new File(new File(root), "cloud/flashcat/otel/agent/openai");
        Set<String> found = new HashSet<String>();
        File[] files = dir.listFiles();
        assertNotNull(files, dir.toString());
        for (File f : files) {
            String n = f.getName();
            if (!n.endsWith(".class") || n.contains("Instrumentation")) {
                continue;
            }
            found.add("cloud.flashcat.otel.agent.openai." + n.substring(0, n.length() - ".class".length()));
        }
        assertEquals(found, new HashSet<String>(FlashcatOpenAiInstrumentationModule.HELPERS));
        for (String h : FlashcatOpenAiInstrumentationModule.HELPERS) {
            String path = h.replace('.', '/') + ".class";
            byte[] bytes = java.nio.file.Files.readAllBytes(new File(new File(root), path).toPath());
            String text = new String(bytes, java.nio.charset.StandardCharsets.ISO_8859_1);
            assertFalse(text.contains("io/opentelemetry/sdk/"), h + " must avoid references to the OTel SDK");
            for (String lib : new String[] {"cloud/flashcat/otel/Flashcat", "cloud/flashcat/otel/Json", "cloud/flashcat/otel/Settings",
                "cloud/flashcat/otel/State", "cloud/flashcat/otel/Version"}) {
                assertFalse(text.contains(lib), h + " must avoid references to the flashcat-otel library class " + lib);
            }
        }
    }

    // ------------------------------------------------------------------ Test doubles

    /** Synchronous stream stub. Throws on reading element failAt when failAt is nonnegative. */
    static final class FakeStream<T> implements StreamResponse<T> {
        private final List<T> items;
        private final int failAt;
        volatile boolean closed;

        FakeStream(List<T> items, int failAt) {
            this.items = items;
            this.failAt = failAt;
        }

        @Override
        public Stream<T> stream() {
            if (failAt < 0) {
                return items.stream();
            }
            List<Integer> idx = new ArrayList<Integer>();
            for (int i = 0; i < items.size(); i++) {
                idx.add(i);
            }
            return idx.stream().map(i -> {
                if (i == failAt) {
                    throw new IllegalStateException("连接断开");
                }
                return items.get(i);
            });
        }

        @Override
        public void close() {
            closed = true;
        }
    }

    /** Asynchronous stream stub. subscribe() pushes all events synchronously before invoking onComplete. */
    static final class FakeAsyncStream<T> implements AsyncStreamResponse<T> {
        private final List<T> items;
        private final CompletableFuture<Void> done = new CompletableFuture<Void>();

        FakeAsyncStream(List<T> items) {
            this.items = items;
        }

        @Override
        public AsyncStreamResponse<T> subscribe(Handler<? super T> handler) {
            for (T t : items) {
                handler.onNext(t);
            }
            handler.onComplete(Optional.<Throwable>empty());
            done.complete(null);
            return this;
        }

        @Override
        public AsyncStreamResponse<T> subscribe(Handler<? super T> handler, Executor executor) {
            return subscribe(handler);
        }

        @Override
        public CompletableFuture<Void> onCompleteFuture() {
            return done;
        }

        @Override
        public void close() {}
    }
}
