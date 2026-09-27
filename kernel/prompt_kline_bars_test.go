package kernel

import (
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

// ============================================================================
// Prompt-side OHLCV trim (user review 2026-09-27): 60 closed bars × 4 TFs
// ≈ 9.9k chars ≈ 63% of a full signal, while every traded field is already
// program-computed. The prompt keeps only the most recent N closed bars per
// timeframe (default 20) plus the newest closed bar's UTC timestamp; the
// indicators still run on the full fetched history.
// ============================================================================

func TestResolvePromptKlineBars(t *testing.T) {
	cases := []struct {
		promptBars, primaryCount, want int
	}{
		{0, 60, 20},   // unset → default 20
		{15, 60, 15},  // explicit
		{5, 60, 10},   // floor at 10
		{200, 60, 60}, // never above the fetched history
		{-1, 60, 60},  // negative = full history (legacy escape hatch)
		{-1, 30, 30},
	}
	for _, c := range cases {
		if got := ResolvePromptKlineBars(c.promptBars, c.primaryCount); got != c.want {
			t.Errorf("ResolvePromptKlineBars(%d, %d) = %d, want %d", c.promptBars, c.primaryCount, got, c.want)
		}
	}
}

func TestPromptKlinesTrimmedToRecentBars(t *testing.T) {
	now := time.Now()
	mkData := func() *market.Data {
		return &market.Data{
			Symbol: "TRIMUSDT", CurrentPrice: 1.06,
			TimeframeData: map[string]*market.TimeframeSeriesData{
				"15m": buildTF("15m", now, 60, 1.0, false),
				"1h":  buildTF("1h", now, 60, 1.0, false),
			},
		}
	}
	run := func(promptBars int) *SymbolSignal {
		cfg := store.GetDefaultStrategyConfig("zh")
		cfg.Indicators.EnableRawKlines = true
		cfg.Indicators.Klines.PrimaryCount = 60
		cfg.Indicators.Klines.PrimaryTimeframe = "15m"
		cfg.Indicators.Klines.SelectedTimeframes = []string{"15m", "1h"}
		cfg.Indicators.Klines.PromptKlineBars = promptBars
		e := NewStrategyEngine(&cfg)
		ctx := &Context{MarketDataMap: map[string]*market.Data{}}
		ctx.CandidateCoins = append(ctx.CandidateCoins, CandidateCoin{Symbol: "TRIMUSDT"})
		ctx.MarketDataMap["TRIMUSDT"] = mkData()
		sig := e.computeCoinSignal(ctx.MarketDataMap["TRIMUSDT"], nil, ctx, &ctx.CandidateCoins[0])
		if sig == nil {
			t.Fatal("computeCoinSignal returned nil")
		}
		return sig
	}

	// Default: 60 fetched bars → the newest 20 closed bars, newest retained.
	sig := run(0)
	got := sig.OHLCV["15m"]
	if len(got) != 20 {
		t.Fatalf("15m prompt bars = %d, want 20 (default trim)", len(got))
	}
	if got[len(got)-1][3] <= got[0][3] {
		t.Fatalf("kept slice must be the NEWEST bars (mild uptrend), first close %.4f last close %.4f", got[0][3], got[len(got)-1][3])
	}
	if sig.OHLCVLastClosed["15m"] == "" || !strings.HasSuffix(sig.OHLCVLastClosed["15m"], "Z") {
		t.Fatalf("missing last-closed UTC stamp for 15m: %v", sig.OHLCVLastClosed)
	}
	if sig.OHLCVLastClosed["1h"] == "" {
		t.Fatalf("missing last-closed UTC stamp for 1h: %v", sig.OHLCVLastClosed)
	}

	// Explicit 15.
	if got := run(15).OHLCV["15m"]; len(got) != 15 {
		t.Fatalf("explicit 15 → %d bars, want 15", len(got))
	}

	// Negative = full history (legacy).
	full := run(-1).OHLCV["15m"]
	if len(full) <= 20 {
		t.Fatalf("negative config must keep the full fetched history, got %d", len(full))
	}
}

// The stamp renders with the trimmed series so the model can judge freshness.
func TestRenderSignalOHLCVLastClosed(t *testing.T) {
	sig := &SymbolSignal{
		Symbol: "TESTUSDT", DataComplete: true, Price: 100,
		OHLCV:           map[string][][5]float64{"15m": {{100, 101, 99, 100.5, 1234}}},
		OHLCVLastClosed: map[string]string{"15m": "2026-09-27T06:45:00Z"},
	}
	out := RenderSignalJSON(sig)
	if !strings.Contains(out, `"ohlcv_last_closed_utc"`) || !strings.Contains(out, "2026-09-27T06:45:00Z") {
		t.Fatalf("last-closed stamp missing from the rendered signal:\n%s", out)
	}
}
