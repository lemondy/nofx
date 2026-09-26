package kernel

import "testing"

// Pins for the direction-matched market-exception verdict
// (QUANT_REVIEW_2026-09-22 B1): a long bb_ride may not evidence a SHORT
// market exception and vice versa; the breakout leg needs its own direction
// AND |directional_score| ≥ MarketExceptionMinScore in the trade's favor
// (prompt condition ④, now code).
func TestMarketExceptionEvidenceDirectionMatched(t *testing.T) {
	cool := func() *DerivSignal { f := 10.0; return &DerivSignal{FundingAnnualizedPct: &f} }
	hot := func() *DerivSignal { f := 60.0; return &DerivSignal{FundingAnnualizedPct: &f} }

	longRide := &SymbolSignal{BBRide: &BBRide{Ride: true}, Derivatives: cool()}
	if !marketExceptionEvidence(longRide, true) {
		t.Fatal("long bb_ride with cool funding must evidence the long exception")
	}
	// 2026-09-27 external review P2: funding-heat filter — crowded-side
	// funding downgrades the market exception (fail-closed on UNKNOWN).
	hotLongRide := &SymbolSignal{BBRide: &BBRide{Ride: true}, Derivatives: hot()}
	if marketExceptionEvidence(hotLongRide, true) {
		t.Fatal("long bb_ride with +60% funding (longs crowded) must NOT get the market exception")
	}
	noFundingRide := &SymbolSignal{BBRide: &BBRide{Ride: true}}
	if marketExceptionEvidence(noFundingRide, true) {
		t.Fatal("UNKNOWN funding must fail-closed: no market exception")
	}
	if marketExceptionEvidence(longRide, false) {
		t.Fatal("a long bb_ride must NOT evidence the short exception (short_ride absent)")
	}

	shortRide := &SymbolSignal{ShortRide: &BBShortRide{Ride: true}, Derivatives: cool()}
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
	withScore.Derivatives = cool()
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
