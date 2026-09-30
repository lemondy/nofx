package agent

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"nofx/logger"
	"nofx/security"
	"strings"
	"time"
)

// apiCallTool executes HTTP requests against the NOFX API server.
// This is the only tool available to the agent.
type apiCallTool struct {
	pending      *apiRequest
	confirmation string
	expires      time.Time
	baseURL      string
	token        string
	client       *http.Client
}

// apiRequest holds the arguments decoded from the LLM's api_request tool call.
type apiRequest struct {
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Body   map[string]any `json:"body"`
}

func newAPICallTool(port int, token string) *apiCallTool {
	return &apiCallTool{
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", port),
		token:   token,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// execute calls the API and returns the response as a string for LLM consumption.
func (t *apiCallTool) execute(req *apiRequest) string {
	req.Method = strings.ToUpper(req.Method)
	u, err := url.Parse(req.Path)
	if err != nil || u.IsAbs() || u.Host != "" || strings.Contains(u.Path, "..") {
		return "error: invalid API path"
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	allowed := map[string]bool{"my-traders": true, "traders": true, "strategies": true, "models": true, "exchanges": true, "status": true, "account": true, "positions": true, "statistics": true, "decisions": true, "orders": true, "equity-history": true, "ai-costs": true}
	if len(parts) < 2 || parts[0] != "api" || !allowed[parts[1]] {
		return "error: endpoint is not available to Telegram assistant"
	}
	if req.Method != "GET" {
		if (parts[1] != "traders" && parts[1] != "strategies") || (req.Method != "POST" && req.Method != "PUT" && req.Method != "DELETE") {
			return "error: configure credentials and account settings in the web dashboard"
		}
		b, err := json.Marshal(req)
		if err != nil {
			return "error: invalid request"
		}
		var copy apiRequest
		if err := json.Unmarshal(b, &copy); err != nil {
			return "error: invalid request"
		}
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "error: cannot issue confirmation"
		}
		t.pending = &copy
		t.confirmation = hex.EncodeToString(nonce[:])
		t.expires = time.Now().Add(5 * time.Minute)
		body, _ := json.MarshalIndent(copy.Body, "", "  ")
		return fmt.Sprintf("Confirmation required (5 minutes):\n%s %s\n%s\n\nSend /confirm %s to execute exactly this request.", copy.Method, copy.Path, body, t.confirmation)
	}
	return t.executeApproved(req)
}

func (t *apiCallTool) confirm(code string) string {
	if t.pending == nil || time.Now().After(t.expires) || code != t.confirmation {
		return "Confirmation invalid or expired. Request the operation again."
	}
	req := t.pending
	t.pending = nil
	t.confirmation = ""
	return t.executeApproved(req)
}

func (t *apiCallTool) executeApproved(req *apiRequest) string {
	if req.Method == "" || req.Path == "" {
		return "error: method and path are required"
	}
	if !strings.HasPrefix(req.Path, "/") {
		req.Path = "/" + req.Path
	}

	var bodyReader io.Reader
	if req.Method != "GET" && len(req.Body) > 0 {
		b, err := json.Marshal(req.Body)
		if err != nil {
			return fmt.Sprintf("error marshaling body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	httpReq, err := http.NewRequest(req.Method, t.baseURL+req.Path, bodyReader)
	if err != nil {
		return fmt.Sprintf("error creating request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+t.token)

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return fmt.Sprintf("API call failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := security.ReadResponseBody(resp.Body)
	if err != nil {
		return fmt.Sprintf("error reading response: %v", err)
	}

	logger.Infof("Agent api_call: %s %s -> %d", req.Method, req.Path, resp.StatusCode)

	if resp.StatusCode >= 400 {
		return fmt.Sprintf("API error %d: %s", resp.StatusCode, string(body))
	}

	// Pretty-print JSON for better LLM readability
	var v any
	if json.Unmarshal(body, &v) == nil {
		if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
			return string(pretty)
		}
	}
	return string(body)
}
