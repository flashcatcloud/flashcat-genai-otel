package cloud.flashcat.genai.it;

import cloud.flashcat.genai.Diagnostics;
import cloud.flashcat.genai.Flashcat;
import cloud.flashcat.genai.FlashcatGenAI;
import com.openai.client.OpenAIClient;
import com.openai.client.okhttp.OpenAIOkHttpClient;
import com.openai.models.ChatModel;
import com.openai.models.chat.completions.ChatCompletion;
import com.openai.models.chat.completions.ChatCompletionCreateParams;
import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.Tracer;
import io.opentelemetry.context.Scope;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.time.Duration;

/**
 * End-to-end test app launched by AgentExtensionIT with the OpenTelemetry Java Agent and Flashcat extension.
 * Takes the mock model server's base URL, such as http://127.0.0.1:1234/v1.
 */
public final class AgentApp {
    private AgentApp() {}

    public static void main(String[] args) throws Exception {
        String base = args[0];
        String explicitContent = System.getenv("IT_EXPLICIT_CONTENT");
        Diagnostics init = explicitContent == null
                ? FlashcatGenAI.init()
                : FlashcatGenAI.init(FlashcatGenAI.Options.builder().captureContent(explicitContent).build());
        System.out.println("INIT:" + init.toJson());

        OpenAIClient client = OpenAIOkHttpClient.builder().baseUrl(base).apiKey("sk-test").maxRetries(0).build();
        ChatCompletionCreateParams params = ChatCompletionCreateParams.builder()
                .model(ChatModel.GPT_4O)
                .addUserMessage("我的订单到哪了")
                .build();

        try (Flashcat.Session s = Flashcat.session().conversationId("c-agent").userId("u-agent").tag("tenant", "t1").start()) {
            String answer = Flashcat.agent("kefu", () -> {
                Flashcat.tool("lookup_order", () -> "shipped");
                // Create a child span with the standard OTel API through the Agent bridge.
                Tracer tracer = GlobalOpenTelemetry.getOrNoop().getTracer("app-manual");
                Span manual = tracer.spanBuilder("manual-child").startSpan();
                try (Scope ignored = manual.makeCurrent()) {
                    // Simulate model instrumentation that sets GenAI attributes just before span end.
                    Span late = tracer.spanBuilder("late-model").startSpan();
                    late.setAttribute("gen_ai.operation.name", "chat").end();
                } finally {
                    manual.end();
                }
                // The Agent instruments this HTTP client call as a child span.
                httpGet(base + "/ping-inside");
                // The Agent instruments this openai-java model call.
                String content;
                try {
                    ChatCompletion cc = client.chat().completions().create(params);
                    content = cc.choices().get(0).message().content().orElse("");
                } catch (RuntimeException e) {
                    content = "openai-error:" + e;
                }
                return content;
            });
            System.out.println("ANSWER:" + answer);
            // Under the Agent, a span within a session needs an enclosing Flashcat span to inherit session attributes.
            httpGet(base + "/ping-outside");
        }

        // Manual llm and annotate (contract §4.2). annotate must find the semantic span in the application Context through the Agent bridge.
        // The extension's session processor uses the parent span to detect the instrumented call as nested inference (contract §4.1).
        Flashcat.llm("gateway-model", Flashcat.SpanOptions.create().provider("custom"), () -> {
            Flashcat.annotate(Flashcat.Annotation.create().inputTokens(11).outputTokens(2).model("gateway-model-v1").tag("route", "gw"));
            try {
                client.chat().completions().create(params);
            } catch (RuntimeException e) {
                System.out.println("NESTED-ERROR:" + e);
            }
        });

        System.out.println("FLUSH:" + FlashcatGenAI.flush(Duration.ofSeconds(10)));
        System.out.println("RESULT:" + FlashcatGenAI.diagnostics().toJson());
    }

    private static void httpGet(String url) {
        try {
            HttpURLConnection c = (HttpURLConnection) new URL(url).openConnection();
            c.setConnectTimeout(2000);
            c.setReadTimeout(2000);
            int code = c.getResponseCode();
            InputStream in = code < 400 ? c.getInputStream() : c.getErrorStream();
            if (in != null) {
                while (in.read() >= 0) {
                    // Consume the entire stream.
                }
                in.close();
            }
        } catch (Exception e) {
            System.out.println("HTTP-ERROR:" + e);
        }
    }
}
