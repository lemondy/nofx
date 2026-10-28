package kernel

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"nofx/market"
)

// Review 2026-10-28 #17 (A3), #18 (A4), #23 (A2) and A8 scenario one.

func r1028Hours(n int, closeAt func(i int) float64, lastOpen time.Time) []market.KlineBar {
	bars := make([]market.KlineBar, n)
	for i := range bars {
		c := closeAt(i)
		bars[i] = market.KlineBar{
			Time: lastOpen.Add(-time.Duration(n-1-i) * time.Hour).UnixMilli(),
			Open: c, High: c, Low: c, Close: c, Volume: 1,
		}
	}
	return bars
}

// r1028Series mimics calculateTimeframeSeries: Klines is the last `count`
// bars, IndicatorKlines the full history (nil when it adds nothing).
func r1028Series(tf string, bars []market.KlineBar, count int) *market.TimeframeSeriesData {
	start := len(bars) - count
	if start < 0 {
		start = 0
	}
	td := &market.TimeframeSeriesData{Timeframe: tf, Klines: append([]market.KlineBar(nil), bars[start:]...)}
	if start > 0 {
		td.IndicatorKlines = bars
	}
	return td
}

func r1028Walk(n int, seed int64, lastOpen time.Time, step time.Duration) []market.KlineBar {
	rng := rand.New(rand.NewSource(seed))
	bars := make([]market.KlineBar, n)
	px := 100.0
	for i := range bars {
		o := px
		px *= 1 + rng.NormFloat64()*0.01
		hi := math.Max(o, px) * (1 + rng.Float64()*0.004)
		lo := math.Min(o, px) * (1 - rng.Float64()*0.004)
		bars[i] = market.KlineBar{
			Time: lastOpen.Add(-time.Duration(n-1-i) * step).UnixMilli(),
			Open: o, High: hi, Low: lo, Close: px, Volume: 100 + rng.Float64()*50,
		}
	}
	return bars
}

func TestReview20261028DisplayCountDoesNotChangeIndicatorSignals(t *testing.T) {
	lastOpen := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	now := lastOpen.Add(30 * time.Minute) // last bar still forming
	bars := r1028Walk(201, 7, lastOpen, time.Hour)

	ref, _ := computeTFSignal("1h", r1028Series("1h", bars, 201), now, 100)
	if ref.EMAFast == nil || ref.EMASlow == nil || ref.MACDHist == nil || ref.RSI14 == nil {
		t.Fatalf("reference signal incomplete: %+v", ref)
	}
	for _, count := range []int{30, 60, 90, 120, 199} {
		got, _ := computeTFSignal("1h", r1028Series("1h", bars, count), now, 100)
		if got.EMAFast == nil || got.EMASlow == nil || got.MACDHist == nil || got.RSI14 == nil {
			t.Fatalf("count=%d: incomplete signal", count)
		}
		if *got.EMAFast != *ref.EMAFast || *got.EMASlow != *ref.EMASlow {
			t.Errorf("count=%d: EMA %v/%v != reference %v/%v", count, *got.EMAFast, *got.EMASlow, *ref.EMAFast, *ref.EMASlow)
		}
		if got.Trend != ref.Trend {
			t.Errorf("count=%d: trend %q != %q", count, got.Trend, ref.Trend)
		}
		if got.ATRPct != ref.ATRPct {
			t.Errorf("count=%d: ATRPct %v != %v", count, got.ATRPct, ref.ATRPct)
		}
		if *got.MACDHist != *ref.MACDHist || got.MACDTrend != ref.MACDTrend {
			t.Errorf("count=%d: MACD %v/%s != %v/%s", count, *got.MACDHist, got.MACDTrend, *ref.MACDHist, ref.MACDTrend)
		}
		if *got.RSI14 != *ref.RSI14 {
			t.Errorf("count=%d: RSI14 %v != %v", count, *got.RSI14, *ref.RSI14)
		}
	}
}

