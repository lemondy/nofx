package breakout

import (
	"math"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"nofx/market"
)

func TestReview20261028RemRSI(t *testing.T) {
	for _, tc := range []struct {
		name       string
		step, want float64
	}{{"flat", 0, 50}, {"rising", 1, 100}, {"falling", -1, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			c := make([]float64, 60)
			for i := range c {
				c[i] = 100 + tc.step*float64(i)
			}
			series := rsiSeries(c, 14)
			for i := 14; i < len(c); i++ {
				if series[i] != tc.want || rsiLast(c[:i+1], 14) != tc.want {
					t.Fatalf("bar %d: scalar=%v series=%v want=%v", i, rsiLast(c[:i+1], 14), series[i], tc.want)
				}
			}
		})
	}
	c := make([]float64, 60)
	for i := range c {
		c[i] = 100 + math.Sin(float64(i))
	}
	series := rsiSeries(c, 14)
	for i := 14; i < len(c); i++ {
		if math.Abs(series[i]-rsiLast(c[:i+1], 14)) > 1e-12 {
			t.Fatal("scalar/series disagree on mixed returns")
		}
	}
	if rsiLast(nil, 14) != 50 || rsiSeries(nil, 14) != nil || rsiLast(c, 0) != 50 || rsiSeries(c, 0) != nil {
		t.Fatal("invalid/warmup RSI")
	}
}

func TestReview20261028RemATRWrapper(t *testing.T) {
	h, l, c := []float64{11, 13, 16, 21, 20}, []float64{9, 11, 14, 18, 17}, []float64{10, 12, 15, 20, 18}
	got := atr(h, l, c, 3)
	shared := market.WilderATRSeries(h, l, c, 3)
	if !reflect.DeepEqual(got[3:], shared[3:]) || got[0] != 2 || math.Abs(got[2]-3) > 1e-12 || math.Abs(got[3]-13.0/3) > 1e-12 || math.Abs(got[4]-35.0/9) > 1e-12 {
		t.Fatalf("ATR wrapper: %v", got)
	}
}

func TestReview20261028RemShortEntryUsesMinutePrice(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	hourly := make([]Kline, 84)
	for i := range hourly {
		hourly[i] = Kline{OpenTime: now.Add(time.Duration(i-84) * 4 * time.Hour).UnixMilli(), Open: 100, High: 101, Low: 99, Close: 100, QuoteVolume: 1000}
	}
	m := &mockDS{series: map[string][]Kline{"1h": hourly, "4h": hourly, "1m": {
		{OpenTime: now.Add(-time.Minute).UnixMilli(), Close: 110},
		{OpenTime: now.UnixMilli(), Close: 999}, // forming; cannot supply entry
	}}}
	sig, err := AnalyzeShort("ENTRYUSDT", 5, nil, m)
	if err != nil {
		t.Fatal(err)
	}
	// AnalyzeShort keeps the closed-1h display price and carries no observation;
	// ScanShorts attaches the ticker (or closed 1m fallback) used for labels.
	if sig.Price != 100 || sig.PriceObservedAt != 0 || sig.PriceSource != "" {
		t.Fatalf("entry: %+v", sig)
	}
	if p := latestClosedMinutePrice(m, now); p.Price != 110 || p.ObservedAt != now.UnixMilli() || p.Source != "closed_1m" {
		t.Fatalf("closed 1m fallback: %+v", p)
	}
	m.series["1m"][0].Close = 120
	other, err := AnalyzeShort("ENTRYUSDT", 5, nil, m)
	if err != nil || other.Score != sig.Score || other.Components != sig.Components {
		t.Fatalf("entry contaminated indicators: %v %+v", err, other)
	}
	if p := tickerPrice(110, now.Add(time.Minute).UnixMilli(), now); p.valid() {
		t.Fatal("future ticker accepted")
	}
}

func TestReview20261028RemTickerObservation(t *testing.T) {
	now := time.Now().UTC().Add(-time.Second).UnixMilli()
	trendMockHTTP(t, func(r *http.Request) (*http.Response, error) {
		return trendJSONResponse([]map[string]interface{}{{"symbol": "ENTRYUSDT", "lastPrice": "110", "priceChangePercent": "10", "quoteVolume": "20000000", "closeTime": now}})
	})
	board, _, index, err := gainerTickerSnapshot(50)
	if err != nil || len(board) != 1 || index["ENTRYUSDT"].Price != 110 || index["ENTRYUSDT"].PriceObservedAt != now {
		t.Fatalf("ticker entry: %v %+v", err, index)
	}
}

