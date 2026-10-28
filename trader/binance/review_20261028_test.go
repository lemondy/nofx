package binance

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
)

// review 2026-10-28: adapter-side execution fixes (#3/#6 TP never degrades to
// closePosition, #7 partial closes reconcile by client id, exposed rules).

type exec1028Server struct {
	mu        sync.Mutex
	algoPosts []url.Values
	orderPost []url.Values
	lookups   []string
}

// exec1028Trader builds an adapter over an in-memory exchange: LOT_SIZE
// (step 1, minQty 1, maxQty 100), tick 0.1. orderPost / lookup customize the
// market-order POST and the client-id lookup.
func exec1028Trader(t *testing.T, srv *exec1028Server, orderPost, lookup http.HandlerFunc) *FuturesTrader {
	t.Helper()
	hc := reviewKHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/exchangeInfo"):
			_, _ = io.WriteString(w, `{"symbols":[{"symbol":"XUSDT","pricePrecision":2,"quantityPrecision":0,"filters":[{"filterType":"LOT_SIZE","stepSize":"1","minQty":"1","maxQty":"100"},{"filterType":"PRICE_FILTER","tickSize":"0.1"},{"filterType":"MIN_NOTIONAL","notional":"5"}]}]}`)
		case strings.Contains(r.URL.Path, "positionRisk"):
			_, _ = io.WriteString(w, `[{"symbol":"XUSDT","positionAmt":"10","entryPrice":"100","markPrice":"100","unRealizedProfit":"0","leverage":"2","liquidationPrice":"50","positionSide":"LONG"}]`)
		case strings.HasSuffix(r.URL.Path, "/openOrders"), strings.HasSuffix(r.URL.Path, "/openAlgoOrders"):
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/fapi/v1/algoOrder" && r.Method == http.MethodPost:
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			srv.mu.Lock()
			srv.algoPosts = append(srv.algoPosts, r.Form)
			srv.mu.Unlock()
			_, _ = io.WriteString(w, `{"algoId":777,"clientAlgoId":"x","algoType":"CONDITIONAL"}`)
		case r.URL.Path == "/fapi/v1/order" && r.Method == http.MethodPost:
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			srv.mu.Lock()
			srv.orderPost = append(srv.orderPost, r.Form)
			srv.mu.Unlock()
			orderPost(w, r)
		case r.URL.Path == "/fapi/v1/order" && r.Method == http.MethodGet:
			srv.mu.Lock()
			srv.lookups = append(srv.lookups, r.URL.Query().Get("origClientOrderId"))
			srv.mu.Unlock()
			lookup(w, r)
		default:
			t.Errorf("unexpected endpoint %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	client := futures.NewClient("key", "secret")
	client.BaseURL = futuresTestURL()
	client.HTTPClient = hc
	return &FuturesTrader{client: client, cacheDuration: time.Minute}
}

func exec1028NoLookup(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected client-id lookup")
		http.Error(w, "unexpected", http.StatusNotFound)
	}
}

// #3/#6: rules exposed to the execution layer + the price quantizer is the
// adapter's own tick rounding.
func TestExec1028_ExposedRulesAndTriggerQuantizer(t *testing.T) {
	srv := &exec1028Server{}
	ft := exec1028Trader(t, srv, func(http.ResponseWriter, *http.Request) {}, exec1028NoLookup(t))
	step, minQty, err := ft.QuantityRules("XUSDT")
	if err != nil || step != 1 || minQty != 1 {
		t.Fatalf("rules step=%v min=%v err=%v", step, minQty, err)
	}
	q, err := ft.QuantizeTriggerPrice("XUSDT", 99.74)
	if err != nil || q != 99.7 {
		t.Fatalf("99.74 on a 0.1 tick must quantize to 99.7, got %v err=%v", q, err)
	}
	if _, err := ft.QuantizeTriggerPrice("XUSDT", 0); err == nil {
		t.Fatal("non-positive trigger accepted")
	}
}

