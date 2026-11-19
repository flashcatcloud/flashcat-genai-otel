package flashcatotel

import (
	"context"
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// MaskingPattern defines an L2 content masking rule. Pattern uses Go regexp (RE2) syntax.
// Replacement follows regexp.ReplaceAllString and can refer to capture groups with $1.
type MaskingPattern struct {
	Name        string
	Pattern     string
	Replacement string
}

// MaskingConfig controls the three layers of masking and truncation (contract §6).
// The zero value enables key masking and the four default content rules. L2 scans up to 262,144 characters per value,
// as in Python, and L3 caps each value at 512 KiB in UTF-8 bytes.
// L2 runs only when content capture is enabled.
type MaskingConfig struct {
	// DisableKeyMasking turns off L1 key masking.
	DisableKeyMasking bool
	// ExtraPatterns are custom rules applied after the default rules.
	ExtraPatterns []MaskingPattern
	// ScanLimitChars is the maximum number of characters L2 scans per value. Zero uses the default of 262,144.
	ScanLimitChars int
	// TruncateBytes is the L3 limit per value in UTF-8 bytes. Zero uses the default of 524,288.
	TruncateBytes int
}

const (
	defaultScanLimit     = 256 * 1024
	defaultTruncateBytes = 512 * 1024
	truncatedAttr        = "flashcat.content_truncated"
	redacted             = "[REDACTED]"
)

// sensitiveKey matches the final key segment for L1 key masking, exactly as in Python.
// This preserves counters such as gen_ai.usage.input_tokens.
var sensitiveKey = regexp.MustCompile(`(?i)(^|[._-])(authorization|api[-_]?key|apikey|password|passwd|secret|client[-_]?secret|` +
	`token|access[-_]?token|refresh[-_]?token|auth[-_]?token|cookie|set[-_]?cookie|private[-_]?key)$`)

var contentKeys = map[string]bool{
	"gen_ai.input.messages":       true,
	"gen_ai.output.messages":      true,
	"gen_ai.system_instructions":  true,
	"gen_ai.tool.call.arguments":  true,
	"gen_ai.tool.call.result":     true,
	"gen_ai.retrieval.query.text": true,
	"gen_ai.retrieval.documents":  true,
}

// contentRule holds a compiled L2 rule.
//
// Python's default rules use (?<!\d) and (?!\d) to check digit boundaries. RE2 lacks lookaround, so the fields enforce the same checks.
// core matches just the body. leftDigit rejects a preceding digit, and rightReject rejects a following character in its set.
// If a boundary check fails, resume the search one character after the start of the match. Each of the four default
// rules has only one valid end position for any given start.
// The body is either fixed-length or ends in greedy \d{4,7}, where shortening it would leave a digit immediately after the match.
// This matches Python's backtracking behavior.
// See spec/fixtures/masking.json and testdata/masking_boundary.json for reference cases.
// Python 3's \d matches all Unicode decimal digits, so use \p{Nd} in RE2.
type contentRule struct {
	name        string
	core        *regexp.Regexp
	replacement string
	literal     bool
	leftDigit   bool
	rightReject func(r rune) bool
}

func isDigit(r rune) bool { return unicode.Is(unicode.Nd, r) }

func isDigitOrX(r rune) bool { return isDigit(r) || r == 'X' || r == 'x' }

// defaultRules follows Python's DEFAULT_PATTERNS order (email, national ID, bank card, mobile phone).
func defaultRules() []contentRule {
	return []contentRule{
		{name: "email", core: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), replacement: "[REDACTED:email]", literal: true},
		{name: "cn_id_card", core: regexp.MustCompile(`[1-9]\p{Nd}{5}(?:19|20)\p{Nd}{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\p{Nd}|3[01])\p{Nd}{3}[\p{Nd}Xx]`),
			replacement: "[REDACTED:id_card]", literal: true, leftDigit: true, rightReject: isDigitOrX},
		{name: "bank_card", core: regexp.MustCompile(`(?:\p{Nd}{4}[ \-]?){3}\p{Nd}{4,7}`),
			replacement: "[REDACTED:bank_card]", literal: true, leftDigit: true, rightReject: isDigit},
		{name: "cn_mobile", core: regexp.MustCompile(`1[3-9]\p{Nd}{9}`),
			replacement: "[REDACTED:phone]", literal: true, leftDigit: true, rightReject: isDigit},
	}
}

