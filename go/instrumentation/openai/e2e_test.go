package flashcatopenai

import (
	"bytes"
	"compress/gzip"
	"context"
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

	flashcatgenai "github.com/flashcatcloud/flashcat-genai-otel/go"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Run flashcatgenai.Init in a subprocess because the global provider is process-wide. The middleware's chat span
// goes through the core SDK's session processing and masking and is exported via OTLP to a fake endpoint.

const scenarioEnv = "FLASHCAT_OPENAI_TEST_SCENARIO"

func TestMain(m *testing.M) {
	if os.Getenv(scenarioEnv) == "e2e" {
		out := runE2EScenario()
		b, _ := json.Marshal(out)
		fmt.Println("RESULT:" + string(b))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runE2EScenario() any {
	ctx := context.Background()
	// Create the middleware before Init. It reads the provider and content capture switch when a request arrives.
	mw := Middleware()
	sdk, err := flashcatgenai.Init(ctx, flashcatgenai.WithServiceName("kefu-agent"), flashcatgenai.WithCaptureContent(flashcatgenai.ContentSpan),
		flashcatgenai.WithMetrics(false))
	if err != nil {
		panic(err)
	}
	client := openai.NewClient(option.WithBaseURL(os.Getenv("FAKE_OPENAI_URL")), option.WithAPIKey("sk-test"),
		option.WithMaxRetries(0), option.WithMiddleware(mw))
	sctx := flashcatgenai.WithSession(ctx, flashcatgenai.Session{ConversationID: "c-123", UserID: "u-9"})
	err = flashcatgenai.Agent(sctx, "kefu", func(ctx context.Context) error {
		_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:    "gpt-4o",
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("手机13812345678，邮箱a@b.com")},
		})
		return err
	})
	if err != nil {
		panic(err)
	}
	_ = sdk.Flush(ctx)
	return sdk.Diagnostics()
}

func TestEndToEndWithCore(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if zr, err := gzip.NewReader(bytes.NewReader(b)); err == nil {
			b, _ = io.ReadAll(zr)
		}
		mu.Lock()
		if r.URL.Path == "/v1/traces" {
			bodies = append(bodies, b)
		}
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer collector.Close()
	fake := newFakeOpenAI(t)

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(k)
		if strings.HasPrefix(up, "OTEL_") || strings.HasPrefix(up, "FLASHCAT_") || strings.HasPrefix(up, "OPENAI_") || strings.Contains(strings.ToLower(k), "proxy") {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, scenarioEnv+"=e2e", "FAKE_OPENAI_URL="+fake.baseURL(),
		"FLASHCAT_ENDPOINT="+collector.URL+"/v1/traces", "FLASHCAT_API_KEY=fck_test:s3cret")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child failed: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	var diag map[string]any
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "RESULT:") {
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "RESULT:")), &diag)
		}
	}
	insts, _ := json.Marshal(diag["instrumentations"])
	if !strings.Contains(string(insts), `"lib":"openai"`) || !strings.Contains(string(insts), `"status":"installed"`) {
		t.Fatalf("instrumentations: %s", insts)
	}

	type span struct {
		name, id, parent string
		attrs            map[string]string
	}
	var spans []span
	mu.Lock()
	for _, b := range bodies {
		var req coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					s := span{name: sp.Name, id: fmt.Sprintf("%x", sp.SpanId), parent: fmt.Sprintf("%x", sp.ParentSpanId), attrs: map[string]string{}}
					for _, kv := range sp.Attributes {
						s.attrs[kv.Key] = kv.Value.GetStringValue()
					}
					spans = append(spans, s)
				}
			}
		}
	}
	mu.Unlock()
	var chat, agent span
	for _, s := range spans {
		switch s.name {
		case "chat gpt-4o":
			chat = s
		case "invoke_agent kefu":
			agent = s
		}
	}
	if chat.id == "" || agent.id == "" || chat.parent != agent.id {
		t.Fatalf("spans: %+v", spans)
	}
	if chat.attrs["gen_ai.conversation.id"] != "c-123" || chat.attrs["user.id"] != "u-9" {
		t.Fatalf("session attrs: %v", chat.attrs)
	}
	in := chat.attrs["gen_ai.input.messages"]
	if in == "" || strings.Contains(in, "13812345678") || strings.Contains(in, "a@b.com") ||
		!strings.Contains(in, "[REDACTED:phone]") || !strings.Contains(in, "[REDACTED:email]") {
		t.Fatalf("masking on exported chat span: %q", in)
	}
	// The request sent to the model is unchanged because masking applies only in the export pipeline.
	if !strings.Contains(string(fake.body("/v1/chat/completions")), "13812345678") {
		t.Fatal("request body sent to the model was altered")
	}
}
