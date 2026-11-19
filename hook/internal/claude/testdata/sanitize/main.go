// Command sanitize turns a real Claude Code transcript into a test fixture:
// every prompt, reply, thinking block, tool input, tool result, path and any
// other free text is replaced with a placeholder, while record types, prompt
// ids, message ids, tool ids, models, stop reasons, flags and usage numbers
// are kept. Record uuids (and every field that points at one) are remapped to
// fresh random uuids, timestamps are shifted by one random offset, cost-state
// numbers are replaced with synthetic values and permissionMode is set to
// "default". See ../README.md.
//
//	go run ./internal/claude/testdata/sanitize -in <session>.jsonl -out <dir> [-from N] [-to M] [-shift D]
//
// Subagent files (<session>/subagents/agent-*.jsonl and .meta.json) are
// sanitized too, with the same uuid map and time offset; with a line range
// only subagents spawned in that range are kept.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// keep lists keys whose string values are structural (ids, enums, times).
var keep = map[string]bool{
	"type": true, "subtype": true, "uuid": true, "parentUuid": true, "logicalParentUuid": true,
	"promptId": true, "sessionId": true, "session_id": true, "timestamp": true, "version": true,
	"model": true, "requestedModel": true, "id": true, "tool_use_id": true, "toolUseId": true,
	"toolUseID": true, "stop_reason": true, "role": true, "kind": true, "promptSource": true,
	"agentId": true, "agentType": true, "status": true, "level": true, "userType": true,
	"entrypoint": true, "permissionMode": true, "service_tier": true, "speed": true,
	"inference_geo": true, "operation": true, "querySource": true, "trigger": true,
	"sourceToolAssistantUUID": true, "leafUuid": true, "messageId": true, "snapshotMessageId": true,
	"requestId": true, "turnOrigin": true, "stop_sequence": true, "renderedRole": true,
	"thinkingDisplay": true, "effort": true, "perTurnEffort": true,
}

// uuidRefs lists keys whose uuid-shaped values are a record uuid or point at
// one; they are remapped through uuidMap so references stay consistent across
// the main transcript and its subagents. (compactMetadata's anchorUuid,
// headUuid and tailUuid are not kept at all and become placeholders.)
var uuidRefs = map[string]bool{
	"uuid": true, "parentUuid": true, "logicalParentUuid": true, "leafUuid": true,
	"sourceToolAssistantUUID": true, "messageId": true, "snapshotMessageId": true,
}

// syntheticCost replaces the numbers of cost-state records (real spend and
// activity of the recorded session). Other numeric fields become 0;
// totalCostUSD is the sum of the per-model costUSD; startTime is shifted.
var syntheticCost = map[string]float64{
	"inputTokens": 1000, "outputTokens": 2000, "thinkingTokens": 500, "cacheReadInputTokens": 30000,
	"cacheCreationInputTokens": 4000, "cacheCreationEphemeral1hInputTokens": 3000,
	"cacheCreationEphemeral5mInputTokens": 1000, "webSearchRequests": 0, "costUSD": 0.25,
	"totalAPIDuration": 60000, "totalAPIDurationWithoutRetries": 60000, "totalDuration": 120000,
	"totalToolDuration": 30000, "totalLinesAdded": 10, "totalLinesRemoved": 5,
}

const interruptPrefix = "[Request interrupted by user"

