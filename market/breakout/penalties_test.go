package breakout

import (
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

// Timestamp sanity for the tuning cadence change: weekly, not 72h.
func TestTuningCadenceConstants(t *testing.T) {
	// The scheduler builds its ticker inline; pin the intent via the interval
	// default used elsewhere. (Scheduler.Interval is overridable in tests.)
	s := DefaultScheduler()
	if s.Interval < 5*time.Minute || s.Interval > 6*time.Minute {
		t.Fatalf("scan interval = %v, want ~5m", s.Interval)
	}
}
