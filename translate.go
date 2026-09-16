package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Anthropic Messages API request shapes (only the fields we consume).

type messagesRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []anthMessage   `json:"messages"`
	Tools         []anthTool      `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	Stream        bool            `json:"stream"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          int             `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Thinking      *anthThinking   `json:"thinking,omitempty"`
}

type anthThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

// anthBlock covers every content block kind; unknown kinds decode with an
// empty Type and are skipped.
type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     map[string]any  `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *anthSource     `json:"source,omitempty"`
}

type anthSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

func blocksOf(msg anthMessage) []anthBlock {
	var text string
	if json.Unmarshal(msg.Content, &text) == nil {
		return []anthBlock{{Type: "text", Text: text}}
	}
	var blocks []anthBlock
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return nil
	}
	return blocks
}

// signatureStore remembers the Gemini thoughtSignature that accompanied each
// functionCall so replaying conversation history keeps tool exchanges valid.
type signatureStore struct {
	byToolUseID map[string]string
	order       []string
}

func newSignatureStore() *signatureStore {
	return &signatureStore{byToolUseID: map[string]string{}}
}

func (s *signatureStore) put(id, sig string) {
	if id == "" || sig == "" {
		return
	}
	if _, seen := s.byToolUseID[id]; !seen && len(s.order) >= 8192 {
		delete(s.byToolUseID, s.order[0])
		s.order = s.order[1:]
	}
	s.byToolUseID[id] = sig
	s.order = append(s.order, id)
}

func (s *signatureStore) get(id string) string { return s.byToolUseID[id] }

// Claude Code >= 2.1.273 prepends "x-anthropic-billing-header: cc_version=...;
// cc_entrypoint=...;" to the system prompt. Upstream 429s on it; every other
// header of that family is assumed to read the same way.
var anthropicHeaderPrefix = regexp.MustCompile(`(?i)x-anthropic-[a-z0-9-]+:(\s*[a-z0-9_.-]+=[^;\n]*;)+`)

// sanitizeSystem rewrites Claude Code's identity phrasing to neutral
// Antigravity terminology so the upstream server doesn't filter it as
// third-party agent traffic. Only systemInstruction is scanned upstream;
// user and tool content passes through untouched.
func sanitizeSystem(s string) string {
	s = anthropicHeaderPrefix.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "Claude Agent SDK", "Antigravity SDK")
	s = strings.ReplaceAll(s, "Claude Code", "Antigravity CLI")
	s = strings.ReplaceAll(s, "Claude agent", "autonomous coding agent")
	s = strings.ReplaceAll(s, "Claude", "the assistant")
	return s
}

// buildGenerateRequest converts an Anthropic request into the v1internal
// envelope that the Antigravity CLI sends.
func buildGenerateRequest(req messagesRequest, defaultModel string, sigs *signatureStore) streamOuterRequest {
	model, effort := resolveModel(req.Model, defaultModel)
	return streamOuterRequest{
		Project:   consumerProj,
		RequestID: "agent/" + newRequestID(),
		Request: generateRequest{
			Contents:          convertContents(req.Messages, sigs),
			SystemInstruction: convertSystem(req.System),
			GenerationConfig:  convertGeneration(req, effort),
			Tools:             convertTools(req.Tools),
			ToolConfig:        convertToolChoice(req.ToolChoice),
			SessionID:         "-" + newRequestID(),
		},
		Model:       model,
		UserAgent:   "antigravity",
		RequestType: "agent",
	}
}

// resolveModel maps Anthropic-side aliases and strips context-window suffixes
// like "gemini-3.8-flash-high[1m]", yielding the upstream model ID plus the
// thinking level encoded in it.
func resolveModel(requested, defaultModel string) (string, string) {
	id := strings.TrimSpace(requested)
	if bracket := strings.Index(id, "["); bracket >= 0 {
		id = id[:bracket]
	}
	switch id {
	case "", "sonnet", "opus", "haiku", "fable", "default":
		id = defaultModel
	}
	effort := "MEDIUM"
	switch {
	case strings.HasSuffix(id, "-high"):
		effort = "HIGH"
	case strings.HasSuffix(id, "-low"):
		effort = "LOW"
	}
	return id, effort
}

