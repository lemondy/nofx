package kernel

import (
	"math"
	"nofx/market"
	"testing"
	"time"
)

func volatileData() *market.Data {
	now := time.Now().Truncate(time.Hour).Add(30 * time.Minute)
	d := &market.Data{Symbol: "XUSDT", CurrentPrice: 100, SampledAt: now, TimeframeData: map[string]*market.TimeframeSeriesData{"1h": {Timeframe: "1h"}}}
	for i := 0; i < 60; i++ {
		d.TimeframeData["1h"].Klines = append(d.TimeframeData["1h"].Klines, market.KlineBar{Time: now.Truncate(time.Hour).Add(time.Duration(i-59) * time.Hour).UnixMilli(), Open: 100, High: 101, Low: 99, Close: 100, Volume: 100})
	}
	d.TimeframeData["1h"].Klines[59].High = 107 // range8% / baseline2% =4
	return d
}
func TestVolatileGatesBothDirections(t *testing.T) {
	oi := -31.0
	opt := SignalOptions{PrimaryTF: "1h", MinQuoteVolume24hMillions: 30, QuoteVolume24hUsd: 29e6, VolSpikeMult: 3, OISurge1hPct: 30, OI1hPct: &oi}
	sig, err := ComputeSymbolSignals("XUSDT", volatileData(), opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []*DirectionGate{sig.HardGate.Long, sig.HardGate.Short} {
		for _, prefix := range []string{"THIN_VOLUME_29.00_LT_30.00", "VOL_SPIKE_1H_4.00_GT_3.00", "OI_SURGE_31.00_GT_30.00"} {
			if !hasCode(g, prefix) {
				t.Fatalf("missing %s in %v", prefix, g.Failed)
			}
		}
	}
}
func TestVolatileGateBoundariesMissingAndExemption(t *testing.T) {
	market.SetEquityClassificationForTesting(map[string]bool{"AAPLUSDT": true}, map[string]bool{"AAPLUSDT": true})
	defer market.SetEquityClassificationForTesting(nil, nil)
	ratio, oi := 3.0, 30.0
	opt := SignalOptions{MinQuoteVolume24hMillions: 30, QuoteVolume24hUsd: 30e6, VolSpikeMult: 3, VolSpike1hRatio: &ratio, OISurge1hPct: 30, OI1hPct: &oi}
	if got := volatileEntryCodes("XUSDT", opt); len(got) != 0 {
		t.Fatal(got)
	}
	opt.QuoteVolume24hUsd = 1
	ratio = 4
	oi = 31
	if got := volatileEntryCodes("AAPLUSDT", opt); len(got) != 0 {
		t.Fatal("bstock not exempt", got)
	}
	if got := volatileEntryCodes("BTCUSDT", SignalOptions{MinQuoteVolume24hMillions: 30, QuoteVolume24hUsd: 1}); len(got) != 0 {
		t.Fatal("BTC turnover exemption", got)
	}
	spike := 9.0
	for _, sym := range []string{"BTCUSDT", "ETHUSDT"} {
		got := volatileEntryCodes(sym, SignalOptions{VolSpikeMult: 4, VolSpike1hRatio: &spike})
		if len(got) != 0 {
			t.Fatal(sym, "must be exempt from VOL_SPIKE_1H", got)
		}
	}
	if got := volatileEntryCodes("IRYSUSDT", SignalOptions{VolSpikeMult: 4, VolSpike1hRatio: &spike}); len(got) != 1 {
		t.Fatal("altcoin must still hit VOL_SPIKE_1H", got)
	}
	opt.QuoteVolume24hUsd = 0
	opt.VolSpike1hRatio = nil
	opt.OI1hPct = nil
	if got := volatileEntryCodes("XUSDT", opt); len(got) != 0 {
		t.Fatal("missing data must fail open", got)
	}
	opt.VolSpike1hRatio = &ratio
	opt.OI1hPct = &oi
	opt.MinQuoteVolume24hMillions = 0
	opt.VolSpikeMult = 0
	opt.OISurge1hPct = 0
	if got := volatileEntryCodes("XUSDT", opt); len(got) != 0 {
		t.Fatal("disabled gate fired", got)
	}
}
func TestHourlyRangeSpikeHistoryValidation(t *testing.T) {
	d := volatileData()
	if v := hourlyRangeSpike(d); v == nil || math.Abs(*v-4) > 1e-9 {
		t.Fatal("wrong ratio", v)
	}
	d.TimeframeData["1h"].Klines[58].Time -= 1
	if hourlyRangeSpike(d) != nil {
		t.Fatal("gap passed")
	}
	d = volatileData()
	d.SampledAt = d.SampledAt.Add(3 * time.Hour)
	if hourlyRangeSpike(d) != nil {
		t.Fatal("stale passed")
	}
	d = volatileData()
	d.TimeframeData["1h"].Klines = d.TimeframeData["1h"].Klines[:24]
	if hourlyRangeSpike(d) != nil {
		t.Fatal("short history passed")
	}
}
