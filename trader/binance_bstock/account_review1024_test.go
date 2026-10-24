package binance_bstock

import (
	"testing"

	binance "github.com/adshao/go-binance/v2"
)

func TestReview1024OneStepBalanceVisible(t *testing.T) {
	for _, qty := range []float64{0.0004, 0.001, 0.002} {
		t.Run(value(qty), func(t *testing.T) {
			tr, f, _ := fixture(t)
			f.balances = []binance.Balance{{Asset: "AAPLB", Free: value(qty)}}
			p, err := tr.GetPositions()
			if err != nil || len(p) != 1 || p[0]["positionAmt"] != qty || p[0]["dust"] != true {
				t.Fatalf("positive inventory hidden: positions=%v err=%v", p, err)
			}
		})
	}
}

func TestReview1024RawBalanceBypassesPositionCache(t *testing.T) {
	tr, f, _ := fixture(t)
	f.balances = []binance.Balance{{Asset: "AAPLB", Free: "2"}}
	if _, err := tr.GetPositions(); err != nil {
		t.Fatal(err)
	}
	f.balances = []binance.Balance{{Asset: "AAPLB", Free: "0.0004", Locked: "0.0006"}}
	raw, err := tr.RawStockBalance("AAPLBUSDT")
	if err != nil || raw != .001 {
		t.Fatalf("raw balance reused filtered/cache data: raw=%g err=%v", raw, err)
	}
	p, err := tr.GetPositions()
	if err != nil || p[0]["positionAmt"] != float64(2) {
		t.Fatal("expected the independent display cache to remain unchanged")
	}
}
