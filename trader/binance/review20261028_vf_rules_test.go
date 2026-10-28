package binance

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"nofx/trader/types"
)

// Filter fields copied from data/cache/futures_exchange_info.json (2026-10-26
// snapshot). The tests never read the runtime cache file.
const vfRulesJSON = `{"symbols":[
{"symbol":"BTCUSDT","pricePrecision":2,"filters":[{"filterType":"PRICE_FILTER","maxPrice":"4529764","minPrice":"556.80","tickSize":"0.10"},{"filterType":"LOT_SIZE","maxQty":"1000","minQty":"0.001","stepSize":"0.001"},{"filterType":"MARKET_LOT_SIZE","maxQty":"120","minQty":"0.001","stepSize":"0.001"},{"filterType":"MIN_NOTIONAL","notional":"50"}]},
{"symbol":"ETHUSDT","pricePrecision":2,"filters":[{"filterType":"PRICE_FILTER","maxPrice":"306177","minPrice":"39.86","tickSize":"0.01"},{"filterType":"LOT_SIZE","maxQty":"10000","minQty":"0.001","stepSize":"0.001"},{"filterType":"MARKET_LOT_SIZE","maxQty":"2000","minQty":"0.001","stepSize":"0.001"},{"filterType":"MIN_NOTIONAL","notional":"20"}]},
{"symbol":"ADAUSDT","pricePrecision":5,"filters":[{"filterType":"PRICE_FILTER","maxPrice":"20000","minPrice":"0.00010","tickSize":"0.00010"},{"filterType":"LOT_SIZE","maxQty":"30000000","minQty":"1","stepSize":"1"},{"filterType":"MARKET_LOT_SIZE","maxQty":"3000000","minQty":"1","stepSize":"1"},{"filterType":"MIN_NOTIONAL","notional":"5"}]}
]}`

func vfRulesTrader(t *testing.T, price *float64, posts *atomic.Int32) *FuturesTrader {
	t.Helper()
	return fapiTestTrader(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/exchangeInfo":
			io.WriteString(w, vfRulesJSON)
		case "/fapi/v1/ticker/price", "/fapi/v2/ticker/price":
			fmt.Fprintf(w, `[{"symbol":%q,"price":"%g"}]`, r.URL.Query().Get("symbol"), *price)
		case "/fapi/v1/order":
			posts.Add(1)
			io.WriteString(w, `{"orderId":1,"status":"NEW"}`)
		case "/fapi/v1/leverage", "/fapi/v1/marginType", "/fapi/v1/positionSide/dual", "/fapi/v1/openOrders", "/fapi/v1/openAlgoOrders", "/fapi/v1/allOpenOrders":
			io.WriteString(w, `{}`)
		default:
			http.Error(w, `{"code":-1,"msg":"unexpected"}`, 404)
		}
	})
}

// Market and limit orders share the exchange MIN_NOTIONAL (was a fixed 10 for
// market): BTC 50 / ETH 20 reject below the floor, a 5-floor coin accepts 5-10.
func TestReview20261028VFMinNotionalFromExchangeRules(t *testing.T) {
	price := 10000.0
	var posts atomic.Int32
	tr := vfRulesTrader(t, &price, &posts)
	for _, tc := range []struct {
		symbol    string
		price     float64
		min       float64
		below, at float64
	}{
		{"BTCUSDT", 10000, 50, 0.004, 0.005},
		{"ETHUSDT", 10000, 20, 0.001, 0.002},
		{"ADAUSDT", 1, 5, 4, 5},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			price = tc.price
			if got := tr.GetMinNotional(tc.symbol); got != tc.min {
				t.Errorf("GetMinNotional=%g want %g", got, tc.min)
			}
			if err := tr.CheckMinNotional(tc.symbol, tc.below); err == nil || !strings.Contains(err.Error(), "MIN_NOTIONAL") {
				t.Errorf("below exchange minimum accepted: %v", err)
			}
			if err := tr.CheckMinNotional(tc.symbol, tc.at); err != nil {
				t.Errorf("exact boundary rejected: %v", err)
			}
			_, err := tr.PlaceLimitOrder(&types.LimitOrderRequest{Symbol: tc.symbol, Side: "BUY", PositionSide: "LONG", Quantity: tc.below, Price: tc.price})
			if err == nil {
				t.Error("limit below exchange minimum accepted")
			}
		})
	}
	// 5-10 USDT used to be refused by the hard-coded 10.
	price = 1
	if err := tr.CheckMinNotional("ADAUSDT", 7); err != nil {
		t.Errorf("7 USDT on a MIN_NOTIONAL=5 coin rejected: %v", err)
	}
	if posts.Load() != 0 {
		t.Errorf("no order may be posted by the checks, got %d", posts.Load())
	}
}

