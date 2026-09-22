package kernel

import "testing"

// Pins for the direction-matched market-exception verdict
// (QUANT_REVIEW_2026-09-22 B1): a long bb_ride may not evidence a SHORT
// market exception and vice versa; the breakout leg needs its own direction
// AND |directional_score| ≥ MarketExceptionMinScore in the trade's favor
// (prompt condition ④, now code).
func TestMarketExceptionEvidenceDirectionMatched(t *testing.T) {
	longRide := &SymbolSignal{BBRide: &BBRide{Ride: true}}
	if !marketExceptionEvidence(longRide, true) {
		t.Fatal("long bb_ride must evidence the long exception")
	}
	if marketExceptionEvidence(longRide, false) {
		t.Fatal("a long bb_ride must NOT evidence the short exception (short_ride absent)")
	}

	shortRide := &SymbolSignal{ShortRide: &BBShortRide{Ride: true}}
	if !marketExceptionEvidence(shortRide, false) {
		t.Fatal("short_ride must evidence the short exception")
	}
	if marketExceptionEvidence(shortRide, true) {
		t.Fatal("a short_ride must NOT evidence the long exception")
	}

	// Breakout confirmed + volume + OI but no directional score → no
	// exception: the six conditions are conjunctive.
	mkBreakout := func(dir string) *SymbolSignal {
		return &SymbolSignal{
			Breakout: &BreakoutState{Status: "confirmed", VolumeConfirm: true, OIConfirm: true, Direction: dir},
		}
	}
	if marketExceptionEvidence(mkBreakout("breakout"), true) {
		t.Fatal("breakout exception without directional_score ≥ +80 must not fire")
	}
	if marketExceptionEvidence(mkBreakout("breakdown"), false) {
		t.Fatal("breakdown exception without directional_score ≤ −80 must not fire")
	}

	withScore := mkBreakout("breakout")
	withScore.SignalConflict = &SignalConflict{DirectionalScore: MarketExceptionMinScore}
	if !marketExceptionEvidence(withScore, true) {
		t.Fatal("breakout + score ≥ +80 must fire the long exception")
	}
	subBar := mkBreakout("breakout")
	subBar.SignalConflict = &SignalConflict{DirectionalScore: MarketExceptionMinScore - 1}
	if marketExceptionEvidence(subBar, true) {
		t.Fatal("score one under the bar must not fire")
	}
	wrongSide := mkBreakout("breakout")
	wrongSide.SignalConflict = &SignalConflict{DirectionalScore: 100}
	if marketExceptionEvidence(wrongSide, false) {
		t.Fatal("a BREAKOUT confirmation must not fire the SHORT exception regardless of score")
	}
}