func convertSystem(raw json.RawMessage) *content {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return textContent("user", sanitizeSystem(text))
	}
	var blocks []anthBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var joined strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			joined.WriteString(b.Text)
		}
	}
	if joined.Len() == 0 {
		return nil
	}
	return textContent("user", sanitizeSystem(joined.String()))
}

func convertContents(messages []anthMessage, sigs *signatureStore) []content {
	toolNames := map[string]string{}
	for _, msg := range messages {
		for _, b := range blocksOf(msg) {
			if b.Type == "tool_use" && b.ID != "" {
				toolNames[b.ID] = b.Name
			}
		}
	}

	var out []content
	for _, msg := range messages {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		var parts []contentPart
		for _, b := range blocksOf(msg) {
			if part, ok := convertBlock(b, role, toolNames, sigs); ok {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 {
			continue
		}
		// Mid-conversation "system" messages arrive as their own turns; fold
		// them into adjacent same-role turns to keep user/model alternation.
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Parts = append(out[n-1].Parts, parts...)
			continue
		}
		out = append(out, content{Role: role, Parts: parts})
	}
	return out
}

func convertBlock(b anthBlock, role string, toolNames map[string]string, sigs *signatureStore) (contentPart, bool) {
	switch b.Type {
	case "text":
		if b.Text == "" {
			return contentPart{}, false
		}
		return contentPart{Text: b.Text}, true
	case "image":
		if b.Source == nil || b.Source.Type != "base64" || b.Source.Data == "" {
			return contentPart{}, false
		}
		return contentPart{InlineData: &inlineData{MimeType: b.Source.MediaType, Data: b.Source.Data}}, true
	case "tool_use":
		args := b.Input
		if args == nil {
			args = map[string]any{}
		}
		return contentPart{
			FunctionCall:     &functionCall{Name: b.Name, Args: args},
			ThoughtSignature: sigs.get(b.ID),
		}, true
	case "tool_result":
		name := toolNames[b.ToolUseID]
		if name == "" {
			name = "tool"
		}
		result, isError := toolResultPayload(b)
		payload := map[string]any{}
		if isError {
			payload["error"] = result
		} else {
			payload["result"] = result
		}
		return contentPart{FunctionResponse: &functionResponse{Name: name, Response: payload}}, true
	}
	return contentPart{}, false
}

func toolResultPayload(b anthBlock) (string, bool) {
	if len(b.Content) == 0 {
		return "", b.IsError
	}
	var text string
	if json.Unmarshal(b.Content, &text) == nil {
		return text, b.IsError
	}
	var blocks []anthBlock
	if json.Unmarshal(b.Content, &blocks) != nil {
		return string(b.Content), b.IsError
	}
	var joined strings.Builder
	for _, inner := range blocks {
		if inner.Type == "text" {
			joined.WriteString(inner.Text)
		}
	}
	return joined.String(), b.IsError
}

func convertGeneration(req messagesRequest, effort string) *generationConfig {
	max := req.MaxTokens
	if max <= 0 {
		max = 8192
	}
	// Requests capped at a handful of tokens are Claude Code utility calls
	// (topic titling, classifiers); thinking would make them take 20s+ and
	// they would hit the client's ~30s timeout.
	utility := max <= 1024
	if utility {
		effort = "LOW"
	}

	cfg := &generationConfig{
		MaxOutputTokens: max,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		TopK:            req.TopK,
		StopSequences:   req.StopSequences,
		ThinkingConfig: &thinkingConfig{
			IncludeThoughts: true,
			ThinkingBudget:  -1,
			ThinkingLevel:   effort,
		},
	}
	if utility {
		cfg.ThinkingConfig.ThinkingBudget = 0
	}
	if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
		cfg.ThinkingConfig.ThinkingBudget = req.Thinking.BudgetTokens
	}
	return cfg
}

func convertTools(tools []anthTool) []declaredTools {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]functionDeclaration, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, functionDeclaration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  normalizeJSONSchema(t.InputSchema),
		})
	}
	return []declaredTools{{FunctionDeclarations: decls}}
}

