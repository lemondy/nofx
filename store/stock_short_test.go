package store

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestStockShortConfig(t *testing.T) {
	for _, mode := range []string{"", "off", "paper"} {
		c := &StockConfig{Symbols: []string{"AAPLBUSDT"}, ShortMode: mode}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if c.EffectiveScanShortTopN() != 3 || c.EffectiveMaxShortExposurePct() != .5 {
			t.Fatal(c)
		}
	}
	for _, mode := range []string{"live", "unknown"} {
		c := &StockConfig{Symbols: []string{"AAPLBUSDT"}, ShortMode: mode}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "short_mode") {
			t.Fatal(err)
		}
	}
	live := false
	c := &StockConfig{Symbols: []string{"AAPLBUSDT"}, ShortMode: "paper", PaperTrading: &live}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "paper_trading=true") {
		t.Fatal(err)
	}
	c.PaperTrading = nil
	for _, n := range []int{-1, 51} {
		c.ScanShortTopN = n
		if err := c.Validate(); err == nil {
			t.Fatal(n)
		}
	}
	c.ScanShortTopN = 0
	for _, n := range []float64{-1, 1.01, math.NaN()} {
		c.MaxShortExposurePct = n
		if err := c.Validate(); err == nil {
			t.Fatal(n)
		}
	}
}

func TestStockSideLedgerFeesFundingAndRestart(t *testing.T) {
	st := newTestStore(t)
	sp := st.StockPaper()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := sp.EnsureAccount("short", 10000); err != nil {
		t.Fatal(err)
	}
	p, fee, err := sp.OpenOrAddSideWithFee("short", "AAPLBUSDT", StockPaperShort, 10, 100, 105, 90, 4, now)
	if err != nil || fee != .4 {
		t.Fatalf("%+v fee=%v err=%v", p, fee, err)
	}
	acct, _ := sp.GetAccount("short")
	if math.Abs(acct.Cash-10999.6) > 1e-8 || math.Abs(acct.Cash+p.MarketValue(100)-9999.6) > 1e-8 {
		t.Fatal(acct)
	}
	if _, _, err := sp.OpenOrAddSideWithFee("short", "AAPLBUSDT", StockPaperLong, 1, 100, 95, 0, 0, now); err == nil {
		t.Fatal("opposite entry allowed")
	}
	acct, _ = sp.GetAccount("short")
	if math.Abs(acct.Cash-10999.6) > 1e-8 {
		t.Fatal("opposite changed cash")
	}
	event := now.Add(time.Hour)
	if err := sp.ApplyShortFunding(p.ID, event, .01, 100); err != nil {
		t.Fatal(err)
	}
	// New store facade models restart: the watermark is entirely in SQLite.
	sp = NewStockPaperStore(st.GormDB())
	if err := sp.ApplyShortFunding(p.ID, event, .01, 100); err != nil {
		t.Fatal(err)
	}
	if err := sp.ApplyShortFunding(p.ID, event.Add(time.Hour), -.005, 100); err != nil {
		t.Fatal(err)
	}
	pnl, closed, fee, err := sp.ReduceSideWithFee("short", "AAPLBUSDT", StockPaperShort, 4, 90, 4, event.Add(2*time.Hour), "reduce")
	if err != nil || closed || math.Abs(pnl-39.856) > 1e-8 || math.Abs(fee-.144) > 1e-8 {
		t.Fatalf("%v %v %v %v", pnl, closed, fee, err)
	}
	pnl, closed, _, err = sp.ReduceSideWithFee("short", "AAPLBUSDT", StockPaperShort, 0, 80, 0, event.Add(3*time.Hour), "take_profit")
	if err != nil || !closed || pnl != 120 {
		t.Fatalf("%v %v %v", pnl, closed, err)
	}
	rows, _ := sp.ListClosed("short", 5)
	acct, _ = sp.GetAccount("short")
	want := 160 - .4 - .144 + 5
	if len(rows) != 1 || rows[0].Funding != 5 || rows[0].LastFundingTime != event.Add(time.Hour).UnixMilli() || math.Abs(rows[0].RealizedPnL-want) > 1e-8 || math.Abs(acct.Cash-(10000+want)) > 1e-8 {
		t.Fatalf("rows=%+v acct=%+v", rows, acct)
	}
}

