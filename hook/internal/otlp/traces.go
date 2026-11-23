// Package otlp turns model.Turns into OTLP traces (CONTRACT §4) and sends them.
package otlp

import (
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/model"
)

// Attribute keys and fixed values.
const (
	ScopeName = "flashcat-hook"
	SourceKey = "flashcat.source"
	Source    = "coding_agent"

	attrOperation   = "gen_ai.operation.name"
	attrAgentName   = "gen_ai.agent.name"
	attrSession     = "gen_ai.conversation.id"
	attrUser        = "user.id"
	attrRepo        = "flashcat.tag.repo"
	attrBranch      = "flashcat.tag.git_branch"
	attrSubagent    = "flashcat.tag.subagent"
	attrCodingTool  = "flashcat.tag.coding_tool"
	attrTruncated   = "flashcat.content_truncated"
	attrInputMsgs   = "gen_ai.input.messages"
	attrOutputMsgs  = "gen_ai.output.messages"
	attrSystemInstr = "gen_ai.system_instructions"

	minSpan = time.Millisecond
)

// Resource is what every request carries besides the per-tool values.
type Resource struct {
	App         string // service.name
	HostName    string // host.name
	HookVersion string // telemetry.sdk.version
}

// Options control encoding.
type Options struct {
	Content model.Content
	// TraceParent is the TRACEPARENT value; when valid, roots become its
	// children and use its trace id.
	TraceParent string
	// MaxAttrChars overrides MaxAttrChars (tests).
	MaxAttrChars int
}

// Build encodes turns into one ptrace.Traces. Turns with the same tool and
// tool version share a ResourceSpans. Turn.UserID and Turn.Repo must already
// be resolved by the caller.
func Build(turns []model.Turn, res Resource, opt Options) ptrace.Traces {
	td := ptrace.NewTraces()
	if opt.MaxAttrChars <= 0 {
		opt.MaxAttrChars = MaxAttrChars
	}
	tp, hasTP := ParseTraceParent(opt.TraceParent)
	scopes := map[string]ptrace.SpanSlice{}
	for i := range turns {
		t := &turns[i]
		key := string(t.Tool) + "\x00" + t.ToolVersion
		spans, ok := scopes[key]
		if !ok {
			rs := td.ResourceSpans().AppendEmpty()
			fillResource(rs.Resource().Attributes(), res, t.Tool, t.ToolVersion)
			ss := rs.ScopeSpans().AppendEmpty()
			ss.Scope().SetName(ScopeName)
			ss.Scope().SetVersion(res.HookVersion)
			spans = ss.Spans()
			scopes[key] = spans
		}
		e := encoder{turn: t, opt: opt, spans: spans}
		e.traceID = TraceID(string(t.Tool), t.SessionID, t.TurnKey)
		e.exportTrace = e.traceID
		if hasTP {
			e.exportTrace = tp.TraceID
			e.parent = tp.SpanID
		}
		e.encode()
	}
	return td
}

func fillResource(a pcommon.Map, res Resource, tool model.Tool, toolVersion string) {
	a.PutStr("service.name", res.App)
	if toolVersion != "" {
		a.PutStr("service.version", toolVersion)
	}
	a.PutStr("telemetry.sdk.name", ScopeName)
	a.PutStr("telemetry.sdk.version", res.HookVersion)
	a.PutStr(SourceKey, Source)
	a.PutStr(attrCodingTool, string(tool))
	if res.HostName != "" {
		a.PutStr("host.name", res.HostName)
	}
}

type encoder struct {
	turn        *model.Turn
	opt         Options
	spans       ptrace.SpanSlice
	traceID     pcommon.TraceID // derived per turn; span ids hang off it
	exportTrace pcommon.TraceID // written to spans (TRACEPARENT's when set)
	parent      pcommon.SpanID
	firstChat   bool
}

func (e *encoder) withMessages() bool {
	return e.opt.Content == model.ContentNoToolContent || e.opt.Content == model.ContentFull
}
func (e *encoder) withTools() bool { return e.opt.Content == model.ContentFull }

