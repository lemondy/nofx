package trader

import (
	"testing"

	"nofx/trader/types"
)

// Pins for the orphaned-protection sweep (user directive 2026-09-26):
// hedge mode matches the exact side; one-way BOTH orders survive as long as
// ANY position exists on the symbol; LIMIT entries never count as protection.
func TestOrphanProtectiveOrder(t *testing.T) {
	live := map[string]bool{
		"BTCUSDT_long": true, // hedge: long alive, short gone
		"ETHUSDT_long": true, // one-way: only this side
	}

	cases := []struct {
		name string
		o    types.OpenOrder
		want bool
	}{
		{"hedge long order with live long", types.OpenOrder{Symbol: "BTCUSDT", PositionSide: "LONG"}, false},
		{"hedge short order, short gone", types.OpenOrder{Symbol: "BTCUSDT", PositionSide: "SHORT"}, true},
		{"one-way BOTH order, any position alive", types.OpenOrder{Symbol: "ETHUSDT", PositionSide: "BOTH"}, false},
		{"one-way BOTH order, no position", types.OpenOrder{Symbol: "XRPUSDT", PositionSide: "BOTH"}, true},
		{"empty sides treated as one-way", types.OpenOrder{Symbol: "XRPUSDT"}, true},
		{"case-insensitive symbol", types.OpenOrder{Symbol: "btcusdt", PositionSide: "LONG"}, false},
	}
	for _, c := range cases {
		if got := orphanProtectiveOrder(c.o, live); got != c.want {
			t.Errorf("%s: orphan=%v, want %v", c.name, got, c.want)
		}
	}
}
