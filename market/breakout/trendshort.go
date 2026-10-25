package breakout

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"nofx/market"
)

// 2026-10-09 trend-continuation short source (shadow, user option A).
const (
	trendShortTopN          = 10         // Calibration: maximum hourly research cohort.
	trendShortMinVolume     = 20_000_000 // Calibration: USDT daily liquidity floor.
	trendShortMinADX        = 20.0       // Calibration: directional strength on closed 1h bars.
	trendShortMaxChange     = -3.0       // Calibration: minimum daily downside momentum, percent.
	trendShortMinRSI        = 25.0       // Calibration: avoid oversold capitulation.
	trendShortMaxExtension  = 3.0        // Calibration: maximum distance below EMA20 in ATR units.
	trendShortStopATR       = 1.5        // Calibration: minimum suggested stop distance.
	trendShortFastEMA       = 20         // Calibration: fast trend lookback on both timeframes.
	trendShortSlowEMA       = 50         // Calibration: slow trend lookback on both timeframes.
	trendShortPeriod        = 14         // Calibration: Wilder ADX, RSI and ATR lookback.
	trendShortStopBars      = 6          // Calibration: recent closed 1h stop high.
	trendShortADXCap        = 50.0       // Calibration: score saturation for strength.
	trendShortSeparationCap = 3.0        // Calibration: score saturation in 4h ATR units.
	trendShortChangeCap     = 10.0       // Calibration: score saturation for daily decline, percent.
	trendShortBars          = 200        // EMA50 warmup on each timeframe.
	trendShortWorkers       = 4          // Bound public HTTP load.
)

type TrendShortPick struct {
	Symbol      string  `json:"symbol"`
	Score       float64 `json:"score"`
	Price       float64 `json:"price"`
	ATR1hPct    float64 `json:"atr_1h_pct"`
	ADX1h       float64 `json:"adx_1h"`
	RSI1h       float64 `json:"rsi_1h"`
	Chg24hPct   float64 `json:"chg_24h_pct"`
	QuoteVol24h float64 `json:"quote_vol_24h"`
	EMA20_1h    float64 `json:"ema20_1h"`
	EMA50_1h    float64 `json:"ema50_1h"`
	EMA20_4h    float64 `json:"ema20_4h"`
	EMA50_4h    float64 `json:"ema50_4h"`
	StopPrice   float64 `json:"stop_price"`
}

type trendShortScan struct {
	picks              []TrendShortPick
	prices             map[string]float64
	universe, failures int
}

// ScanTrendShorts has no live candidate consumers; the scheduler journals it only.
func ScanTrendShorts(now time.Time) ([]TrendShortPick, error) {
	scan, err := scanTrendShorts(now)
	return scan.picks, err
}

func scanTrendShorts(now time.Time) (trendShortScan, error) {
	out := trendShortScan{prices: map[string]float64{}}
	tickers, err := fetchBoardTickers()
	if err != nil {
		return out, err
	}
	// Same selection as TopVolumeSymbols(50) and BoardLists(0,0,30), one fetch.
	symbols := topVolumeTickers(tickers, 50)
	_, _, losers := boardListsFromTickers(tickers, 20, 20, 30)
	symbols = append(symbols, losers...)
	bySymbol := map[string]boardTicker{}
	for _, t := range tickers {
		bySymbol[t.Symbol] = t
	}
	seen := map[string]bool{}
	var universe []string
	for _, sym := range symbols {
		if seen[sym] || market.IsBStockSymbol(sym) {
			continue
		}
		seen[sym] = true
		universe = append(universe, sym)
	}
	out.universe = len(universe)
	jobs := make(chan string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < trendShortWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sym := range jobs {
				ds := NewBinanceDS(sym)
				h1, e1 := ds.Klines("1h", trendShortBars+1)
				h4, e4 := ds.Klines("4h", trendShortBars+1)
				h1 = trendClosedBars(h1, now, time.Hour)
				h4 = trendClosedBars(h4, now, 4*time.Hour)
				mu.Lock()
				if e1 != nil || e4 != nil || !trendValidBars(h1) || !trendValidBars(h4) {
					out.failures++
					mu.Unlock()
					continue
				}
				out.prices[sym] = h1[len(h1)-1].Close
				mu.Unlock()
				pick, ok := qualifyTrendShort(bySymbol[sym], h1, h4)
				if ok {
					mu.Lock()
					out.picks = append(out.picks, pick)
					mu.Unlock()
				}
			}
		}()
	}
	for _, sym := range universe {
		jobs <- sym
	}
	close(jobs)
	wg.Wait()
	rankTrendShorts(&out)
	if len(universe) > 0 && len(out.prices) == 0 {
		return out, fmt.Errorf("trend short: all %d symbols failed", len(universe))
	}
	return out, nil
}