// Market quantities obey MARKET_LOT_SIZE.maxQty (no auto split, clear error);
// limit quantities keep LOT_SIZE.
func TestReview20261028VFMarketLotSizeMax(t *testing.T) {
	price := 10000.0
	var posts atomic.Int32
	tr := vfRulesTrader(t, &price, &posts)
	for _, tc := range []struct {
		symbol    string
		max, over float64
	}{
		{"BTCUSDT", 120, 120.001},
		{"ETHUSDT", 2000, 2000.001},
		{"ADAUSDT", 3000000, 3000001},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			if _, err := tr.FormatQuantity(tc.symbol, tc.max); err != nil {
				t.Errorf("market max boundary rejected: %v", err)
			}
			_, err := tr.FormatQuantity(tc.symbol, tc.over)
			if err == nil || !strings.Contains(err.Error(), "MARKET_LOT_SIZE") || !strings.Contains(err.Error(), "maxQty") {
				t.Errorf("want clear MARKET_LOT_SIZE maxQty error, got %v", err)
			}
			before := posts.Load()
			if _, err := tr.PlaceLimitOrder(&types.LimitOrderRequest{Symbol: tc.symbol, Side: "BUY", PositionSide: "LONG", Quantity: tc.over, Price: 10000}); err != nil {
				t.Errorf("limit must keep LOT_SIZE.maxQty: %v", err)
			}
			if posts.Load() != before+1 {
				t.Errorf("limit order not posted")
			}
		})
	}
	before := posts.Load()
	if _, err := tr.OpenLong("BTCUSDT", 121, 3); err == nil || !strings.Contains(err.Error(), "MARKET_LOT_SIZE") {
		t.Errorf("OpenLong above market maxQty: %v", err)
	}
	if posts.Load() != before {
		t.Error("oversize market open must not reach the exchange")
	}
}

// Reductions are never blocked by the MIN_NOTIONAL floor (reduce-only is
// exempt on Binance): a dust-notional close still reaches the exchange.
func TestReview20261028VFCloseIgnoresMinNotional(t *testing.T) {
	price := 10000.0
	var posts atomic.Int32
	tr := vfRulesTrader(t, &price, &posts)
	for _, closeFn := range []func(string, float64) (map[string]interface{}, error){tr.CloseLong, tr.CloseShort} {
		before := posts.Load()
		// 0.001 BTC = 10 USDT, far below BTC's MIN_NOTIONAL of 50.
		if _, err := closeFn("BTCUSDT", 0.001); err != nil {
			t.Fatalf("close blocked: %v", err)
		}
		if posts.Load() != before+1 {
			t.Fatalf("close not posted")
		}
	}
}

// Rules unavailable: fall back to the old conservative 10 (with a WARN) rather
// than silently allowing; market opens are still checked against it.
func TestReview20261028VFRulesUnavailableFallsBackTo10(t *testing.T) {
	tr := fapiTestTrader(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/ticker/price", "/fapi/v2/ticker/price":
			io.WriteString(w, `[{"symbol":"BTCUSDT","price":"100"}]`)
		default:
			http.Error(w, `{"code":-1000,"msg":"down"}`, 500)
		}
	})
	if got := tr.GetMinNotional("BTCUSDT"); got != 10 {
		t.Fatalf("fallback=%g want 10", got)
	}
	if err := tr.CheckMinNotional("BTCUSDT", 0.05); err == nil { // 5 USDT
		t.Error("below the legacy floor accepted")
	}
	if err := tr.CheckMinNotional("BTCUSDT", 0.1); err != nil { // 10 USDT
		t.Errorf("legacy floor boundary rejected: %v", err)
	}
}
