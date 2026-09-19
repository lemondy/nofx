package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"nofx/logger"
	"strings"
	"time"
)

const (
	ProviderCustom = "custom"

	MCPClientTemperature = 0.5
)

var (
	DefaultTimeout = 120 * time.Second

	MaxRetryTimes = 2

	retryableErrors = []string{
		"EOF",
		"timeout",
		"Timeout", // http.Client errors read "Client.Timeout exceeded ..."
		"deadline exceeded",
		"connection reset",
		"connection refused",
		"temporary failure",
		"no such host",
		"stream error",   // HTTP/2 stream error
		"INTERNAL_ERROR", // Server internal error
		"status 502",     // Bad Gateway
		"status 503",     // Service Unavailable
		"status 520",     // Cloudflare origin error
		"status 524",     // Cloudflare timeout
	}

	// TokenUsageCallback is called after each AI request with token usage info
	TokenUsageCallback func(usage TokenUsage)
)

// TokenUsage represents token usage from AI API response
type TokenUsage struct {
	Provider         string // provider name
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Client AI API configuration
type Client struct {
	Provider   string
	APIKey     string
	BaseURL    string
	Model      string
	UseFullURL bool // Whether to use full URL (without appending /chat/completions)
	MaxTokens  int  // Maximum tokens for AI response

	HTTPClient *http.Client // Exported for sub-packages
	Log        Logger       // Exported for sub-packages
	Cfg        *Config      // Exported for sub-packages

	// Hooks are used to implement dynamic dispatch (polymorphism)
	// When provider.DeepSeekClient embeds Client, Hooks point to DeepSeekClient
	// This way methods called in Call() are automatically dispatched to the overridden version
	Hooks ClientHooks
}

// New creates default client (backward compatible)
//
// Deprecated: Recommend using NewClient(...opts) for better flexibility
func New() AIClient {
	return NewClient()
}

// NewClient creates client (supports options pattern)
//
// Usage examples:
//
//	// Basic usage (backward compatible)
//	client := mcp.NewClient()
//
//	// Custom logger
//	client := mcp.NewClient(mcp.WithLogger(customLogger))
//
//	// Custom timeout
//	client := mcp.NewClient(mcp.WithTimeout(60*time.Second))
//
//	// Combine multiple options
//	client := mcp.NewClient(
//	    mcp.WithDeepSeekConfig("sk-xxx"),
//	    mcp.WithLogger(customLogger),
//	    mcp.WithTimeout(60*time.Second),
//	)
func NewClient(opts ...ClientOption) AIClient {
	// 1. Create default config
	cfg := DefaultConfig()

	// 2. Apply user options
	for _, opt := range opts {
		opt(cfg)
	}

	// 3. Create client instance
	client := &Client{
		Provider:   cfg.Provider,
		APIKey:     cfg.APIKey,
		BaseURL:    cfg.BaseURL,
		Model:      cfg.Model,
		MaxTokens:  cfg.MaxTokens,
		UseFullURL: cfg.UseFullURL,
		HTTPClient: cfg.HTTPClient,
		Log:        cfg.Logger,
		Cfg:        cfg,
	}

	// 4. Set default Provider (if not set)
	if client.Provider == "" {
		client.Provider = ProviderDeepSeek
		client.BaseURL = DefaultDeepSeekBaseURL
		client.Model = DefaultDeepSeekModel
	}

	// 5. Set hooks to point to self
	client.Hooks = client

	return client
}

// SetCustomAPI sets custom OpenAI-compatible API
func (client *Client) SetAPIKey(apiKey, apiURL, customModel string) {
	client.Provider = ProviderCustom
	client.APIKey = apiKey

	// Check if URL ends with #, if so use full URL (without appending /chat/completions)
	if strings.HasSuffix(apiURL, "#") {
		client.BaseURL = strings.TrimSuffix(apiURL, "#")
		client.UseFullURL = true
	} else {
		client.BaseURL = apiURL
		client.UseFullURL = false
	}

	client.Model = customModel
}

func (client *Client) SetTimeout(timeout time.Duration) {
	client.HTTPClient.Timeout = timeout
}

// CallWithMessages template method - fixed retry flow (cannot be overridden)
func (client *Client) CallWithMessages(systemPrompt, userPrompt string) (string, error) {
	if client.APIKey == "" {
		return "", fmt.Errorf("AI API key not set, please call SetAPIKey first")
	}

	// Fixed retry flow
	var lastErr error
	maxRetries := client.Cfg.MaxRetries

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			// 09-19: the retry line used to swallow lastErr — a night of
			// "context deadline exceeded" failures was invisible at the
			// retry point and only surfaced in the final aggregate error.
			client.Log.Warnf("⚠️  AI API call failed (attempt %d/%d): %v — retrying", attempt-1, maxRetries, lastErr)
		}

		// Streamable OpenAI-compatible providers use SSE streaming: headers
		// return immediately, so a long reasoning generation no longer hits
		// the "awaiting headers" request timeout. Non-streamable providers
		// (claude wire format) keeps the fixed non-stream flow.
		var result string
		var err error
		if client.Cfg.StreamDecisions && client.streamableProvider() {
			result, err = client.callStreamSingle(systemPrompt, userPrompt, streamHardCap(attempt))
		} else {
			result, err = client.Hooks.Call(systemPrompt, userPrompt)
		}
		if err == nil {
			if attempt > 1 {
				client.Log.Infof("✓ AI API retry succeeded")
			}
			return result, nil
		}

		lastErr = err
		// Check if error is retryable via hooks (supports custom retry strategy)
		if !client.Hooks.IsRetryableError(err) {
			return "", err
		}

		// Wait before retry
		if attempt < maxRetries {
			waitTime := client.Cfg.RetryWaitBase * time.Duration(attempt)
			client.Log.Infof("⏳ Waiting %v before retry...", waitTime)
			time.Sleep(waitTime)
		}
	}

	return "", fmt.Errorf("still failed after %d retries: %w", maxRetries, lastErr)
}

