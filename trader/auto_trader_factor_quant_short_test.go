package trader

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/market"
	"nofx/market/usstock"
	"nofx/store"
)

type fqUSShortFixture struct {
	at       *AutoTrader
	b        *fqStockBroker
	perps    map[string]float64
	now      time.Time
	quoteErr error
}

func fqUSShortTest(t *testing.T) *fqUSShortFixture {
	t.Helper()
	fqStubUS(t, map[string]float64{"AAPLBUSDT": 200, "MSFTBUSDT": 400, "NOPERPBUSDT": 50})
	at := fqPaperTrader(t, fqTestStore(t), "fq-us-short", "us", &paperTestPrices{mark: 100})
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.USShortMode = "paper"
	f := &fqUSShortFixture{at: at, perps: map[string]float64{"AAPLUSDT": 100, "MSFTUSDT": 300}, now: fqDay("2026-11-05")}
	at.fqNow = func() time.Time { return f.now }
	oldLookup, oldPerp, oldFunding := stockLookupSymbol, stockGetPerpQuote, stockFundingSettlements
	t.Cleanup(func() { stockLookupSymbol, stockGetPerpQuote, stockFundingSettlements = oldLookup, oldPerp, oldFunding })
	stockLookupSymbol = func(ctx context.Context, sym string) (usstock.SymbolInfo, bool) {
		info, ok := oldLookup(ctx, sym)
		perp := strings.TrimSuffix(sym, "BUSDT") + "USDT"
		if _, exists := f.perps[perp]; exists {
			info.PerpSymbol = perp
		}
		return info, ok
	}
	stockGetPerpQuote = func(_ context.Context, sym string) (*usstock.PerpQuote, error) {
		if f.quoteErr != nil {
			return nil, f.quoteErr
		}
		price, ok := f.perps[sym]
		if !ok {
			return nil, fmt.Errorf("unknown perp %s", sym)
		}
		return &usstock.PerpQuote{Symbol: sym, Price: price, Time: f.now}, nil
	}
	stockFundingSettlements = func(string, time.Time, time.Time) ([]market.FundingSettlement, error) { return nil, nil }
	broker, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	f.b = broker.(*fqStockBroker)
	return f
}

func (f *fqUSShortFixture) seedShort(t *testing.T, qty, price float64) *store.StockPaperPosition {
	t.Helper()
	p, _, err := f.at.store.StockPaper().OpenOrAddSideWithFee(f.at.id, "AAPLBUSDT", store.StockPaperShort, qty, price, 0, 0, 0, f.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFactorQuantUSShortGateAndPlanConfig(t *testing.T) {
	targets := fqTestTargets("2026-11-05", map[string]float64{"AAPLBUSDT": .5, "MSFTBUSDT": -.5})
	targets.Market = "us"
	for _, mode := range []string{"", "off", "paper"} {
		cfg := &store.FactorQuantConfig{Market: "us", USShortMode: mode}
		dec := fqShouldRebalance(targets, cfg, "", false, fqDay("2026-11-05"))
		if mode == "paper" {
			if !dec.Rebalance || fqResolvedPlanConfig(cfg).LongOnly {
				t.Fatalf("paper shorts disabled: %+v", dec)
			}
		} else if dec.Rebalance || dec.Code != fqCodeShortDisabled || !dec.noisy() || !strings.Contains(dec.Reason, "负权重") || !fqResolvedPlanConfig(cfg).LongOnly {
			t.Fatalf("mode=%q: %+v", mode, dec)
		}
	}
	crypto := &store.FactorQuantConfig{Market: "crypto"}
	if !fqShouldRebalance(targets, crypto, "", false, fqDay("2026-11-05")).Rebalance || fqResolvedPlanConfig(crypto).LongOnly {
		t.Fatal("crypto long/short gate changed")
	}
}

func TestFactorQuantUSShortDisabledSkipsWholeTick(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprint(held), func(t *testing.T) {
			f := fqUSShortTest(t)
			cfg := f.at.config.StrategyConfig.FactorQuantConfig
			cfg.USShortMode = "off"
			cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("us", "ok", "2026-11-05", 28, `{"AAPLBUSDT":0.5,"MSFTBUSDT":-0.5}`))
			if held {
				if _, err := f.at.store.StockPaper().OpenOrAdd(f.at.id, "AAPLBUSDT", 1, 200, 0, 0, f.now); err != nil {
					t.Fatal(err)
				}
				fqSetOwned(t, f.at, "2026-08-01") // Even stale flatten must not turn this skip into a partial rebalance.
			}
			before, err := f.b.Account()
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				f.at.factorQuantTick(f.b, f.now)
			}
			after, err := f.b.Account()
			if err != nil || len(after.Positions) != len(before.Positions) || after.Balance != before.Balance {
				t.Fatalf("short-disabled tick traded: %+v %v", after, err)
			}
			records := fqRecords(t, f.at)
			if len(records) != 1 || len(records[0].Decisions) != 0 || !strings.Contains(strings.Join(records[0].ExecutionLog, "\n"), "short_disabled") {
				t.Fatalf("skip record: %+v", records)
			}
			if f.at.factorQuantRiskStatus().FactorQuant.LastTick.Code != fqCodeShortDisabled {
				t.Fatal("short-disabled tick missing from run status")
			}
		})
	}
}

