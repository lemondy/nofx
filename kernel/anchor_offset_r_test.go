package kernel

import (
	"testing"

	"nofx/store"
)

func storeRiskControl(t *testing.T, trimR, fullR float64) *store.RiskControlConfig {
	t.Helper()
	return &store.RiskControlConfig{TPTrimAtR: trimR, TPFullAtR: fullR}
}

func boolPtr(b bool) *bool { return &b }

// R-tier resolvers (2026-09-29 unit unification): precedence and off states.
func TestTpAtRResolvers(t *testing.T) {
	if TpTrimAtR(nil) != 0 || TpFullAtR(nil) != 0 {
		t.Fatal("nil config must resolve to 0 (legacy fallback)")
	}
	rc := storeRiskControl(t, 1.2, -1)
	if TpTrimAtR(rc) != 1.2 {
		t.Fatalf("trim R = %v, want 1.2", TpTrimAtR(rc))
	}
	if TpFullAtR(rc) != -1 {
		t.Fatalf("full R = %v, want -1 (tier off)", TpFullAtR(rc))
	}
	if !TpLadderUsesR(rc) {
		t.Fatal("any R tier ≠0 must switch the ladder to R units")
	}
	if TpLadderUsesR(storeRiskControl(t, 0, 0)) {
		t.Fatal("both R tiers 0 must stay on the legacy ROE ladder")
	}
	if PeakDrawdownArmR(storeRiskControl(t, 0, 0)) != 0 {
		t.Fatal("drawdown arm must default to legacy ROE mode")
	}
	if gb := PeakDrawdownGivebackR(nil); gb != 0.5 {
		t.Fatalf("giveback default = %v, want 0.5", gb)
	}
	rcGB := &store.RiskControlConfig{PeakDrawdownGivebackR: 1.5}
	if gb := PeakDrawdownGivebackR(rcGB); gb != 0.99 {
		t.Fatalf("giveback ≥1 must clamp below 1 (never fires otherwise), got %v", gb)
	}
}

// The R ladder mirrors TpTierAction's shape: full → trim, once-only trim,
// yields-to-lock flags honored.
func TestTpTierActionR(t *testing.T) {
	rc := storeRiskControl(t, 1.2, -1) // trim 1.2R, full off
	rc.TpTrimYieldsToLock = boolPtr(false)
	if got := TpTierActionR(1.19, false, rc); got != "" {
		t.Fatalf("below trim threshold → %q, want \"\"", got)
	}
	if got := TpTierActionR(1.2, false, rc); got != "trim" {
		t.Fatalf("at trim threshold → %q, want trim", got)
	}
	if got := TpTierActionR(5, true, rc); got != "" {
		t.Fatalf("trim already taken → %q, want \"\"", got)
	}

	// Default yields-to-lock (nil = true) + active lock: the trim tier
	// stands down — same rule as the legacy TpTierAction.
	if got := TpTierActionR(2, false, storeRiskControl(t, 1.2, -1)); got != "" {
		t.Fatalf("trim yields to lock (default) → %q, want \"\"", got)
	}

	// Full tier fires above its R and outranks trim.
	rcFull := storeRiskControl(t, 1.2, 3)
	if got := TpTierActionR(3, false, rcFull); got != "full" {
		t.Fatalf("at full threshold → %q, want full", got)
	}

	// tp_full_yields_to_lock=true + active lock: the full tier stands down.
	rcFullYield := storeRiskControl(t, 1.2, 3)
	rcFullYield.TpFullYieldsToLock = true
	if got := TpTierActionR(3, false, rcFullYield); got != "" {
		t.Fatalf("full yields to lock → %q, want \"\"", got)
	}

	// tp_trim_yields_to_lock=true + active lock: the trim tier stands down.
	rcTrimYield := storeRiskControl(t, 1.2, -1)
	rcTrimYield.TpTrimYieldsToLock = boolPtr(true)
	if got := TpTierActionR(2, false, rcTrimYield); got != "" {
		t.Fatalf("trim yields to lock → %q, want \"\"", got)
	}
}
