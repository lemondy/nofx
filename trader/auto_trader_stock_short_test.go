package trader

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"nofx/kernel/stockengine"
	"nofx/market"
	"nofx/market/usstock"
	"nofx/store"
)

func enableStockShortFakes(t *testing.T, fx *stockFixture, price *float64) {
	t.Helper()
	oldLookup, oldPerp, oldFunding := stockLookupSymbol, stockGetPerpQuote, stockFundingSettlements
	t.Cleanup(func() { stockLookupSymbol, stockGetPerpQuote, stockFundingSettlements = oldLookup, oldPerp, oldFunding })
	stockLookupSymbol = func(ctx context.Context, sym string) (usstock.SymbolInfo, bool) {
		info, ok := oldLookup(ctx, sym)
		if ok {
			info.PerpSymbol = strings.TrimSuffix(sym, "BUSDT") + "USDT"
		}
		return info, ok
	}
	stockGetPerpQuote = func(_ context.Context, sym string) (*usstock.PerpQuote, error) {
		return &usstock.PerpQuote{Symbol: sym, Price: *price, Time: fx.at.stockClock()}, nil
	}
	stockFundingSettlements = func(string, time.Time, time.Time) ([]market.FundingSettlement, error) { return nil, nil }
}

func TestStockShortCycleStateAPIAndClose(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, func(call int) string {
		if call == 1 {
			return `[{"symbol":"AAPLBUSDT","action":"open_short","stop_loss":130,"take_profit":110,"confidence":85,"thesis":"daily downtrend","invalidation":"swing high"}]`
		}
		return `[{"symbol":"AAPLBUSDT","action":"close_short"}]`
	}, func(c *store.StockConfig) { c.ShortMode = "paper" })
	price := 125.0
	enableStockShortFakes(t, fx, &price)
	fx.cycle(t)
	rows, _ := fx.at.store.StockPaper().ListOpen("t1")
	if len(rows) != 1 || rows[0].Side != "SHORT" || rows[0].AvgPrice != 125 || rows[0].Fee <= 0 {
		t.Fatalf("%+v", rows)
	}
	qty := rows[0].Quantity
	price = 115
	fx.at.stock.setPerpPrice("AAPLBUSDT", price)
	fx.at.stock.setPrice("AAPLBUSDT", 500) // a spot quote cannot mark a short
	st, err := fx.at.stockGatherPaper(map[string]float64{"AAPLBUSDT": 600})
	if err != nil {
		t.Fatal(err)
	}
	want := 10000 + 10*qty - 125*qty*.0004
	if !near(st.Equity, want) || !near(st.Exposure, 115*qty) || !near(st.Available, want-st.Exposure) {
		t.Fatalf("%+v", st)
	}
	st.repricePaper(map[string]float64{"AAPLBUSDT": 700})
	if !near(st.Equity, want) || st.enginePositions(nil)[0].Side != "SHORT" {
		t.Fatalf("%+v", st)
	}
	positions, err := fx.at.stockPositionsAPI()
	if err != nil || len(positions) != 1 || positions[0]["side"] != "short" || !near(positions[0]["unrealized_pnl"].(float64), 10*qty) {
		t.Fatalf("%+v %v", positions, err)
	}
	acct, err := fx.at.stockAccountInfo()
	if err != nil || !near(acct["total_equity"].(float64), want) || !near(acct["unrealized_profit"].(float64), 10*qty) {
		t.Fatalf("%+v %v", acct, err)
	}
	risk := fx.at.stockRiskStatus()
	if len(risk.Positions) != 1 || risk.Positions[0].Side != "short" || !near(risk.Positions[0].CurrentR, 2) {
		t.Fatal(risk)
	}
	fx.cycle(t)
	closed, _ := fx.at.store.StockPaper().ListClosed("t1", 1)
	wantPnL := (10 - 125*.0004 - 115*.0004) * qty
	if len(closed) != 1 || !near(closed[0].RealizedPnL, wantPnL) || closed[0].ExitPrice != 115 {
		t.Fatalf("%+v", closed)
	}
	if len(fx.spot.buys)+len(fx.spot.sells)+len(fx.spot.setProt) > 0 {
		t.Fatal("paper short reached spot executor")
	}
}

