package kernel

import (
	"strings"
	"testing"

	"nofx/store"
)

// ============================================================================
// NEGATIVE_EDGE health gate (user review 2026-09-27 #4): "证据极强/RR 明显
// 占优" had no numbers, so open and wait were BOTH arguable from the same
// prompt — BRUSDT 09-27 passed the plain min_rr gate at directional_score
// −50 with net-losing history and a 1.84R first target. The gate now lands
// per-condition codes in hard_entry_gate.failed; these tests pin the codes,
// the resolver defaults and the FirstTargetRR plumbing.
// ============================================================================

func negEdgeSignal(score int, hist *TraderHistoryStat, fastLong bool) *SymbolSignal {
	fast, slow := 101.0, 100.0
	if !fastLong {
		fast, slow = 99.0, 100.0
	}
	return &SymbolSignal{
		Price:          100,
		LimitBuyPrice:  99.5,
		LimitSellPrice: 100.5,
		SignalConflict: &SignalConflict{DirectionalScore: score},
		Timeframes: map[string]*TFSignal{
			"1h": {EMAFast: &fast, EMASlow: &slow},
			"4h": {EMAFast: &fast, EMASlow: &slow},
		},
		TraderHistory: hist,
		DataQuality:   &DataQuality{Complete: true, Sufficient: true},
	}
}

