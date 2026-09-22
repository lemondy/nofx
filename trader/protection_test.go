package trader

import (
	"math"
	"testing"

	"nofx/store"
	"nofx/trader/types"
)

// Round-4 review R4-14: the naked-position watchdog's core math, extracted
// as computedProtectionLevels, gets the coverage the surrounding watchdog
// flow never had. SL = mark ∓ 1.5×ATR, TP = mark ∓ 2× that distance (1:2 RR).
func TestComputedProtectionLevels(t *testing.T) {
	sl, tp, ok := computedProtectionLevels("long", 100, 2)
	if !ok || math.Abs(sl-97) > 1e-9 || math.Abs(tp-106) > 1e-9 {
		t.Fatalf("long: got sl=%v tp=%v ok=%v, want 97/106/true", sl, tp, ok)
	}
	// TP distance must be exactly twice the SL distance (1:2 RR).
	if math.Abs((tp-100)-2*(100-sl)) > 1e-9 {
		t.Fatalf("long RR not 1:2: slDist=%v tpDist=%v", 100-sl, tp-100)
	}

	sl, tp, ok = computedProtectionLevels("short", 100, 2)
	if !ok || math.Abs(sl-103) > 1e-9 || math.Abs(tp-94) > 1e-9 {
		t.Fatalf("short: got sl=%v tp=%v ok=%v, want 103/94/true", sl, tp, ok)
	}
	if math.Abs((100-tp)-2*(sl-100)) > 1e-9 {
		t.Fatalf("short RR not 1:2: slDist=%v tpDist=%v", sl-100, 100-tp)
	}

	for _, tc := range []struct {
		side                 string
		mark, atr            float64
		name                 string
	}{
		{"long", 0, 2, "zero mark"},
		{"long", 100, 0, "zero atr"},
		{"long", 100, -1, "negative atr"},
		{"sideways", 100, 2, "unknown side"},
		{"", 100, 2, "empty side"},
	} {
		if _, _, ok := computedProtectionLevels(tc.side, tc.mark, tc.atr); ok {
			t.Fatalf("%s must be rejected, got ok=true", tc.name)
		}
	}
}

// Round-4 review R4-7: the TP-runner flag shares the position lifecycle —
// without the delete, a re-opened symbol_side inherits "runner done" and
// never converts to the trend-run again.
func TestClearPeakPnLCacheClearsTPRunner(t *testing.T) {
	at := &AutoTrader{
		tpRunnerDoneMap: map[string]bool{"BTCUSDT_long": true},
		peakPnLCache:    map[string]float64{"BTCUSDT_long": 3.2},
		tpTrimDone:      map[string]bool{"BTCUSDT_long": true},
		r1TrimDone:      map[string]bool{"BTCUSDT_long": true},
		partialTrimmed:  map[string]float64{"BTCUSDT_long": 0.25},
	}
	if !at.tpRunnerDone("BTCUSDT_long") {
		t.Fatal("precondition: runner flag set")
	}
	at.ClearPeakPnLCache("BTCUSDT", "long")
	if at.tpRunnerDone("BTCUSDT_long") {
		t.Fatal("tpRunnerDoneMap must be cleared with the position lifecycle")
	}
	if at.peakPnLCache["BTCUSDT_long"] != 0 || at.tpTrimDone["BTCUSDT_long"] || at.r1TrimDone["BTCUSDT_long"] || at.partialTrimmed["BTCUSDT_long"] != 0 {
		t.Fatal("sibling lifecycle maps must also be cleared")
	}
}

// 09-21 split-TP experiment: the fraction only applies when the trailing
// stop can ratchet the runner out — without it the split collapses to a
// full close (a runner without a ratchet gives the move back).
func TestEffectiveTPCloseFraction(t *testing.T) {
	cfg := &store.StrategyConfig{}
	at := &AutoTrader{config: AutoTraderConfig{StrategyConfig: cfg}}

	cfg.RiskControl.TrailingStopEnabled = false
	cfg.RiskControl.TPCloseFraction = 0.5
	if got := at.effectiveTPCloseFraction(); got != 1.0 {
		t.Fatalf("trailing off must collapse to full close, got %v", got)
	}

	cfg.RiskControl.TrailingStopEnabled = true
	if got := at.effectiveTPCloseFraction(); got != 0.5 {
		t.Fatalf("trailing on + default fraction must be 0.5, got %v", got)
	}

	cfg.RiskControl.TPCloseFraction = -1
	if got := at.effectiveTPCloseFraction(); got != 1.0 {
		t.Fatalf("negative fraction = legacy full close, got %v", got)
	}
}

// 09-22 mandatory post-open leg verification: the report must name exactly
// the INTENDED legs that are missing — a resting limit entry is noise (the
// watchdog's matching rule), and unwanted legs are never demanded.
func TestMissingLegsReport(t *testing.T) {
	sl := types.OpenOrder{Symbol: "X", Type: "STOP_MARKET", StopPrice: 0.0471}
	tp := types.OpenOrder{Symbol: "X", Type: "TAKE_PROFIT_MARKET", StopPrice: 0.0885}
	limit := types.OpenOrder{Symbol: "X", Type: "LIMIT", Price: 0.05}

	if got := missingLegsReport([]types.OpenOrder{sl, tp}, true, true); got != "" {
		t.Fatalf("both legs resting: got %q, want empty", got)
	}
	if got := missingLegsReport([]types.OpenOrder{sl, limit}, true, true); got != "TP" {
		t.Fatalf("TP missing: got %q, want TP (limit entry must not count)", got)
	}
	if got := missingLegsReport([]types.OpenOrder{limit}, true, true); got != "SL+TP" {
		t.Fatalf("both missing: got %q, want SL+TP", got)
	}
	// SL-only position (TP not intended): a missing TP must NOT be reported.
	if got := missingLegsReport([]types.OpenOrder{sl}, true, false); got != "" {
		t.Fatalf("TP not wanted: got %q, want empty", got)
	}
}
