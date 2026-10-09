package binance_bstock

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	binance "github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/common"
)

type requestRecord struct {
	method, path string
	params       url.Values
}
type fakeTransport struct {
	t        *testing.T
	requests []requestRecord
	symbols  []binance.Symbol
	balances []binance.Balance
	orders   []*binance.Order
	fills    []*binance.TradeV3
	handle   func(requestRecord) (interface{}, int, bool)
}

func stockRule() binance.Symbol {
	return binance.Symbol{Symbol: "AAPLBUSDT", Status: "TRADING", BaseAsset: "AAPLB", QuoteAsset: "USDT", IsSpotTradingAllowed: true, OcoAllowed: true, QuoteOrderQtyMarketAllowed: true, Filters: []map[string]interface{}{
		{"filterType": "LOT_SIZE", "stepSize": "0.001", "minQty": "0.001", "maxQty": "10000"},
		{"filterType": "PRICE_FILTER", "tickSize": "0.01", "minPrice": "0.01", "maxPrice": "100000"},
		{"filterType": "NOTIONAL", "minNotional": "5", "maxNotional": "1000000", "applyMinToMarket": true, "applyMaxToMarket": true},
		{"filterType": "PERCENT_PRICE_BY_SIDE", "bidMultiplierDown": "0.8", "bidMultiplierUp": "1.2", "askMultiplierDown": "0.8", "askMultiplierUp": "1.2", "avgPriceMins": 0},
	}}
}

func fixture(t *testing.T) (*BStockTrader, *fakeTransport, *time.Time) {
	t.Helper()
	clock := time.Unix(1700000000, 0)
	f := &fakeTransport{t: t, symbols: []binance.Symbol{stockRule()}, balances: []binance.Balance{{Asset: "USDT", Free: "100", Locked: "20"}, {Asset: "AAPLB", Free: "10", Locked: "0"}}}
	tr := NewBStockTrader("test-key", "test-secret")
	tr.client.HTTPClient = &http.Client{Transport: f}
	tr.now = func() time.Time { return clock }
	return tr, f, &clock
}

func (f *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Helper()
	if err := r.ParseForm(); err != nil {
		f.t.Fatal(err)
	}
	record := requestRecord{r.Method, r.URL.Path, r.Form}
	f.requests = append(f.requests, record)
	var body interface{}
	status := 200
	handled := false
	if f.handle != nil {
		body, status, handled = f.handle(record)
	}
	if !handled {
		switch record.path {
		case "/api/v3/exchangeInfo":
			body = map[string]interface{}{"symbols": f.symbols}
		case "/api/v3/account":
			body = map[string]interface{}{"balances": f.balances}
		case "/api/v3/ticker/price":
			body = map[string]interface{}{"symbol": record.params.Get("symbol"), "price": "100"}
		case "/api/v3/avgPrice":
			body = map[string]interface{}{"mins": 5, "price": "100"}
		case "/api/v3/myTrades":
			body = f.fills
			if body == nil || f.fills == nil {
				body = []interface{}{}
			}
		case "/api/v3/openOrders":
			if record.method == "DELETE" {
				f.orders = nil
				body = []interface{}{}
			} else {
				body = f.orders
				if f.orders == nil {
					body = []interface{}{}
				}
			}
		case "/api/v3/order":
			if record.method == "DELETE" {
				f.orders = nil
				for i := range f.balances {
					if f.balances[i].Asset == "AAPLB" {
						_, total := holding(&binance.Account{Balances: f.balances}, "AAPLB")
						f.balances[i].Free = value(total)
						f.balances[i].Locked = "0"
					}
				}
				body = map[string]interface{}{"orderId": 100}
			} else if record.method == "POST" {
				body = map[string]interface{}{"symbol": "AAPLBUSDT", "orderId": 100, "clientOrderId": record.params.Get("newClientOrderId"), "side": record.params.Get("side"), "type": record.params.Get("type"), "status": "NEW", "origQty": record.params.Get("quantity"), "price": record.params.Get("price"), "executedQty": "0", "cummulativeQuoteQty": "0"}
			} else {
				f.t.Fatalf("unhandled request: %+v", record)
			}
		case "/api/v3/orderList":
			if record.method != "DELETE" {
				f.t.Fatalf("unexpected list method: %s", record.method)
			}
			f.orders = nil
			for i := range f.balances {
				if f.balances[i].Asset == "AAPLB" {
					_, total := holding(&binance.Account{Balances: f.balances}, "AAPLB")
					f.balances[i].Free = value(total)
					f.balances[i].Locked = "0"
				}
			}
			body = map[string]interface{}{"orderListId": 9}
		case "/api/v3/orderList/oco":
			if r.Header.Get("X-MBX-APIKEY") != "test-key" {
				f.t.Fatal("raw request lost API key")
			}
			query := r.URL.RawQuery
			unsigned, sig, ok := strings.Cut(query, "&signature=")
			mac := hmac.New(sha256.New, []byte("test-secret"))
			_, _ = mac.Write([]byte(unsigned))
			if !ok || sig != hex.EncodeToString(mac.Sum(nil)) {
				f.t.Fatal("invalid raw signature")
			}
			body = map[string]interface{}{"orderListId": 9, "orders": []map[string]interface{}{{"orderId": 101, "clientOrderId": record.params.Get("aboveClientOrderId")}, {"orderId": 102, "clientOrderId": record.params.Get("belowClientOrderId")}}}
		case "/api/v3/order/test":
			body = map[string]interface{}{}
		default:
			f.t.Fatalf("unexpected request: %+v", record)
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data))), Request: r}, nil
}

