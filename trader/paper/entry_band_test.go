package paper

import (
	"math"
	"testing"
	"time"
)

func TestLimitTradeThrough(t *testing.T) {
	for _, side := range []string{"BUY", "SELL"} {
		for _, x := range []float64{0, 10} {
			t.Run(side+fmtBps(x), func(t *testing.T) {
				p, _, f := setup(t, Config{LimitTradeThroughBps: x})
				ps := "LONG"
				if side == "SELL" {
					ps = "SHORT"
				}
				o := limit(t, p, side, ps, 1, 100, false)
				f.step(2*time.Minute, 100, bar(f.now().Add(time.Minute), 100, 100, 100))
				must(t, p.Advance(f.now()))
				s, err := p.GetOrderStatus("BTCUSDT", o.OrderID)
				must(t, err)
				if (s["status"] == "FILLED") != (x == 0) {
					t.Fatalf("touch x=%v: %+v", x, s)
				}
				if x > 0 {
					// Just inside the threshold remains unfilled.
					f.step(time.Minute, 100, bar(f.now(), 99.95, 100.05, 100))
					must(t, p.Advance(f.now()))
					s, err = p.GetOrderStatus("BTCUSDT", o.OrderID)
					must(t, err)
					if s["status"] != "NEW" {
						t.Fatalf("insufficient trade-through filled: %+v", s)
					}
					f.step(time.Minute, 100, bar(f.now(), 100*(1-x/10000), 100*(1+x/10000), 100))
					must(t, p.Advance(f.now()))
					s, err = p.GetOrderStatus("BTCUSDT", o.OrderID)
					must(t, err)
					if s["status"] != "FILLED" {
						t.Fatalf("threshold didn't fill: %+v", s)
					}
				}
				near(t, s["avgPrice"].(float64), 100)
			})
		}
	}
}
func fmtBps(x float64) string {
	if x == 0 {
		return "_touch"
	}
	return "_through"
}

func TestLimitTradeThroughValidation(t *testing.T) {
	for _, x := range []float64{-1, 10000, math.NaN(), math.Inf(1)} {
		p := New(Config{LimitTradeThroughBps: x}, nil, newFake())
		if p.initErr == nil {
			t.Fatalf("accepted %v", x)
		}
	}
}

func TestPaperBandTick(t *testing.T) {
	p, _, f := setup(t, Config{})
	f.rule.TickSize = "0.025"
	tick, err := p.GetSymbolTickSize("BTCUSDT")
	if err != nil || tick != .025 {
		t.Fatalf("tick=%g %v", tick, err)
	}
}
