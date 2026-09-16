package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type proxyServer struct {
	agy          *agyClient
	catalog      *catalog
	quota        *quotaCache
	sigs         *signatureStore
	defaultModel string
	authToken    string
	logger       *logger
}

func (s *proxyServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /usage", s.handleUsage)
	mux.HandleFunc("HEAD /api/hello", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	return s.accessLog(s.withAuth(mux))
}

func (s *proxyServer) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logger.log.Printf("%s %s%s -> %d (%s)", r.Method, r.URL.Path,
			queryOrNull(r), rec.status, time.Since(start).Round(time.Millisecond))
	})
}

func queryOrNull(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *proxyServer) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || s.authToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("x-api-key")
		if auth := r.Header.Get("authorization"); strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimPrefix(auth, "Bearer ")
		}
		if got != s.authToken {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid proxy auth token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *proxyServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req messagesRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil || json.Unmarshal(body, &req) != nil {
		s.logger.log.Printf("unparseable /v1/messages body (%d bytes): %v", len(body), err)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "could not parse request body as Anthropic Messages JSON")
		return
	}
	model, effort := resolveModel(req.Model, s.defaultModel)
	generateReq := buildGenerateRequest(req, s.defaultModel, s.sigs)
	s.logger.debugf("upstream model=%s effort=%s stream=%v messages=%d tools=%d", model, effort, req.Stream, len(req.Messages), len(req.Tools))
	if path := os.Getenv("AGY_PROXY_DUMP"); path != "" {
		if encoded, derr := json.Marshal(generateReq); derr == nil {
			_ = os.WriteFile(path, encoded, 0o600)
		}
	}

	if !req.Stream {
		s.handleNonStreaming(w, r, model, generateReq)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming unsupported by this server")
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)

	enc := &eventWriter{w: w, flusher: flusher}
	state := newStreamState(s.sigs)
	enc.emit("message_start", map[string]any{"message": map[string]any{
		"id":            "msg_" + newRequestID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         usageInfo{},
	}})
	enc.emit("ping", map[string]any{"type": "ping"})

	sawContentBlock := false
	note := func(events []streamEvent) {
		for _, ev := range events {
			if ev.Name == "content_block_start" {
				sawContentBlock = true
			}
			enc.emit(ev.Name, ev.Data)
		}
	}
	stopPings := enc.keepAlivePings(r.Context())
	defer stopPings()
	streamErr := s.agy.StreamGenerate(r.Context(), generateReq, func(chunk streamChunk) error {
		stopPings()
		note(state.Feed(chunk))
		return nil
	})
	if streamErr != nil {
		if r.Context().Err() != nil {
			s.logger.debugf("client abandoned stream: %v", streamErr)
			return
		}
		s.logger.debugf("stream aborted: %v", streamErr)
		enc.emit("error", map[string]any{"type": "error", "error": map[string]any{
			"type":    anthropicErrorType(streamErr),
			"message": streamErr.Error(),
		}})
		return
	}
	note(state.closeAll())
	if !sawContentBlock {
		s.logger.debugf("upstream returned an empty candidate; emitting placeholder block")
		enc.emit("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		enc.emit("content_block_delta", deltaEvent(0, map[string]any{"type": "text_delta", "text": "(the model returned an empty response)"}))
		enc.emit("content_block_stop", map[string]any{"index": 0})
	}
	enc.emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": state.StopReason(), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": state.usage.OutputTokens},
	})
	enc.emit("message_stop", map[string]any{"type": "message_stop"})
}

// handleNonStreaming collects the same stream events and assembles a final
// Anthropic message from them.
func (s *proxyServer) handleNonStreaming(w http.ResponseWriter, r *http.Request, model string, generateReq streamOuterRequest) {
	state := newStreamState(s.sigs)
	var events []streamEvent
	err := s.agy.StreamGenerate(r.Context(), generateReq, func(chunk streamChunk) error {
		events = append(events, state.Feed(chunk)...)
		return nil
	})
	if err != nil {
		if r.Context().Err() != nil {
			s.logger.debugf("client abandoned non-stream request: %v", err)
			return
		}
		s.logger.log.Printf("non-stream request failed: %v", err)
		status, code := httpStatusForError(err)
		writeAnthropicError(w, status, code, err.Error())
		return
	}
	state.closeAll()
	message := assembleMessage(model, state, events)
	if content, _ := message["content"].([]any); len(content) == 0 {
		s.logger.debugf("upstream returned an empty candidate (non-stream); emitting placeholder")
		message["content"] = []any{map[string]any{"type": "text", "text": "(the model returned an empty response)"}}
	}
	writeJSON(w, http.StatusOK, message)
}

