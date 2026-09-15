package kernel

import (
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

// bbRideSeries builds n closed 15m bars: `up` consecutive rising bars hugging
// a rising band at the tail, preceded by flat bars.
func bbRideSeries(n, up int) []market.KlineBar {
	base := time.Now().Add(-time.Duration(n+1) * 15 * time.Minute)
	bars := make([]market.KlineBar, 0, n)
	p := 100.0
	for i := 0; i < n; i++ {
		p *= 1.001 // gentle drift for the flat prefix
		o, c := p, p*1.0002
		vol := 10.0
		h := c * 1.0005
		if i >= n-up { // the ride tail: strong bullish bars through the band
			o = p
			c = p * 1.01
			h = c * 1.004
			vol = 40
			p = c
		} else {
			p = c
		}
		bars = append(bars, market.KlineBar{Time: base.UnixMilli(), Open: o, High: h, Low: p * 0.995, Close: c, Volume: vol})
		base = base.Add(15 * time.Minute)
	}
	return bars
}

func TestComputeBBRide(t *testing.T) {
	// 30 flat bars then 5 strong ride bars: windows ≥3, surge, ride true.
	data := &market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{
		"15m": {Klines: bbRideSeries(30, 5)},
	}}
	r := computeBBRide(data)
	if r == nil {
		t.Fatal("nil ride for valid 15m data")
	}
	if r.Windows < 3 {
		t.Fatalf("ride windows = %d, want ≥3", r.Windows)
	}
	if !r.VolumeSurge {
		t.Fatal("volume surge not detected on 4× volume bars")
	}
	if !r.Ride {
		t.Fatal("ride must be true for band-riding surge series")
	}
	if r.UpperBand <= 0 {
		t.Fatal("upper band must be populated")
	}

	// A red bar at the tail stops the count at the bar before it.
	broken := bbRideSeries(30, 5)
	last := broken[len(broken)-1]
	broken[len(broken)-1] = market.KlineBar{Time: last.Time, Open: last.Open, High: last.Open * 1.0005, Low: last.Open * 0.995, Close: last.Open, Volume: 40}
	r2 := computeBBRide(&market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{"15m": {Klines: broken}}})
	if r2.Windows != 4 {
		t.Fatalf("red tail bar must stop the streak at 4, windows = %d", r2.Windows)
	}

	// Only 2 qualifying bars → below the ≥3 threshold → not a ride.
	short := computeBBRide(&market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{"15m": {Klines: bbRideSeries(30, 2)}}})
	if short.Windows >= 3 || short.Ride {
		t.Fatalf("2 qualifying bars must not ride: windows = %d, ride = %v", short.Windows, short.Ride)
	}

	// Volume absent → surge false even with band-riding bars.
	quiet := bbRideSeries(30, 5)
	for i := range quiet {
		quiet[i].Volume = 10
	}
	r3 := computeBBRide(&market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{"15m": {Klines: quiet}}})
	if r3.VolumeSurge || r3.Ride {
		t.Fatal("no volume surge → ride false")
	}

	if computeBBRide(nil) != nil {
		t.Fatal("nil data → nil ride")
	}
}

