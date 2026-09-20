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

// Round-4 review R4-15: decimal-place rounding is only tick-correct for
// power-of-ten ticks. On a 0.025 tick, 3dp rounding emits 1.234 — NOT a
// multiple of 0.025 — and Binance rejects with -1111, leaving the position
// unprotected. quantizeToTick snaps to the tick grid instead.
func TestQuantizeToTick(t *testing.T) {
	// Non-power-of-ten tick: 1.234 → 49×0.025 = 1.225 (plain 3dp rounding
	// would have kept 1.234 and drawn -1111).
	if got, err := quantizeToTick(1.234, 0.025, 3); err != nil || got != "1.225" {
		t.Fatalf("0.025-tick quantize = %q err %v, want 1.225", got, err)
	}
	// Power-of-ten parity with the legacy formatter (UNIUSDT live case).
	if got, err := quantizeToTick(8.86573, 0.001, 3); err != nil || got != "8.866" {
		t.Fatalf("0.001-tick quantize = %q err %v, want 8.866", got, err)
	}
	// Coarse tick still respects the >1% drift guard: rounding 4.4 onto a
	// 10-tick grid would emit 0 — a wrong level, refuse.
	if _, err := quantizeToTick(4.4, 10, 0); err == nil {
		t.Fatal("drift guard must refuse a coarse-tick rounding that moves the trigger >1%")
	}
	// Degenerate tick falls back to plain 8dp formatting rather than panic.
	if got, _ := quantizeToTick(8.86573, 0, 3); got != "8.86573000" {
		t.Fatalf("degenerate tick fallback = %q", got)
	}
}
