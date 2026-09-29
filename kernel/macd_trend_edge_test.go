package kernel

import (
	"testing"
	"time"

	"nofx/market"
)

// 09-28 review P3: with exactly EMA-slow (26) closed bars,
// ExportCalculateMACD on kb[:len-1] returns 0 (insufficient data) and the
// sign comparison mislabeled a rising line as falling and vice versa — the
// trend label must stay ABSENT when the previous-bar MACD is not
// computable. Also pins the MACDHist division guard (a zero close in the
// vendor tail must not produce ±Inf and fail the whole signal JSON).
func TestMACDTrendAbsentWithExactlySlowPeriodBars(t *testing.T) {
	now := time.Now()
	mk := func(n int, close func(i int) float64) *market.TimeframeSeriesData {
		tfd := &market.TimeframeSeriesData{Timeframe: "1h"}
		for i := 0; i < n; i++ {
			p := close(i)
			barTime := now.Add(time.Duration(i-n) * time.Hour)
			tfd.Klines = append(tfd.Klines, market.KlineBar{
				Time: barTime.UnixMilli(), Open: p * 0.999, High: p * 1.002, Low: p * 0.998, Close: p, Volume: 10,
			})
		}
		return tfd
	}
	// 26 bars: MACD line computable, previous-bar MACD NOT (25 bars).
	steady := mk(26, func(i int) float64 { return 100 + float64(i)*0.05 })
	sig, _ := computeTFSignal("1h", steady, now, 101.25)
	if sig == nil {
		t.Fatal("computeTFSignal returned nil")
	}
	if sig.MACDHist == nil {
		t.Fatal("MACDHist (price-normalized line) must still render at 26 bars")
	}
	if sig.MACDTrend != "" {
		t.Fatalf("26-bar series must leave macd_trend unlabeled (prev-bar MACD not computable), got %q", sig.MACDTrend)
	}

	// 27 bars: previous-bar MACD valid → the label appears and reflects the
	// direction of the line change.
	rising := mk(27, func(i int) float64 { return 100 + float64(i)*0.05 })
	sig27, _ := computeTFSignal("1h", rising, now, 101.3)
	if sig27 == nil || sig27.MACDTrend == "" {
		t.Fatalf("27-bar series must carry a macd_trend label, got %q", sig27.MACDTrend)
	}
}
