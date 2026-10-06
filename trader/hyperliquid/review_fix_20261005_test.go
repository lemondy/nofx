package hyperliquid

import (
	hl "github.com/sonirico/go-hyperliquid"
	"testing"
)

func TestFix05FrontendTriggerDetailsAreNormalized(t *testing.T) {
	orders, err := normalizeFrontendOrders("BTCUSDT", []hl.FrontendOpenOrder{
		{Coin: "BTC", Oid: 1, Side: "A", OrderType: "Stop Market", IsTrigger: true, ReduceOnly: true, TriggerPx: 95, Sz: 0.4},
		{Coin: "BTC", Oid: 2, Side: "A", OrderType: "Take Profit Market", IsTrigger: true, ReduceOnly: true, TriggerPx: 110, Sz: 0.2},
		{Coin: "ETH", Oid: 3},
	})
	if err != nil || len(orders) != 2 {
		t.Fatalf("orders=%v err=%v", orders, err)
	}
	if orders[0].StopPrice != 95 || orders[0].Type != "STOP_MARKET" || orders[0].Quantity != 0.4 || !orders[0].ReduceOnly || !orders[0].Algo {
		t.Fatalf("SL fields lost: %+v", orders[0])
	}
	if orders[1].Type != "TAKE_PROFIT_MARKET" || orders[1].StopPrice != 110 {
		t.Fatalf("TP fields lost: %+v", orders[1])
	}
}
