package kernel

import (
	"math"
	"nofx/market"
	"nofx/store"
	"strings"
	"sync"
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
		for _, prefix := range []string{"THIN_VOLUME_29.00_LT_30.00", "VOL_SPIKE_1H_4.00_GT_3.00", "OI_SURGE_1H_31.00_GT_30.00"} {
			if !hasCode(g, prefix) {
				t.Fatalf("missing %s in %v", prefix, g.Failed)
			}
		}
	}
}
func TestVolatileGateBoundariesMissingAndExemption(t *testing.T) {
	market.SetEquityClassificationForTesting(map[string]bool{"AAPLUSDT": true}, map[string]bool{"AAPLUSDT": true})
	defer market.SetEquityClassificationForTesting(nil, nil)
	ratio, oi := 2.99, 30.0
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
	if v := hourlyRangeSpike(d, d.SampledAt); v == nil || math.Abs(*v-4) > 1e-9 {
		t.Fatal("wrong ratio", v)
	}
	d.TimeframeData["1h"].Klines[58].Time -= 1
	if hourlyRangeSpike(d, d.SampledAt) != nil {
		t.Fatal("gap passed")
	}
	d = volatileData()
	d.SampledAt = d.SampledAt.Add(3 * time.Hour)
	if hourlyRangeSpike(d, d.SampledAt) != nil {
		t.Fatal("stale passed")
	}
	d = volatileData()
	d.TimeframeData["1h"].Klines = d.TimeframeData["1h"].Klines[:24]
	if hourlyRangeSpike(d, d.SampledAt) != nil {
		t.Fatal("short history passed")
	}
}

func rollingVolatileData(tf string, count int, now time.Time) *market.Data {
	period := market.TimeframeDuration(tf)
	d := &market.Data{Symbol: "ROLLUSDT", CurrentPrice: 100, SampledAt: now, TimeframeData: map[string]*market.TimeframeSeriesData{tf: {Timeframe: tf}}}
	for i := 0; i < count; i++ {
		d.TimeframeData[tf].Klines = append(d.TimeframeData[tf].Klines, market.KlineBar{
			Time: now.Truncate(period).Add(time.Duration(i-count+1) * period).UnixMilli(),
			Open: 100, High: 101, Low: 99, Close: 100, Volume: 100,
		})
	}
	return d
}

func TestRollingRangeIncludesFormingBarAndSurvivesHourBoundary(t *testing.T) {
	now := time.Date(2026, 10, 25, 1, 2, 0, 0, time.UTC)
	d := rollingVolatileData("5m", 311, now)
	bars := d.TimeframeData["5m"].Klines
	bars[len(bars)-1].High = 119 // forming 01:00 bar, range 20 / 2 = 10
	five, fifteen, hour := volatilityRangeSpikes(d, now)
	for _, v := range []*float64{five, fifteen, hour} {
		if v == nil || *v != 10 {
			t.Fatalf("forming bar excluded: %v", v)
		}
	}
	bars[len(bars)-1].High = 101
	bars[len(bars)-2].High = 119 // event at 00:55, new 01:00 bar is quiet
	five, fifteen, hour = volatilityRangeSpikes(d, now)
	if five == nil || *five != 1 || fifteen == nil || *fifteen != 10 || hour == nil || *hour != 10 {
		t.Fatalf("hour rollover erased rolling event: %v %v %v", five, fifteen, hour)
	}
	opt := SignalOptions{Now: now, VolSpike5mMult: 8, VolSpike15mMult: 6, VolSpikeMult: 5}
	sig, err := ComputeSymbolSignals(d.Symbol, d, opt)
	if err != nil || !hasCode(sig.HardGate.Long, "VOL_SPIKE_15M_10.00_GT_6.00") || !hasCode(sig.HardGate.Short, "VOL_SPIKE_1H_10.00_GT_5.00") {
		t.Fatalf("rollover must ban both directions: %v %v", sig, err)
	}
}

func TestRollingRangeBaselineDoesNotOverlapCurrentWindow(t *testing.T) {
	now := time.Date(2026, 10, 25, 1, 2, 0, 0, time.UTC)
	d := rollingVolatileData("5m", 311, now)
	bars := d.TimeframeData["5m"].Klines
	for i := range bars {
		bars[i].High = 101 + float64(i)*.1
		if i >= len(bars)-12 {
			bars[i].High = 200
		}
	}
	// Historical 12-bar windows end at indices 11..298; median range
	// is 2 + (154+155)/2 * .1 = 17.45. No index 299..310 may enter it.
	v := rollingRangeSpike(bars, 5*time.Minute, 12, 288, 144, now)
	if v == nil || math.Abs(*v-101/17.45) > 1e-9 {
		t.Fatalf("self-contaminated baseline: %v", v)
	}
	// Window open, not last open/close, is the denominator.
	bars[len(bars)-12].Open = 200
	v = rollingRangeSpike(bars, 5*time.Minute, 12, 288, 144, now)
	if v == nil || math.Abs(*v-50.5/17.45) > 1e-9 {
		t.Fatalf("wrong window denominator: %v", v)
	}
}

