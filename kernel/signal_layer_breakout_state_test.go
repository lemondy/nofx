package kernel

import (
	"math"
	"testing"
	"time"

	"nofx/market"
)

// ── review 2026-10-04 C1: the breakout state machine ──
//
// The level used to be the ROLLING 30-bar max high (including the newest
// closed bar): no bar could exceed it, cross detection never fired, and
// fake_break/retest_hold/extended were unreachable — the retest anchor only
// activated while the live price printed a rolling high, i.e. as a chase.
// These sequence tests pin the FIXED pre-push level and every reachable
// status, plus the long/short mirror.

// boBars builds n CLOSED 1h bars from (high, low, close); a forming candle
// is appended by boSignal.
func boBars(n int, fn func(i int) (h, l, c float64)) []market.KlineBar {
	bars := make([]market.KlineBar, 0, n+1)
	for i := 0; i < n; i++ {
		h, l, c := fn(i)
		bars = append(bars, market.KlineBar{Time: int64(i) * 3_600_000, Open: c, High: h, Low: l, Close: c, Volume: 100})
	}
	return bars
}

// boSignal wraps bars (treated as closed; one forming candle — timestamped
// to still be forming at the returned now — is appended) into the minimal
// SymbolSignal/Data computeBreakoutState needs. StructureHigh/Low are the
// ROLLING extremes over all closed bars (as computeTFSignal would report
// them). Returns now so callers pass the same settlement instant.
func boSignal(trend string, bars []market.KlineBar, price float64, volRatio float64, oi1h float64) (*SymbolSignal, *market.Data, time.Time) {
	sh, sl := 0.0, 1e18
	for _, b := range bars {
		if b.High > sh {
			sh = b.High
		}
		if b.Low < sl {
			sl = b.Low
		}
	}
	tf := &TFSignal{Trend: trend, ATRPct: 0.5, StructureHigh: &sh, StructureLow: &sl}
	if volRatio >= 0 {
		tf.VolumeRatio = &volRatio
	}
	sig := &SymbolSignal{
		Symbol:      "TESTUSDT",
		Price:       price,
		Timeframes:  map[string]*TFSignal{"1h": tf},
		Derivatives: &DerivSignal{},
	}
	if oi1h != 0 {
		oi := oi1h
		sig.Derivatives.OIChange1hPct = &oi
	}
	// Closed bars anchored 1h apart; the appended forming candle's window
	// ends after `now` so ClosedKlines drops it.
	base := int64(1_700_000_000_000)
	for i := range bars {
		bars[i].Time = base + int64(i)*3_600_000
	}
	now := time.UnixMilli(base + int64(len(bars))*3_600_000)
	klines := append(append([]market.KlineBar{}, bars...), market.KlineBar{Time: base + int64(len(bars))*3_600_000, Open: price, High: price, Low: price, Close: price})
	data := &market.Data{Symbol: "TESTUSDT", CurrentPrice: price, TimeframeData: map[string]*market.TimeframeSeriesData{
		"1h": {Timeframe: "1h", Klines: klines},
	}}
	return sig, data, now
}

// Fresh cross inside the last 8 bars (outside the level window) with volume
// + OI expansion → confirmed, and the level is the FIXED prior-window max,
// not the rolling one.
func TestBreakoutStateConfirmedFixedLevel(t *testing.T) {
	bars := boBars(40, func(i int) (h, l, c float64) {
		if i < 32 {
			return 100.2, 99.8, 100.0
		}
		if i == 32 {
			return 102.0, 101.5, 101.8 // the cross bar — outside the level window
		}
		return 102.5, 102.0, 102.2
	})
	sig, data, now := boSignal("up", bars, 102.8, 1.8, 0.5)
	st := computeBreakoutState(data, sig, now)
	if st == nil {
		t.Fatal("state = nil")
	}
	if st.Level != 100.2 {
		t.Fatalf("level = %.2f, want the FIXED prior-window max 100.2 (rolling would be 102.5) — C1 regression", st.Level)
	}
	if st.Status != "confirmed" {
		t.Fatalf("status = %q, want confirmed", st.Status)
	}
}

