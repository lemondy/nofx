package kucoin

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFix05KucoinProtectionDirectionTypeAndBaseQuantity(t *testing.T) {
	for _, c := range []struct{ side, stop, kind string }{{"sell", "down", "STOP_MARKET"}, {"sell", "up", "TAKE_PROFIT_MARKET"}, {"buy", "up", "STOP_MARKET"}, {"buy", "down", "TAKE_PROFIT_MARKET"}} {
		o, err := normalizeKucoinOrder(kucoinOpenOrder{ID: "1", Side: c.side, Stop: c.stop, StopPrice: "100", Size: 100, DealSize: 20, ReduceOnly: true}, 0.01)
		if err != nil || o.Type != c.kind || o.Quantity != 0.8 || !o.ReduceOnly || o.PositionSide != "BOTH" || o.StopPrice != 100 {
			t.Fatalf("%+v -> %+v err=%v", c, o, err)
		}
	}
	if _, err := normalizeKucoinOrder(kucoinOpenOrder{Side: "sell", StopPrice: "100"}, 1); err == nil {
		t.Fatal("unknown trigger direction accepted")
	}
}

func TestFix05KucoinRejectsIncompleteOrderPages(t *testing.T) {
	if _, err := decodeKucoinOrders([]byte(`{"items":[],"totalPage":2}`)); err == nil {
		t.Fatal("partial order book treated as complete protection snapshot")
	}
}

type fix05KucoinTransport func(*http.Request) (*http.Response, error)

func (f fix05KucoinTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestFix05KucoinFillReceiptUsesAverageAndBaseUnits(t *testing.T) {
	tr := &KuCoinTrader{contractsCache: map[string]*KuCoinContract{"XBTUSDTM": {Multiplier: 0.001}}, contractsCacheTime: time.Now()}
	tr.httpClient = &http.Client{Transport: fix05KucoinTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":"200000","data":{"id":"1","symbol":"XBTUSDTM","size":100,"dealSize":100,"avgDealPrice":"50000","isActive":false,"cancelExist":false,"status":"done"}}`))}, nil
	})}
	status, err := tr.GetOrderStatus("BTCUSDT", "1")
	if err != nil || status["executedQty"] != 0.1 || status["avgPrice"] != 50000.0 || status["status"] != "FILLED" {
		t.Fatalf("receipt=%v err=%v", status, err)
	}
}
