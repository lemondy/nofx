package trader

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/market/usstock"
)

func TestStockReview1024ManualKeepsRemovedHolding(t *testing.T) {
	fx := newStockFixture(t, true, stockTestNow, func(int) string { return closeAAPL })
	if _, err := fx.at.store.StockPaper().EnsureAccount(fx.at.id, 10000); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.at.store.StockPaper().OpenOrAdd(fx.at.id, "AAPLBUSDT", 1, 120, 115, 0, stockTestNow); err != nil {
		t.Fatal(err)
	}
	fx.at.stock.cfg.Symbols = []string{"MSFTBUSDT"}
	fx.cycle(t)
	rec := fx.lastRecord(t)
	if len(rec.Decisions) != 1 || !rec.Decisions[0].Success {
		t.Fatalf("removed holding could not close: %+v", rec.Decisions)
	}
}

func TestStockReview1024ExpiredScanRetainsOnlyHeldAndManual(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, func(int) string { return holdAAPL }, hybridMode(2, []string{"MSFTBUSDT"}, nil))
	report := scanReportOf(scanRes("NVDABUSDT", "wedge_pop", true, 90))
	report.GeneratedAt = stockTestNow.Add(-2 * time.Hour)
	useScanReport(report, fmt.Errorf("refresh failed"))
	syms, _ := fx.at.stockCycleSymbols(context.Background(), fx.at.stock.cfg, &stockState{Holdings: map[string]*stockHolding{"AAPLBUSDT": {Owned: 1}}}, func(string, ...interface{}) {})
	if strings.Join(syms, ",") != "AAPLBUSDT,MSFTBUSDT" {
		t.Fatalf("expired report authorized scan candidates: %v", syms)
	}
}

func TestStockReview1024EntryRechecksClock(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 59, 0, 0, stockET)
	fx := newStockFixture(t, false, now, func(int) string { return openAAPL })
	oldQuote := stockGetQuote
	fx.ai.respond = func(int) string {
		stockGetQuote = func(ctx context.Context, sym string) (*usstock.Quote, error) {
			q, e := oldQuote(ctx, sym)
			q.BStockTime = now.Add(2 * time.Minute)
			return q, e
		}
		fx.at.stockNow = func() time.Time { return now.Add(2 * time.Minute) }
		return openAAPL
	}
	fx.cycle(t)
	rec := fx.lastRecord(t)
	if len(fx.spot.buys) != 0 || len(rec.Decisions) != 1 || !strings.Contains(rec.Decisions[0].Error, "SESSION_NOT_ALLOWED") {
		t.Fatalf("late entry submitted: buys=%v decisions=%+v", fx.spot.buys, rec.Decisions)
	}
}

func TestStockReview1024FreshReportWithOldDataCannotAuthorize(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, func(int) string { return holdAAPL }, scanMode(2, nil, nil))
	r := scanRes("NVDABUSDT", "wedge_pop", true, 90)
	r.DataThrough = "2026-10-12"
	useScanReport(scanReportOf(r), nil)
	syms, entries, _ := fx.at.stockCycleSelection(context.Background(), fx.at.stock.cfg, &stockState{Holdings: map[string]*stockHolding{"AAPLBUSDT": {Owned: 1}}}, func(string, ...interface{}) {})
	if strings.Join(syms, ",") != "AAPLBUSDT" || len(entries) != 0 {
		t.Fatalf("old source bars authorized entry: syms=%v entries=%v", syms, entries)
	}
}

func TestStockReview1024RemovedHoldingCannotAdd(t *testing.T) {
	fx := newStockFixture(t, true, stockTestNow, func(int) string { return strings.ReplaceAll(openAAPL, "open_long", "add_long") })
	if _, err := fx.at.store.StockPaper().EnsureAccount(fx.at.id, 10000); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.at.store.StockPaper().OpenOrAdd(fx.at.id, "AAPLBUSDT", 1, 110, 105, 0, stockTestNow); err != nil {
		t.Fatal(err)
	}
	fx.at.stock.cfg.Symbols = []string{"MSFTBUSDT"}
	fx.cycle(t)
	rec := fx.lastRecord(t)
	if len(rec.Decisions) != 1 || !strings.Contains(rec.Decisions[0].Error, "EXIT_ONLY_SYMBOL") {
		t.Fatalf("removed holding authorized add: %+v", rec.Decisions)
	}
}

type review1024HiddenInventory struct {
	*fakeSpot
	rawErr error
}