// Crossed within the last 8 bars and the last close is back inside →
// fake_break (the old rolling level made this unreachable).
func TestBreakoutStateFakeBreak(t *testing.T) {
	bars := boBars(40, func(i int) (h, l, c float64) {
		if i < 32 {
			return 100.2, 99.8, 100.0
		}
		if i == 32 {
			return 102.0, 101.5, 101.8
		}
		if i == 39 {
			return 100.6, 99.9, 100.0 // closed back inside the level
		}
		return 102.5, 102.0, 102.2
	})
	sig, data, now := boSignal("up", bars, 100.8, 1.8, 0.5) // live poking back above intrabar
	st := computeBreakoutState(data, sig, now)
	if st.Status != "fake_break" {
		t.Fatalf("status = %q, want fake_break", st.Status)
	}
}

// boRetestBars (review 2026-10-07 B2-3): 60 bars. Bar 10 is an OLDER higher
// high (before the level window, 104) that must NOT influence the state.
// Level window [22,52) has max high 101.5. Bars 52-53 stay inside, bar 54
// closes across (prev close 101.0 -> 102.0), then a hold tail. touchAt>0
// makes that tail bar dip to 101.2 (within 0.25% of the level) and close
// beyond; backInside makes the last closed bar close back under the level.
func boRetestBars(touchAt int, backInside bool) []market.KlineBar {
	return boBars(60, func(i int) (h, l, c float64) {
		if i == 10 {
			return 104.0, 103.4, 103.5
		}
		if backInside && i == 59 {
			return 101.6, 100.9, 101.0
		}
		if i >= 54 {
			if i == touchAt {
				return 102.3, 101.2, 102.0
			}
			return 102.3, 102.0, 102.0
		}
		if i >= 22 {
			return 101.5, 100.8, 101.0
		}
		return 95.5, 95.0, 95.2
	})
}

func boMirror(long []market.KlineBar) []market.KlineBar {
	short := make([]market.KlineBar, len(long))
	for i, b := range long {
		short[i] = market.KlineBar{Time: b.Time, Open: 200 - b.Open, High: 200 - b.Low, Low: 200 - b.High, Close: 200 - b.Close, Volume: b.Volume}
	}
	return short
}

// boRun evaluates the long scenario and its short mirror.
func boRun(bars []market.KlineBar, price, vol, oi float64) (long, short *BreakoutState) {
	sigL, dataL, nowL := boSignal("up", bars, price, vol, oi)
	sigS, dataS, nowS := boSignal("down", boMirror(bars), 200-price, vol, oi)
	return computeBreakoutState(dataL, sigL, nowL), computeBreakoutState(dataS, sigS, nowS)
}

// Cross inside the last 8 bars, pullback to the level after the cross and
// held → retest_hold. Previously unreachable (olderCross needed a pre-window
// higher high; here the old spike exists but the verdict must come from the
// real cross), B2-3 case B.
func TestBreakoutStateRetestHold(t *testing.T) {
	l, s := boRun(boRetestBars(57, false), 102.5, 0, 0)
	if l.Level != 101.5 {
		t.Fatalf("level = %.2f, want 101.5", l.Level)
	}
	if l.Status != "retest_hold" || s.Status != "retest_hold" {
		t.Fatalf("long=%q short=%q, want retest_hold", l.Status, s.Status)
	}
}

// A dip BEFORE the cross bar is not a retest of the break.
func TestBreakoutStateTouchBeforeCrossIsNotRetest(t *testing.T) {
	bars := boRetestBars(-1, false) // bars 52-53 already sit at 100.8..101.5 (touch zone) but precede the cross
	l, s := boRun(bars, 102.5, 0, 0)
	if l.Status != "extended" || s.Status != "extended" {
		t.Fatalf("long=%q short=%q, want extended (no retest AFTER the cross)", l.Status, s.Status)
	}
}

// Cross, still beyond, no retest, no confirmation, cross is 5 bars old →
// extended (chase context).
func TestBreakoutStateExtended(t *testing.T) {
	l, s := boRun(boRetestBars(-1, false), 102.5, 0, 0)
	if l.Status != "extended" || s.Status != "extended" {
		t.Fatalf("long=%q short=%q, want extended", l.Status, s.Status)
	}
}

// Fresh break with volume+OI confirmation reaches confirmed even though an
// older higher high exists outside the window (B2-3 case A: used to be
// labeled extended/retest_hold, disabling the market exception).
func TestBreakoutStateConfirmedDespiteOlderHigh(t *testing.T) {
	l, s := boRun(boRetestBars(-1, false), 102.5, 1.8, 0.5)
	if l.Status != "confirmed" || s.Status != "confirmed" {
		t.Fatalf("long=%q short=%q, want confirmed", l.Status, s.Status)
	}
}