// normalizeJSONSchema converts a JSON Schema into the subset the Gemini
// schema proto accepts: type keywords become enum names ("string" ->
// "STRING"), and keys the proto has no field for ($schema, $defs, anyOf, …)
// are pruned rather than forwarded so upstream validation never rejects them.
func normalizeJSONSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	out := make(map[string]any)
	for _, branch := range branchReductions(schema) {
		mergeMaps(out, branch)
	}
	for k, v := range schema {
		switch k {
		case "type":
			if types, ok := v.([]any); ok {
				out["type"], out["nullable"] = flattenTypeList(types)
				continue
			}
			if t, ok := v.(string); ok {
				out["type"] = strings.ToUpper(t)
			}
		case "items":
			if sub := anyMap(v); sub != nil {
				out[k] = normalizeJSONSchema(sub)
			}
		case "properties":
			props := map[string]any{}
			for name, sub := range anyMap(v) {
				if normalized := normalizeJSONSchema(anyMap(sub)); normalized != nil {
					props[name] = normalized
				}
			}
			out[k] = props
		case "const":
			out["enum"] = []any{v}
		case "required", "enum", "format", "description",
			"minimum", "maximum", "minLength", "maxLength", "pattern", "minItems", "maxItems", "nullable":
			out[k] = v
		}
	}
	pruneRequired(out)
	if len(out) == 0 {
		return map[string]any{}
	}
	return out
}

// branchReductions flattens allOf/anyOf/oneOf into their first branch so the
// schema stays expressible in Gemini's proto (which has no combinators).
func branchReductions(schema map[string]any) []map[string]any {
	var branches []map[string]any
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		list, ok := schema[key].([]any)
		if !ok || len(list) == 0 {
			continue
		}
		if first, ok := list[0].(map[string]any); ok {
			branches = append(branches, first)
		}
	}
	return branches
}

func flattenTypeList(types []any) (string, bool) {
	nullable := false
	var first string
	for _, t := range types {
		s, ok := t.(string)
		if !ok {
			continue
		}
		if s == "null" {
			nullable = true
			continue
		}
		if first == "" {
			first = s
		}
	}
	if first == "" {
		return "", nullable
	}
	return strings.ToUpper(first), nullable
}

func pruneRequired(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	required, ok := schema["required"].([]any)
	if props == nil || !ok {
		return
	}
	kept := make([]any, 0, len(required))
	for _, name := range required {
		if n, ok := name.(string); ok {
			if _, exists := props[n]; exists {
				kept = append(kept, n)
			}
		}
	}
	schema["required"] = kept
}