var (
	reToolUseID = regexp.MustCompile(`<tool-use-id>\s*([^<\s]+)\s*</tool-use-id>`)
	reTaskID    = regexp.MustCompile(`<task-id>\s*([^<\s]+)\s*</task-id>`)
	reStatus    = regexp.MustCompile(`<status>\s*([^<\s]+)\s*</status>`)
	reIdentKey  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
	reUUID      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

var (
	uuidMap = map[string]string{} // original uuid -> fresh uuid, shared by all files of one run
	shift   time.Duration         // added to every timestamp of one run
)

func main() {
	in := flag.String("in", "", "transcript .jsonl")
	out := flag.String("out", "", "output directory")
	from := flag.Int("from", 0, "first line to keep (0-based)")
	to := flag.Int("to", -1, "last line to keep (inclusive); -1 = end")
	flag.DurationVar(&shift, "shift", 0, "offset added to all timestamps; 0 = random 40-90 days back, not on a whole second")
	flag.Parse()
	if *in == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if shift == 0 {
		shift = randomShift()
	}
	base := strings.TrimSuffix(filepath.Base(*in), ".jsonl")
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(err)
	}
	toolUses := map[string]bool{}
	n, err := sanitizeFile(*in, filepath.Join(*out, base+".jsonl"), *from, *to, toolUses)
	if err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "main: %d lines\n", n)

	subIn := filepath.Join(filepath.Dir(*in), base, "subagents")
	metas, _ := filepath.Glob(filepath.Join(subIn, "agent-*.meta.json"))
	for _, m := range metas {
		b, err := os.ReadFile(m)
		if err != nil {
			fail(err)
		}
		var meta map[string]any
		if err := json.Unmarshal(b, &meta); err != nil {
			fail(err)
		}
		if id, _ := meta["toolUseId"].(string); !toolUses[id] {
			continue
		}
		subOut := filepath.Join(*out, base, "subagents")
		if err := os.MkdirAll(subOut, 0o755); err != nil {
			fail(err)
		}
		clean := sanitizeValue("", meta, nil).(map[string]any)
		mb, _ := json.Marshal(clean)
		if err := os.WriteFile(filepath.Join(subOut, filepath.Base(m)), append(mb, '\n'), 0o644); err != nil {
			fail(err)
		}
		agent := strings.TrimSuffix(filepath.Base(m), ".meta.json")
		n, err := sanitizeFile(filepath.Join(subIn, agent+".jsonl"), filepath.Join(subOut, agent+".jsonl"), 0, -1, map[string]bool{})
		if err != nil {
			fail(err)
		}
		fmt.Fprintf(os.Stderr, "%s: %d lines\n", agent, n)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func randInt(n int64) int64 {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		fail(err)
	}
	return v.Int64()
}

// randomShift is 40-90 days back plus a random number of seconds and
// milliseconds, so it is never a whole day, hour or second.
func randomShift() time.Duration {
	d := time.Duration(40+randInt(51)) * 24 * time.Hour
	d += time.Duration(1+randInt(86398)) * time.Second
	d += time.Duration(1+randInt(998)) * time.Millisecond
	return -d
}

// remapUUID returns the fresh uuid for an original one (random v4, stable
// within a run).
func remapUUID(s string) string {
	if n, ok := uuidMap[s]; ok {
		return n
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		fail(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	n := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	uuidMap[s] = n
	return n
}

// shiftTime moves an RFC 3339 timestamp by shift, keeping its zone and the
// number of fractional digits.
func shiftTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	layout := time.RFC3339
	if i := strings.IndexByte(s, '.'); i >= 0 {
		n := 0
		for _, c := range s[i+1:] {
			if c < '0' || c > '9' {
				break
			}
			n++
		}
		layout = "2006-01-02T15:04:05." + strings.Repeat("0", n) + "Z07:00"
	}
	return t.Add(shift).Format(layout)
}

// costValue rewrites the numbers of a cost-state record.
func costValue(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = costValue(k, val)
		}
		if usage, ok := x["modelUsage"].(map[string]any); ok {
			total := 0.0
			for _, m := range usage {
				if mm, ok := m.(map[string]any); ok {
					c, _ := mm["costUSD"].(float64)
					total += c
				}
			}
			x["totalCostUSD"] = total
		}
		return x
	case float64:
		if key == "startTime" {
			return x + float64(shift.Milliseconds())
		}
		return syntheticCost[key]
	}
	return v
}

