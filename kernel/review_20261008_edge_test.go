package kernel

import (
	"strings"
	"testing"

	"nofx/market"
	"nofx/store"
)

// review 2026-10-08 A: an all-win window left store ProfitFactor at 0 (no
// loser to divide by) and the kernel read it as NEGATIVE_EDGE — the best
// window armed the worst-case gate and printed "需改进".

func TestResolveStrategyEdgeNoLosingTrade(t *testing.T) {
	cases := []struct {
		name string
		st   *TradingStats
		want string
	}{
		{"all-win", &TradingStats{TotalTrades: 3, ProfitFactor: 0, AvgLoss: 0, AvgWin: 2, TotalPnL: 6}, "POSITIVE_EDGE"},
		{"all-breakeven", &TradingStats{TotalTrades: 3}, "NO_EDGE"},
		{"real-loser", &TradingStats{TotalTrades: 20, ProfitFactor: 0.5, AvgLoss: 1, AvgWin: 1, TotalPnL: -2}, "NEGATIVE_EDGE"},
		{"nil", nil, "POSITIVE_EDGE"},
		{"zero-trades", &TradingStats{}, "POSITIVE_EDGE"},
	}
	for _, c := range cases {
		if got := ResolveStrategyEdge(c.st); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestAllWinWindowDoesNotArmNegativeEdgeGate(t *testing.T) {
	cfg := store.GetDefaultStrategyConfig("zh")
	e := NewStrategyEngine(&cfg)
	ctx := &Context{TradingStats: &TradingStats{TotalTrades: 3, AvgWin: 2, TotalPnL: 6}}
	if e.strategyEdge(ctx) == "NEGATIVE_EDGE" {
		t.Fatal("all-win window must not resolve to NEGATIVE_EDGE (gate input)")
	}
	ctx.TradingStats = &TradingStats{TotalTrades: 20, ProfitFactor: 0.5, AvgWin: 1, AvgLoss: 1, TotalPnL: -2}
	if e.strategyEdge(ctx) != "NEGATIVE_EDGE" {
		t.Fatal("real losing window must still arm the gate")
	}
}

func TestAllWinStatsBlockRendering(t *testing.T) {
	ctx := &Context{
		TradingStats:  &TradingStats{TotalTrades: 3, WinRate: 100, AvgWin: 2, TotalPnL: 6, WindowDays: 30},
		MarketDataMap: map[string]*market.Data{},
	}
	for _, lang := range []string{"zh", "en"} {
		cfg := store.GetDefaultStrategyConfig(lang)
		out := NewStrategyEngine(&cfg).BuildUserPrompt(ctx)
		for _, bad := range []string{"NEGATIVE_EDGE", "需改进", "NEEDS IMPROVEMENT"} {
			if strings.Contains(out, bad) {
				t.Fatalf("%s: all-win block must not contain %q: %s", lang, bad, out)
			}
		}
		if !strings.Contains(out, "n/a") || !strings.Contains(out, "strategy_health: POSITIVE_EDGE") {
			t.Fatalf("%s: missing n/a PF marker or POSITIVE_EDGE: %s", lang, out)
		}
	}
}

// User decision 2026-10-08: fewer than MinNegativeEdgeTrades closed trades
// never classify NEGATIVE_EDGE.
func TestNegativeEdgeNeedsMinSample(t *testing.T) {
	mk := func(n int) *Context {
		return &Context{
			TradingStats:  &TradingStats{TotalTrades: n, WinRate: 30, ProfitFactor: 0.5, AvgWin: 1, AvgLoss: 1, TotalPnL: -2, WindowDays: 30},
			MarketDataMap: map[string]*market.Data{},
		}
	}
	cfg := store.GetDefaultStrategyConfig("zh")
	e := NewStrategyEngine(&cfg)
	if got := e.strategyEdge(mk(MinNegativeEdgeTrades - 1)); got != "NO_EDGE" {
		t.Fatalf("19 trades: got %s want NO_EDGE", got)
	}
	if out := e.BuildUserPrompt(mk(MinNegativeEdgeTrades - 1)); !strings.Contains(out, "样本不足") || strings.Contains(out, "⚠️ 当前策略整体无正期望") {
		t.Fatalf("thin-sample marker missing or NEGATIVE_EDGE prose present: %s", out)
	}
	if got := e.strategyEdge(mk(MinNegativeEdgeTrades)); got != "NEGATIVE_EDGE" {
		t.Fatalf("20 trades: got %s want NEGATIVE_EDGE", got)
	}
	if out := e.BuildUserPrompt(mk(MinNegativeEdgeTrades)); strings.Contains(out, "样本不足") {
		t.Fatalf("20 trades must not carry thin marker")
	}
}
