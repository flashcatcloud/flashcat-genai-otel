package flashcatopenai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeOpenAI is a local model server that returns JSON or SSE based on the path and the request's stream field. It makes no upstream requests.
type fakeOpenAI struct {
	srv      *httptest.Server
	mu       sync.Mutex
	lastBody map[string][]byte
	// A nonzero status overrides the status code for all inference endpoints.
	status int
	// chatSSE overrides the entire SSE response for streaming chat requests.
	chatSSE string
}

func newFakeOpenAI(t *testing.T) *fakeOpenAI {
	f := &fakeOpenAI{lastBody: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenAI) baseURL() string { return f.srv.URL + "/v1/" }

func (f *fakeOpenAI) body(path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody[path]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, raw string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	for _, ev := range strings.SplitAfter(raw, "\n\n") {
		if ev == "" {
			continue
		}
		_, _ = io.WriteString(w, ev)
		if fl != nil {
			fl.Flush()
		}
	}
}

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	return b.String()
}

const chatJSON = `{"id":"chatcmpl-test","object":"chat.completion","created":1761620000,"model":"gpt-4o-2024-08-06",
"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"您的订单已发货"}}],
"usage":{"prompt_tokens":120,"completion_tokens":12,"total_tokens":132,"prompt_tokens_details":{"cached_tokens":100}}}`

var chatStreamDefault = sse(
	`data: {"id":"chatcmpl-s","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
	`data: {"id":"chatcmpl-s","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"content":"您的"}}]}`,
	`data: {"id":"chatcmpl-s","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{"content":"订单已发货"}}]}`,
	`data: {"id":"chatcmpl-s","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	`data: {"id":"chatcmpl-s","object":"chat.completion.chunk","model":"gpt-4o-2024-08-06","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`,
	`data: [DONE]`,
)

const responsesJSON = `{"id":"resp_1","object":"response","created_at":1761620000,"model":"gpt-4.1-2025-04-14","status":"completed",
"output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"简短回答","annotations":[]}]}],
"usage":{"input_tokens":30,"output_tokens":4,"total_tokens":34,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`

var responsesStream = sse(
	`event: response.created`+"\n"+`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_s","object":"response","model":"gpt-4.1-2025-04-14","status":"in_progress","output":[]}}`,
	`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"简短"}`,
	`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"回答"}`,
	`event: response.completed`+"\n"+`data: {"type":"response.completed","sequence_number":3,"response":`+strings.ReplaceAll(strings.ReplaceAll(responsesJSON, "\n", ""), `"resp_1"`, `"resp_s"`)+`}`,
)

func (f *fakeOpenAI) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.lastBody[r.URL.Path] = body
	status, chatSSE := f.status, f.chatSSE
	f.mu.Unlock()
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	if r.Method == http.MethodGet {
		writeJSON(w, map[string]any{"object": "list", "data": []any{}})
		return
	}
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`)
		return
	}
	switch r.URL.Path {
	case "/v1/chat/completions":
		if req.Stream {
			if chatSSE == "" {
				chatSSE = chatStreamDefault
			}
			writeSSE(w, chatSSE)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatJSON)
	case "/v1/completions":
		if req.Stream {
			writeSSE(w, sse(
				`data: {"id":"cmpl-s","object":"text_completion","model":"gpt-3.5-turbo-instruct","choices":[{"index":0,"text":"Hello"}]}`,
				`data: {"id":"cmpl-s","object":"text_completion","model":"gpt-3.5-turbo-instruct","choices":[{"index":0,"text":" world","finish_reason":"length"}]}`,
				`data: [DONE]`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmpl-1","object":"text_completion","created":1,"model":"gpt-3.5-turbo-instruct",
"choices":[{"index":0,"text":"Hello world","finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	case "/v1/embeddings":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","model":"text-embedding-3-small","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],
"usage":{"prompt_tokens":8,"total_tokens":8}}`)
	case "/v1/responses":
		if req.Stream {
			writeSSE(w, responsesStream)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responsesJSON)
	default:
		http.Error(w, fmt.Sprintf("unexpected %s", r.URL.Path), 404)
	}
}
