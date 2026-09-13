package kernel

import (
	"strings"
	"testing"

	"nofx/market"
)

// formatTechnicalNarrative must turn a pump-crash-range daily chart into the
// human-readable narrative the AI reasons over.
func TestFormatTechnicalNarrativePumpCrashRange(t *testing.T) {
	data := &market.Data{TimeframeData: map[string]*market.TimeframeSeriesData{}}
	// 22 daily bars (production fetches 30, enough for MA20): drift up to a
	// spike high, one crash bar, then a wide range below the high with the
	// last bar recovering to mid-box.
	bars := make([]market.KlineBar, 0, 22)
	base := 60.0
	day := int64(1787900000000) // arbitrary epoch, format only
	step := int64(24 * 3600 * 1000)
	for i := 0; i < 8; i++ {
		base *= 1.01
		bars = append(bars, market.KlineBar{Time: day, Open: base, High: base * 1.01, Low: base * 0.99, Close: base, Volume: 1000})
		day += step
	}
	// spike to 76.9
	spike := 76.9
	bars = append(bars, market.KlineBar{Time: day, Open: base, High: spike, Low: base, Close: spike * 0.98, Volume: 2600})
	day += step
	// crash bar -9%
	crash := spike * 0.98 * 0.91
	bars = append(bars, market.KlineBar{Time: day, Open: spike * 0.98, High: spike * 0.98, Low: crash, Close: crash, Volume: 2400})
	day += step
	// range 62-71: deterministic wide oscillation below the spike high
	osc := []float64{70.5, 64.5, 71.0, 63.0, 69.0, 62.5, 70.8, 63.5, 70.0, 64.3}
	for _, p := range osc {
		bars = append(bars, market.KlineBar{Time: day, Open: p * 1.005, High: p * 1.02, Low: p * 0.98, Close: p, Volume: 1200})
		day += step
	}

	data.TimeframeData["1d"] = &market.TimeframeSeriesData{Timeframe: "1d", Klines: bars}

	out := formatTechnicalNarrative(data)
	if out == "" {
		t.Fatal("narrative is empty")
	}
	for _, want := range []string{"技术面：", "剧烈波动", "单日暴跌", "冲高", "振幅", "放量", "今日报 64.3", "形态判断", "20 日线"} {
		if !strings.Contains(out, want) {
			t.Errorf("narrative missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "76.9") || !strings.Contains(out, "单日暴跌") {
		t.Errorf("narrative should cite the spike high 76.9 and the crash day:\n%s", out)
	}
}

// Without daily bars the narrative must be skipped entirely (empty string).
func TestFormatTechnicalNarrativeNoDaily(t *testing.T) {
	data := &market.Data{TimeframeData: map[string]*market.TimeframeSeriesData{
		"1h": {Timeframe: "1h", Klines: []market.KlineBar{
			{Time: 1, Open: 1, High: 1, Low: 1, Close: 1, Volume: 1},
		}},
	}}
	if out := formatTechnicalNarrative(data); out != "" {
		t.Fatalf("expected empty narrative without 1d data, got:\n%s", out)
	}
}
