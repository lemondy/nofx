package trader

import "testing"

func nearly(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

// Vol target: notional = equity × risk% ÷ ATR%.
func TestVolTargetNotional(t *testing.T) {
	// equity 80, risk 1.5%, ATR 1.5% → 1× equity = 80.
	if got := volTargetNotional(80, 1.5, 1.5); !nearly(got, 80) {
		t.Fatalf("got %.2f, want 80", got)
	}
	// Vol doubles → position halves.
	if got := volTargetNotional(80, 1.5, 3.0); !nearly(got, 40) {
		t.Fatalf("got %.2f, want 40", got)
	}
	if got := volTargetNotional(80, 1.5, 0); got != 0 {
		t.Fatalf("ATR 0 must yield 0, got %.2f", got)
	}
}

// Hysteresis band: [80%,120%] → hold; outside → act.
func TestResizeActionBand(t *testing.T) {
	target := 100.0
	cases := map[float64]string{
		100: "none", 80: "none", 120: "none", 80.5: "none", 119: "none",
		121: "reduce", 200: "reduce", 79: "underweight", 50: "underweight",
	}
	for actual, want := range cases {
		if got := resizeAction(actual, target); got != want {
			t.Errorf("resizeAction(%v) = %q, want %q", actual, got, want)
		}
	}
}

// Trailing: arms at 1.5× initial stop distance, trails 2×ATR, tighten-only.
func TestTrailingDecision(t *testing.T) {
	// Long: entry 100, initial SL 98 (2% dist). Trail arms at +3% (103).
	entry, initialSL, atr := 100.0, 98.0, 1.0 // ATR 1%
	// Price 102: profit 2% < 3% → not armed.
	if _, move, armed := trailingDecision("long", entry, initialSL, 102, atr, initialSL); armed || move {
		t.Fatal("must not arm below 1.5× stop distance")
	}
	// Price 103.5: armed; candidate = 103.5 − 2×(1%×103.5) = 101.43 > 98 → move.
	newSL, move, armed := trailingDecision("long", entry, initialSL, 103.5, atr, initialSL)
	if !armed || !move || !nearly(newSL, 101.43) {
		t.Fatalf("armed trail wrong: newSL=%.2f move=%v armed=%v", newSL, move, armed)
	}
	// Monotonic: a candidate below the current SL never widens it.
	newSL, move, _ = trailingDecision("long", entry, initialSL, 103.6, atr, 103.0)
	if move || newSL != 103.0 {
		t.Fatalf("trail must never widen: newSL=%.2f move=%v", newSL, move)
	}
	// Short mirror: entry 100, initial SL 102, price 96.5 (profit 3.5% armed).
	// candidate = 96.5 + 2×(1%×96.5) = 98.43 < 102 → move down.
	newSL, move, armed = trailingDecision("short", entry, 102.0, 96.5, atr, 102.0)
	if !armed || !move || !nearly(newSL, 98.43) {
		t.Fatalf("short trail wrong: newSL=%.2f move=%v armed=%v", newSL, move, armed)
	}
	// Short monotonic: candidate above current SL never widens.
	newSL, move, _ = trailingDecision("short", entry, 102.0, 96.4, atr, 98.0)
	if move || newSL != 98.0 {
		t.Fatalf("short trail must never widen: newSL=%.2f move=%v", newSL, move)
	}
}

// TP runner: fires when the favorable move reaches the planned TP distance.
func TestTPRunnerDue(t *testing.T) {
	if !tpRunnerDue("long", 100, 106, 106.2) {
		t.Fatal("long at TP distance must fire")
	}
	if tpRunnerDue("long", 100, 106, 105.9) {
		t.Fatal("long below TP distance must not fire")
	}
	if !tpRunnerDue("short", 100, 94, 93.8) {
		t.Fatal("short at TP distance must fire")
	}
	if tpRunnerDue("long", 100, 0, 110) {
		t.Fatal("no TP → never fires")
	}
}