func requestsTo(f *fakeTransport, path, method string) []requestRecord {
	out := []requestRecord{}
	for _, r := range f.requests {
		if r.path == path && (method == "" || r.method == method) {
			out = append(out, r)
		}
	}
	return out
}
func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-8 {
		t.Fatalf("got %.12f, want %.12f", got, want)
	}
}
func requireError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("got error %v, want containing %q", err, contains)
	}
}
func tagged(t *testing.T, s string) {
	t.Helper()
	if !strings.HasPrefix(s, clientPrefix) || len(s) > 36 {
		t.Fatalf("invalid client ID %q", s)
	}
}
func fill(n int64, buy bool, qty, p, fee float64, asset string) *binance.TradeV3 {
	return &binance.TradeV3{ID: n, OrderID: n + 1000, Symbol: "AAPLBUSDT", IsBuyer: buy, Quantity: value(qty), Price: value(p), QuoteQuantity: value(qty * p), Commission: value(fee), CommissionAsset: asset, Time: 1700000000000 + n*1000}
}

func TestBalancePositionsAndCache(t *testing.T) {
	tr, f, clock := fixture(t)
	f.balances = []binance.Balance{{Asset: "USDT", Free: "100", Locked: "20"}, {Asset: "AAPLB", Free: "1.5", Locked: "0.5"}, {Asset: "BTC", Free: "100"}, {Asset: "DUSTB", Free: "0.001"}, {Asset: "NOTB", Free: "9"}}
	dust := stockRule()
	dust.Symbol = "DUSTBUSDT"
	dust.BaseAsset = "DUSTB"
	non := stockRule()
	non.Symbol = "NOTBUSDT"
	non.BaseAsset = "NOTB"
	non.Status = "BREAK"
	f.symbols = append(f.symbols, dust, non)
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.path == "/api/v3/myTrades" && r.params.Get("symbol") == "AAPLBUSDT" {
			return []*binance.TradeV3{fill(1, true, 2, 80, 0, "USDT")}, 200, true
		}
		return nil, 0, false
	}
	p, err := tr.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 || p[0]["side"] != "long" || p[0]["symbol"] != "AAPLBUSDT" {
		t.Fatalf("positions: %#v", p)
	}
	near(t, p[0]["positionAmt"].(float64), 2)
	near(t, p[0]["entryPrice"].(float64), 80)
	near(t, p[0]["markPrice"].(float64), 100)
	near(t, p[0]["unRealizedProfit"].(float64), 40)
	near(t, p[0]["leverage"].(float64), 1)
	near(t, p[0]["liquidationPrice"].(float64), 0)
	b, err := tr.GetBalance()
	if err != nil {
		t.Fatal(err)
	}
	near(t, b["totalEquity"].(float64), 320.1)
	near(t, b["availableBalance"].(float64), 100)
	near(t, b["totalUnrealizedProfit"].(float64), 40)
	near(t, b["totalWalletBalance"].(float64), 280.1)
	count := len(f.requests)
	p[0]["positionAmt"] = 99.0
	b["availableBalance"] = 0.0
	p, _ = tr.GetPositions()
	b, _ = tr.GetBalance()
	near(t, p[0]["positionAmt"].(float64), 2)
	near(t, b["availableBalance"].(float64), 100)
	if len(f.requests) != count {
		t.Fatal("cache hit performed HTTP request")
	}
	*clock = clock.Add(16 * time.Second)
	if _, err = tr.GetBalance(); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) == count {
		t.Fatal("expired cache did not refresh")
	}
}