func TestReview20261028RemShortSampleVersionAndOutcome(t *testing.T) {
	old := shortTuningPath
	shortTuningPath = filepath.Join(t.TempDir(), "signals.jsonl")
	t.Cleanup(func() { shortTuningPath = old })
	now := time.Date(2026, 10, 20, 12, 45, 0, 0, time.UTC)
	observed := now.Add(-90 * time.Second).UnixMilli()
	legacy := shortSample{TS: now.UnixMilli(), Symbol: "LEGACYUSDT", Price: 100, LabelVersion: 2, Evaluated: true, Outcome: -10.2}
	if err := writeSamples([]shortSample{legacy}); err != nil {
		t.Fatal(err)
	}
	// Different cohort hour keeps the existing sampling throttle out of this assertion.
	legacy.TS = now.Add(-time.Hour).UnixMilli()
	if err := writeSamples([]shortSample{legacy}); err != nil {
		t.Fatal(err)
	}
	SampleShortSignals([]ShortSignal{{Symbol: "ENTRYUSDT", Price: 110, PriceSource: "ticker_24h_last", PriceObservedAt: observed}}, now)
	rows := readSamples()
	if len(rows) != 2 || rows[1].LabelVersion != 3 || rows[1].PriceObservedAt != observed || rows[1].PriceSource != "ticker_24h_last" {
		t.Fatalf("sample metadata: %+v", rows)
	}
	var exitCalls int
	trendMockHTTP(t, func(r *http.Request) (*http.Response, error) {
		start, _ := strconv.ParseInt(r.URL.Query().Get("startTime"), 10, 64)
		if r.URL.Path == "/fapi/v1/fundingRate" {
			if start != observed+1 {
				t.Errorf("funding starts at %d want %d", start, observed+1)
			}
			return trendJSONResponse([]interface{}{})
		}
		exitCalls++
		wantOpen := time.UnixMilli(observed).Add(24 * time.Hour).Truncate(time.Minute).Add(-time.Minute).UnixMilli()
		if start != wantOpen {
			t.Errorf("exit anchored at cohort time: %d want %d", start, wantOpen)
		}
		return trendJSONResponse([][]interface{}{{start, "110", "111", "109", "110", "0", start + 59999}})
	})
	RunShortTuner(now.Add(25 * time.Hour))
	rows = readSamples()
	if exitCalls != 1 || rows[0].LabelVersion != 2 || rows[0].Outcome != -10.2 || !rows[1].Evaluated || rows[1].LabelVersion != 3 || math.Abs(rows[1].Outcome+btCostRoundTrip) > 1e-12 {
		t.Fatalf("legacy/current labels mixed or wrong entry: %+v", rows)
	}
}

func TestReview20261028RemWeightVersions(t *testing.T) {
	samples := reviewRankICSamples(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 40, 10)
	for i := range samples {
		samples[i].LabelVersion = 3
	}
	base := DefaultShortWeights()
	if _, _, ok := updateShortWeightsWithDiagnostics(samples, base, shortTunerEta); !ok {
		t.Fatal("homogeneous v3 cohort rejected")
	}
	samples[0].LabelVersion = 2
	got, _, ok := updateShortWeightsWithDiagnostics(samples, base, shortTunerEta)
	if ok || !reflect.DeepEqual(got, base) {
		t.Fatal("mixed entry versions changed weights")
	}
}

