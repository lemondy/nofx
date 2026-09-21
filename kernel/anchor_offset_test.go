package kernel

import (
	"math"
	"testing"

	"nofx/store"
)

// ProfitLockTargets: R-multiple = PnL% ÷ initial-stop-distance%; the stop
// move (bePrice > 0) only while the stop still sits beyond the breakeven
// level; both actions arm at lockR.
func TestProfitLockTargets(t *testing.T) {
	// Long 100 entry, initial SL 98 → 1R = 2%. Mark 102 = +1R.
	// Mark at exactly 1R, stop still at the initial SL: both actions arm.
	// Offset 0 = classic breakeven at entry.
	be, trim := ProfitLockTargets("long", 100, 98, 98, 102, 1.0, 0)
	if be != 100 || !trim {
		t.Fatalf("long at 1R: be=%v trim=%v, want 100/true", be, trim)
	}
	// Stop already moved to entry: breakeven must not re-fire, trim still due.
	be, trim = ProfitLockTargets("long", 100, 98, 100, 102, 1.0, 0)
	if be != 0 || !trim {
		t.Fatalf("post-breakeven: be=%v trim=%v, want 0/true", be, trim)
	}
	// Below 1R: nothing.
	be, trim = ProfitLockTargets("long", 100, 98, 98, 101.5, 1.0, 0)
	if be != 0 || trim {
		t.Fatalf("0.75R must not arm: be=%v trim=%v", be, trim)
	}
	// Short: entry 100, initial SL 102 → 1R = 2%; mark 98 = +1R.
	be, trim = ProfitLockTargets("short", 100, 102, 102, 98, 1.0, 0)
	if be != 100 || !trim {
		t.Fatalf("short at 1R: be=%v trim=%v, want 100/true", be, trim)
	}
	// Short already at breakeven (SL 100) → breakeven no, trim yes.
	be, trim = ProfitLockTargets("short", 100, 102, 100, 98, 1.0, 0)
	if be != 0 || !trim {
		t.Fatalf("short post-breakeven: be=%v trim=%v", be, trim)
	}
	// Disabled lock: nothing arms.
	be, trim = ProfitLockTargets("long", 100, 98, 98, 105, 0, 0)
	if be != 0 || trim {
		t.Fatal("lockR=0 disables")
	}
}

// 09-21 user experiment: the lock parks the stop PAST entry (entry ±
// beOffsetR × opening-risk distance) so a post-lock pullback can't scratch
// the runner back to flat. Idempotent exactly like the classic breakeven.
func TestProfitLockTargetsBreakevenOffset(t *testing.T) {
	// Long 100 entry, initial SL 98 (R = 2 price points), offset 0.2 →
	// breakeven level = 100.4; arms once, then the moved stop silences it.
	be, trim := ProfitLockTargets("long", 100, 98, 98, 102, 1.0, 0.2)
	if math.Abs(be-100.4) > 1e-9 || !trim {
		t.Fatalf("long offset: be=%v trim=%v, want 100.4/true", be, trim)
	}
	be, trim = ProfitLockTargets("long", 100, 98, 100.4, 102, 1.0, 0.2)
	if be != 0 || !trim {
		t.Fatalf("long offset idempotent: be=%v trim=%v, want 0/true", be, trim)
	}
	// A trailing stop already ABOVE the offset level must not be pulled back.
	be, trim = ProfitLockTargets("long", 100, 98, 101, 102, 1.0, 0.2)
	if be != 0 || !trim {
		t.Fatalf("tighter live stop must win: be=%v trim=%v", be, trim)
	}
	// Short mirror: entry 100, initial SL 102, offset 0.2 → 99.6.
	be, trim = ProfitLockTargets("short", 100, 102, 102, 98, 1.0, 0.2)
	if math.Abs(be-99.6) > 1e-9 || !trim {
		t.Fatalf("short offset: be=%v trim=%v, want 99.6/true", be, trim)
	}
}

// Resolvers for the 09-21 experiment knobs: 0 = experiment default,
// negative = legacy behavior, sanity clamps on the fraction.
func TestProfitLockAndTPFractionResolvers(t *testing.T) {
	if ProfitLockBreakevenOffsetR(nil) != 0.2 {
		t.Fatal("nil config → default 0.2R offset")
	}
	rc := &store.RiskControlConfig{}
	if ProfitLockBreakevenOffsetR(rc) != 0.2 {
		t.Fatal("unset → default 0.2R offset")
	}
	rc.ProfitLockBEOffsetR = -1
	if ProfitLockBreakevenOffsetR(rc) != 0 {
		t.Fatal("negative → pure breakeven (0)")
	}
	rc.ProfitLockBEOffsetR = 0.3
	if ProfitLockBreakevenOffsetR(rc) != 0.3 {
		t.Fatal("explicit value must pass through")
	}

	if TPCloseFraction(nil) != 0.5 {
		t.Fatal("nil config → default 0.5 split")
	}
	rc = &store.RiskControlConfig{}
	if TPCloseFraction(rc) != 0.5 {
		t.Fatal("unset → default 0.5 split")
	}
	rc.TPCloseFraction = -1
	if TPCloseFraction(rc) != 1.0 {
		t.Fatal("negative → legacy full close")
	}
	rc.TPCloseFraction = 0.3
	if TPCloseFraction(rc) != 0.3 {
		t.Fatal("explicit fraction must pass through")
	}
	rc.TPCloseFraction = 0.001
	if TPCloseFraction(rc) != 0.05 {
		t.Fatal("fraction clamps up to 0.05")
	}
	rc.TPCloseFraction = 1.7
	if TPCloseFraction(rc) != 1.0 {
		t.Fatal("fraction clamps down to 1.0")
	}
}
