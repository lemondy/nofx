package trader

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"nofx/kernel/stockengine"
	"nofx/market/usstock"
	"nofx/store"
)

// Channels establish happens-before edges for the fixture's non-concurrent
// fields. Cleanup always releases and joins the cycle before restoring seams.
func stockReview1029Run(t *testing.T, fx *stockFixture, ctx context.Context) func() {
	t.Helper()
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		err = fx.at.runStockCycle(ctx)
	}()
	wait := func() {
		t.Helper()
		select {
		case <-done:
			if err != nil {
				t.Errorf("cycle: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("stock cycle did not finish")
		}
	}
	t.Cleanup(wait)
	return wait
}

func stockReview1029BlockAI(t *testing.T, fx *stockFixture, ctx context.Context) (release, wait func()) {
	t.Helper()
	entered, resume := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	respond := fx.ai.respond
	fx.ai.respond = func(call int) string {
		enterOnce.Do(func() { close(entered) })
		<-resume
		return respond(call)
	}
	wait = stockReview1029Run(t, fx, ctx)
	release = func() { releaseOnce.Do(func() { close(resume) }) }
	t.Cleanup(release)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("LLM was not called")
	}
	return release, wait
}

func stockReview1029Unlocked(t *testing.T, at *AutoTrader) {
	t.Helper()
	mu := at.stockMutex()
	if !mu.TryLock() {
		t.Fatal("stock execution mutex is still locked")
	}
	mu.Unlock()
}

func stockReview1029Account(t *testing.T, fx *stockFixture, paper bool) {
	t.Helper()
	if paper {
		return
	}
	// Exercise the real account registry and the mutex used by crypto peers.
	fx.at.exchangeID = t.Name()
	peer := &AutoTrader{userID: fx.at.userID, exchangeID: fx.at.exchangeID}
	if fx.at.stockMutex() != peer.executionMutex() {
		t.Fatal("live stock and peer do not share the execution mutex")
	}
	t.Cleanup(func() { unregisterAccountTrader(peer); unregisterAccountTrader(fx.at) })
}

