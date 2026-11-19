// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Modifications Copyright 2026 Flashcat Cloud.
// Ported from open-telemetry/opentelemetry-go-compile-instrumentation,
// instrumentation/github.com/openai/openai-go/internal/streaming/streaming.go. See UPSTREAM.md for the baseline and changes.

package flashcatopenai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// errStreamAborted indicates that the stream closed before a finish_reason, [DONE], or Responses API terminal event arrived.
var errStreamAborted = errors.New("stream aborted")

const maxCapturedChoices = 16 // Cap the number of accumulated choices because the server controls the index.

// contentAccumulator keeps only the content that can be exported and stops storing more at the limit to bound memory use.
type contentAccumulator struct {
	b         strings.Builder
	truncated bool
}

func (c *contentAccumulator) Append(s string) {
	if s == "" || c.truncated {
		return
	}
	remaining := contentLimit - c.b.Len()
	if remaining >= len(s) {
		c.b.WriteString(s)
		return
	}
	if remaining > 0 {
		cut := remaining
		for cut > 0 && cut < len(s) && s[cut]&0xc0 == 0x80 {
			cut--
		}
		c.b.WriteString(s[:cut])
	}
	c.b.WriteString(truncatedSuffix)
	c.truncated = true
}

type toolCallAcc struct {
	id   string
	name string
	args contentAccumulator
}

type choiceAcc struct {
	text         contentAccumulator
	tools        map[int]*toolCallAcc
	finishReason string
}

// streamingReader parses the SSE response body as it is read. It accumulates usage and output,
// records time to first chunk, and ends the span when the stream ends.
// Read and Close may run in separate goroutines. Parsing and span completion hold mu, while reads from the underlying stream run outside the lock.
type streamingReader struct {
	reader  io.ReadCloser
	span    trace.Span
	start   time.Time
	op      operationType
	capture bool
	onDone  func([]attribute.KeyValue)

	mu         sync.Mutex
	lineBuffer bytes.Buffer
	done       bool
	completed  bool
	failed     string // Responses API failed or error event.
	first      time.Time

	inputTokens, outputTokens, totalTokens *int64
	cacheReadTokens                        int64
	id, responseModel                      string
	reasons                                []string
	choices                                map[int]*choiceAcc
	responsesFinal                         *responsesBody
}

func newStreamingReader(body io.ReadCloser, span trace.Span, start time.Time, op operationType, capture bool, onDone func([]attribute.KeyValue)) io.ReadCloser {
	return &streamingReader{reader: body, span: span, start: start, op: op, capture: capture, onDone: onDone}
}

func (r *streamingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return n, err
	}
	if n > 0 {
		r.lineBuffer.Write(p[:n])
		r.processLines()
	}
	if err != nil {
		r.done = true
		// Only a clean EOF makes lineBuffer a complete final line without a newline. Other errors may leave a partial chunk, so skip parsing it.
		if err == io.EOF {
			r.flushRemaining()
		}
		r.finalize(err)
	}
	return n, err
}

func (r *streamingReader) Close() error {
	r.mu.Lock()
	if !r.done {
		r.done = true
		// Parse the last buffered line even if the caller stops reading early.
		r.flushRemaining()
		if r.isCompleted() {
			r.finalize(nil)
		} else {
			r.finalize(errStreamAborted)
		}
	}
	r.mu.Unlock()
	return r.reader.Close()
}

func (r *streamingReader) isCompleted() bool {
	if r.op == opResponses {
		return r.completed
	}
	return r.completed || len(r.reasons) > 0
}

