package binance

import (
	"github.com/adshao/go-binance/v2/futures"
	"nofx/market"
	"testing"
)

func TestThinStopWorkingType(t *testing.T) {
	market.SetEquityClassificationForTesting(map[string]bool{"AAPLUSDT": true}, map[string]bool{"AAPLUSDT": true})
	defer market.SetEquityClassificationForTesting(nil, nil)
	for _, tc := range []struct {
		name, symbol  string
		oi, threshold float64
		known, mark   bool
	}{
		{"default thin", "XUSDT", 44, 0, true, true}, {"boundary", "XUSDT", 45, 0, true, false}, {"liquid", "XUSDT", 90, 0, true, false}, {"unknown", "XUSDT", 0, 0, false, false},
		{"zero OI", "XUSDT", 0, 0, true, true}, {"custom", "XUSDT", 50, 60, true, true}, {"disabled", "XUSDT", 1, -1, true, false}, {"bstock", "AAPLUSDT", 1, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &FuturesTrader{oiValueFn: func(string) (float64, bool) { return tc.oi, tc.known }}
			if tc.threshold != 0 {
				tr.SetThinOIStopThreshold(tc.threshold)
			}
			want := futures.WorkingTypeContractPrice
			if tc.mark {
				want = futures.WorkingTypeMarkPrice
			}
			if got := tr.stopWorkingType(tc.symbol); got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		})
	}
	// Missing cache never attempts network and retains the old trigger.
	if got := (&FuturesTrader{}).stopWorkingType("MISSINGOIUSDT"); got != futures.WorkingTypeContractPrice {
		t.Fatal(got)
	}
}
