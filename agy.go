package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Upstream: the Code Assist "v1internal" API used by the Antigravity CLI.
const (
	defaultBaseURL = "https://daily-cloudcode-pa.googleapis.com"
	consumerProj   = "aicode-consumers"
	cliUserAgent   = "antigravity/cli/1.2.2 (aidev_client; os_type=darwin; arch=arm64; cl=980147163; auth_method=consumer)"
)

type agyClient struct {
	baseURL    string
	httpClient *http.Client
	tokens     *tokenManager
	logger     *logger
}

func newAgyClient(baseURL string, tokens *tokenManager, log *logger) *agyClient {
	return &agyClient{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{Timeout: 10 * time.Minute},
		tokens:     tokens,
		logger:     log,
	}
}

// Gemini-side request types (v1internal / google.cloud.ai.v1beta1 JSON shape).

type contentPart struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
	InlineData       *inlineData       `json:"inlineData,omitempty"`
}

type functionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type functionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type content struct {
	Role  string        `json:"role"`
	Parts []contentPart `json:"parts"`
}

type thinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts"`
	ThinkingBudget  int    `json:"thinkingBudget"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
}

type generationConfig struct {
	MaxOutputTokens int             `json:"maxOutputTokens"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"topP,omitempty"`
	TopK            int             `json:"topK,omitempty"`
	StopSequences   []string        `json:"stopSequences,omitempty"`
	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type functionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type declaredTools struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations,omitempty"`
}

type functionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig *functionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type generateRequest struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
	Tools             []declaredTools   `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	SessionID         string            `json:"sessionId,omitempty"`
}

type streamOuterRequest struct {
	Project     string          `json:"project"`
	RequestID   string          `json:"requestId"`
	Request     generateRequest `json:"request"`
	Model       string          `json:"model"`
	UserAgent   string          `json:"userAgent"`
	RequestType string          `json:"requestType"`
}

// StreamChunk is one decoded SSE event from streamGenerateContent.
type streamChunk struct {
	Response struct {
		Candidates []struct {
			Content      content `json:"content"`
			FinishReason string  `json:"finishReason"`
			Index        int     `json:"index"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
		ModelVersion string `json:"modelVersion"`
		ResponseID   string `json:"responseId"`
	} `json:"response"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

// apiError reports an upstream failure with its HTTP status and message text.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return fmt.Sprintf("upstream %d: %s", e.status, e.message) }

// StreamGenerate streams a generation, invoking onChunk for every SSE event.
func (c *agyClient) StreamGenerate(ctx context.Context, req streamOuterRequest, onChunk func(streamChunk) error) error {
	token, err := c.tokens.Token()
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1internal:streamGenerateContent?alt=sse", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("authorization", "Bearer "+token)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("user-agent", cliUserAgent)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return &apiError{status: resp.StatusCode, message: extractErrorMessage(string(msg))}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := data.String()
		data.Reset()
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			c.logger.debugf("skipping unparseable upstream event: %v", err)
			return nil
		}
		if chunk.Error != nil {
			return &apiError{status: chunk.Error.Code, message: chunk.Error.Message}
		}
		return onChunk(chunk)
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "data: "):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(line[len("data: "):])
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return scanner.Err()
}

func extractErrorMessage(raw string) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	// The upstream error body arrives as a JSON array of one object (gRPC
	// transcode streaming), which the first decode pass normalizes away.
	if json.Unmarshal([]byte(strings.TrimPrefix(raw, "[")), &envelope) == nil && envelope.Error.Message != "" {
		if envelope.Error.Status != "" {
			return envelope.Error.Message + " (" + envelope.Error.Status + ")"
		}
		return envelope.Error.Message
	}
	if len(raw) > 300 {
		return raw[:300]
	}
	return raw
}

type modelCatalogEntry struct {
	DisplayName   string `json:"displayName"`
	MaxOutputToks int    `json:"maxOutputTokens"`
	MaxTokens     int    `json:"maxTokens"`
}

type catalog struct {
	mu      sync.Mutex
	loaded  time.Time
	entries map[string]modelCatalogEntry
	client  *agyClient
}

func newCatalog(c *agyClient) *catalog { return &catalog{client: c} }

func (cat *catalog) Models(ctx context.Context) (map[string]modelCatalogEntry, error) {
	cat.mu.Lock()
	defer cat.mu.Unlock()
	if cat.entries != nil && time.Since(cat.loaded) < time.Hour {
		return cat.entries, nil
	}
	token, err := cat.client.tokens.Token()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"project": consumerProj})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cat.client.baseURL+"/v1internal:fetchAvailableModels", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", cliUserAgent)
	resp, err := cat.client.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, &apiError{status: resp.StatusCode, message: extractErrorMessage(string(msg))}
	}
	var payload struct {
		Models map[string]modelCatalogEntry `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	cat.entries = payload.Models
	cat.loaded = time.Now()
	return cat.entries, nil
}
