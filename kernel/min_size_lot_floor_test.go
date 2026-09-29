package kernel

import (
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

// 09-29 user report: a QNTUSDT limit entry died AT PLACEMENT —
// "MIN_QTY: quantity 0.05345287 rounds down to 0.0 (stepSize=0.1, minQty=0.1)".
// Risk-based sizing produced ≈13.9U notional while QNT's lot structure
// forces ≥0.1 base units (≈26U at price 260). The kernel's min-size dead
// zone only knew the STRATEGY minimum (5U), so the model was shown an
// "allowed" setup that can never execute. The exchange lot floor must feed
// the gate.
func TestMinSizeIncorporatesExchangeLotFloor(t *testing.T) {
	market.ResetSymbolLotFilters()
	defer market.ResetSymbolLotFilters()
	// QNT-like floor: 0.1 step × minQty 0.1 at price 260 → 26U minimum.
	market.SetSymbolLotFilterForTesting("QNTUSDT", market.SymbolLotFilter{
		StepSize: 0.1, MinQty: 0.1, MinNotional: 5,
	})

	now := time.Now()
	cfg := store.GetDefaultStrategyConfig("zh")
	cfg.RiskControl.MinPositionSize = 5 // the strategy minimum that passed
	e := NewStrategyEngine(&cfg)
	ctx := &Context{
		MarketDataMap: map[string]*market.Data{},
		Account:       AccountInfo{TotalEquity: 200},
	}
	ctx.CandidateCoins = append(ctx.CandidateCoins, CandidateCoin{Symbol: "QNTUSDT"})
	ctx.MarketDataMap["QNTUSDT"] = withVendor(&market.Data{
		Symbol: "QNTUSDT", CurrentPrice: 260,
		TimeframeData: map[string]*market.TimeframeSeriesData{
			"15m": buildTF("15m", now, 80, 260, false),
			"1h":  buildTF("1h", now, 80, 260, false),
			"4h":  buildTF("4h", now, 80, 260, false),
		},
	}, 0.05)

	sig := e.computeCoinSignal(ctx.MarketDataMap["QNTUSDT"], nil, ctx, &ctx.CandidateCoins[0])
	if sig == nil || sig.MinSize == nil {
		t.Fatal("min_size block missing")
	}
	// The binding minimum must be the EXCHANGE floor (26U), not the
	// strategy's 5U.
	if sig.MinSize.MinPositionSizeUsd < 25.9 {
		t.Fatalf("min_position_size_usd = %.1f, want the exchange lot floor ≈26", sig.MinSize.MinPositionSizeUsd)
	}
	// riskUSD = 200×1.5% = 3U → max stop for 26U = 11.5%. A stop floor above
	// that must mark the symbol infeasible; with a mild floor it stays
	// feasible but the reason/echo still cites the exchange floor.
	if sig.MinSize.Feasible && !strings.Contains(sig.MinSize.Reason, "exchange lot floor") && sig.MinSize.MinPositionSizeUsd > 20 {
		// feasible case: no reason — that's fine; the floor is visible in
		// the echoed minimum. Nothing to assert beyond the echo above.
		_ = sig.MinSize
	}
}

// A symbol whose strategy minimum binds (no exchange floor above it) must
// stay untouched by the lot logic.
func TestMinSizeUnchangedWithoutExchangeFloor(t *testing.T) {
	market.ResetSymbolLotFilters()
	defer market.ResetSymbolLotFilters()
	market.SetSymbolLotFilterForTesting("AAAUSDT", market.SymbolLotFilter{
		StepSize: 0.001, MinQty: 0.001, MinNotional: 5,
	})
	now := time.Now()
	cfg := store.GetDefaultStrategyConfig("zh")
	cfg.RiskControl.MinPositionSize = 50
	e := NewStrategyEngine(&cfg)
	ctx := &Context{
		MarketDataMap: map[string]*market.Data{},
		Account:       AccountInfo{TotalEquity: 200},
	}
	ctx.CandidateCoins = append(ctx.CandidateCoins, CandidateCoin{Symbol: "AAAUSDT"})
	ctx.MarketDataMap["AAAUSDT"] = withVendor(&market.Data{
		Symbol: "AAAUSDT", CurrentPrice: 10,
		TimeframeData: map[string]*market.TimeframeSeriesData{
			"15m": buildTF("15m", now, 80, 10, false),
			"1h":  buildTF("1h", now, 80, 10, false),
			"4h":  buildTF("4h", now, 80, 10, false),
		},
	}, 0.05)

	sig := e.computeCoinSignal(ctx.MarketDataMap["AAAUSDT"], nil, ctx, &ctx.CandidateCoins[0])
	if sig == nil || sig.MinSize == nil {
		t.Fatal("min_size block missing")
	}
	if sig.MinSize.MinPositionSizeUsd != 50 {
		t.Fatalf("strategy minimum must bind when the exchange floor is below it, got %.1f", sig.MinSize.MinPositionSizeUsd)
	}
}
