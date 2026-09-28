package binance

import (
	"strings"
	"testing"
)

func TestFormatQuantityWithRulesRejectsZeroAndRoundsDown(t *testing.T) {
	rules := symbolOrderRules{
		StepSizeText: "0.1",
		StepSize:     0.1,
		MinQty:       0.1,
		MaxQty:       1000,
		MinNotional:  10,
	}
	if _, err := formatQuantityWithRules("QNTUSDT", 0.03067, rules); err == nil || !strings.Contains(err.Error(), "MIN_QTY") {
		t.Fatalf("small quantity must fail locally with MIN_QTY, got %v", err)
	}
	if got, err := formatQuantityWithRules("QNTUSDT", 0.35, rules); err != nil || got != "0.3" {
		t.Fatalf("quantity must round down to risk-safe step: got %q, err=%v", got, err)
	}

	nonDecimalStep := symbolOrderRules{StepSizeText: "0.025", StepSize: 0.025, MinQty: 0.025}
	if got, err := formatQuantityWithRules("TESTUSDT", 0.089, nonDecimalStep); err != nil || got != "0.075" {
		t.Fatalf("non-decimal step must be quantized exactly: got %q, err=%v", got, err)
	}
}

func TestValidateOrderNotional(t *testing.T) {
	if err := validateOrderNotional("QNTUSDT", 0.1, 260.8, 10); err != nil {
		t.Fatalf("valid notional rejected: %v", err)
	}
	if err := validateOrderNotional("TESTUSDT", 0.1, 50, 10); err == nil || !strings.Contains(err.Error(), "MIN_NOTIONAL") {
		t.Fatalf("small notional must fail locally with MIN_NOTIONAL, got %v", err)
	}
}
