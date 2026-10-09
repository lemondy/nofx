package stockengine

import (
	"encoding/json"
	"fmt"
	"nofx/mcp"
	"strings"
	"time"
)

func validAction(action string) bool {
	switch action {
	case ActionOpenLong, ActionAddLong, ActionReduceLong, ActionCloseLong, ActionAdjustStop, ActionHold, ActionWait:
		return true
	}
	return false
}

// ParseDecisions selects the last array that decodes as []Decision. Scanning
// with json.Decoder handles brackets in strings, fences, and trailing prose.
func ParseDecisions(response string) ([]Decision, error) {
	var found []Decision
	ok := false
	for i := 0; i < len(response); i++ {
		if response[i] != '[' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(response[i:]))
		var candidate []Decision
		if err := decoder.Decode(&candidate); err != nil {
			continue
		}
		found, ok = candidate, true
		// Do not treat nested arrays inside this successfully decoded array as decisions.
		i += int(decoder.InputOffset()) - 1
	}
	if !ok {
		return nil, fmt.Errorf("no JSON decision array parses in response")
	}
	decisions := make([]Decision, 0, len(found))
	for _, d := range found {
		d.Symbol = strings.ToUpper(strings.TrimSpace(d.Symbol))
		d.Action = strings.ToLower(strings.TrimSpace(d.Action))
		d.EntryType = strings.ToLower(strings.TrimSpace(d.EntryType))
		if validAction(d.Action) {
			decisions = append(decisions, d)
		}
	}
	return decisions, nil
}

// Decide builds prompts, invokes the injected client, parses, and validates.
// Only the client call uses wall-clock timing; all trading time comes from ctx.Now.
func Decide(ctx *Context, client mcp.AIClient, lang string) (*Result, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil stock context")
	}
	r := &Result{SystemPrompt: BuildSystemPrompt(ctx.Config, contextPreset(ctx), lang), UserPrompt: BuildUserPrompt(ctx, lang)}
	if client == nil {
		return r, fmt.Errorf("nil AI client")
	}
	start := time.Now()
	response, err := client.CallWithMessages(r.SystemPrompt, r.UserPrompt)
	r.DurationMs = time.Since(start).Milliseconds()
	r.RawResponse = response
	if err != nil {
		return r, fmt.Errorf("AI call failed: %w", err)
	}
	r.Decisions, err = ParseDecisions(response)
	if err != nil {
		return r, err
	}
	r.Verdicts = Validate(ctx, r.Decisions)
	return r, nil
}