// Crossed within the window, then the latest close is back inside → fake_break.
func TestBreakoutStateFakeBreakAfterCrossWithOlderHigh(t *testing.T) {
	l, s := boRun(boRetestBars(57, true), 101.8, 1.8, 0.5) // live price pokes back above intrabar
	if l.Status != "fake_break" || s.Status != "fake_break" {
		t.Fatalf("long=%q short=%q, want fake_break", l.Status, s.Status)
	}
}

// A wick above the level with every close inside is not a cross: with price
// still inside the level the state is below/approach, never fake_break.
func TestBreakoutStateWickOnlyIsNotCross(t *testing.T) {
	bars := boBars(60, func(i int) (h, l, c float64) {
		if i == 56 {
			return 102.0, 100.8, 101.0 // wick through 101.5, close inside
		}
		if i >= 22 {
			return 101.5, 100.8, 101.0
		}
		return 95.5, 95.0, 95.2
	})
	l, _ := boRun(bars, 101.0, 1.8, 0.5)
	if l.Status == "fake_break" || l.Status == "retest_hold" || l.Status == "extended" {
		t.Fatalf("status = %q, a wick without a close-cross must not produce a cross status", l.Status)
	}
}

// Short-side mirror of the confirmed scenario: the shape flipped through
// p → 200−p must produce the same status at the mirrored level.
func TestBreakoutStateMirrorSymmetry(t *testing.T) {
	long := boBars(40, func(i int) (h, l, c float64) {
		if i < 32 {
			return 100.2, 99.8, 100.0
		}
		if i == 32 {
			return 102.0, 101.5, 101.8
		}
		return 102.5, 102.0, 102.2
	})
	short := make([]market.KlineBar, len(long))
	for i, b := range long {
		short[i] = market.KlineBar{Time: b.Time, Open: 200 - b.Open, High: 200 - b.Low, Low: 200 - b.High, Close: 200 - b.Close, Volume: b.Volume}
	}
	sigL, dataL, nowL := boSignal("up", long, 102.8, 1.8, 0.5)
	sigS, dataS, nowS := boSignal("down", short, 97.2, 1.8, 0.5)
	stL := computeBreakoutState(dataL, sigL, nowL)
	stS := computeBreakoutState(dataS, sigS, nowS)
	if stL.Status != "confirmed" || stS.Status != "confirmed" {
		t.Fatalf("mirror statuses: long=%q short=%q, want confirmed/confirmed", stL.Status, stS.Status)
	}
	if math.Abs(stS.Level-(200-stL.Level)) > 1e-9 {
		t.Fatalf("mirror levels: long=%.2f short=%.2f, want 200−level symmetry", stL.Level, stS.Level)
	}
}

// A series too short for a prior window falls back to the TF's rolling
// structure extreme instead of nil.
func TestBreakoutStateShortSeriesFallsBack(t *testing.T) {
	bars := boBars(15, func(i int) (h, l, c float64) {
		return 100.2, 99.8, 100.0
	})
	sig, data, now := boSignal("up", bars, 101.0, 2.0, 1.0)
	st := computeBreakoutState(data, sig, now)
	if st == nil {
		t.Fatal("short series must fall back to the rolling level, not nil")
	}
	if st.Level != 100.2 {
		t.Fatalf("level = %.2f, want the rolling structure high 100.2", st.Level)
	}
}

