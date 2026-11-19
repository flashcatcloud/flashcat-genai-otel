package codex

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/flashcatcloud/flashcat-otel/hook/internal/model"
)

// subLink is what a turn says about one subagent thread.
type subLink struct {
	thread        string
	spawnCall     string   // SubAgentActivity kind=started id == spawn_agent call_id
	interactCalls []string // kind=interacted ids == send_message / followup_task call_ids
	at            time.Time
}

// built is a turn's steps before subagents are attached.
type built struct {
	steps  []model.Step
	calls  map[string]int // call_id -> index in steps
	input  string
	output string
	links  []*subLink
}

// buildSteps turns a turn's events into model and tool steps.
//
// Step keys are response ids and call ids; synthetic keys carry the turn id
// because a subagent's steps from several child turns share one span tree.
//
// Model calls: one per token_usage_record (deduplicated by response_id), or
// per token_count when the turn has no usage record. A call spans from the
// previous call's end (or the turn start) to its record. Assistant text,
// reasoning summaries and tool calls written since the previous call are its
// output; real user / agent messages and tool outputs since the previous call
// are its input.
func buildSteps(t *turnBuf, provider string) built {
	b := built{calls: map[string]int{}}
	var (
		pendingIn  []model.Message
		outParts   []model.Part
		boundary   = t.start
		seenResp   = map[string]bool{}
		nLLM       int
		agentInput string
		openExec   []int
		linkByID   = map[string]*subLink{}
	)
	link := func(thread string, at time.Time) *subLink {
		l := linkByID[thread]
		if l == nil {
			l = &subLink{thread: thread, at: at}
			linkByID[thread] = l
			b.links = append(b.links, l)
		}
		return l
	}

	for _, e := range t.events {
		switch e.kind {
		case evUser:
			if b.input == "" {
				b.input = e.text
			}
			pendingIn = append(pendingIn, textMessage("user", e.text))
		case evAgentMsg:
			if agentInput == "" {
				agentInput = e.text
			}
			pendingIn = append(pendingIn, textMessage("user", e.text))
		case evAssistant:
			if e.text == "" {
				continue
			}
			outParts = append(outParts, model.Part{Type: "text", Content: e.text})
			if e.phase == "" || e.phase == "final_answer" {
				b.output = e.text
			}
		case evReasoning:
			outParts = append(outParts, model.Part{Type: "reasoning", Content: e.text})
		case evCall:
			if _, dup := b.calls[e.callID]; dup && e.callID != "" {
				continue
			}
			name := toolName(e.name, e.namespace)
			key := e.callID
			if key == "" {
				key = fmt.Sprintf("%s:call-%d", t.id, len(b.steps))
			}
			b.calls[e.callID] = len(b.steps)
			if e.custom && e.name == "exec" {
				openExec = append(openExec, len(b.steps))
			}
			b.steps = append(b.steps, model.Step{
				Kind:  model.StepTool,
				Key:   key,
				Start: e.ts,
				Tool:  &model.ToolCall{Name: name, CallID: e.callID, Arguments: e.args, Status: model.StatusOK},
			})
			outParts = append(outParts, model.Part{Type: "tool_call", ID: e.callID, Name: name, Arguments: e.args})
		case evOutput:
			if idx, ok := b.calls[e.callID]; ok && e.callID != "" {
				s := &b.steps[idx]
				if s.End.IsZero() {
					s.End = e.ts
				}
				s.Tool.Result = e.text
				if e.failed {
					setToolError(s.Tool, "Script failed")
				}
				openExec = removeIndex(openExec, idx)
			}
			pendingIn = append(pendingIn, model.Message{Role: "tool", Parts: []model.Part{{Type: "tool_call_response", ID: e.callID, Response: e.text}}})
		case evItem:
			it := e.item
			switch it.Type {
			case "SubAgentActivity":
				if it.AgentThreadID == "" {
					continue
				}
				l := link(it.AgentThreadID, e.ts)
				switch it.Kind {
				case "started":
					if l.spawnCall == "" {
						l.spawnCall = it.ID
					}
				case "interacted":
					l.interactCalls = append(l.interactCalls, it.ID)
				}
				continue
			case "CollabAgentToolCall":
				if it.Tool == "spawn_agent" {
					for _, id := range receivers(it) {
						if l := link(id, e.ts); l.spawnCall == "" {
							l.spawnCall = it.ID
						}
					}
				}
			}
			if idx, ok := b.calls[it.ID]; ok && it.ID != "" {
				applyItem(&b.steps[idx], it)
			} else if it.Type == "CommandExecution" && it.Status == "declined" {
				// Commands run inside an exec script have their own ids; the
				// script is the open exec call. Only a declined command fails
				// the script step (nested non-zero exits are often handled).
				if n := len(openExec); n > 0 {
					setToolError(b.steps[openExec[n-1]].Tool, "declined")
				}
			}
		case evRecord, evTokenCount:
			if (e.kind == evRecord) != t.hasRecords {
				continue // token_count is only the fallback
			}
			var key string
			if e.kind == evRecord {
				key = e.responseID
				if key == "" {
					key = fmt.Sprintf("%s:response-%d", t.id, nLLM)
				}
				if seenResp[key] {
					continue
				}
				seenResp[key] = true
			} else {
				if e.dup {
					continue
				}
				key = fmt.Sprintf("%s:token_count-%d", t.id, nLLM)
			}
			start := boundary
			if start.IsZero() || start.After(e.ts) {
				start = e.ts
			}
			call := &model.LLMCall{
				Provider:       provider,
				RequestModel:   t.model,
				ResponseModel:  t.model,
				ResponseID:     e.responseID,
				Usage:          mapUsage(e.usage),
				InputMessages:  pendingIn,
				OutputMessages: assistantMessage(outParts),
			}
			b.steps = append(b.steps, model.Step{Kind: model.StepLLM, Key: key, Start: start, End: e.ts, LLM: call})
			nLLM++
			pendingIn, outParts = nil, nil
			boundary = e.ts
		}
	}

	if t.lastAgentMsg != "" {
		b.output = t.lastAgentMsg
	}
	if b.input == "" {
		b.input = agentInput
	}
	for i := range b.steps {
		s := &b.steps[i]
		if s.Kind != model.StepTool || !s.End.IsZero() {
			continue
		}
		s.End = t.end
		if s.End.Before(s.Start) {
			s.End = s.Start
		}
		if t.ended == endAborted || t.ended == endSuperseded {
			s.Tool.Status = model.StatusAborted
			s.Tool.ErrorMessage = "aborted"
		}
	}
	return b
}

