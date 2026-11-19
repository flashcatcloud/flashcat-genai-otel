package cloud.flashcat.otel.it;

import cloud.flashcat.otel.Flashcat;
import cloud.flashcat.otel.FlashcatOtel;
import com.openai.client.OpenAIClient;
import com.openai.client.okhttp.OpenAIOkHttpClient;
import com.openai.core.http.AsyncStreamResponse;
import com.openai.core.http.StreamResponse;
import com.openai.models.chat.completions.ChatCompletionChunk;
import com.openai.models.chat.completions.ChatCompletionCreateParams;
import com.openai.models.responses.Response;
import com.openai.models.responses.ResponseCreateParams;
import com.openai.models.responses.ResponseStreamEvent;
import java.time.Duration;
import java.util.Optional;
import java.util.concurrent.atomic.AtomicReference;

/**
 * ResponsesIT launches this app with the OpenTelemetry Java Agent and Flashcat extension to
 * test Responses API instrumentation. The argument is the mock model server's base URL.
 * Each step prints "STEP:name:response id" so the test can match responses to spans.
 */
public final class ResponsesApp {
    private ResponsesApp() {}

    private static ResponseCreateParams params(String input) {
        return ResponseCreateParams.builder()
                .model("gpt-4o-mini")
                .input(input)
                .instructions("回答要简短")
                .maxOutputTokens(64L)
                .temperature(0.2)
                .build();
    }

    public static void main(String[] args) throws Exception {
        String base = args[0];
        OpenAIClient client = OpenAIOkHttpClient.builder().baseUrl(base).apiKey("sk-test").maxRetries(0).build();
        // fromEnv() reads OPENAI_BASE_URL and OPENAI_API_KEY.
        OpenAIClient fromEnv = OpenAIOkHttpClient.fromEnv();

        try (Flashcat.Session s = Flashcat.session().conversationId("c-resp").userId("u-resp").start()) {
            Flashcat.agent("responses-agent", () -> {
                // 1. Synchronous call.
                Response r = client.responses().create(params("sync 我的订单到哪了 13800138000"));
                System.out.println("STEP:sync:" + r.id());

                // 2. Read the entire stream.
                AtomicReference<String> id = new AtomicReference<String>();
                StringBuilder text = new StringBuilder();
                try (StreamResponse<ResponseStreamEvent> st = client.responses().createStreaming(params("stream"))) {
                    st.stream().forEach(e -> {
                        if (e.isOutputTextDelta()) {
                            text.append(e.asOutputTextDelta().delta());
                        }
                        if (e.isCompleted()) {
                            id.set(e.asCompleted().response().id());
                        }
                    });
                }
                System.out.println("STEP:stream:" + id.get());
                System.out.println("STREAM_TEXT:" + text);

                // 3. Close the stream after reading the first event.
                try (StreamResponse<ResponseStreamEvent> st = client.responses().createStreaming(params("early-close"))) {
                    Optional<ResponseStreamEvent> first = st.stream().findFirst();
                    System.out.println("STEP:early:" + (first.isPresent() && first.get().isCreated() ? first.get().asCreated().response().id() : "?"));
                }

                // 4. The mock server returns 429. The original exception reaches the caller.
                try {
                    client.responses().create(params("error [[fault:429]]"));
                    System.out.println("STEP:error:no-exception");
                } catch (RuntimeException e) {
                    System.out.println("STEP:error:" + e.getClass().getName());
                }

                // 5. Asynchronous call.
                Response ar = client.async().responses().create(params("async")).get();
                System.out.println("STEP:async:" + ar.id());

                // 6. Asynchronous streaming call.
                AtomicReference<String> aid = new AtomicReference<String>();
                AsyncStreamResponse<ResponseStreamEvent> as = client.async().responses().createStreaming(params("async-stream"));
                as.subscribe(new AsyncStreamResponse.Handler<ResponseStreamEvent>() {
                    @Override
                    public void onNext(ResponseStreamEvent e) {
                        if (e.isCompleted()) {
                            aid.set(e.asCompleted().response().id());
                        }
                    }

                    @Override
                    public void onComplete(Optional<Throwable> error) {}
                });
                as.onCompleteFuture().get();
                System.out.println("STEP:async-stream:" + aid.get());

                // 7. Client constructed with fromEnv().
                System.out.println("STEP:from-env:" + fromEnv.responses().create(params("from-env")).id());

                // 8. Streaming Chat Completions. The extension adds time to first token (TTFT) to the upstream span.
                ChatCompletionCreateParams chat = ChatCompletionCreateParams.builder().model("gpt-4o-mini").addUserMessage("chat-stream").build();
                StringBuilder chatText = new StringBuilder();
                try (StreamResponse<ChatCompletionChunk> st = client.chat().completions().createStreaming(chat)) {
                    st.stream().forEach(c -> {
                        for (ChatCompletionChunk.Choice ch : c.choices()) {
                            chatText.append(ch.delta().content().orElse(""));
                        }
                    });
                }
                System.out.println("CHAT_TEXT:" + chatText);
                // The asynchronous client's create call emits only the upstream span.
                System.out.println("CHAT:async:" + client.async().chat().completions().create(chat).get().id());

                // 9–11. The extension provides spans when Chat Completions bypasses the upstream proxy.
                // Spring AI uses both paths tested here: a client derived with withOptions and withRawResponse().
                OpenAIClient derived = client.withOptions(o -> o.maxRetries(0));
                System.out.println("CHAT:derived:" + derived.chat().completions().create(chat).id());
                System.out.println("CHAT:raw:" + client.chat().completions().withRawResponse().create(chat).parse().id());
                StringBuilder derivedText = new StringBuilder();
                try (StreamResponse<ChatCompletionChunk> st = derived.chat().completions().createStreaming(chat)) {
                    st.stream().forEach(c -> {
                        for (ChatCompletionChunk.Choice ch : c.choices()) {
                            derivedText.append(ch.delta().content().orElse(""));
                        }
                    });
                }
                System.out.println("CHAT:derived-stream:" + derivedText);
            });
        }
        System.out.println("FLUSH:" + FlashcatOtel.flush(Duration.ofSeconds(10)));
        System.out.println("RESULT:" + FlashcatOtel.diagnostics().toJson());
        // The asynchronous client's thread pool uses non-daemon threads.
        System.exit(0);
    }
}