func (r *streamingReader) finalize(err error) {
	completed := r.isCompleted()
	hardErr := err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, errStreamAborted)
	var errAttrs []attribute.KeyValue
	switch {
	case r.failed != "":
		r.span.SetStatus(codes.Error, r.failed)
		errAttrs = []attribute.KeyValue{attribute.String("error.type", "response_failed")}
	case !completed || hardErr:
		if len(r.reasons) == 0 {
			r.reasons = []string{"error"}
		}
		r.span.SetStatus(codes.Error, "stream aborted")
		if hardErr {
			r.span.RecordError(err)
			errAttrs = []attribute.KeyValue{semconv.ErrorType(err)}
		} else {
			errAttrs = []attribute.KeyValue{attribute.String("error.type", "stream_aborted")}
		}
	}
	if len(errAttrs) > 0 {
		r.span.SetAttributes(errAttrs...)
	}

	if r.responsesFinal != nil {
		applyResponsesBody(r.span, r.responsesFinal, r.capture)
	} else {
		var attrs []attribute.KeyValue
		if len(r.reasons) > 0 {
			attrs = append(attrs, attribute.StringSlice("gen_ai.response.finish_reasons", r.reasons))
		}
		if r.inputTokens != nil {
			attrs = append(attrs, attribute.Int64("gen_ai.usage.input_tokens", *r.inputTokens))
		}
		if r.outputTokens != nil {
			attrs = append(attrs, attribute.Int64("gen_ai.usage.output_tokens", *r.outputTokens))
		}
		if r.totalTokens != nil {
			attrs = append(attrs, attribute.Int64("gen_ai.usage.total_tokens", *r.totalTokens))
		}
		if r.cacheReadTokens > 0 {
			attrs = append(attrs, attribute.Int64("gen_ai.usage.cache_read.input_tokens", r.cacheReadTokens))
		}
		if r.id != "" {
			attrs = append(attrs, attribute.String("gen_ai.response.id", r.id))
		}
		if r.responseModel != "" {
			attrs = append(attrs, attribute.String("gen_ai.response.model", r.responseModel))
		}
		if r.capture {
			if out := marshalMessages(r.outputMessages()); out != "" {
				attrs = append(attrs, attribute.String("gen_ai.output.messages", out))
			}
		}
		r.span.SetAttributes(attrs...)
	}
	if !r.first.IsZero() {
		r.span.SetAttributes(attribute.Float64("gen_ai.response.time_to_first_chunk", r.first.Sub(r.start).Seconds()))
	}
	r.span.End()
	if r.onDone != nil {
		r.onDone(errAttrs)
	}
}

func (r *streamingReader) outputMessages() []message {
	if len(r.choices) == 0 {
		return nil
	}
	idx := make([]int, 0, len(r.choices))
	for i := range r.choices {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]message, 0, len(idx))
	for _, i := range idx {
		c := r.choices[i]
		msg := message{Role: "assistant", FinishReason: c.finishReason}
		if s := c.text.b.String(); s != "" {
			msg.Parts = append(msg.Parts, part{Type: "text", Content: s})
		}
		tidx := make([]int, 0, len(c.tools))
		for ti := range c.tools {
			tidx = append(tidx, ti)
		}
		sort.Ints(tidx)
		for _, ti := range tidx {
			tc := c.tools[ti]
			msg.Parts = append(msg.Parts, part{Type: "tool_call", ID: tc.id, Name: tc.name, Arguments: toolArguments(tc.args.b.String())})
		}
		out = append(out, msg)
	}
	return out
}

// flushRemaining parses the rest of lineBuffer as the final line because EOF may arrive without a trailing newline.
func (r *streamingReader) flushRemaining() {
	line := bytes.TrimSpace(r.lineBuffer.Bytes())
	r.lineBuffer.Reset()
	if len(line) > 0 {
		r.processLine(line)
	}
}

func (r *streamingReader) processLines() {
	data := r.lineBuffer.Bytes()
	last := bytes.LastIndexByte(data, '\n')
	if last < 0 {
		return
	}
	complete := data[:last]
	rest := append([]byte(nil), data[last+1:]...)
	for _, line := range bytes.Split(complete, []byte("\n")) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			r.processLine(line)
		}
	}
	r.lineBuffer.Reset()
	r.lineBuffer.Write(rest)
}

func (r *streamingReader) processLine(line []byte) {
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return // Skip event, id, and comment lines. The Responses API also includes the event type in the data payload's type field.
	}
	payload = bytes.TrimSpace(payload)
	if bytes.Equal(payload, []byte("[DONE]")) {
		r.completed = true
		return
	}
	switch r.op {
	case opChat:
		r.processChatChunk(payload)
	case opCompletion:
		r.processCompletionChunk(payload)
	case opResponses:
		r.processResponsesEvent(payload)
	}
}

// markFirst records time to first chunk using only nonempty output (text, refusals, or tool call names or arguments), per the GenAI semantic conventions.
func (r *streamingReader) markFirst() {
	if r.first.IsZero() {
		r.first = time.Now()
	}
}

func (r *streamingReader) choice(index int) *choiceAcc {
	if r.choices == nil {
		r.choices = map[int]*choiceAcc{}
	}
	c := r.choices[index]
	if c == nil {
		if len(r.choices) >= maxCapturedChoices {
			return nil
		}
		c = &choiceAcc{}
		r.choices[index] = c
	}
	return c
}

func (r *streamingReader) applyUsage(u *completionsUsage) {
	if u == nil {
		return
	}
	if u.PromptTokens != nil {
		r.inputTokens = u.PromptTokens
	}
	if u.CompletionTokens != nil {
		r.outputTokens = u.CompletionTokens
	}
	if u.TotalTokens != nil {
		r.totalTokens = u.TotalTokens
	}
	if cached := cachedPromptTokens(u.PromptTokensDetails); cached > 0 {
		r.cacheReadTokens = cached
	}
}

