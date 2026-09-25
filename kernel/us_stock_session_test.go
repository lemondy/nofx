package kernel

import (
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

// Pins for the US-session equity weighting (user directive 2026-09-23):
// during US regular hours, US equity tokens' short scores are multiplied
// before the top-N cut; off-session, disabled config, and crypto symbols are
// untouched.
func TestApplyUSStockSessionBoost(t *testing.T) {
	// Inject the classification (market package globals are inaccessible
	// from kernel — the boost reads through market.IsUSEquitySymbol, so run
	// the session assertion against the live classifier with synthetic
	// candidates for a symbol we KNOW classifies as US equity: AAPLUSDT).
	marketOpen := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC) // 11:00 ET Wed
	marketClosed := time.Date(2026, 9, 23, 20, 0, 0, 0, time.UTC) // 16:00+ ET Wed

	// Pin the classification explicitly — the test must not depend on live
	// exchangeInfo (offline sandboxes made AAPLUSDT classify non-equity and
	// the boost silently became a no-op).
	market.SetEquityClassificationForTesting(
		map[string]bool{"AAPLUSDT": true},
		map[string]bool{"AAPLUSDT": true},
	)
	defer market.SetEquityClassificationForTesting(nil, nil)

	if !market.IsUSMarketOpen(marketOpen) || market.IsUSMarketOpen(marketClosed) {
		t.Fatalf("session fixture broken: open=%v closed=%v", market.IsUSMarketOpen(marketOpen), market.IsUSMarketOpen(marketClosed))
	}

	e := &StrategyEngine{config: &store.StrategyConfig{USStockSessionBoostPct: 50}}
	mk := func() []CandidateCoin {
		return []CandidateCoin{
			{Symbol: "AAPLUSDT", ShortScore: 60}, // US equity token (real classifier)
			{Symbol: "BTCUSDT", ShortScore: 60},  // crypto
		}
	}

	// Off-session: nothing changes.
	cands := mk()
	e.applyUSStockSessionBoost(cands, marketClosed)
	if cands[0].ShortScore != 60 || len(cands[0].ShortReasons) != 0 {
		t.Fatalf("off-session must be a no-op, got score %.1f reasons %v", cands[0].ShortScore, cands[0].ShortReasons)
	}

	// In-session: equity score ×1.5 with a visible reason; crypto untouched.
	cands = mk()
	e.applyUSStockSessionBoost(cands, marketOpen)
	if cands[0].ShortScore != 90 {
		t.Fatalf("boosted equity score = %.1f, want 90", cands[0].ShortScore)
	}
	if len(cands[0].ShortReasons) == 0 {
		t.Fatal("boosted candidate must carry the session-weight reason")
	}
	if cands[1].ShortScore != 60 || len(cands[1].ShortReasons) != 0 {
		t.Fatalf("crypto must be untouched, got %.1f", cands[1].ShortScore)
	}

	// Score caps at 100.
	cands = mk()
	cands[0].ShortScore = 80
	e.applyUSStockSessionBoost(cands, marketOpen)
	if cands[0].ShortScore != 100 {
		t.Fatalf("capped score = %.1f, want 100", cands[0].ShortScore)
	}

	// Disabled (negative) → no-op even in session.
	eOff := &StrategyEngine{config: &store.StrategyConfig{USStockSessionBoostPct: -1}}
	cands = mk()
	eOff.applyUSStockSessionBoost(cands, marketOpen)
	if cands[0].ShortScore != 60 {
		t.Fatalf("disabled boost must be a no-op, got %.1f", cands[0].ShortScore)
	}

	// Config semantics: unset → default 20, negative → 0.
	if got := (&store.StrategyConfig{}).EffectiveUSStockSessionBoostPct(); got != store.DefaultUSStockSessionBoostPct {
		t.Fatalf("unset boost = %.0f, want default %.0f", got, store.DefaultUSStockSessionBoostPct)
	}
	if got := (&store.StrategyConfig{USStockSessionBoostPct: -5}).EffectiveUSStockSessionBoostPct(); got != 0 {
		t.Fatalf("negative boost = %.0f, want 0 (disabled)", got)
	}
}
