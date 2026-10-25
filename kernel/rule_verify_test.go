package kernel

import (
	"testing"
	"time"

	"nofx/store"
)

// review 2026-10-09 K

func vTrade(lev int, net float64, ai *bool, ageDays int, now int64) *store.TradeJournalDB {
	return &store.TradeJournalDB{
		Symbol: "BTCUSDT", Side: "LONG", EntryPrice: 100, Quantity: 1, Leverage: lev,
		PlannedStopLoss: 98, PlannedTakeProfit: 104,
		RealizedPnL: net, ExitTime: now - int64(ageDays)*24*3600*1000, AIManaged: ai,
	}
}

func bp(b bool) *bool { return &b }

const levRule = `{"field":"leverage","op":">","value":10}`

func TestVerifyHardRule(t *testing.T) {
	now := time.Now().UnixMilli()
	ai := bp(true)

	mk := func(highNet float64, nHigh int) []*store.TradeJournalDB {
		var ts []*store.TradeJournalDB
		for i := 0; i < nHigh; i++ {
			ts = append(ts, vTrade(20, highNet, ai, 5, now))
		}
		for i := 0; i < 6; i++ {
			ts = append(ts, vTrade(5, 1, ai, 5, now))
		}
		return ts
	}

	v := VerifyHardRule("hard", levRule, mk(-3, 4), 0, now)
	if v.Status != VerifySupported || v.Matched != 4 || v.Wins != 0 || v.Population != 10 {
		t.Fatalf("losing high leverage should be supported: %+v", v)
	}
	v = VerifyHardRule("hard", levRule, mk(5, 4), 0, now)
	if v.Status != VerifyContradicted {
		t.Fatalf("winning high leverage should be contradicted: %+v", v)
	}
	v = VerifyHardRule("hard", levRule, mk(-3, 2), 0, now)
	if v.Status != VerifyWeak || v.Matched != 2 {
		t.Fatalf("2 matches should be weak: %+v", v)
	}
	if v = VerifyHardRule("hard", `{"field":"confidence","op":"<","value":60}`, mk(-3, 4), 0, now); v.Status != VerifyUnverifiable {
		t.Fatalf("confidence should be unverifiable: %+v", v)
	}
	if v = VerifyHardRule("hard", `{"field":"position_value_pct","op":">","value":50}`, mk(-3, 4), 0, now); v.Status != VerifyUnverifiable {
		t.Fatalf("position_value_pct without equity should be unverifiable: %+v", v)
	}
	if v = VerifyHardRule("hard", `{"field":"position_value_pct","op":">","value":50}`, mk(-3, 4), 150, now); v.Status == VerifyUnverifiable {
		t.Fatalf("position_value_pct with equity should be checkable: %+v", v)
	}
	if v = VerifyHardRule("soft", "", mk(-3, 4), 0, now); v.Status != VerifySoft {
		t.Fatalf("soft: %+v", v)
	}
	// risk_reward is reconstructed through the live fact extraction: tp 4% / sl 2% = 2.
	if v = VerifyHardRule("hard", `{"field":"risk_reward","op":">=","value":2}`, mk(-3, 4), 0, now); v.Matched != 10 {
		t.Fatalf("risk_reward replay: %+v", v)
	}
}

func TestVerifyPopulationExcludesManualNullAndOld(t *testing.T) {
	now := time.Now().UnixMilli()
	ts := mkHigh(-3, 3, now)
	// noise that would flip the verdict if it leaked in
	ts = append(ts, vTrade(20, 100, bp(false), 5, now), vTrade(20, 100, nil, 5, now),
		vTrade(20, 100, bp(true), 91, now), vTrade(20, 100, bp(true), 120, now))
	v := VerifyHardRule("hard", levRule, ts, 0, now)
	if v.Matched != 3 || v.Population != 3+6 || v.Status != VerifySupported {
		t.Fatalf("manual/NULL/old rows leaked: %+v", v)
	}
}

func mkHigh(net float64, n int, now int64) []*store.TradeJournalDB {
	var ts []*store.TradeJournalDB
	for i := 0; i < n; i++ {
		ts = append(ts, vTrade(20, net, bp(true), 5, now))
	}
	for i := 0; i < 6; i++ {
		ts = append(ts, vTrade(5, 1, bp(true), 5, now))
	}
	return ts
}
