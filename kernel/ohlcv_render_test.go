package kernel

import (
	"strings"
	"testing"
)

// The 市场数据 panel (enable_raw_klines) promises raw candles: the signal
// JSON must carry the OHLCV block when populated, and omit it when not.
func TestRenderSignalOHLCV(t *testing.T) {
	sig := &SymbolSignal{
		Symbol:       "TESTUSDT",
		DataComplete: true,
		Price:        100,
		OHLCV: map[string][][5]float64{
			"15m": {{100, 101, 99, 100.5, 1234}, {100.5, 102, 100.2, 101.8, 2345}},
		},
	}
	out := RenderSignalJSON(sig)
	if !strings.Contains(out, `"ohlcv"`) || !strings.Contains(out, "101.8") {
		t.Fatalf("populated OHLCV must render into the signal JSON:\n%s", out)
	}

	empty := &SymbolSignal{Symbol: "TESTUSDT", DataComplete: true, Price: 100}
	if strings.Contains(RenderSignalJSON(empty), "ohlcv") {
		t.Fatal("absent OHLCV must be omitted (omitempty), not rendered as null")
	}
}