func hasCodePrefix(list []string, prefix string) bool {
	for _, c := range list {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestNegativeEdgeGateCodes(t *testing.T) {
	losing := &TraderHistoryStat{ClosedTrades: 5, Wins: 2, WinRatePct: 40, RealizedPnL: -3.29}
	opt := SignalOptions{NegativeEdge: true, NegativeEdgeMinScore: 80, NegativeEdgeBlockLosing: true}

	// BRUSDT shape: weak consensus + aligned EMAs + net-losing symbol
	// (RR condition off here — covered separately).
	g := computeHardEntryGate(negEdgeSignal(-50, losing, true), opt)
	if !hasCodePrefix(g.Long.Failed, "NEG_EDGE_SCORE_-50") {
		t.Fatalf("long gate missing NEG_EDGE_SCORE code: %v", g.Long.Failed)
	}
	if hasCodePrefix(g.Long.Failed, "NEG_EDGE_TREND_MISALIGNED") {
		t.Fatalf("aligned EMAs must not be flagged: %v", g.Long.Failed)
	}
	if !hasCodePrefix(g.Long.Failed, "NEG_EDGE_LOSING_SYMBOL") {
		t.Fatalf("net-losing symbol must be flagged: %v", g.Long.Failed)
	}
	if g.Long.Allowed {
		t.Fatalf("BRUSDT-shaped long must be blocked: %v", g.Long.Failed)
	}

	// Mirror check: a short with the SAME weak score against SHORT-side
	// aligned EMAs (fast<slow on both TFs) is blocked on score/history only.
	gs := computeHardEntryGate(negEdgeSignal(-50, nil, false), opt)
	if !hasCodePrefix(gs.Short.Failed, "NEG_EDGE_SCORE_-50") || hasCodePrefix(gs.Short.Failed, "NEG_EDGE_TREND_MISALIGNED") {
		t.Fatalf("short verdict wrong: %v", gs.Short.Failed)
	}

	// Counter-trend direction: EMAs long-side, trade short → misaligned.
	gc := computeHardEntryGate(negEdgeSignal(-90, nil, true), opt)
	if !hasCodePrefix(gc.Short.Failed, "NEG_EDGE_TREND_MISALIGNED") {
		t.Fatalf("counter-trend short must be misaligned: %v", gc.Short.Failed)
	}

	// Strong + aligned + clean history + thresholds met → no NEG_EDGE codes.
	gOK := computeHardEntryGate(negEdgeSignal(90, nil, true), opt)
	for _, c := range gOK.Long.Failed {
		if strings.HasPrefix(c, "NEG_EDGE") {
			t.Fatalf("strong setup must pass the health gate, got %v", gOK.Long.Failed)
		}
	}
	if !gOK.Long.Allowed {
		t.Fatalf("strong setup must be allowed in this fixture: %v", gOK.Long.Failed)
	}

	// RR condition: RR unknown/nil counts as below the floor (fail-closed).
	optRR := opt
	optRR.NegativeEdgeMinRR = 2
	gRR := computeHardEntryGate(negEdgeSignal(90, nil, true), optRR)
	if !hasCodePrefix(gRR.Long.Failed, "NEG_EDGE_RR_") {
		t.Fatalf("nil rr_scan must fail the RR condition: %v", gRR.Long.Failed)
	}

	// Gate disabled → zero NEG_EDGE codes even for the worst shape.
	optOff := opt
	optOff.NegativeEdge = false
	gOff := computeHardEntryGate(negEdgeSignal(-50, losing, true), optOff)
	for _, c := range gOff.Long.Failed {
		if strings.HasPrefix(c, "NEG_EDGE") {
			t.Fatalf("disabled gate must not emit codes: %v", gOff.Long.Failed)
		}
	}
}

// FirstTargetRR carries the RR AT the adopted TP — the health gate's
// "first_target_rr ≥ threshold" input (a 1.84R min-qualifying target must
// stay visible as below 2.0).
func TestScanRRFirstTargetRR(t *testing.T) {
	tfs := map[string]*TFSignal{"15m": {Resistance: []float64{103.5, 110}}}
	scan := scanRR(100, "limit_anchor", 2, 98, tfs, true, 1.5)
	if !scan.Usable || scan.FirstRRGeTarget != 103.5 {
		t.Fatalf("scan = usable %v first %g, want true/103.5", scan.Usable, scan.FirstRRGeTarget)
	}
	want := round2((103.5 - 100) / 100 * 100 / 2) // 1.75 — below a 2.0 health floor
	if scan.FirstTargetRR != want {
		t.Fatalf("first_target_rr = %.2f, want %.2f", scan.FirstTargetRR, want)
	}
}

// Resolver pins: defaults 80/2.0 with gate+losing-symbol ON; negative values
// disable individual conditions; gate switch off disables everything.
func TestNegativeEdgeResolvers(t *testing.T) {
	if !NegativeEdgeGateEnabled(nil) {
		t.Fatal("nil config = gate enabled")
	}
	off := false
	if NegativeEdgeGateEnabled(&store.RiskControlConfig{NegativeEdgeGate: &off}) {
		t.Fatal("explicit false must disable the gate")
	}
	if v := NegativeEdgeMinScore(nil); v != 80 {
		t.Fatalf("default min score = %.0f, want 80", v)
	}
	if v := NegativeEdgeMinRR(nil); v != 2 {
		t.Fatalf("default min rr = %.0f, want 2", v)
	}
	rc := &store.RiskControlConfig{NegativeEdgeMinScore: -1, NegativeEdgeMinRR: -1}
	if v := NegativeEdgeMinScore(rc); v != 0 {
		t.Fatalf("negative min score must disable the condition, got %.0f", v)
	}
	if v := NegativeEdgeMinRR(rc); v != 0 {
		t.Fatalf("negative min rr must disable the condition, got %.0f", v)
	}
	rc2 := &store.RiskControlConfig{NegativeEdgeMinScore: 65, NegativeEdgeMinRR: 2.5}
	if v := NegativeEdgeMinScore(rc2); v != 65 {
		t.Fatalf("explicit min score = %.0f, want 65", v)
	}
	if v := NegativeEdgeMinRR(rc2); v != 2.5 {
		t.Fatalf("explicit min rr = %.1f, want 2.5", v)
	}
	if !NegativeEdgeBlockLosingSymbol(nil) {
		t.Fatal("nil config = losing-symbol block on")
	}
}

// StrategyHealthEdge: the single PF-regime definition the stats line and the
// gate share.
func TestStrategyHealthEdge(t *testing.T) {
	cases := map[float64]string{0.5: "NEGATIVE_EDGE", 0.9: "NO_EDGE", 1.05: "NO_EDGE", 1.1: "POSITIVE_EDGE", 2.0: "POSITIVE_EDGE"}
	for pf, want := range cases {
		if got := StrategyHealthEdge(pf); got != want {
			t.Errorf("StrategyHealthEdge(%.2f) = %s, want %s", pf, got, want)
		}
	}
}