// #6: an explicit quantity that cannot be formatted must fail, NEVER become a
// whole-position closePosition take-profit.
func TestExec1028_SetTakeProfitNeverDegradesToClosePosition(t *testing.T) {
	for _, tc := range []struct {
		name string
		qty  float64
	}{
		{"below_min_qty_underflow", 0.4},
		{"above_max_qty", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &exec1028Server{}
			ft := exec1028Trader(t, srv, func(http.ResponseWriter, *http.Request) {}, exec1028NoLookup(t))
			err := ft.SetTakeProfit("XUSDT", "LONG", tc.qty, 110)
			if err == nil {
				t.Fatal("unplaceable partial quantity accepted")
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if len(srv.algoPosts) != 0 {
				t.Fatalf("a TP order was sent despite the formatting error (closePosition fallback): %v", srv.algoPosts)
			}
		})
	}

	// Explicit quantity<=0 is still the closePosition mode; a valid partial
	// quantity is sent as such.
	srv := &exec1028Server{}
	ft := exec1028Trader(t, srv, func(http.ResponseWriter, *http.Request) {}, exec1028NoLookup(t))
	if err := ft.SetTakeProfit("XUSDT", "LONG", 0, 110); err != nil {
		t.Fatal(err)
	}
	if err := ft.SetTakeProfit("XUSDT", "LONG", 2, 110); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.algoPosts) != 2 {
		t.Fatalf("algo posts=%d", len(srv.algoPosts))
	}
	if srv.algoPosts[0].Get("closePosition") != "true" || srv.algoPosts[0].Get("quantity") != "" {
		t.Fatalf("quantity<=0 must use closePosition: %v", srv.algoPosts[0])
	}
	if srv.algoPosts[1].Get("quantity") != "2" || srv.algoPosts[1].Get("closePosition") == "true" {
		t.Fatalf("partial TP must carry its quantity, not closePosition: %v", srv.algoPosts[1])
	}
}

type exec1028Close func(ft *FuturesTrader, qty float64) (map[string]interface{}, error)

func exec1028Closers() map[string]exec1028Close {
	return map[string]exec1028Close{
		"long":  func(ft *FuturesTrader, q float64) (map[string]interface{}, error) { return ft.CloseLong("XUSDT", q) },
		"short": func(ft *FuturesTrader, q float64) (map[string]interface{}, error) { return ft.CloseShort("XUSDT", q) },
	}
}

// #7: the lost response of an EXECUTED partial close is resolved by looking up
// the SAME client order id — one POST, success with the confirmed fill.
func TestExec1028_PartialCloseTimeoutRecoversByClientID(t *testing.T) {
	reviewKInstantRecovery(t)
	for dir, closer := range exec1028Closers() {
		t.Run(dir, func(t *testing.T) {
			srv := &exec1028Server{}
			var submittedID string
			ft := exec1028Trader(t, srv, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `Unknown error`)
			}, func(w http.ResponseWriter, r *http.Request) {
				srv.mu.Lock()
				if len(srv.orderPost) > 0 {
					submittedID = srv.orderPost[0].Get("newClientOrderId")
				}
				srv.mu.Unlock()
				_, _ = io.WriteString(w, `{"orderId":123,"symbol":"XUSDT","clientOrderId":"`+submittedID+`","status":"FILLED","executedQty":"5","avgPrice":"100"}`)
			})
			res, err := closer(ft, 5)
			if err != nil {
				t.Fatalf("an executed partial close must be adopted, got %v", err)
			}
			srv.mu.Lock()
			defer srv.mu.Unlock()
			if len(srv.orderPost) != 1 || len(srv.lookups) != 1 || srv.lookups[0] != srv.orderPost[0].Get("newClientOrderId") {
				t.Fatalf("exactly one POST then a lookup by the SAME client id: posts=%d lookups=%v", len(srv.orderPost), srv.lookups)
			}
			if res["recoveredUnknownSubmit"] != true || res["executedQty"] != 5.0 {
				t.Fatalf("receipt must carry the recovery + confirmed fill: %v", res)
			}
		})
	}
}