func stockReview1029Seed(t *testing.T, fx *stockFixture, paper bool, qty float64) {
	t.Helper()
	if paper {
		if _, err := fx.at.store.StockPaper().EnsureAccount(fx.at.id, 10000); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.at.store.StockPaper().OpenOrAdd(fx.at.id, "AAPLBUSDT", qty, 110, 100, 130, stockTestNow); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := fx.at.stockRecordBuy("AAPLBUSDT", qty, 110, 0, "seed1029", 100, stockTestNow); err != nil {
		t.Fatal(err)
	}
	fx.spot.hold["AAPLBUSDT"] = qty
	if err := fx.at.stockPlaceProtection(context.Background(), "AAPLBUSDT", qty, 100, 130, 120); err != nil {
		t.Fatal(err)
	}
}

func TestStockReview1029LLMReleasesExecutionLock(t *testing.T) {
	for _, paper := range []bool{true, false} {
		t.Run(fmt.Sprintf("paper=%v", paper), func(t *testing.T) {
			fx := newStockFixture(t, paper, stockTestNow, func(int) string { return openAAPL })
			stockReview1029Account(t, fx, paper)
			release, wait := stockReview1029BlockAI(t, fx, context.Background())
			stockReview1029Unlocked(t, fx.at)
			release()
			wait()
			rec := fx.lastRecord(t)
			if len(rec.Decisions) != 1 || !rec.Decisions[0].Success {
				t.Fatalf("decision did not execute: %+v", rec.Decisions)
			}
			if paper {
				if paperQty(t, fx) <= 0 {
					t.Fatal("paper entry not booked")
				}
			} else if len(fx.spot.buys) != 1 {
				t.Fatalf("live buys = %v", fx.spot.buys)
			}
			stockReview1029Unlocked(t, fx.at)
		})
	}
}

func TestStockReview1029OwnershipChangedDuringLLM(t *testing.T) {
	for _, paper := range []bool{true, false} {
		for _, missing := range []bool{true, false} {
			for _, action := range []string{"close_long", "reduce_long", "adjust_stop", "add_long", "hold", "wait"} {
				t.Run(fmt.Sprintf("paper=%v/missing=%v/%s", paper, missing, action), func(t *testing.T) {
					response := fmt.Sprintf(`[{"symbol":"AAPLBUSDT","action":%q,"reduce_fraction":0.5,"stop_loss":115,"take_profit":130,"confidence":80,"thesis":"trend continuation over weeks","invalidation":"daily close below 115"}]`, action)
					fx := newStockFixture(t, paper, stockTestNow, func(int) string { return response })
					stockReview1029Account(t, fx, paper)
					stockReview1029Seed(t, fx, paper, 10)
					release, wait := stockReview1029BlockAI(t, fx, context.Background())
					stockReview1029Unlocked(t, fx.at)
					if missing {
						// The API is allowed to close while the model is still deciding.
						if _, err := fx.at.ForceCloseStockPosition("AAPLBUSDT"); err != nil {
							t.Fatal(err)
						}
					} else {
						mu := fx.at.stockMutex()
						mu.Lock()
						var err error
						if paper {
							_, _, err = fx.at.store.StockPaper().Reduce(fx.at.id, "AAPLBUSDT", 5, 120, stockTestNow, "manual_reduce")
						} else {
							var st *stockState
							st, err = fx.at.stockReadState(true)
							if err == nil {
								_, err = fx.at.stockSellLive(context.Background(), st.Holdings["AAPLBUSDT"], 5, false, "manual_reduce", stockTestNow)
							}
						}
						mu.Unlock()
						if err != nil {
							t.Fatal(err)
						}
					}
					sells, protections := len(fx.spot.sells), len(fx.spot.setProt)
					release()
					wait()
					rec := fx.lastRecord(t)
					if len(rec.Decisions) != 1 {
						t.Fatalf("decisions: %+v", rec.Decisions)
					}
					a := rec.Decisions[0]
					if action == "hold" || action == "wait" {
						if !a.Success || a.Error != "" {
							t.Fatalf("noop was rejected: %+v", a)
						}
					} else if a.Success || !strings.Contains(a.Error, "STATE_CHANGED") {
						t.Fatalf("stale ownership was accepted: %+v", a)
					}
					if len(fx.spot.sells) != sells || len(fx.spot.setProt) != protections || len(fx.spot.buys) != 0 {
						t.Fatal("stale decision sent an order")
					}
					if paper {
						want := 5.0
						if missing {
							want = 0
						}
						if got := paperQty(t, fx); !near(got, want) {
							t.Fatalf("stale decision mutated ledger: qty=%v want=%v", got, want)
						}
					}
					stockReview1029Unlocked(t, fx.at)
				})
			}
		}
	}
}

func TestStockReview1029OpenRejectsNewOwnedHolding(t *testing.T) {
	for _, paper := range []bool{true, false} {
		t.Run(fmt.Sprintf("paper=%v", paper), func(t *testing.T) {
			fx := newStockFixture(t, paper, stockTestNow, func(int) string { return openAAPL })
			stockReview1029Account(t, fx, paper)
			release, wait := stockReview1029BlockAI(t, fx, context.Background())
			stockReview1029Unlocked(t, fx.at)
			mu := fx.at.stockMutex()
			mu.Lock()
			func() {
				defer mu.Unlock()
				stockReview1029Seed(t, fx, paper, 10)
			}()
			release()
			wait()
			rec := fx.lastRecord(t)
			if len(rec.Decisions) != 1 || rec.Decisions[0].Success || !strings.Contains(rec.Decisions[0].Error, "STATE_CHANGED") {
				t.Fatalf("open accepted a new holding: %+v", rec.Decisions)
			}
			if len(fx.spot.buys) != 0 || (paper && !near(paperQty(t, fx), 10)) {
				t.Fatal("duplicate entry executed")
			}
			if !strings.Contains(strings.Join(fx.alert, "\n"), "AAPLBUSDT STATE_CHANGED") || !strings.Contains(strings.Join(rec.ExecutionLog, "\n"), "rejected: STATE_CHANGED") {
				t.Fatal("entry rejection was not recorded and notified")
			}
		})
	}
}

func TestStockReview1029StoppedDuringLLM(t *testing.T) {
	for _, paper := range []bool{true, false} {
		for _, cancelOnly := range []bool{true, false} {
			t.Run(fmt.Sprintf("paper=%v/cancel=%v", paper, cancelOnly), func(t *testing.T) {
				fx := newStockFixture(t, paper, stockTestNow, func(int) string { return openAAPL })
				stockReview1029Account(t, fx, paper)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				release, wait := stockReview1029BlockAI(t, fx, ctx)
				stockReview1029Unlocked(t, fx.at)
				if cancelOnly {
					cancel()
				} else {
					fx.at.Stop()
				}
				release()
				wait()
				if len(fx.spot.buys) != 0 || (paper && paperQty(t, fx) != 0) {
					t.Fatal("entry executed after stop/cancellation")
				}
				stockReview1029Unlocked(t, fx.at)
			})
		}
	}
}

func TestStockReview1029RegimeRefreshesAfterDataFetch(t *testing.T) {
	fx := regimeFixture(t, .5, nil, stockTestNow)
	fx.ai.respond = func(int) string { return closeAAPL }
	entered, resume := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	series := stockGetSeries
	stockGetSeries = func(ctx context.Context, sym, tf string, need int, yahoo bool) (*usstock.Series, error) {
		once.Do(func() { close(entered); <-resume })
		return series(ctx, sym, tf, need, yahoo)
	}
	wait := stockReview1029Run(t, fx, context.Background())
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	t.Cleanup(release)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("series fetch was not reached")
	}
	stockReview1029Unlocked(t, fx.at)
	mu := fx.at.stockMutex()
	mu.Lock()
	_, err := fx.at.store.StockPaper().OpenOrAdd(fx.at.id, "AAPLBUSDT", 6, 120, 115, 130, stockTestNow)
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	release()
	wait()
	rec := fx.lastRecord(t)
	trims := regimeTrimActions(rec)
	if len(trims) != 1 || !trims[0].Success || !near(trims[0].Quantity, 8) {
		t.Fatalf("regime used stale pre-fetch quantity: %+v", rec.Decisions)
	}
	if !strings.Contains(fx.ai.user, "qty=8.000000") || len(rec.Decisions) != 2 || !rec.Decisions[1].Success || !near(rec.Decisions[1].Quantity, 8) || paperQty(t, fx) != 0 {
		t.Fatalf("execution did not use post-trim prompt state: %+v", rec.Decisions)
	}
}

