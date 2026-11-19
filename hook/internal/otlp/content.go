package otlp

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

// MaxAttrChars is the per-attribute content limit (CONTRACT §4.4).
const MaxAttrChars = 20000

const truncMark = "…[truncated]"

// otelPart / otelMessage follow the OTel GenAI JSON message schema.
type otelPart struct {
	Type      string `json:"type"`
	Content   string `json:"content,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments any    `json:"arguments,omitempty"`
	Response  any    `json:"response,omitempty"`
}

type otelMessage struct {
	Role         string     `json:"role"`
	Parts        []otelPart `json:"parts"`
	FinishReason string     `json:"finish_reason,omitempty"`
}

// toolArgs embeds valid JSON arguments as JSON, anything else as a string.
func toolArgs(s string) any {
	if s == "" {
		return nil
	}
	b := []byte(s)
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	return s
}

// convertMessages maps model messages to the OTel shape. withTool=false drops
// tool_call arguments and tool_call_response results (no_tool_content).
func convertMessages(msgs []model.Message, withTool bool, finish string) []otelMessage {
	out := make([]otelMessage, 0, len(msgs))
	for _, m := range msgs {
		om := otelMessage{Role: m.Role, Parts: make([]otelPart, 0, len(m.Parts))}
		for _, p := range m.Parts {
			op := otelPart{Type: p.Type}
			switch p.Type {
			case "tool_call":
				op.ID, op.Name = p.ID, p.Name
				if withTool {
					op.Arguments = toolArgs(p.Arguments)
				}
			case "tool_call_response":
				op.ID = p.ID
				if withTool {
					op.Response = p.Response
				}
			default:
				op.Content = p.Content
			}
			om.Parts = append(om.Parts, op)
		}
		if m.Role == "assistant" {
			om.FinishReason = finish
		}
		out = append(out, om)
	}
	return out
}

func textMessage(role, text, finish string) []otelMessage {
	m := otelMessage{Role: role, Parts: []otelPart{{Type: "text", Content: text}}, FinishReason: finish}
	return []otelMessage{m}
}

func marshal(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}

// marshalMessages serializes messages within limit characters, shortening the
// longest text fields inside the parts so the result stays valid JSON.
func marshalMessages(msgs []otelMessage, limit int) (string, bool) {
	s := marshal(msgs)
	if utf8.RuneCountInString(s) <= limit {
		return s, false
	}
	for iter := 0; iter < 64; iter++ {
		over := utf8.RuneCountInString(s) - limit
		if over <= 0 {
			return s, true
		}
		field := longestField(msgs)
		if field == nil {
			break
		}
		n := utf8.RuneCountInString(*field)
		keep := n - over - 2*utf8.RuneCountInString(truncMark) - 16
		if keep < 0 {
			keep = 0
		}
		if keep >= n {
			keep = n / 2
		}
		*field = truncateRunes(*field, keep) + truncMark
		s = marshal(msgs)
	}
	// Still too long (many small parts): drop trailing parts.
	for len(msgs) > 0 && utf8.RuneCountInString(s) > limit {
		last := &msgs[len(msgs)-1]
		if len(last.Parts) > 1 {
			last.Parts = last.Parts[:len(last.Parts)-1]
		} else {
			msgs = msgs[:len(msgs)-1]
		}
		s = marshal(msgs)
	}
	return s, true
}

// longestField returns a pointer to the longest truncatable string in msgs.
// Once truncation is needed, tool arguments and results become JSON strings
// (held as *string) so they can be shortened like text.
func longestField(msgs []otelMessage) *string {
	var best *string
	for i := range msgs {
		for j := range msgs[i].Parts {
			p := &msgs[i].Parts[j]
			p.Arguments = asStringPtr(p.Arguments)
			p.Response = asStringPtr(p.Response)
			args, _ := p.Arguments.(*string)
			resp, _ := p.Response.(*string)
			for _, f := range []*string{&p.Content, args, resp} {
				if f != nil && (best == nil || len(*f) > len(*best)) {
					best = f
				}
			}
		}
	}
	if best == nil || len(*best) <= 2*len(truncMark) {
		return nil
	}
	return best
}

func asStringPtr(v any) any {
	switch x := v.(type) {
	case json.RawMessage:
		s := string(x)
		return &s
	case string:
		return &x
	}
	return v
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// truncateText cuts a plain string attribute to limit characters.
func truncateText(s string, limit int) (string, bool) {
	if utf8.RuneCountInString(s) <= limit {
		return s, false
	}
	return truncateRunes(s, limit-utf8.RuneCountInString(truncMark)) + truncMark, true
}

// systemInstructions is gen_ai.system_instructions: a list of text parts.
func systemInstructions(text string) []otelPart {
	return []otelPart{{Type: "text", Content: text}}
}
