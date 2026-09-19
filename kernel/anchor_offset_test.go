package kernel

import "testing"

// ProfitLockTargets: R-multiple = PnL% ÷ initial-stop-distance%; breakeven
// only while the stop still sits beyond entry; both actions arm at lockR.
func TestProfitLockTargets(t *testing.T) {
	// Long 100 entry, initial SL 98 → 1R = 2%. Mark 102 = +1R.
	// Mark at exactly 1R, stop still at the initial SL: both actions arm.
	be, trim := ProfitLockTargets("long", 100, 98, 98, 102, 1.0)
	if !be || !trim {
		t.Fatalf("long at 1R: be=%v trim=%v, want true/true", be, trim)
	}
	// Stop already moved to entry: breakeven must not re-fire, trim still due.
	be, trim = ProfitLockTargets("long", 100, 98, 100, 102, 1.0)
	if be || !trim {
		t.Fatalf("post-breakeven: be=%v trim=%v, want false/true", be, trim)
	}
	// Below 1R: nothing.
	be, trim = ProfitLockTargets("long", 100, 98, 98, 101.5, 1.0)
	if be || trim {
		t.Fatalf("0.75R must not arm: be=%v trim=%v", be, trim)
	}
	// Short: entry 100, initial SL 102 → 1R = 2%; mark 98 = +1R.
	be, trim = ProfitLockTargets("short", 100, 102, 102, 98, 1.0)
	if !be || !trim {
		t.Fatalf("short at 1R: be=%v trim=%v, want true/true", be, trim)
	}
	// Short already at breakeven (SL 100) → breakeven no, trim yes.
	be, trim = ProfitLockTargets("short", 100, 102, 100, 98, 1.0)
	if be || !trim {
		t.Fatalf("short post-breakeven: be=%v trim=%v", be, trim)
	}
	// Disabled lock: nothing arms.
	be, trim = ProfitLockTargets("long", 100, 98, 98, 105, 0)
	if be || trim {
		t.Fatal("lockR=0 disables")
	}
}