func TestStockPaperDirectionalStopsTPAndFees(t *testing.T) {
	cases := []struct {
		name, side, entry             string
		price, stop, tp, fill, feeBps float64
	}{
		{"long stop gap", "LONG", "market", 90, 95, 110, 90, 10},
		{"long tp", "LONG", "limit", 115, 95, 110, 110, 0},
		{"short stop gap", "SHORT", "market", 112, 105, 90, 112, 4},
		{"short tp", "SHORT", "limit", 80, 105, 90, 90, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newStockFixtureCfg(t, true, stockTestNow, nil, func(c *store.StockConfig) { c.ShortMode = "paper" })
			price := tc.price
			enableStockShortFakes(t, fx, &price)
			fx.at.store.StockPaper().EnsureAccount("t1", 10000)
			opened := stockTestNow.Add(-time.Hour)
			p, _, err := fx.at.store.StockPaper().OpenOrAddSideWithFee("t1", "AAPLBUSDT", tc.side, 2, 100, tc.stop, tc.tp, 0, opened)
			if err != nil {
				t.Fatal(err)
			}
			fx.price = tc.price
			// Paper protection is also called during the closed weekend session.
			weekend := time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC)
			fx.at.stockTickPaper(context.Background(), weekend, func(string, ...interface{}) {})
			rows, _ := fx.at.store.StockPaper().ListClosed("t1", 1)
			fee := 2 * tc.fill * tc.feeBps / 10000
			gross := (tc.fill - 100) * 2
			if tc.side == "SHORT" {
				gross = -gross
			}
			if len(rows) != 1 || rows[0].ID != p.ID || rows[0].ExitPrice != tc.fill || !near(rows[0].Fee, fee) || !near(rows[0].RealizedPnL, gross-fee) {
				t.Fatalf("%+v", rows)
			}
		})
	}
}

func TestStockPaperLimitDirectionsAndStopRounding(t *testing.T) {
	for _, short := range []bool{false, true} {
		for _, fill := range []bool{false, true} {
			t.Run(fmt.Sprintf("short=%t/fill=%t", short, fill), func(t *testing.T) {
				fx := newStockFixture(t, true, stockTestNow, nil)
				price := 100.0
				enableStockShortFakes(t, fx, &price)
				fx.at.store.StockPaper().EnsureAccount("t1", 10000)
				d := stockengine.Decision{Symbol: "AAPLBUSDT", Action: stockengine.ActionOpenLong, EntryType: "limit", LimitPrice: 101, StopLoss: 95.005}
				if short {
					d.Action = stockengine.ActionOpenShort
					d.LimitPrice = 99
					d.StopLoss = 105.005
				}
				if !fill {
					if short {
						d.LimitPrice = 101
					} else {
						d.LimitPrice = 99
					}
				}
				out, err := fx.at.stockOpenPaper(d, 2, price, nil, usstock.SymbolInfo{TickSize: .01}, stockTestNow)
				if !fill {
					if err == nil {
						t.Fatal("unfilled limit accepted")
					}
					return
				}
				if err != nil || out.Price != 100 {
					t.Fatalf("%+v %v", out, err)
				}
				rows, _ := fx.at.store.StockPaper().ListOpen("t1")
				stop := 95.0
				if short {
					stop = 105.01
				}
				if len(rows) != 1 || rows[0].Fee != 0 || !near(rows[0].Stop, stop) {
					t.Fatalf("%+v", rows)
				}
			})
		}
	}
}

