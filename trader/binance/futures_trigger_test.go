package binance

import "testing"

// Algo-order trigger prices must round to the symbol's tick precision — a
// raw %.8f hit Binance -1111 "Precision is over the maximum defined for this
// asset" on UNIUSDT (tick 0.001, stop 8.86573) and the position ran
// unprotected (2026-09-20). The >1% drift guard exists because a mis-rounded
// trigger is a stop at the WRONG LEVEL: instant or never.
func TestFormatTriggerPrice(t *testing.T) {
	// UNIUSDT tick 0.001 — the live failure case.
	got, err := formatTriggerPrice(8.86573, 3)
	if err != nil || got != "8.866" {
		t.Fatalf("got %q err %v, want 8.866", got, err)
	}
	// Fine-tick symbol keeps its decimals.
	if got, _ := formatTriggerPrice(0.0242572, 6); got != "0.024257" {
		t.Fatalf("fine-tick rounding = %q, want 0.024257", got)
	}
	// Whole-number ticks (BTC-like 0.10 → 1dp).
	if got, _ := formatTriggerPrice(80655.9, 1); got != "80655.9" {
		t.Fatalf("btc-like = %q", got)
	}
	// Rounding drift >1% must be refused, not placed at a wrong level.
	if _, err := formatTriggerPrice(0.0242572, 2); err == nil {
		t.Fatal("2dp rounding of a 0.024 stop drifted >1% — must be refused")
	}
	// Absurd precision falls back to 8dp, negative to integer behavior of 8dp.
	if got, _ := formatTriggerPrice(8.86573, -1); got != "8.86573000" {
		t.Fatalf("fallback precision = %q", got)
	}
}
