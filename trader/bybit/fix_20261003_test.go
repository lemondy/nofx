package bybit

import "testing"

func TestGenericConditionalOrdersAreClassifiedBySideAndTriggerDirection(t *testing.T) {
	for _, tc := range []struct {
		side      string
		direction int
		kind      string
	}{{"Sell", 1, "TAKE_PROFIT_MARKET"}, {"Sell", 2, "STOP_MARKET"}, {"Buy", 1, "STOP_MARKET"}, {"Buy", 2, "TAKE_PROFIT_MARKET"}} {
		if got := protectiveOrderType("Stop", tc.side, tc.direction); got != tc.kind {
			t.Fatalf("%s direction %d classified as %s", tc.side, tc.direction, got)
		}
	}
	if got := protectiveOrderType("Stop", "Sell", 0); got != "" {
		t.Fatal("unknown trigger classified as SL")
	}
}