// The market-exception prompt line must reference the pre-computed flag and
// the confidence floor.
func TestPromptMentionsBBRide(t *testing.T) {
	engine := NewStrategyEngine(&store.StrategyConfig{})
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.LimitEntryEnabled = true
	engine = NewStrategyEngine(cfg)
	sp := engine.BuildSystemPrompt(100, "")
	for _, want := range []string{"布林上轨骑行", "`bb_ride.ride`=true", "confidence≥80",
		"布林下轨骑行", "`short_ride.ride`=true"} {
		if !strings.Contains(sp, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

// invertSeries mirrors a kline series about the price axis (p' = 1/p): every
// up bar becomes down, highs/lows swap, so the short-side fixture is the
// EXACT mirror of the proven long fixture — band asymmetries from handcrafted
// drift cannot silently break the symmetry test.
func invertSeries(bars []market.KlineBar) []market.KlineBar {
	out := make([]market.KlineBar, len(bars))
	for i, b := range bars {
		out[i] = market.KlineBar{Time: b.Time, Open: 1 / b.Open, High: 1 / b.Low, Low: 1 / b.High, Close: 1 / b.Close, Volume: b.Volume}
	}
	return out
}

func bbShortSeries(n, down int) []market.KlineBar { return invertSeries(bbRideSeries(n, down)) }

// The lower-band plunge ride (09-16 short todo, closing the "bb_ride 空头版"
// gap): the ZEC-style scenario — 15m already down, short anchor suppressed at
// the swing-low, waiting for the bounce to the anchor is design non-fill.
func TestComputeBBShortRide(t *testing.T) {
	data := &market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{
		"15m": {Klines: bbShortSeries(30, 5)},
	}}
	r := computeBBShortRide(data)
	if r == nil {
		t.Fatal("nil short ride for valid 15m data")
	}
	if r.Windows < 3 || !r.VolumeSurge || !r.Ride {
		t.Fatalf("plunge series must ride: %+v", r)
	}
	if r.LowerBand <= 0 {
		t.Fatal("lower band must be populated")
	}

	// An up bar at the tail stops the streak (built by inverting a source whose
	// tail bar was replaced with a green one).
	srcBroken := bbRideSeries(30, 5)
	last := srcBroken[len(srcBroken)-1]
	srcBroken[len(srcBroken)-1] = market.KlineBar{Time: last.Time, Open: last.Open, High: last.Open * 1.0005, Low: last.Open * 0.995, Close: last.Open * 1.001, Volume: 40}
	r2 := computeBBShortRide(&market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{"15m": {Klines: invertSeries(srcBroken)}}})
	if r2.Windows != 4 {
		t.Fatalf("green tail bar must stop the streak at 4, windows = %d", r2.Windows)
	}

	// Only 2 qualifying bars → below the ≥3 threshold.
	short := computeBBShortRide(&market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{"15m": {Klines: bbShortSeries(30, 2)}}})
	if short.Windows >= 3 || short.Ride {
		t.Fatalf("2 qualifying bars must not ride: %+v", short)
	}

	// Quiet volume → no ride even with band-riding bars.
	quietSrc := bbRideSeries(30, 5)
	for i := range quietSrc {
		quietSrc[i].Volume = 10
	}
	quiet := invertSeries(quietSrc)
	r3 := computeBBShortRide(&market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{"15m": {Klines: quiet}}})
	if r3.VolumeSurge || r3.Ride {
		t.Fatalf("no surge → no ride: %+v", r3)
	}

	if computeBBShortRide(nil) != nil {
		t.Fatal("nil data → nil ride")
	}
}

// The short_ride verdict must reach the model inside the per-coin JSON.
func TestShortRideRendersInSignal(t *testing.T) {
	now := time.Now()
	p := 100.0
	tf15 := &market.TimeframeSeriesData{Timeframe: "15m", Klines: bbShortSeries(30, 4)}
	p = tf15.Klines[len(tf15.Klines)-1].Close
	tf1h := buildTF("1h", now, 80, p, false)
	data := &market.Data{
		Symbol: "PLUNGEUSDT", CurrentPrice: p,
		TimeframeData: map[string]*market.TimeframeSeriesData{"15m": tf15, "1h": tf1h},
	}
	sig, err := ComputeSymbolSignals("PLUNGEUSDT", data, SignalOptions{Now: now, PrimaryTF: "15m", SLMinATRMult: 1.5, MinRR: 1.5})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if sig.ShortRide == nil {
		t.Fatal("short_ride missing from computed signal")
	}
	if !sig.ShortRide.Ride {
		t.Fatalf("plunge fixture must ride: %+v", sig.ShortRide)
	}
	js := RenderSignalJSON(sig)
	if !strings.Contains(js, `"short_ride"`) {
		t.Fatal("short_ride not rendered in JSON")
	}
}
