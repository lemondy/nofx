package binance_bstock

import (
	"fmt"
	"testing"
	"time"

	binance "github.com/adshao/go-binance/v2"
)

func TestCostBasisFIFOAndCommission(t *testing.T) {
	tr, f, _ := fixture(t)
	f.fills = []*binance.TradeV3{fill(1, true, 10, 100, 1, "AAPLB"), fill(2, true, 5, 120, 6, "USDT"), fill(3, false, 10, 150, 3, "USDT")}
	avg, qty, err := tr.CostBasis("AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	near(t, avg, 121.2)
	near(t, qty, 4)
	r, err := tr.GetClosedPnLForSymbol("AAPLBUSDT", time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 1 {
		t.Fatal(r)
	}
	near(t, r[0].Quantity, 10)
	near(t, r[0].EntryPrice, 112.12)
	near(t, r[0].RealizedPnL, 375.8)
	near(t, r[0].Fee, 104.2)
	if r[0].OrderID != "1003" || r[0].ExchangeID != "3" || r[0].EntryTime.UnixMilli() != f.fills[0].Time {
		t.Fatal(r[0])
	}
	tr.history["AAPLBUSDT"].at = time.Time{}
	f.fills = []*binance.TradeV3{fill(4, false, 1, 160, 0.1, "AAPLB")}
	avg, qty, err = tr.CostBasis("AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	near(t, avg, 121.2)
	near(t, qty, 2.9)
	r, err = tr.GetClosedPnLForSymbol("AAPLBUSDT", time.Time{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	near(t, r[0].RealizedPnL, 26.68)
	// startTime filters outputs only; FIFO still includes every prior buy.
	r, err = tr.GetClosedPnLForSymbol("AAPLBUSDT", time.UnixMilli(f.fills[0].Time+1), 100)
	if err != nil || len(r) != 0 {
		t.Fatalf("window: %+v %v", r, err)
	}
}

func TestIncrementalTradePagination(t *testing.T) {
	tr, f, clock := fixture(t)
	pages := []string{}
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.path != "/api/v3/myTrades" {
			return nil, 0, false
		}
		if r.params.Get("limit") != "1000" {
			t.Fatal(r.params)
		}
		from := r.params.Get("fromId")
		pages = append(pages, from)
		switch from {
		case "0":
			p := make([]*binance.TradeV3, 1000)
			for i := range p {
				p[i] = fill(int64(i), true, 1, 10, 0, "USDT")
			}
			return p, 200, true
		case "1000":
			return []*binance.TradeV3{fill(1000, true, 2, 20, 0, "USDT")}, 200, true
		case "1001":
			return []*binance.TradeV3{fill(1001, false, 3, 30, 0, "USDT")}, 200, true
		default:
			t.Fatalf("unexpected fromId %s", from)
			return nil, 0, false
		}
	}
	avg, qty, err := tr.CostBasis("AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	near(t, qty, 1002)
	near(t, avg, 10040.0/1002)
	_, _, err = tr.CostBasis("AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(pages) != "[0 1000]" {
		t.Fatal(pages)
	}
	*clock = clock.Add(16 * time.Second)
	avg, qty, err = tr.CostBasis("AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	near(t, qty, 999)
	near(t, avg, 10010.0/999)
	if fmt.Sprint(pages) != "[0 1000 1001]" {
		t.Fatal(pages)
	}
	r, err := tr.GetClosedPnL(time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 1 {
		t.Fatal(r)
	}
	near(t, r[0].RealizedPnL, 60)
}

func TestHistoryRefreshFailureDoesNotPublishPartialState(t *testing.T) {
	tr, f, clock := fixture(t)
	f.fills = []*binance.TradeV3{fill(1, true, 2, 100, 0, "USDT")}
	if _, _, err := tr.CostBasis("AAPLBUSDT"); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(16 * time.Second)
	f.fills = []*binance.TradeV3{fill(2, false, 1, 110, 0, "USDT"), fill(3, true, 1, 120, 1, "UNKNOWN")}
	_, _, err := tr.CostBasis("AAPLBUSDT")
	requireError(t, err, "commission conversion unsupported")
	h := tr.history["AAPLBUSDT"]
	if h.nextID != 2 || len(h.closed) != 0 {
		t.Fatalf("failed refresh changed cache: %+v", h)
	}
	near(t, h.lots[0].qty, 2)
	f.fills = []*binance.TradeV3{fill(2, false, 1, 110, 0, "USDT")}
	avg, qty, err := tr.CostBasis("AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	near(t, avg, 100)
	near(t, qty, 1)
}

func TestBNBCommissionUsesHistoricalConversion(t *testing.T) {
	tr, f, _ := fixture(t)
	f.fills = []*binance.TradeV3{fill(1, true, 2, 100, 0.01, "BNB")}
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.path == "/api/v3/klines" {
			if r.params.Get("symbol") != "BNBUSDT" || r.params.Get("interval") != "1m" || r.params.Get("startTime") != "1699999980000" {
				t.Fatal(r.params)
			}
			return [][]interface{}{{int64(1699999980000), "590", "610", "580", "600", "1", int64(1700000039999), "600", 1, "1", "600", "0"}}, 200, true
		}
		return nil, 0, false
	}
	avg, qty, err := tr.CostBasis("AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	near(t, avg, 103)
	near(t, qty, 2)
}

func TestOrderStatusAndFilledResult(t *testing.T) {
	tr, f, _ := fixture(t)
	b1, b2 := fill(1, true, 2, 80, 0.01, "AAPLB"), fill(2, true, 1, 120, 0.2, "USDT")
	b1.OrderID = 99
	b2.OrderID = 99
	f.fills = []*binance.TradeV3{b1, b2}
	f.handle = func(r requestRecord) (interface{}, int, bool) {
		if r.path == "/api/v3/order" && r.method == "GET" {
			return map[string]interface{}{"symbol": "AAPLBUSDT", "orderId": 99, "status": "FILLED", "side": "BUY", "type": "MARKET", "executedQty": "3", "cummulativeQuoteQty": "280"}, 200, true
		}
		if r.path == "/api/v3/order" && r.method == "POST" {
			return map[string]interface{}{"symbol": "AAPLBUSDT", "orderId": 99, "clientOrderId": r.params.Get("newClientOrderId"), "status": "FILLED", "side": "BUY", "type": "MARKET", "executedQty": "3", "cummulativeQuoteQty": "280", "fills": []map[string]interface{}{{"price": "80", "qty": "2", "commission": "0.01", "commissionAsset": "AAPLB"}, {"price": "120", "qty": "1", "commission": "0.2", "commissionAsset": "USDT"}}}, 200, true
		}
		return nil, 0, false
	}
	o, err := tr.BuyMarketNotional("AAPLBUSDT", 280)
	if err != nil {
		t.Fatal(err)
	}
	near(t, o.AvgPrice, 280.0/3)
	near(t, o.Commission, 1)
	if o.CommissionAsset != "USDT" {
		t.Fatal(o)
	}
	s, err := tr.GetOrderStatus("AAPLBUSDT", "99")
	if err != nil {
		t.Fatal(err)
	}
	near(t, s["avgPrice"].(float64), 280.0/3)
	near(t, s["executedQty"].(float64), 3)
	near(t, s["commission"].(float64), 1)
	if s["status"] != "FILLED" {
		t.Fatal(s)
	}
}

func TestClosedPnLIncludesFullySoldSymbol(t *testing.T) {
	tr, f, _ := fixture(t)
	f.balances = []binance.Balance{{Asset: "USDT", Free: "100"}}
	f.fills = []*binance.TradeV3{fill(1, true, 1, 100, 1, "USDT"), fill(2, false, 1, 110, 1, "USDT")}
	r, err := tr.GetClosedPnL(time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 1 {
		t.Fatal(r)
	}
	near(t, r[0].RealizedPnL, 8)
	near(t, r[0].Fee, 2)
}

func TestClosedPnLSameTimeUsesNumericTradeID(t *testing.T) {
	tr, f, _ := fixture(t)
	first, second := fill(9, false, 1, 110, 0, "USDT"), fill(10, false, 1, 120, 0, "USDT")
	second.Time = first.Time
	f.fills = []*binance.TradeV3{fill(1, true, 2, 100, 0, "USDT"), first, second}
	r, err := tr.GetClosedPnLForSymbol("AAPLBUSDT", time.Time{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 1 || r[0].ExchangeID != "10" {
		t.Fatal(r)
	}
	near(t, r[0].RealizedPnL, 20)
}
