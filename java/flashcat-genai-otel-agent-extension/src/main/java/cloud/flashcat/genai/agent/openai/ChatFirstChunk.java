package cloud.flashcat.genai.agent.openai;

import com.openai.models.chat.completions.ChatCompletionChunk;
import io.opentelemetry.api.trace.Span;
import java.util.List;
import java.util.Optional;

/**
 * Adds {@code gen_ai.response.time_to_first_chunk} to Chat Completions streaming spans from the
 * OpenTelemetry Java Agent's upstream openai-java-1.1 instrumentation.
 *
 * <p>The upstream client proxy makes its chat span current before calling the implementation's
 * {@code createStreaming}, then wraps the returned stream. {@link ChatCall#enterStreaming} detects
 * that span so this observer can record the first chunk with output (text, refusal, or a tool call,
 * as in the Go middleware). The upstream instrumentation owns span creation and completion.
 */
final class ChatFirstChunk implements StreamObserver<ChatCompletionChunk> {
    private final Span span;
    private final long startNanos;
    private volatile boolean marked;

    ChatFirstChunk(Span span, long startNanos) {
        this.span = span;
        this.startNanos = startNanos;
    }

    @Override
    public void onItem(ChatCompletionChunk chunk) {
        if (marked) {
            return;
        }
        try {
            if (hasOutput(chunk)) {
                marked = true;
                span.setAttribute(OpenAiTelemetry.TIME_TO_FIRST_CHUNK, (System.nanoTime() - startNanos) / 1e9);
            }
        } catch (Throwable ignored) {
            // Telemetry failures leave the application call unaffected.
        }
    }

    /** The upstream instrumentation ends the span. */
    @Override
    public void onEnd(Throwable error) {}

    /** Matches the Go markFirst rule: a chunk with nonempty text, a refusal, or a tool call. */
    static boolean hasOutput(ChatCompletionChunk chunk) {
        for (ChatCompletionChunk.Choice c : chunk.choices()) {
            ChatCompletionChunk.Choice.Delta d = c.delta();
            Optional<String> content = d.content();
            if (content.isPresent() && !content.get().isEmpty()) {
                return true;
            }
            Optional<String> refusal = d.refusal();
            if (refusal.isPresent() && !refusal.get().isEmpty()) {
                return true;
            }
            Optional<List<ChatCompletionChunk.Choice.Delta.ToolCall>> tools = d.toolCalls();
            if (tools.isPresent() && !tools.get().isEmpty()) {
                return true;
            }
        }
        return false;
    }
}