func trendClosedBars(bars []Kline, now time.Time, interval time.Duration) []Kline {
	var out []Kline
	for _, b := range bars {
		if b.OpenTime+interval.Milliseconds() <= now.UnixMilli() {
			out = append(out, b)
		}
	}
	return out
}

func trendValidBars(bars []Kline) bool {
	if len(bars) < trendShortBars {
		return false
	}
	for _, b := range bars {
		if !trendFinite(b.Close) || !trendFinite(b.High) || !trendFinite(b.Low) || b.Low <= 0 || b.Close < b.Low || b.Close > b.High {
			return false
		}
	}
	return true
}
func trendFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func rankTrendShorts(out *trendShortScan) {
	sort.Slice(out.picks, func(i, j int) bool {
		if out.picks[i].Score == out.picks[j].Score {
			return out.picks[i].Symbol < out.picks[j].Symbol
		}
		return out.picks[i].Score > out.picks[j].Score
	})
	if len(out.picks) > trendShortTopN {
		out.picks = out.picks[:trendShortTopN]
	}
}

func qualifyTrendShort(t boardTicker, h1, h4 []Kline) (TrendShortPick, bool) {
	p := TrendShortPick{Symbol: t.Symbol, QuoteVol24h: toF(t.QuoteVolume), Chg24hPct: toF(t.PriceChangePercent)}
	if !trendValidBars(h1) || !trendValidBars(h4) {
		return p, false
	}
	series := func(bars []Kline) (c, h, l []float64) {
		for _, b := range bars {
			c = append(c, b.Close)
			h = append(h, b.High)
			l = append(l, b.Low)
		}
		return
	}
	c1, h, l := series(h1)
	c4, h4s, l4 := series(h4)
	last := func(xs []float64) float64 { return xs[len(xs)-1] }
	p.Price = last(c1)
	p.EMA20_1h = last(ema(c1, trendShortFastEMA))
	p.EMA50_1h = last(ema(c1, trendShortSlowEMA))
	p.EMA20_4h = last(ema(c4, trendShortFastEMA))
	p.EMA50_4h = last(ema(c4, trendShortSlowEMA))
	a1, a4 := last(atr(h, l, c1, trendShortPeriod)), last(atr(h4s, l4, c4, trendShortPeriod))
	p.ADX1h = adxLast(h1, trendShortPeriod)
	p.RSI1h = rsiLast(c1, trendShortPeriod)
	if !trendFinite(p.QuoteVol24h) || !trendFinite(p.Chg24hPct) || p.QuoteVol24h < trendShortMinVolume || p.Chg24hPct > trendShortMaxChange ||
		p.EMA20_4h >= p.EMA50_4h || last(c4) >= p.EMA20_4h || p.EMA20_1h >= p.EMA50_1h || p.Price >= p.EMA50_1h ||
		p.ADX1h < trendShortMinADX || p.RSI1h < trendShortMinRSI || a1 <= 0 || a4 <= 0 || p.EMA20_1h-p.Price > trendShortMaxExtension*a1 {
		return p, false
	}
	p.ATR1hPct = a1 / p.Price * 100
	// Score = capped ADX (50) + 10×capped 4h EMA separation/ATR (3)
	// + 2×capped daily decline (10%). Range 0..100; higher ranks first.
	p.Score = math.Min(p.ADX1h, trendShortADXCap) + 10*math.Min((p.EMA50_4h-p.EMA20_4h)/a4, trendShortSeparationCap) + 2*math.Min(-p.Chg24hPct, trendShortChangeCap)
	p.StopPrice = p.Price + trendShortStopATR*a1
	for _, b := range h1[len(h1)-trendShortStopBars:] {
		p.StopPrice = math.Max(p.StopPrice, b.High)
	}
	return p, true
}
