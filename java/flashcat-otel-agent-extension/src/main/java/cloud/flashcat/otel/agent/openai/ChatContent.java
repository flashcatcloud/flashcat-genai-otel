package cloud.flashcat.otel.agent.openai;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.JsonNodeFactory;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.openai.models.chat.completions.ChatCompletion;
import com.openai.models.chat.completions.ChatCompletionChunk;
import com.openai.models.chat.completions.ChatCompletionCreateParams;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanBuilder;
import java.util.List;
import java.util.Optional;
import java.util.TreeMap;

/**
 * Content for Chat Completions fallback spans when the content capture switch is on. The format
 * matches {@link ResponsesContent} and the Go middleware ({@code chatInputMessages} and
 * {@code chatOutputMessages}). Messages with the {@code tool} role use {@code tool_call_response}
 * parts, and tool calls use {@code tool_call} parts.
 */
final class ChatContent {
    private static final JsonNodeFactory NODES = JsonNodeFactory.instance;

    private ChatContent() {}

    /** Maps request messages to {@code gen_ai.input.messages}. */
    static void request(SpanBuilder b, ChatCompletionCreateParams p) {
        try {
            JsonNode body = ResponsesContent.mapper().valueToTree(p._body());
            JsonNode messages = body.get("messages");
            if (messages == null || !messages.isArray()) {
                return;
            }
            ArrayNode out = NODES.arrayNode();
            for (JsonNode m : messages) {
                String role = ResponsesContent.text(m, "role");
                ObjectNode msg = ResponsesContent.message(role, null);
                ArrayNode parts = (ArrayNode) msg.get("parts");
                String text = ResponsesContent.contentText(m.get("content"));
                if ("tool".equals(role)) {
                    ObjectNode part = NODES.objectNode();
                    part.put("type", "tool_call_response");
                    ResponsesContent.putIfNotNull(part, "id", ResponsesContent.text(m, "tool_call_id"));
                    part.put("response", ResponsesContent.truncate(text));
                    parts.add(part);
                } else if (!text.isEmpty()) {
                    parts.add(ResponsesContent.textPart(text));
                }
                addToolCalls(parts, m.get("tool_calls"));
                out.add(msg);
            }
            if (out.size() > 0) {
                b.setAttribute(OpenAiTelemetry.INPUT_MESSAGES, ResponsesContent.write(out));
            }
        } catch (Throwable ignored) {
            // Keep the span if content capture fails.
        }
    }

    /** Maps non-streaming response choices to {@code gen_ai.output.messages}. */
    static void response(Span span, ChatCompletion r) {
        try {
            JsonNode body = ResponsesContent.mapper().valueToTree(r);
            JsonNode choices = body.get("choices");
            if (choices == null || !choices.isArray()) {
                return;
            }
            ArrayNode out = NODES.arrayNode();
            for (JsonNode c : choices) {
                JsonNode m = c.get("message");
                String role = ResponsesContent.text(m, "role");
                ObjectNode msg = ResponsesContent.message(role != null ? role : "assistant", null);
                ArrayNode parts = (ArrayNode) msg.get("parts");
                String text = ResponsesContent.contentText(m == null ? null : m.get("content"));
                if (text.isEmpty()) {
                    String refusal = ResponsesContent.text(m, "refusal");
                    text = refusal == null ? "" : refusal;
                }
                if (!text.isEmpty()) {
                    parts.add(ResponsesContent.textPart(text));
                }
                addToolCalls(parts, m == null ? null : m.get("tool_calls"));
                String reason = ResponsesContent.text(c, "finish_reason");
                if (reason != null) {
                    msg.put("finish_reason", reason);
                }
                out.add(msg);
            }
            span.setAttribute(OpenAiTelemetry.OUTPUT_MESSAGES, ResponsesContent.write(out));
        } catch (Throwable ignored) {
            // Keep the span if content capture fails.
        }
    }

    /** Maps accumulated streaming output to {@code gen_ai.output.messages}. */
    static void streamed(Span span, List<ChoiceAcc> choices) {
        try {
            ArrayNode out = NODES.arrayNode();
            for (ChoiceAcc a : choices) {
                ObjectNode msg = ResponsesContent.message("assistant", null);
                ArrayNode parts = (ArrayNode) msg.get("parts");
                String text = a.text.length() > 0 ? a.text.toString() : a.refusal.toString();
                if (!text.isEmpty()) {
                    parts.add(ResponsesContent.textPart(text));
                }
                for (ToolAcc t : a.tools.values()) {
                    parts.add(ResponsesContent.toolCall(t.id, t.name, t.arguments.toString()));
                }
                if (a.finishReason != null) {
                    msg.put("finish_reason", a.finishReason);
                }
                out.add(msg);
            }
            if (out.size() > 0) {
                span.setAttribute(OpenAiTelemetry.OUTPUT_MESSAGES, ResponsesContent.write(out));
            }
        } catch (Throwable ignored) {
            // Keep the span if content capture fails.
        }
    }

    private static void addToolCalls(ArrayNode parts, JsonNode toolCalls) {
        if (toolCalls == null || !toolCalls.isArray()) {
            return;
        }
        for (JsonNode tc : toolCalls) {
            JsonNode fn = tc.get("function");
            parts.add(ResponsesContent.toolCall(ResponsesContent.text(tc, "id"), ResponsesContent.text(fn, "name"), ResponsesContent.text(fn, "arguments")));
        }
    }

    /** Accumulates one streaming choice. Discards text beyond the limit to bound memory use. */
    static final class ChoiceAcc {
        volatile String finishReason;
        final StringBuilder text = new StringBuilder();
        final StringBuilder refusal = new StringBuilder();
        final TreeMap<Long, ToolAcc> tools = new TreeMap<Long, ToolAcc>();

        void append(ChatCompletionChunk.Choice.Delta d) {
            appendBounded(text, d.content());
            appendBounded(refusal, d.refusal());
            Optional<List<ChatCompletionChunk.Choice.Delta.ToolCall>> calls = d.toolCalls();
            if (!calls.isPresent()) {
                return;
            }
            for (ChatCompletionChunk.Choice.Delta.ToolCall tc : calls.get()) {
                ToolAcc t = tools.get(tc.index());
                if (t == null) {
                    if (tools.size() >= 16) {
                        continue;
                    }
                    t = new ToolAcc();
                    tools.put(tc.index(), t);
                }
                if (tc.id().isPresent()) {
                    t.id = tc.id().get();
                }
                if (tc.function().isPresent()) {
                    if (tc.function().get().name().isPresent()) {
                        t.name = tc.function().get().name().get();
                    }
                    appendBounded(t.arguments, tc.function().get().arguments());
                }
            }
        }

        private static void appendBounded(StringBuilder sb, Optional<String> s) {
            if (s.isPresent() && sb.length() <= ResponsesContent.LIMIT) {
                sb.append(s.get());
            }
        }
    }

    /** Accumulates one streaming tool call. */
    static final class ToolAcc {
        String id;
        String name;
        final StringBuilder arguments = new StringBuilder();
    }
}
