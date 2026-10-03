package kernel

import (
	"strings"
	"testing"

	"nofx/market"
)

// lpSig builds a minimal long-breakout signal: breakout confirmed at `level`,
// live price `price`, 4h EMA20 at `ema20`.
func lpSig(level, price, ema20 float64) *SymbolSignal {
	e := ema20
	return &SymbolSignal{
		Symbol: "TESTUSDT",
		Price:  price,
		Timeframes: map[string]*TFSignal{
			"4h": {EMAFast: &e},
		},
		Breakout: &BreakoutState{
			Status:    "confirmed",
			Direction: "breakout",
			Level:     level,
		},
	}
}

// lpData builds the minimal market.Data the plan builder needs (live price).
func lpData(price float64) *market.Data {
	return &market.Data{Symbol: "TESTUSDT", CurrentPrice: price}
}

func lpOpt() SignalOptions {
	return SignalOptions{
		LongPullbackEntry:   true,
		LongMaxEMA20DistPct: 12,
		BTCFilterLong:       true,
		MinRR:               1.2,
	}
}

func hasCode(g *DirectionGate, prefix string) bool {
	if g == nil {
		return false
	}
	for _, c := range g.Failed {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestLongPullbackAnchorIsBrokenLevel(t *testing.T) {
	sig := lpSig(100, 110, 95)
	plan := buildLongPullbackPlan(sig, lpData(110), lpOpt())
	if plan == nil || !plan.Active || plan.Entry != 100 {
		t.Fatalf("confirmed breakout @110 over level 100 must activate the retest anchor at 100, got %+v", plan)
	}
	if plan.ChaseDistPct < 9.9 || plan.ChaseDistPct > 10.1 {
		t.Fatalf("chase distance should be ~10%%, got %v", plan.ChaseDistPct)
	}
	if plan.EMA20Dist4hPct == nil || *plan.EMA20Dist4hPct < 15.7 || *plan.EMA20Dist4hPct > 15.9 {
		t.Fatalf("EMA20 distance should be ~15.8%%, got %v", plan.EMA20Dist4hPct)
	}
}

func TestLongPullbackInactiveStates(t *testing.T) {
	sig := lpSig(100, 99, 95) // price below level = failed retest
	sig.Breakout.Status = "fake_break"
	if p := buildLongPullbackPlan(sig, lpData(99), lpOpt()); p != nil && p.Active {
		t.Fatal("fake_break must not activate the retest anchor")
	}
	sig2 := lpSig(100, 110, 95)
	sig2.Breakout.Direction = "breakdown"
	if p := buildLongPullbackPlan(sig2, lpData(110), lpOpt()); p != nil && p.Active {
		t.Fatal("breakdown direction must not activate the long retest anchor")
	}
	off := lpOpt()
	off.LongPullbackEntry = false
	if p := buildLongPullbackPlan(lpSig(100, 110, 95), lpData(110), off); p != nil && p.Active {
		t.Fatal("switch off must keep the legacy offset anchor")
	}
}

func TestEMA20StretchGateExemptsPullback(t *testing.T) {
	// price 15.8% above the 4h EMA20, cap 12 — a CHASE must be blocked.
	sig := lpSig(100, 110, 95)
	sig.LongPullback = nil // no pullback anchor = chase context
	g := computeHardEntryGate(sig, lpOpt())
	if !hasCode(g.Long, "EMA20_STRETCH_") {
		t.Fatalf("chase 15.8%% above EMA20 (cap 12) must emit EMA20_STRETCH, failed=%v", g.Long.Failed)
	}
	// The same stretch WITH an active retest anchor is the designed path.
	sig.LongPullback = buildLongPullbackPlan(sig, lpData(110), lpOpt())
	sig.LongPullback.Active = true
	g2 := computeHardEntryGate(sig, lpOpt())
	if hasCode(g2.Long, "EMA20_STRETCH_") {
		t.Fatalf("retest-anchor path must be exempt from the stretch gate, failed=%v", g2.Long.Failed)
	}
}

func TestBTCWeakLongGates(t *testing.T) {
	declining := make([]float64, 80) // TRUE 4h closes: steady −0.5%/bar
	base := 100.0
	for i := range declining {
		declining[i] = base
		base *= 0.995
	}
	rising := make([]float64, 80)
	base = 100.0
	for i := range rising {
		rising[i] = base
		base *= 1.005
	}

	opt := lpOpt()
	opt.BtcTrendCloses = declining
	weak := lpSig(100, 110, 95)
	weak.Derivatives = &DerivSignal{}
	// declining series' own 24h return ≈ −11.3% — the coin must lag even that
	w24 := -15.0
	weak.Derivatives.PriceChange24hLivePct = &w24
	g := computeHardEntryGate(weak, opt)
	if !hasCode(g.Long, "BTC_4H_DOWNTREND") || !hasCode(g.Long, "BTC_WEAK_LONG_") {
		t.Fatalf("declining BTC + weak coin must emit both codes, failed=%v", g.Long.Failed)
	}

	strongOpt := lpOpt()
	strongOpt.BtcTrendCloses = rising
	strong := lpSig(100, 110, 95)
	strong.Derivatives = &DerivSignal{}
	// rising series' own 24h return ≈ +12.7% — the coin must beat that
	s24 := 20.0
	strong.Derivatives.PriceChange24hLivePct = &s24
	gs := computeHardEntryGate(strong, strongOpt)
	if hasCode(gs.Long, "BTC_4H_DOWNTREND") || hasCode(gs.Long, "BTC_WEAK_LONG_") {
		t.Fatalf("rising BTC + strong coin must pass the BTC filter, failed=%v", gs.Long.Failed)
	}
}

func TestSentimentDeweightOnLongGateReads(t *testing.T) {
	// Directional score +45 with NEG_EDGE_MIN_SCORE 60: |45|<60 fires the
	// score code; under the greed deweight (−10) the code must read +35 —
	// the penalty is visible in the cited number.
	mk := func(pts int, armed bool) *DirectionGate {
		sig := lpSig(100, 101, 95)
		sig.SignalConflict = &SignalConflict{DirectionalScore: 45}
		opt := lpOpt()
		opt.NegativeEdge = true
		opt.NegativeEdgeMinScore = 60
		opt.SentimentLongDeweightPts = pts
		opt.SentimentLongDeweightArmed = armed
		g := computeHardEntryGate(sig, opt)
		return g.Long
	}
	if !hasCode(mk(0, false), "NEG_EDGE_SCORE_+45_LT_60") {
		t.Fatal("baseline: score +45 with cap 60 must cite +45")
	}
	g := mk(10, true)
	if !hasCode(g, "NEG_EDGE_SCORE_+35_LT_60") {
		t.Fatalf("greed deweight must read the long score as +35, failed=%v", g.Failed)
	}
}

func TestWideStopGate(t *testing.T) {
	// Craft a wide stop: entry 100, 1h ATR 2% (buffer), a deep support at 85
	// → the stop plan lands ≥13% below the entry, over the 10% cap.
	wide := &SymbolSignal{
		Symbol: "WIDEUSDT",
		Price:  100,
		Timeframes: map[string]*TFSignal{
			// 4h ATR 9% makes the band cap max(18,8)=18% — band-legal wide
			// stop, exactly the USUSDT shape the cap targets.
			"1h": {ATRPct: 2.0, Support: []float64{85}, StructuralSupport: []float64{85}},
			"4h": {ATRPct: 9.0},
		},
	}
	opt := lpOpt()
	opt.SLMinATRMult = 1.5 // stop-plan block runs only with a noise floor
	opt.MaxStopDistancePct = 10
	g := computeHardEntryGate(wide, opt)
	if !hasCode(g.Long, "WIDE_STOP_") {
		t.Fatalf("wide stop plan must emit WIDE_STOP, failed=%v stopPct=%v", g.Long.Failed, g.Long.StopPlanPct)
	}
	// Disabled (negative → resolver 0) → no code.
	off := opt
	off.MaxStopDistancePct = 0
	g2 := computeHardEntryGate(wide, off)
	if hasCode(g2.Long, "WIDE_STOP_") {
		t.Fatal("disabled cap must not emit WIDE_STOP")
	}
}