func TestRollingRangeHistoryValidationAndMinimum(t *testing.T) {
	now := time.Date(2026, 10, 25, 1, 2, 0, 0, time.UTC)
	for _, window := range []int{1, 3, 12} {
		for _, count := range []int{144 + 2*window - 2, 144 + 2*window - 1, 200, 311} {
			d := rollingVolatileData("5m", count, now)
			v := rollingRangeSpike(d.TimeframeData["5m"].Klines, 5*time.Minute, window, 288, 144, now)
			if (v != nil) != (count >= 144+2*window-1) || (v != nil && *v != 1) {
				t.Fatalf("window=%d count=%d ratio=%v", window, count, v)
			}
		}
	}
	for _, invalid := range []string{"gap", "stale", "future", "nan", "zero-open", "bad-range", "infinite", "zero-baseline"} {
		t.Run(invalid, func(t *testing.T) {
			d := rollingVolatileData("5m", 311, now)
			bars := d.TimeframeData["5m"].Klines
			eval := now
			switch invalid {
			case "gap":
				bars[len(bars)-2].Time--
			case "stale":
				eval = now.Add(11 * time.Minute)
			case "future":
				eval = now.Add(-5 * time.Minute)
			case "nan":
				bars[len(bars)-1].High = math.NaN()
			case "zero-open":
				bars[len(bars)-1].Open = 0
			case "bad-range":
				bars[len(bars)-1].Low = 102
			case "infinite":
				bars[len(bars)-1].High = math.Inf(1)
			case "zero-baseline":
				for i := range bars {
					bars[i].High, bars[i].Low = 100, 100
				}
			}
			for _, w := range []int{1, 3, 12} {
				if rollingRangeSpike(bars, 5*time.Minute, w, 288, 144, eval) != nil {
					t.Fatal("invalid data did not fail open", w)
				}
			}
		})
	}
}

func TestVolatilityDataFallbackChain(t *testing.T) {
	now := time.Date(2026, 10, 25, 1, 2, 0, 0, time.UTC)
	d := rollingVolatileData("5m", 200, now)
	d.TimeframeData["15m"] = rollingVolatileData("15m", 104, now).TimeframeData["15m"]
	d.TimeframeData["1h"] = rollingVolatileData("1h", 25, now).TimeframeData["1h"]
	for tf, high := range map[string]float64{"5m": 119, "15m": 139, "1h": 159} {
		bars := d.TimeframeData[tf].Klines
		bars[len(bars)-1].High = high
	}
	five, fifteen, hour := volatilityRangeSpikes(d, now)
	if five == nil || *five != 10 || fifteen == nil || *fifteen != 10 || hour == nil || *hour != 10 {
		t.Fatal("5m not preferred")
	}
	delete(d.TimeframeData, "5m")
	five, fifteen, hour = volatilityRangeSpikes(d, now)
	if five != nil || fifteen == nil || *fifteen != 20 || hour == nil || *hour != 20 {
		t.Fatal("15m fallback wrong")
	}
	d.TimeframeData["5m"] = rollingVolatileData("5m", 100, now).TimeframeData["5m"]
	five, fifteen, hour = volatilityRangeSpikes(d, now)
	if five != nil || fifteen == nil || *fifteen != 20 || hour == nil || *hour != 20 {
		t.Fatal("too-short 5m must fall back")
	}
	delete(d.TimeframeData, "5m")
	// Legacy execution datasets have only ~20 aggregated 15m bars.
	d.TimeframeData["15m"].Klines = d.TimeframeData["15m"].Klines[84:]
	five, fifteen, hour = volatilityRangeSpikes(d, now)
	if five != nil || fifteen != nil || hour == nil || *hour != 30 {
		t.Fatal("1h fallback wrong")
	}
	delete(d.TimeframeData, "1h")
	five, fifteen, hour = volatilityRangeSpikes(d, now)
	if five != nil || fifteen != nil || hour != nil {
		t.Fatal("missing data did not fail open")
	}
	// Corrupt a sufficiently long 5m series: never hide its gap using 15m.
	d = rollingVolatileData("5m", 200, now)
	d.TimeframeData["5m"].Klines[198].Time--
	d.TimeframeData["15m"] = rollingVolatileData("15m", 104, now).TimeframeData["15m"]
	five, fifteen, hour = volatilityRangeSpikes(d, now)
	if five != nil || fifteen != nil || hour != nil {
		t.Fatal("corrupt source masked by fallback")
	}
}