func TestFactorQuantUSShortOpenAddAndClose(t *testing.T) {
	f := fqUSShortTest(t)
	led := f.at.store.StockPaper()
	openFill := 100 * (1 - .0003)
	for i, kind := range []string{fqKindOpen, fqKindIncrease} {
		res := f.b.Place(fqOrder{Symbol: "AAPLBUSDT", Side: fqSideShort, Kind: kind, Qty: 2, Price: 999})
		if !res.OK {
			t.Fatal(res)
		}
		fqNear(t, res.FillPrice, openFill, "fresh perp short fill")
		fqNear(t, res.Fee, 2*openFill*.0004, "4 bp short opening fee")
		rows, err := led.ListOpen(f.at.id)
		if err != nil || len(rows) != 1 || rows[0].Side != store.StockPaperShort || rows[0].Quantity != float64(2*(i+1)) {
			t.Fatalf("short ledger: %+v %v", rows, err)
		}
		acct, _ := led.GetAccount(f.at.id)
		fqNear(t, acct.Cash, 10000+float64(2*(i+1))*openFill*(1-.0004), "short proceeds less fees")
	}
	f.now = f.now.Add(time.Minute)
	f.perps["AAPLUSDT"] = 90
	closeFill := 90 * (1 + .0003)
	res := f.b.Place(fqOrder{Symbol: "AAPLBUSDT", Side: fqSideShort, Kind: fqKindClose, Qty: .01, CloseAll: true, Price: 777})
	if !res.OK {
		t.Fatal(res)
	}
	fqNear(t, res.FillPrice, closeFill, "fresh perp cover fill")
	closed, err := led.ListClosed(f.at.id, 1)
	if err != nil || len(closed) != 1 || closed[0].Quantity != 0 || closed[0].ExitedQty != 4 {
		t.Fatalf("full close: %+v %v", closed, err)
	}
	fee := 4 * (openFill + closeFill) * .0004
	pnl := 4*(openFill-closeFill) - fee
	fqNear(t, closed[0].RealizedPnL, pnl, "net realized short PnL")
	fqNear(t, closed[0].Fee, fee, "open and close fees")
	acct, _ := led.GetAccount(f.at.id)
	fqNear(t, acct.Cash, 10000+pnl, "cash after short round trip")
	if f.at.stockTrader != nil {
		t.Fatal("live executor constructed")
	}
}

func TestFactorQuantUSShortNotShortableAndQuoteFailure(t *testing.T) {
	f := fqUSShortTest(t)
	res := f.b.Place(fqOrder{Symbol: "NOPERPBUSDT", Side: fqSideShort, Kind: fqKindOpen, Qty: 2, Price: 50})
	if res.OK || res.Err != "NOT_SHORTABLE: NOPERPBUSDT" {
		t.Fatalf("unmapped perp accepted: %+v", res)
	}
	for _, price := range []float64{0, math.NaN(), math.Inf(1)} {
		f.perps["AAPLUSDT"] = price
		res = f.b.Place(fqOrder{Symbol: "AAPLBUSDT", Side: fqSideShort, Kind: fqKindOpen, Qty: 2, Price: 100})
		if res.OK {
			t.Fatal("invalid fresh perp price accepted")
		}
	}
	f.quoteErr = errors.New("perp offline")
	res = f.b.Place(fqOrder{Symbol: "AAPLBUSDT", Side: fqSideShort, Kind: fqKindOpen, Qty: 2, Price: 100})
	if res.OK || res.Err != "perp offline" {
		t.Fatalf("quote failure ignored: %+v", res)
	}
	rows, _ := f.at.store.StockPaper().ListOpen(f.at.id)
	if len(rows) != 0 {
		t.Fatal("failed orders changed ledger")
	}
}