func TestReview20261028RemTrendEntryAndControls(t *testing.T) {
	trendJournalFixture(t)
	now := time.Now().UTC().Truncate(time.Minute)
	h1, h4 := trendFixture(now, time.Hour, -.1, .7), trendFixture(now, 4*time.Hour, -.2, 0)
	ticker := boardTicker{Symbol: "ENTRYUSDT", LastPrice: "110", CloseTime: now.UnixMilli(), QuoteVolume: "20000000", PriceChangePercent: "-3"}
	p, ok := qualifyTrendShort(ticker, h1, h4)
	if !ok || p.Price != 110 || p.PriceObservedAt != now.UnixMilli() {
		t.Fatalf("trend entry: %+v", p)
	}
	ticker.LastPrice = "120"
	q, ok := qualifyTrendShort(ticker, h1, h4)
	if !ok || q.Score != p.Score || q.RSI1h != p.RSI1h || q.StopPrice != p.StopPrice || q.ATR1hPct != p.ATR1hPct {
		t.Fatal("live price contaminated closed-bar indicators")
	}
	entry := PriceObservation{110, "ticker_24h_last", now.UnixMilli()}
	scan := trendShortScan{picks: []TrendShortPick{p}, prices: map[string]float64{p.Symbol: 110}, observations: map[string]PriceObservation{p.Symbol: entry}}
	if err := appendTrendShadow(scan, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadTrendShortShadow()
	if err != nil || len(rows) != 2 || rows[0].LabelVersion != 3 || rows[1].LabelVersion != 3 || rows[1].PriceObservations[p.Symbol] != entry {
		t.Fatalf("trend/control metadata: %v %+v", err, rows)
	}
	legacy := TrendShortShadowRow{Kind: "pick", TS: now.Add(-time.Hour).UnixMilli(), TrendShortPick: TrendShortPick{Symbol: "LEGACYUSDT", Price: 100}}
	if err := writeTrendShadow(append(rows, legacy)); err != nil {
		t.Fatal(err)
	}
	current, err := ReadTrendShortShadow()
	if err != nil || len(current) != 2 {
		t.Fatalf("statistics mixed label versions: %v %+v", err, current)
	}
	old, err := ReadTrendShortShadowVersion(0)
	if err != nil || len(old) != 1 || old[0].Symbol != "LEGACYUSDT" {
		t.Fatalf("legacy cohort no longer readable: %v %+v", err, old)
	}
}

func TestReview20261028RemBacktestNextOpen(t *testing.T) {
	bars := make([]Kline, 97)
	for i := range bars {
		bars[i] = Kline{OpenTime: int64(i) * (15 * time.Minute).Milliseconds(), Open: 110, Close: 121}
	}
	bars[0].Close = 100
	if entry, ok := nextOpenEntry(bars, 0); !ok || entry != 110 {
		t.Fatal("signal close used as fill")
	}
	for _, n := range []int{btForwardBars1h, btForwardBars4h, btForwardBars24h} {
		for _, dir := range []string{DirUp, DirDown} {
			want := 10.0
			if dir == DirDown {
				want = -10
			}
			if got := forwardReturn(bars, 0, n, dir); math.Abs(got-want) > 1e-12 {
				t.Fatalf("%s %d bars: %v want %v", dir, n, got, want)
			}
		}
	}
	if _, ok := nextOpenEntry(bars, len(bars)-1); ok {
		t.Fatal("entry without a next bar")
	}
	bars[1].OpenTime++
	if _, ok := nextOpenEntry(bars, 0); ok {
		t.Fatal("missing next candle filled across a gap")
	}
}

func TestReview20261028RemTrendPathObservationBoundary(t *testing.T) {
	entryAt := time.Date(2026, 10, 20, 12, 3, 20, 123000000, time.UTC)
	firstMinute := entryAt.Truncate(time.Minute).Add(time.Minute).UnixMilli()
	lastMinute := entryAt.Add(24 * time.Hour).Truncate(time.Minute).UnixMilli()
	var calls int
	trendMockHTTP(t, func(r *http.Request) (*http.Response, error) {
		calls++
		start, _ := strconv.ParseInt(r.URL.Query().Get("startTime"), 10, 64)
		end, _ := strconv.ParseInt(r.URL.Query().Get("endTime"), 10, 64)
		if start < firstMinute || end >= lastMinute {
			t.Errorf("path includes a boundary fragment outside observation: %d..%d", start, end)
		}
		step := time.Minute.Milliseconds()
		if r.URL.Query().Get("interval") == "15m" {
			step *= 15
		}
		var rows [][]interface{}
		for at := start; at <= end; at += step {
			rows = append(rows, []interface{}{at, 100, 102, 96, 99, 100, at + step - 1})
		}
		return trendJSONResponse(rows)
	})
	out, r, _, _, err := historicalTrendPath(entryAt.UnixMilli(), "ENTRYUSDT", 100, 105)
	if err != nil || calls != 3 || out != "timeout" || math.Abs(r-.2) > 1e-12 {
		t.Fatalf("millisecond observation path: %s %v calls=%d err=%v", out, r, calls, err)
	}
}

func TestReview20261028RemATRShortHistoryKeepsScale(t *testing.T) {
	h := []float64{11, 12, 13}
	l := []float64{9, 10, 11}
	c := []float64{10, 11, 12}
	got := atr(h, l, c, 14)
	if len(got) != 3 || got[2] <= 0 {
		t.Fatalf("short history ATR = %v, want positive expanding mean", got)
	}
	long := make([]float64, 40)
	hh, ll := make([]float64, 40), make([]float64, 40)
	for i := range long {
		long[i] = 100 + float64(i%5)
		hh[i], ll[i] = long[i]+1, long[i]-1
	}
	want := market.WilderATRSeries(hh, ll, long, 14)
	gotLong := atr(hh, ll, long, 14)
	for i := 14; i < 40; i++ {
		if gotLong[i] != want[i] {
			t.Fatalf("index %d: %v != shared %v", i, gotLong[i], want[i])
		}
	}
}
