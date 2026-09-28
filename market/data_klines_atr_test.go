package market

import (
	"math"
	"testing"
	"time"
)

// 09-28 review P2: the executor's ATR yardsticks (stop-band floor/cap,
// protection levels) were computed over klines INCLUDING the live-patched
// forming candle, while the kernel's prompt-side ATR is closed-bars-only —
// a steady-state ~1.5-3.5% low bias, the same gate/executor divergence
// class as the 09-19 BTCUSDT stop-band loop.
func TestCalculateTimeframeSeriesATRExcludesFormingBar(t *testing.T) {
	now := time.Now()
	dur := TimeframeDuration("1h")

	// 30 closed bars, each true range ≈ 1% of price.
	closedATR := 0.0
	var klines []Kline
	for i := 0; i < 30; i++ {
		p := 100 + float64(i)*0.01
		openTime := now.Add(time.Duration(i-30)*dur + dur/2).UnixMilli() // closed: bar end in the past
		klines = append(klines, Kline{
			OpenTime: openTime, Open: p * 0.995, High: p * 1.005, Low: p * 0.995, Close: p,
		})
		closedATR = math.Max(closedATR, p*0.01)
	}
	data := calculateTimeframeSeries(klines, "1h", 30)
	if data.ATR14 < closedATR*0.97 || data.ATR14 > closedATR*1.03 {
		t.Fatalf("closed-only series ATR14 = %.4f, want ≈%.4f", data.ATR14, closedATR)
	}

	// Append a just-opened forming bar with a tiny range: it must be
	// excluded, or the Wilder seed drags ATR down ~6%.
	formingOpen := now.Add(-dur / 20).UnixMilli() // bar opened 5% into its window
	klines = append(klines, Kline{
		OpenTime: formingOpen, Open: 100.3, High: 100.31, Low: 100.30, Close: 100.30,
	})
	data2 := calculateTimeframeSeries(klines, "1h", 30)
	if data2.ATR14 < closedATR*0.97 || data2.ATR14 > closedATR*1.03 {
		t.Fatalf("forming bar leaked into ATR14: %.4f (closed-only ≈%.4f)", data2.ATR14, closedATR)
	}
}
