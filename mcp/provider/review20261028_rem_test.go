package provider

import (
	"fmt"
	"strings"
	"testing"

	"nofx/mcp"
)

type remClaudeLogger struct {
	mcp.Logger
	warnings []string
}

func (l *remClaudeLogger) Warnf(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

// Claude's current call path is non-streaming. Check its native stop_reason
// without introducing a new streaming transport or changing returned content.
func TestReview20261028RemClaudeTruncationWarning(t *testing.T) {
	for _, stop := range []string{"max_tokens", "end_turn", "tool_use", ""} {
		t.Run(stop, func(t *testing.T) {
			log := &remClaudeLogger{Logger: mcp.NewNoopLogger()}
			c := NewClaudeClientWithOptions(mcp.WithModel("claude-test-model"), mcp.WithLogger(log)).(*ClaudeClient)
			body := fmt.Sprintf(`{"stop_reason":%q,"content":[{"type":"text","text":"部分"},{"type":"text","text":"内容"},{"type":"tool_use","id":"call-1","name":"lookup","input":{"symbol":"BTCUSDT"}}]}`, stop)
			result, err := c.ParseMCPResponseFull([]byte(body))
			if err != nil || result.Content != "内容" || len(result.ToolCalls) != 1 || result.ToolCalls[0].Function.Arguments != `{"symbol":"BTCUSDT"}` {
				t.Fatalf("parse semantics changed: result=%+v err=%v", result, err)
			}
			if stop != "max_tokens" {
				if len(log.warnings) != 0 {
					t.Fatalf("normal completion warned: %v", log.warnings)
				}
				return
			}
			if len(log.warnings) != 1 {
				t.Fatalf("want one warning, got %v", log.warnings)
			}
			for _, want := range []string{"claude-test-model", "Response TRUNCATED by max_tokens", "stop_reason=max_tokens", "4 chars received"} {
				if !strings.Contains(log.warnings[0], want) {
					t.Errorf("warning missing %q: %s", want, log.warnings[0])
				}
			}
		})
	}
}
