package trader

import (
	"testing"

	"nofx/store"
)

// 2026-10-07 review F1: the risk read model reports R against the opening
// stop for a negative-quantity short, and is read-only.
func TestRiskStatusPositionR(t *testing.T) {
	m := &fix06Mock{audit05Mock: audit05Mock{mark: 95}, symbol: "XUSDT", side: "short", qty: -1}
	at := riskTestTrader(store.RiskControlConfig{LossStreakBanEnabled: true})
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.SetInitialStopLoss("XUSDT", "short", 105)
	at.SetRecordedStopLoss("XUSDT", "short", 99)

	rs := at.RiskStatus()
	if len(rs.Positions) != 1 {
		t.Fatalf("positions=%+v", rs.Positions)
	}
	p := rs.Positions[0]
	if !p.HasInitialStop || p.CurrentR != 1.0 || p.StopLockedR != 0.2 {
		t.Fatalf("got R=%v locked=%v hasStop=%v, want 1.0 / 0.2 / true", p.CurrentR, p.StopLockedR, p.HasInitialStop)
	}
	if rs.DailyLossCapPct != store.DefaultDailyMaxLossPct {
		t.Fatalf("daily cap=%v, want default %v (0 = default, not disabled)", rs.DailyLossCapPct, store.DefaultDailyMaxLossPct)
	}
	if rs.LossStreakMax != lossStreakDefaultN || rs.LossStreakBans == nil || rs.Pending == nil {
		t.Fatalf("streak/pending fields not initialised: %+v", rs)
	}
	if m.slCalls != 0 {
		t.Fatal("RiskStatus placed orders — it must be read-only")
	}
}
