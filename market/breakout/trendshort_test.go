package breakout

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"nofx/market"
)

func trendFixture(now time.Time, interval time.Duration, slope, amplitude float64) []Kline {
	var bars []Kline
	for i := 0; i < 200; i++ {
		p := 200 + slope*float64(i) + amplitude*math.Sin(float64(i)*2*math.Pi/10)
		bars = append(bars, Kline{OpenTime: now.Add(time.Duration(i-200) * interval).UnixMilli(), Open: p, High: p + .6, Low: p - .6, Close: p})
	}
	return bars
}
func TestTrendShortQualification(t *testing.T) {
	now := time.Now().Truncate(time.Hour)
	down := trendFixture(now, time.Hour, -.1, .7)
	four := trendFixture(now, 4*time.Hour, -.2, 0)
	ticker := boardTicker{Symbol: "MONUSDT", QuoteVolume: "20000000", PriceChangePercent: "-3"}
	p, ok := qualifyTrendShort(ticker, down, four)
	if !ok {
		t.Fatalf("steady downtrend rejected: %+v", p)
	}
	if p.ADX1h < 20 || p.RSI1h < 25 || p.StopPrice <= p.Price {
		t.Fatalf("bad indicators: %+v", p)
	}
	hs, ls := []float64{}, []float64{}
	closes := []float64{}
	for _, b := range down {
		closes = append(closes, b.Close)
		hs = append(hs, b.High)
		ls = append(ls, b.Low)
	}
	atrSeries := atr(hs, ls, closes, 14)
	stop := p.Price + 1.5*atrSeries[len(atrSeries)-1]
	for _, b := range down[len(down)-6:] {
		stop = math.Max(stop, b.High)
	}
	if p.StopPrice != stop {
		t.Fatalf("stop %v want %v", p.StopPrice, stop)
	}

	want := math.Min(p.ADX1h, 50) + 10*math.Min((p.EMA50_4h-p.EMA20_4h)/1.2, 3) + 6
	if math.Abs(p.Score-want) > 1e-6 {
		t.Fatalf("score %.8f want %.8f", p.Score, want)
	}
	tests := []struct {
		name   string
		h1, h4 []Kline
		ticker boardTicker
	}{
		{"uptrend", trendFixture(now, time.Hour, .1, .7), trendFixture(now, 4*time.Hour, .2, 0), ticker},
		{"flat", trendFixture(now, time.Hour, 0, 0), trendFixture(now, 4*time.Hour, 0, 0), ticker},
		{"rsi_flush", trendFixture(now, time.Hour, -.2, 0), four, ticker},
		{"illiquid", down, four, boardTicker{Symbol: "LOWUSDT", QuoteVolume: "19999999", PriceChangePercent: "-5"}},
		{"no_momentum", down, four, boardTicker{Symbol: "SLOWUSDT", QuoteVolume: "30000000", PriceChangePercent: "-2.99"}},
	}
	flush := append([]Kline(nil), down...)
	// A mature directional decline with enough RSI rebounds, but very narrow TR.
	for i := range flush {
		flush[i].High = flush[i].Close + .001
		flush[i].Low = flush[i].Close - .001
	}
	c, h, l := []float64{}, []float64{}, []float64{}
	for _, b := range flush {
		c = append(c, b.Close)
		h = append(h, b.High)
		l = append(l, b.Low)
	}
	a := atr(h, l, c, 14)
	e := ema(c, 20)
	if rsiLast(c, 14) < 25 || e[len(e)-1]-c[len(c)-1] <= 3*a[len(a)-1] {
		t.Fatal("ATR flush fixture must isolate extension gate")
	}
	tests = append(tests, struct {
		name   string
		h1, h4 []Kline
		ticker boardTicker
	}{"atr_flush", flush, four, ticker})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if p, ok := qualifyTrendShort(tt.ticker, tt.h1, tt.h4); ok {
				t.Fatalf("qualified: %+v", p)
			}
		})
	}
}
func TestTrendShortWilderADX(t *testing.T) {
	now := time.Now()
	if got := adxLast(trendFixture(now, time.Hour, -.2, 0), 14); math.Abs(got-100) > 1e-9 {
		t.Fatalf("directional ADX=%v", got)
	}
	if got := adxLast(trendFixture(now, time.Hour, 0, 0), 14); got != 0 {
		t.Fatalf("flat ADX=%v", got)
	}
	if got := adxLast(make([]Kline, 20), 14); got != 0 {
		t.Fatalf("unseeded ADX=%v", got)
	}
}
func trendMockHTTP(t *testing.T, fn func(*http.Request) (*http.Response, error)) {
	t.Helper()
	old := binanceHTTP
	binanceHTTP = &http.Client{Transport: fix06Transport(fn)}
	t.Cleanup(func() { binanceHTTP = old })
	t.Setenv("BINANCE_FAPI_BASE", "https://trend-shadow.test")
}
func trendJSONResponse(v interface{}) (*http.Response, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return fix06Response(string(b))
}
func trendJournalFixture(t *testing.T) string {
	t.Helper()
	old := trendShadowPath
	p := filepath.Join(t.TempDir(), "shadow.jsonl")
	SetTrendShortShadowPath(p)
	t.Cleanup(func() { SetTrendShortShadowPath(old) })
	return p
}
func TestTrendShortUniverseRankingAndDedupe(t *testing.T) {
	path := trendJournalFixture(t)
	market.SetEquityClassificationForTesting(map[string]bool{"S00USDT": true}, map[string]bool{})
	t.Cleanup(func() { market.SetEquityClassificationForTesting(nil, nil) })
	now := time.Now().UTC().Truncate(time.Hour)
	var tickerCalls, klineCalls atomic.Int32
	trendMockHTTP(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/fapi/v1/ticker/24hr" {
			tickerCalls.Add(1)
			var tickers []boardTicker
			for i := 0; i < 70; i++ {
				ticker := boardTicker{Symbol: fmt.Sprintf("S%02dUSDT", i), QuoteVolume: fmt.Sprint(100_000_000 - i*1_000_000), PriceChangePercent: fmt.Sprint(-3 - float64(i)/10)}
				if i == 1 {
					ticker.QuoteVolume = "15000000"
					ticker.PriceChangePercent = "-50"
				}
				tickers = append(tickers, ticker)
			}
			tickers = append(tickers, boardTicker{Symbol: "BADUSDT_261225", QuoteVolume: "1000000000"}, boardTicker{Symbol: "BTCEUR", QuoteVolume: "1000000000"})
			return trendJSONResponse(tickers)
		}
		klineCalls.Add(1)
		if r.URL.Query().Get("symbol") == "S02USDT" {
			return fix06Response("[]")
		}
		interval := time.Hour
		if r.URL.Query().Get("interval") == "4h" {
			interval = 4 * time.Hour
		}
		if r.URL.Query().Get("limit") != "201" {
			t.Errorf("warmup limit=%s", r.URL.Query().Get("limit"))
		}
		bars := trendFixture(now, interval, -.1, .7)
		var raw [][]interface{}
		for _, b := range bars {
			raw = append(raw, []interface{}{b.OpenTime, b.Open, b.High, b.Low, b.Close, 100, b.OpenTime + interval.Milliseconds() - 1, 100, 10, 10, 10})
		}
		// Forming candle must never contribute to the control entry or indicators.
		raw = append(raw, []interface{}{now.UnixMilli(), 9999, 9999, 9999, 9999, 100, now.Add(interval).UnixMilli() - 1, 100, 10, 10, 10})
		return trendJSONResponse(raw)
	})
	scan, err := scanTrendShorts(now)
	if err != nil {
		t.Fatal(err)
	}
	if tickerCalls.Load() != 1 || klineCalls.Load() != 138 || scan.universe != 69 || scan.failures != 1 || len(scan.prices) != 68 || len(scan.picks) != 10 {
		t.Fatalf("scan=%+v tickers=%d klines=%d", scan, tickerCalls.Load(), klineCalls.Load())
	}
	if _, ok := scan.prices["S00USDT"]; ok {
		t.Fatal("stock scanned")
	}
	if _, ok := scan.prices["S01USDT"]; !ok {
		t.Fatal("non-qualifying liquid-board symbol absent from controls")
	}
	for i, p := range scan.picks {
		if p.Symbol == "S01USDT" {
			t.Fatal("illiquid symbol selected")
		}
		if i > 0 && scan.picks[i-1].Score < p.Score {
			t.Fatal("ranking")
		}
		if p.Price > 200 {
			t.Fatal("forming bar used")
		}
	}
	if scan.picks[0].Symbol != "S69USDT" {
		t.Fatalf("first=%s", scan.picks[0].Symbol)
	}
	if err := appendTrendShadow(scan, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// Reset path as on a fresh process; there is no in-memory sampling cursor.
	SetTrendShortShadowPath(path)
	RunTrendShortShadow(now.Add(35 * time.Minute))
	rows, err := ReadTrendShortShadow()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 11 || tickerCalls.Load() != 1 {
		t.Fatalf("dedupe rows=%d ticker calls=%d", len(rows), tickerCalls.Load())
	}
	empty := trendShortScan{prices: map[string]float64{}}
	if err := appendTrendShadow(empty, now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := appendTrendShadow(empty, now.Add(time.Hour+30*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	rows, _ = ReadTrendShortShadow()
	if len(rows) != 12 {
		t.Fatalf("empty cohort dedupe %d", len(rows))
	}
}

func TestTrendShortPathOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name string
		bars []Kline
		out  string
		r    float64
	}{
		{"tp", []Kline{{High: 102, Low: 89, Close: 95}, {High: 106, Low: 94, Close: 100}}, "tp_first", 2},
		{"sl", []Kline{{High: 106, Low: 99, Close: 100}, {High: 101, Low: 89, Close: 95}}, "sl_first", -1},
		{"both", []Kline{{High: 106, Low: 89, Close: 100}}, "sl_first", -1},
		{"timeout", []Kline{{High: 102, Low: 94, Close: 96}}, "timeout", .8},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, r, mfe, mae, err := trendPath(tt.bars, 100, 105)
			if err != nil || out != tt.out || math.Abs(r-tt.r) > 1e-9 {
				t.Fatalf("%s %v %v", out, r, err)
			}
			if mfe <= 0 || mae <= 0 {
				t.Fatalf("excursions %v %v", mfe, mae)
			}
		})
	}
}