func (e *encoder) encode() {
	t := e.turn
	steps := fillTimes(t.Steps, t.Start, t.End)
	start, end := t.Start, t.End
	start, end = cover(start, end, steps)
	if start.IsZero() {
		start = time.Now()
	}
	if end.Before(start.Add(minSpan)) {
		end = start.Add(minSpan)
	}

	root := e.spans.AppendEmpty()
	rootID := RootSpanID(e.traceID)
	root.SetTraceID(e.exportTrace)
	root.SetSpanID(rootID)
	if !e.parent.IsEmpty() {
		root.SetParentSpanID(e.parent)
	}
	root.SetName("invoke_agent " + string(t.Tool))
	root.SetKind(ptrace.SpanKindInternal)
	root.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
	root.SetEndTimestamp(pcommon.NewTimestampFromTime(end))
	a := root.Attributes()
	a.PutStr(attrOperation, "invoke_agent")
	e.common(a)
	trunc := false
	if e.withMessages() {
		if t.Input != "" {
			trunc = e.putMessages(a, attrInputMsgs, textMessage("user", t.Input, "")) || trunc
		}
		if t.Output != "" {
			trunc = e.putMessages(a, attrOutputMsgs, textMessage("assistant", t.Output, "stop")) || trunc
		}
	}
	if trunc {
		a.PutBool(attrTruncated, true)
	}
	setStatus(root.Status(), t.Status, t.StatusMessage)

	e.firstChat = true
	e.encodeSteps(steps, rootID, "")
}

// encodeSteps writes sibling steps under parent; prefix is the key path of
// the enclosing tool for steps inside a subagent.
func (e *encoder) encodeSteps(steps []model.Step, parent pcommon.SpanID, prefix string) {
	seen := map[string]int{}
	for i := range steps {
		s := &steps[i]
		key := s.Key
		if key == "" {
			key = fmt.Sprintf("#%d", i)
		}
		if n := seen[key]; n > 0 {
			seen[key] = n + 1
			key = fmt.Sprintf("%s#%d", key, n)
		} else {
			seen[key] = 1
		}
		path := key
		if prefix != "" {
			path = prefix + "/" + key
		}
		switch {
		case s.Kind == model.StepLLM && s.LLM != nil:
			e.encodeLLM(s, parent, path)
		case s.Kind == model.StepTool && s.Tool != nil:
			e.encodeTool(s, parent, path)
		}
	}
}

func (e *encoder) newSpan(parent pcommon.SpanID, id pcommon.SpanID, name string, kind ptrace.SpanKind, start, end time.Time) ptrace.Span {
	sp := e.spans.AppendEmpty()
	sp.SetTraceID(e.exportTrace)
	sp.SetSpanID(id)
	sp.SetParentSpanID(parent)
	sp.SetName(name)
	sp.SetKind(kind)
	sp.SetStartTimestamp(pcommon.NewTimestampFromTime(start))
	sp.SetEndTimestamp(pcommon.NewTimestampFromTime(end))
	return sp
}

