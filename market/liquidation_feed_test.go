package market

import (
	"math"
	"testing"
	"time"
)

// Pins for the liquidation feed aggregator (user directive 2026-09-25):
// side semantics (SELL = long liquidated), 1h/24h bucketing, cold-start
// absent-vs-zero, and window pruning.
func TestLiquidationStats(t *testing.T) {
	liqMu.Lock()
	liqBySymbol = map[string][]liqEvent{}
	liqEvents = nil
	liqStarted = time.Now().Add(-30 * time.Hour) // pretend the feed has been up >24h
	liqMu.Unlock()

	now := time.Now()
	recordLiquidation("BTCUSDT", liqEvent{ts: now.Add(-30 * time.Minute), longLiq: true, notional: 500_000})
	recordLiquidation("BTCUSDT", liqEvent{ts: now.Add(-10 * time.Minute), longLiq: false, notional: 200_000})
	recordLiquidation("BTCUSDT", liqEvent{ts: now.Add(-25 * time.Hour), longLiq: true, notional: 1_000_000}) // outside 24h — pruned on write
	recordLiquidation("BTCUSDT", liqEvent{ts: now.Add(-5 * time.Minute), longLiq: true, notional: 50_000})
	recordLiquidation("ETHUSDT", liqEvent{ts: now.Add(-5 * time.Minute), longLiq: false, notional: 10_000})

	w, ok := LiquidationStats("BTCUSDT")
	if !ok {
		t.Fatal("window has events — must report ok")
	}
	if w.Long24hUSD != 550_000 || w.Short24hUSD != 200_000 {
		t.Fatalf("24h sides wrong: long %.0f short %.0f", w.Long24hUSD, w.Short24hUSD)
	}
	if w.Long1hUSD != 550_000 || w.Short1hUSD != 200_000 {
		t.Fatalf("1h sides wrong: long %.0f short %.0f", w.Long1hUSD, w.Short1hUSD)
	}
	if w.Count24h != 3 || w.Count1h != 3 {
		t.Fatalf("counts 24h=%d 1h=%d, want 3/3 (25h-old event pruned)", w.Count24h, w.Count1h)
	}
	if w.Sample == "" {
		t.Fatal("sample must carry the largest liquidation")
	}

	// Isolation: ETH stats don't bleed into BTC.
	w2, ok := LiquidationStats("ETHUSDT")
	if !ok || w2.Short24hUSD != 10_000 {
		t.Fatalf("ETH stats wrong: %+v", w2)
	}

	// Cold start: zero events → absent, never zero.
	liqMu.Lock()
	liqBySymbol["XYZUSDT"] = nil
	liqMu.Unlock()
	if _, ok := LiquidationStats("XYZUSDT"); ok {
		t.Fatal("empty window must report absent (cold start ≠ zero liquidations)")
	}
}

func TestDecodeLiquidationOrdersAcceptsRawAndCombinedStreams(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantN   int
	}{
		{
			name:    "raw stream",
			payload: `{"e":"forceOrder","o":{"s":"BTCUSDT","S":"SELL","ap":"61234.50","z":"0.025","T":1700000000000}}`,
			wantN:   1,
		},
		{
			name:    "combined stream",
			payload: `{"stream":"!forceOrder@arr","data":{"e":"forceOrder","o":{"s":"ETHUSDT","S":"BUY","ap":"2450.25","z":"1.5","T":1700000000001}}}`,
			wantN:   1,
		},
		{
			name: "batched raw stream",
			payload: `[{"o":{"s":"BTCUSDT","S":"SELL","ap":"1","z":"2"}},` +
				`{"o":{"s":"ETHUSDT","S":"BUY","ap":"3","z":"4"}}]`,
			wantN: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			orders, err := decodeLiquidationOrders([]byte(tc.payload))
			if err != nil {
				t.Fatalf("decodeLiquidationOrders() error = %v", err)
			}
			if len(orders) != tc.wantN {
				t.Fatalf("decoded %d orders, want %d", len(orders), tc.wantN)
			}
			if orders[0].Symbol == "" {
				t.Fatal("decoded order has an empty symbol")
			}
		})
	}
}

func TestParseFloatSafePreservesDecimalScale(t *testing.T) {
	for input, want := range map[string]float64{
		"61234.50": 61234.5,
		"0.025":    0.025,
		" -1.25 ":  -1.25,
		"bad":      0,
	} {
		if got := parseFloatSafe(input); math.Abs(got-want) > 1e-9 {
			t.Errorf("parseFloatSafe(%q) = %v, want %v", input, got, want)
		}
	}
}