func TestExec1028_PartialCloseTimeoutConfirmedAbsentAndUnknown(t *testing.T) {
	reviewKInstantRecovery(t)
	// Confirmed absent (-2013 x3): the ORIGINAL error comes back, not UNKNOWN —
	// the caller may retry, nothing executed.
	srv := &exec1028Server{}
	ft := exec1028Trader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"code":-1007,"msg":"execution status unknown"}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":-2013,"msg":"Order does not exist."}`)
	})
	_, err := ft.CloseLong("XUSDT", 5)
	var api *common.APIError
	if err == nil || errors.Is(err, ErrSubmitUnknown) || !errors.As(err, &api) || api.Code != -1007 {
		t.Fatalf("confirmed-absent close must return the original error: %v", err)
	}
	srv.mu.Lock()
	if len(srv.orderPost) != 1 || len(srv.lookups) != 3 {
		t.Fatalf("posts=%d lookups=%d", len(srv.orderPost), len(srv.lookups))
	}
	srv.mu.Unlock()

	// Lookup also unreachable: UNKNOWN, still exactly one POST.
	srv = &exec1028Server{}
	ft = exec1028Trader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"code":-1007,"msg":"execution status unknown"}`)
	}, func(w http.ResponseWriter, r *http.Request) {})
	base := ft.client.HTTPClient.Transport
	ft.client.HTTPClient.Transport = batch1Transport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == "/fapi/v1/order" {
			return nil, errors.New("lookup connection lost")
		}
		return base.RoundTrip(r)
	})
	_, err = ft.CloseShort("XUSDT", 5)
	if !errors.Is(err, ErrSubmitUnknown) {
		t.Fatalf("unresolvable partial close must surface UNKNOWN, got %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.orderPost) != 1 {
		t.Fatalf("an unknown close must not be re-POSTed: posts=%d", len(srv.orderPost))
	}
}

// A definitive exchange rejection is not ambiguous: no lookup, plain error.
func TestExec1028_PartialCloseDefinitiveRejectionNoLookup(t *testing.T) {
	srv := &exec1028Server{}
	ft := exec1028Trader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":-2022,"msg":"ReduceOnly Order is rejected."}`)
	}, exec1028NoLookup(t))
	_, err := ft.CloseLong("XUSDT", 5)
	if err == nil || errors.Is(err, ErrSubmitUnknown) {
		t.Fatalf("definitive rejection: %v", err)
	}
}

// A normal partial close asks for RESULT so the receipt carries the fill the
// execution layer sizes its TP re-arm from; a FULL close keeps the old ACK
// single-shot path (no lookup on ambiguity).
func TestExec1028_PartialCloseReceiptAndFullCloseUnchanged(t *testing.T) {
	srv := &exec1028Server{}
	ft := exec1028Trader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"orderId":9,"symbol":"XUSDT","clientOrderId":"c","status":"FILLED","executedQty":"5","origQty":"5"}`)
	}, exec1028NoLookup(t))
	res, err := ft.CloseLong("XUSDT", 5)
	if err != nil || res["executedQty"] != 5.0 {
		t.Fatalf("receipt=%v err=%v", res, err)
	}
	srv.mu.Lock()
	if got := srv.orderPost[0].Get("newOrderRespType"); got != "RESULT" {
		t.Fatalf("partial close must request RESULT, got %q", got)
	}
	srv.mu.Unlock()

	// Full close (qty 0): ambiguous 503 is NOT looked up (legacy behavior).
	srv = &exec1028Server{}
	ft = exec1028Trader(t, srv, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `Unknown error`)
	}, exec1028NoLookup(t))
	if _, err := ft.CloseLong("XUSDT", 0); err == nil {
		t.Fatal("full close with a 503 must still fail without reconciliation")
	}
}
