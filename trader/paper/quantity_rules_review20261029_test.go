package paper

import "testing"

// The trader package discovers lot rules through this exact method set
// (quantityStepOf in trader/auto_trader_orders.go). Without it the split-TP
// size was never quantized and a sub-step tail fell back to closePosition.
var _ interface {
	QuantityRules(string) (float64, float64, error)
} = (*PaperFuturesTrader)(nil)

func TestReview20261029PaperQuantityRulesExposeLotStep(t *testing.T) {
	p, _, f := setup(t, Config{})
	f.rule = QuantityRule{StepSize: "0.01", MinQty: .01, MinNotional: 5}
	step, minQty, err := p.QuantityRules("SOXLUSDT")
	if err != nil || step != .01 || minQty != .01 {
		t.Fatalf("QuantityRules = %v, %v, %v; want 0.01, 0.01, nil", step, minQty, err)
	}
	f.rule = QuantityRule{StepSize: ""}
	if _, _, err := p.QuantityRules("SOXLUSDT"); err == nil {
		t.Fatal("missing LOT_SIZE step must be an error, not a zero step")
	}
}
