package kernel

import (
	"testing"

	"nofx/store"
)

// Pins for tp_full_yields_to_lock (QUANT_REVIEW_2026-09-22 C3): default OFF
// (full tier fires as before — the exit experiment is measuring it), ON =
// the full tier yields to an active 1R lock; with the lock disabled the full
// tier always fires.
func TestTpFullYieldsToLock(t *testing.T) {
	rc := &store.RiskControlConfig{} // defaults: lock 1R active, full 25%
	pnl := 30.0                      // past the 25% full tier

	if got := TpTierAction(pnl, false, rc); got != "full" {
		t.Fatalf("default must keep current behavior (full at 25%%), got %q", got)
	}

	rcYield := &store.RiskControlConfig{TpFullYieldsToLock: true}
	if got := TpTierAction(pnl, false, rcYield); got != "" {
		t.Fatalf("with yield-to-lock the full tier must yield to the active lock, got %q", got)
	}

	// Lock disabled → full tier fires even with the yield flag set.
	rcNoLock := &store.RiskControlConfig{TpFullYieldsToLock: true, ProfitLockAtR: -1}
	if got := TpTierAction(pnl, false, rcNoLock); got != "full" {
		t.Fatalf("no lock → full tier must fire regardless of the flag, got %q", got)
	}

	// Yield flag without a full tier (-1) → nothing changes.
	rcNoFull := &store.RiskControlConfig{TpFullYieldsToLock: true, TpFullProfitPct: -1}
	if got := TpTierAction(pnl, false, rcNoFull); got != "" {
		t.Fatalf("full tier off → no action, got %q", got)
	}
}
