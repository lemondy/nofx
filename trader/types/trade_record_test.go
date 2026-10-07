package types

import "testing"

// 2026-10-07 review N2: ownership uses the exchange order id when known.
func TestTradeRecordPositionOrderID(t *testing.T) {
	if got := (TradeRecord{TradeID: "46693004", OrderID: "482032646"}).PositionOrderID(); got != "482032646" {
		t.Fatalf("got %s, want order id", got)
	}
	if got := (TradeRecord{TradeID: "46693004"}).PositionOrderID(); got != "46693004" {
		t.Fatalf("got %s, want trade-id fallback", got)
	}
}
