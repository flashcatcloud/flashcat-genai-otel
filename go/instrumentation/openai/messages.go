// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Modifications Copyright 2026 Flashcat Cloud.
// contentFromJSON was ported from open-telemetry/opentelemetry-go-compile-instrumentation,
// instrumentation/github.com/openai/openai-go/v3/middleware.go. Flashcat added the rest (see UPSTREAM.md).

package flashcatopenai

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// When content capture is enabled, write JSON string attributes in the current GenAI semantic convention formats.
// gen_ai.input.messages and gen_ai.output.messages use [{"role":…,"parts":[…]}].
// gen_ai.system_instructions uses [{"type":"text","content":…}].

const (
	contentLimit    = 32 * 1024 // Per-text limit in UTF-8 bytes, matching the core semantic API's serialization limit.
	truncatedSuffix = "…[truncated]"
)

type part struct {
	Type      string `json:"type"`
	Content   string `json:"content,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments any    `json:"arguments,omitempty"`
	Response  any    `json:"response,omitempty"`
}

type message struct {
	Role         string `json:"role"`
	Parts        []part `json:"parts"`
	FinishReason string `json:"finish_reason,omitempty"`
}

func truncateText(s string) string {
	if len(s) <= contentLimit {
		return s
	}
	cut := contentLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedSuffix
}

func textPart(s string) part { return part{Type: "text", Content: truncateText(s)} }

// toolArguments decodes JSON-encoded arguments into a value and keeps the original string if the JSON is invalid.
func toolArguments(s string) any {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil
	}
	if len(t) <= contentLimit && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	return truncateText(s)
}

func marshalMessages(msgs []message) string {
	if len(msgs) == 0 {
		return ""
	}
	for i := range msgs {
		if msgs[i].Parts == nil {
			msgs[i].Parts = []part{}
		}
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		return ""
	}
	return string(b)
}

func marshalParts(parts []part) string {
	if len(parts) == 0 {
		return ""
	}
	b, err := json.Marshal(parts)
	if err != nil {
		return ""
	}
	return string(b)
}

// contentFromJSON returns text from a content field. It preserves strings and joins only the text parts of multimodal arrays.
// Ported from contentFromJSON in the upstream middleware.go.
func contentFromJSON(content json.RawMessage) string {
	content = bytes.TrimSpace(content)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		return ""
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(content)
}

type chatToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatInputMessages converts chat completions request messages to the GenAI input message format.
func chatInputMessages(body []byte) []message {
	var req struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCalls  []chatToolCall  `json:"tool_calls"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	out := make([]message, 0, len(req.Messages))
	for _, m := range req.Messages {
		msg := message{Role: m.Role}
		text := contentFromJSON(m.Content)
		if m.Role == "tool" {
			msg.Parts = append(msg.Parts, part{Type: "tool_call_response", ID: m.ToolCallID, Response: truncateText(text)})
		} else if text != "" {
			msg.Parts = append(msg.Parts, textPart(text))
		}
		for _, tc := range m.ToolCalls {
			msg.Parts = append(msg.Parts, part{Type: "tool_call", ID: tc.ID, Name: tc.Function.Name, Arguments: toolArguments(tc.Function.Arguments)})
		}
		out = append(out, msg)
	}
	return out
}

// completionInputMessages converts the legacy completions prompt (a string or array of strings) to user messages.
func completionInputMessages(body []byte) []message {
	var req struct {
		Prompt json.RawMessage `json:"prompt"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Prompt) == 0 {
		return nil
	}
	var prompts []string
	var one string
	if json.Unmarshal(req.Prompt, &one) == nil {
		prompts = []string{one}
	} else if json.Unmarshal(req.Prompt, &prompts) != nil {
		return nil
	}
	out := make([]message, 0, len(prompts))
	for _, p := range prompts {
		out = append(out, message{Role: "user", Parts: []part{textPart(p)}})
	}
	return out
}

// chatOutputMessages converts chat completions response choices to the GenAI output message format.
func chatOutputMessages(body []byte) []message {
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				Refusal   string          `json:"refusal"`
				ToolCalls []chatToolCall  `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return nil
	}
	out := make([]message, 0, len(resp.Choices))
	for _, c := range resp.Choices {
		role := c.Message.Role
		if role == "" {
			role = "assistant"
		}
		msg := message{Role: role, FinishReason: c.FinishReason}
		if text := contentFromJSON(c.Message.Content); text != "" {
			msg.Parts = append(msg.Parts, textPart(text))
		} else if c.Message.Refusal != "" {
			msg.Parts = append(msg.Parts, textPart(c.Message.Refusal))
		}
		for _, tc := range c.Message.ToolCalls {
			msg.Parts = append(msg.Parts, part{Type: "tool_call", ID: tc.ID, Name: tc.Function.Name, Arguments: toolArguments(tc.Function.Arguments)})
		}
		out = append(out, msg)
	}
	return out
}

func completionOutputMessages(body []byte) []message {
	var resp struct {
		Choices []struct {
			Text         string `json:"text"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return nil
	}
	out := make([]message, 0, len(resp.Choices))
	for _, c := range resp.Choices {
		out = append(out, message{Role: "assistant", Parts: []part{textPart(c.Text)}, FinishReason: c.FinishReason})
	}
	return out
}
