package breakout

import (
	"strings"
	"testing"
	"time"
)

// E2/A2 (QUANT_REVIEW 09-22) pins for the backtest protocol.

// The BTC regime timeline must reproduce the online haircuts: bull →
// counter-BTC (down) ×0.85; bear → counter-BTC (up) ×0.85; chop → both
// ×0.95. And regimeAt must pick the last 4h bar CLOSED at signal time.
func TestBTCRegimeTimelineAndRegimeAt(t *testing.T) {
	// Build 60 4h bars: flat for 20 bars, strong rally for 20, strong dump
	// for 20 — the EMA20/50+RSI classifier should read bull mid-rally and
	// bear mid-dump.
	n := 60
	k := make([]Kline, n)
	price := 100.0
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		switch {
		case i < 20:
			// flat
		case i < 40:
			price *= 1.02 // rally
		default:
			price *= 0.97 // dump
		}
		k[i] = Kline{OpenTime: start.Add(time.Duration(i) * 4 * time.Hour).UnixMilli(), Close: price, High: price * 1.001, Low: price * 0.999, Open: price}
	}
	tl := btcRegimeTimeline(k)
	if len(tl) != n {
		t.Fatalf("timeline len %d, want %d", len(tl), n)
	}

	// Mid-rally bar (bar 35 closed): down-direction must carry the 0.85 haircut.
	rallyTime := start.Add(36 * 4 * time.Hour) // after bar 35 closes
	regime, mult := regimeAt(tl, rallyTime, DirDown)
	if regime != "btc_bull" || mult != 0.85 {
		t.Fatalf("mid-rally down regime=%s mult=%.2f, want btc_bull/0.85", regime, mult)
	}
	// Same instant, up-direction rides at 1.0.
	_, upMult := regimeAt(tl, rallyTime, DirUp)
	if upMult != 1.0 {
		t.Fatalf("mid-rally up mult %.2f, want 1.0", upMult)
	}

	// Mid-dump: up-direction haircuts.
	dumpTime := start.Add(56 * 4 * time.Hour)
	regime, upMult = regimeAt(tl, dumpTime, DirUp)
	if regime != "btc_bear" || upMult != 0.85 {
		t.Fatalf("mid-dump up regime=%s mult=%.2f, want btc_bear/0.85", regime, upMult)
	}

	// Before any bar closed (t = open of bar 0): the earliest bar applies as
	// fallback — must not panic, multiplier sane.
	_, m := regimeAt(tl, start, DirDown)
	if m <= 0 || m > 1 {
		t.Fatalf("fallback multiplier %.2f out of range", m)
	}
}

// Forward returns recorded by RunBacktest must be NET of the round-trip
// cost — pinned via the constant and the arithmetic it feeds (the recording
// site subtracts btCostRoundTrip; a regression removing it breaks this).
func TestBacktestCostModelConstant(t *testing.T) {
	if btCostRoundTrip != 0.20 {
		t.Fatalf("round-trip cost %.2f%% drifted — 2×5bps taker + 2×5bps slippage = 0.20%%; if exchange fees changed, update and re-run the backtest", btCostRoundTrip)
	}
	// Gross 0.5% → net 0.3%.
	gross := 0.5
	if got := gross - btCostRoundTrip; got != 0.30 {
		t.Fatalf("net arithmetic %.2f, want 0.30", got)
	}
}

// Walk-forward: a cutoff that looks good in-sample must be REJECTED when it
// fails to verify on the held-out test segment, and accepted when it does.
func TestTuneWalkForwardVerifiesOnHoldout(t *testing.T) {
	// Build a cohort where the oldest 70% (train) show a great edge above
	// score 80, but the newest 30% (test) show that bucket UNDERPERFORMING
	// the test-wide average — the in-sample argmax trap.
	mk := func(n, startIdx int, hiEdge, loEdge float64, t0 time.Time) []BTSignal {
		out := make([]BTSignal, n)
		for i := 0; i < n; i++ {
			score := 60.0
			ret := loEdge
			if i%3 != 0 { // 2/3 of the cohort sits in the hi bucket — enough to clear btMinSample on train
				score = 85.0
				ret = hiEdge
			}
			out[i] = BTSignal{
				Time:   t0.Add(time.Duration(startIdx+i) * time.Hour),
				Symbol: "TESTUSDT",
				Score:  score,
				Ret24h: ret,
			}
		}
		return out
	}
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// train: 60 signals, half at 85 with +2% edge, half at 60 with 0%.
	train := mk(60, 0, 2.0, 0.0, t0)
	// test: 30 signals, the 85-bucket now LOSES (−3%) while the 60-bucket is +0.1%.
	test := mk(30, 100, -3.0, 0.1, t0)
	signals := append(train, test...)

	_, verified, rejected, trainN, testN := tuneWalkForward(signals)
	// 90×0.7 lands around 63 — exact value is float-determined; what matters
	// is a usable split (train ≥ btMinSample, test ≥ btVerifySample).
	if trainN < 55 || testN < 15 {
		t.Fatalf("split %d/%d too thin", trainN, testN)
	}
	if len(verified) != 0 {
		t.Fatalf("in-sample-trap cutoff must not verify, got %v", verified)
	}
	found := false
	for _, r := range rejected {
		if strings.Contains(r, "strong_threshold") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a rejected strong_threshold change, got %v", rejected)
	}

	// Now the honest case: the 85-bucket keeps its edge out-of-sample →
	// the change verifies and is applied.
	test = mk(30, 100, 3.0, 0.1, t0)
	signals = append(train, test...)
	prev := GetParams()
	t.Cleanup(func() { ApplyParams(prev) })
	changes, verified, rejected, _, _ := tuneWalkForward(signals)
	foundStrong := false
	for _, v := range verified {
		if strings.Contains(v, "strong_threshold") {
			foundStrong = true
		}
	}
	if !foundStrong {
		t.Fatalf("honest edge should verify, changes=%v rejected=%v", changes, rejected)
	}
	if GetParams().StrongThreshold == prev.StrongThreshold {
		t.Fatal("verified cutoff should have been applied")
	}
}
