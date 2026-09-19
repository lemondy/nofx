package kernel

import (
	"math"
	"strings"
	"testing"

	"nofx/store"
)

// Regression for the 09-19 RR audit: the gate scored RR at the noise-floor
// stop (1.5×ATR(1h)) while actual orders stopped at structure+buffer —
// systematically wider — so "1.5R" trades executed at 1.3-1.4R. The plan,
// the scan and the executor's checkRR must all price the METHODOLOGY stop.
//
// Fixture = the audited KORUSDT short (09-18 15:31 snapshot): live 19.28,
// ATR(1h) 1.65% (floor 2.475%), 15m resistance 19.78, 4h resistance 19.77,
// supports 18.38/18.29 (4h), noise-scale 5m levels excluded from the stop
// anchor. The user's hand math: structure stop ≈ 19.91-19.94 (d≈3.3-3.4%),
// at which 18.38 gives RR 1.39-1.40 — BELOW the 1.5 gate the old code passed.
func koruSignal() *SymbolSignal {
	return &SymbolSignal{
		Price: 19.28,
		Timeframes: map[string]*TFSignal{
			"15m": {ATRPct: 0.92, Resistance: []float64{19.78, 20.0009}, Support: []float64{19.084, 19.07}},
			"1h":  {ATRPct: 1.65, Resistance: []float64{20.29, 20.61}, Support: []float64{19.178}},
			"4h":  {ATRPct: 3.48, Resistance: []float64{19.77, 20.09}, Support: []float64{18.38, 18.29}},
			"5m":  {ATRPct: 0.64, Resistance: []float64{19.44, 19.473}, Support: []float64{19.26, 19.123}}, // noise scale — must NOT anchor the stop
		},
		ExecutionFilter: &ExecutionFilter{MicroTF: "15m", MicroTrend: "down", LongAllowed: false, ShortAllowed: true},
		DataQuality:     &DataQuality{Complete: true, Sufficient: true},
	}
}

func koruOpt() SignalOptions {
	return SignalOptions{SLMinATRMult: 1.5, MinRR: 1.5}
}

func TestStopPlanKORUShort(t *testing.T) {
	g := computeHardEntryGate(koruSignal(), koruOpt())
	s := g.Short

	// Nearest ≥15m resistance above entry = 19.77 (4h), + 0.5×1.65% buffer.
	wantStop := 19.77 + 0.5*1.65/100*19.28
	if math.Abs(s.StopPlanPrice-wantStop) > 1e-6 {
		t.Errorf("stop_plan_price = %.4f, want %.4f", s.StopPlanPrice, wantStop)
	}
	wantDist := (wantStop - 19.28) / 19.28 * 100 // ≈ 3.37% — the user's 3.3-3.4%
	if math.Abs(s.StopPlanPct-wantDist) > 0.01 {
		t.Errorf("stop_plan_pct = %.2f, want ≈%.2f", s.StopPlanPct, wantDist)
	}
	if s.RR == nil {
		t.Fatal("rr_scan missing")
	}
	if s.RR.StopPrice != s.StopPlanPrice {
		t.Errorf("rr_scan.stop_price %.4f ≠ stop_plan_price %.4f — gate and plan must price the same stop",
			s.RR.StopPrice, s.StopPlanPrice)
	}

	// The heart of the audit: at the OLD floor stop (2.475%) 18.38 scored
	// RR 1.89 and was adopted; at the REAL stop it is 1.39 and must NOT
	// qualify. The nearest genuine 1.5R target shifts one level out to 18.29.
	rr1838 := (19.28 - 18.38) / 19.28 * 100 / s.RR.StopDistancePct
	if rr1838 >= 1.5 {
		t.Fatalf("fixture drift: 18.38 RR = %.2f should sit below 1.5 at the methodology stop", rr1838)
	}
	if s.RR.FirstRRGeTarget != 18.29 {
		t.Errorf("first_rr_ge_target = %g, want 18.29 (18.38 only cleared the gate at the noise-floor stop)", s.RR.FirstRRGeTarget)
	}
	if !s.RR.Usable || s.RR.BestRR < 1.5 {
		t.Errorf("usable=%v best_rr=%.2f, want a genuine ≥1.5 target at the methodology stop", s.RR.Usable, s.RR.BestRR)
	}
	// The 5m pivot (19.44) must not have anchored the stop: its plan would
	// clamp back to the floor and reproduce the audited distortion.
	if s.StopPlanPct < 3.0 {
		t.Errorf("stop_plan_pct = %.2f — the 5m noise pivot leaked into the stop anchor", s.StopPlanPct)
	}
	if !s.Allowed {
		t.Errorf("short must pass with the shifted target: failed=%v", s.Failed)
	}
}

