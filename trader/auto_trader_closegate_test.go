package trader

import (
	"testing"

	"nofx/store"

	"nofx/kernel"
)

func TestCloseRejectBreakoutBlocks(t *testing.T) {
	threshold := 1.0

	t.Run("long underwater near overhead resistance blocked", func(t *testing.T) {
		// NEAR prototype: entry 2.391, mark 2.385, 1h resistance 2.3913 (~0.27% away).
		v := &closeGateView{
			NearestOppositeLevel: 2.3913,
			PrimaryTFTrend:       "up",
			MarkPrice:            2.385,
		}
		blocked, reason := closeRejectBreakoutBlocks("long", 2.391, v, threshold)
		if !blocked {
			t.Fatalf("expected block, got none")
		}
		if reason == "" {
			t.Fatal("expected a rejection reason")
		}
	})

	t.Run("long profitable near resistance is a legit take-profit", func(t *testing.T) {
		v := &closeGateView{
			NearestOppositeLevel: 2.412,
			PrimaryTFTrend:       "up",
			MarkPrice:            2.395, // above entry
		}
		if blocked, _ := closeRejectBreakoutBlocks("long", 2.391, v, threshold); blocked {
			t.Fatal("profitable close must never be blocked")
		}
	})

	t.Run("long with 15m support broken lets the exit through", func(t *testing.T) {
		v := &closeGateView{
			NearestOppositeLevel: 2.3913,
			StructureBroken:      true,
			PrimaryTFTrend:       "down",
			MarkPrice:            2.385,
		}
		if blocked, _ := closeRejectBreakoutBlocks("long", 2.391, v, threshold); blocked {
			t.Fatal("structure-broken close must not be blocked")
		}
	})

	t.Run("long with resistance beyond threshold is free to exit", func(t *testing.T) {
		v := &closeGateView{
			NearestOppositeLevel: 2.443, // ~2.4% away
			PrimaryTFTrend:       "range",
			MarkPrice:            2.385,
		}
		if blocked, _ := closeRejectBreakoutBlocks("long", 2.391, v, threshold); blocked {
			t.Fatal("far resistance must not block the exit")
		}
	})

	t.Run("short underwater near support below blocked", func(t *testing.T) {
		// Mirror case: short from 2.36, price rallied to 2.40 (underwater),
		// support sits 2.392 (~0.2% below the mark) — breakdown room blocked.
		v := &closeGateView{
			NearestOppositeLevel: 2.392,
			PrimaryTFTrend:       "up",
			MarkPrice:            2.40,
		}
		blocked, _ := closeRejectBreakoutBlocks("short", 2.36, v, threshold)
		if !blocked {
			t.Fatal("expected short-side block")
		}
	})

	t.Run("short profitable is exempt", func(t *testing.T) {
		v := &closeGateView{
			NearestOppositeLevel: 2.392,
			PrimaryTFTrend:       "down",
			MarkPrice:            2.38, // below entry — profit
		}
		if blocked, _ := closeRejectBreakoutBlocks("short", 2.42, v, threshold); blocked {
			t.Fatal("profitable short close must never be blocked")
		}
	})

	t.Run("missing level or bad data fails open", func(t *testing.T) {
		if blocked, _ := closeRejectBreakoutBlocks("long", 2.391, nil, threshold); blocked {
			t.Fatal("nil view must fail open")
		}
		v := &closeGateView{NearestOppositeLevel: 0, MarkPrice: 2.385}
		if blocked, _ := closeRejectBreakoutBlocks("long", 2.391, v, threshold); blocked {
			t.Fatal("no-level view must fail open")
		}
		v2 := &closeGateView{NearestOppositeLevel: 2.3913, MarkPrice: 2.385}
		if blocked, _ := closeRejectBreakoutBlocks("long", 2.391, v2, 0); blocked {
			t.Fatal("disabled threshold must fail open")
		}
	})
}

func TestBuildCloseGateView(t *testing.T) {
	t.Run("nil data fails open", func(t *testing.T) {
		if v := buildCloseGateView(nil, "long", 100); v != nil {
			t.Fatal("nil data must return nil view")
		}
	})
	// Structure extraction itself is exercised via kernel.ComputeSymbolSignals
	// tests (support/resistance arrays + anchor semantics); here we only pin
	// the fail-open contract of the view builder.
}