func (f *review1024HiddenInventory) GetPositions() ([]map[string]interface{}, error) { return nil, nil }
func (f *review1024HiddenInventory) RawStockBalance(sym string) (float64, error) {
	if f.rawErr != nil {
		return 0, f.rawErr
	}
	return f.fakeSpot.RawStockBalance(sym)
}

func TestStockReview1024ShrinkVerifiesRawInventory(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			fx := newStockFixture(t, false, stockTestNow, func(int) string { return holdAAPL })
			fx.openLiveOwned(t, .001, .001)
			if _, err := fx.spot.SetProtection("AAPLBUSDT", .001, 115, 114.65, 0); err != nil {
				t.Fatal(err)
			}
			hidden := &review1024HiddenInventory{fakeSpot: fx.spot}
			if fail {
				hidden.rawErr = fmt.Errorf("account unavailable")
			}
			fx.at.stockTrader = hidden
			st, err := fx.at.stockGatherLive(context.Background(), stockTestNow, true, true)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := fx.at.stockOwnedRows()
			if err != nil {
				t.Fatal(err)
			}
			qty, _, _, _ := sumRows(rows["AAPLBUSDT"])
			h := st.Holdings["AAPLBUSDT"]
			if !near(qty, .001) || h == nil || !near(h.Owned, .001) || fx.spot.cancelProt != 0 || fx.spot.prot["AAPLBUSDT"] == nil {
				t.Fatalf("raw inventory lost protection/ownership: qty=%g h=%+v cancels=%d", qty, h, fx.spot.cancelProt)
			}
			if fail != h.Quarantined {
				t.Fatalf("raw-read ambiguity not quarantined: %+v", h)
			}
		})
	}
}

func TestStockReview1024EntryRechecksQuote(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		mutate     func(*usstock.Quote)
	}{
		{"bstock old", "BSTOCK_PRICE_STALE", func(q *usstock.Quote) { q.BStockTime = stockTestNow.Add(-31 * time.Second) }},
		{"reference old", "BSTOCK_REF_STALE", func(q *usstock.Quote) { q.RefTime = stockTestNow.Add(-16 * time.Minute) }},
		{"divergence", "BSTOCK_DIVERGENCE", func(q *usstock.Quote) { q.BStockPrice = 123; q.RefPrice = 120 }},
		{"stop distance", "STOP_OUT_OF_BAND", func(q *usstock.Quote) { q.BStockPrice = 122; q.RefPrice = 122 }},
		{"invalid bstock", "BSTOCK_PRICE_STALE", func(q *usstock.Quote) { q.BStockPrice = math.NaN() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newStockFixture(t, false, stockTestNow, func(int) string { return openAAPL })
			oldQuote := stockGetQuote
			fx.ai.respond = func(int) string {
				stockGetQuote = func(ctx context.Context, sym string) (*usstock.Quote, error) {
					q, e := oldQuote(ctx, sym)
					tc.mutate(q)
					return q, e
				}
				return openAAPL
			}
			fx.cycle(t)
			rec := fx.lastRecord(t)
			if len(fx.spot.buys) != 0 || len(rec.Decisions) != 1 || !strings.Contains(rec.Decisions[0].Error, tc.code) {
				t.Fatalf("unsafe entry submitted: buys=%v decisions=%+v", fx.spot.buys, rec.Decisions)
			}
		})
	}
}

func TestStockReview1024FinalPriceResizesRisk(t *testing.T) {
	fx := newStockFixture(t, false, stockTestNow, func(int) string { return openAAPL })
	fx.at.stock.cfg.RiskPerTradePct = .1
	fx.ai.respond = func(int) string { fx.price = 120.5; fx.spot.price = 120.5; return openAAPL }
	fx.cycle(t)
	rec := fx.lastRecord(t)
	if len(fx.spot.buys) != 1 || len(rec.Decisions) != 1 || !rec.Decisions[0].Success {
		t.Fatalf("entry did not execute: %+v", rec.Decisions)
	}
	// Final distance is 120.5-115=5.5: risk budget 10 USDT permits 1.818 units,
	// versus 2 units using the pre-LLM price 120.
	if fx.spot.buys[0] > 219.07 || fx.spot.buys[0] < 219.05 || rec.Decisions[0].Price != 120.5 {
		t.Fatalf("size not based on final price/risk: buys=%v decisions=%+v", fx.spot.buys, rec.Decisions)
	}
}
