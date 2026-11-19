package cloud.flashcat.otel.agent.openai;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.JsonNodeFactory;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.openai.core.ObjectMappers;
import com.openai.models.responses.Response;
import com.openai.models.responses.ResponseCreateParams;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.SpanBuilder;

/**
 * Responses API content captured when the content capture switch is on. The format matches
 * contract §4 and the Go middleware. {@link ChatContent} shares these helpers for Chat Completions.
 *
 * <ul>
 *   <li>{@code gen_ai.input.messages} and {@code gen_ai.output.messages} use {@code [{"role":…,"parts":[…]}]}.
 *       Output includes {@code finish_reason}. Parts are {@code text}, {@code tool_call}
 *       ({@code id}, {@code name}, {@code arguments}), or {@code tool_call_response} ({@code id}, {@code response}).</li>
 *   <li>{@code gen_ai.system_instructions} uses {@code [{"type":"text","content":…}]}.</li>
 * </ul>
 *
 * <p>Uses openai-java's Jackson mappings to read request and response fields from JSON trees,
 * avoiding version-specific typed accessors. Text segments longer than 32,768 UTF-16 code units
 * are truncated and get the suffix {@code …[truncated]}. The Flashcat export pipeline masks content.
 */
final class ResponsesContent {
    static final int LIMIT = 32 * 1024;
    static final String TRUNCATED_SUFFIX = "…[truncated]";
    private static final JsonNodeFactory NODES = JsonNodeFactory.instance;

    private ResponsesContent() {}

    /** Maps request input to {@code gen_ai.input.messages} and instructions to {@code gen_ai.system_instructions}. */
    static void request(SpanBuilder b, ResponseCreateParams p) {
        try {
            JsonNode body = mapper().valueToTree(p._body());
            JsonNode instructions = body.get("instructions");
            if (instructions != null && instructions.isTextual() && !instructions.asText().isEmpty()) {
                ArrayNode parts = NODES.arrayNode();
                parts.add(textPart(instructions.asText()));
                b.setAttribute(OpenAiTelemetry.SYSTEM_INSTRUCTIONS, write(parts));
            }
            ArrayNode messages = inputMessages(body.get("input"));
            if (messages.size() > 0) {
                b.setAttribute(OpenAiTelemetry.INPUT_MESSAGES, write(messages));
            }
        } catch (Throwable ignored) {
            // Keep the span if content capture fails.
        }
    }

    /** Maps message and function_call items in the output to one assistant message. */
    static void response(Span span, Response r, String finishReason) {
        try {
            JsonNode body = mapper().valueToTree(r);
            ObjectNode msg = NODES.objectNode();
            msg.put("role", "assistant");
            ArrayNode parts = msg.putArray("parts");
            JsonNode output = body.get("output");
            if (output != null && output.isArray()) {
                for (JsonNode item : output) {
                    String type = text(item, "type");
                    if ("message".equals(type)) {
                        JsonNode content = item.get("content");
                        if (content != null && content.isArray()) {
                            for (JsonNode c : content) {
                                String t = text(c, "text");
                                if (t == null || t.isEmpty()) {
                                    t = text(c, "refusal");
                                }
                                if (t != null && !t.isEmpty()) {
                                    parts.add(textPart(t));
                                }
                            }
                        }
                    } else if ("function_call".equals(type)) {
                        parts.add(toolCall(text(item, "call_id"), text(item, "name"), text(item, "arguments")));
                    }
                }
            }
            if (finishReason != null) {
                msg.put("finish_reason", finishReason);
            }
            ArrayNode messages = NODES.arrayNode();
            messages.add(msg);
            span.setAttribute(OpenAiTelemetry.OUTPUT_MESSAGES, write(messages));
        } catch (Throwable ignored) {
            // Keep the span if content capture fails.
        }
    }

    /** Handles input given as a string or an array of message, function_call, or function_call_output items. */
    static ArrayNode inputMessages(JsonNode input) {
        ArrayNode out = NODES.arrayNode();
        if (input == null || input.isNull()) {
            return out;
        }
        if (input.isTextual()) {
            out.add(message("user", textPart(input.asText())));
            return out;
        }
        if (!input.isArray()) {
            return out;
        }
        for (JsonNode it : input) {
            String type = text(it, "type");
            String role = text(it, "role");
            if ("function_call".equals(type)) {
                out.add(message("assistant", toolCall(text(it, "call_id"), text(it, "name"), text(it, "arguments"))));
            } else if ("function_call_output".equals(type)) {
                ObjectNode part = NODES.objectNode();
                part.put("type", "tool_call_response");
                putIfNotNull(part, "id", text(it, "call_id"));
                part.put("response", truncate(contentText(it.get("output"))));
                out.add(message("tool", part));
            } else if ("message".equals(type) || (type == null && role != null)) {
                String t = contentText(it.get("content"));
                out.add(t.isEmpty() ? message(role, null) : message(role, textPart(t)));
            }
        }
        return out;
    }

    /** Preserves strings and joins only the text parts of multimodal content arrays, matching Go's contentFromJSON. */
    static String contentText(JsonNode content) {
        if (content == null || content.isNull()) {
            return "";
        }
        if (content.isTextual()) {
            return content.asText();
        }
        if (content.isArray()) {
            StringBuilder sb = new StringBuilder();
            for (JsonNode p : content) {
                String t = text(p, "text");
                if (t != null && !t.isEmpty()) {
                    if (sb.length() > 0) {
                        sb.append('\n');
                    }
                    sb.append(t);
                }
            }
            return sb.toString();
        }
        return content.toString();
    }

    static String truncate(String s) {
        if (s == null || s.length() <= LIMIT) {
            return s;
        }
        int cut = LIMIT;
        if (Character.isHighSurrogate(s.charAt(cut - 1))) {
            cut--;
        }
        return s.substring(0, cut) + TRUNCATED_SUFFIX;
    }

    static ObjectNode message(String role, ObjectNode part) {
        ObjectNode m = NODES.objectNode();
        m.put("role", role);
        ArrayNode parts = m.putArray("parts");
        if (part != null) {
            parts.add(part);
        }
        return m;
    }

    static ObjectNode textPart(String s) {
        ObjectNode p = NODES.objectNode();
        p.put("type", "text");
        p.put("content", truncate(s));
        return p;
    }

    /** Parses tool arguments encoded as JSON and preserves the string when it is invalid JSON. */
    static ObjectNode toolCall(String id, String name, String arguments) {
        ObjectNode p = NODES.objectNode();
        p.put("type", "tool_call");
        putIfNotNull(p, "id", id);
        putIfNotNull(p, "name", name);
        String a = arguments == null ? null : arguments.trim();
        if (a != null && !a.isEmpty()) {
            JsonNode parsed = null;
            if (a.length() <= LIMIT) {
                try {
                    parsed = mapper().readTree(a);
                } catch (Exception ignored) {
                    // Fall back to the original string if JSON parsing fails.
                }
            }
            if (parsed != null) {
                p.set("arguments", parsed);
            } else {
                p.put("arguments", truncate(arguments));
            }
        }
        return p;
    }

    static void putIfNotNull(ObjectNode n, String k, String v) {
        if (v != null && !v.isEmpty()) {
            n.put(k, v);
        }
    }

    static String text(JsonNode n, String field) {
        JsonNode v = n == null ? null : n.get(field);
        return v != null && v.isTextual() ? v.asText() : null;
    }

    static String write(JsonNode n) throws Exception {
        return mapper().writeValueAsString(n);
    }

    static ObjectMapper mapper() {
        return ObjectMappers.jsonMapper();
    }
}