func TestBuyOrdersAndInvalidation(t *testing.T) {
	tr, f, _ := fixture(t)
	if _, err := tr.GetBalance(); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.GetPositions(); err != nil {
		t.Fatal(err)
	}
	o, err := tr.BuyMarketNotional("AAPLBUSDT", 123.45)
	if err != nil {
		t.Fatal(err)
	}
	tagged(t, o.ClientOrderID)
	r := requestsTo(f, "/api/v3/order", "POST")[0]
	if r.params.Get("quoteOrderQty") != "123.45" || r.params.Get("quantity") != "" || r.params.Get("type") != "MARKET" {
		t.Fatalf("market request: %v", r.params)
	}
	if tr.balance != nil || tr.positions != nil {
		t.Fatal("own order did not invalidate caches")
	}
	o, err = tr.BuyLimit("AAPLBUSDT", 0.12399, 100.019)
	if err != nil {
		t.Fatal(err)
	}
	r = requestsTo(f, "/api/v3/order", "POST")[1]
	if r.params.Get("quantity") != "0.123" || r.params.Get("price") != "100.01" || r.params.Get("timeInForce") != "GTC" {
		t.Fatalf("limit request: %v", r.params)
	}
	tagged(t, o.ClientOrderID)
	_, err = tr.BuyLimit("AAPLBUSDT", 0.0499, 100)
	requireError(t, err, "NOTIONAL")
	_, err = tr.BuyMarketNotional("AAPLBUSDT", 4.99)
	requireError(t, err, "NOTIONAL")
	_, err = tr.BuyMarketNotional("AAPLBUSDT", 1000001)
	requireError(t, err, "NOTIONAL")
	if len(requestsTo(f, "/api/v3/order", "POST")) != 2 {
		t.Fatal("invalid notional reached order endpoint")
	}
	s, err := tr.FormatQuantity("AAPLBUSDT", 1.001)
	if err != nil || s != "1.001" {
		t.Fatalf("exact step got %q %v", s, err)
	}
	s, err = tr.FormatQuantity("AAPLBUSDT", 1.0019)
	if err != nil || s != "1.001" {
		t.Fatalf("round down got %q %v", s, err)
	}
	_, err = tr.BuyLimit("BTCUSDT", 1, 100)
	requireError(t, err, "symbol not tradable")
}

func TestQuoteMarketAllowedAndMarketLotSize(t *testing.T) {
	tr, f, _ := fixture(t)
	f.symbols[0].QuoteOrderQtyMarketAllowed = false
	_, err := tr.BuyMarketNotional("AAPLBUSDT", 20)
	requireError(t, err, "not allowed")
	f.symbols[0].Filters = append(f.symbols[0].Filters, map[string]interface{}{"filterType": "MARKET_LOT_SIZE", "stepSize": "0.01", "minQty": "0.1", "maxQty": "1"})
	tr.rules = nil
	_, err = tr.OpenLong("AAPLBUSDT", 1.1, 1)
	requireError(t, err, "MARKET_LOT_SIZE")
	_, err = tr.OpenLong("AAPLBUSDT", 0.129, 1)
	if err != nil {
		t.Fatal(err)
	}
	r := requestsTo(f, "/api/v3/order", "POST")[0]
	if r.params.Get("quantity") != "0.12" || r.params.Get("quoteOrderQty") != "" {
		t.Fatal(r.params)
	}
}

func TestUnsupportedMethods(t *testing.T) {
	tr, f, _ := fixture(t)
	_, err := tr.OpenShort("AAPLBUSDT", 1, 1)
	requireError(t, err, "not supported on spot bStock")
	_, err = tr.CloseShort("AAPLBUSDT", 0)
	requireError(t, err, "not supported on spot bStock")
	_, err = tr.OpenLong("AAPLBUSDT", 1, 2)
	requireError(t, err, "not supported on spot bStock")
	requireError(t, tr.SetLeverage("AAPLBUSDT", 2), "not supported on spot bStock")
	requireError(t, tr.SetMarginMode("AAPLBUSDT", true), "not supported on spot bStock")
	requireError(t, tr.SetStopLoss("AAPLBUSDT", "SHORT", 1, 90), "not supported on spot bStock")
	if err = tr.SetLeverage("AAPLBUSDT", 1); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 0 {
		t.Fatal("unsupported methods sent requests")
	}
}

