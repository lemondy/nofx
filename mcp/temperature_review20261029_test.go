package mcp_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"nofx/mcp"
	"nofx/mcp/provider"
)

type temperatureReview20261029Transport func(*http.Request) (*http.Response, error)

func (f temperatureReview20261029Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type temperatureReview20261029Logger struct {
	mcp.Logger
	mu       sync.Mutex
	debug    []string
	warnings []string
}

func (l *temperatureReview20261029Logger) Debugf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debug = append(l.debug, fmt.Sprintf(format, args...))
}

func (l *temperatureReview20261029Logger) Warnf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

func TestTemperatureReview20261029FinalRequests(t *testing.T) {
	value := func(v float64) *float64 { return &v }
	type factory func(...mcp.ClientOption) mcp.AIClient
	for _, tc := range []struct {
		name                  string
		newClient             factory
		model, apiURL, effort string
		reqModel, reqEffort   string
		override, want        *float64
		requestOnly, claude   bool
	}{
		{name: "glm default", newClient: provider.NewGLMClientWithOptions, model: "glm-5.3-flash", want: value(0.5)},
		{name: "openai high", newClient: provider.NewOpenAIClientWithOptions, model: "gpt-5.4", effort: "high"},
		{name: "openai unspecified", newClient: provider.NewOpenAIClientWithOptions, model: "gpt-5.4"},
		{name: "openai none", newClient: provider.NewOpenAIClientWithOptions, model: "gpt-5.4", effort: "none", want: value(0.5)},
		{name: "openai chat exception", newClient: provider.NewOpenAIClientWithOptions, model: "gpt-5-chat-latest", want: value(0.5)},
		{name: "openai old model", newClient: provider.NewOpenAIClientWithOptions, model: "gpt-4.1", want: value(0.5)},
		{name: "openai unknown gpt5", newClient: provider.NewOpenAIClientWithOptions, model: "gpt-5.6-luna"},
		{name: "custom official", newClient: mcp.NewClient, model: "gpt-5.4", apiURL: "https://api.openai.com/v1", effort: "high"},
		{name: "custom official full URL", newClient: mcp.NewClient, model: "gpt-5.4", apiURL: "https://API.OPENAI.COM:443/v1/chat/completions#"},
		{name: "custom official none", newClient: mcp.NewClient, model: "gpt-5.4", apiURL: "https://api.openai.com/v1", effort: "none", want: value(0.5)},
		{name: "custom other host", newClient: mcp.NewClient, model: "gpt-5.4", apiURL: "https://api.openai.com.example.test/v1", want: value(0.5)},
		{name: "custom official in path", newClient: mcp.NewClient, model: "gpt-5.4", apiURL: "https://example.test/api.openai.com", want: value(0.5)},
		{name: "custom official in userinfo", newClient: mcp.NewClient, model: "gpt-5.4", apiURL: "https://api.openai.com@example.test/v1", want: value(0.5)},
		{name: "kimi override", newClient: provider.NewKimiClientWithOptions, model: "kimi-k2.5", want: value(1)},
		{name: "claude default", newClient: provider.NewClaudeClientWithOptions, model: "claude-opus-4-6", claude: true},
		{name: "claude explicit", newClient: provider.NewClaudeClientWithOptions, model: "claude-opus-4-6", override: value(0.2), want: value(0.2), requestOnly: true, claude: true},
		{name: "request override suppressed", newClient: provider.NewOpenAIClientWithOptions, model: "gpt-4.1", reqModel: "gpt-5.4", reqEffort: "high", override: value(0.2), requestOnly: true},
		{name: "request none overrides config", newClient: provider.NewOpenAIClientWithOptions, model: "gpt-5.4", effort: "high", reqEffort: "none", override: value(0), want: value(0), requestOnly: true},
		{name: "request high overrides config", newClient: provider.NewOpenAIClientWithOptions, model: "gpt-5.4", effort: "none", reqEffort: "high", override: value(0.2), requestOnly: true},
		{name: "request chat override", newClient: provider.NewOpenAIClientWithOptions, model: "gpt-5-chat-latest", override: value(0.2), want: value(0.2), requestOnly: true},
	} {
		for _, mode := range []struct {
			name    string
			stream  bool
			request bool
		}{
			{"messages", false, false},
			{"messages stream", true, false},
			{"request", false, true},
			{"request full", false, true},
			{"request stream", true, true},
		} {
			// Claude's Anthropic streaming wire format is not supported by the
			// generic Request SSE API; decision streaming falls back to Call.
			if tc.requestOnly && !mode.request || tc.claude && mode.name == "request stream" {
				continue
			}
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				const apiKey = "secret-review20261029-key"
				const prompt = "private-review20261029-prompt"
				log := &temperatureReview20261029Logger{Logger: mcp.NewNoopLogger()}
				var bodies []map[string]any
				httpClient := &http.Client{Transport: temperatureReview20261029Transport(func(req *http.Request) (*http.Response, error) {
					defer req.Body.Close()
					var body map[string]any
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						return nil, err
					}
					bodies = append(bodies, body)
					response := `{"choices":[{"message":{"content":"ok"}}]}`
					if tc.claude {
						response = `{"content":[{"type":"text","text":"ok"}]}`
					} else if body["stream"] == true {
						response = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
				})}
				c := tc.newClient(mcp.WithAPIKey(apiKey), mcp.WithModel(tc.model), mcp.WithLogger(log),
					mcp.WithHTTPClient(httpClient), mcp.WithStreamDecisions(mode.stream), mcp.WithReasoningEffort(tc.effort))
				if tc.apiURL != "" {
					c.SetAPIKey(apiKey, tc.apiURL, tc.model)
				}
				req := mcp.NewRequestBuilder().WithUserPrompt(prompt).WithReasoningEffort(tc.reqEffort).MustBuild()
				req.Model, req.Temperature = tc.reqModel, tc.override
				var result string
				var err error
				switch mode.name {
				case "messages", "messages stream":
					result, err = c.CallWithMessages(prompt, prompt)
				case "request":
					result, err = c.CallWithRequest(req)
				case "request full":
					var full *mcp.LLMResponse
					full, err = c.CallWithRequestFull(req)
					if full != nil {
						result = full.Content
					}
				case "request stream":
					result, err = c.CallWithRequestStream(req, nil)
				}
				if err != nil || result != "ok" || len(bodies) != 1 {
					t.Fatalf("result=%q error=%v requests=%d", result, err, len(bodies))
				}
				body := bodies[0]
				got, sent := body["temperature"]
				if sent != (tc.want != nil) || sent && got != *tc.want {
					t.Fatalf("temperature=%v sent=%t want=%v", got, sent, tc.want)
				}
				effort := tc.effort
				if mode.request && tc.reqEffort != "" {
					effort = tc.reqEffort
				}
				if !tc.claude && effort != "" && body["reasoning_effort"] != effort {
					t.Errorf("reasoning_effort=%v want=%q", body["reasoning_effort"], effort)
				}
				if effort == "" {
					if _, present := body["reasoning_effort"]; present {
						t.Error("unset reasoning effort must stay omitted")
					}
				}
				model := tc.model
				if mode.request && tc.reqModel != "" {
					model = tc.reqModel
				}
				base := c.(mcp.ClientEmbedder).BaseClient()
				if body["model"] != model {
					t.Errorf("model=%v want=%q", body["model"], model)
				}
				parameterLogs := 0
				for _, line := range log.debug {
					if strings.Contains(line, apiKey) || strings.Contains(line, prompt) {
						t.Error("request log exposed credentials or prompt")
					}
					if strings.Contains(line, "LLM request parameters:") {
						parameterLogs++
						value := "omitted"
						if tc.want != nil {
							value = fmt.Sprint(*tc.want)
						}
						wantLog := fmt.Sprintf("provider=%s model=%s temperature_sent=%t temperature=%s", base.Provider, model, sent, value)
						if !strings.Contains(line, wantLog) {
							t.Errorf("parameter log=%q want=%q", line, wantLog)
						}
					}
				}
				if parameterLogs != 1 {
					t.Errorf("parameter logs=%d want=1", parameterLogs)
				}
			})
		}
	}
}