func (client *Client) SetAuthHeader(reqHeader http.Header) {
	reqHeader.Set("Authorization", fmt.Sprintf("Bearer %s", client.APIKey))
}

func (client *Client) BuildMCPRequestBody(systemPrompt, userPrompt string) map[string]any {
	// Build messages array
	messages := []map[string]string{}

	// If system prompt exists, add system message
	if systemPrompt != "" {
		messages = append(messages, map[string]string{
			"role":    "system",
			"content": systemPrompt,
		})
	}
	// Add user message
	messages = append(messages, map[string]string{
		"role":    "user",
		"content": userPrompt,
	})

	// Guard: truncate messages if they would exceed the model's context window
	if client.Cfg.MaxContext > 0 {
		truncated, removed := truncateMessages(messages, client.Cfg.MaxContext, client.MaxTokens)
		if removed > 0 {
			client.Log.Warnf("⚠️  [%s] Context guard: truncated %d oldest messages to fit within %d token limit",
				client.String(), removed, client.Cfg.MaxContext)
			messages = truncated
		}
	}

	// Build request body
	requestBody := map[string]interface{}{
		"model":       client.Model,
		"messages":    messages,
		"temperature": client.Cfg.Temperature, // Use configured temperature
	}
	// OpenAI newer models use max_completion_tokens instead of max_tokens
	if client.Provider == ProviderOpenAI {
		requestBody["max_completion_tokens"] = client.MaxTokens
	} else {
		requestBody["max_tokens"] = client.MaxTokens
	}
	return requestBody
}

// MarshalRequestBody can be used to marshal the request body and can be overridden
func (client *Client) MarshalRequestBody(requestBody map[string]any) ([]byte, error) {
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize request: %w", err)
	}
	return jsonData, nil
}

