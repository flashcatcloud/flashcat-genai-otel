package otlp

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

// TraceID is sha256("<tool>|<SessionID>|<TurnKey>")[:16] (CONTRACT §4.5).
// <tool> is the model.Tool value (claude-code / codex / cursor).
func TraceID(tool, session, turnKey string) pcommon.TraceID {
	h := sha256.Sum256([]byte(tool + "|" + session + "|" + turnKey))
	var id pcommon.TraceID
	copy(id[:], h[:16])
	return id
}

// spanID is sha256(trace_id || suffix)[:8] over the 16 raw trace id bytes.
func spanID(trace pcommon.TraceID, suffix string) pcommon.SpanID {
	h := sha256.New()
	h.Write(trace[:])
	h.Write([]byte(suffix))
	var id pcommon.SpanID
	copy(id[:], h.Sum(nil)[:8])
	return id
}

// RootSpanID is the span id of a turn's invoke_agent root.
func RootSpanID(trace pcommon.TraceID) pcommon.SpanID { return spanID(trace, "root") }

// StepSpanID is the span id of a step; path is the Key chain from the root,
// joined with "/" (steps inside a subagent are prefixed with the key of the
// tool that spawned it).
func StepSpanID(trace pcommon.TraceID, path string) pcommon.SpanID {
	return spanID(trace, "step|"+path)
}

// SubagentSpanID is the span id of the invoke_agent span a tool spawned;
// toolPath is the path of that tool step.
func SubagentSpanID(trace pcommon.TraceID, toolPath string) pcommon.SpanID {
	return spanID(trace, "subagent|"+toolPath)
}

// TraceParent is a parsed W3C traceparent.
type TraceParent struct {
	TraceID pcommon.TraceID
	SpanID  pcommon.SpanID
}

// ParseTraceParent parses "00-<32 hex>-<16 hex>-<2 hex>". ok=false when the
// value is empty or invalid (including all-zero ids).
func ParseTraceParent(v string) (TraceParent, bool) {
	var tp TraceParent
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || parts[0] == "ff" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return tp, false
	}
	if parts[0] == "00" && len(parts) != 4 {
		return tp, false
	}
	t, err1 := hex.DecodeString(parts[1])
	s, err2 := hex.DecodeString(parts[2])
	_, err3 := hex.DecodeString(parts[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return tp, false
	}
	copy(tp.TraceID[:], t)
	copy(tp.SpanID[:], s)
	if tp.TraceID.IsEmpty() || tp.SpanID.IsEmpty() {
		return tp, false
	}
	return tp, true
}