func TestReview20261028StepChangeIsUptrendNotRange(t *testing.T) {
	lastOpen := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	now := lastOpen.Add(time.Hour + time.Minute) // every bar closed
	bars := r1028Hours(200, func(i int) float64 {
		if i < 140 {
			return 100
		}
		return 200
	}, lastOpen)

	sig, _ := computeTFSignal("1h", r1028Series("1h", bars, 60), now, 200)
	if sig.Trend != "up" {
		t.Fatalf("trend = %q, want up (EMA20 %v EMA50 %v)", sig.Trend, ptrVal(sig.EMAFast), ptrVal(sig.EMASlow))
	}
	if sig.EMAFast == nil || math.Abs(*sig.EMAFast-199.753363) > 1e-4 || math.Abs(*sig.EMASlow-190.931108) > 1e-4 {
		t.Fatalf("EMA20/EMA50 = %v/%v, want 199.753363/190.931108", ptrVal(sig.EMAFast), ptrVal(sig.EMASlow))
	}
	// Without the full history (old snapshots) the legacy behaviour remains.
	legacy, _ := computeTFSignal("1h", &market.TimeframeSeriesData{Timeframe: "1h", Klines: bars[140:]}, now, 200)
	if legacy.Trend != "range" {
		t.Fatalf("display-only fallback trend = %q, want range (unchanged legacy behaviour)", legacy.Trend)
	}
}

