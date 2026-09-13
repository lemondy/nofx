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
	for _, want := range []string{"布林上轨骑行", "`bb_ride.ride`=true", "confidence≥80"} {
		if !strings.Contains(sp, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}
