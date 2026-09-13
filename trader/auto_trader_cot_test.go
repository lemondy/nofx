package trader

import (
	"strings"
	"testing"

	"nofx/kernel"
	"nofx/store"
)

// Only exchange-touching actions justify a CoT push; holds/waits don't.
func TestCotHasActionable(t *testing.T) {
	if cotHasActionable([]kernel.Decision{{Action: "wait"}, {Action: "hold"}}) {
		t.Error("wait/hold cycle must not be actionable")
	}
	if !cotHasActionable([]kernel.Decision{{Action: "wait"}, {Action: "open_long_limit"}}) {
		t.Error("limit open must count as actionable")
	}
	if !cotHasActionable([]kernel.Decision{{Action: "close_short"}}) {
		t.Error("close must count as actionable")
	}
}

// Outcome mapping: executed → ✓/✗, filtered actionable → "被闸门过滤",
// passive stays markerless.
func TestBuildCoTSummaries(t *testing.T) {
	proposed := []kernel.Decision{
		{Symbol: "WLDUSDT", Action: "open_long_limit", Price: 0.4599, PositionSizeUSD: 50, StopLoss: 0.44, TakeProfit: 0.49, Confidence: 85},
		{Symbol: "ICPUSDT", Action: "open_short", Confidence: 80}, // gate-filtered, never executed
		{Symbol: "SUIUSDT", Action: "wait"},
	}
	executed := []store.DecisionAction{
		{Symbol: "WLDUSDT", Action: "open_long_limit", Success: true},
	}
	summaries := buildCoTSummaries(proposed, executed)
	if len(summaries) != 3 {
		t.Fatalf("want 3 summaries, got %d", len(summaries))
	}
	if !summaries[0].OK || !strings.Contains(summaries[0].Detail, "@0.4599") || !strings.Contains(summaries[0].Detail, "置信85") {
		t.Errorf("executed summary wrong: %+v", summaries[0])
	}
	if summaries[1].ErrText == "" || !strings.Contains(summaries[1].ErrText, "闸门") {
		t.Errorf("filtered summary must carry the filter note: %+v", summaries[1])
	}
	if summaries[2].OK || summaries[2].ErrText != "" {
		t.Errorf("passive summary must stay plain: %+v", summaries[2])
	}
}

// Execution failure surfaces the stored error text.
func TestBuildCoTSummariesFailure(t *testing.T) {
	proposed := []kernel.Decision{{Symbol: "XAUUSDT", Action: "close_long"}}
	executed := []store.DecisionAction{{Symbol: "XAUUSDT", Action: "close_long", Success: false, Error: "binance reject"}}
	s := buildCoTSummaries(proposed, executed)
	if s[0].OK || !strings.Contains(s[0].ErrText, "binance reject") {
		t.Errorf("failure must surface the error: %+v", s[0])
	}
}

func TestCotClamp(t *testing.T) {
	if got := cotClamp("short", 10); got != "short" {
		t.Errorf("short string must pass through: %q", got)
	}
	got := cotClamp(strings.Repeat("字", 50), 10)
	if utf8ClampCheck(got, 10) {
		t.Errorf("clamped string over cap: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clamped string must end with ellipsis: %q", got)
	}
}

func utf8ClampCheck(s string, max int) bool {
	return len([]rune(s)) > max
}
