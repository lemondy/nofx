package kernel

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

// Regression tests for the 09-18 prompt-audit batch:
//   - #6 structure staleness: structure_high/low_dist_pct vs the LIVE price,
//     and S/R arrays that were filtered away by the live price marshal as []
//     (never vanish from the JSON);
//   - vendor-vs-live divergence hard gate (VENDOR_DIVERGENCE blocker);
//   - #1 position line quoting PnL figures from the same live price it shows.

func TestStructureDistFlagsBreachedStructure(t *testing.T) {
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)
	// Series tops out ~105.9; the live price (130) has cleared every high in
	// the window — the BTC 4h shape from the audit (structure_high 79570.9
	// vs live 80852.5).
	data := gateTestMarket(now, 100.0)
	data.CurrentPrice = 130.0
	sig, err := ComputeSymbolSignals("BTCUSDT", data, SignalOptions{Now: now, PrimaryTF: "1h"})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	tf := sig.Timeframes["1h"]
	if tf == nil {
		t.Fatal("1h block missing")
	}
	if tf.StructureHighDistPct == nil || tf.StructureLowDistPct == nil {
		t.Fatal("structure dist fields missing")
	}
	if *tf.StructureHighDistPct >= 0 {
		t.Errorf("structure_high_dist_pct = %.2f, want negative (live price above window high)", *tf.StructureHighDistPct)
	}
	if *tf.StructureLowDistPct >= 0 {
		t.Errorf("structure_low_dist_pct = %.2f, want negative (window low below live price — the normal state)", *tf.StructureLowDistPct)
	}
	// Every pivot high is below the live price → resistance filtered away —
	// but the key must still be there as an explicit [].
	if len(tf.Resistance) != 0 {
		t.Fatalf("resistance should be empty above every swing high, got %v", tf.Resistance)
	}
	if tf.Resistance == nil || tf.ResistanceDistPct == nil || tf.Support == nil || tf.SupportDistPct == nil {
		t.Fatalf("S/R arrays must be non-nil (marshal as [], not vanish): %+v", tf)
	}
	j, err := json.Marshal(tf)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"resistance":[]`, `"support_dist_pct":[`} {
		if !strings.Contains(string(j), key) {
			t.Errorf("marshaled TF JSON missing %s — empty side vanished again\n%s", key, j)
		}
	}
}

func TestVendorDivergenceGate(t *testing.T) {
	now := time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)

	compute := func(div *float64, threshold float64) *HardEntryGate {
		data := gateTestMarket(now, 0.08515)
		sig, err := ComputeSymbolSignals("MYXUSDT", data, SignalOptions{Now: now, PrimaryTF: "1h"})
		if err != nil {
			t.Fatalf("compute: %v", err)
		}
		if sig.DataFreshness == nil {
			sig.DataFreshness = &DataFreshness{}
		}
		sig.DataFreshness.VendorDivergencePct = div
		return computeHardEntryGate(sig, SignalOptions{MaxVendorDivergencePct: threshold})
	}

	hasBlocker := func(g *DirectionGate) bool {
		for _, code := range g.Failed {
			if strings.HasPrefix(code, "VENDOR_DIVERGENCE_") {
				return true
			}
		}
		return false
	}

	// MYX 09-18: −2.57% vs a 2% gate → both directions blocked.
	div := -2.57
	gate := compute(&div, 2)
	for name, g := range map[string]*DirectionGate{"long": gate.Long, "short": gate.Short} {
		if g.Allowed {
			t.Errorf("%s: allowed=true with divergence %.2f%% beyond the 2%% gate", name, div)
		}
		if !hasBlocker(g) {
			t.Errorf("%s: VENDOR_DIVERGENCE blocker missing from failed=%v", name, g.Failed)
		}
	}

	// Divergence inside the threshold → gate silent.
	divIn := 1.9
	gateIn := compute(&divIn, 2)
	for name, g := range map[string]*DirectionGate{"long": gateIn.Long, "short": gateIn.Short} {
		if hasBlocker(g) {
			t.Errorf("%s: divergence 1.9%% within the 2%% gate must not block, got %v", name, g.Failed)
		}
	}

	// Disabled (negative threshold) → no check.
	gateOff := compute(&div, -1)
	for name, g := range map[string]*DirectionGate{"long": gateOff.Long, "short": gateOff.Short} {
		if hasBlocker(g) {
			t.Errorf("%s: disabled gate must not block, got %v", name, g.Failed)
		}
	}

	// Missing divergence data (nil) must not block.
	gateNil := compute(nil, 2)
	if hasBlocker(gateNil.Long) || hasBlocker(gateNil.Short) {
		t.Error("nil divergence must fail open, not block")
	}
}

func TestEffectiveMaxVendorDivergencePct(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, 2},   // unset → default 2
		{-1, -1}, // negative → disabled
		{3.5, 3.5},
	}
	for _, c := range cases {
		rc := &store.RiskControlConfig{MaxVendorDivergencePct: c.in}
		if got := EffectiveMaxVendorDivergencePct(rc); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("EffectiveMaxVendorDivergencePct(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	if got := EffectiveMaxVendorDivergencePct(nil); got != 2 {
		t.Errorf("nil config = %v, want default 2", got)
	}
}

func TestFormatPositionInfoSameSourcePnL(t *testing.T) {
	// 09-18 audit #1: KORU SHORT entry 19.23, exchange snapshot priced at
	// 19.2549 (uPnL −0.09, ROI −0.64%), live ticker 19.28. The line must
	// derive every PnL figure from the SAME live price it displays.
	pos := PositionInfo{
		Symbol: "KORUUSDT", Side: "short",
		EntryPrice: 19.23, MarkPrice: 19.2549, Quantity: 3.62,
		Leverage:         5,
		UnrealizedPnL:    -0.09,
		UnrealizedPnLPct: -0.64,
		PriceReturnPct:   -0.13,
		MarginUsed:       13.94,
	}
	engine := NewStrategyEngine(&store.StrategyConfig{})
	ctx := &Context{
		MarketDataMap: map[string]*market.Data{
			"KORUUSDT": {Symbol: "KORUUSDT", CurrentPrice: 19.28},
		},
	}
	line := engine.formatPositionInfo(1, pos, ctx)
	for _, want := range []string{
		"Entry 19.2300 Last 19.2800",
		"Margin ROI -1.30%",   // −0.26% × 5x
		"Price Return -0.26%", // (19.23−19.28)/19.23
		"Unrealized PnL -0.18", // −0.05 × 3.62
	} {
		if !strings.Contains(line, want) {
			t.Errorf("position line missing %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "-0.64%") || strings.Contains(line, "-0.13%") {
		t.Errorf("stale exchange-snapshot figures leaked into the line:\n%s", line)
	}
}