func TestEntrySupplyZoneBlocks(t *testing.T) {
	threshold := 0.5

	sig := &kernel.SymbolSignal{
		Timeframes: map[string]*kernel.TFSignal{
			"15m": {
				Support:    []float64{105.61, 104.94},
				Resistance: []float64{106.89, 107.14},
			},
			"1h": {
				Support:       []float64{104.59, 103.06},
				Resistance:    []float64{107.28},
				StructureHigh: ptrFloat(107.36),
			},
		},
	}

	t.Run("SOL prototype: long anchor just under 15m resistance blocked", func(t *testing.T) {
		// Anchor 106.76, nearest overhead 106.89 → 0.12% < 0.5%.
		blocked, reason := entrySupplyZoneBlocks(sig, "long", 106.76, threshold)
		if !blocked {
			t.Fatal("anchor 0.12% under resistance must be blocked")
		}
		if reason == "" {
			t.Fatal("expected a rejection reason")
		}
	})

	t.Run("long anchor with clean air above allowed", func(t *testing.T) {
		// Anchor 106.0 → nearest overhead 106.89 = 0.82% away > 0.5%.
		if blocked, _ := entrySupplyZoneBlocks(sig, "long", 106.0, threshold); blocked {
			t.Fatal("anchor 0.82% under resistance must pass")
		}
	})

	t.Run("short anchor just above 15m support blocked", func(t *testing.T) {
		// Anchor 105.80, support 105.61 → 0.18% above support < 0.5%.
		blocked, _ := entrySupplyZoneBlocks(sig, "short", 105.80, threshold)
		if !blocked {
			t.Fatal("anchor 0.18% above support must be blocked")
		}
	})

	t.Run("short anchor with clean air below allowed", func(t *testing.T) {
		// Anchor 106.4 → support 105.61 = 0.74% below > 0.5%.
		if blocked, _ := entrySupplyZoneBlocks(sig, "short", 106.4, threshold); blocked {
			t.Fatal("anchor 0.74% above support must pass")
		}
	})

	t.Run("no opposite level or disabled fails open", func(t *testing.T) {
		empty := &kernel.SymbolSignal{Timeframes: map[string]*kernel.TFSignal{
			"15m": {Support: []float64{}, Resistance: []float64{}},
		}}
		if blocked, _ := entrySupplyZoneBlocks(empty, "long", 106.76, threshold); blocked {
			t.Fatal("no-level signal must fail open")
		}
		if blocked, _ := entrySupplyZoneBlocks(sig, "long", 106.76, 0); blocked {
			t.Fatal("disabled threshold must fail open")
		}
		if blocked, _ := entrySupplyZoneBlocks(nil, "long", 106.76, threshold); blocked {
			t.Fatal("nil signal must fail open")
		}
	})
}

func ptrFloat(v float64) *float64 { return &v }

func TestDrawdownProtectThresholds(t *testing.T) {
	at := &AutoTrader{}
	// nil config → designed defaults
	minP, maxDD := at.drawdownProtectThresholds()
	if minP != 5.0 || maxDD != 55.0 {
		t.Fatalf("nil config: want 5/55, got %.1f/%.1f", minP, maxDD)
	}
	// explicit strategy overrides
	at.config = AutoTraderConfig{StrategyConfig: &store.StrategyConfig{RiskControl: store.RiskControlConfig{
		PeakDrawdownMinProfitPct: 8,
		PeakDrawdownMaxDDPct:     40,
	}}}
	minP, maxDD = at.drawdownProtectThresholds()
	if minP != 8.0 || maxDD != 40.0 {
		t.Fatalf("override: want 8/40, got %.1f/%.1f", minP, maxDD)
	}
	// zero values fall back to defaults
	at.config.StrategyConfig.RiskControl.PeakDrawdownMinProfitPct = 0
	at.config.StrategyConfig.RiskControl.PeakDrawdownMaxDDPct = 0
	minP, maxDD = at.drawdownProtectThresholds()
	if minP != 5.0 || maxDD != 55.0 {
		t.Fatalf("zeros: want 5/55 defaults, got %.1f/%.1f", minP, maxDD)
	}
}
