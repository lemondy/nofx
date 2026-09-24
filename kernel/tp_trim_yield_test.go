package kernel

import (
	"testing"

	"nofx/store"
)

// Pins for tp_trim_yields_to_lock (CAPUSDT 2026-09-24, user option 2):
// nil/true = 09-21 default — the active 1R lock suppresses the ROE trim tier
// entirely (tp_trim_profit_pct is dead text); false = division of labor —
// the trim tier fires at its threshold while the lock only handles
// breakeven.
func TestTpTrimYieldsToLock(t *testing.T) {
	rc := &store.RiskControlConfig{} // defaults: lock 1R active, trim 15
	pnl := 17.0                      // CAP's peak: past a 15% trim, below 1R (21.6%)

	// Default (nil): the lock suppresses the trim tier — the CAP surprise.
	if got := TpTierAction(pnl, false, rc); got != "" {
		t.Fatalf("nil yields flag: lock must suppress the 15%% tier, got %q", got)
	}

	// Explicit true = same suppression.
	on := true
	rcOn := &store.RiskControlConfig{TpTrimYieldsToLock: &on}
	if got := TpTierAction(pnl, false, rcOn); got != "" {
		t.Fatalf("yields=true: lock must suppress the trim tier, got %q", got)
	}

	// false: division of labor — the 15% tier fires even with the lock on.
	off := false
	rcOff := &store.RiskControlConfig{TpTrimYieldsToLock: &off}
	if got := TpTierAction(pnl, false, rcOff); got != "trim" {
		t.Fatalf("yields=false: 15%% tier must fire, got %q", got)
	}
	if rcOff.TrimYieldsToLock() {
		t.Fatal("TrimYieldsToLock must resolve false for the off pointer")
	}

	// Division of labor, already trimmed → nothing further until full tier.
	if got := TpTierAction(pnl, true, rcOff); got != "" {
		t.Fatalf("yields=false + trimDone: must stay quiet, got %q", got)
	}

	// Helper semantics: nil resolves to true (current default).
	if !(&store.RiskControlConfig{}).TrimYieldsToLock() {
		t.Fatal("nil flag must resolve to true (yields = 09-21 default)")
	}
}
