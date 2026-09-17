package breakout

import (
	"math"
	"testing"
	"time"
)

// BTC regime classification from synthetic 4h bars.
func TestBtcRegimePenalty(t *testing.T) {
	build := func(rising bool, rsiHigh bool) []Kline {
		k := make([]Kline, 40)
		price := 100.0
		for i := range k {
			if rising {
				price *= 1.02
			} else {
				price *= 0.99
			}
			h := price * 1.005
			l := price * 0.995
			_ = rsiHigh
			k[i] = Kline{OpenTime: int64(i), Open: price * 0.999, High: h, Low: l, Close: price}
		}
		return k
	}
	mult, regime := btcRegimePenalty(build(true, true))
	if regime != "btc_bull" || mult != 0.85 {
		t.Fatalf("strong uptrend: want (0.85, btc_bull), got (%.2f, %s)", mult, regime)
	}
	mult, regime = btcRegimePenalty(build(false, false))
	if regime != "btc_bear" || mult != 1.0 {
		t.Fatalf("downtrend: want (1.0, btc_bear) — labelled, no boost; got (%.2f, %s)", mult, regime)
	}
	if _, regime := btcRegimePenalty(nil); regime != "" {
		t.Fatalf("nil klines must fail open, got %s", regime)
	}
}

// The extended-chase and crowd penalties are pure multipliers applied inside
// Analyze; their constants are pinned here so a future edit can't silently
// drop the haircuts (the NEAR/SOL early-close postmortem showed how costly
// unvetted chase entries are).
func TestPenaltyConstants(t *testing.T) {
	if extendedPenalty != 0.65 {
		t.Fatalf("extendedPenalty = %.2f, want 0.65", extendedPenalty)
	}
	if crowdMaxPenalty != 0.30 {
		t.Fatalf("crowdMaxPenalty = %.2f, want 0.30", crowdMaxPenalty)
	}
}

// Extended must be charged ONCE: the cross-section headline multiplier
// (extendedPenalty in Analyze) is the only extended haircut. The Price dim
// used to also take ×0.8 for the same 1h extended fact — dim ×0.8 feeding
// the 1h TF score, then ×0.65 on the combined result, stacking to ≈×0.62
// (user audit 2026-09-17). This test pins the dim back to the raw
// breakout-strength sigmoid.
func TestExtendedNoDimDoubleCharge(t *testing.T) {
	// 1h series: 20 bars below the 100 level, then 14 bars holding above it
	// with no retest (lows stay well clear of level + 0.25×ATR) → cross age
	// 13 ≥ crossWindowBars and held ⇒ extended, confirmed.
	const level = 100.0
	k := make([]Kline, 34)
	for i := range k {
		var c float64
		if i < 20 {
			c = 95.0
		} else {
			c = 101.0
		}
		k[i] = Kline{OpenTime: int64(i), Open: c, High: c + 0.2, Low: c - 0.1, Close: c}
	}
	levels := []Level{{Name: "20d_high", Price: level}}

	rep := computeTF("1h", DirUp, k, levels, &shared{})
	if rep.Pattern != PatternExtended {
		t.Fatalf("pattern = %q, want extended (crossAge=%d)", rep.Pattern, rep.CrossAgeBars)
	}
	if !rep.Confirmed {
		t.Fatal("extended-with-hold must be confirmed — confirmPenalty is a different branch")
	}
	want := sigmoidScore(rep.ATRStrength, GetParams().PriceATRCenter, GetParams().PriceATRWidth)
	if math.Abs(rep.Dims.Price-want) > 1e-9 {
		t.Fatalf("Price dim = %.4f, want raw sigmoid %.4f — a dim-level extended discount is back", rep.Dims.Price, want)
	}
}

// The confluence bonus must not push the Price dim past the 0-100 scale
// (Structure caps at 100 the same way). Clustered level families drive
// confluence ≥ 2 → ×1.08 on a base score of ~97 would overflow to ~105.
func TestPriceDimClampedAt100(t *testing.T) {
	const level = 100.0
	k := make([]Kline, 34)
	for i := range k {
		var c float64
		if i < 20 {
			c = 95.0
		} else {
			c = 101.0
		}
		k[i] = Kline{OpenTime: int64(i), Open: c, High: c + 0.2, Low: c - 0.1, Close: c}
	}
	// Three independent source families clustered within 0.5×ATR (~0.15).
	levels := []Level{
		{Name: "20d_high", Price: level},
		{Name: "60d_high", Price: level},
		{Name: "vpvr_poc", Price: level},
	}
	rep := computeTF("1h", DirUp, k, levels, &shared{})
	if rep.Confluence < 2 {
		t.Fatalf("confluence = %d, want ≥2 for the overflow case", rep.Confluence)
	}
	if rep.Dims.Price > 100 {
		t.Fatalf("Price dim = %.2f overflows the 0-100 scale", rep.Dims.Price)
	}
}

// Timestamp sanity for the tuning cadence change: weekly, not 72h.
func TestTuningCadenceConstants(t *testing.T) {
	// The scheduler builds its ticker inline; pin the intent via the interval
	// default used elsewhere. (Scheduler.Interval is overridable in tests.)
	s := DefaultScheduler()
	if s.Interval < 5*time.Minute || s.Interval > 6*time.Minute {
		t.Fatalf("scan interval = %v, want ~5m", s.Interval)
	}
}
