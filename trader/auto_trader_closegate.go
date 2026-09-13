package trader

import (
	"fmt"
	"nofx/kernel"
	"nofx/market"
	"time"
)

// closeGateView is the minimal structure evidence the close gate needs,
// extracted from the symbol's signal snapshot (kernel.ComputeSymbolSignals —
// the same normalized block the prompt sees).
type closeGateView struct {
	// For a long: nearest overhead resistance above the mark (15m/1h arrays +
	// structure highs). For a short: nearest support below the mark.
	NearestOppositeLevel float64
	// True when the 15m near-TF structure already reversed: long — price at/
	// below the nearest 15m support; short — price at/above the nearest 15m
	// resistance. The gate only blocks while this is FALSE.
	StructureBroken bool
	PrimaryTFTrend  string
	MarkPrice       float64
}

// nearestOppositeLevel scans the 15m/1h structure arrays for the level that
// caps (long: nearest resistance above) or floors (short: nearest support
// below) the given reference price. Returns 0 when no such level exists.
func nearestOppositeLevel(sig *kernel.SymbolSignal, side string, refPrice float64) float64 {
	if sig == nil || refPrice <= 0 {
		return 0
	}
	var level float64
	for _, tfName := range []string{"15m", "1h"} {
		t, ok := sig.Timeframes[tfName]
		if !ok || t == nil {
			continue
		}
		for _, res := range t.Resistance {
			if side == "long" && res > refPrice && (level == 0 || res < level) {
				level = res
			}
		}
		for _, sup := range t.Support {
			if side == "short" && sup > 0 && sup < refPrice && (level == 0 || sup > level) {
				level = sup
			}
		}
		if t.StructureHigh != nil && side == "long" && *t.StructureHigh > refPrice &&
			(level == 0 || *t.StructureHigh < level) {
			level = *t.StructureHigh
		}
		if t.StructureLow != nil && side == "short" && *t.StructureLow > 0 && *t.StructureLow < refPrice &&
			(level == 0 || *t.StructureLow > level) {
			level = *t.StructureLow
		}
	}
	return level
}

// buildCloseGateView pulls the structure levels out of the market data for one
// symbol. Returns nil when no snapshot exists or the data is unusable — the
// gate then stays silent (never block on missing data).
func buildCloseGateView(data *market.Data, side string, markPrice float64) *closeGateView {
	if data == nil || markPrice <= 0 {
		return nil
	}
	sig, err := kernel.ComputeSymbolSignals(data.Symbol, data, kernel.SignalOptions{
		Now:          time.Now(),
		PrimaryTF:    "15m",
		CurrentPrice: markPrice,
	})
	if err != nil || sig == nil {
		return nil
	}
	tf15, ok := sig.Timeframes["15m"]
	if !ok || tf15 == nil {
		return nil
	}
	v := &closeGateView{
		PrimaryTFTrend:       tf15.Trend,
		MarkPrice:            markPrice,
		NearestOppositeLevel: nearestOppositeLevel(sig, side, markPrice),
	}
	if side == "long" {
		// 15m structure broken: price at/below the nearest support.
		if len(tf15.Support) > 0 && tf15.Support[0] > 0 {
			v.StructureBroken = markPrice <= tf15.Support[0]
		}
	} else {
		// 15m structure broken: price at/above the nearest resistance.
		if len(tf15.Resistance) > 0 && tf15.Resistance[0] > 0 {
			v.StructureBroken = markPrice >= tf15.Resistance[0]
		}
	}
	return v
}

// closeRejectBreakoutBlocks decides whether an AI-initiated close of a LOSING
// position must be blocked to give a nearby breakout/breakdown room:
//  - the position is underwater (long below entry / short above entry),
//  - the nearest opposite structure level is within thresholdPct of the mark,
//  - the 15m structure is NOT yet broken (no confirmed reversal).
// Exiting at a loss into a nearby level that rejected the price is exactly
// where breakouts happen; the position still exits via its stop-loss, a
// confirmed 15m structure break, or a normal profitable take-profit (all
// bypass this gate). Fail-open: missing data never blocks.
func closeRejectBreakoutBlocks(side string, entryPrice float64, v *closeGateView, thresholdPct float64) (bool, string) {
	if v == nil || thresholdPct <= 0 || entryPrice <= 0 || v.MarkPrice <= 0 {
		return false, ""
	}
	// Only guard LOSING exits — a profitable exit near resistance is a
	// legitimate take-profit, not a panic sell.
	if side == "long" && v.MarkPrice >= entryPrice {
		return false, ""
	}
	if side == "short" && v.MarkPrice <= entryPrice {
		return false, ""
	}
	if v.NearestOppositeLevel <= 0 {
		return false, "" // no structure level known — nothing to wait for
	}
	var distPct float64
	if side == "long" {
		distPct = (v.NearestOppositeLevel - v.MarkPrice) / v.MarkPrice * 100
	} else {
		distPct = (v.MarkPrice - v.NearestOppositeLevel) / v.MarkPrice * 100
	}
	if distPct <= 0 || distPct >= thresholdPct {
		return false, "" // level absent, or too far to matter
	}
	if v.StructureBroken {
		return false, "" // 15m structure already reversed — let the AI exit
	}
	kind := "resistance"
	if side == "short" {
		kind = "support"
	}
	return true, fmt.Sprintf(
		"breakout-hold: %s is underwater but nearest %s %.2f%% away and 15m structure intact (trend %s) — rejection-exit blocked, wait for breakout or 15m break",
		side, kind, distPct, v.PrimaryTFTrend,
	)
}

// entrySupplyZoneBlocks rejects a limit-entry anchor parked right at the
// opposite-side structure: a long limit filled just below resistance buys
// into supply with no room; a short limit just above support sells into the
// bounce zone. Fail-open on missing data. side: "long"/"short";
// anchorPrice: the limit price about to be placed.
func entrySupplyZoneBlocks(sig *kernel.SymbolSignal, side string, anchorPrice, thresholdPct float64) (bool, string) {
	if sig == nil || anchorPrice <= 0 || thresholdPct <= 0 {
		return false, ""
	}
	level := nearestOppositeLevel(sig, side, anchorPrice)
	if level <= 0 {
		return false, ""
	}
	var distPct float64
	if side == "long" {
		distPct = (level - anchorPrice) / anchorPrice * 100
	} else {
		distPct = (anchorPrice - level) / anchorPrice * 100
	}
	if distPct <= 0 || distPct >= thresholdPct {
		return false, ""
	}
	kind := "resistance"
	if side == "short" {
		kind = "support"
	}
	return true, fmt.Sprintf(
		"supply-zone: %s anchor %.6g sits %.2f%% from 15m/1h %s %.6g (< %.2f%%) — fill would land inside the %s zone, re-anchor lower/higher or skip the setup",
		side, anchorPrice, distPct, kind, level, thresholdPct, map[string]string{"long": "supply", "short": "demand"}[side],
	)
}