func TestStockSideLongNetFeesAndLegacyGross(t *testing.T) {
	st := newTestStore(t)
	sp := st.StockPaper()
	now := time.Now()
	sp.EnsureAccount("long", 1000)
	p, _, err := sp.OpenOrAddSideWithFee("long", "AAPLBUSDT", StockPaperLong, 2, 100, 95, 110, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Side != "LONG" || p.Fee != .2 {
		t.Fatal(p)
	}
	_, _, _, err = sp.ReduceSideWithFee("long", "AAPLBUSDT", StockPaperLong, 0, 110, 10, now, "close")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := sp.ListClosed("long", 1)
	acct, _ := sp.GetAccount("long")
	if math.Abs(rows[0].RealizedPnL-19.58) > 1e-8 || math.Abs(acct.Cash-1019.58) > 1e-8 {
		t.Fatalf("%+v %+v", rows[0], acct)
	}
	sp.EnsureAccount("legacy", 1000)
	sp.OpenOrAddWithFee("legacy", "AAPLBUSDT", 2, 100, 95, 110, 10, now)
	pnl, _, _, err := sp.ReduceWithFee("legacy", "AAPLBUSDT", 0, 110, 10, now, "close")
	if err != nil || pnl != 20 {
		t.Fatalf("factor_quant legacy %v %v", pnl, err)
	}
}

func TestStockPaperMigrationDefaultsLegacyRowsToLong(t *testing.T) {
	st := newTestStore(t)
	sp := st.StockPaper()
	now := time.Now()
	sp.EnsureAccount("legacy", 1000)
	if _, err := sp.OpenOrAdd("legacy", "AAPLBUSDT", 2, 100, 95, 0, now); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"side", "funding", "last_funding_time"} {
		if err := st.GormDB().Migrator().DropColumn(&StockPaperPosition{}, column); err != nil {
			t.Fatal(err)
		}
	}
	if err := sp.initTables(); err != nil {
		t.Fatal(err)
	}
	rows, err := sp.ListOpen("legacy")
	if err != nil || len(rows) != 1 || rows[0].Side != "LONG" || rows[0].Funding != 0 || rows[0].LastFundingTime != 0 || rows[0].MarketValue(100) != 200 {
		t.Fatalf("%+v %v", rows, err)
	}
}

func TestStockShortFundingAtomicRollback(t *testing.T) {
	st := newTestStore(t)
	sp := st.StockPaper()
	now := time.Now()
	sp.EnsureAccount("tr", 1000)
	p, _, err := sp.OpenOrAddSideWithFee("tr", "AAPLBUSDT", "SHORT", 2, 100, 105, 0, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GormDB().Exec(`CREATE TRIGGER funding_fail BEFORE UPDATE ON stock_paper_accounts BEGIN SELECT RAISE(ABORT, 'injected'); END`).Error; err != nil {
		t.Fatal(err)
	}
	event := now.Add(time.Hour)
	if err := sp.ApplyShortFunding(p.ID, event, .01, 100); err == nil {
		t.Fatal("funding failure ignored")
	}
	rows, _ := sp.ListOpen("tr")
	acct, _ := sp.GetAccount("tr")
	if rows[0].Funding != 0 || rows[0].LastFundingTime != p.LastFundingTime || acct.Cash != 1200 {
		t.Fatalf("%+v %+v", rows[0], acct)
	}
	if err := st.GormDB().Exec(`DROP TRIGGER funding_fail`).Error; err != nil {
		t.Fatal(err)
	}
	if err := sp.ApplyShortFunding(p.ID, event, .01, 100); err != nil {
		t.Fatal(err)
	}
	rows, _ = sp.ListOpen("tr")
	if rows[0].Funding != 2 {
		t.Fatal(rows[0])
	}
}
