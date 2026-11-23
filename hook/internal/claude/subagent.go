package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/model"
)

const maxSubagentDepth = 4

// subagentMeta is agent-<id>.meta.json.
type subagentMeta struct {
	AgentType string `json:"agentType"`
	ToolUseID string `json:"toolUseId"`
}

type subagentRef struct {
	id   string // agent id (file name without "agent-" and extension)
	meta subagentMeta
}

// subagentIndex maps the spawning tool_use id, and the agent id, to a file.
type subagentIndex struct {
	byToolUse map[string]subagentRef
	byAgentID map[string]subagentRef
}

func loadSubagentIndex(dir string) subagentIndex {
	idx := subagentIndex{byToolUse: map[string]subagentRef{}, byAgentID: map[string]subagentRef{}}
	if dir == "" {
		return idx
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	for _, m := range matches {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "agent-"), ".jsonl")
		ref := subagentRef{id: id}
		if b, err := os.ReadFile(filepath.Join(dir, "agent-"+id+".meta.json")); err == nil {
			_ = json.Unmarshal(b, &ref.meta)
		}
		idx.byAgentID[id] = ref
		if ref.meta.ToolUseID != "" {
			idx.byToolUse[ref.meta.ToolUseID] = ref
		}
	}
	return idx
}

// attachSubagents replaces the Subagent of Agent/Task tool steps with the
// parsed subagent file (meta.toolUseId == tool_use.id; the agentId from the
// tool result is the fallback). Nested agents are attached recursively.
func attachSubagents(steps []model.Step, idx subagentIndex, dir string, depth int) {
	if depth >= maxSubagentDepth {
		return
	}
	for i := range steps {
		tc := steps[i].Tool
		if steps[i].Kind != model.StepTool || tc == nil {
			continue
		}
		ref, ok := idx.byToolUse[tc.CallID]
		if !ok && tc.Subagent != nil && tc.Subagent.ID != "" {
			ref, ok = idx.byAgentID[tc.Subagent.ID]
		}
		if !ok {
			if tc.Subagent != nil && len(tc.Subagent.Steps) == 0 {
				tc.Subagent = nil // agent id but no file: keep the plain tool span
			}
			continue
		}
		sa := parseSubagentFile(filepath.Join(dir, "agent-"+ref.id+".jsonl"), ref)
		if sa == nil {
			if tc.Subagent != nil && len(tc.Subagent.Steps) == 0 {
				tc.Subagent = nil
			}
			continue
		}
		if tc.Status == model.StatusError && sa.Status == model.StatusOK {
			sa.Status, sa.StatusMessage = model.StatusError, tc.ErrorMessage
		}
		attachSubagents(sa.Steps, idx, dir, depth+1)
		tc.Subagent = sa
	}
}

// parseSubagentFile parses a whole subagent transcript as one run.
func parseSubagentFile(path string, ref subagentRef) *model.Subagent {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n') // the agent may still be writing; take what is there
	}
	records, _ := parseLines(data, 0)
	u := newRun()
	for _, r := range records {
		u.add(r)
	}
	if len(u.steps) == 0 {
		return nil
	}
	status, msg := u.status()
	typ := ref.meta.AgentType
	if typ == "" {
		typ = "general-purpose"
	}
	return &model.Subagent{
		Type:          typ,
		ID:            ref.id,
		Start:         u.start,
		End:           u.end,
		Input:         u.input,
		Output:        u.output,
		Status:        status,
		StatusMessage: msg,
		Steps:         u.steps,
	}
}