func TestFactorQuantUSShortAccountAndReaders(t *testing.T) {
	f := fqUSShortTest(t)
	f.seedShort(t, 2, 100)
	if _, err := f.at.store.StockPaper().OpenOrAdd(f.at.id, "MSFTBUSDT", 1, 390, 0, 0, f.now); err != nil {
		t.Fatal(err)
	}
	f.perps["AAPLUSDT"] = 90
	f.at.stock.setPrice("AAPLBUSDT", 900) // A spot cache cannot mark a perpetual short.
	acct, err := f.b.Account()
	if err != nil || len(acct.Positions) != 2 {
		t.Fatalf("account: %+v %v", acct, err)
	}
	fqNear(t, acct.Equity, 10030, "cash + signed market values")
	fqNear(t, acct.Exposure, 580, "gross exposure")
	fqNear(t, acct.Unrealized, 30, "side-aware unrealized")
	info, err := f.at.GetAccountInfo()
	if err != nil {
		t.Fatal(err)
	}
	fqNear(t, info["total_equity"].(float64), acct.Equity, "API equity")
	fqNear(t, info["unrealized_profit"].(float64), 30, "API unrealized")
	positions, err := f.at.GetPositions()
	if err != nil || len(positions) != 2 || positions[0]["side"] != "short" {
		t.Fatalf("API positions: %+v %v", positions, err)
	}
	fqNear(t, positions[0]["market_value"].(float64), -180, "short negative market value")
	fqNear(t, positions[0]["unrealized_pnl"].(float64), 20, "short unrealized")
	fqNear(t, positions[0]["unrealized_pnl_pct"].(float64), 10, "short return")
	if snapshots := fqPositionSnapshots(acct); snapshots[0].PositionAmt != -2 || snapshots[0].UnrealizedProfit != 20 {
		t.Fatalf("decision positions: %+v", snapshots)
	}
	if err := f.at.store.FactorQuant().SetPeakEquity(f.at.id, 11000); err != nil {
		t.Fatal(err)
	}
	if err := f.at.store.FactorQuant().SetDayStart(f.at.id, f.now.Format(factorDateFmt), 10500); err != nil {
		t.Fatal(err)
	}
	risk := f.at.factorQuantRiskStatus()
	fqNear(t, risk.Equity, acct.Equity, "risk reader equity")
	fqNear(t, risk.AccountDrawdownPct, (11000-10030.0)/11000*100, "risk drawdown")
	fqNear(t, risk.DailyLossPct, (10500-10030.0)/10500*100, "risk daily loss")
	f.quoteErr = errors.New("perp offline")
	acct, err = f.b.Account()
	if err != nil {
		t.Fatal(err)
	}
	fqNear(t, acct.Positions[0].Mark, 90, "cached perp fallback")
	f.at.stock.mu.Lock()
	delete(f.at.stock.lastPerpPrices, "AAPLBUSDT")
	f.at.stock.mu.Unlock()
	acct, err = f.b.Account()
	if err != nil {
		t.Fatal(err)
	}
	fqNear(t, acct.Positions[0].Mark, 100, "entry fallback, never spot")
}

func TestFactorQuantUSShortFormatQtyFallback(t *testing.T) {
	f := fqUSShortTest(t)
	for _, tc := range []struct{ qty, price, want float64 }{{1.239, 100, 1.23}, {.001, 100, 0}, {.04, 100, 0}} {
		q, err := f.b.FormatQty("AAPLBUSDT", tc.qty, tc.price)
		if err != nil {
			t.Fatal(err)
		}
		fqNear(t, q, tc.want, "registry bStock lot fallback")
	}
}

func TestFactorQuantUSShortFundingThrottleSharedConcurrentAndExpiry(t *testing.T) {
	f := fqUSShortTest(t)
	f.seedShort(t, 2, 100)
	var calls atomic.Int32
	stockFundingSettlements = func(string, time.Time, time.Time) ([]market.FundingSettlement, error) {
		calls.Add(1)
		return nil, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// API readers create another broker; they must share the same throttle.
			b, err := f.at.newFactorQuantBroker()
			if err == nil {
				_, err = b.Account()
			}
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent account settlement calls=%d, want 1", calls.Load())
	}
	f.now = f.now.Add(30*time.Minute - time.Nanosecond)
	if _, err := f.b.Account(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("settled before 30 minutes")
	}
	f.now = f.now.Add(time.Nanosecond)
	if _, err := f.b.Account(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("did not settle at 30 minutes")
	}
}

