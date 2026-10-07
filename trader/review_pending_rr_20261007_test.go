package trader

import (
	"testing"

	"nofx/kernel"
	"nofx/market"
	"nofx/store"
)

// Incident 2026-10-07 STXUSDT 8641141308: placed long @0.392925 SL 0.38536
// TP 0.4136; the next cycle priced RR at the LIVE price (RR_MAX_0.71) and the
// recheck cancelled an order whose own plan is RR≈2.38 net of cost.
func stxPending() *pendingEntry {
	return &pendingEntry{Symbol: "STXUSDT", Side: "long", Price: 0.392925, StopLoss: 0.38536, TakeProfit: 0.4136, OrderID: "8641141308"}
}

func pendingRRTrader(minRR float64) *AutoTrader {
	at := riskTestTrader(store.RiskControlConfig{})
	cfg := fix7Config()
	cfg.RiskControl.MinRiskRewardRatio = minRR
	at.config.StrategyConfig = cfg
	return at
}

func TestPendingPlanNetRRMatchesGateFormula(t *testing.T) {
	rr, ok := pendingPlanNetRR("long", 0.392925, 0.38536, 0.4136, 20)
	if !ok || rr < 2.35 || rr > 2.41 {
		t.Fatalf("STX plan net RR = %.3f ok=%v, want ≈2.38", rr, ok)
	}
	// Short mirror: entry 100, stop 102, target 95 → (5-0.2)/(2+0.2)
	rr, ok = pendingPlanNetRR("short", 100, 102, 95, 20)
	if !ok || rr < 2.18 || rr > 2.19 {
		t.Fatalf("short net RR = %.4f ok=%v, want 2.1818", rr, ok)
	}
	// Incoherent plans never qualify.
	if _, ok := pendingPlanNetRR("long", 100, 101, 110, 20); ok {
		t.Fatal("long with stop above entry must be rejected")
	}
	if _, ok := pendingPlanNetRR("short", 100, 102, 103, 20); ok {
		t.Fatal("short with target above entry must be rejected")
	}
}

func TestPendingRROnlyBlockKeepsLimitWhosePlanClears(t *testing.T) {
	at := pendingRRTrader(1.2)
	if !at.pendingRROnlyBlockClears(stxPending(), []string{"RR_MAX_0.71"}) {
		t.Fatal("RR-only live-price block must not cancel a limit whose own plan clears min RR (STX incident)")
	}
}

func TestPendingRROnlyBlockStillCancelsOtherCodes(t *testing.T) {
	at := pendingRRTrader(1.2)
	for _, failed := range [][]string{
		{"RR_MAX_0.71", "MICRO_TREND_NOT_LONG"},
		{"MICRO_TREND_NOT_LONG"},
		{"NEG_EDGE_RR_0.90_LT_1.50"},
		nil,
	} {
		if at.pendingRROnlyBlockClears(stxPending(), failed) {
			t.Fatalf("failed=%v must still cancel", failed)
		}
	}
}

func TestPendingRROnlyBlockCancelsWhenPlanRRTooLow(t *testing.T) {
	at := pendingRRTrader(1.2)
	pe := stxPending()
	pe.TakeProfit = 0.3950 // plan RR at the limit ≈ 0.07
	if at.pendingRROnlyBlockClears(pe, []string{"RR_MAX_0.71"}) {
		t.Fatal("a plan that fails min RR at its own limit must still be cancelled")
	}
}

func TestPendingDirectionBlockedRespectsRROnlyExemption(t *testing.T) {
	at := pendingRRTrader(1.2)
	tfs := at.recheckConfiguredTimeframes("XUSDT")
	at.recheckDataFn = func(symbol string) (*market.Data, error) { return fix7StrategyData(tfs, true), nil }
	// fix7's healthy plan (entry 100 / SL 95 / TP 110): net RR ≈ 1.86 ≥ 1.2.
	pe := &pendingEntry{Symbol: "XUSDT", Side: "long", Price: 100, StopLoss: 95, TakeProfit: 110, OrderID: "entry"}

	at.cycleGateStates = map[string]*kernel.GateState{"XUSDT": {LongAllowed: false, LongFailed: []string{"RR_MAX_0.71"}}}
	if reason := at.pendingDirectionBlocked(pe); reason != "" {
		t.Fatalf("RR-only live-price block: want kept, got cancel reason %q", reason)
	}

	at.cycleGateStates = map[string]*kernel.GateState{"XUSDT": {LongAllowed: false, LongFailed: []string{"RR_MAX_0.71", "MICRO_TREND_NOT_LONG"}}}
	if reason := at.pendingDirectionBlocked(pe); reason != "pending direction gate disallowed" {
		t.Fatalf("structural block must still cancel, got %q", reason)
	}
}
