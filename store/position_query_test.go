package store

import (
	"math"
	"testing"
)

// The max drawdown must be measured against the trader's REAL capital base.
// Regression: a hardcoded 10000 base shrank a ~9% account drawdown to 0.2%.
func TestMaxDrawdownUsesRealCapitalBase(t *testing.T) {
	// 82 USDT account: +2, then -5, then +1 → equity 82→84→79→80.
	pnls := []float64{2, -5, 1}
	got := calculateMaxDrawdownFromPnls(pnls, 82)
	want := (84.0 - 79.0) / 84.0 * 100 // 5.95%
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("max drawdown = %.4f%%, want %.4f%%", got, want)
	}

	// Unknown base falls back to 100 USDT (not the old 10k).
	got = calculateMaxDrawdownFromPnls(pnls, 0)
	want = (102.0 - 97.0) / 102.0 * 100 // 4.90%
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("fallback drawdown = %.4f%%, want %.4f%%", got, want)
	}

	// A >100% relative drawdown can never happen against a positive base.
	got = calculateMaxDrawdownFromPnls([]float64{-1}, 100)
	if got > 100 || got < 0 {
		t.Fatalf("drawdown out of range: %.2f%%", got)
	}
}