func (e *encoder) encodeLLM(s *model.Step, parent pcommon.SpanID, path string) {
	c := s.LLM
	model_ := c.ResponseModel
	if model_ == "" {
		model_ = c.RequestModel
	}
	name := "chat"
	if model_ != "" {
		name = "chat " + model_
	}
	sp := e.newSpan(parent, StepSpanID(e.traceID, path), name, ptrace.SpanKindClient, s.Start, s.End)
	a := sp.Attributes()
	a.PutStr(attrOperation, "chat")
	e.common(a)
	providerName := c.Provider
	if providerName == "" {
		providerName = "unknown"
	}
	a.PutStr("gen_ai.provider.name", providerName)
	putNonEmpty(a, "gen_ai.request.model", c.RequestModel)
	putNonEmpty(a, "gen_ai.response.model", c.ResponseModel)
	putNonEmpty(a, "gen_ai.response.id", c.ResponseID)
	if len(c.FinishReasons) > 0 {
		sl := a.PutEmptySlice("gen_ai.response.finish_reasons")
		for _, r := range c.FinishReasons {
			sl.AppendEmpty().SetStr(r)
		}
	}
	u := c.Usage
	if u.InputTokens != 0 || u.OutputTokens != 0 {
		a.PutInt("gen_ai.usage.input_tokens", u.InputTokens)
		a.PutInt("gen_ai.usage.output_tokens", u.OutputTokens)
	} else if c.UsageInOtherCall {
		// tokens are on another call of this turn: explicit zeros mark the usage as known rather than missing
		a.PutInt("gen_ai.usage.input_tokens", 0)
		a.PutInt("gen_ai.usage.output_tokens", 0)
		a.PutBool("flashcat.usage.aggregated", true)
	}
	putNonZero(a, "gen_ai.usage.cache_read.input_tokens", u.CacheReadTokens)
	putNonZero(a, "gen_ai.usage.cache_write.input_tokens", u.CacheWriteTokens)
	putNonZero(a, "gen_ai.usage.reasoning.output_tokens", u.ReasoningTokens)
	if c.ReportedCostUSD > 0 {
		a.PutDouble("llm.cost.total", c.ReportedCostUSD)
	}
	trunc := false
	if e.withMessages() {
		if e.firstChat && e.turn.SystemInstructions != "" {
			v, cut := marshalMessagesParts(systemInstructions(e.turn.SystemInstructions), e.opt.MaxAttrChars)
			a.PutStr(attrSystemInstr, v)
			trunc = cut || trunc
		}
		finish := ""
		if len(c.FinishReasons) > 0 {
			finish = c.FinishReasons[0]
		}
		if len(c.InputMessages) > 0 {
			trunc = e.putMessages(a, attrInputMsgs, convertMessages(c.InputMessages, e.withTools(), "")) || trunc
		}
		if len(c.OutputMessages) > 0 {
			trunc = e.putMessages(a, attrOutputMsgs, convertMessages(c.OutputMessages, e.withTools(), finish)) || trunc
		}
	}
	e.firstChat = false
	if trunc {
		a.PutBool(attrTruncated, true)
	}
}

func (e *encoder) encodeTool(s *model.Step, parent pcommon.SpanID, path string) {
	tc := s.Tool
	name := "execute_tool"
	if tc.Name != "" {
		name = "execute_tool " + tc.Name
	}
	id := StepSpanID(e.traceID, path)
	sp := e.newSpan(parent, id, name, ptrace.SpanKindInternal, s.Start, s.End)
	a := sp.Attributes()
	a.PutStr(attrOperation, "execute_tool")
	e.common(a)
	putNonEmpty(a, "gen_ai.tool.name", tc.Name)
	putNonEmpty(a, "gen_ai.tool.call.id", tc.CallID)
	trunc := false
	if e.withTools() {
		if tc.Arguments != "" {
			v, cut := truncateText(tc.Arguments, e.opt.MaxAttrChars)
			a.PutStr("gen_ai.tool.call.arguments", v)
			trunc = cut || trunc
		}
		if tc.Result != "" {
			v, cut := truncateText(tc.Result, e.opt.MaxAttrChars)
			a.PutStr("gen_ai.tool.call.result", v)
			trunc = cut || trunc
		}
	}
	if trunc {
		a.PutBool(attrTruncated, true)
	}
	msg := tc.ErrorMessage
	if msg == "" && tc.Status == model.StatusError {
		msg = "error"
	}
	setStatus(sp.Status(), tc.Status, msg)

	if sa := tc.Subagent; sa != nil {
		e.encodeSubagent(sa, id, path, s)
	}
}

func (e *encoder) encodeSubagent(sa *model.Subagent, parent pcommon.SpanID, toolPath string, tool *model.Step) {
	steps := fillTimes(sa.Steps, sa.Start, sa.End)
	start, end := cover(sa.Start, sa.End, steps)
	if start.IsZero() {
		start = tool.Start
	}
	if end.Before(start.Add(minSpan)) {
		end = start.Add(minSpan)
	}
	typ := sa.Type
	if typ == "" {
		typ = "subagent"
	}
	id := SubagentSpanID(e.traceID, toolPath)
	sp := e.newSpan(parent, id, "invoke_agent "+typ, ptrace.SpanKindInternal, start, end)
	a := sp.Attributes()
	a.PutStr(attrOperation, "invoke_agent")
	a.PutStr(attrSubagent, typ)
	e.common(a)
	trunc := false
	if e.withMessages() {
		if sa.Input != "" {
			trunc = e.putMessages(a, attrInputMsgs, textMessage("user", sa.Input, "")) || trunc
		}
		if sa.Output != "" {
			trunc = e.putMessages(a, attrOutputMsgs, textMessage("assistant", sa.Output, "stop")) || trunc
		}
	}
	if trunc {
		a.PutBool(attrTruncated, true)
	}
	setStatus(sp.Status(), sa.Status, sa.StatusMessage)
	e.encodeSteps(steps, id, toolPath)
}