func TestTrendShortLabelsFundingControlAndMerge(t *testing.T) {
	trendJournalFixture(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-25 * time.Hour)
	pick := TrendShortPick{Symbol: "MONUSDT", Price: 100, StopPrice: 105}
	if err := appendTrendShadow(trendShortScan{picks: []TrendShortPick{pick}, prices: map[string]float64{"MONUSDT": 100, "OTHERUSDT": 100}}, base.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var appended atomic.Bool
	trendMockHTTP(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		// Concurrent append during unlocked network phase must survive the merge.
		if appended.CompareAndSwap(false, true) {
			if err := appendTrendShadow(trendShortScan{prices: map[string]float64{}}, base.Add(25*time.Hour).UnixMilli()); err != nil {
				t.Error(err)
			}
		}
		start, _ := strconv.ParseInt(r.URL.Query().Get("startTime"), 10, 64)
		if r.URL.Path == "/fapi/v1/fundingRate" {
			return trendJSONResponse([]map[string]interface{}{
				{"fundingTime": base.Add(2 * time.Hour).UnixMilli(), "fundingRate": "0.001"},
				{"fundingTime": base.Add(8 * time.Hour).UnixMilli(), "fundingRate": "-0.002"},
				{"fundingTime": base.UnixMilli(), "fundingRate": "99"},
			})
		}
		if r.URL.Query().Get("interval") == "15m" {
			var raw [][]interface{}
			for i := 0; i < 96; i++ {
				at := start + int64(i)*15*time.Minute.Milliseconds()
				raw = append(raw, []interface{}{at, 100, 102, 89, 95, 100, at + 15*time.Minute.Milliseconds() - 1})
			}
			return trendJSONResponse(raw)
		}
		p := 90.0
		if start < base.Add(5*time.Hour).UnixMilli() {
			p = 95
		}
		if r.URL.Query().Get("symbol") == "OTHERUSDT" {
			p = 100
		}
		return trendJSONResponse([][]interface{}{{start, 100, 110, 80, p, 100, start + 59999}})
	})
	if err := labelTrendShadow(base.Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadTrendShortShadow()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("concurrent append lost: %d", len(rows))
	}
	l := rows[0].Labels
	if math.Abs(l.H4.NetPct-4.9) > 1e-9 || math.Abs(l.H24.NetPct-9.7) > 1e-9 || l.H24.Path != "tp_first" || l.H24.R != 2 || l.H24.MFER != 2.2 || l.H24.MAER != .4 {
		t.Fatalf("labels %+v", l)
	}
	if l.H4.FundingPct != .1 || math.Abs(l.H24.FundingPct+.1) > 1e-9 || l.H4.CostPct != btCostRoundTrip {
		t.Fatalf("signed funding/cost %+v", l)
	}
	control := rows[1].ControlLabels
	mean4 := (control["MONUSDT"].H4.NetPct + control["OTHERUSDT"].H4.NetPct) / 2
	mean24 := (control["MONUSDT"].H24.NetPct + control["OTHERUSDT"].H24.NetPct) / 2
	if math.Abs(mean4-2.4) > 1e-9 || math.Abs(mean24-4.7) > 1e-9 {
		t.Fatalf("control means %v %v", mean4, mean24)
	}
	if calls.Load() != 9 {
		t.Fatalf("duplicate pick/control HTTP: %d", calls.Load())
	}
	if err := labelTrendShadow(base.Add(26 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 9 {
		t.Fatal("labelled rows refetched")
	}
}
func TestTrendShortRetryBackoffPrune(t *testing.T) {
	path := trendJournalFixture(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-25 * time.Hour)
	scan := trendShortScan{picks: []TrendShortPick{{Symbol: "GONEUSDT", Price: 100, StopPrice: 105}}, prices: map[string]float64{"GONEUSDT": 100}}
	if err := appendTrendShadow(scan, base.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := appendTrendShadow(scan, base.Add(-61*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	trendMockHTTP(t, func(r *http.Request) (*http.Response, error) { calls.Add(1); return fix06Response("[]") })
	now := base.Add(25 * time.Hour)
	if err := labelTrendShadow(now); err != nil {
		t.Fatal(err)
	}
	if err := labelTrendShadow(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("backoff fetches %d", calls.Load())
	}
	for i := 1; i < trendShortMaxAttempts; i++ {
		now = now.Add(17 * time.Hour)
		if err := labelTrendShadow(now); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := ReadTrendShortShadow()
	if len(rows) != 2 || !rows[0].Labels.H24.Unpriceable || !rows[0].Labels.H24.Done || rows[0].Labels.H24.Attempts != 8 {
		t.Fatalf("terminal/prune %+v", rows)
	}
	before := calls.Load()
	if err := labelTrendShadow(now.Add(17 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before {
		t.Fatal("terminal labels retried")
	}
	// Corrupt journals must never silently reset the durable dedupe history.
	if err := os.WriteFile(path, []byte("invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTrendShortShadow(); err == nil {
		t.Fatal("corrupt journal accepted")
	}
}

func TestTrendShortPathHistoricalBoundaries(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Hour).Add(-25*time.Hour + 7*time.Minute).UnixMilli()
	var calls atomic.Int32
	trendMockHTTP(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		start, _ := strconv.ParseInt(req.URL.Query().Get("startTime"), 10, 64)
		end, _ := strconv.ParseInt(req.URL.Query().Get("endTime"), 10, 64)
		step := time.Minute.Milliseconds()
		if req.URL.Query().Get("interval") == "15m" {
			step = 15 * time.Minute.Milliseconds()
		}
		if start < base || end >= base+24*time.Hour.Milliseconds() {
			t.Errorf("path includes pre/post observation candles: %d..%d", start, end)
		}
		var raw [][]interface{}
		for open := start; open <= end; open += step {
			raw = append(raw, []interface{}{open, 100, 102, 94, 96, 100, open + step - 1})
		}
		return trendJSONResponse(raw)
	})
	out, r, mfe, mae, err := historicalTrendPath(base, "MONUSDT", 100, 105)
	if err != nil || out != "timeout" || r != .8 || mfe != 1.2 || mae != .4 || calls.Load() != 3 {
		t.Fatalf("boundary path %s %v %v %v %v calls=%d", out, r, mfe, mae, err, calls.Load())
	}
}