func TestStopPlanNoStructureAndOutOfBand(t *testing.T) {
	// New-highs short: every resistance sits BELOW the live price — no
	// structural stop exists → STOP_PLAN_NO_STRUCTURE, direction dead.
	sig := koruSignal()
	sig.Price = 21.5 // above every resistance
	g := computeHardEntryGate(sig, koruOpt())
	if !has(g.Short.Failed, "STOP_PLAN_NO_STRUCTURE") {
		t.Errorf("short failed = %v, want STOP_PLAN_NO_STRUCTURE", g.Short.Failed)
	}
	if g.Short.RR != nil {
		t.Errorf("rr_scan must be absent without a stop plan, got %+v", g.Short.RR)
	}

	// Structure stop beyond the band cap (max(2×ATR(4h), 8%) = 8% here):
	// a far resistance makes the plan distance overshoot → OUT_OF_BAND.
	sig2 := koruSignal()
	sig2.Price = 19.0
	sig2.Timeframes["15m"] = &TFSignal{ATRPct: 0.92, Resistance: []float64{20.9}, Support: []float64{18.5}}
	sig2.Timeframes["1h"] = &TFSignal{ATRPct: 1.65, Support: []float64{18.6}}         // no stop-side structure closer than 20.9
	sig2.Timeframes["4h"] = &TFSignal{ATRPct: 3.48, Support: []float64{18.38, 18.29}} // cap = max(6.96, 8) = 8
	g2 := computeHardEntryGate(sig2, koruOpt())
	// stop = 20.9 + 0.157 → dist ≈ 10.8% > 8% cap.
	if !has(g2.Short.Failed, "STOP_PLAN_OUT_OF_BAND") {
		t.Errorf("short failed = %v, want STOP_PLAN_OUT_OF_BAND", g2.Short.Failed)
	}
}

func TestStopPlanStepOutRejectsNoMansLand(t *testing.T) {
	// 09-19 audit 二: a too-tight structure must STEP OUT to the next one —
	// never clamp up to the floor (the old code manufactured a 94.00 stop in
	// no-man's-land between 99.5 and nothing, scoring RR off a fantasy stop).
	sig := &SymbolSignal{
		Price: 100.0,
		Timeframes: map[string]*TFSignal{
			"1h":  {ATRPct: 2.0, Support: []float64{99.5}, Resistance: []float64{102}},
			"15m": {ATRPct: 0.8, Support: []float64{99.7}},
		},
		DataQuality: &DataQuality{Complete: true, Sufficient: true},
	}
	// Floor 6%: both supports sit 1.1-1.4% below — no structural stop lands
	// in band → OUT_OF_BAND (放弃), never a clamped 94.00/6.00%.
	if _, _, code := methodStopPlan(sig, 100.0, 6.0, true); code != "STOP_PLAN_OUT_OF_BAND" {
		t.Errorf("code = %q, want STOP_PLAN_OUT_OF_BAND (no in-band structure)", code)
	}
	// Wide-enough floor: the NEAREST structure wins, buffer on the 0.4 end.
	price, dist, code2 := methodStopPlan(sig, 100.0, 1.0, true)
	if code2 != "" || math.Abs(price-98.9) > 1e-9 || math.Abs(dist-1.1) > 1e-9 {
		t.Errorf("plan = %.4f/%.2f%% code=%q, want 98.90/1.10%% (nearest support 99.7 − 0.4×2%%)", price, dist, code2)
	}
}

