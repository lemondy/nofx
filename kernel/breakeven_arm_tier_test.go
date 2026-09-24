package kernel

import (
	"testing"

	"nofx/store"
)

// Pins for breakeven_arm_r (user 2026-09-24, the (0,1R) give-back tier):
// 0/negative = disabled; >0 arms via ProfitLockTargets at that R multiple
// with the shared +offset BE price — no trim involvement.
func TestBreakevenArmR(t *testing.T) {
	if got := BreakevenArmR(&store.RiskControlConfig{}); got != 0 {
		t.Fatalf("unset = %.2f, want 0 (disabled by default)", got)
	}
	if got := BreakevenArmR(&store.RiskControlConfig{BreakevenArmR: -1}); got != 0 {
		t.Fatalf("negative = %.2f, want 0 (disabled)", got)
	}
	if got := BreakevenArmR(&store.RiskControlConfig{BreakevenArmR: 0.5}); got != 0.5 {
		t.Fatalf("0.5 = %.2f", got)
	}

	// Arming semantics: at 0.5R the tier returns the +0.2R BE price; below
	// it returns nothing. Short side mirrored. Live stop already at/beyond
	// BE → no re-arm (the SOLUSDT/UNIUSDT one-shot requirement).
	rc := &store.RiskControlConfig{BreakevenArmR: 0.5}
	entry, isl := 100.0, 96.0 // risk 4, BE = 100 + 0.2*4 = 100.8

	be, _ := ProfitLockTargets("long", entry, isl, isl, 102.0, BreakevenArmR(rc), 0.2) // 0.5R
	if be != 100.8 {
		t.Fatalf("long arm at 0.5R: be = %.2f, want 100.8", be)
	}
	if be, _ := ProfitLockTargets("long", entry, isl, isl, 101.0, BreakevenArmR(rc), 0.2); be != 0 {
		t.Fatalf("below arm threshold must not arm, got %.2f", be)
	}
	if be, _ := ProfitLockTargets("long", entry, isl, 100.8, 102.0, BreakevenArmR(rc), 0.2); be != 0 {
		t.Fatalf("already armed (currentSL at BE) must not re-arm, got %.2f", be)
	}
	if be, _ := ProfitLockTargets("long", entry, isl, 101.0, 102.0, BreakevenArmR(rc), 0.2); be != 0 {
		t.Fatalf("AI tighten past BE must not loosen, got %.2f", be)
	}

	beS, _ := ProfitLockTargets("short", 100.0, 104.0, 104.0, 98.0, BreakevenArmR(rc), 0.2)
	if beS != 99.2 {
		t.Fatalf("short arm: be = %.2f, want 99.2", beS)
	}
}