func sanitizeFile(in, out string, from, to int, toolUses map[string]bool) (int, error) {
	f, err := os.Open(in)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var buf bytes.Buffer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	n := 0
	for i := 0; sc.Scan(); i++ {
		if i < from || (to >= 0 && i > to) {
			continue
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		clean := sanitizeRecord(rec, toolUses)
		b, err := json.Marshal(clean)
		if err != nil {
			return n, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
		n++
	}
	if err := sc.Err(); err != nil {
		return n, err
	}
	return n, os.WriteFile(out, buf.Bytes(), 0o644)
}

// bulky lists record types the parser ignores whose payloads are collapsed
// to keep fixtures small; their type and ids stay.
var bulky = map[string]bool{"api-request": true, "api-request-blob": true, "api-request-shape": true, "attachment": true}

func sanitizeRecord(rec map[string]any, toolUses map[string]bool) map[string]any {
	if t, _ := rec["type"].(string); bulky[t] {
		for k, v := range rec {
			switch v.(type) {
			case map[string]any, []any:
				rec[k] = map[string]any{"redacted": true}
			}
		}
	}
	origin, _ := rec["origin"].(map[string]any)
	notification := origin != nil && origin["kind"] == "task-notification"
	if msg, ok := rec["message"].(map[string]any); ok {
		if blocks, ok := msg["content"].([]any); ok {
			for _, b := range blocks {
				if bm, ok := b.(map[string]any); ok && bm["type"] == "tool_use" {
					if id, ok := bm["id"].(string); ok {
						toolUses[id] = true
					}
				}
			}
		}
		if s, ok := msg["content"].(string); ok && notification {
			msg["content"] = notificationPlaceholder(s)
		}
	}
	clean := sanitizeValue("", rec, nil).(map[string]any)
	if clean["type"] == "cost-state" {
		costValue("", clean)
	}
	return clean
}

// notificationPlaceholder keeps the ids and status of a task-notification.
func notificationPlaceholder(s string) string {
	var b strings.Builder
	b.WriteString("<task-notification>")
	if m := reTaskID.FindStringSubmatch(s); m != nil {
		b.WriteString("<task-id>" + m[1] + "</task-id>")
	}
	if m := reToolUseID.FindStringSubmatch(s); m != nil {
		b.WriteString("<tool-use-id>" + m[1] + "</tool-use-id>")
	}
	if m := reStatus.FindStringSubmatch(s); m != nil {
		b.WriteString("<status>" + m[1] + "</status>")
	}
	b.WriteString("<summary>redacted</summary></task-notification>")
	return b.String()
}

// sanitizeValue walks a decoded JSON value. parent is the enclosing object.
func sanitizeValue(key string, v any, parent map[string]any) any {
	switch x := v.(type) {
	case map[string]any:
		if key == "input" && parent != nil && (parent["type"] == "tool_use" || parent["type"] == "server_tool_use") {
			return map[string]any{"redacted": true}
		}
		out := make(map[string]any, len(x))
		i := 0
		for k, val := range x {
			ck := k
			if !reIdentKey.MatchString(k) {
				// Keys that are not identifiers (file paths in snapshots…).
				i++
				ck = fmt.Sprintf("key%d", i)
			}
			out[ck] = sanitizeValue(k, val, x)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = sanitizeValue(key, val, parent)
		}
		return out
	case string:
		return sanitizeString(key, x, parent)
	default:
		return v
	}
}

func sanitizeString(key, s string, parent map[string]any) string {
	switch {
	case s == "":
		return ""
	case strings.HasPrefix(strings.TrimSpace(s), interruptPrefix):
		return strings.TrimSpace(s)
	case strings.HasPrefix(strings.TrimSpace(s), "<task-notification>"):
		return notificationPlaceholder(s) // keeps only ids and status
	case key == "name" && parent != nil && (parent["type"] == "tool_use" || parent["type"] == "server_tool_use"):
		return s
	case key == "cwd":
		return "/work/project"
	case key == "gitBranch":
		return "main"
	case key == "permissionMode":
		return "default"
	case key == "timestamp":
		return shiftTime(s)
	case uuidRefs[key] && reUUID.MatchString(s):
		return remapUUID(s)
	case keep[key]:
		return s
	case key == "text" || key == "content":
		return "redacted text"
	case key == "thinking":
		return "redacted thinking"
	}
	return "redacted"
}