// The prompt must teach ADOPTION of the precomputed stop (the reversal of
// the old "严禁照抄为 stop_loss" contract) and the executor-parity promise.
func TestStopPlanPromptWording(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.SLMinATRMult = 1.5
	engine := NewStrategyEngine(cfg)
	sp := engine.BuildSystemPrompt(100, "")
	for _, want := range []string{
		"止损(程序预计算,逐字采用)",
		"stop_plan_price 就是按方法论算好的止损",
		"空 0.5×ATR(1h)/多 0.4×ATR(1h)",
		"STOP_PLAN_NO_STRUCTURE", "STOP_PLAN_OUT_OF_BAND",
		"stop_plan_price + first_rr_ge_target",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	// The old noise-floor-scan contract must be gone.
	for _, gone := range []string{
		"严禁照抄为 stop_loss", "通常比噪声下限更宽", "d ≥ 1.5×ATR(1h)(噪声下限)且 d ≤ 上限",
	} {
		if strings.Contains(sp, gone) {
			t.Errorf("obsolete floor-scan wording still present: %q", gone)
		}
	}
}

// TestStopPlanZECStepOut — the audited ZEC long (09-19): the first cut
// clamped the too-close 15m supports (d 1.01%/1.27% < floor 3.16%) up to the
// floor, printing stop_plan_price 1503.59 — a no-man's-land stop between
// structures 1545.95 and 1475.71 that scored RR 1.95 where the real
// methodology stop scores 1.06. Step-out must land on the 1h structure.
func TestStopPlanZECStepOut(t *testing.T) {
	sig := &SymbolSignal{
		Price: 1552.64,
		Timeframes: map[string]*TFSignal{
			"15m": {ATRPct: 0.92, Support: []float64{1550.03, 1545.95}, Resistance: []float64{1648.08}},
			"1h":  {ATRPct: 2.11, Support: []float64{1475.71}},
			"4h":  {ATRPct: 4.41}, // cap = 2×4.41 = 8.82%
		},
		DataQuality: &DataQuality{Complete: true, Sufficient: true},
	}
	entry := 1552.64
	floor := 1.5 * 2.11 // 3.165%

	price, dist, code := methodStopPlan(sig, entry, floor, true)
	if code != "" {
		t.Fatalf("code = %q, want ok (the 1h structure 1475.71 lands in band)", code)
	}
	wantStop := 1475.71 - 0.4*2.11/100*entry // ≈ 1462.6
	if math.Abs(price-wantStop) > 1e-6 {
		t.Errorf("stop_plan_price = %.2f, want %.2f (1h structure − 0.4×ATR buffer)", price, wantStop)
	}
	if math.Abs(dist-5.79) > 0.05 {
		t.Errorf("stop_plan_pct = %.2f, want ≈5.79 (user's hand math)", dist)
	}
	if price >= 1545.95 {
		t.Errorf("plan %.2f must sit BELOW the second structure — no clamp no-man's-land", price)
	}
	if old := entry * (1 - 0.0316); math.Abs(price-old) < 1 {
		t.Errorf("plan reproduced the clamped fantasy stop %.2f", old)
	}

	// Gate level: at the REAL stop the 1648.08 target is RR 1.06 — the
	// direction must now be RR-blocked, which the clamp had hidden behind a
	// fake 1.95.
	g := computeHardEntryGate(sig, SignalOptions{SLMinATRMult: 1.5, MinRR: 1.5})
	l := g.Long
	if l.Allowed || !has(l.Failed, "RR_MAX_") {
		t.Errorf("long allowed=%v failed=%v, want RR_MAX block at the real stop", l.Allowed, l.Failed)
	}
	if l.RR == nil || math.Abs(l.RR.BestRR-1.06) > 0.02 {
		t.Errorf("best_rr = %v, want ≈1.06 (TP 1648.08 at the 5.79%% stop)", l.RR)
	}
	if l.StopPlanSource != "structure" {
		t.Errorf("stop_plan_source = %q, want structure", l.StopPlanSource)
	}
}

// The ZEC rejection (09-19 06:42 UTC): the model echoed its own 2.13% stop
// against the gated plan and the executor's band check rejected the whole
// output. The plan IS the trade — post-parse, the stop snaps to it (same
// contract as the limit-anchor compliance).
func TestCorrectStopLossToPlan(t *testing.T) {
	gates := map[string]*GateState{
		"ZECUSDT":    {LongStopPlanPrice: 1462.6, ShortStopPlanPrice: 1601.9},
		"NOPLANUSDT": {}, // gate blocked with STOP_PLAN_* → nothing to snap to
	}
	decs := []Decision{
		{Symbol: "ZECUSDT", Action: "open_long_limit", StopLoss: 1508.57}, // model's own math, 3% off
		{Symbol: "ZECUSDT", Action: "open_short", StopLoss: 1602.5},       // within 0.05% echo tolerance
		{Symbol: "ZECUSDT", Action: "open_long", StopLoss: 0},             // placeholder zero
		{Symbol: "NOPLANUSDT", Action: "open_long", StopLoss: 100.0},      // no plan — untouched
		{Symbol: "ETHUSDT", Action: "hold", StopLoss: 999.0},              // not an open — untouched
	}
	correctStopLossToPlan(decs, gates, StopPlanTolerancePct)

	if decs[0].StopLoss != 1462.6 {
		t.Errorf("drifting stop not snapped: %.4f, want 1462.6", decs[0].StopLoss)
	}
	if decs[1].StopLoss != 1602.5 {
		t.Errorf("echo-tolerance stop was modified: %.4f", decs[1].StopLoss)
	}
	if decs[2].StopLoss != 1462.6 {
		t.Errorf("placeholder zero not filled from plan: %.4f", decs[2].StopLoss)
	}
	if decs[3].StopLoss != 100.0 {
		t.Errorf("no-plan symbol must pass through, got %.4f", decs[3].StopLoss)
	}
	if decs[4].StopLoss != 999.0 {
		t.Errorf("non-open action must pass through, got %.4f", decs[4].StopLoss)
	}
}