func (client *Client) ParseMCPResponse(body []byte) (string, error) {
	r, err := client.ParseMCPResponseFull(body)
	if err != nil {
		return "", err
	}
	return r.Content, nil
}

// ParseMCPResponseFull parses the OpenAI-format response body and returns both
// the text content and any tool calls.
func (client *Client) ParseMCPResponseFull(body []byte) (*LLMResponse, error) {
	var result struct {
		Choices []struct {
			Message struct {
				Content          string     `json:"content"`
				ReasoningContent string     `json:"reasoning_content"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("API returned empty response")
	}

	// Report token usage if callback is set
	if TokenUsageCallback != nil && result.Usage.TotalTokens > 0 {
		TokenUsageCallback(TokenUsage{
			Provider:         client.Provider,
			Model:            client.Model,
			PromptTokens:     result.Usage.PromptTokens,
			CompletionTokens: result.Usage.CompletionTokens,
			TotalTokens:      result.Usage.TotalTokens,
		})
	}

	msg := result.Choices[0].Message
	content := msg.Content

	// Reasoning models (GLM thinking, DeepSeek-R1, Qwen thinking, ...) may put
	// all their output in reasoning_content and leave content empty — fall back
	// to it so callers don't see a blank response.
	if strings.TrimSpace(content) == "" && strings.TrimSpace(msg.ReasoningContent) != "" {
		client.Log.Infof("🧠 [%s] Empty content, falling back to reasoning_content (%d chars)",
			client.String(), len(msg.ReasoningContent))
		content = msg.ReasoningContent
	}

	if strings.TrimSpace(content) == "" {
		client.Log.Warnf("⚠️ [%s] AI response has empty content (finish_reason=%s, body %d bytes) — response may have been truncated or filtered",
			client.String(), result.Choices[0].FinishReason, len(body))
	}

	// finish_reason=length means the output hit max_tokens mid-generation:
	// whatever JSON the caller expected is amputated. Surface it loudly here
	// (the response still flows to the caller, which salvages what it can)
	// so the fix (raise AI_MAX_TOKENS) is diagnosable from the log alone.
	if result.Choices[0].FinishReason == "length" {
		client.Log.Warnf("⚠️ [%s] Response TRUNCATED by max_tokens (finish_reason=length, %d completion tokens) — decision JSON is cut off. Raise AI_MAX_TOKENS.",
			client.String(), result.Usage.CompletionTokens)
	}

	return &LLMResponse{
		Content:   content,
		ToolCalls: msg.ToolCalls,
	}, nil
}

func (client *Client) BuildUrl() string {
	if client.UseFullURL {
		return client.BaseURL
	}
	return fmt.Sprintf("%s/chat/completions", client.BaseURL)
}

func (client *Client) BuildRequest(url string, jsonData []byte) (*http.Request, error) {
	// Create HTTP request
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("fail to build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Set auth header via hooks (supports overriding)
	client.Hooks.SetAuthHeader(req.Header)

	return req, nil
}

// Call single AI API call (fixed flow, cannot be overridden)
func (client *Client) Call(systemPrompt, userPrompt string) (string, error) {
	// Print current AI configuration
	client.Log.Infof("📡 [%s] Request AI Server: BaseURL: %s", client.String(), client.BaseURL)
	client.Log.Debugf("[%s] UseFullURL: %v", client.String(), client.UseFullURL)
	if len(client.APIKey) > 8 {
		client.Log.Debugf("[%s]   API Key: %s...%s", client.String(), client.APIKey[:4], client.APIKey[len(client.APIKey)-4:])
	}

	// Step 1: Build request body (via hooks for dynamic dispatch)
	requestBody := client.Hooks.BuildMCPRequestBody(systemPrompt, userPrompt)

	// Step 2: Serialize request body (via hooks for dynamic dispatch)
	jsonData, err := client.Hooks.MarshalRequestBody(requestBody)
	if err != nil {
		return "", err
	}

	// Step 3: Build URL (via hooks for dynamic dispatch)
	url := client.Hooks.BuildUrl()
	client.Log.Infof("📡 [MCP %s] Request URL: %s", client.String(), url)

	// Step 4: Create HTTP request (fixed logic)
	req, err := client.Hooks.BuildRequest(url, jsonData)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Step 5: Send HTTP request (fixed logic)
	resp, err := client.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	// Step 6: Read response body (fixed logic)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// Step 7: Check HTTP status code (fixed logic)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned error (status %d): %s", resp.StatusCode, string(body))
	}

	// Step 8: Parse response (via hooks for dynamic dispatch)
	result, err := client.Hooks.ParseMCPResponse(body)
	if err != nil {
		return "", fmt.Errorf("fail to parse AI server response: %w", err)
	}

	return result, nil
}

func (client *Client) String() string {
	return fmt.Sprintf("[Provider: %s, Model: %s]",
		client.Provider, client.Model)
}

// BaseClient returns the underlying *Client (satisfies ClientEmbedder interface).
func (c *Client) BaseClient() *Client { return c }

// IsRetryableError determines if error is retryable (network errors, timeouts, etc.)
func (client *Client) IsRetryableError(err error) bool {
	// Case-insensitive matching: Go's http errors read "Client.Timeout exceeded"
	// while context errors read "context deadline exceeded".
	errStr := strings.ToLower(err.Error())
	for _, retryable := range client.Cfg.RetryableErrors {
		if strings.Contains(errStr, strings.ToLower(retryable)) {
			return true
		}
	}
	return false
}

// ============================================================
// Builder Pattern API (Advanced Features)
// ============================================================

// CallWithRequest calls AI API using Request object (supports advanced features)
func (client *Client) CallWithRequest(req *Request) (string, error) {
	if client.APIKey == "" {
		return "", fmt.Errorf("AI API key not set, please call SetAPIKey first")
	}

	// If Model is not set in Request, use Client's Model
	if req.Model == "" {
		req.Model = client.Model
	}

	// Fixed retry flow
	var lastErr error
	maxRetries := client.Cfg.MaxRetries

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			client.Log.Warnf("⚠️  AI API call failed, retrying (%d/%d)...", attempt, maxRetries)
		}

		// Call single request
		result, err := client.callWithRequest(req)
		if err == nil {
			if attempt > 1 {
				client.Log.Infof("✓ AI API retry succeeded")
			}
			return result, nil
		}

		lastErr = err
		// Check if error is retryable
		if !client.Hooks.IsRetryableError(err) {
			return "", err
		}

		// Wait before retry
		if attempt < maxRetries {
			waitTime := client.Cfg.RetryWaitBase * time.Duration(attempt)
			client.Log.Infof("⏳ Waiting %v before retry...", waitTime)
			time.Sleep(waitTime)
		}
	}

	return "", fmt.Errorf("still failed after %d retries: %w", maxRetries, lastErr)
}

// CallWithRequestFull calls the AI API and returns both text content and tool calls.
func (client *Client) CallWithRequestFull(req *Request) (*LLMResponse, error) {
	if client.APIKey == "" {
		return nil, fmt.Errorf("AI API key not set, please call SetAPIKey first")
	}
	if req.Model == "" {
		req.Model = client.Model
	}

	var lastErr error
	maxRetries := client.Cfg.MaxRetries
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			client.Log.Warnf("⚠️  AI API call failed, retrying (%d/%d)...", attempt, maxRetries)
		}
		result, err := client.callWithRequestFull(req)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !client.Hooks.IsRetryableError(err) {
			return nil, err
		}
		if attempt < maxRetries {
			waitTime := client.Cfg.RetryWaitBase * time.Duration(attempt)
			time.Sleep(waitTime)
		}
	}
	return nil, fmt.Errorf("still failed after %d retries: %w", maxRetries, lastErr)
}

// callWithRequestFull single call that returns LLMResponse (content + tool calls).
func (client *Client) callWithRequestFull(req *Request) (*LLMResponse, error) {
	client.Log.Infof("📡 [%s] Request AI Server (full): BaseURL: %s", client.String(), client.BaseURL)

	requestBody := client.Hooks.BuildRequestBodyFromRequest(req)
	jsonData, err := client.Hooks.MarshalRequestBody(requestBody)
	if err != nil {
		return nil, err
	}

	url := client.Hooks.BuildUrl()
	httpReq, err := client.Hooks.BuildRequest(url, jsonData)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned error (status %d): %s", resp.StatusCode, string(body))
	}

	return client.Hooks.ParseMCPResponseFull(body)
}

// callWithRequest single AI API call (using Request object)
func (client *Client) callWithRequest(req *Request) (string, error) {
	// Print current AI configuration
	client.Log.Infof("📡 [%s] Request AI Server with Builder: BaseURL: %s", client.String(), client.BaseURL)
	client.Log.Debugf("[%s] Messages count: %d", client.String(), len(req.Messages))

	requestBody := client.Hooks.BuildRequestBodyFromRequest(req)

	jsonData, err := client.Hooks.MarshalRequestBody(requestBody)
	if err != nil {
		return "", err
	}

	url := client.Hooks.BuildUrl()
	client.Log.Infof("📡 [MCP %s] Request URL: %s", client.String(), url)

	httpReq, err := client.Hooks.BuildRequest(url, jsonData)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.HTTPClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned error (status %d): %s", resp.StatusCode, string(body))
	}

	result, err := client.Hooks.ParseMCPResponse(body)
	if err != nil {
		return "", fmt.Errorf("fail to parse AI server response: %w", err)
	}

	return result, nil
}

// BuildRequestBodyFromRequest builds request body from Request object
func (client *Client) BuildRequestBodyFromRequest(req *Request) map[string]any {
	// Convert Message to API format — must use map[string]any to support
	// tool-call messages (tool_calls, tool_call_id fields).
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, msg := range req.Messages {
		m := map[string]any{"role": msg.Role}
		if len(msg.ToolCalls) > 0 {
			// Assistant message that contains tool invocations.
			// content must be null/omitted for OpenAI compatibility.
			m["tool_calls"] = msg.ToolCalls
		} else if msg.ToolCallID != "" {
			// Tool result message (role="tool").
			m["tool_call_id"] = msg.ToolCallID
			m["content"] = msg.Content
		} else {
			m["content"] = msg.Content
		}
		messages = append(messages, m)
	}

	// Guard: truncate messages if they would exceed the model's context window
	maxOut := client.MaxTokens
	if req.MaxTokens != nil {
		maxOut = *req.MaxTokens
	}
	if client.Cfg.MaxContext > 0 {
		truncated, removed := truncateMessagesAny(messages, client.Cfg.MaxContext, maxOut)
		if removed > 0 {
			client.Log.Warnf("⚠️  [%s] Context guard: truncated %d oldest messages to fit within %d token limit",
				client.String(), removed, client.Cfg.MaxContext)
			messages = truncated
		}
	}

	// Build basic request body
	requestBody := map[string]interface{}{
		"model":    req.Model,
		"messages": messages,
	}

	// Add optional parameters (only add non-nil parameters)
	if req.Temperature != nil {
		requestBody["temperature"] = *req.Temperature
	} else {
		// If not set in Request, use Client's configuration
		requestBody["temperature"] = client.Cfg.Temperature
	}

	// OpenAI newer models use max_completion_tokens instead of max_tokens
	tokenKey := "max_tokens"
	if client.Provider == ProviderOpenAI {
		tokenKey = "max_completion_tokens"
	}
	if req.MaxTokens != nil {
		requestBody[tokenKey] = *req.MaxTokens
	} else {
		// If not set in Request, use Client's MaxTokens
		requestBody[tokenKey] = client.MaxTokens
	}

	if req.TopP != nil {
		requestBody["top_p"] = *req.TopP
	}

	if req.FrequencyPenalty != nil {
		requestBody["frequency_penalty"] = *req.FrequencyPenalty
	}

	if req.PresencePenalty != nil {
		requestBody["presence_penalty"] = *req.PresencePenalty
	}

	if len(req.Stop) > 0 {
		requestBody["stop"] = req.Stop
	}

	if len(req.Tools) > 0 {
		requestBody["tools"] = req.Tools
	}

	if req.ToolChoice != "" {
		requestBody["tool_choice"] = req.ToolChoice
	}

	if req.Stream {
		requestBody["stream"] = true
	}

	return requestBody
}

// CallWithRequestStream streams the LLM response via SSE (Server-Sent Events).
// onChunk is called with the full accumulated text so far after each received chunk.
// Returns the complete final text when the stream ends.
//
// Idle timeout: if no chunk arrives for 30 seconds the stream is cancelled automatically.
// This prevents the scanner from blocking indefinitely on a hung or stalled connection.
func (client *Client) CallWithRequestStream(req *Request, onChunk func(string)) (string, error) {
	if client.APIKey == "" {
		return "", fmt.Errorf("AI API key not set")
	}
	if req.Model == "" {
		req.Model = client.Model
	}
	req.Stream = true

	requestBody := client.Hooks.BuildRequestBodyFromRequest(req)
	jsonData, err := client.Hooks.MarshalRequestBody(requestBody)
	if err != nil {
		return "", err
	}

	url := client.Hooks.BuildUrl()
	httpReq, err := client.Hooks.BuildRequest(url, jsonData)
	if err != nil {
		return "", err
	}

	// Idle-timeout watchdog: cancel the request if no SSE line arrives for 60 seconds.
	// This breaks the scanner out of an indefinitely blocking Read on a hung connection.
	const idleTimeout = 60 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetCh := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(idleTimeout)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cancel() // idle timeout: kill the connection
				return
			case <-resetCh:
				// received a line — reset the idle timer
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(idleTimeout)
			}
		}
	}()

	httpReq = httpReq.WithContext(ctx)
	resp, err := client.HTTPClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("streaming request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	return ParseSSEStream(resp.Body, onChunk, func() {
		select {
		case resetCh <- struct{}{}:
		default:
		}
	})
}

// ParseSSEStream reads an SSE response body, accumulates text deltas,
// and calls onChunk with the full accumulated text after each chunk.
// If onLine is non-nil, it is called after each raw SSE line is scanned
// (useful for resetting idle-timeout watchdogs).
// Returns the complete accumulated text.
func ParseSSEStream(body io.Reader, onChunk func(string), onLine func()) (string, error) {
	var accumulated strings.Builder
	var reasoning strings.Builder
	scanner := bufio.NewScanner(body)

	for scanner.Scan() {
		if onLine != nil {
			onLine()
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage,omitempty"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // skip malformed chunks
		}

		if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
			fmt.Printf("📊 [TokenUsage] prompt=%d, completion=%d, total=%d\n",
				chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens)
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta.Content
		if delta != "" {
			accumulated.WriteString(delta)
			if onChunk != nil {
				onChunk(accumulated.String())
			}
			continue
		}

		// Reasoning models stream their thinking via reasoning_content — keep
		// it as a fallback in case content never arrives.
		if rc := chunk.Choices[0].Delta.ReasoningContent; rc != "" {
			reasoning.WriteString(rc)
		}
	}

	if err := scanner.Err(); err != nil {
		return accumulated.String(), fmt.Errorf("stream interrupted: %w", err)
	}

	// Content-only fallback: if the model produced reasoning but no final
	// content, surface the reasoning instead of an empty string.
	if strings.TrimSpace(accumulated.String()) == "" && reasoning.Len() > 0 {
		return reasoning.String(), nil
	}

	return accumulated.String(), nil
}

// streamableProvider reports whether this provider speaks OpenAI-compatible
// SSE streaming. claude (Anthropic wire format)
// must stay non-stream.
func (client *Client) streamableProvider() bool {
	switch client.Provider {
	case ProviderOpenAI, ProviderDeepSeek, ProviderQwen, ProviderGLM,
		ProviderKimi, ProviderMiniMax, ProviderGrok, ProviderGemini, ProviderCustom:
		return true
	}
	return false
}

// streamHardCap returns the per-attempt hard cap for streaming calls.
// Attempt 1 fails fast at 300s; the retry escalates to 600s. Evidence
// (DB 09-18, mac-nofx/GLM): successful generations ran 253-299s — exactly on
// the old fixed 300s cap — so any provider slow-phase tipped BOTH attempts
// into "context deadline exceeded" and the whole cycle failed. The 90s idle
// watchdog still bounds genuinely hung connections; only slow-but-flowing
// streams ever reach the hard cap.
func streamHardCap(attempt int) time.Duration {
	if attempt <= 1 {
		return 300 * time.Second
	}
	return 600 * time.Second
}

// callStreamSingle performs one streaming attempt: same request shape as the
// non-stream flow plus stream=true, with a 90s idle watchdog and a hard cap
// (300s on the first attempt, 600s on retries — see streamHardCap). Long
// reasoning generations stream chunks continuously, so the idle watchdog —
// not the total timeout — is what bounds a hung connection; the hard cap
// only backstops providers that keep the socket dripping.
func (client *Client) callStreamSingle(systemPrompt, userPrompt string, hardCap time.Duration) (string, error) {
	messages := []map[string]interface{}{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": userPrompt},
	}
	requestBody := map[string]interface{}{
		"model":       client.Model,
		"messages":    messages,
		"temperature": client.Cfg.Temperature,
		"stream":      true,
	}
	// OpenAI newer models use max_completion_tokens instead of max_tokens
	if client.Provider == ProviderOpenAI {
		requestBody["max_completion_tokens"] = client.MaxTokens
	} else {
		requestBody["max_tokens"] = client.MaxTokens
	}

	jsonData, err := client.MarshalRequestBody(requestBody)
	if err != nil {
		return "", err
	}

	url := client.Hooks.BuildUrl()
	httpReq, err := client.Hooks.BuildRequest(url, jsonData)
	if err != nil {
		return "", err
	}

	// Same (SSRF-safe) transport with the per-attempt hard cap. Big strategy
	// prompts + restored ranking context push reasoning generations past
	// 150s, and the hard cap was aborting legitimate mid-stream responses.
	// The 90s idle watchdog still bounds genuinely hung connections.
	httpClient := &http.Client{
		Transport: client.HTTPClient.Transport,
		Timeout:   hardCap,
	}

	const idleTimeout = 90 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resetCh := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(idleTimeout)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cancel()
				return
			case <-resetCh:
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(idleTimeout)
			}
		}
	}()

	httpReq = httpReq.WithContext(ctx)
	client.Log.Infof("📡 [%s] Streaming request: %s", client.String(), url)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("streaming request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		// 4xx usually means the provider rejected the stream parameter —
		// retry this attempt once via the non-stream flow.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			logger.Warnf("⚠️  Stream rejected (status %d) — falling back to non-stream for this attempt", resp.StatusCode)
			return client.Hooks.Call(systemPrompt, userPrompt)
		}
		return "", fmt.Errorf("API error (status %d): %s", resp.StatusCode, truncate(string(body), 200))
	}

	return ParseSSEStream(resp.Body, nil, func() {
		select {
		case resetCh <- struct{}{}:
		default:
		}
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
