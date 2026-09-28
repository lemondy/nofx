package kernel

import (
	"testing"
	"time"

	"nofx/market"
)

// 09-28 review P2: the `rsi > 0` sentinel conflated "insufficient data"
// (calculateRSI returns 0 for len ≤ period) with the max-oversold extreme —
// a pure monotonic decline drives Wilder avgGain to exactly 0 and RSI to
// exactly 0, silently dropping the most directional reading from the signal
// block (the bearish mirror-image of RSI=100, which always passed).
func TestTFSignalKeepsZeroRSIOnMonotonicDecline(t *testing.T) {
	now := time.Now()
	tfd := &market.TimeframeSeriesData{Timeframe: "1h"}
	// 30 closed 1h bars, pure decline: every close below the previous.
	for i := 0; i < 30; i++ {
		p := 100 - float64(i)
		barTime := now.Add(time.Duration(i-30) * time.Hour)
		tfd.Klines = append(tfd.Klines, market.KlineBar{
			Time: barTime.UnixMilli(), Open: p + 0.5, High: p + 0.6, Low: p - 0.6, Close: p, Volume: 10,
		})
	}
	sig, _ := computeTFSignal("1h", tfd, now, 70)
	if sig == nil {
		t.Fatal("computeTFSignal returned nil")
	}
	if sig.RSI14 == nil {
		t.Fatal("RSI14 must be present on a monotonic decline (RSI = 0 is a real reading, not missing data)")
	}
	if *sig.RSI14 != 0 {
		t.Fatalf("monotonic decline RSI = %v, want exactly 0", *sig.RSI14)
	}
}

// The insufficient-data case must still omit the field: ≤15 bars cannot
// produce a 14-period RSI.
func TestTFSignalOmitsRSIWhenInsufficientData(t *testing.T) {
	now := time.Now()
	tfd := &market.TimeframeSeriesData{Timeframe: "1h"}
	for i := 0; i < 12; i++ {
		p := 100 - float64(i)
		barTime := now.Add(time.Duration(i-12) * time.Hour)
		tfd.Klines = append(tfd.Klines, market.KlineBar{
			Time: barTime.UnixMilli(), Open: p + 0.5, High: p + 0.6, Low: p - 0.6, Close: p, Volume: 10,
		})
	}
	sig, _ := computeTFSignal("1h", tfd, now, 88)
	if sig == nil {
		t.Fatal("computeTFSignal returned nil")
	}
	if sig.RSI14 != nil {
		t.Fatalf("RSI14 must be omitted with insufficient history, got %.1f", *sig.RSI14)
	}
}