func TestPreflightClassification(t *testing.T) {
	for _, tc := range []struct {
		code          int
		msg, category string
	}{{-2015, "Invalid API-key, IP, or permissions for action.", "auth/permission"}, {-2014, "API-key format invalid.", "auth/permission"}, {-2010, "This symbol is not permitted for this account.", "auth/permission"}, {-1121, "Invalid symbol.", "symbol not tradable"}, {-1013, "Filter failure: NOTIONAL", "filter failure"}} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			tr, f, _ := fixture(t)
			f.symbols[0].Filters[3]["avgPriceMins"] = 5
			f.handle = func(r requestRecord) (interface{}, int, bool) {
				if r.path == "/api/v3/order/test" {
					return map[string]interface{}{"code": tc.code, "msg": tc.msg}, 400, true
				}
				return nil, 0, false
			}
			err := tr.Preflight("AAPLBUSDT")
			requireError(t, err, tc.category)
			requireError(t, err, fmt.Sprint(tc.code))
			var api *common.APIError
			if !errors.As(err, &api) || api.Code != int64(tc.code) {
				t.Fatal("error wrapping lost Binance error")
			}
			if tc.category == "auth/permission" && !errors.Is(err, ErrAuthOrPermission) {
				t.Fatal("permission error lacks sentinel")
			}
			r := requestsTo(f, "/api/v3/order/test", "POST")[0]
			if r.params.Get("price") != "100" || r.params.Get("quantity") != "0.05" || r.params.Get("type") != "LIMIT" {
				t.Fatal(r.params)
			}
			tagged(t, r.params.Get("newClientOrderId"))
			if len(requestsTo(f, "/api/v3/order", "POST")) != 0 {
				t.Fatal("preflight placed real order")
			}
		})
	}
	tr, _, _ := fixture(t)
	if err := tr.Preflight("AAPLBUSDT"); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightIntersectsPriceFilters(t *testing.T) {
	tr, f, _ := fixture(t)
	f.symbols[0].Filters = append(f.symbols[0].Filters, map[string]interface{}{"filterType": "PERCENT_PRICE", "multiplierDown": "0.9", "multiplierUp": "0.95", "avgPriceMins": 0})
	if err := tr.Preflight("AAPLBUSDT"); err != nil {
		t.Fatal(err)
	}
	r := requestsTo(f, "/api/v3/order/test", "POST")[0]
	if r.params.Get("price") != "92.5" {
		t.Fatal(r.params)
	}
	tr.rules = nil
	f.symbols[0].Filters[4]["multiplierDown"] = "1.000001"
	f.symbols[0].Filters[4]["multiplierUp"] = "1.000002"
	requireError(t, tr.Preflight("AAPLBUSDT"), "no tick price")
	if len(requestsTo(f, "/api/v3/order/test", "POST")) != 1 {
		t.Fatal("invalid filter intersection reached test endpoint")
	}
}

func TestCloseLongUsesFreeBalance(t *testing.T) {
	tr, f, _ := fixture(t)
	f.balances = []binance.Balance{{Asset: "AAPLB", Free: "1", Locked: "2"}}
	_, err := tr.CloseLong("AAPLBUSDT", 2)
	requireError(t, err, "exceeds free balance")
	if len(requestsTo(f, "/api/v3/order", "POST")) != 0 {
		t.Fatal("sell exceeded unlocked inventory")
	}
	o, err := tr.CloseLong("AAPLBUSDT", 0.1299)
	if err != nil {
		t.Fatal(err)
	}
	if o["side"] != "SELL" || o["type"] != "MARKET" || o["orderId"] != "100" {
		t.Fatal(o)
	}
	r := requestsTo(f, "/api/v3/order", "POST")[0]
	if r.params.Get("quantity") != "0.129" {
		t.Fatal(r.params)
	}
	tagged(t, r.params.Get("newClientOrderId"))
}

func TestPlacedOrderRequiresID(t *testing.T) {
	tr, f, _ := fixture(t)
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.path == "/api/v3/order" && r.method == "POST" {
			return map[string]interface{}{"symbol": "AAPLBUSDT", "status": "NEW"}, 200, true
		}
		return nil, 0, false
	}
	o, err := tr.BuyMarketNotional("AAPLBUSDT", 20)
	requireError(t, err, "reconcile before retrying")
	if o == nil || len(requestsTo(f, "/api/v3/order", "POST")) != 1 {
		t.Fatal("ambiguous placement retried or discarded response")
	}
}