func ptrVal(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestReview20261028RegimeLineAgreesWithEMASlow(t *testing.T) {
	lastOpen := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	now := lastOpen.Add(30 * time.Minute)
	bars := r1028Walk(201, 11, lastOpen, time.Hour)
	for _, count := range []int{60, 100} {
		td := r1028Series("1h", bars, count)
		// Precomputed tail-aligned EMA50 as calculateTimeframeSeries builds it.
		full := barsToKlines(bars)
		for i := len(bars) - count; i < len(bars); i++ {
			td.EMA50Values = append(td.EMA50Values, market.ExportCalculateEMA(full[:i+1], 50))
		}
		data := &market.Data{Symbol: "TESTUSDT", CurrentPrice: 100, SampledAt: now, TimeframeData: map[string]*market.TimeframeSeriesData{"1h": td}}
		sig, _ := computeTFSignal("1h", td, now, 100)
		line := RegimeLine(data, "1h", now)
		if sig.EMASlow == nil || line != *sig.EMASlow {
			t.Fatalf("count=%d: regime_line %v != ema_slow %v", count, line, ptrVal(sig.EMASlow))
		}
	}
}

// A2: snapshot sampled 11:59:30 with the 11:00 bar still forming (close 151),
// executed 12:00:05. The EMA50 must be the sample-time one (100), not 102.
func TestReview20261028RegimeLineUsesSnapshotClock(t *testing.T) {
	sampled := time.Date(2026, 10, 28, 11, 59, 30, 0, time.UTC)
	exec := time.Date(2026, 10, 28, 12, 0, 5, 0, time.UTC)
	lastOpen := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	bars := r1028Hours(200, func(i int) float64 {
		if i == 199 {
			return 151
		}
		return 100
	}, lastOpen)

	for _, warm := range []bool{true, false} {
		td := r1028Series("1h", bars, 60)
		if !warm {
			td.IndicatorKlines = nil
			td.Klines = bars // 200-bar display so EMA50Values can be tail-aligned
		}
		full := barsToKlines(bars)
		for i := 49; i < len(bars); i++ {
			if !warm || i >= len(bars)-60 {
				td.EMA50Values = append(td.EMA50Values, market.ExportCalculateEMA(full[:i+1], 50))
			}
		}
		data := &market.Data{Symbol: "TESTUSDT", CurrentPrice: 151, SampledAt: sampled, TimeframeData: map[string]*market.TimeframeSeriesData{"1h": td}}
		if got := RegimeLine(data, "1h", exec); math.Abs(got-100) > 1e-9 {
			t.Errorf("warm=%v: RegimeLine with snapshot clock = %v, want 100", warm, got)
		}
		// Without a sampling time the caller's clock is all there is.
		data.SampledAt = time.Time{}
		if got := RegimeLine(data, "1h", exec); math.Abs(got-102) > 1e-9 {
			t.Errorf("warm=%v: RegimeLine with execution clock only = %v, want 102", warm, got)
		}
	}
}

func TestReview20261028ExecutionMicroTrendUsesSnapshotClock(t *testing.T) {
	sampled := time.Date(2026, 10, 28, 11, 59, 30, 0, time.UTC)
	exec := time.Date(2026, 10, 28, 12, 0, 5, 0, time.UTC)
	lastOpen := time.Date(2026, 10, 28, 11, 45, 0, 0, time.UTC) // forming at sample time
	n := 100
	bars := make([]market.KlineBar, n)
	for i := range bars {
		c := 100.0
		if i == n-1 {
			c = 130
		}
		bars[i] = market.KlineBar{
			Time: lastOpen.Add(-time.Duration(n-1-i) * 15 * time.Minute).UnixMilli(),
			Open: c, High: c, Low: c, Close: c, Volume: 1,
		}
	}
	data := &market.Data{Symbol: "TESTUSDT", CurrentPrice: 130, SampledAt: sampled,
		TimeframeData: map[string]*market.TimeframeSeriesData{"15m": {Timeframe: "15m", Klines: bars}}}

	_, atSample := ExecutionMicroTrend(data, sampled)
	_, atExec := ExecutionMicroTrend(data, exec)
	if atSample != "range" || atExec != atSample {
		t.Fatalf("snapshot clock: micro trend at sample=%q at exec=%q, want both range", atSample, atExec)
	}
	data.SampledAt = time.Time{}
	if _, trend := ExecutionMicroTrend(data, exec); trend != "up" {
		t.Fatalf("control: execution clock alone should count the bar as closed (up), got %q", trend)
	}
}

func TestReview20261028ATRSeedMatchesMarketAndSeries(t *testing.T) {
	// A4: 15 bars, bar 0 has High-Low=20, the other 14 TR=2, all closes 100.
	bars := make([]market.Kline, 15)
	for i := range bars {
		bars[i] = market.Kline{High: 101, Low: 99, Close: 100}
	}
	bars[0].High, bars[0].Low = 110, 90
	if got := wilderATR(bars, 14); math.Abs(got-2) > 1e-12 {
		t.Fatalf("wilderATR = %v, want 2", got)
	}
	if got := market.ExportCalculateATR(bars, 14); math.Abs(got-2) > 1e-12 {
		t.Fatalf("market ATR = %v, want 2", got)
	}
	if got := wilderATR(bars[:14], 14); got != 0 {
		t.Fatalf("14 bars must be unknown (0), got %v", got)
	}
	if got := wilderATR(nil, 14); got != 0 {
		t.Fatalf("empty must be 0, got %v", got)
	}

	// Kernel scalar == last of the series == market implementation on random data.
	rng := rand.New(rand.NewSource(3))
	for trial := 0; trial < 50; trial++ {
		n := 15 + rng.Intn(200)
		ks := make([]market.Kline, n)
		px := 50.0
		for i := range ks {
			o := px
			px *= 1 + rng.NormFloat64()*0.02
			ks[i] = market.Kline{Open: o, Close: px,
				High: math.Max(o, px) * (1 + rng.Float64()*0.01),
				Low:  math.Min(o, px) * (1 - rng.Float64()*0.01)}
		}
		series := wilderATRSeries(ks, 14)
		scalar := wilderATR(ks, 14)
		want := market.ExportCalculateATR(ks, 14)
		if scalar != series[len(series)-1] || math.Abs(scalar-want) > 1e-9*math.Max(1, want) {
			t.Fatalf("trial %d n=%d: kernel %v series %v market %v", trial, n, scalar, series[len(series)-1], want)
		}
	}
}

// A8 scenario one: at 12:14 the 11:00 close is 100, the live price 110.
func TestReview20261028Rolling60mAnchorsOnTimestamp(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 14, 0, 0, time.UTC)
	mk := func(open time.Time, c float64) market.KlineBar {
		return market.KlineBar{Time: open.UnixMilli(), Open: c, High: c, Low: c, Close: c, Volume: 1}
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 28, h, m, 0, 0, time.UTC) }
	data := func(klines ...market.KlineBar) *market.Data {
		return &market.Data{CurrentPrice: 110, PriceChange1h: -99, TimeframeData: map[string]*market.TimeframeSeriesData{
			"15m": {Timeframe: "15m", Klines: klines}}}
	}

	full := data(mk(at(10, 30), 90), mk(at(10, 45), 100), mk(at(11, 0), 110), mk(at(11, 15), 110),
		mk(at(11, 30), 110), mk(at(11, 45), 110), mk(at(12, 0), 110))
	if got := rolling1hChangePct(full, now); math.Abs(got-10) > 1e-9 {
		t.Fatalf("rolling 60m = %v, want +10 (close at 11:00 is 100)", got)
	}

	// A missing middle bar must not shift the base by a bar (old bar-count
	// lookback landed on the 90 close here: +22.2%).
	gap := data(mk(at(10, 30), 90), mk(at(10, 45), 100), mk(at(11, 0), 105),
		mk(at(11, 45), 110), mk(at(12, 0), 110))
	if got := rolling1hChangePct(gap, now); math.Abs(got-10) > 1e-9 {
		t.Fatalf("rolling 60m with a gap = %v, want +10", got)
	}

	// History that does not reach now-60m falls back to the vendor 1h change.
	short := data(mk(at(11, 45), 110), mk(at(12, 0), 110))
	if got := rolling1hChangePct(short, now); got != -99 {
		t.Fatalf("short history = %v, want fallback -99", got)
	}
}