func TestFactorQuantUSShortFundingForcedBeforeReduceAndErrorsContinue(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			f := fqUSShortTest(t)
			p := f.seedShort(t, 2, 100)
			calls := 0
			stockFundingSettlements = func(sym string, start, end time.Time) ([]market.FundingSettlement, error) {
				calls++
				if sym != "AAPLUSDT" {
					t.Fatal(sym)
				}
				if fail {
					return nil, errors.New("funding offline")
				}
				if calls == 1 {
					return nil, nil
				}
				event := market.FundingSettlement{Time: f.now.Add(-time.Second), Rate: .01, MarkPrice: 100}
				if event.Time.Before(start) {
					return nil, nil
				}
				return []market.FundingSettlement{event}, nil
			}
			for i := 0; i < 2; i++ {
				if _, err := f.b.Account(); err != nil {
					t.Fatalf("funding error blocked account: %v", err)
				}
			}
			if calls != 1 {
				t.Fatalf("funding throttle calls=%d", calls)
			}
			res := f.b.Place(fqOrder{Symbol: p.Symbol, Side: fqSideShort, Kind: fqKindReduce, Qty: 1, Price: 999})
			if !res.OK || calls != 2 {
				t.Fatalf("forced funding blocked reduce: %+v calls=%d", res, calls)
			}
			rows, err := f.at.store.StockPaper().ListOpen(f.at.id)
			if err != nil || len(rows) != 1 || rows[0].Quantity != 1 {
				t.Fatalf("reduction: %+v %v", rows, err)
			}
			funding := 2.0 // Settlement must use the two units held BEFORE reducing.
			if fail {
				funding = 0
			}
			fqNear(t, rows[0].Funding, funding, "funding before quantity change")
			account, err := f.b.Account()
			if err != nil || calls != 2 {
				t.Fatalf("forced settlement did not refresh throttle: %+v %v calls=%d", account, err, calls)
			}
			fqNear(t, account.Balance, 10200+funding-res.FillPrice-res.Fee, "cash includes funding and cover")
		})
	}
}

func TestFactorQuantUSShortFundingIncludedInAccountEquity(t *testing.T) {
	f := fqUSShortTest(t)
	f.seedShort(t, 2, 100)
	stockFundingSettlements = func(string, time.Time, time.Time) ([]market.FundingSettlement, error) {
		return []market.FundingSettlement{{Time: f.now.Add(-time.Minute), Rate: .01, MarkPrice: 100}}, nil
	}
	for i := 0; i < 2; i++ {
		acct, err := f.b.Account()
		if err != nil {
			t.Fatal(err)
		}
		fqNear(t, acct.Equity, 10002, "account equity includes newly settled funding exactly once")
	}
}

func TestFactorQuantUSShortFundingFailureDoesNotBlockRebalance(t *testing.T) {
	f := fqUSShortTest(t)
	f.seedShort(t, 2, 100)
	fqSetOwned(t, f.at, "2026-10-01")
	stockFundingSettlements = func(string, time.Time, time.Time) ([]market.FundingSettlement, error) {
		return nil, errors.New("funding offline")
	}
	cfg := f.at.config.StrategyConfig.FactorQuantConfig
	cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("us", "ok", "2026-11-05", 28, `{"MSFTBUSDT":0.5}`))
	f.at.factorQuantTick(f.b, f.now)
	rows, err := f.at.store.StockPaper().ListOpen(f.at.id)
	if err != nil || len(rows) != 1 || rows[0].Symbol != "MSFTBUSDT" || rows[0].Side != store.StockPaperLong {
		t.Fatalf("funding failure blocked close-short/open-long rebalance: %+v %v", rows, err)
	}
	records := fqRecords(t, f.at)
	if len(records) != 1 || !records[0].Success || len(records[0].Decisions) != 2 {
		t.Fatalf("rebalance failed on funding error: %+v", records)
	}
}

