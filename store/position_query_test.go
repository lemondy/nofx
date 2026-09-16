package store

import (
	"math"
	"testing"
	"time"
)

// The max drawdown must be measured against the trader's REAL capital base.
// Regression: a hardcoded 10000 base shrank a ~9% account drawdown to 0.2%.
func TestMaxDrawdownUsesRealCapitalBase(t *testing.T) {
	// 82 USDT account: +2, then -5, then +1 → equity 82→84→79→80.
	pnls := []float64{2, -5, 1}
	got := calculateMaxDrawdownFromPnls(pnls, 82)
	want := (84.0 - 79.0) / 84.0 * 100 // 5.95%
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("max drawdown = %.4f%%, want %.4f%%", got, want)
	}

	// Unknown base falls back to 100 USDT (not the old 10k).
	got = calculateMaxDrawdownFromPnls(pnls, 0)
	want = (102.0 - 97.0) / 102.0 * 100 // 4.90%
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("fallback drawdown = %.4f%%, want %.4f%%", got, want)
	}

	// A >100% relative drawdown can never happen against a positive base.
	got = calculateMaxDrawdownFromPnls([]float64{-1}, 100)
	if got > 100 || got < 0 {
		t.Fatalf("drawdown out of range: %.2f%%", got)
	}
}

// Rolling stats window: only trades closed within the last N days feed the
// numbers, so ancient losses stop dragging PF down forever. The window must
// stay a query-time filter — no row is ever deleted or moved.
func TestGetRollingStatsWindow(t *testing.T) {
	st := newTestStore(t)
	if err := st.Position().InitTables(); err != nil {
		t.Fatalf("init: %v", err)
	}
	day := func(daysAgo int) int64 {
		return time.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour).UnixMilli()
	}
	rows := []TraderPosition{
		{TraderID: "T1", Symbol: "AINUSDT", Status: "CLOSED", RealizedPnL: 3, EntryTime: day(3), ExitTime: day(2)},
		{TraderID: "T1", Symbol: "BRUSDT", Status: "CLOSED", RealizedPnL: -1, EntryTime: day(11), ExitTime: day(10)},
		{TraderID: "T1", Symbol: "OLDUSDT", Status: "CLOSED", RealizedPnL: -20, EntryTime: day(46), ExitTime: day(45)},
		{TraderID: "T1", Symbol: "OPENUSDT", Status: "OPEN", RealizedPnL: 0, EntryTime: day(1), ExitTime: 0},
	}
	for _, r := range rows {
		if err := st.Position().db.Create(&r).Error; err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	full, err := st.Position().GetFullStats("T1", 100)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if full.TotalTrades != 3 || full.WindowDays != 0 {
		t.Fatalf("full stats = %+v, want 3 closed trades, window 0", full)
	}
	if full.ProfitFactor != 3.0/21.0 {
		t.Fatalf("full PF = %.4f, want %.4f", full.ProfitFactor, 3.0/21.0)
	}
	if full.TotalWin != 3 || full.TotalLoss != 21 {
		t.Fatalf("full totals = win %.2f / loss %.2f, want 3 / 21", full.TotalWin, full.TotalLoss)
	}

	rolling, err := st.Position().GetRollingStats("T1", 100, 30)
	if err != nil {
		t.Fatalf("rolling: %v", err)
	}
	if rolling.TotalTrades != 2 || rolling.WindowDays != 30 {
		t.Fatalf("rolling stats = %+v, want 2 trades, window 30", rolling)
	}
	if rolling.ProfitFactor != 3.0 {
		t.Fatalf("rolling PF = %.2f, want 3.00 — the 45-day-old loss must stay out of the window", rolling.ProfitFactor)
	}

	// Negative window = full history (window disabled).
	neg, err := st.Position().GetRollingStats("T1", 100, -1)
	if err != nil {
		t.Fatalf("negative window: %v", err)
	}
	if neg.TotalTrades != 3 {
		t.Fatalf("negative window = %d trades, want full history (3)", neg.TotalTrades)
	}
}

// Config semantics for stats_window_days: unset → 30, negative → disabled
// (full history), positive → used as-is.
func TestEffectiveStatsWindowDays(t *testing.T) {
	cases := []struct {
		cfg  int
		want int
	}{{0, 30}, {-5, 0}, {7, 7}, {90, 90}}
	for _, c := range cases {
		cfg := StrategyConfig{StatsWindowDays: c.cfg}
		if got := cfg.EffectiveStatsWindowDays(); got != c.want {
			t.Fatalf("StatsWindowDays=%d resolved to %d, want %d", c.cfg, got, c.want)
		}
	}
}