func TestVolatileAnyWindowAndOIAbsolute(t *testing.T) {
	for _, code := range []string{"VOL_SPIKE_5M_", "VOL_SPIKE_15M_", "VOL_SPIKE_1H_", "OI_SURGE_15M_", "OI_SURGE_1H_"} {
		for _, sign := range []float64{-1, 1} {
			opt := SignalOptions{VolSpike5mMult: 8, VolSpike15mMult: 6, VolSpikeMult: 5, OISurge15mPct: 8, OISurge1hPct: 15}
			v := 20.0
			switch code {
			case "VOL_SPIKE_5M_":
				opt.VolSpike5mRatio = &v
			case "VOL_SPIKE_15M_":
				opt.VolSpike15mRatio = &v
			case "VOL_SPIKE_1H_":
				opt.VolSpike1hRatio = &v
			case "OI_SURGE_15M_":
				v *= sign
				opt.OI15mPct = &v
			case "OI_SURGE_1H_":
				v *= sign
				opt.OI1hPct = &v
			}
			codes := volatileEntryCodes("WINDOWUSDT", opt)
			if len(codes) != 1 || !strings.HasPrefix(codes[0], code) {
				t.Fatal(code, codes)
			}
		}
	}
	v := 8.0
	if codes := volatileEntryCodes("WINDOWUSDT", SignalOptions{VolSpike5mMult: 8, VolSpike5mRatio: &v}); len(codes) != 1 {
		t.Fatal("replay boundary >=8 lost", codes)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		opt := SignalOptions{VolSpike5mMult: 8, VolSpike5mRatio: &v, OISurge15mPct: 8, OI15mPct: &v}
		if codes := volatileEntryCodes("WINDOWUSDT", opt); len(codes) != 0 {
			t.Fatal("invalid must fail open", codes)
		}
	}
}

