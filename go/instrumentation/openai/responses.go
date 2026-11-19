package flashcatopenai

import (
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Flashcat adds Responses API (POST /responses) parsing to cover an endpoint the upstream otelc middleware does not handle.

type responsesRequest struct {
	Model           string          `json:"model"`
	MaxOutputTokens *int64          `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Stream          bool            `json:"stream"`
	Instructions    json.RawMessage `json:"instructions"`
	Input           json.RawMessage `json:"input"`
}

func parseResponsesRequest(body []byte, capture bool) requestInfo {
	var req responsesRequest
	if json.Unmarshal(body, &req) != nil {
		return requestInfo{}
	}
	info := requestInfo{model: req.Model}
	if req.MaxOutputTokens != nil {
		info.attrs = append(info.attrs, attribute.Int64("gen_ai.request.max_tokens", *req.MaxOutputTokens))
	}
	if req.Temperature != nil {
		info.attrs = append(info.attrs, attribute.Float64("gen_ai.request.temperature", *req.Temperature))
	}
	if req.TopP != nil {
		info.attrs = append(info.attrs, attribute.Float64("gen_ai.request.top_p", *req.TopP))
	}
	if !capture {
		return info
	}
	var instructions string
	if json.Unmarshal(req.Instructions, &instructions) == nil && instructions != "" {
		info.systemInstructions = marshalParts([]part{textPart(instructions)})
	}
	info.inputMessages = marshalMessages(responsesInputMessages(req.Input))
	return info
}

type responsesContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

// responsesInputMessages handles input as a string or an array of message, function_call, or function_call_output items.
func responsesInputMessages(raw json.RawMessage) []message {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []message{{Role: "user", Parts: []part{textPart(s)}}}
	}
	var items []struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []message
	for _, it := range items {
		switch {
		case it.Type == "function_call":
			out = append(out, message{Role: "assistant", Parts: []part{{Type: "tool_call", ID: it.CallID, Name: it.Name, Arguments: toolArguments(it.Arguments)}}})
		case it.Type == "function_call_output":
			out = append(out, message{Role: "tool", Parts: []part{{Type: "tool_call_response", ID: it.CallID, Response: truncateText(contentFromJSON(it.Output))}}})
		case it.Type == "message" || (it.Type == "" && it.Role != ""):
			msg := message{Role: it.Role}
			if text := contentFromJSON(it.Content); text != "" {
				msg.Parts = append(msg.Parts, textPart(text))
			}
			out = append(out, msg)
		}
	}
	return out
}

type responsesBody struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Output []struct {
		Type      string             `json:"type"`
		Role      string             `json:"role"`
		Content   []responsesContent `json:"content"`
		CallID    string             `json:"call_id"`
		Name      string             `json:"name"`
		Arguments string             `json:"arguments"`
	} `json:"output"`
	Usage *struct {
		InputTokens        *int64 `json:"input_tokens"`
		OutputTokens       *int64 `json:"output_tokens"`
		TotalTokens        *int64 `json:"total_tokens"`
		InputTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

func responsesFinishReason(b *responsesBody) string {
	switch b.Status {
	case "completed":
		return "stop"
	case "incomplete":
		if b.IncompleteDetails != nil && b.IncompleteDetails.Reason != "" {
			return b.IncompleteDetails.Reason
		}
		return "incomplete"
	case "failed":
		return "error"
	}
	return b.Status
}

// applyResponsesBody records a complete Response on the span, from either a non-streaming response body
// or the response field of a streaming response.completed event.
func applyResponsesBody(span trace.Span, b *responsesBody, capture bool) {
	var attrs []attribute.KeyValue
	if b.ID != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.id", b.ID))
	}
	if b.Model != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.model", b.Model))
	}
	reason := responsesFinishReason(b)
	if reason != "" {
		attrs = append(attrs, attribute.StringSlice("gen_ai.response.finish_reasons", []string{reason}))
	}
	if u := b.Usage; u != nil {
		if u.InputTokens != nil {
			attrs = append(attrs, attribute.Int64("gen_ai.usage.input_tokens", *u.InputTokens))
		}
		if u.OutputTokens != nil {
			attrs = append(attrs, attribute.Int64("gen_ai.usage.output_tokens", *u.OutputTokens))
		}
		if u.TotalTokens != nil {
			attrs = append(attrs, attribute.Int64("gen_ai.usage.total_tokens", *u.TotalTokens))
		}
		if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
			attrs = append(attrs, attribute.Int64("gen_ai.usage.cache_read.input_tokens", u.InputTokensDetails.CachedTokens))
		}
	}
	if capture {
		msg := message{Role: "assistant", FinishReason: reason}
		for _, item := range b.Output {
			switch item.Type {
			case "message":
				for _, c := range item.Content {
					if c.Text != "" {
						msg.Parts = append(msg.Parts, textPart(c.Text))
					} else if c.Refusal != "" {
						msg.Parts = append(msg.Parts, textPart(c.Refusal))
					}
				}
			case "function_call":
				msg.Parts = append(msg.Parts, part{Type: "tool_call", ID: item.CallID, Name: item.Name, Arguments: toolArguments(item.Arguments)})
			}
		}
		if out := marshalMessages([]message{msg}); out != "" {
			attrs = append(attrs, attribute.String("gen_ai.output.messages", out))
		}
	}
	span.SetAttributes(attrs...)
}