func TestFactorQuantUSShortTargetsFileEndToEnd(t *testing.T) {
	f := fqUSShortTest(t)
	cfg := f.at.config.StrategyConfig.FactorQuantConfig
	cfg.USSlippageBps = fqPtr(0)
	f.b.slipBps = 0
	cfg.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("us", "ok", "2026-11-05", 28, `{"MSFTBUSDT":0.5,"AAPLBUSDT":-0.5}`))
	f.at.factorQuantTick(f.b, f.now)
	rows, err := f.at.store.StockPaper().ListOpen(f.at.id)
	if err != nil || len(rows) != 2 {
		t.Fatalf("long/short targets: %+v %v", rows, err)
	}
	budget := 10000 / (1 + fqUSCostRate(cfg))
	fees := 0.0
	for _, p := range rows {
		fees += p.Fee
		price := 400.0
		if p.Side == store.StockPaperShort {
			price = 100 // Spot is 200; shorts must size and fill against the perp.
		}
		fqNear(t, p.AvgPrice, price, "directional quote")
		fqNear(t, p.Quantity, floorStep(budget*.5/price, .01), "target quantity")
	}
	acct, err := f.b.Account()
	if err != nil {
		t.Fatal(err)
	}
	fqNear(t, acct.Equity, 10000-fees, "final equity = initial - fees")
	records := fqRecords(t, f.at)
	if len(records) != 1 || !records[0].Success || len(records[0].Decisions) != 2 {
		t.Fatalf("decision record: %+v", records)
	}
	actions := map[string]bool{}
	for _, d := range records[0].Decisions {
		actions[d.Action] = d.Success
	}
	if !actions["open_long"] || !actions["open_short"] {
		t.Fatalf("orders=%v", actions)
	}
	if f.at.stockTrader != nil {
		t.Fatal("paper targets created a live executor")
	}
}

func TestFactorQuantUSShortDirectionFlipsKeepLongQuotes(t *testing.T) {
	f := fqUSShortTest(t)
	f.b.feeBps, f.b.shortFeeBps, f.b.slipBps = 0, 0, 0
	if _, err := f.at.store.StockPaper().OpenOrAdd(f.at.id, "AAPLBUSDT", 5, 200, 0, 0, f.now); err != nil {
		t.Fatal(err)
	}
	for _, w := range []float64{-.5, .5} {
		targets := fqTestTargets("2026-11-05", map[string]float64{"AAPLBUSDT": w})
		targets.Market = "us"
		rep := fqRunRebalance(f.b, targets, fqResolvedPlanConfig(f.at.config.StrategyConfig.FactorQuantConfig))
		if rep.Err != nil || rep.failedCount() != 0 || len(rep.Results) != 2 {
			t.Fatalf("direction flip: %+v", rep)
		}
		for _, r := range rep.Results {
			price := 200.0
			if r.Order.Side == fqSideShort {
				price = 100
			}
			fqNear(t, r.FillPrice, price, "flip fill uses the position side's venue")
		}
		fqNear(t, rep.After.Equity, 10000, "spot/perp quote divergence does not create profit")
	}
}

func TestFactorQuantUSShortRiskBreakersUseSignedEquity(t *testing.T) {
	for _, kind := range []string{fqHaltDrawdown, fqHaltDaily} {
		t.Run(kind, func(t *testing.T) {
			f := fqUSShortTest(t)
			cfg := fqEnableRisk(f.at)
			cfg.DrawdownWarnPct = fqPtr(0)
			if kind == fqHaltDaily {
				cfg.MaxDrawdownPct = fqPtr(0)
			} else {
				cfg.DailyLossPct = fqPtr(0)
			}
			f.seedShort(t, 50, 100)
			fqSetOwned(t, f.at, "2026-11-05")
			ready, halted := f.at.factorQuantRiskCheck(f.b, f.now)
			if !ready || halted {
				t.Fatal("healthy short account halted")
			}
			f.perps["AAPLUSDT"] = 160 // Equity = 15000 - 8000 = 7000.
			ready, halted = f.at.factorQuantRiskCheck(f.b, f.now.Add(time.Minute))
			if !ready || !halted || fqRiskState(t, f.at).HaltKind != kind {
				t.Fatalf("short loss did not trigger %s", kind)
			}
			rows, err := f.at.store.StockPaper().ListOpen(f.at.id)
			if err != nil || len(rows) != 0 {
				t.Fatalf("short breaker failed to flatten: %+v %v", rows, err)
			}
		})
	}
}
