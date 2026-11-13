package store

import (
	"math"
	"testing"
	"time"
)

func TestEntryBandConfigValidation(t *testing.T) {
	for _, mode := range []string{"", "roll", "fixed"} {
		cfg := &StrategyConfig{StrategyType: "ai_trading", RiskControl: RiskControlConfig{EntryBandEnabled: true, EntryBandMode: mode, EntryBandFixedBps: 8}}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutate := range []func(*StrategyConfig){
		func(c *StrategyConfig) { c.RiskControl.LimitEntryEnabled = true },
		func(c *StrategyConfig) { c.StrategyType = "grid_trading" },
		func(c *StrategyConfig) { c.StrategyType = StrategyTypeFactorQuant },
		func(c *StrategyConfig) { c.StrategyType = StrategyTypeUSStock },
		func(c *StrategyConfig) { c.RiskControl.EntryBandMode = "unknown" },
		func(c *StrategyConfig) { c.RiskControl.EntryBandMode = "fixed" },
		func(c *StrategyConfig) { c.RiskControl.EntryBandFixedBps = -1 },
		func(c *StrategyConfig) { c.RiskControl.EntryBandPhiBps = -1 },
		func(c *StrategyConfig) { c.RiskControl.EntryBandTimeoutMin = -1 },
		func(c *StrategyConfig) { c.RiskControl.EntryBandPhiBps = math.NaN() },
	} {
		cfg := &StrategyConfig{RiskControl: RiskControlConfig{EntryBandEnabled: true}}
		mutate(cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted invalid config: %+v", cfg)
		}
	}
	if err := (&StrategyConfig{}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEntryBandPendingMigrationAndRoundTrip(t *testing.T) {
	st := newTestStore(t)
	// Remove additive fields to reproduce an actual pre-band schema and row.
	for _, col := range []string{"Mode", "RefPrice", "ThetaBps", "SG", "ThetaReason", "Deadline"} {
		if err := st.gdb.Migrator().DropColumn(&PendingEntryDB{}, col); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.gdb.Exec("INSERT INTO trader_pending_entries(trader_id,symbol,side,order_id,price,quantity) VALUES ('t','XUSDT','long','old',100,1)").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.PendingEntry().initTables(); err != nil {
		t.Fatal(err)
	}
	rows, err := st.PendingEntry().List("t")
	if err != nil || len(rows) != 1 {
		t.Fatalf("migration: %+v %v", rows, err)
	}
	if rows[0].Mode != "" || rows[0].ThetaBps != 0 || !rows[0].Deadline.IsZero() || rows[0].OrderID != "old" {
		t.Fatalf("legacy changed: %+v", rows[0])
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	row := rows[0]
	row.Mode = "band"
	row.RefPrice = 101
	row.ThetaBps = 8
	row.SG = .0004
	row.Deadline = now.Add(30 * time.Minute)
	row.ThetaReason = "theta_fallback_phi"
	if err := st.PendingEntry().Upsert(row); err != nil {
		t.Fatal(err)
	}
	rows, err = st.PendingEntry().List("t")
	if err != nil || rows[0].Mode != "band" || rows[0].RefPrice != 101 || rows[0].SG != .0004 || !rows[0].Deadline.Equal(row.Deadline) || rows[0].ThetaReason != row.ThetaReason {
		t.Fatalf("roundtrip: %+v %v", rows, err)
	}
	event := &EntryBandEvent{TraderID: "t", Symbol: "XUSDT", Side: "long", OrderID: "old", Outcome: "filled", RefPrice: 101, LimitPrice: 100, ThetaBps: 8, SG: .0004}
	if err := st.PendingEntry().RecordBandEvent(event); err != nil {
		t.Fatal(err)
	}
	var got EntryBandEvent
	if err := st.gdb.First(&got, event.ID).Error; err != nil || got.Extremes != nil || got.SG != event.SG || got.CreatedAt.IsZero() {
		t.Fatalf("event: %+v %v", got, err)
	}
}

func TestPaperTradeThroughStoredConfig(t *testing.T) {
	for _, x := range []float64{-1, 10000, math.NaN(), math.Inf(1)} {
		cfg := DefaultPaperConfig()
		cfg.LimitTradeThroughBps = x
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted %v", x)
		}
	}
	cfg, err := (&Exchange{PaperConfig: `{"limit_trade_through_bps":12.5}`}).ParsePaperConfig()
	if err != nil || cfg.LimitTradeThroughBps != 12.5 {
		t.Fatalf("parse: %+v %v", cfg, err)
	}
	cfg, err = (&Exchange{}).ParsePaperConfig()
	if err != nil || cfg.LimitTradeThroughBps != 0 {
		t.Fatalf("default: %+v %v", cfg, err)
	}
}
