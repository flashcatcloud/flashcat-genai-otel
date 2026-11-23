package flashcatgenai

import (
	"context"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	attrConversationID = "gen_ai.conversation.id"
	attrUserID         = "user.id"
	attrTagPrefix      = "flashcat.tag."
	attrOperationName  = "gen_ai.operation.name"
)

// Session holds session context (contract §5).
type Session struct {
	// ConversationID sets gen_ai.conversation.id when the span has no value for it.
	ConversationID string
	// UserID sets user.id.
	UserID string
	// Tags are written as flashcat.tag.<k> and merged across nested sessions.
	Tags map[string]string
	// Propagate adds baggage for gen_ai.conversation.id and user.id, which travels in HTTP headers to downstream services,
	// including model providers. Baggage is off by default.
	Propagate bool
}

type sessionKey struct{}

type sessionValue struct {
	conv string
	user string
	tags map[string]string
}

// WithSession returns a context carrying the session. The session processor adds session attributes to every span
// started from that context or a derived context.
// Baggage is off by default.
func WithSession(ctx context.Context, s Session) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	prev, _ := ctx.Value(sessionKey{}).(*sessionValue)
	v := &sessionValue{}
	if prev != nil {
		v.conv, v.user = prev.conv, prev.user
		if len(prev.tags) > 0 {
			v.tags = make(map[string]string, len(prev.tags)+len(s.Tags))
			for k, val := range prev.tags {
				v.tags[k] = val
			}
		}
	}
	if s.ConversationID != "" {
		v.conv = s.ConversationID
	}
	if s.UserID != "" {
		v.user = s.UserID
	}
	if len(s.Tags) > 0 {
		if v.tags == nil {
			v.tags = make(map[string]string, len(s.Tags))
		}
		for k, val := range s.Tags {
			v.tags[k] = val
		}
	}
	ctx = context.WithValue(ctx, sessionKey{}, v)
	if s.Propagate {
		bag := baggage.FromContext(ctx)
		if s.ConversationID != "" {
			if m, err := baggage.NewMemberRaw(attrConversationID, s.ConversationID); err == nil {
				bag, _ = bag.SetMember(m)
			}
		}
		if s.UserID != "" {
			if m, err := baggage.NewMemberRaw(attrUserID, s.UserID); err == nil {
				bag, _ = bag.SetMember(m)
			}
		}
		ctx = baggage.ContextWithBaggage(ctx, bag)
	}
	return ctx
}

var inferenceOps = map[string]bool{"chat": true, "text_completion": true, "generate_content": true, "embeddings": true}

const nestedInferenceProblem = "手工 llm / embedding span 里又有被插桩的模型调用，同一次调用会重复计数：只用 llm / embedding 标没有被插桩的模型客户端，或去掉外层的手工 span"

// sessionProcessor handles only span starts. It adds session attributes to spans started within the session scope
// and inherits them from in-process parent spans. It also sets the AI root marker and counts orphan inference spans.
type sessionProcessor struct{}

func (sessionProcessor) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	defer func() { _ = recover() }() // Keep SDK failures from affecting application code.
	var conv, user string
	var tags map[string]string
	if v, ok := parent.Value(sessionKey{}).(*sessionValue); ok && v != nil {
		conv, user, tags = v.conv, v.user, v.tags
	}
	// Inherit session attributes from an in-process SDK parent span even if the private context key is unavailable.
	var inherited map[string]string
	var parentAttrs []attribute.KeyValue
	p, _ := trace.SpanFromContext(parent).(sdktrace.ReadOnlySpan)
	if p != nil {
		parentAttrs = p.Attributes()
		for _, kv := range parentAttrs {
			k := string(kv.Key)
			switch {
			case k == attrConversationID && conv == "":
				conv = kv.Value.Emit()
			case k == attrUserID && user == "":
				user = kv.Value.Emit()
			case strings.HasPrefix(k, attrTagPrefix):
				if _, own := tags[strings.TrimPrefix(k, attrTagPrefix)]; !own {
					if inherited == nil {
						inherited = map[string]string{}
					}
					inherited[k] = kv.Value.Emit()
				}
			}
		}
	}
	hasConv, op, oiKind := false, "", ""
	spanAttrs := s.Attributes()
	for _, kv := range spanAttrs {
		switch string(kv.Key) {
		case attrConversationID:
			hasConv = true
		case attrOperationName:
			op = kv.Value.AsString()
		case "openinference.span.kind":
			oiKind = kv.Value.AsString()
		}
	}
	var attrs []attribute.KeyValue
	if conv != "" && !hasConv {
		attrs = append(attrs, attribute.String(attrConversationID, conv))
	}
	if user != "" {
		attrs = append(attrs, attribute.String(attrUserID, user))
	}
	for k, v := range inherited {
		attrs = append(attrs, attribute.String(k, v))
	}
	for k, v := range tags {
		attrs = append(attrs, attribute.String(attrTagPrefix+k, v))
	}
	// Check ancestry for every span because a regular span can sit between two GenAI spans.
	parentSC := s.Parent()
	hasGenAIAncestor := false
	if parentSC.IsValid() && !parentSC.IsRemote() {
		hasGenAIAncestor = global.ancestry.has(parentSC) ||
			(p != nil && identity(p.SpanContext()) == identity(parentSC) && isGenAISpan(p, parentAttrs))
		if hasGenAIAncestor {
			global.ancestry.remember(s.SpanContext())
		}
	}
	if isGenAISpan(s, spanAttrs) {
		attrs = append(attrs, attribute.Bool(attrGenAIRoot, !hasGenAIAncestor))
	}
	if len(attrs) > 0 {
		s.SetAttributes(attrs...)
	}
	if !parentSC.IsValid() && inferenceOps[op] {
		global.orphans.Add(1)
	}
	// A semantic API LLM or Embedding parent can cause the same call to be counted twice (contract §4.1).
	if inferenceOps[op] || oiKind == "LLM" || oiKind == "EMBEDDING" {
		if f, ok := parent.Value(frameKey{}).(*Span); ok && f != nil && (f.kind == kindLLM || f.kind == kindEmbedding) &&
			f.span != nil && f.span.SpanContext().SpanID() == trace.SpanContextFromContext(parent).SpanID() {
			global.nested.Add(1)
			if global.nestedWarned.CompareAndSwap(false, true) {
				slog.Warn("flashcat-genai-otel: " + nestedInferenceProblem)
			}
		}
	}
}

func (sessionProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (sessionProcessor) Shutdown(context.Context) error   { return nil }
func (sessionProcessor) ForceFlush(context.Context) error { return nil }