func TestStockShortFundingTickReplayFailureAndFallback(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, nil, func(c *store.StockConfig) { c.ShortMode = "paper" })
	price := 100.0
	enableStockShortFakes(t, fx, &price)
	sp := fx.at.store.StockPaper()
	sp.EnsureAccount("t1", 10000)
	opened := stockTestNow.Add(-24 * time.Hour)
	p, _, err := sp.OpenOrAddSideWithFee("t1", "AAPLBUSDT", "SHORT", 2, 100, 110, 90, 0, opened)
	if err != nil {
		t.Fatal(err)
	}
	events := []market.FundingSettlement{{Time: opened.Add(time.Hour), Rate: .01, MarkPrice: 120}, {Time: opened.Add(2 * time.Hour), Rate: -.005}}
	fail := true
	stockFundingSettlements = func(sym string, start, end time.Time) ([]market.FundingSettlement, error) {
		if sym != "AAPLUSDT" {
			t.Fatal(sym)
		}
		if fail {
			return nil, errors.New("offline injected")
		}
		var out []market.FundingSettlement
		for _, e := range events {
			if !e.Time.Before(start) && !e.Time.After(end) {
				out = append(out, e)
			}
		}
		return out, nil
	}
	fx.at.stockTickPaper(context.Background(), stockTestNow, func(string, ...interface{}) {})
	rows, _ := sp.ListOpen("t1")
	if rows[0].Funding != 0 || rows[0].LastFundingTime != p.OpenedAt {
		t.Fatal(rows[0])
	}
	fail = false
	fx.at.stockTickPaper(context.Background(), stockTestNow, func(string, ...interface{}) {})
	// Re-read durable state as a restarted protection worker would.
	fx.at.stockTickPaper(context.Background(), stockTestNow, func(string, ...interface{}) {})
	rows, _ = sp.ListOpen("t1")
	acct, _ := sp.GetAccount("t1")
	if !near(rows[0].Funding, 1.4) || !near(acct.Cash, 10201.4) || rows[0].LastFundingTime != events[1].Time.UnixMilli() {
		t.Fatalf("%+v %+v", rows[0], acct)
	}
}

func TestStockLiveRejectsEveryShortActionBeforeExecutor(t *testing.T) {
	fx := newStockFixture(t, false, stockTestNow, nil)
	fx.at.stock.cfg.ShortMode = "paper" // invalid configuration bypassed at runtime
	for _, action := range []string{stockengine.ActionOpenShort, stockengine.ActionAddShort} {
		_, _, err := fx.at.execStockEntry(context.Background(), nil, stockengine.Decision{Action: action}, nil, nil, false, stockTestNow, func(string, ...interface{}) {})
		if err == nil || !strings.Contains(err.Error(), "SHORT_DISABLED") {
			t.Fatal(action, err)
		}
	}
	for _, action := range []string{stockengine.ActionReduceShort, stockengine.ActionCloseShort} {
		_, err := fx.at.execStockExit(context.Background(), stockengine.Decision{Action: action}, nil, nil, false, stockTestNow)
		if err == nil || !strings.Contains(err.Error(), "SHORT_DISABLED") {
			t.Fatal(action, err)
		}
	}
	if len(fx.spot.buys)+len(fx.spot.sells)+len(fx.spot.setProt) > 0 {
		t.Fatal("short reached live executor")
	}
}

func TestStockShortEntryRequoteFreshness(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, nil, func(c *store.StockConfig) { c.ShortMode = "paper" })
	price := 125.0
	enableStockShortFakes(t, fx, &price)
	info, _ := stockLookupSymbol(context.Background(), "AAPLBUSDT")
	snaps, _, _ := fx.at.buildStockSnapshots(context.Background(), fx.at.stock.cfg, stockengine.ResolvePreset(fx.at.stock.cfg), stockTestNow, nil, func(string, ...interface{}) {})
	c := &stockengine.Context{Config: fx.at.stock.cfg, Account: stockengine.Account{Equity: 10000, Available: 10000}, Snapshots: snaps}
	d := stockengine.Decision{Symbol: info.Symbol, Action: stockengine.ActionOpenShort, StopLoss: 130, Confidence: 85, Thesis: "trend", Invalidation: "swing high"}
	for _, age := range []time.Duration{30 * time.Second, 31 * time.Second, -time.Second} {
		stockGetPerpQuote = func(_ context.Context, sym string) (*usstock.PerpQuote, error) {
			return &usstock.PerpQuote{Symbol: sym, Price: 125, Time: stockTestNow.Add(-age)}, nil
		}
		_, _, err := fx.at.stockEntryContext(context.Background(), c, d, snaps[0])
		if (err == nil) != (age == 30*time.Second) {
			t.Fatalf("age=%v err=%v", age, err)
		}
	}
}

