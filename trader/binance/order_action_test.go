package binance

import "testing"

// review 2026-10-07 B1-7: hedge mode is classified purely by
// (positionSide, side); the PnL heuristic only applies to one-way mode.
func TestDetermineOrderAction(t *testing.T) {
	tr := &FuturesTrader{}
	cases := []struct {
		side, posSide string
		pnl           float64
		want          string
	}{
		{"BUY", "LONG", 0, "open_long"},
		{"BUY", "LONG", 5, "open_long"},
		{"SELL", "LONG", 0, "close_long"}, // breakeven close
		{"SELL", "LONG", -3, "close_long"},
		{"SELL", "SHORT", 0, "open_short"},
		{"SELL", "SHORT", 5, "open_short"},
		{"BUY", "SHORT", 0, "close_short"}, // breakeven close
		{"BUY", "SHORT", 4, "close_short"},
		{"buy", "long", 0, "open_long"},
		// one-way mode keeps the PnL heuristic
		{"BUY", "", 0, "open_long"},
		{"BUY", "", 2, "close_short"},
		{"SELL", "", 0, "open_short"},
		{"SELL", "", -2, "close_long"},
		{"BUY", "BOTH", 0, "open_long"},
		{"BUY", "BOTH", 2, "close_short"},
		{"SELL", "BOTH", 0, "open_short"},
		{"SELL", "BOTH", 2, "close_long"},
	}
	for _, c := range cases {
		if got := tr.determineOrderAction(c.side, c.posSide, c.pnl); got != c.want {
			t.Errorf("side=%q posSide=%q pnl=%v: got %s want %s", c.side, c.posSide, c.pnl, got, c.want)
		}
	}
}