func resetVolatileHoldoffs(t *testing.T) {
	reset := func() {
		volatileHoldoffs.mu.Lock()
		volatileHoldoffs.last = map[string]time.Time{}
		volatileHoldoffs.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func TestVolatileHoldoffClockExpiryAndExemptions(t *testing.T) {
	resetVolatileHoldoffs(t)
	market.SetEquityClassificationForTesting(map[string]bool{"AAPLUSDT": true}, map[string]bool{"AAPLUSDT": true})
	defer market.SetEquityClassificationForTesting(nil, nil)
	now := time.Date(2026, 10, 25, 1, 2, 0, 0, time.UTC)
	spike := 9.0
	for _, cause := range []string{"price", "oi"} {
		sym := strings.ToUpper(cause) + "USDT"
		opt := SignalOptions{Now: now, VolSpikeHoldoffMinutes: 30}
		if cause == "price" {
			opt.VolSpike5mMult, opt.VolSpike5mRatio = 8, &spike
		} else {
			opt.OISurge15mPct, opt.OI15mPct = 8, &spike
		}
		if codes := volatileEntryCodes(sym, opt); len(codes) != 1 || strings.HasPrefix(codes[0], "VOL_HOLDOFF_") {
			t.Fatal(cause, codes)
		}
		// Missing current data must not erase an observed event; both sides ban.
		quiet := SignalOptions{Now: now.Add(29 * time.Minute), VolSpikeHoldoffMinutes: 30}
		sig := &SymbolSignal{Symbol: sym, Timeframes: map[string]*TFSignal{}}
		gate := computeHardEntryGate(sig, quiet)
		if !hasCode(gate.Long, "VOL_HOLDOFF_30m") || !hasCode(gate.Short, "VOL_HOLDOFF_30m") {
			t.Fatal("holdoff bypass", gate)
		}
		quiet.Now = now.Add(30 * time.Minute)
		if codes := volatileEntryCodes(sym, quiet); len(codes) != 0 {
			t.Fatal("expiry did not release", codes)
		}
	}
	for _, sym := range []string{"BTCUSDT", "ETHUSDT", "AAPLUSDT"} {
		opt := SignalOptions{Now: now, VolSpikeHoldoffMinutes: 30,
			VolSpike5mMult: 8, VolSpike5mRatio: &spike, VolSpike15mMult: 6, VolSpike15mRatio: &spike, VolSpikeMult: 5, VolSpike1hRatio: &spike,
			OISurge15mPct: 8, OI15mPct: &spike, OISurge1hPct: 8, OI1hPct: &spike}
		// Even a pre-existing state must not ban exempt assets.
		volatileHoldoffs.active(sym, now, 30, true)
		if codes := volatileEntryCodes(sym, opt); len(codes) != 0 {
			t.Fatal("exemption", sym, codes)
		}
	}
	if codes := volatileEntryCodes("OTHERUSDT", SignalOptions{Now: now, VolSpikeHoldoffMinutes: 30}); len(codes) != 0 {
		t.Fatal("symbol state leaked", codes)
	}
	for _, minutes := range []float64{0, -1} {
		volatileHoldoffs.active("OFFUSDT", now, 30, true)
		if codes := volatileEntryCodes("OFFUSDT", SignalOptions{Now: now, VolSpikeHoldoffMinutes: minutes}); len(codes) != 0 {
			t.Fatal("holdoff off", codes)
		}
	}
	// Thin volume alone is not a manipulation event and must not arm holdoff.
	volatileEntryCodes("THINUSDT", SignalOptions{Now: now, VolSpikeHoldoffMinutes: 30, MinQuoteVolume24hMillions: 5, QuoteVolume24hUsd: 1e6})
	if codes := volatileEntryCodes("THINUSDT", SignalOptions{Now: now.Add(time.Minute), VolSpikeHoldoffMinutes: 30}); len(codes) != 0 {
		t.Fatal("turnover armed holdoff", codes)
	}
}

func TestVolatileQuantContextPreservedWithoutReplacingSampleHistory(t *testing.T) {
	stubLiveMarket(t, neutralBTC4h())
	d := volatileData()
	d.Symbol = "QUANTONLYUSDT" // no sampled local history for this symbol
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.VolSpike5mMult, cfg.RiskControl.VolSpike15mMult, cfg.RiskControl.VolSpikeMult = -1, -1, -1
	q := &QuantData{OI: map[string]*OIData{"binance": {Delta: map[string]*OIDeltaData{"1h": {OIDeltaPercent: 50}}}}}
	sig := NewStrategyEngine(cfg).computeCoinSignal(d, q, nil, nil)
	if sig == nil || sig.Derivatives == nil || sig.Derivatives.OIChange1hPct == nil || *sig.Derivatives.OIChange1hPct != 50 {
		t.Fatal("existing quant direction/context changed", sig)
	}
	for _, gate := range []*DirectionGate{sig.HardGate.Long, sig.HardGate.Short} {
		for _, code := range gate.Failed {
			if strings.HasPrefix(code, "OI_SURGE_") || strings.HasPrefix(code, "VOL_HOLDOFF_") {
				t.Fatal("untimestamped quant bypassed sampled history validation", code)
			}
		}
	}
}

func TestVolatileHoldoffRefreshAndConcurrency(t *testing.T) {
	resetVolatileHoldoffs(t)
	now := time.Date(2026, 10, 25, 1, 2, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				volatileHoldoffs.active("CONCURRENTUSDT", now, 30, true)
			}
		}()
	}
	wg.Wait()
	volatileHoldoffs.active("CONCURRENTUSDT", now.Add(20*time.Minute), 30, true)
	if !volatileHoldoffs.active("CONCURRENTUSDT", now.Add(49*time.Minute), 30, false) {
		t.Fatal("new anomaly did not refresh")
	}
	if volatileHoldoffs.active("CONCURRENTUSDT", now.Add(50*time.Minute), 30, false) {
		t.Fatal("refreshed expiry not released")
	}
}

func TestVolatileStaleSnapshotUsesEvaluationClock(t *testing.T) {
	now := time.Now().Add(-time.Hour)
	d := rollingVolatileData("5m", 200, now)
	d.TimeframeData["5m"].Klines[199].High = 150
	// SampledAt remains fresh relative to bars, but the caller is evaluating
	// the snapshot much later. New price gates must fail open on stale bars.
	for _, eval := range []time.Time{now.Add(time.Hour), {}} {
		sig, err := ComputeSymbolSignals(d.Symbol, d, SignalOptions{Now: eval, VolSpike5mMult: 8, VolSpike15mMult: 6, VolSpikeMult: 5})
		if err != nil {
			t.Fatal(err)
		}
		for _, gate := range []*DirectionGate{sig.HardGate.Long, sig.HardGate.Short} {
			for _, code := range gate.Failed {
				if strings.HasPrefix(code, "VOL_SPIKE_") {
					t.Fatal("stale snapshot fired", code)
				}
			}
		}
	}
}