func (r *contentRule) apply(s string) string {
	if r.leftDigit == false && r.rightReject == nil {
		if r.literal {
			return r.core.ReplaceAllLiteralString(s, r.replacement)
		}
		return r.core.ReplaceAllString(s, r.replacement)
	}
	var out []byte
	last, pos := 0, 0
	for pos <= len(s) {
		loc := r.core.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		start, end := pos+loc[0], pos+loc[1]
		ok := true
		if r.leftDigit && start > 0 {
			if prev, _ := utf8.DecodeLastRuneInString(s[:start]); isDigit(prev) {
				ok = false
			}
		}
		if ok && r.rightReject != nil && end < len(s) {
			if next, _ := utf8.DecodeRuneInString(s[end:]); r.rightReject(next) {
				ok = false
			}
		}
		if !ok {
			// Resume one character after the match start, as a backtracking engine would.
			_, size := utf8.DecodeRuneInString(s[start:])
			if size == 0 {
				size = 1
			}
			pos = start + size
			continue
		}
		out = append(out, s[last:start]...)
		out = append(out, r.replacement...)
		last = end
		if end == start {
			end++
		}
		pos = end
	}
	if out == nil {
		return s
	}
	out = append(out, s[last:]...)
	return string(out)
}

// masker applies the three layers of masking and truncation and records global semantics counters.
type masker struct {
	keyMasking bool
	rules      []contentRule
	scanLimit  int
	truncate   int
}

func newMasker(cfg MaskingConfig) (*masker, error) {
	m := &masker{keyMasking: !cfg.DisableKeyMasking, rules: defaultRules(), scanLimit: cfg.ScanLimitChars, truncate: cfg.TruncateBytes}
	if m.scanLimit <= 0 {
		m.scanLimit = defaultScanLimit
	}
	if m.truncate <= 0 {
		m.truncate = defaultTruncateBytes
	}
	for _, p := range cfg.ExtraPatterns {
		rx, err := regexp.Compile(p.Pattern)
		if err != nil {
			return nil, &configError{msg: fmt.Sprintf("脱敏规则 %q 的正则无法编译：%v", p.Name, err)}
		}
		m.rules = append(m.rules, contentRule{name: p.Name, core: rx, replacement: p.Replacement})
	}
	return m, nil
}

// maskContent applies L2 masking to the first scanLimit characters of content.
func (m *masker) maskContent(v string) string {
	head, tail := v, ""
	if utf8.RuneCountInString(v) > m.scanLimit {
		n, i := 0, 0
		for i = range v {
			if n == m.scanLimit {
				break
			}
			n++
		}
		head, tail = v[:i], v[i:]
	}
	for i := range m.rules {
		head = m.rules[i].apply(head)
	}
	return head + tail
}

// truncateUTF8 cuts at a valid character boundary at or before limit bytes.
func truncateUTF8(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// maskAttrs returns the rewritten attributes. changed includes any masking or truncation, and truncated indicates L3 truncation.
func (m *masker) maskAttrs(attrs []attribute.KeyValue, content bool) ([]attribute.KeyValue, bool, bool) {
	var out []attribute.KeyValue
	changed, truncated := false, false
	for i, kv := range attrs {
		if kv.Value.Type() != attribute.STRING {
			continue
		}
		v := kv.Value.AsString()
		nv := v
		key := string(kv.Key)
		if m.keyMasking && sensitiveKey.MatchString(key) {
			nv = redacted
		} else if content && contentKeys[key] {
			nv = m.maskContent(nv)
		}
		nv, cut := truncateUTF8(nv, m.truncate)
		truncated = truncated || cut
		if nv != v {
			if out == nil {
				out = make([]attribute.KeyValue, len(attrs), len(attrs)+1)
				copy(out, attrs)
			}
			out[i] = attribute.String(key, nv)
			changed = true
		}
	}
	if !changed && !truncated {
		return attrs, false, false
	}
	if out == nil {
		out = append([]attribute.KeyValue(nil), attrs...)
	}
	if truncated {
		out = append(out, attribute.Bool(truncatedAttr, true))
	}
	return out, changed, truncated
}

// spanWithAttrs overrides Attributes() so the Flashcat export pipeline can rewrite attributes on a read-only span.
type spanWithAttrs struct {
	sdktrace.ReadOnlySpan
	attrs []attribute.KeyValue
}

func (s spanWithAttrs) Attributes() []attribute.KeyValue { return s.attrs }

// maskingExporter replaces each span in ExportSpans before passing it downstream, protecting only the Flashcat export pipeline.
type maskingExporter struct {
	next    sdktrace.SpanExporter
	m       *masker
	content bool
}

func (e *maskingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = e.maskOne(s)
	}
	return e.next.ExportSpans(ctx, out)
}

func (e *maskingExporter) maskOne(s sdktrace.ReadOnlySpan) (res sdktrace.ReadOnlySpan) {
	res = s
	defer func() {
		if recover() != nil { // Keep SDK failures from disrupting export.
			res = s
		}
	}()
	attrs, changed, truncated := e.m.maskAttrs(s.Attributes(), e.content)
	if !changed && !truncated {
		return s
	}
	if changed {
		global.masked.Add(1)
	}
	if truncated {
		global.truncated.Add(1)
	}
	return spanWithAttrs{ReadOnlySpan: s, attrs: attrs}
}

func (e *maskingExporter) Shutdown(ctx context.Context) error { return e.next.Shutdown(ctx) }
