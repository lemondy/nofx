package mcp

import (
	"net/http"
	"os"
	"strconv"
	"time"

	"nofx/logger"
	"nofx/security"
)

// Config client configuration (centralized management of all configurations)
type Config struct {
	// Provider configuration
	Provider string
	APIKey   string
	BaseURL  string
	Model    string

	// Behavior configuration
	// StreamDecisions: use SSE streaming for AI calls (OpenAI-compatible
	// providers). Headers return immediately, so long reasoning generations no
	// longer hit the "awaiting headers" timeout.
	StreamDecisions bool
	MaxTokens       int
	MaxContext      int // Model's max context window in tokens (0 = no limit)
	Temperature     float64
	UseFullURL      bool

	// Retry configuration
	MaxRetries      int
	RetryWaitBase   time.Duration
	RetryableErrors []string

	// Timeout configuration
	Timeout time.Duration

	// Dependency injection
	Logger     Logger
	HTTPClient *http.Client
}

// DefaultConfig returns default configuration
func DefaultConfig() *Config {
	return &Config{
		// Default values
		// 16384: reasoning models (DeepSeek-R1, GLM thinking, ...) spend
		// thousands of tokens thinking before writing the JSON decision — an
		// 8192 cap truncated full CoT+JSON responses mid-sentence (the CoT
		// alone can run 6-10K tokens with multi-timeframe signals), leaving
		// the final JSON unparseable.
		StreamDecisions: getEnvInt("AI_STREAM_DECISIONS", 1) != 0,
		MaxTokens:       getEnvInt("AI_MAX_TOKENS", 16384),
		Temperature:     MCPClientTemperature,
		MaxRetries:      MaxRetryTimes,
		RetryWaitBase:   2 * time.Second,
		Timeout:         DefaultTimeout,
		RetryableErrors: retryableErrors,

		// Default dependencies (use global logger)
		Logger:     logger.NewMCPLogger(),
		HTTPClient: security.SafeHTTPClient(DefaultTimeout),
	}
}

// getEnvInt reads integer from environment variable, returns default value if failed
func getEnvInt(key string, defaultValue int) int {
	if val := os.Getenv(key); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultValue
}

// getEnvString reads string from environment variable, returns default value if empty
func getEnvString(key string, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}
