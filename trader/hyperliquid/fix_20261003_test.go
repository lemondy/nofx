package hyperliquid

import (
	"github.com/sonirico/go-hyperliquid"
	"testing"
)

func TestRealExchangeReceiptsHaveStableIDsAndNoInventedFill(t *testing.T) {
	rejected := "rejected"
	for _, status := range []hyperliquid.OrderStatus{{}, {Error: &rejected}, {Filled: &hyperliquid.OrderStatusFilled{Oid: 7, TotalSz: "0", AvgPx: "100"}}} {
		if _, err := hyperliquidOrderReceipt(status); err == nil {
			t.Fatalf("invalid receipt accepted: %+v", status)
		}
	}
	resting, err := hyperliquidOrderReceipt(hyperliquid.OrderStatus{Resting: &hyperliquid.OrderStatusResting{Oid: 123}})
	if err != nil || resting["orderId"] != int64(123) || resting["status"] != "NEW" {
		t.Fatalf("resting receipt=%+v %v", resting, err)
	}
	filled, err := hyperliquidOrderReceipt(hyperliquid.OrderStatus{Filled: &hyperliquid.OrderStatusFilled{Oid: 124, TotalSz: "0.4", AvgPx: "99.5"}})
	if err != nil || filled["orderId"] != int64(124) || filled["executedQty"] != 0.4 || filled["avgPrice"] != 99.5 {
		t.Fatalf("filled receipt=%+v %v", filled, err)
	}
}