func TestStockShortScanMergesAndOffUnchanged(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, nil, func(c *store.StockConfig) { c.SymbolSource = "scan"; c.ScanTopN = 1; c.ScanShortTopN = 2 })
	report := &stockengine.ScanReport{GeneratedAt: stockTestNow, Results: []stockengine.ScanResult{
		{Symbol: "AAPLBUSDT", DataThrough: "2026-10-13", Cycle: stockengine.CycleAssessment{LongSetup: true, ShortSetup: true}},
		{Symbol: "MSFTBUSDT", DataThrough: "2026-10-13", Cycle: stockengine.CycleAssessment{ShortSetup: true}, Momentum: stockengine.MomentumAssessment{Mom12_1Pct: -20}},
		{Symbol: "NVDABUSDT", DataThrough: "2026-10-13", Momentum: stockengine.MomentumAssessment{Valid: true, BelowSMA200: true, Mom12_1Pct: -30}},
	}}
	stockGetScanReport = func(context.Context, time.Duration) (*stockengine.ScanReport, error) { return report, nil }
	state := &stockState{Holdings: map[string]*stockHolding{}}
	off, _, _ := fx.at.stockCycleSelection(context.Background(), fx.at.stock.cfg, state, func(string, ...interface{}) {})
	if !reflect.DeepEqual(off, []string{"AAPLBUSDT"}) {
		t.Fatal(off)
	}
	fx.at.stock.cfg.ShortMode = "paper"
	on, entries, _ := fx.at.stockCycleSelection(context.Background(), fx.at.stock.cfg, state, func(string, ...interface{}) {})
	if !reflect.DeepEqual(on, []string{"AAPLBUSDT", "MSFTBUSDT"}) || !entries["MSFTBUSDT"] {
		t.Fatalf("%v %v", on, entries)
	}
}

func TestStockShortFundingSettlesBeforeQuantityChanges(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, nil, func(c *store.StockConfig) { c.ShortMode = "paper" })
	price := 100.0
	enableStockShortFakes(t, fx, &price)
	sp := fx.at.store.StockPaper()
	sp.EnsureAccount("t1", 10000)
	opened := stockTestNow.Add(-4 * time.Hour)
	p, _, err := sp.OpenOrAddSideWithFee("t1", "AAPLBUSDT", "SHORT", 2, 100, 110, 90, 0, opened)
	if err != nil {
		t.Fatal(err)
	}
	events := []market.FundingSettlement{{Time: opened.Add(time.Hour), Rate: .01, MarkPrice: 100}, {Time: opened.Add(3 * time.Hour), Rate: .01, MarkPrice: 100}}
	stockFundingSettlements = func(_ string, start, end time.Time) ([]market.FundingSettlement, error) {
		var out []market.FundingSettlement
		for _, e := range events {
			if !e.Time.Before(start) && !e.Time.After(end) {
				out = append(out, e)
			}
		}
		return out, nil
	}
	h := &stockHolding{Symbol: p.Symbol, Side: "SHORT", Owned: 2, Stop: 110}
	d := stockengine.Decision{Symbol: p.Symbol, Action: stockengine.ActionAddShort, EntryType: "limit", LimitPrice: 100, StopLoss: 108}
	if _, err := fx.at.stockOpenPaper(d, 2, 100, h, usstock.SymbolInfo{TickSize: .01}, opened.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, _ := sp.ListOpen("t1")
	if rows[0].Funding != 2 || rows[0].Quantity != 4 || rows[0].Stop != 108 {
		t.Fatal(rows[0])
	}
	if _, err := fx.at.stockSellPaper(p.Symbol, 2, 100, "ai_reduce", stockTestNow); err != nil {
		t.Fatal(err)
	}
	rows, _ = sp.ListOpen("t1")
	if rows[0].Funding != 6 || rows[0].Quantity != 2 {
		t.Fatal(rows[0])
	}
}

