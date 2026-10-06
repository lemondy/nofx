package kernel

import (
	"nofx/market"
	"testing"
	"time"
)

func TestAudit05UnknownRuleFieldsMustBeRejected(t *testing.T) {
	if _, err := ParseRuleCondition(`{"field":"funding_rate","op":">","value":0.03}`); err == nil {
		t.Error("unsupported funding_rate rule accepted; evaluator has no implementation")
	}
}

func TestFix05MinimumSizeIncludesRoundTripCost(t *testing.T) {
	now := time.Date(2026, 10, 5, 4, 0, 0, 0, time.UTC)
	market.SetSymbolLotFilterForTesting("FIXMINUSDT", market.SymbolLotFilter{StepSize: 0.001, MinQty: 0.001, MinNotional: 1})
	opts := SignalOptions{Now: now, PrimaryTF: "1h", EquityUSDT: 1, RiskPct: 1.5, MinPositionSizeUSDT: 10}
	data := gateTestMarket(now, 100)
	before, err := ComputeSymbolSignals("FIXMINUSDT", data, opts)
	if err != nil || before.MinSize == nil || !before.MinSize.Feasible {
		t.Fatalf("gross floor fixture: signal=%+v err=%v", before, err)
	}
	opts.EntryRoundTripCostBps = 20
	after, err := ComputeSymbolSignals("FIXMINUSDT", data, opts)
	if err != nil || after.MinSize == nil || after.MinSize.Feasible {
		t.Fatalf("minimum trade cost alone exceeds budget but stays feasible: signal=%+v err=%v", after, err)
	}
}

func TestFix05TargetMenuUsesNetRR(t *testing.T) {
	tfs := map[string]*TFSignal{"1h": {Resistance: []float64{103, 104}, Support: []float64{97, 96}}}
	long := scanRRWindowWithCost(100, "live", 2, 98, tfs, true, 1.5, 15*time.Minute, 4*time.Hour, 20)
	short := scanRRWindowWithCost(100, "live", 2, 102, tfs, false, 1.5, 15*time.Minute, 4*time.Hour, 20)
	if long.FirstRRGeTarget != 104 || short.FirstRRGeTarget != 96 {
		t.Fatalf("gross 1.5R target survived costs: long=%+v short=%+v", long, short)
	}
	if long.CostBps != 20 || long.FirstTargetRR < 1.5 {
		t.Fatal("cost model absent from qualifying target")
	}
}
