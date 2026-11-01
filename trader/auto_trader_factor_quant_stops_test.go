package trader

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/trader/paper"
)

func TestFuturesBrokerRefreshesBothStopsAfterEveryFill(t *testing.T) {
	for _, side := range []string{fqSideLong, fqSideShort} {
		t.Run(side, func(t *testing.T) {
			f := newFakeFutures(10000, map[string]float64{"XUSDT": 100})
			b := newFQFuturesBroker(f, 3, 0.2, 0.2)
			factor := 1.2
			if side == fqSideLong {
				factor = 0.8
			}
			for _, tc := range []struct {
				kind                        string
				qty, mark, remaining, entry float64
			}{
				{fqKindOpen, 10, 100, 10, 100},
				{fqKindIncrease, 5, 110, 15, 310.0 / 3},
				{fqKindReduce, 3, 110, 12, 310.0 / 3},
			} {
				f.prices["XUSDT"] = tc.mark
				res := b.Place(fqOrder{Symbol: "XUSDT", Side: side, Kind: tc.kind, Qty: tc.qty})
				if res.failed() || res.StopExited {
					t.Fatalf("%s: %+v", tc.kind, res)
				}
				fqNear(t, res.StopPrice, tc.entry*factor, tc.kind+" stop")
				fqNear(t, f.pos["XUSDT:"+side].qty, tc.remaining, tc.kind+" quantity")
				want := fmt.Sprintf("stop XUSDT %s %.8g @%.8g", strings.ToUpper(side), tc.remaining, tc.entry*factor)
				if f.calls[len(f.calls)-1] != want {
					t.Fatalf("%s: want %q, calls=%v", tc.kind, want, f.calls)
				}
			}
			res := b.Place(fqOrder{Symbol: "XUSDT", Side: side, Kind: fqKindClose, CloseAll: true})
			if res.failed() || res.StopPrice != 0 || len(f.stops) != 0 || len(f.pos) != 0 {
				t.Fatalf("full close: %+v positions=%v stops=%v", res, f.pos, f.stops)
			}
		})
	}
}

func TestFuturesBrokerBreachedStopsExitAtMarket(t *testing.T) {
	for _, tc := range []struct {
		side string
		mark float64
	}{
		{fqSideLong, 80}, {fqSideLong, 79}, {fqSideShort, 120}, {fqSideShort, 121},
	} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-%g-fail=%v", tc.side, tc.mark, fail), func(t *testing.T) {
				f := newFakeFutures(10000, map[string]float64{"XUSDT": tc.mark})
				f.pos["XUSDT:"+tc.side] = &fakePos{"XUSDT", tc.side, 5, 100}
				f.failClose["XUSDT"] = fail
				out := newFQFuturesBroker(f, 3, 0.2, 0.2).ensureStops()
				if len(out) != 1 {
					t.Fatalf("results=%+v", out)
				}
				r := out[0]
				if r.Side != tc.side || r.Exited == fail || (r.Err != "") != fail || len(f.stops) != 0 {
					t.Fatalf("result=%+v stops=%v", r, f.stops)
				}
				if !fail && (r.Note != "止损价已越过，已市价平仓" || len(f.pos) != 0) {
					t.Fatalf("exit=%+v positions=%v", r, f.pos)
				}
				if fqCountCalls(f, "close_"+tc.side+" XUSDT 0") != 1 {
					t.Fatalf("close all missing: %v", f.calls)
				}
			})
		}
	}
}

func TestFuturesBrokerReportsStopExitAfterReduction(t *testing.T) {
	f := newFakeFutures(10000, map[string]float64{"XUSDT": 75})
	f.pos["XUSDT:long"] = &fakePos{"XUSDT", fqSideLong, 10, 100}
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	res := b.Place(fqOrder{Symbol: "XUSDT", Side: fqSideLong, Kind: fqKindReduce, Qty: 2})
	if res.failed() || !res.StopExited || res.Note != "止损价已越过，已市价平仓" || len(f.pos) != 0 {
		t.Fatalf("reduction exit=%+v positions=%v", res, f.pos)
	}
	fqNear(t, res.StopPrice, 80, "breached stop")
}