func TestStockShortFundingPagination(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, nil, func(c *store.StockConfig) { c.ShortMode = "paper" })
	price := 100.0
	enableStockShortFakes(t, fx, &price)
	sp := fx.at.store.StockPaper()
	sp.EnsureAccount("t1", 10000)
	opened := stockTestNow.Add(-2 * time.Hour)
	p, _, err := sp.OpenOrAddSideWithFee("t1", "AAPLBUSDT", "SHORT", 1, 100, 110, 90, 0, opened)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	stockFundingSettlements = func(_ string, start, end time.Time) ([]market.FundingSettlement, error) {
		calls++
		var events []market.FundingSettlement
		for i := 1; i <= 1001 && len(events) < 1000; i++ {
			at := opened.Add(time.Duration(i) * time.Second)
			if !at.Before(start) && !at.After(end) {
				events = append(events, market.FundingSettlement{Time: at, Rate: .0001, MarkPrice: 100})
			}
		}
		return events, nil
	}
	if err := fx.at.stockApplyFunding(context.Background(), p, 100, stockTestNow); err != nil {
		t.Fatal(err)
	}
	rows, _ := sp.ListOpen("t1")
	if calls != 2 || !near(rows[0].Funding, 10.01) || rows[0].LastFundingTime != opened.Add(1001*time.Second).UnixMilli() {
		t.Fatalf("calls=%d %+v", calls, rows[0])
	}
}

func TestStockRegimeLeavesShortHoldingsIntact(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, func(int) string { return holdAAPL }, func(c *store.StockConfig) {
		c.ShortMode = "paper"
		c.RegimeScaler = store.StockRegimeBasketVol
		c.RegimeFloor = .5
	})
	price := 120.0
	enableStockShortFakes(t, fx, &price)
	useScanReport(regimeScanReport(), nil)
	sp := fx.at.store.StockPaper()
	sp.EnsureAccount("t1", 10000)
	if _, _, err := sp.OpenOrAddSideWithFee("t1", "AAPLBUSDT", "SHORT", 10, 120, 125, 110, 0, stockTestNow); err != nil {
		t.Fatal(err)
	}
	if err := fx.at.store.StockRegimeScales().Set(scaleKey(fx), "AAPLBUSDT", 1); err != nil {
		t.Fatal(err)
	}
	fx.cycle(t)
	if qty := paperQty(t, fx); qty != 10 {
		t.Fatal(qty)
	}
	if actions := regimeTrimActions(fx.lastRecord(t)); len(actions) != 0 {
		t.Fatal(actions)
	}
}

func TestStockShortStopFillsWhenFundingReadFails(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, nil, func(c *store.StockConfig) { c.ShortMode = "paper" })
	price := 112.0
	enableStockShortFakes(t, fx, &price)
	sp := fx.at.store.StockPaper()
	sp.EnsureAccount("t1", 10000)
	if _, _, err := sp.OpenOrAddSideWithFee("t1", "AAPLBUSDT", "SHORT", 2, 100, 110, 90, 0, stockTestNow.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stockFundingSettlements = func(string, time.Time, time.Time) ([]market.FundingSettlement, error) {
		return nil, errors.New("offline injected")
	}
	fx.at.stockTickPaper(context.Background(), stockTestNow, func(string, ...interface{}) {})
	if rows, _ := sp.ListOpen("t1"); len(rows) != 0 {
		t.Fatalf("short stop skipped on funding failure: %+v", rows[0])
	}
}