// applyItem fills timing and status from an item_completed whose id is the
// tool call id (McpToolCall, CollabAgentToolCall, 1:1 CommandExecution).
func applyItem(s *model.Step, it *item) {
	if !it.startedAt.IsZero() {
		s.Start = it.startedAt
	}
	if !it.completedAt.IsZero() && !it.completedAt.Before(s.Start) {
		s.End = it.completedAt
	}
	if it.Type == "McpToolCall" && it.Server != "" && it.Tool != "" {
		s.Tool.Name = it.Server + "." + it.Tool
	}
	switch {
	case it.Status == "failed" || it.Status == "declined":
		setToolError(s.Tool, it.Status)
	case it.ExitCode != nil && *it.ExitCode != 0:
		setToolError(s.Tool, fmt.Sprintf("exit_code %d", *it.ExitCode))
	case it.isError():
		setToolError(s.Tool, "isError")
	}
}

// setToolError marks a tool call failed. Messages are status words only, never
// tool output, so they are safe at every content level.
func setToolError(tc *model.ToolCall, msg string) {
	tc.Status = model.StatusError
	if tc.ErrorMessage == "" {
		tc.ErrorMessage = msg
	}
}

// toolName: the function name; MCP tools (namespace mcp__<server>) become
// <server>.<tool> like the McpToolCall item does.
func toolName(name, namespace string) string {
	if server, ok := strings.CutPrefix(namespace, "mcp__"); ok && server != "" && name != "" {
		return server + "." + name
	}
	if name == "" {
		return "unknown"
	}
	return name
}

func receivers(it *item) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range it.ReceiverThreadIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, a := range it.ReceiverAgents {
		if a.ThreadID != "" && !seen[a.ThreadID] {
			seen[a.ThreadID] = true
			out = append(out, a.ThreadID)
		}
	}
	return out
}

// mapUsage: Codex input_tokens already include cached input and output_tokens
// already include reasoning, which is what model.Usage expects.
func mapUsage(u usage) model.Usage {
	return model.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CachedInputTokens,
		CacheWriteTokens: u.CacheWriteInputTokens,
		ReasoningTokens:  u.ReasoningOutputTokens,
	}
}

func textMessage(role, text string) model.Message {
	return model.Message{Role: role, Parts: []model.Part{{Type: "text", Content: text}}}
}

func assistantMessage(parts []model.Part) []model.Message {
	if len(parts) == 0 {
		return nil
	}
	return []model.Message{{Role: "assistant", Parts: parts}}
}

func removeIndex(xs []int, v int) []int {
	for i, x := range xs {
		if x == v {
			return append(xs[:i], xs[i+1:]...)
		}
	}
	return xs
}

func sortSteps(steps []model.Step) {
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Start.Before(steps[j].Start) })
}

func turnStatus(t *turnBuf) (model.Status, string) {
	switch t.ended {
	case endComplete:
		if t.errText != "" {
			return model.StatusError, t.errText
		}
	case endAborted, endSuperseded:
		return model.StatusAborted, "aborted"
	}
	return model.StatusOK, ""
}
