package trader

import (
	"math"
	"nofx/market"
	"testing"
	"time"
)

// Exit-path classification truth table (2026-09-29 close_reason repair:
// 251/254 closed rows were indistinguishable 'sync').
func TestClassifyExitReason(t *testing.T) {
	cases := []struct {
		name   string
		side   string
		exit   float64
		entry  float64
		sl     float64
		tp     float64
		runner bool
		intent string
		want   string
	}{
		{"loss-side stop long", "long", 96.2, 100, 96.0, 110, false, "", "stop_loss"},
		{"loss-side stop short", "short", 103.8, 100, 104.0, 90, false, "", "stop_loss"},
		{"trailing stop long (SL past entry)", "long", 108.9, 100, 109.0, 120, true, "", "trailing_stop"},
		{"breakeven stop long (SL at entry)", "long", 100.1, 100, 100.0, 120, false, "", "trailing_stop"},
		{"structure TP", "long", 119.9, 100, 96.0, 120, false, "", "take_profit"},
		{"runner exit near no static level", "long", 128.4, 100, 105.0, 120, true, "", "trend_runner"},
		{"program close by intent", "long", 105.5, 100, 96.0, 120, false, "drawdown_protect", "drawdown_protect"},
		{"manual close on exchange", "long", 102.3, 100, 96.0, 120, false, "", "external"},
		{"price evidence outranks stale intent", "long", 96.1, 100, 96.0, 120, false, "tp_trim", "stop_loss"},
		{"no exit price → stays sync", "long", 0, 100, 96, 120, false, "", "sync"},
	}
	for _, tc := range cases {
		if got := classifyExitReason(tc.side, tc.exit, tc.entry, tc.sl, tc.tp, tc.runner, tc.intent); got != tc.want {
			t.Errorf("%s: classifyExitReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// MAE/MFE replay over synthetic 1m bars, side-aware, R conversion vs the
// write-once opening stop.
func TestComputeExitExcursions(t *testing.T) {
	bars := []market.Kline{
		{High: 101, Low: 99},
		{High: 104, Low: 100}, // favorable extreme 104
		{High: 102, Low: 97},  // adverse extreme 97
		{High: 103, Low: 101},
	}
	mae, mfe := computeExitExcursions(bars, "long", 100)
	if math.Abs(mae-3) > 1e-9 || math.Abs(mfe-4) > 1e-9 {
		t.Fatalf("long: mae %.2f mfe %.2f, want 3 / 4", mae, mfe)
	}
	mae, mfe = computeExitExcursions(bars, "short", 100)
	if math.Abs(mae-4) > 1e-9 || math.Abs(mfe-3) > 1e-9 {
		t.Fatalf("short: mae %.2f mfe %.2f, want 4 / 3 (mirrored)", mae, mfe)
	}
	// R conversion: opening stop 2% away → 3% adverse = 1.5R.
	if r := excursionR(3, 100, 98); math.Abs(r-1.5) > 1e-9 {
		t.Fatalf("excursionR = %.3f, want 1.5", r)
	}
	if r := excursionR(3, 100, 0); r != 0 {
		t.Fatalf("no anchor → 0R, got %.3f", r)
	}
	if mae, mfe := computeExitExcursions(nil, "long", 100); mae != 0 || mfe != 0 {
		t.Fatalf("no bars → zeros, got %.2f/%.2f", mae, mfe)
	}
}

// Close-intent registry: consume pops once, TTL drops stale intents.
func TestCloseIntentRegistry(t *testing.T) {
	at := &AutoTrader{closeIntents: make(map[string]closeIntent)}
	at.markCloseIntent("BTCUSDT", "long", "tp_trim")
	if got := at.consumeCloseIntent("btcusdt", "LONG"); got != "tp_trim" {
		t.Fatalf("first consume = %q, want tp_trim (key normalization)", got)
	}
	if got := at.consumeCloseIntent("BTCUSDT", "long"); got != "" {
		t.Fatalf("second consume = %q, want \"\" (popped)", got)
	}
	// Stale intent expires.
	at.closeIntents[keyOf("ETHUSDT", "short")] = closeIntent{reason: "ai_close", at: time.Now().Add(-2 * closeIntentTTL)}
	if got := at.consumeCloseIntent("ETHUSDT", "short"); got != "" {
		t.Fatalf("stale intent = %q, want \"\"", got)
	}
}
