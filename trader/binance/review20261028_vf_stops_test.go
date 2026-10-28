package binance

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type vfStopEnv struct {
	posts, cancels, tickers atomic.Int32
	workingTypes            []string
	last                    string // ticker/price answer; "" = ticker unavailable
	postCodes               []int  // per-POST Binance error code, 0 = accept
	clock                   time.Time
}

func (e *vfStopEnv) trader(t *testing.T, thin bool) *FuturesTrader {
	t.Helper()
	tr := fapiTestTrader(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/exchangeInfo":
			io.WriteString(w, `{"symbols":[{"symbol":"XUSDT","pricePrecision":1,"filters":[{"filterType":"PRICE_FILTER","tickSize":"0.1"}]}]}`)
		case "/fapi/v1/ticker/price", "/fapi/v2/ticker/price":
			e.tickers.Add(1)
			if e.last == "" {
				http.Error(w, `{"code":-1000,"msg":"down"}`, 500)
				return
			}
			fmt.Fprintf(w, `[{"symbol":"XUSDT","price":%q}]`, e.last)
		case "/fapi/v1/openOrders":
			io.WriteString(w, `[]`)
		case "/fapi/v1/openAlgoOrders":
			io.WriteString(w, `[{"algoId":1,"symbol":"XUSDT","orderType":"STOP_MARKET","positionSide":"LONG","triggerPrice":"98.5","closePosition":true}]`)
		case "/fapi/v1/algoOrder":
			if r.Method == http.MethodDelete {
				e.cancels.Add(1)
				io.WriteString(w, `{"code":"200"}`)
				return
			}
			r.ParseForm()
			e.workingTypes = append(e.workingTypes, r.Form.Get("workingType"))
			n := int(e.posts.Add(1))
			code := 0
			if n <= len(e.postCodes) {
				code = e.postCodes[n-1]
			}
			if code == 0 {
				fmt.Fprintf(w, `{"algoId":%d}`, 100+n)
				return
			}
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"code":%d,"msg":"Order would immediately trigger."}`, code)
		default:
			t.Errorf("unexpected endpoint %s", r.URL)
			http.Error(w, "unexpected", 404)
		}
	})
	e.clock = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	tr.nowFn = func() time.Time { return e.clock }
	tr.oiValueFn = func(string) (float64, bool) { return 10, thin }
	return tr
}

// Review 2026-10-28 #2: MARK=100.2, CONTRACT=99.9, trailing SL=100.1. The
// CONTRACT trigger is already crossed; the pre-check must stop it BEFORE any
// exchange write (no cancel of the old protection, no POST).
func TestReview20261028VFStopPreflightBlocksBeforeCancel(t *testing.T) {
	for _, tc := range []struct {
		name, side string
		mark, last string
		stop       float64
	}{
		{"contract_long", "LONG", "100.2", "99.9", 100.1},
		{"contract_short", "SHORT", "99.8", "100.1", 99.9},
		{"tick_rounded_equal", "LONG", "100.2", "100.1", 100.09},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &vfStopEnv{last: tc.last, postCodes: []int{-4130, -2021}}
			tr := e.trader(t, false)
			var m float64
			fmt.Sscanf(tc.mark, "%g", &m)
			tr.ObserveStopMarkPrice("XUSDT", m)
			if err := tr.SetStopLoss("XUSDT", tc.side, 1, tc.stop); err == nil {
				t.Fatal("candidate already crossed by the working-type price was accepted")
			}
			if e.cancels.Load() != 0 || e.posts.Load() != 0 {
				t.Fatalf("cancel=%d POST=%d, want 0/0", e.cancels.Load(), e.posts.Load())
			}
		})
	}
}

// MARK_PRICE symbols keep their behaviour: the same candidate is legal against
// MARK (100.2 > 100.1) and is submitted untouched, with no extra ticker read.
func TestReview20261028VFStopMarkPriceSymbolUnchanged(t *testing.T) {
	e := &vfStopEnv{last: "99.9"}
	tr := e.trader(t, true)
	tr.ObserveStopMarkPrice("XUSDT", 100.2)
	if err := tr.SetStopLoss("XUSDT", "LONG", 1, 100.1); err != nil {
		t.Fatalf("MARK_PRICE candidate rejected: %v", err)
	}
	if e.posts.Load() != 1 || e.workingTypes[0] != "MARK_PRICE" {
		t.Fatalf("POST=%d workingTypes=%v", e.posts.Load(), e.workingTypes)
	}
	if e.tickers.Load() != 0 {
		t.Errorf("MARK_PRICE path must not read the ticker, got %d", e.tickers.Load())
	}
	// And a MARK-crossed candidate is blocked locally.
	if err := tr.SetStopLoss("XUSDT", "LONG", 1, 100.3); err == nil || e.posts.Load() != 1 {
		t.Errorf("candidate above MARK accepted: err=%v POST=%d", err, e.posts.Load())
	}
}

// Without any observed reference price (callers that never feed MARK, stale
// samples) nothing is blocked locally: the exchange stays the final judge, so
// initial/repair stops are never refused on missing data.
func TestReview20261028VFStopNoFreshReferenceDoesNotBlock(t *testing.T) {
	e := &vfStopEnv{last: "99.9"}
	tr := e.trader(t, false)
	if err := tr.SetStopLoss("XUSDT", "LONG", 1, 100.1); err != nil || e.posts.Load() != 1 {
		t.Fatalf("no observed price: err=%v POST=%d", err, e.posts.Load())
	}
	tr.ObserveStopMarkPrice("XUSDT", 100.2)
	e.clock = e.clock.Add(stopRefMaxAge + time.Second) // sample is stale now
	if err := tr.SetStopLoss("XUSDT", "LONG", 1, 100.1); err != nil || e.posts.Load() != 2 {
		t.Fatalf("stale reference blocked a stop: err=%v POST=%d", err, e.posts.Load())
	}
}

// -2021 from the exchange (reference unknown locally: MARK only, ticker down)
// starts a backoff; neither the direct nor the -4130 replacement variant
// repeats the same request every cycle, and a less aggressive candidate or the
// end of the window is allowed through.
func TestReview20261028VFStopImmediateTriggerBackoff(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%v", replace), func(t *testing.T) {
			e := &vfStopEnv{last: ""}
			e.postCodes = []int{-2021}
			wantPosts, wantCancels := int32(1), int32(0)
			if replace {
				// -4130, then the replacement is -2021, then the restore of
				// the old stop succeeds.
				e.postCodes = []int{-4130, -2021, 0}
				wantPosts, wantCancels = 3, 1
			}
			tr := e.trader(t, false)
			tr.ObserveStopMarkPrice("XUSDT", 100.2)
			for i := 0; i < 3; i++ {
				if err := tr.SetStopLoss("XUSDT", "LONG", 1, 100.1); err == nil {
					t.Fatalf("attempt %d: expected rejection", i)
				}
			}
			if e.posts.Load() != wantPosts || e.cancels.Load() != wantCancels {
				t.Fatalf("POST=%d cancel=%d, want %d/%d (backoff must stop repeats)", e.posts.Load(), e.cancels.Load(), wantPosts, wantCancels)
			}
			// A more aggressive candidate is also held back.
			if err := tr.SetStopLoss("XUSDT", "LONG", 1, 100.15); err == nil || e.posts.Load() != wantPosts {
				t.Fatalf("more aggressive candidate must stay in backoff: err=%v POST=%d", err, e.posts.Load())
			}
			// A less aggressive one goes to the exchange.
			before := e.posts.Load()
			tr.SetStopLoss("XUSDT", "LONG", 1, 99.0)
			if e.posts.Load() != before+1 {
				t.Fatalf("less aggressive candidate was not attempted")
			}
			// After the window the original candidate is retried.
			e.clock = e.clock.Add(stopImmediateTriggerBackoff + time.Second)
			tr.ObserveStopMarkPrice("XUSDT", 100.2)
			before = e.posts.Load()
			tr.SetStopLoss("XUSDT", "LONG", 1, 100.1)
			if e.posts.Load() == before {
				t.Fatal("candidate never retried after the backoff window")
			}
		})
	}
}
