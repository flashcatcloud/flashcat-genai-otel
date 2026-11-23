package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/flashcatcloud/flashcat-genai-otel/hook/internal/model"
)

// Delivery limits (CONTRACT §4).
const (
	DefaultTimeout  = 3 * time.Second
	MaxRequestBytes = 4 << 20
)

// Sender posts OTLP/HTTP protobuf requests.
type Sender struct {
	Endpoint  string
	Key       string // "keyID:secret", sent as Basic auth
	UserAgent string
	Client    *http.Client
	Timeout   time.Duration // total for one Send; default 3 s
	MaxBytes  int           // split threshold; default 4 MiB
}

// HTTPError is a non-2xx response or a partial rejection.
type HTTPError struct {
	Status   int    // HTTP status; 0 for partial_success rejections
	Reason   string // short reason from the response body
	Rejected int64  // partial_success.rejected_spans
}

func (e *HTTPError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("rejected %d spans: %s", e.Rejected, e.Reason)
	}
	if e.Reason == "" {
		return fmt.Sprintf("HTTP %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Reason)
}

// Send delivers td, split into requests of at most MaxBytes. It succeeds
// only if every request gets a 2xx with no rejected spans.
func (s *Sender) Send(ctx context.Context, td ptrace.Traces) error {
	if td.SpanCount() == 0 {
		return nil
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	max := s.MaxBytes
	if max <= 0 {
		max = MaxRequestBytes
	}
	for _, part := range Split(td, max) {
		if err := s.post(ctx, part); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sender) post(ctx context.Context, td ptrace.Traces) error {
	body, err := ptraceotlp.NewExportRequestFromTraces(td).MarshalProto()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(s.Key)))
	if s.UserAgent != "" {
		req.Header.Set("User-Agent", s.UserAgent)
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Status: resp.StatusCode, Reason: reason(resp, respBody)}
	}
	if len(respBody) == 0 {
		return nil
	}
	er := ptraceotlp.NewExportResponse()
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		if er.UnmarshalJSON(respBody) != nil {
			return nil
		}
	} else if er.UnmarshalProto(respBody) != nil {
		return nil
	}
	if ps := er.PartialSuccess(); ps.RejectedSpans() > 0 {
		return &HTTPError{Rejected: ps.RejectedSpans(), Reason: ps.ErrorMessage()}
	}
	return nil
}

// reason extracts a short explanation from an error response: the
// google.rpc.Status message for protobuf bodies, else the text itself.
func reason(resp *http.Response, body []byte) string {
	ct := resp.Header.Get("Content-Type")
	var msg string
	switch {
	case strings.Contains(ct, "protobuf"):
		msg = rpcStatusMessage(body)
	default:
		msg = string(body)
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 200 {
		msg = truncateRunes(msg, 200) + "…"
	}
	return msg
}

// rpcStatusMessage reads field 2 (message) of a google.rpc.Status without
// pulling in genproto.
func rpcStatusMessage(b []byte) string {
	for i := 0; i < len(b); {
		key, n := uvarint(b[i:])
		if n <= 0 {
			return ""
		}
		i += n
		field, wire := key>>3, key&7
		switch wire {
		case 0:
			_, n = uvarint(b[i:])
			if n <= 0 {
				return ""
			}
			i += n
		case 2:
			l, n := uvarint(b[i:])
			if n <= 0 || i+n+int(l) > len(b) {
				return ""
			}
			i += n
			if field == 2 {
				return string(b[i : i+int(l)])
			}
			i += int(l)
		default:
			return ""
		}
	}
	return ""
}

func uvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i == 10 {
			return 0, -1
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}

// Split cuts td into pieces whose protobuf size is at most max bytes,
// preferring cuts between traces. A single span larger than max is sent on
// its own.
func Split(td ptrace.Traces, max int) []ptrace.Traces {
	var sizer ptrace.ProtoMarshaler
	if sizer.TracesSize(td) <= max {
		return []ptrace.Traces{td}
	}
	type item struct {
		rs, ss int
		span   ptrace.Span
	}
	var items []item
	rss := td.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		sss := rss.At(i).ScopeSpans()
		for j := 0; j < sss.Len(); j++ {
			spans := sss.At(j).Spans()
			for k := 0; k < spans.Len(); k++ {
				items = append(items, item{i, j, spans.At(k)})
			}
		}
	}
	build := func(part []item) ptrace.Traces {
		out := ptrace.NewTraces()
		type key struct{ rs, ss int }
		dst := map[key]ptrace.SpanSlice{}
		rsMap := map[int]ptrace.ResourceSpans{}
		for _, it := range part {
			k := key{it.rs, it.ss}
			spans, ok := dst[k]
			if !ok {
				ors, ok := rsMap[it.rs]
				if !ok {
					ors = out.ResourceSpans().AppendEmpty()
					src := rss.At(it.rs)
					src.Resource().CopyTo(ors.Resource())
					ors.SetSchemaUrl(src.SchemaUrl())
					rsMap[it.rs] = ors
				}
				oss := ors.ScopeSpans().AppendEmpty()
				srcSS := rss.At(it.rs).ScopeSpans().At(it.ss)
				srcSS.Scope().CopyTo(oss.Scope())
				oss.SetSchemaUrl(srcSS.SchemaUrl())
				spans = oss.Spans()
				dst[k] = spans
			}
			it.span.CopyTo(spans.AppendEmpty())
		}
		return out
	}
	var out []ptrace.Traces
	var rec func(part []item)
	rec = func(part []item) {
		t := build(part)
		if len(part) == 1 || sizer.TracesSize(t) <= max {
			out = append(out, t)
			return
		}
		mid := len(part) / 2
		// Move the cut to the nearest trace boundary when there is one.
		best := -1
		for d := 0; d < len(part)/2 && best < 0; d++ {
			for _, c := range []int{mid - d, mid + d} {
				if c > 0 && c < len(part) && part[c].span.TraceID() != part[c-1].span.TraceID() {
					best = c
					break
				}
			}
		}
		if best > 0 {
			mid = best
		}
		rec(part[:mid])
		rec(part[mid:])
	}
	rec(items)
	return out
}

// Probe builds the onboarding probe (CONTRACT §4.6), a single span that the
// Flashcat ingest gateway counts and the backend's GenAI mapping skips.
func Probe(res Resource, tool model.Tool, now time.Time) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	fillResource(rs.Resource().Attributes(), res, tool, "")
	ss := rs.ScopeSpans().AppendEmpty()
	ss.Scope().SetName(ScopeName)
	ss.Scope().SetVersion(res.HookVersion)
	sp := ss.Spans().AppendEmpty()
	var tid pcommon.TraceID
	var sid pcommon.SpanID
	_, _ = rand.Read(tid[:])
	_, _ = rand.Read(sid[:])
	sp.SetTraceID(tid)
	sp.SetSpanID(sid)
	sp.SetName("flashcat-hook probe")
	sp.SetKind(ptrace.SpanKindInternal)
	sp.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	sp.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Millisecond)))
	a := sp.Attributes()
	a.PutStr("flashcat.span.kind", "onboarding_probe")
	a.PutStr(attrAgentName, string(tool))
	return td
}
