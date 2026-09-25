package kernel

import (
	"testing"

	"nofx/market"
)

// Pins for the bstock stop-band yardstick (user directive 2026-09-25):
// equity tokens price floor/buffer/cap off the DAILY ATR; crypto keeps the
// 1h-floor/4h-cap asymmetry. Classification is injected — no network.
func TestStopBandScaleForStocks(t *testing.T) {
	market.SetEquityClassificationForTesting(
		map[string]bool{"AAPLUSDT": true},
		map[string]bool{"AAPLUSDT": true},
	)
	defer market.SetEquityClassificationForTesting(nil, nil)

	tfs := map[string]*TFSignal{
		"1h": {ATRPct: 1.0},
		"4h": {ATRPct: 2.0},
		"1d": {ATRPct: 5.0},
	}

	// Crypto: floor 1.5×ATR(1h) = 1.5; cap 2×ATR(4h) = 4.
	sigCrypto := &SymbolSignal{Symbol: "BTCUSDT", Timeframes: tfs}
	if got := stopFloorPct(sigCrypto, 1.5); got != 1.5 {
		t.Fatalf("crypto floor = %.2f, want 1.5 (1h scale)", got)
	}
	_, _, code := methodStopPlan(sigCrypto, 100, 1.5, true)
	_ = code // exercised below via stock cap comparison

	// Stock: floor 1.5×ATR(1d) = 7.5; the buffer and cap also ride 1d.
	sigStock := &SymbolSignal{Symbol: "AAPLUSDT", Timeframes: tfs}
	if got := stopFloorPct(sigStock, 1.5); got != 7.5 {
		t.Fatalf("stock floor = %.2f, want 7.5 (1d scale)", got)
	}

	// Cap: crypto cap 2×ATR(4h)=4 < stock cap 2×ATR(1d)=10 — verify via the
	// band acceptance: a structure 8% away is OUT of band for crypto but IN
	// band for stock (with buffer 0.4×ATR: crypto buf 0.4% → stop 8.4% > 4
	// = OUT; stock buf 2% → stop 10% ≤ 10 = IN).
	// Direct cap check through methodStopPlan needs S/R levels — build one.
	sigStockLv := &SymbolSignal{Symbol: "AAPLUSDT", Timeframes: map[string]*TFSignal{
		"1h": {ATRPct: 1.0, Support: []float64{92.0}},
		"4h": {ATRPct: 2.0},
		"1d": {ATRPct: 5.0},
	}}
	price, dist, code := methodStopPlan(sigStockLv, 100, 7.5, true)
	if code != "" {
		t.Fatalf("stock plan should accept the 8%% structure (cap 10), got %s", code)
	}
	if price != 92-0.4*5.0 { // long: stop = structure − 0.4×ATR(1d) = 90
		t.Fatalf("stock stop = %.2f, want 90.0 (structure − 0.4×ATR(1d))", price)
	}
	if dist < 7.5 || dist > 10 {
		t.Fatalf("stock stop distance %.2f outside the daily band", dist)
	}

	// Crypto mirror: the SAME 8%-away structure is OUT of band (cap 4).
	sigCryptoLv := &SymbolSignal{Symbol: "BTCUSDT", Timeframes: map[string]*TFSignal{
		"1h": {ATRPct: 1.0, Support: []float64{92.0}},
		"4h": {ATRPct: 2.0},
		"1d": {ATRPct: 5.0},
	}}
	_, _, codeC := methodStopPlan(sigCryptoLv, 100, 1.5, true)
	if codeC != "STOP_PLAN_OUT_OF_BAND" {
		t.Fatalf("crypto plan must reject the 8%% structure (cap 4), got %q", codeC)
	}
}