// common writes the attributes every span carries (CONTRACT §4.2).
func (e *encoder) common(a pcommon.Map) {
	t := e.turn
	a.PutStr(attrAgentName, string(t.Tool))
	putNonEmpty(a, attrSession, t.SessionID)
	putNonEmpty(a, attrUser, t.UserID)
	putNonEmpty(a, attrRepo, t.Repo)
	putNonEmpty(a, attrBranch, t.GitBranch)
}

func (e *encoder) putMessages(a pcommon.Map, key string, msgs []otelMessage) bool {
	v, cut := marshalMessages(msgs, e.opt.MaxAttrChars)
	a.PutStr(key, v)
	return cut
}

func marshalMessagesParts(parts []otelPart, limit int) (string, bool) {
	s := marshal(parts)
	if len([]rune(s)) <= limit {
		return s, false
	}
	// Wrap as a message to reuse the part-aware truncation, then unwrap.
	msgs := []otelMessage{{Role: "system", Parts: parts}}
	if _, cut := marshalMessages(msgs, limit); cut {
		return marshal(msgs[0].Parts), true
	}
	return s, false
}

func setStatus(st ptrace.Status, s model.Status, msg string) {
	switch s {
	case model.StatusError:
		st.SetCode(ptrace.StatusCodeError)
		st.SetMessage(msg)
	case model.StatusAborted:
		if msg == "" {
			msg = "aborted"
		}
		st.SetCode(ptrace.StatusCodeError)
		st.SetMessage(msg)
	}
}

func putNonEmpty(a pcommon.Map, k, v string) {
	if strings.TrimSpace(v) != "" {
		a.PutStr(k, v)
	}
}

func putNonZero(a pcommon.Map, k string, v int64) {
	if v != 0 {
		a.PutInt(k, v)
	}
}

// fillTimes returns a copy of steps with missing times inferred from
// neighbours (CONTRACT §4.5): a missing start takes the previous step's end
// (or its start + 1 ms, or the parent start), a missing end the next step's
// start (or the parent end); every step lasts at least 1 ms.
func fillTimes(in []model.Step, parentStart, parentEnd time.Time) []model.Step {
	steps := make([]model.Step, len(in))
	copy(steps, in)
	for i := range steps {
		s := &steps[i]
		if !s.Start.IsZero() {
			continue
		}
		switch {
		case i > 0 && !steps[i-1].End.IsZero():
			s.Start = steps[i-1].End
		case i > 0 && !steps[i-1].Start.IsZero():
			s.Start = steps[i-1].Start.Add(minSpan)
		case !parentStart.IsZero():
			s.Start = parentStart
		default:
			s.Start = s.End
		}
	}
	for i := range steps {
		s := &steps[i]
		if s.End.IsZero() {
			for j := i + 1; j < len(steps); j++ {
				if !steps[j].Start.IsZero() && !steps[j].Start.Before(s.Start) {
					s.End = steps[j].Start
					break
				}
			}
			if s.End.IsZero() && !parentEnd.Before(s.Start) {
				s.End = parentEnd
			}
		}
		if s.End.Before(s.Start.Add(minSpan)) {
			s.End = s.Start.Add(minSpan)
		}
	}
	return steps
}

// cover widens [start, end] to include all steps and nested subagents.
func cover(start, end time.Time, steps []model.Step) (time.Time, time.Time) {
	for _, s := range steps {
		if !s.Start.IsZero() && (start.IsZero() || s.Start.Before(start)) {
			start = s.Start
		}
		if s.End.After(end) {
			end = s.End
		}
		if s.Tool != nil && s.Tool.Subagent != nil {
			sa := s.Tool.Subagent
			sub := fillTimes(sa.Steps, sa.Start, sa.End)
			ss, se := cover(sa.Start, sa.End, sub)
			if !ss.IsZero() && (start.IsZero() || ss.Before(start)) {
				start = ss
			}
			if se.After(end) {
				end = se
			}
		}
	}
	return start, end
}