// Informational: how far typical live values move (run with -v).
func TestReview20261028WarmupMagnitude(t *testing.T) {
	lastOpen := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	now := lastOpen.Add(time.Hour + time.Minute)
	oldATR := func(bars []market.Kline, period int) float64 { // pre-fix kernel.wilderATR
		n := len(bars)
		tr := make([]float64, n)
		tr[0] = bars[0].High - bars[0].Low
		for i := 1; i < n; i++ {
			pc := bars[i-1].Close
			tr[i] = math.Max(bars[i].High-bars[i].Low, math.Max(math.Abs(bars[i].High-pc), math.Abs(bars[i].Low-pc)))
		}
		a := tr[0]
		for i := 1; i < n; i++ {
			if i < period {
				a = a + (tr[i]-a)/float64(i+1)
			} else {
				a = (a*float64(period-1) + tr[i]) / float64(period)
			}
		}
		return a
	}
	var emaMax, emaSum, atrMax, atrSum float64
	flips := 0
	const trials = 200
	for seed := int64(1); seed <= trials; seed++ {
		bars := r1028Walk(200, seed, lastOpen, time.Hour)
		disp := bars[140:]
		oldEMA := market.ExportCalculateEMA(barsToKlines(disp), 50)
		newEMA := market.ExportCalculateEMA(barsToKlines(bars), 50)
		d := math.Abs(newEMA/oldEMA-1) * 100
		emaSum += d
		emaMax = math.Max(emaMax, d)
		oldA := oldATR(barsToKlines(disp), 14) / disp[59].Close * 100
		newA := wilderATR(barsToKlines(bars), 14) / bars[199].Close * 100
		da := math.Abs(newA/oldA-1) * 100
		atrSum += da
		atrMax = math.Max(atrMax, da)
		old, _ := computeTFSignal("1h", &market.TimeframeSeriesData{Timeframe: "1h", Klines: disp}, now, 100)
		cur, _ := computeTFSignal("1h", r1028Series("1h", bars, 60), now, 100)
		if old.Trend != cur.Trend {
			flips++
		}
	}
	t.Logf("EMA50 old(display) vs new(full): mean |diff| %.3f%%, max %.3f%%", emaSum/trials, emaMax)
	t.Logf("ATR%% old vs new:                mean |diff| %.3f%%, max %.3f%%", atrSum/trials, atrMax)
	t.Logf("trend label differs in %d / %d random walks (count=60)", flips, trials)
}