func choiceIndex(index *int, position int) int {
	if index != nil {
		return *index
	}
	return position
}

func (r *streamingReader) processChatChunk(payload []byte) {
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index        *int   `json:"index"`
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				Content   string `json:"content"`
				Refusal   string `json:"refusal"`
				ToolCalls []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *completionsUsage `json:"usage"`
	}
	if json.Unmarshal(payload, &chunk) != nil {
		return
	}
	if chunk.ID != "" {
		r.id = chunk.ID
	}
	if chunk.Model != "" {
		r.responseModel = chunk.Model
	}
	r.applyUsage(chunk.Usage)
	for pos, c := range chunk.Choices {
		d := c.Delta
		if d.Content != "" || d.Refusal != "" || len(d.ToolCalls) > 0 {
			r.markFirst()
		}
		if c.FinishReason != "" {
			r.reasons = append(r.reasons, c.FinishReason)
		}
		if !r.capture {
			continue
		}
		acc := r.choice(choiceIndex(c.Index, pos))
		if acc == nil {
			continue
		}
		if c.FinishReason != "" {
			acc.finishReason = c.FinishReason
		}
		acc.text.Append(d.Content)
		acc.text.Append(d.Refusal)
		for tpos, tc := range d.ToolCalls {
			ti := choiceIndex(tc.Index, tpos)
			if acc.tools == nil {
				acc.tools = map[int]*toolCallAcc{}
			}
			t := acc.tools[ti]
			if t == nil {
				if len(acc.tools) >= maxCapturedChoices {
					continue
				}
				t = &toolCallAcc{}
				acc.tools[ti] = t
			}
			if tc.ID != "" {
				t.id = tc.ID
			}
			if tc.Function.Name != "" {
				t.name = tc.Function.Name
			}
			t.args.Append(tc.Function.Arguments)
		}
	}
}

func (r *streamingReader) processCompletionChunk(payload []byte) {
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index        *int   `json:"index"`
			FinishReason string `json:"finish_reason"`
			Text         string `json:"text"`
		} `json:"choices"`
		Usage *completionsUsage `json:"usage"`
	}
	if json.Unmarshal(payload, &chunk) != nil {
		return
	}
	if chunk.ID != "" {
		r.id = chunk.ID
	}
	if chunk.Model != "" {
		r.responseModel = chunk.Model
	}
	r.applyUsage(chunk.Usage)
	for pos, c := range chunk.Choices {
		if c.Text != "" {
			r.markFirst()
		}
		if c.FinishReason != "" {
			r.reasons = append(r.reasons, c.FinishReason)
		}
		if !r.capture {
			continue
		}
		if acc := r.choice(choiceIndex(c.Index, pos)); acc != nil {
			if c.FinishReason != "" {
				acc.finishReason = c.FinishReason
			}
			acc.text.Append(c.Text)
		}
	}
}

// processResponsesEvent handles streaming events from the Responses API. These streams end without [DONE].
// A response.completed, response.incomplete, or response.failed event ends the stream and includes the full response object.
func (r *streamingReader) processResponsesEvent(payload []byte) {
	var ev struct {
		Type     string          `json:"type"`
		Delta    string          `json:"delta"`
		Message  string          `json:"message"`
		Response json.RawMessage `json:"response"`
		Item     struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"item"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	switch ev.Type {
	case "response.output_text.delta", "response.refusal.delta", "response.function_call_arguments.delta",
		"response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.audio.delta":
		if ev.Delta != "" {
			r.markFirst()
		}
	case "response.output_item.added":
		if ev.Item.Type == "function_call" && ev.Item.Name != "" {
			r.markFirst()
		}
	case "response.created", "response.in_progress":
		var b responsesBody
		if json.Unmarshal(ev.Response, &b) == nil {
			if b.ID != "" {
				r.id = b.ID
			}
			if b.Model != "" {
				r.responseModel = b.Model
			}
		}
	case "response.completed", "response.incomplete", "response.failed":
		r.completed = true
		var b responsesBody
		if json.Unmarshal(ev.Response, &b) == nil {
			r.responsesFinal = &b
			if ev.Type == "response.failed" {
				r.failed = "response failed"
				if b.Error != nil && b.Error.Message != "" {
					r.failed = b.Error.Message
				}
			}
		}
	case "error":
		r.completed = true
		r.failed = "error event"
		if ev.Message != "" {
			r.failed = ev.Message
		}
	}
}