// ── review 2026-10-04 C4: NEG_EDGE_SCORE is SIGNED ──
//
// The old math.Abs read let a long pass the evidence bar on |−70| of
// OPPOSING evidence (and a short on bullish +70) whenever the configured
// threshold sat below the CONSENSUS_OPPOSED ±50 net.
func TestNegativeEdgeScoreSignedNotAbsolute(t *testing.T) {
	mkLong := func(score int) *DirectionGate {
		sig := lpSig(100, 101, 95)
		sig.SignalConflict = &SignalConflict{DirectionalScore: score}
		opt := lpOpt()
		opt.NegativeEdge = true
		opt.NegativeEdgeMinScore = 60
		return computeHardEntryGate(sig, opt).Long
	}
	if !hasCode(mkLong(-70), "NEG_EDGE_SCORE_-70_LT_60") {
		t.Fatal("a long carrying −70 of OPPOSING evidence must fail the signed evidence bar")
	}
	if hasCode(mkLong(70), "NEG_EDGE_SCORE_") {
		t.Fatal("a long with +70 aligned evidence passes the bar")
	}
	// Short mirror: bullish +70 must not count FOR a short.
	sig := lpSig(100, 99, 105)
	sig.SignalConflict = &SignalConflict{DirectionalScore: 70}
	opt := lpOpt()
	opt.NegativeEdge = true
	opt.NegativeEdgeMinScore = 60
	g := computeHardEntryGate(sig, opt)
	if !hasCode(g.Short, "NEG_EDGE_SCORE_+70_LT_60") {
		t.Fatalf("a short carrying +70 of opposing evidence must fail, failed=%v", g.Short.Failed)
	}
	// Sanity: an aligned-but-weak long still cites its own score.
	sig2 := lpSig(100, 101, 95)
	sig2.SignalConflict = &SignalConflict{DirectionalScore: 45}
	g2 := computeHardEntryGate(sig2, opt)
	if !hasCode(g2.Long, "NEG_EDGE_SCORE_+45_LT_60") {
		t.Fatalf("aligned-but-weak long must still cite its score, failed=%v", g2.Long.Failed)
	}
}

// ── review 2026-10-04 batch-2: the two short-side gates ──
func TestBTCFilterShortGateOptIn(t *testing.T) {
	rising := make([]float64, 80)
	base := 100.0
	for i := range rising {
		rising[i] = base
		base *= 1.005
	}
	short := lpSig(100, 99, 105)
	opt := lpOpt()
	opt.BtcTrendCloses = rising
	// Default OFF — the playbook treats BTC strength for shorts at scan
	// level (×0.85 haircut off the same classifier); the hard pause is
	// opt-in via btc_filter_short.
	if g := computeHardEntryGate(short, opt); hasCode(g.Short, "BTC_4H_STRONGBULL") {
		t.Fatalf("opt-in gate must be OFF by default, failed=%v", g.Short.Failed)
	}
	opt.BTCFilterShort = true
	g := computeHardEntryGate(short, opt)
	if !hasCode(g.Short, "BTC_4H_STRONGBULL") {
		t.Fatalf("strong BTC bull must block the short when enabled, failed=%v", g.Short.Failed)
	}
	if hasCode(g.Long, "BTC_4H_STRONGBULL") {
		t.Fatal("the short-side filter must not touch the long gate")
	}
	// Missing BTC data → gate stays silent (fail-open like the long side).
	opt.BtcTrendCloses = make([]float64, 40)
	if g := computeHardEntryGate(short, opt); hasCode(g.Short, "BTC_4H_STRONGBULL") {
		t.Fatal("unclassifiable BTC data must not fire the gate")
	}
}

func TestShortTopConfirmGate(t *testing.T) {
	no, yes := false, true
	short := lpSig(100, 99, 105)
	mk := func(gate bool, confirmed *bool) *DirectionGate {
		opt := lpOpt()
		opt.ShortTopConfirmGate = gate
		opt.ShortScanConfirmed = confirmed
		return computeHardEntryGate(short, opt).Short
	}
	if !hasCode(mk(true, &no), "SHORT_TOP_CONFIRM_MISSING") {
		t.Fatal("short_scan candidate without topping confirmation must be blocked (默认姿态 program-enforced)")
	}
	if hasCode(mk(true, &yes), "SHORT_TOP_CONFIRM_MISSING") {
		t.Fatal("confirmed short_scan candidate must pass")
	}
	// No short_scan evidence (piggy breakdown / model-initiated) → not gated.
	if hasCode(mk(true, nil), "SHORT_TOP_CONFIRM_MISSING") {
		t.Fatal("candidates without short_scan evidence must not be gated")
	}
	// Config off → never fires.
	if hasCode(mk(false, &no), "SHORT_TOP_CONFIRM_MISSING") {
		t.Fatal("gate disabled must not fire")
	}
	// The long gate never reads the short confirmation.
	optL := lpOpt()
	optL.ShortTopConfirmGate = true
	optL.ShortScanConfirmed = &no
	if g := computeHardEntryGate(lpSig(100, 101, 95), optL); hasCode(g.Long, "SHORT_TOP_CONFIRM_MISSING") {
		t.Fatal("the confirmation gate must not touch the long side")
	}
}
