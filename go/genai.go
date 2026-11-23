package flashcatgenai

import (
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	attrGenAIRoot      = "flashcat.genai.root"
	genAIAncestryLimit = 65536
)

// isGenAI uses the same rule as the Flashcat backend to classify GenAI spans (contract §5.1).
func isGenAI(attrs []attribute.KeyValue) bool {
	var hasOperation, hasAIAttr bool
	for _, kv := range attrs {
		key := string(kv.Key)
		switch key {
		case attrConversationID:
			continue // Session attributes also appear on ordinary spans.
		case "flashcat.span.kind", "ai.operationId":
			return true
		case "operation.name":
			hasOperation = true
		}
		// The remaining GenAI semantic keys use these prefixes.
		if strings.HasPrefix(key, "gen_ai.") || strings.HasPrefix(key, "llm.") ||
			strings.HasPrefix(key, "openinference.") || strings.HasPrefix(key, "traceloop.") ||
			strings.HasPrefix(key, "langfuse.observation.") {
			return true
		}
		hasAIAttr = hasAIAttr || strings.HasPrefix(key, "ai.")
	}
	return hasOperation && hasAIAttr
}

func isGenAISpan(s sdktrace.ReadOnlySpan, attrs []attribute.KeyValue) bool {
	// Some OpenInference instrumentation adds attributes at span end. Identify those spans at start by scope name.
	return strings.HasPrefix(s.InstrumentationScope().Name, "openinference.instrumentation.") || isGenAI(attrs)
}

type spanIdentity struct {
	traceID trace.TraceID
	spanID  trace.SpanID
}

func identity(sc trace.SpanContext) spanIdentity {
	return spanIdentity{traceID: sc.TraceID(), spanID: sc.SpanID()}
}

// genAIAncestry tracks only spans with an in-process GenAI ancestor and evicts them in start order.
// Ordinary APM traffic consumes no storage.
type genAIAncestry struct {
	mu    sync.RWMutex
	ids   map[spanIdentity]struct{}
	order []spanIdentity
	next  int
}

func (a *genAIAncestry) has(sc trace.SpanContext) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, ok := a.ids[identity(sc)]
	return ok
}

func (a *genAIAncestry) remember(sc trace.SpanContext) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ids == nil {
		a.ids = make(map[spanIdentity]struct{})
	}
	id := identity(sc)
	if _, ok := a.ids[id]; ok {
		return
	}
	if len(a.order) < genAIAncestryLimit {
		a.order = append(a.order, id)
	} else {
		delete(a.ids, a.order[a.next])
		a.order[a.next] = id
		a.next = (a.next + 1) % genAIAncestryLimit
	}
	a.ids[id] = struct{}{}
}

// genAIRootProcessor fills in AI root markers from final attributes before batching can lead to ancestry record eviction.
type genAIRootProcessor struct{ sdktrace.SpanProcessor }

func (p genAIRootProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	attrs := s.Attributes()
	if !slices.ContainsFunc(attrs, func(kv attribute.KeyValue) bool { return kv.Key == attrGenAIRoot }) && isGenAI(attrs) {
		out := make([]attribute.KeyValue, len(attrs)+1)
		copy(out, attrs)
		out[len(attrs)] = attribute.Bool(attrGenAIRoot, !global.ancestry.has(s.SpanContext()))
		s = spanWithAttrs{ReadOnlySpan: s, attrs: out}
	}
	p.SpanProcessor.OnEnd(s)
}