func TestFuturesBrokerStopDisabledPerSide(t *testing.T) {
	for _, disabled := range []string{fqSideLong, fqSideShort, "both"} {
		t.Run(disabled, func(t *testing.T) {
			f := newFakeFutures(10000, map[string]float64{"XUSDT": 100})
			longPct, shortPct := 0.2, 0.2
			if disabled == fqSideLong || disabled == "both" {
				longPct = 0
			}
			if disabled == fqSideShort || disabled == "both" {
				shortPct = 0
			}
			for _, side := range []string{fqSideLong, fqSideShort} {
				f.pos["XUSDT:"+side] = &fakePos{"XUSDT", side, 10, 100}
				f.stops["XUSDT:"+strings.ToUpper(side)] = 150 // preexisting protection
			}
			b := newFQFuturesBroker(f, 3, longPct, shortPct)
			out := b.ensureStops()
			want := 1
			if disabled == "both" {
				want = 0
			}
			if len(out) != want || len(f.stops) != want {
				t.Fatalf("results=%+v stops=%v", out, f.stops)
			}
			for _, side := range []string{fqSideLong, fqSideShort} {
				res := b.Place(fqOrder{Symbol: "XUSDT", Side: side, Kind: fqKindIncrease, Qty: 1})
				if res.failed() {
					t.Fatalf("increase=%+v", res)
				}
				if (res.StopPrice == 0) != (disabled == side || disabled == "both") {
					t.Fatalf("side=%s result=%+v", side, res)
				}
			}
		})
	}
}

func TestFactorQuantLoopStartMigratesExistingPaperStops(t *testing.T) {
	at := fqPaperTrader(t, fqTestStore(t), "fq-start-migrate", "crypto", &paperTestPrices{mark: 100})
	pt := at.trader.(*paper.PaperFuturesTrader)
	if _, err := pt.OpenShort("XUSDT", 5, 3); err != nil {
		t.Fatal(err)
	}
	if err := pt.SetStopLoss("XUSDT", "SHORT", 5, 150); err != nil {
		t.Fatal(err)
	}
	if _, err := pt.OpenLong("XUSDT", 5, 3); err != nil {
		t.Fatal(err)
	}
	// Missing targets prevent any rebalance. Startup must still repair both sides.
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = filepath.Join(t.TempDir(), "missing.json")
	at.stopMonitorCh = make(chan struct{})
	close(at.stopMonitorCh) // run the actual startup and first tick, then return
	if err := at.runFactorQuantLoop(); err != nil {
		t.Fatal(err)
	}
	orders, err := pt.GetOpenOrders("XUSDT")
	if err != nil || len(orders) != 2 {
		t.Fatalf("orders=%+v err=%v", orders, err)
	}
	for _, o := range orders {
		want, action := 120.0, "BUY"
		if o.PositionSide == "LONG" {
			want, action = 80, "SELL"
		}
		if o.Type != "STOP_MARKET" || o.Side != action || !o.ClosePosition || o.Quantity != 0 {
			t.Fatalf("paper stop semantics: %+v", o)
		}
		fqNear(t, o.StopPrice, want, o.PositionSide+" migrated stop")
	}
	if state, err := at.store.FactorQuant().Get(at.id); err != nil || (state != nil && state.LastAsOf != "") {
		t.Fatalf("unexpected rebalance=%+v %v", state, err)
	}
}

func TestFactorQuantPaperStopsAfterReductionAndFullClose(t *testing.T) {
	at := fqPaperTrader(t, fqTestStore(t), "fq-paper-reduce", "crypto", &paperTestPrices{mark: 100})
	b, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{fqSideLong, fqSideShort} {
		res := b.Place(fqOrder{Symbol: "XUSDT", Side: side, Kind: fqKindOpen, Qty: 5})
		if res.failed() {
			t.Fatalf("open=%+v", res)
		}
	}
	for i, side := range []string{fqSideLong, fqSideShort} {
		before, err := at.trader.GetOpenOrders("XUSDT")
		if err != nil {
			t.Fatal(err)
		}
		oldID := ""
		for _, o := range before {
			if o.PositionSide == strings.ToUpper(side) {
				oldID = o.OrderID
			}
		}
		res := b.Place(fqOrder{Symbol: "XUSDT", Side: side, Kind: fqKindReduce, Qty: 2})
		if res.failed() {
			t.Fatalf("reduce=%+v", res)
		}
		after, err := at.trader.GetOpenOrders("XUSDT")
		if err != nil || len(after) != 2-i {
			t.Fatalf("reduce orders=%+v %v", after, err)
		}
		for _, o := range after {
			if o.PositionSide == strings.ToUpper(side) && (o.OrderID == oldID || !o.ClosePosition) {
				t.Fatalf("stop not replaced: %+v", o)
			}
		}
		res = b.Place(fqOrder{Symbol: "XUSDT", Side: side, Kind: fqKindClose, CloseAll: true})
		if res.failed() || res.StopPrice != 0 {
			t.Fatalf("close=%+v", res)
		}
		orders, err := at.trader.GetOpenOrders("XUSDT")
		if err != nil || len(orders) != 1-i {
			t.Fatalf("stale stops=%+v %v", orders, err)
		}
		for _, o := range orders {
			if o.PositionSide == strings.ToUpper(side) {
				t.Fatalf("closed side stop remains: %+v", o)
			}
		}
	}
}

