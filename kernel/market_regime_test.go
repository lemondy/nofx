package kernel

import (
	"math"
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

func TestRequiredRegimeTimeframesPreserveOrderAndDeduplicate(t *testing.T) {
	in := []string{"5m", "15m", "1h", "15m"}
	got := withRequiredRegimeTimeframes(in)
	want := []string{"5m", "15m", "1h", "4h"}
	if len(got) != len(want) {
		t.Fatalf("timeframes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("timeframes = %v, want %v", got, want)
		}
	}
	if len(in) != 4 || in[3] != "15m" {
		t.Fatalf("input config mutated: %v", in)
	}
}

func regimeTF(now time.Time, tf string, bars int, price func(int) float64) *market.TimeframeSeriesData {
	dur := tfDuration(tf)
	series := &market.TimeframeSeriesData{Timeframe: tf}
	start := now.Add(-time.Duration(bars) * dur)
	for i := 0; i < bars; i++ {
		p := price(i)
		spread := p * 0.002
		series.Klines = append(series.Klines, market.KlineBar{
			Time: start.Add(time.Duration(i) * dur).UnixMilli(),
			Open: p - spread/4, High: p + spread, Low: p - spread,
			Close: p, Volume: 1000 + float64(i),
		})
	}
	return series
}

func TestComputeMarketRegimeTrendDirectionAndConfirmation(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		step float64
		want string
	}{
		{name: "up", step: 0.002, want: "TREND_UP"},
		{name: "down", step: -0.002, want: "TREND_DOWN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			price := func(i int) float64 { return 100 * math.Exp(tc.step*float64(i)) }
			data := &market.Data{TimeframeData: map[string]*market.TimeframeSeriesData{
				"1h": regimeTF(now, "1h", 100, price),
				"4h": regimeTF(now, "4h", 100, price),
			}}
			r := computeMarketRegime(data, now)
			if r.Regime != tc.want || r.TrendState == "UNKNOWN" {
				t.Fatalf("regime = %+v, want %s", r, tc.want)
			}
			if r.ConfirmedBars != 2 {
				t.Fatalf("confirmed_bars = %d, want 2", r.ConfirmedBars)
			}
			if !r.BarsClosed || r.AsOfUTC != now.Format(time.RFC3339) {
				t.Fatalf("closed/as-of metadata wrong: %+v", r)
			}
			if r.Inputs.ADX1h == nil || *r.Inputs.ADX1h < regimeADXTrend1h ||
				r.Inputs.ADX4h == nil || *r.Inputs.ADX4h < regimeADXTrend4h {
				t.Fatalf("trend must carry strong ADX evidence: %+v", r.Inputs)
			}
		})
	}
}

func TestComputeMarketRegimeHighVolatilityChop(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	chop := func(i int) float64 {
		amplitude := 0.002 + 0.0007*float64(i)
		if i%2 == 0 {
			return 100 * (1 + amplitude)
		}
		return 100 * (1 - amplitude)
	}
	flat := func(i int) float64 { return 100 + math.Sin(float64(i)/3)*0.03 }
	data := &market.Data{TimeframeData: map[string]*market.TimeframeSeriesData{
		"1h": regimeTF(now, "1h", 100, chop),
		"4h": regimeTF(now, "4h", 100, flat),
	}}
	r := computeMarketRegime(data, now)
	if r.TrendState != "RANGE" {
		t.Fatalf("trend_state = %s, want RANGE: %+v", r.TrendState, r)
	}
	if r.Regime != "CHOP_HIGH_VOL" {
		t.Fatalf("regime = %s, want CHOP_HIGH_VOL: %+v", r.Regime, r)
	}
	if r.VolatilityState != "HIGH" && r.VolatilityState != "EXPANDING" {
		t.Fatalf("volatility_state = %s, want HIGH/EXPANDING", r.VolatilityState)
	}
}

func TestComputeMarketRegimeMissingTimeframeFailsClosed(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	data := &market.Data{TimeframeData: map[string]*market.TimeframeSeriesData{
		"1h": regimeTF(now, "1h", 100, func(i int) float64 { return 100 + float64(i) }),
	}}
	r := computeMarketRegime(data, now)
	if r.Regime != "UNKNOWN" || r.BarsClosed || r.ConfirmedBars != 0 {
		t.Fatalf("missing 4h must fail closed: %+v", r)
	}
	if marketRegimeAllowsException(&SymbolSignal{MarketRegime: r}, true) {
		t.Fatal("UNKNOWN regime must not allow a market exception")
	}
}

func TestWilderADXFlatIsValidZero(t *testing.T) {
	bars := make([]market.Kline, 60)
	for i := range bars {
		bars[i] = market.Kline{Open: 100, High: 100, Low: 100, Close: 100}
	}
	if got := wilderADX(bars, 14); got != 0 {
		t.Fatalf("flat ADX = %g, want 0", got)
	}
}

func TestMarketRegimePromptContract(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.LimitEntryEnabled = true
	prompt := NewStrategyEngine(cfg).BuildSystemPrompt(100, "")
	for _, want := range []string{
		"market_regime.confirmed_bars", "TREND_UP", "TREND_DOWN",
		"RANGE_LOW_VOL/RANGE_NORMAL/CHOP_HIGH_VOL/UNKNOWN",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("system prompt missing regime contract %q", want)
		}
	}
	for _, want := range []string{"Market regime (program-computed", "ADX", "Bollinger width", "do not veto an otherwise valid structure-based limit entry"} {
		if !strings.Contains(signalBlockLegend, want) {
			t.Fatalf("signal legend missing regime contract %q", want)
		}
	}
}