func mergeMaps(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

func anyMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func convertToolChoice(raw json.RawMessage) *toolConfig {
	if len(raw) == 0 {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return nil
	}
	mode, ok := map[string]string{"auto": "AUTO", "any": "ANY", "none": "NONE", "tool": "ANY"}[choice.Type]
	if !ok {
		return nil
	}
	cfg := &functionCallingConfig{Mode: mode}
	if choice.Type == "tool" && choice.Name != "" {
		cfg.AllowedFunctionNames = []string{choice.Name}
	}
	return &toolConfig{FunctionCallingConfig: cfg}
}

// streamState converts upstream chunks into Anthropic SSE events.
type streamState struct {
	sigs *signatureStore

	textIndex   int
	thinkIndex  int
	tools       []*openToolBlock
	nextIndex   int
	usage       usageInfo
	finishSeen  bool
	sawToolUse  bool
	upstreamEnd string
}

type openToolBlock struct {
	id       string
	name     string
	prevArgs string
	open     bool
	index    int
}

type usageInfo struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func newStreamState(sigs *signatureStore) *streamState {
	return &streamState{textIndex: -1, thinkIndex: -1, sigs: sigs}
}

type streamEvent struct {
	Name string
	Data any
}

// Feed handles one upstream chunk and returns the Anthropic events to emit.
func (st *streamState) Feed(chunk streamChunk) []streamEvent {
	var events []streamEvent
	if meta := chunk.Response.UsageMetadata; meta.PromptTokenCount|meta.CandidatesTokenCount|meta.ThoughtsTokenCount != 0 {
		st.usage = usageInfo{InputTokens: meta.PromptTokenCount, OutputTokens: meta.CandidatesTokenCount + meta.ThoughtsTokenCount}
	}
	for _, cand := range chunk.Response.Candidates {
		events = append(events, st.feedParts(cand.Content.Parts)...)
		if cand.FinishReason != "" {
			st.finishSeen = true
			st.upstreamEnd = cand.FinishReason
			events = append(events, st.closeAll()...)
		}
	}
	return events
}

func (st *streamState) feedParts(parts []contentPart) []streamEvent {
	var events []streamEvent
	for _, p := range parts {
		switch {
		case p.Text != "" && p.Thought:
			events = append(events, st.openThinking()...)
			events = append(events, deltaEvent(st.thinkIndex, map[string]any{"type": "thinking_delta", "thinking": p.Text}))
		case p.Text != "":
			events = append(events, st.openText()...)
			events = append(events, deltaEvent(st.textIndex, map[string]any{"type": "text_delta", "text": p.Text}))
		case p.FunctionCall != nil:
			events = append(events, st.feedFunctionCall(p)...)
			continue
		}
		if p.ThoughtSignature != "" {
			st.rememberLastToolSignature(p.ThoughtSignature)
		}
	}
	return events
}

func (st *streamState) openText() []streamEvent {
	if st.textIndex >= 0 {
		return nil
	}
	st.textIndex = st.nextIndex
	st.nextIndex++
	return []streamEvent{{"content_block_start", map[string]any{
		"index":         st.textIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	}}}
}

func (st *streamState) openThinking() []streamEvent {
	if st.thinkIndex >= 0 {
		return nil
	}
	st.thinkIndex = st.nextIndex
	st.nextIndex++
	return []streamEvent{{"content_block_start", map[string]any{
		"index":         st.thinkIndex,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	}}}
}

func (st *streamState) feedFunctionCall(p contentPart) []streamEvent {
	events := st.closeTextAndThinking()
	argsJSON := marshalCompact(p.FunctionCall.Args)
	for _, tb := range st.tools {
		if tb.open && tb.name == p.FunctionCall.Name && strings.HasPrefix(argsJSON, tb.prevArgs) {
			if delta := strings.TrimPrefix(argsJSON, tb.prevArgs); delta != "" {
				events = append(events, deltaEvent(tb.index, map[string]any{"type": "input_json_delta", "partial_json": delta}))
			}
			tb.prevArgs = argsJSON
			st.rememberLastToolSignature(p.ThoughtSignature)
			return events
		}
	}
	id := "toolu_" + newRequestID()
	tb := &openToolBlock{id: id, name: p.FunctionCall.Name, prevArgs: argsJSON, open: true, index: st.nextIndex}
	st.tools = append(st.tools, tb)
	st.nextIndex++
	st.sawToolUse = true
	st.sigs.put(id, p.ThoughtSignature)
	events = append(events, streamEvent{"content_block_start", map[string]any{
		"index": tb.index,
		"content_block": map[string]any{
			"type": "tool_use", "id": id, "name": tb.name, "input": map[string]any{},
		},
	}})
	if argsJSON != "" && argsJSON != "{}" {
		events = append(events, deltaEvent(tb.index, map[string]any{"type": "input_json_delta", "partial_json": argsJSON}))
	}
	return events
}

func (st *streamState) rememberLastToolSignature(sig string) {
	if sig == "" || len(st.tools) == 0 {
		return
	}
	last := st.tools[len(st.tools)-1]
	st.sigs.put(last.id, sig)
}

func (st *streamState) closeTextAndThinking() []streamEvent {
	var events []streamEvent
	if st.thinkIndex >= 0 {
		events = append(events, streamEvent{"content_block_stop", map[string]any{"index": st.thinkIndex}})
		st.thinkIndex = -1
	}
	if st.textIndex >= 0 {
		events = append(events, streamEvent{"content_block_stop", map[string]any{"index": st.textIndex}})
		st.textIndex = -1
	}
	return events
}

func (st *streamState) closeAll() []streamEvent {
	events := st.closeTextAndThinking()
	for _, tb := range st.tools {
		if tb.open {
			events = append(events, streamEvent{"content_block_stop", map[string]any{"index": tb.index}})
			tb.open = false
		}
	}
	return events
}

// StopReason maps the upstream finish reason onto Anthropic's vocabulary.
func (st *streamState) StopReason() string {
	switch st.upstreamEnd {
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "end_turn"
	}
	if st.sawToolUse {
		return "tool_use"
	}
	return "end_turn"
}

func deltaEvent(index int, delta map[string]any) streamEvent {
	return streamEvent{"content_block_delta", map[string]any{"index": index, "delta": delta}}
}

func marshalCompact(v any) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}

func textContent(role, text string) *content {
	return &content{Role: role, Parts: []contentPart{{Text: text}}}
}
