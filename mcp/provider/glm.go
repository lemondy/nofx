package provider

import (
	"net/http"

	"nofx/mcp"
)

const (
	DefaultGLMBaseURL = "https://open.bigmodel.cn/api/paas/v4" // Zhipu BigModel (use https://api.z.ai/api/paas/v4 for international)
	DefaultGLMModel   = "glm-5"
)

func init() {
	mcp.RegisterProvider(mcp.ProviderGLM, func(opts ...mcp.ClientOption) mcp.AIClient {
		return NewGLMClientWithOptions(opts...)
	})
}

type GLMClient struct {
	*mcp.Client
}

func (c *GLMClient) BaseClient() *mcp.Client { return c.Client }

// NewGLMClient creates GLM (Zhipu) client (backward compatible)
func NewGLMClient() mcp.AIClient {
	return NewGLMClientWithOptions()
}

// NewGLMClientWithOptions creates GLM (Zhipu) client (supports options pattern)
func NewGLMClientWithOptions(opts ...mcp.ClientOption) mcp.AIClient {
	glmOpts := []mcp.ClientOption{
		mcp.WithProvider(mcp.ProviderGLM),
		mcp.WithModel(DefaultGLMModel),
		mcp.WithBaseURL(DefaultGLMBaseURL),
	}

	allOpts := append(glmOpts, opts...)
	baseClient := mcp.NewClient(allOpts...).(*mcp.Client)

	glmClient := &GLMClient{
		Client: baseClient,
	}

	baseClient.Hooks = glmClient
	return glmClient
}

func (c *GLMClient) SetAPIKey(apiKey string, customURL string, customModel string) {
	c.APIKey = apiKey

	if len(apiKey) > 8 {
		c.Log.Infof("🔧 [MCP] GLM API Key: %s...%s", apiKey[:4], apiKey[len(apiKey)-4:])
	}
	if customURL != "" {
		c.BaseURL = customURL
		c.Log.Infof("🔧 [MCP] GLM using custom BaseURL: %s", customURL)
	} else {
		c.Log.Infof("🔧 [MCP] GLM using default BaseURL: %s", c.BaseURL)
	}
	if customModel != "" {
		c.Model = customModel
		c.Log.Infof("🔧 [MCP] GLM using custom Model: %s", customModel)
	} else {
		c.Log.Infof("🔧 [MCP] GLM using default Model: %s", c.Model)
	}
}

// GLM uses standard OpenAI-compatible API with Bearer auth
func (c *GLMClient) SetAuthHeader(reqHeaders http.Header) {
	c.Client.SetAuthHeader(reqHeaders)
}
