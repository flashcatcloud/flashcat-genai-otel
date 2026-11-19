package flashcatotel

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type maskingFixture struct {
	SensitiveKeyRegex string            `json:"sensitive_key_regex"`
	ContentKeys       []string          `json:"content_keys"`
	ScanLimitBytes    int               `json:"scan_limit_bytes"`
	TruncateBytes     int               `json:"truncate_bytes"`
	TruncatedAttr     string            `json:"truncated_attr"`
	ContentPatterns   []json.RawMessage `json:"content_patterns"`
	KeyCases          []struct {
		Key    string `json:"key"`
		Masked bool   `json:"masked"`
	} `json:"key_cases"`
	ContentCases []struct {
		Input  string `json:"input"`
		Output string `json:"output"`
	} `json:"content_cases"`
}

func loadFixture(t *testing.T, p string, v any) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
}

func TestMaskingFixtures(t *testing.T) {
	var fx maskingFixture
	loadFixture(t, "../spec/fixtures/masking.json", &fx)
	if fx.ScanLimitBytes != defaultScanLimit || fx.TruncateBytes != defaultTruncateBytes || fx.TruncatedAttr != truncatedAttr {
		t.Fatalf("defaults drifted from fixture: %+v", fx)
	}
	if len(fx.ContentKeys) != len(contentKeys) {
		t.Fatalf("content keys drifted: %v", fx.ContentKeys)
	}
	for _, k := range fx.ContentKeys {
		if !contentKeys[k] {
			t.Fatalf("content key %s missing", k)
		}
	}
	if len(fx.ContentPatterns) != len(defaultRules()) {
		t.Fatalf("pattern count drifted")
	}
	for _, c := range fx.KeyCases {
		if got := sensitiveKey.MatchString(c.Key); got != c.Masked {
			t.Errorf("key %q: masked=%v want %v", c.Key, got, c.Masked)
		}
	}
	m, err := newMasker(MaskingConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fx.ContentCases {
		if got := m.maskContent(c.Input); got != c.Output {
			t.Errorf("content %q:\n got  %q\n want %q", c.Input, got, c.Output)
		}
	}
	var bd struct {
		Cases []struct {
			Input  string `json:"input"`
			Output string `json:"output"`
		} `json:"cases"`
	}
	loadFixture(t, "testdata/masking_boundary.json", &bd)
	if len(bd.Cases) < 20 {
		t.Fatalf("boundary fixture too small")
	}
	for _, c := range bd.Cases {
		if got := m.maskContent(c.Input); got != c.Output {
			t.Errorf("boundary %q:\n got  %q\n want %q", c.Input, got, c.Output)
		}
	}
}

func TestKeyMaskingDoesNotTouchTokenCounts(t *testing.T) {
	m, _ := newMasker(MaskingConfig{TruncateBytes: 10})
	attrs := []attribute.KeyValue{
		attribute.String("http.request.header.authorization", "Bearer abc"),
		attribute.Int("gen_ai.usage.input_tokens", 5),
		attribute.String("gen_ai.usage.output_tokens", "7"),
		attribute.String("big", "中文中文中文"),
	}
	out, changed, truncated := m.maskAttrs(attrs, false)
	got := map[string]attribute.Value{}
	for _, kv := range out {
		got[string(kv.Key)] = kv.Value
	}
	if got["http.request.header.authorization"].AsString() != "[REDACTED]" || got["gen_ai.usage.input_tokens"].AsInt64() != 5 ||
		got["gen_ai.usage.output_tokens"].AsString() != "7" || got["big"].AsString() != "中文中" || !got[truncatedAttr].AsBool() {
		t.Fatalf("attrs: %v", out)
	}
	if !changed || !truncated {
		t.Fatalf("changed=%v truncated=%v", changed, truncated)
	}
	if attrs[0].Value.AsString() != "Bearer abc" {
		t.Fatal("input attributes mutated")
	}
}

func TestContentMaskingOnlyWhenContentOn(t *testing.T) {
	m, _ := newMasker(MaskingConfig{})
	attrs := []attribute.KeyValue{attribute.String("gen_ai.input.messages", "手机13812345678"), attribute.String("other", "13812345678")}
	out, changed, _ := m.maskAttrs(attrs, false)
	if changed || out[0].Value.AsString() != "手机13812345678" {
		t.Fatalf("L2 ran with content off: %v", out)
	}
	out, changed, _ = m.maskAttrs(attrs, true)
	if !changed || out[0].Value.AsString() != "手机[REDACTED:phone]" || out[1].Value.AsString() != "13812345678" {
		t.Fatalf("L2 result: %v", out)
	}
}

func TestScanLimitIsInCharacters(t *testing.T) {
	m, _ := newMasker(MaskingConfig{ScanLimitChars: 13})
	// Replace the phone number in the first 13 characters and skip content beyond that limit.
	in := "中13812345678中13812345678"
	if got := m.maskContent(in); got != "中[REDACTED:phone]中13812345678" {
		t.Fatalf("got %q", got)
	}
}

func TestExtraPatternsAndBadRegex(t *testing.T) {
	m, err := newMasker(MaskingConfig{ExtraPatterns: []MaskingPattern{{Name: "order", Pattern: `ORD-(\d+)`, Replacement: "ORD-[$1]"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.maskContent("单号 ORD-42"); got != "单号 ORD-[42]" {
		t.Fatalf("got %q", got)
	}
	if _, err := resolve([]Option{WithMasking(MaskingConfig{ExtraPatterns: []MaskingPattern{{Name: "bad", Pattern: `(`}}})}); err == nil {
		t.Fatal("bad regex should be an explicit option error")
	}
}

func TestTruncateUTF8Boundary(t *testing.T) {
	big := strings.Repeat("中", 200000) // 600,000 bytes
	out, cut := truncateUTF8(big, defaultTruncateBytes)
	if !cut || len(out) > defaultTruncateBytes || len(out)%3 != 0 {
		t.Fatalf("len=%d cut=%v", len(out), cut)
	}
}

func TestMaskingExporterWrapsSpans(t *testing.T) {
	before := global.masked.Load()
	beforeT := global.truncated.Load()
	sink := tracetest.NewInMemoryExporter()
	m, _ := newMasker(MaskingConfig{TruncateBytes: 64})
	exp := &maskingExporter{next: sink, m: m, content: true}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	_, s := tp.Tracer("t").Start(context.Background(), "x")
	s.SetAttributes(attribute.String("api_key", "sk-1"), attribute.String("gen_ai.tool.call.arguments", `{"email":"a@b.com"}`),
		attribute.String("blob", strings.Repeat("x", 100)))
	s.End()
	got := sink.GetSpans()
	if len(got) != 1 {
		t.Fatalf("spans: %d", len(got))
	}
	attrs := map[string]attribute.Value{}
	for _, kv := range got[0].Attributes {
		attrs[string(kv.Key)] = kv.Value
	}
	if attrs["api_key"].AsString() != "[REDACTED]" || attrs["gen_ai.tool.call.arguments"].AsString() != `{"email":"[REDACTED:email]"}` ||
		len(attrs["blob"].AsString()) != 64 || !attrs[truncatedAttr].AsBool() {
		t.Fatalf("attrs: %v", attrs)
	}
	if global.masked.Load() != before+1 || global.truncated.Load() != beforeT+1 {
		t.Fatalf("counters: masked %d→%d truncated %d→%d", before, global.masked.Load(), beforeT, global.truncated.Load())
	}
}