func TestFactorQuantCompletedRebalanceProtectsPositionsInsideBand(t *testing.T) {
	at := fqPaperTrader(t, fqTestStore(t), "fq-band-protect", "crypto", &paperTestPrices{mark: 100})
	fqSetOwned(t, at, "2026-10-28")
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, `{"LONGUSDT":0.5,"SHORTUSDT":-0.5}`))
	f := newFakeFutures(1000, map[string]float64{"LONGUSDT": 100, "SHORTUSDT": 100})
	f.pos["LONGUSDT:long"] = &fakePos{"LONGUSDT", fqSideLong, 5, 100}
	f.pos["SHORTUSDT:short"] = &fakePos{"SHORTUSDT", fqSideShort, 5, 100}
	f.failStop["LONGUSDT"] = true
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	at.factorQuantTick(b, fqDay("2026-11-01"))
	fqCheckRetry(t, at, "2026-10-31", 0)
	if fqCountCalls(f, "open_") != 0 || fqCountCalls(f, "close_") != 0 {
		t.Fatalf("traded inside band: %v", f.calls)
	}
	fqNear(t, f.stops["SHORTUSDT:SHORT"], 120, "short repaired inside band")
	f.failStop["LONGUSDT"] = false
	at.factorQuantTick(b, fqDay("2026-11-01").Add(5*time.Minute))
	fqCheckRetry(t, at, "", 0)
	fqNear(t, f.stops["LONGUSDT:LONG"], 80, "long repaired by retry")
	if recs := fqRecords(t, at); len(recs) != 2 || recs[1].Success || !recs[0].Success || !strings.Contains(recs[1].CoTTrace, "补放止损（多） LONGUSDT 失败") {
		t.Fatalf("protection reports=%+v", recs)
	}
}

func TestFactorQuantStartupProtectionFailureDoesNotBlockTick(t *testing.T) {
	at := fqPaperTrader(t, fqTestStore(t), "fq-start-failure", "crypto", &paperTestPrices{mark: 100})
	fqSetOwned(t, at, "2026-10-28")
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = writeTargetsFile(t, t.TempDir(), targetsJSON("crypto", "ok", "2026-10-31", 3, `{"LONGUSDT":0.5,"SHORTUSDT":-0.5}`))
	f := newFakeFutures(1000, map[string]float64{"LONGUSDT": 100, "SHORTUSDT": 100})
	f.pos["LONGUSDT:long"] = &fakePos{"LONGUSDT", fqSideLong, 5, 100}
	f.failStop["LONGUSDT"] = true
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	at.factorQuantInitStops(b)
	at.factorQuantTick(b, fqDay("2026-11-01"))
	state := fqCheckRetry(t, at, "2026-10-31", 0)
	if state.LastAsOf != "2026-10-31" || f.pos["SHORTUSDT:short"] == nil {
		t.Fatalf("startup stop failure blocked rebalance: %+v calls=%v", state, f.calls)
	}
}

func TestFactorQuantPaperLongAndShortStopsTriggerForRemainingQuantity(t *testing.T) {
	for _, side := range []string{fqSideLong, fqSideShort} {
		t.Run(side, func(t *testing.T) {
			prices := &paperTestPrices{mark: 100}
			at := fqPaperTrader(t, fqTestStore(t), "fq-trigger-"+side, "crypto", prices)
			b, err := at.newFactorQuantBroker()
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range []fqOrder{
				{Symbol: "XUSDT", Side: side, Kind: fqKindOpen, Qty: 5},
				{Symbol: "XUSDT", Side: side, Kind: fqKindReduce, Qty: 2},
			} {
				if res := b.Place(o); res.failed() {
					t.Fatalf("place=%+v", res)
				}
			}
			prices.mark = 79
			if side == fqSideShort {
				prices.mark = 121
			}
			pt := at.trader.(*paper.PaperFuturesTrader)
			if err := pt.Advance(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if positions, err := pt.GetPositions(); err != nil || len(positions) != 0 {
				t.Fatalf("stop positions=%+v %v", positions, err)
			}
			if orders, err := pt.GetOpenOrders("XUSDT"); err != nil || len(orders) != 0 {
				t.Fatalf("stale stop=%+v %v", orders, err)
			}
		})
	}
}