func TestStockReview1029ExecutionSizesFreshAccount(t *testing.T) {
	for _, paper := range []bool{true, false} {
		t.Run(fmt.Sprintf("paper=%v", paper), func(t *testing.T) {
			fx := newStockFixture(t, paper, stockTestNow, func(int) string { return openAAPL })
			stockReview1029Account(t, fx, paper)
			release, wait := stockReview1029BlockAI(t, fx, context.Background())
			stockReview1029Unlocked(t, fx.at)
			mu := fx.at.stockMutex()
			mu.Lock()
			if paper {
				_, err := fx.at.store.StockPaper().OpenOrAdd(fx.at.id, "MSFTBUSDT", 80, 120, 110, 0, stockTestNow)
				if err != nil {
					mu.Unlock()
					t.Fatal(err)
				}
			} else {
				fx.spot.mu.Lock()
				fx.spot.usdt = 400
				fx.spot.hold["MSFTBUSDT"] = 80 // manual holdings also consume exposure
				fx.spot.mu.Unlock()
			}
			mu.Unlock()
			release()
			wait()
			rec := fx.lastRecord(t)
			if len(rec.Decisions) != 1 || rec.Decisions[0].Success || !strings.Contains(rec.Decisions[0].Error, "NO_CAPACITY") || len(fx.spot.buys) != 0 {
				t.Fatalf("entry used stale exposure/cash: %+v buys=%v", rec.Decisions, fx.spot.buys)
			}
		})
	}
}

func TestStockReview1029LockedPhaseFailureAndPanicUnlock(t *testing.T) {
	for _, phase := range []string{"A", "C", "E"} {
		for _, panics := range []bool{false, true} {
			t.Run(fmt.Sprintf("phase=%s/panic=%v", phase, panics), func(t *testing.T) {
				fx := newStockFixtureCfg(t, false, stockTestNow, func(int) string { return holdAAPL }, func(sc *store.StockConfig) {
					if phase == "C" {
						sc.RegimeScaler = store.StockRegimeBasketVol
					}
				})
				stockReview1029Account(t, fx, false)
				hit := false
				inject := func() {
					fx.spot.onPositions = func() {
						hit = true
						if fx.at.stockMutex().TryLock() {
							fx.at.stockMutex().Unlock()
							t.Error("account gather ran outside the locked phase")
						}
						panic("locked phase " + phase)
					}
					if !panics {
						fx.spot.onPositions = nil
						fx.spot.balanceErr = fmt.Errorf("locked phase %s failure", phase)
					}
				}
				if phase == "A" {
					inject()
				} else if phase == "C" {
					series := stockGetSeries
					stockGetSeries = func(ctx context.Context, sym, tf string, need int, yahoo bool) (*usstock.Series, error) {
						inject()
						return series(ctx, sym, tf, need, yahoo)
					}
				} else {
					fx.ai.respond = func(int) string { inject(); return holdAAPL }
				}
				if panics {
					fx.at.runStockCycleSafe(context.Background())
					if !hit {
						t.Fatal("locked-phase panic was not reached")
					}
				} else {
					if err := fx.at.runStockCycle(context.Background()); err == nil {
						t.Fatal("gather failure was swallowed")
					}
					rec := fx.lastRecord(t)
					if rec.Success || !strings.Contains(rec.ErrorMessage, "locked phase "+phase+" failure") {
						t.Fatalf("failed cycle was not saved: %+v", rec)
					}
				}
				stockReview1029Unlocked(t, fx.at)
			})
		}
	}
}

func TestStockReview1029OwnershipTolerance(t *testing.T) {
	for _, action := range []string{stockengine.ActionAddLong, stockengine.ActionCloseLong, stockengine.ActionReduceLong, stockengine.ActionAdjustStop} {
		for _, qty := range []float64{.001, 10, 1e6} {
			prompt := &stockHolding{Owned: qty}
			if stockDecisionStateChanged(action, prompt, &stockHolding{Owned: qty * (1 + 1e-8)}) {
				t.Fatalf("%s rejected float noise at qty=%g", action, qty)
			}
			if !stockDecisionStateChanged(action, prompt, &stockHolding{Owned: qty * 1.001}) || !stockDecisionStateChanged(action, prompt, nil) || !stockDecisionStateChanged(action, prompt, &stockHolding{Owned: math.NaN()}) {
				t.Fatalf("%s missed an ownership change at qty=%g", action, qty)
			}
		}
	}
}