// assembleMessage folds emitted SSE events back into a final message body —
// the event stream remains the single source of truth for both modes.
func assembleMessage(model string, state *streamState, events []streamEvent) map[string]any {
	type block struct {
		kind    string
		id      string
		name    string
		text    strings.Builder
		argsRaw strings.Builder
	}
	byIndex := map[int]*block{}
	var indices []int
	ensure := func(i int) *block {
		if b, ok := byIndex[i]; ok {
			return b
		}
		b := &block{}
		byIndex[i] = b
		indices = append(indices, i)
		return b
	}
	for _, ev := range events {
		data, _ := json.Marshal(ev.Data)
		var d struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &d) != nil {
			continue
		}
		switch ev.Name {
		case "content_block_start":
			b := ensure(d.Index)
			b.kind, b.id, b.name = d.ContentBlock.Type, d.ContentBlock.ID, d.ContentBlock.Name
		case "content_block_delta":
			b := ensure(d.Index)
			switch d.Delta.Type {
			case "text_delta":
				b.text.WriteString(d.Delta.Text)
			case "thinking_delta":
				b.text.WriteString(d.Delta.Thinking)
			case "input_json_delta":
				b.argsRaw.WriteString(d.Delta.PartialJSON)
			}
		}
	}
	sort.Ints(indices)
	content := make([]any, 0, len(indices))
	for _, i := range indices {
		b := byIndex[i]
		switch b.kind {
		case "thinking":
			content = append(content, map[string]any{"type": "thinking", "thinking": b.text.String(), "signature": ""})
		case "tool_use":
			var input map[string]any
			if json.Unmarshal([]byte(b.argsRaw.String()), &input) != nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{"type": "tool_use", "id": b.id, "name": b.name, "input": input})
		case "text":
			content = append(content, map[string]any{"type": "text", "text": b.text.String()})
		}
	}
	return map[string]any{
		"id":            "msg_" + newRequestID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   state.StopReason(),
		"stop_sequence": nil,
		"usage":         state.usage,
	}
}

// handleUsage reports the Google subscription's remaining quota, for status
// lines and the like. Values come straight from the upstream summary.
func (s *proxyServer) handleUsage(w http.ResponseWriter, r *http.Request) {
	buckets, fetched, err := s.quota.Buckets(r.Context())
	if err != nil {
		status, code := httpStatusForError(err)
		writeAnthropicError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fetchedAt": fetched.UTC().Format(time.RFC3339),
		"buckets":   buckets,
	})
}

func (s *proxyServer) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	// A rough heuristic; Claude Code only uses this for context accounting.
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": len(body) / 4})
}

func (s *proxyServer) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.catalog.Models(r.Context())
	if err != nil {
		status, code := httpStatusForError(err)
		writeAnthropicError(w, status, code, err.Error())
		return
	}
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{
			"type":         "model",
			"id":           id,
			"display_name": models[id].DisplayName,
			"created_at":   time.Now().UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data, "has_more": false, "first_id": ids[0], "last_id": ids[len(ids)-1],
	})
}

// eventWriter serializes Anthropic SSE events and flushes immediately.
type eventWriter struct {
	mu      sync.Mutex
	w       io.Writer
	flusher http.Flusher
}

func (e *eventWriter) emit(event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, payload)
	e.flusher.Flush()
}

const keepAliveInterval = 5 * time.Second

// keepAlivePings holds the SSE connection open while the upstream model thinks.
// Claude Code drops a stream that stays byte-silent (CLAUDE_BYTE_STREAM_IDLE_TIMEOUT_MS),
// and slow Gemini tiers can take 30s to emit their first token.
func (e *eventWriter) keepAlivePings(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(keepAliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.emit("ping", map[string]any{"type": "ping"})
			}
		}
	}()
	return sync.OnceFunc(func() { close(done) })
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

func anthropicErrorType(err error) string {
	status, code := httpStatusForError(err)
	_ = status
	return code
}

func httpStatusForError(err error) (int, string) {
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.status {
		case http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "rate_limit_error"
		case http.StatusUnauthorized, http.StatusForbidden:
			return ae.status, "authentication_error"
		case http.StatusBadRequest:
			return http.StatusBadRequest, "invalid_request_error"
		default:
			return ae.status, "api_error"
		}
	}
	return http.StatusBadGateway, "api_error"
}