func TestTemperatureReview20261029ReasoningFamilies(t *testing.T) {
	for _, model := range []string{"o1", "o1-preview", "o3", "o3-mini", "o4-mini", "gpt-5", "gpt-5-mini", "gpt-5-nano", "gpt-5.2", "gpt-5.4", "gpt-5.6-luna"} {
		for _, effort := range []string{"", "minimal", "low", "medium", "high", "none", "NONE", " none "} {
			c := provider.NewOpenAIClientWithOptions(mcp.WithModel(model), mcp.WithReasoningEffort(effort), mcp.WithLogger(mcp.NewNoopLogger())).(mcp.ClientEmbedder).BaseClient()
			_, sent := c.Hooks.BuildMCPRequestBody("system", "user")["temperature"]
			if sent != (effort == "none") {
				t.Errorf("model=%q effort=%q temperature sent=%t", model, effort, sent)
			}
		}
	}
}

func TestTemperatureReview20261029WarnOnceAcrossClients(t *testing.T) {
	log := &temperatureReview20261029Logger{Logger: mcp.NewNoopLogger()}
	// Unique even with go test -count=N, since warning suppression is process-wide.
	model := fmt.Sprintf("gpt-5.4-review20261029-%d", time.Now().UnixNano())
	override := 0.2
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := provider.NewOpenAIClientWithOptions(mcp.WithModel(model), mcp.WithLogger(log)).(mcp.ClientEmbedder).BaseClient()
			for j := 0; j < 3; j++ {
				body := c.Hooks.BuildRequestBodyFromRequest(&mcp.Request{Model: model, Temperature: &override, ReasoningEffort: "high"})
				if _, sent := body["temperature"]; sent {
					t.Error("unsupported explicit temperature was sent")
				}
			}
		}()
	}
	wg.Wait()
	if len(log.warnings) != 1 || !strings.Contains(log.warnings[0], "provider=openai model="+model) {
		t.Fatalf("warnings=%v want one provider+model warning", log.warnings)
	}
	// Different provider+model pairs each retain their own first warning.
	for _, pair := range []struct{ provider, model string }{{mcp.ProviderOpenAI, model + "-other"}, {mcp.ProviderCustom, model}} {
		c := mcp.NewClient(mcp.WithProvider(pair.provider), mcp.WithBaseURL("https://api.openai.com/v1"), mcp.WithLogger(log)).(*mcp.Client)
		c.BuildRequestBodyFromRequest(&mcp.Request{Model: pair.model, Temperature: &override})
	}
	if len(log.warnings) != 3 {
		t.Fatalf("warnings=%v want three distinct provider+model pairs", log.warnings)
	}
}
