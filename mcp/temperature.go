package mcp

import (
	"net/url"
	"strings"
	"sync"
)

// First matching prefix wins; keep chat exceptions before reasoning families.
// Basis: OpenAI's GPT-5 parameter compatibility guidance:
// https://developers.openai.com/api/docs/guides/latest-model?model=gpt-5.4
// GPT-5.2/5.4 allow temperature only with reasoning effort "none"; older
// GPT-5 models reject it. Conservatively require an explicit "none" for all
// listed reasoning families, including unknown GPT-5 variants, rather than
// assuming a server default or claiming support for their other parameters.
var temperatureRules = []struct {
	prefix       string
	requiresNone bool
}{
	{"gpt-5-chat", false},
	{"o1", true},
	{"o3", true},
	{"o4", true},
	{"gpt-5", true},
}

func temperatureSupported(provider, model, reasoningEffort string) bool {
	if provider != ProviderOpenAI {
		return true
	}
	for _, rule := range temperatureRules {
		if strings.HasPrefix(model, rule.prefix) {
			return !rule.requiresNone || reasoningEffort == "none"
		}
	}
	return true
}

// SetAPIKey on the base Client labels OpenAI-compatible endpoints "custom".
// Inspect the actual endpoint too, using an exact hostname (not a substring).
func (client *Client) temperatureProvider() string {
	u, err := url.Parse(client.Hooks.BuildUrl())
	if err == nil && strings.EqualFold(u.Hostname(), "api.openai.com") {
		return ProviderOpenAI
	}
	return client.Provider
}

var unsupportedTemperatureWarnings sync.Map // process-wide provider + model pairs

// applyTemperature is shared by the simple, streaming and Request builders.
// An empty effort preserves the server default, which is not explicit "none".
func (client *Client) applyTemperature(body map[string]any, override *float64, effort string) {
	if effort != "" {
		body["reasoning_effort"] = effort
	}
	model, _ := body["model"].(string)
	if !temperatureSupported(client.temperatureProvider(), model, effort) {
		if override != nil {
			key := struct{ provider, model string }{client.Provider, model}
			if _, loaded := unsupportedTemperatureWarnings.LoadOrStore(key, struct{}{}); !loaded {
				client.Log.Warnf("LLM temperature override omitted: provider=%s model=%s reasoning_effort=%q does not support temperature",
					client.Provider, model, effort)
			}
		}
		return
	}
	temperature := client.Cfg.Temperature
	if override != nil {
		temperature = *override
	}
	body["temperature"] = temperature
}

// Log only the final sampling fields, after provider hooks build the body.
// In particular, Claude's omitted default must be reported as omitted.
func (client *Client) logTemperature(body map[string]any) {
	temperature, sent := body["temperature"]
	if !sent {
		temperature = "omitted"
	}
	client.Log.Debugf("LLM request parameters: provider=%s model=%v temperature_sent=%t temperature=%v",
		client.Provider, body["model"], sent, temperature)
}
