package trader

import (
	"testing"

	"nofx/store"
)

// Hash contract (2026-10-04): decision_records.config_hash carries
// trader.RiskControlHash; strategy_config_versions.risk_hash carries
// store.HashRiskControlOf. Version attribution joins on them — the two
// implementations must never diverge.
func TestRiskHashContractBetweenStoreAndTrader(t *testing.T) {
	rc := store.RiskControlConfig{
		MaxPositions:    3,
		MinConfidence:   75,
		RiskPerTradePct: 1.5,
		SLMinATRMult:    1.5,
		TPMenuEnabled:   nil,
		ProfitLockAtR:   1.0,
		EntryTimingGate: true,
	}
	cfg := &store.StrategyConfig{RiskControl: rc}
	if got, want := store.HashRiskControlOf(cfg), RiskControlHash(&rc); got != want {
		t.Fatalf("risk hash divergence: store=%s trader=%s", got, want)
	}
}
