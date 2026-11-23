package flashcatgenai

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// ---- OTel's global provider is process-wide. Re-exec the test binary for scenarios that need fresh global state. ----

const scenarioEnv = "FLASHCAT_GO_TEST_SCENARIO"

var scenarios = map[string]func() any{}

func TestMain(m *testing.M) {
	if name := os.Getenv(scenarioEnv); name != "" {
		fn, ok := scenarios[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown scenario %q\n", name)
			os.Exit(2)
		}
		out := fn()
		b, err := json.Marshal(out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
			os.Exit(3)
		}
		fmt.Println("RESULT:" + string(b))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runScenario runs a scenario in a subprocess and parses the RESULT line as JSON.
// It clears inherited OTel and Flashcat environment variables.
func runScenario(t *testing.T, name string, extraEnv map[string]string) map[string]any {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	var envv []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(k)
		if strings.HasPrefix(up, "OTEL_") || strings.HasPrefix(up, "FLASHCAT_") || strings.HasPrefix(up, "OPENAI_") ||
			strings.HasPrefix(up, "ANTHROPIC_") || strings.HasPrefix(up, "OPENINFERENCE_") || strings.Contains(strings.ToLower(k), "proxy") {
			continue
		}
		envv = append(envv, kv)
	}
	envv = append(envv, scenarioEnv+"="+name)
	for k, v := range extraEnv {
		envv = append(envv, k+"="+v)
	}
	cmd.Env = envv
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("scenario %s failed: %v\nSTDOUT:%s\nSTDERR:%s", name, err, stdout.String(), stderr.String())
	}
	var last string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "RESULT:") {
			last = strings.TrimPrefix(line, "RESULT:")
		}
	}
	if last == "" {
		t.Fatalf("scenario %s printed no RESULT\nSTDOUT:%s\nSTDERR:%s", name, stdout.String(), stderr.String())
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(last), &out); err != nil {
		t.Fatalf("bad RESULT json: %v: %s", err, last)
	}
	out["_stderr"] = stderr.String()
	return out
}

// ---- Fake endpoint for decoding gzip and protobuf ----

type recordedRequest struct {
	Path    string
	Headers http.Header
	Body    []byte
	Status  int
}

type collector struct {
	mu       sync.Mutex
	requests []recordedRequest
	status   int
	srv      *httptest.Server
}

func newCollector(t *testing.T, status int) *collector {
	c := &collector{status: status}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
				body, _ = io.ReadAll(zr)
			}
		}
		c.mu.Lock()
		c.requests = append(c.requests, recordedRequest{Path: r.URL.Path, Headers: r.Header.Clone(), Body: body, Status: c.status})
		st := c.status
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(st)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) url() string { return c.srv.URL }

func (c *collector) traceRequests() []recordedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []recordedRequest
	for _, r := range c.requests {
		if r.Path == "/v1/traces" {
			out = append(out, r)
		}
	}
	return out
}

type gotSpan struct {
	Name     string
	Scope    string
	Resource map[string]any
	SpanID   string
	Parent   string
	Attrs    map[string]any
	Status   int32
	Events   []string
}

func anyValue(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonpb.AnyValue_ArrayValue:
		var out []any
		for _, e := range x.ArrayValue.Values {
			out = append(out, anyValue(e))
		}
		return out
	}
	return nil
}

func kvMap(kvs []*commonpb.KeyValue) map[string]any {
	m := map[string]any{}
	for _, kv := range kvs {
		m[kv.Key] = anyValue(kv.Value)
	}
	return m
}

func (c *collector) spans(t *testing.T) []gotSpan {
	t.Helper()
	var out []gotSpan
	for _, r := range c.traceRequests() {
		if r.Status != 200 {
			continue
		}
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(r.Body, &req); err != nil {
			t.Fatalf("protobuf decode: %v", err)
		}
		for _, rs := range req.ResourceSpans {
			res := kvMap(rs.Resource.GetAttributes())
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					g := gotSpan{Name: sp.Name, Scope: ss.Scope.GetName(), Resource: res, SpanID: hex.EncodeToString(sp.SpanId),
						Parent: hex.EncodeToString(sp.ParentSpanId), Attrs: kvMap(sp.Attributes), Status: int32(sp.Status.GetCode())}
					for _, e := range sp.Events {
						g.Events = append(g.Events, e.Name)
					}
					out = append(out, g)
				}
			}
		}
	}
	return out
}

func byOp(spans []gotSpan) map[string]gotSpan {
	m := map[string]gotSpan{}
	for _, s := range spans {
		if op, ok := s.Attrs["gen_ai.operation.name"].(string); ok {
			m[op] = s
		} else {
			m[s.Name] = s
		}
	}
	return m
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func path(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}
