// Binance-backed market-wide data for the strategy engine: OI rankings, price
// rankings, and per-symbol quant snapshots built from official Binance Futures
// endpoints. These replaced the vergex/nofxos sources for the datasets Binance
// can provide directly — no browser relay or third-party key required.
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"nofx/logger"
	"nofx/market"
	"nofx/market/bnweight"
	"nofx/market/pubcache"
	"nofx/provider/nofxos"
	"nofx/provider/openbb"
	"nofx/security"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	binanceFAPIBase    = "https://fapi.binance.com"
	binanceFetchLimit  = 150 // symbols deep the rankings scan (top by quote volume)
	rankingCacheTTL    = 5 * time.Minute
	quantSnapshotTTL   = 60 * time.Second
	rankingConcurrency = 8
)

// ── Binance fapi plumbing ────────────────────────────────────────────────────

type binance24hrTicker struct {
	Symbol             string `json:"symbol"`
	LastPrice          float64
	PriceChangePercent float64
	QuoteVolume        float64
}

func (t *binance24hrTicker) UnmarshalJSON(b []byte) error {
	var raw struct {
		Symbol             string `json:"symbol"`
		LastPrice          string `json:"lastPrice"`
		PriceChangePercent string `json:"priceChangePercent"`
		QuoteVolume        string `json:"quoteVolume"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	t.Symbol = raw.Symbol
	t.LastPrice, _ = strconv.ParseFloat(raw.LastPrice, 64)
	t.PriceChangePercent, _ = strconv.ParseFloat(raw.PriceChangePercent, 64)
	t.QuoteVolume, _ = strconv.ParseFloat(raw.QuoteVolume, 64)
	return nil
}

// binanceGetFn is the single injectable seam over binanceGet — tests swap it
// to serve canned responses without touching the network.
var binanceGetFn = binanceGet

func binanceGet(ctx context.Context, path string, out interface{}) error {
	// Same SSRF-validated, proxy-aware client as security.SafeGet, with the
	// shared Binance weight meter (Normal: strategy-engine market data) so the
	// guard can hold it under load and honour a 429/418 cooldown.
	rawURL := binanceFAPIBase + path
	if err := security.ValidateURL(rawURL); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	client := security.SafeHTTPClient(15 * time.Second)
	client.Transport = pubcache.Wrap(bnweight.Wrap(client.Transport, bnweight.Normal))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// binanceTopTickers returns USDT perpetual tickers sorted by quote volume
// (descending), capped at binanceFetchLimit. Cached 60s — every ranking reads it.
var (
	tickersMu      sync.Mutex
	tickersCache   []binance24hrTicker
	tickersFetched time.Time
)

func binanceTopTickers(ctx context.Context) ([]binance24hrTicker, error) {
	tickersMu.Lock()
	defer tickersMu.Unlock()
	if tickersCache != nil && time.Since(tickersFetched) < 60*time.Second {
		return tickersCache, nil
	}
	var all []binance24hrTicker
	if err := binanceGetFn(ctx, "/fapi/v1/ticker/24hr", &all); err != nil {
		return nil, fmt.Errorf("binance ticker/24hr failed: %w", err)
	}
	usdt := make([]binance24hrTicker, 0, len(all))
	for _, t := range all {
		if len(t.Symbol) > 4 && t.Symbol[len(t.Symbol)-4:] == "USDT" && t.QuoteVolume > 0 {
			usdt = append(usdt, t)
		}
	}
	sort.Slice(usdt, func(i, j int) bool { return usdt[i].QuoteVolume > usdt[j].QuoteVolume })
	// Cache the FULL USDT-perp universe, volume-descending: per-symbol
	// lookups (24h quote volume, quant price patch) must also find symbols
	// outside the ranking depth, or their liquidity block silently vanishes
	// (09-18: PIEVERSEUSDT had no liquidity field at all). Ranking consumers
	// slice with binanceTickersTopN.
	tickersCache = usdt
	tickersFetched = time.Now()
	return usdt, nil
}

// binanceTickersTopN returns the deepest-N symbols by 24h quote volume for
// ranking consumers (the cache holds the full volume-sorted universe).
func binanceTickersTopN(tickers []binance24hrTicker, n int) []binance24hrTicker {
	if len(tickers) > n {
		return tickers[:n]
	}
	return tickers
}

// binanceNow is the sampling-clock seam for timestamp-based rolling returns.
var binanceNow = time.Now

// binanceRollingInterval chooses the finest interval that keeps the request
// below 100 bars (the same weight tier as the former limit=1 request).
func binanceRollingInterval(window time.Duration) string {
	for _, interval := range []string{"1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "8h", "12h", "1d"} {
		bar := market.TimeframeDuration(interval)
		if (window+bar-1)/bar+2 < 100 {
			return interval
		}
	}
	return "1d"
}

// binanceKlineChange returns a full rolling duration ending now. It never
// measures the open-to-now return of the current forming duration bar.
func binanceKlineChange(ctx context.Context, symbol, duration string) (float64, float64, error) {
	window := market.TimeframeDuration(duration)
	if window <= 0 {
		return 0, 0, fmt.Errorf("unsupported rolling duration %q", duration)
	}
	now := binanceNow()
	return binanceChangeAt(ctx, symbol, binanceRollingInterval(window), now.Add(-window), now, true)
}

// binanceRollingChange measures against NOW-barsAgo*interval, selecting the
// latest close at/before that target rather than relying on array position.
func binanceRollingChange(ctx context.Context, symbol, interval string, barsAgo int) (float64, float64, error) {
	bar := market.TimeframeDuration(interval)
	if bar <= 0 || barsAgo <= 0 {
		return 0, 0, fmt.Errorf("invalid rolling window for %s", symbol)
	}
	now := binanceNow()
	return binanceChangeAt(ctx, symbol, interval, now.Add(-time.Duration(barsAgo)*bar), now, true)
}

// binanceChangeAt uses close timestamps with one-bar tolerance. For a live
// return the last forming close is the current price; historical OI pairs
// instead use closes at/before BOTH observation times in the same request.
func binanceChangeAt(ctx context.Context, symbol, interval string, start, end time.Time, live bool) (float64, float64, error) {
	bar := market.TimeframeDuration(interval)
	if bar <= 0 || !end.After(start) {
		return 0, 0, fmt.Errorf("invalid price window for %s", symbol)
	}
	limit := int((end.Sub(start)+bar-1)/bar) + 2
	path := "/fapi/v1/klines?symbol=" + symbol + "&interval=" + interval + "&limit=" + strconv.Itoa(limit)
	if !live {
		path += "&endTime=" + strconv.FormatInt(end.UnixMilli(), 10)
	}
	var candles [][]interface{}
	if err := binanceGetFn(ctx, path, &candles); err != nil {
		return 0, 0, err
	}
	var ref, last float64
	var refAt, lastAt int64
	for _, candle := range candles {
		if len(candle) < 7 {
			continue
		}
		openMS, openOK := candle[0].(float64)
		closeMS, closeOK := candle[6].(float64)
		closeText, priceOK := candle[4].(string)
		price, err := strconv.ParseFloat(closeText, 64)
		if !openOK || !closeOK || !priceOK || err != nil || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) || openMS <= 0 || closeMS < openMS || math.IsNaN(openMS) || math.IsNaN(closeMS) || math.IsInf(openMS, 0) || math.IsInf(closeMS, 0) {
			continue
		}
		openAt, closeAt := int64(openMS), int64(closeMS)
		if closeAt <= start.UnixMilli() && closeAt > refAt {
			ref, refAt = price, closeAt
		}
		if live {
			// Ignore future bars; an old final bar must also pass the age check.
			if openAt <= end.UnixMilli() && openAt > lastAt {
				last, lastAt = price, openAt
			}
		} else if closeAt <= end.UnixMilli() && closeAt > lastAt {
			last, lastAt = price, closeAt
		}
	}
	if refAt == 0 || lastAt == 0 || start.Sub(time.UnixMilli(refAt)) > bar || end.Sub(time.UnixMilli(lastAt)) > bar {
		return 0, 0, fmt.Errorf("insufficient timestamped price history for %s", symbol)
	}
	return (last/ref - 1) * 100, last, nil
}

// binanceQuoteVolume24h returns the symbol's 24h quote turnover from the
// cached top-tickers snapshot (0 when the symbol is outside the top universe).
func binanceQuoteVolume24h(symbol string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tickers, err := binanceTopTickers(ctx)
	if err != nil {
		return 0
	}
	for _, t := range tickers {
		if t.Symbol == symbol {
			return t.QuoteVolume
		}
	}
	return 0
}

type longShortMetrics struct {
	AccountRatio *float64
	TopPosRatio  *float64
	TakerBuySell *float64
}

var (
	lsMetricsMu    sync.Mutex
	lsMetricsCache = map[string]struct {
		m  longShortMetrics
		at time.Time
	}{}
)

// binanceLongShortMetrics fetches crowd-positioning ratios (global accounts,
// top traders by position, taker buy/sell) with a 10-minute cache.
func binanceLongShortMetrics(symbol string) (longShortMetrics, error) {
	lsMetricsMu.Lock()
	if c, ok := lsMetricsCache[symbol]; ok && time.Since(c.at) < 10*time.Minute {
		m := c.m
		lsMetricsMu.Unlock()
		return m, nil
	}
	lsMetricsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var m longShortMetrics
	parseRatio := func(raw interface{}) float64 {
		if s, ok := raw.(string); ok {
			v, _ := strconv.ParseFloat(s, 64)
			return v
		}
		if f, ok := raw.(float64); ok {
			return f
		}
		return 0
	}
	fetchLatest := func(path string) (map[string]interface{}, error) {
		var rows []map[string]interface{}
		if err := binanceGetFn(ctx, path, &rows); err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("empty %s", path)
		}
		return rows[len(rows)-1], nil
	}

	if row, err := fetchLatest("/futures/data/globalLongShortAccountRatio?symbol=" + symbol + "&period=1h&limit=1"); err == nil {
		if v := parseRatio(row["longShortRatio"]); v > 0 {
			m.AccountRatio = &v
		}
	}
	if row, err := fetchLatest("/futures/data/topLongShortPositionRatio?symbol=" + symbol + "&period=1h&limit=1"); err == nil {
		if v := parseRatio(row["longShortRatio"]); v > 0 {
			m.TopPosRatio = &v
		}
	}
	if row, err := fetchLatest("/futures/data/takerlongshortRatio?symbol=" + symbol + "&period=1h&limit=1"); err == nil {
		if v := parseRatio(row["buySellRatio"]); v > 0 {
			m.TakerBuySell = &v
		}
	}

	lsMetricsMu.Lock()
	lsMetricsCache[symbol] = struct {
		m  longShortMetrics
		at time.Time
	}{m, time.Now()}
	lsMetricsMu.Unlock()
	return m, nil
}

var (
	btcClosesMu      sync.Mutex
	btcClosesCache   []float64 // 1h closes ONLY
	btcClosesFetched time.Time
	// 2026-10-03 review P0: the 4h helper originally shared this cache with
	// no interval tag — a cache hit returned 1h rows as "4h" (or vice versa)
	// depending on which helper fetched last. Separate storage per interval.
	btc4hClosesCache   []float64
	btc4hClosesFetched time.Time
)

// binanceBTC1hCloses returns the last `limit` closed 1h BTC closes (cached 5 min).
func binanceBTC1hCloses(limit int) []float64 {
	btcClosesMu.Lock()
	if len(btcClosesCache) >= limit && time.Since(btcClosesFetched) < 5*time.Minute {
		out := btcClosesCache[len(btcClosesCache)-limit:]
		btcClosesMu.Unlock()
		return out
	}
	btcClosesMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var candles [][]interface{}
	if err := binanceGetFn(ctx, "/fapi/v1/klines?symbol=BTCUSDT&interval=1h&limit="+strconv.Itoa(limit+1), &candles); err != nil {
		return nil
	}
	closes := make([]float64, 0, len(candles))
	for i := 0; i < len(candles)-1; i++ { // drop the forming candle
		if c, ok := candles[i][4].(string); ok {
			if v, err := strconv.ParseFloat(c, 64); err == nil {
				closes = append(closes, v)
			}
		}
	}
	btcClosesMu.Lock()
	btcClosesCache = closes
	btcClosesFetched = time.Now()
	btcClosesMu.Unlock()
	return closes
}

type binanceOIObservation struct {
	curBase, curValue, deltaBase, deltaValue, deltaPct float64
	start, end                                         time.Time
}

// binanceOIDelta returns current OI (in base asset and USDT value) and the
// change versus one `period` ago, from openInterestHist.
func binanceOIDelta(ctx context.Context, symbol, period string) (curBase, curValue, deltaBase, deltaValue, deltaPct float64, err error) {
	o, err := binanceOIObservedDelta(ctx, symbol, period)
	return o.curBase, o.curValue, o.deltaBase, o.deltaValue, o.deltaPct, err
}

func binanceOIObservedDelta(ctx context.Context, symbol, period string) (binanceOIObservation, error) {
	var o binanceOIObservation
	var hist []struct {
		SumOpenInterest      string `json:"sumOpenInterest"`
		SumOpenInterestValue string `json:"sumOpenInterestValue"`
		Timestamp            int64  `json:"timestamp"`
	}
	if err := binanceGetFn(ctx, "/futures/data/openInterestHist?symbol="+symbol+"&period="+period+"&limit=2", &hist); err != nil {
		return o, err
	}
	if len(hist) == 0 {
		return o, fmt.Errorf("no OI history for %s", symbol)
	}
	parse := func(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v }
	o.curBase = parse(hist[len(hist)-1].SumOpenInterest)
	o.curValue = parse(hist[len(hist)-1].SumOpenInterestValue)
	if len(hist) >= 2 {
		prevBase := parse(hist[0].SumOpenInterest)
		prevValue := parse(hist[0].SumOpenInterestValue)
		if prevBase <= 0 {
			return o, fmt.Errorf("no OI baseline (new listing?) for %s", symbol)
		}
		o.deltaBase, o.deltaValue, o.deltaPct = oiDeltas(o.curBase, prevBase, o.curValue, prevValue)
		// Dust baseline percentages are meaningless for rankings/candidates.
		if math.IsNaN(o.deltaPct) || o.deltaPct > 500 || o.deltaPct < -500 || prevValue < 500_000 {
			return o, fmt.Errorf("degenerate OI baseline for %s (prev %.0f, pct %.1f)", symbol, prevValue, o.deltaPct)
		}
		if hist[0].Timestamp > 0 && hist[len(hist)-1].Timestamp > hist[0].Timestamp {
			o.start, o.end = time.UnixMilli(hist[0].Timestamp), time.UnixMilli(hist[len(hist)-1].Timestamp)
		}
	}
	return o, nil
}

// oiDeltas computes signed OI deltas. The USD delta is the BASE-unit change
// valued at the LATEST series price — not the raw notional difference, whose
// sign can flip against the percentage when price moved hard inside the
// window (OI -10% + price +12% used to render as "+3.4M / -10.47%").
func oiDeltas(curBase, prevBase, curValue, prevValue float64) (deltaBase, deltaValue, deltaPct float64) {
	deltaBase = curBase - prevBase
	if prevBase > 0 {
		deltaPct = deltaBase / prevBase * 100
	}
	if curBase > 0 && curValue > 0 {
		deltaValue = deltaBase * (curValue / curBase) // latest implied mark price
	}
	return
}

// mapParallel runs fn over items with bounded concurrency, dropping items
// whose fn returns an error (rankings tolerate partial data).
func mapParallel[T any, R any](items []T, fn func(T) (R, error)) []R {
	in := make(chan T)
	out := make(chan R, len(items))
	var wg sync.WaitGroup
	workers := rankingConcurrency
	if len(items) < workers {
		workers = len(items)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range in {
				if r, err := fn(item); err == nil {
					out <- r
				}
			}
		}()
	}
	for _, item := range items {
		in <- item
	}
	close(in)
	wg.Wait()
	close(out)
	results := make([]R, 0, len(out))
	for r := range out {
		results = append(results, r)
	}
	return results
}

// ── OI ranking ───────────────────────────────────────────────────────────────

type oiRankingEntry struct {
	position nofxos.OIPosition
	deltaPct float64
}

var (
	oiRankMu        sync.Mutex
	oiRankCache     *nofxos.OIRankingData
	oiRankCacheKey  string
	oiRankFetchedAt time.Time
)

// binanceOIRanking builds the OI increase/decrease ranking from Binance
// openInterestHist across the top-volume universe.
func binanceOIRanking(ctx context.Context, duration string, limit int) (*nofxos.OIRankingData, error) {
	duration = strings.ToLower(strings.TrimSpace(duration))
	if duration == "" {
		duration = "1h"
	}
	if limit <= 0 {
		limit = 20
	}
	oiRankMu.Lock()
	if oiRankCache != nil && oiRankCacheKey == duration && time.Since(oiRankFetchedAt) < rankingCacheTTL {
		data := copyOIRanking(oiRankCache, limit)
		oiRankMu.Unlock()
		return data, nil
	}
	oiRankMu.Unlock()

	tickers, err := binanceTopTickers(ctx)
	if err != nil {
		return nil, err
	}
	entries := mapParallel(binanceTickersTopN(tickers, binanceFetchLimit), func(t binance24hrTicker) (oiRankingEntry, error) {
		o, err := binanceOIObservedDelta(ctx, t.Symbol, duration)
		if err != nil {
			return oiRankingEntry{}, err
		}
		if o.curBase <= 0 {
			return oiRankingEntry{}, fmt.Errorf("no current OI for %s", t.Symbol)
		}
		priceDeltaPct := 0.0
		if !o.start.IsZero() && !o.end.IsZero() {
			if chg, _, err := binanceChangeAt(ctx, t.Symbol, binanceRollingInterval(o.end.Sub(o.start)), o.start, o.end, false); err == nil {
				priceDeltaPct = chg
			}
		} else {
			// Legacy/malformed history without observation timestamps cannot be
			// aligned: this fallback price return ends NOW, while OI may lag by
			// up to one history period. Normal Binance responses take the path above.
			if chg, _, err := binanceKlineChange(ctx, t.Symbol, duration); err == nil {
				priceDeltaPct = chg
			}
		}
		return oiRankingEntry{position: nofxos.OIPosition{
			Symbol:            t.Symbol,
			Price:             t.LastPrice,
			CurrentOI:         o.curBase,
			OIDelta:           o.deltaBase,
			OIDeltaValue:      o.deltaValue,
			OIDeltaPercent:    o.deltaPct,
			PriceDeltaPercent: priceDeltaPct,
		}, deltaPct: o.deltaPct}, nil
	})
	if len(entries) == 0 {
		return nil, fmt.Errorf("binance OI ranking: no symbols resolved")
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].deltaPct > entries[j].deltaPct })
	data := &nofxos.OIRankingData{
		Duration:  duration,
		FetchedAt: time.Now(),
	}
	for i, e := range entries {
		e.position.Rank = i + 1
		data.TopPositions = append(data.TopPositions, e.position)
	}
	// Decrease ranking: walk from the most negative.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].deltaPct < entries[j].deltaPct })
	for i, e := range entries {
		e.position.Rank = i + 1
		data.LowPositions = append(data.LowPositions, e.position)
	}

	oiRankMu.Lock()
	oiRankCache = data
	oiRankCacheKey = duration
	oiRankFetchedAt = time.Now()
	oiRankMu.Unlock()
	logger.Infof("📊 Binance OI ranking (%s): top %d / low %d from %d symbols", duration, len(data.TopPositions), len(data.LowPositions), len(entries))
	return copyOIRanking(data, limit), nil
}

// copyOIRanking returns caller-owned slices; the cache always holds full depth.
func copyOIRanking(data *nofxos.OIRankingData, limit int) *nofxos.OIRankingData {
	cp := *data
	cp.TopPositions = append([]nofxos.OIPosition(nil), data.TopPositions[:min(limit, len(data.TopPositions))]...)
	cp.LowPositions = append([]nofxos.OIPosition(nil), data.LowPositions[:min(limit, len(data.LowPositions))]...)
	return &cp
}

// ── Price ranking ────────────────────────────────────────────────────────────

type priceChangeSample struct {
	symbol   string
	price    float64
	chg1h    float64
	chg4h    float64
	chg24h   float64
	quoteVol float64
}

var (
	priceRankMu       sync.Mutex
	priceRankCache    *nofxos.PriceRankingData
	priceRankCacheKey string
	priceRankFetched  time.Time
)

// binancePriceRanking caches full rankings per normalized duration set. Each
// request receives an independent map, duration objects and trimmed slices.
func binancePriceRanking(ctx context.Context, durations string, limit int) (*nofxos.PriceRankingData, error) {
	durations = normalizeRankingDurations(durations)
	if limit <= 0 {
		limit = 10
	}
	priceRankMu.Lock()
	if priceRankCache != nil && priceRankCacheKey == durations && time.Since(priceRankFetched) < rankingCacheTTL {
		data := copyPriceRanking(priceRankCache, limit)
		priceRankMu.Unlock()
		// Retry transient OI enrichment failures on the caller's copy.
		fillPriceRankingOIFromRanking(ctx, data)
		return data, nil
	}
	priceRankMu.Unlock()

	want := map[string]bool{}
	for _, d := range splitDurations(durations) {
		want[d] = true
	}
	tickers, err := binanceTopTickers(ctx)
	if err != nil {
		return nil, err
	}
	samples := mapParallel(binanceTickersTopN(tickers, binanceFetchLimit), func(t binance24hrTicker) (priceChangeSample, error) {
		s := priceChangeSample{symbol: t.Symbol, price: t.LastPrice, chg24h: t.PriceChangePercent, quoteVol: t.QuoteVolume}
		var err error
		if want["1h"] {
			if s.chg1h, _, err = binanceKlineChange(ctx, t.Symbol, "1h"); err != nil {
				return s, err
			}
		}
		if want["4h"] {
			if s.chg4h, _, err = binanceKlineChange(ctx, t.Symbol, "4h"); err != nil {
				return s, err
			}
		}
		return s, nil
	})
	if len(samples) < limit {
		return nil, fmt.Errorf("binance price ranking: only %d symbols resolved", len(samples))
	}

	data := &nofxos.PriceRankingData{Durations: map[string]*nofxos.PriceRankingDuration{}, FetchedAt: time.Now()}
	build := func(key string, get func(priceChangeSample) float64) {
		if !want[key] {
			return
		}
		sorted := append([]priceChangeSample(nil), samples...)
		sort.Slice(sorted, func(i, j int) bool { return get(sorted[i]) > get(sorted[j]) })
		dur := &nofxos.PriceRankingDuration{}
		for _, s := range sorted {
			dur.Top = append(dur.Top, nofxos.PriceRankingItem{
				Pair: s.symbol, Symbol: s.symbol, Price: s.price,
				PriceDelta: get(s) / 100, // decimal format: 0.0723 = 7.23%
			})
		}
		for i := len(sorted) - 1; i >= 0; i-- {
			s := sorted[i]
			dur.Low = append(dur.Low, nofxos.PriceRankingItem{
				Pair: s.symbol, Symbol: s.symbol, Price: s.price,
				PriceDelta: get(s) / 100,
			})
		}
		data.Durations[key] = dur
	}
	build("1h", func(s priceChangeSample) float64 { return s.chg1h })
	build("4h", func(s priceChangeSample) float64 { return s.chg4h })
	build("24h", func(s priceChangeSample) float64 { return s.chg24h })

	fillPriceRankingOIFromRanking(ctx, data)

	priceRankMu.Lock()
	priceRankCache = data
	priceRankCacheKey = durations
	priceRankFetched = time.Now()
	priceRankMu.Unlock()
	logger.Infof("📈 Binance price ranking ready for %d durations (%d symbols)", len(data.Durations), len(samples))
	return copyPriceRanking(data, limit), nil
}

func normalizeRankingDurations(durations string) string {
	seen := map[string]bool{}
	for _, d := range strings.Split(durations, ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			seen[d] = true
		}
	}
	if len(seen) == 0 {
		seen["1h"] = true
	}
	keys := make([]string, 0, len(seen))
	for d := range seen {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func copyPriceRanking(data *nofxos.PriceRankingData, limit int) *nofxos.PriceRankingData {
	cp := *data
	cp.Durations = make(map[string]*nofxos.PriceRankingDuration, len(data.Durations))
	for key, d := range data.Durations {
		if d == nil {
			continue
		}
		cp.Durations[key] = &nofxos.PriceRankingDuration{
			Top: append([]nofxos.PriceRankingItem(nil), d.Top[:min(limit, len(d.Top))]...),
			Low: append([]nofxos.PriceRankingItem(nil), d.Low[:min(limit, len(d.Low))]...),
		}
	}
	return &cp
}

// fillPriceRankingOIFromRanking patches the 1h table's OI column from the
// (cached) Binance OI ranking; 4h/24h have no OI equivalent and stay empty —
// the renderer renders per-row blanks instead of fake zeros.
func fillPriceRankingOIFromRanking(ctx context.Context, data *nofxos.PriceRankingData) {
	d1h, ok := data.Durations["1h"]
	if !ok || len(d1h.Top) == 0 {
		return
	}
	// Skip when the column already has data (cache-hit fast path).
	for _, it := range d1h.Top {
		if it.OIDeltaValue != 0 {
			return
		}
	}
	oiRank, err := binanceOIRanking(ctx, "1h", binanceFetchLimit)
	if err != nil {
		return
	}
	oiBySymbol := make(map[string]float64, len(oiRank.TopPositions)+len(oiRank.LowPositions))
	for _, pos := range oiRank.TopPositions {
		oiBySymbol[pos.Symbol] = pos.OIDeltaValue
	}
	for _, pos := range oiRank.LowPositions {
		oiBySymbol[pos.Symbol] = pos.OIDeltaValue
	}
	for i := range d1h.Top {
		if v, ok := oiBySymbol[d1h.Top[i].Symbol]; ok {
			d1h.Top[i].OIDeltaValue = v
		}
	}
	for i := range d1h.Low {
		if v, ok := oiBySymbol[d1h.Low[i].Symbol]; ok {
			d1h.Low[i].OIDeltaValue = v
		}
	}
}

func splitDurations(durations string) []string {
	var out []string
	cur := ""
	for _, r := range durations + "," {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if r != ' ' {
			cur += string(r)
		}
	}
	return out
}

// ── Per-symbol quant snapshot ────────────────────────────────────────────────

type quantSnapshot struct {
	data *QuantData
	at   time.Time
}

var (
	quantMu    sync.Mutex
	quantCache = map[string]quantSnapshot{}
)

// binanceQuantSnapshot builds the per-symbol quant block (price, price change,
// OI level and 1h delta, funding rate) from Binance endpoints. Fund flow has no
// Binance equivalent and stays empty — the AI prompt renders it as absent.
func binanceQuantSnapshot(symbol string) (*QuantData, error) {
	quantMu.Lock()
	if snap, ok := quantCache[symbol]; ok && time.Since(snap.at) < quantSnapshotTTL {
		quantMu.Unlock()
		return snap.data, nil
	}
	quantMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	data := &QuantData{Symbol: symbol}

	// Price + changes. The 1h figure is a rolling ~60-minute change from
	// 15m klines — the same definition as the signal layer's return_1h_pct —
	// so the prompt never shows two different "1h change" numbers.
	chg1h, lastPrice, err := binanceRollingChange(ctx, symbol, "15m", 4)
	if err != nil {
		return nil, err
	}
	data.Price = lastPrice
	// QuantData.PriceChange is a DECIMAL fraction (0.0723 = 7.23%) — the
	// renderer multiplies by 100. Storing percent here made every small drop
	// render as an impossible < -100% move.
	data.PriceChange = map[string]float64{"60m": chg1h / 100}
	tickers, err := binanceTopTickers(ctx)
	if err == nil {
		for _, t := range tickers {
			if t.Symbol == symbol {
				if t.LastPrice > 0 {
					data.Price = t.LastPrice
				}
				data.PriceChange["24h"] = t.PriceChangePercent / 100
				break
			}
		}
	}

	// OI: current level + 1h delta.
	curBase, _, deltaBase, deltaValue, deltaPct, err := binanceOIDelta(ctx, symbol, "1h")
	if err == nil {
		data.OI = map[string]*OIData{
			"binance": {
				CurrentOI: curBase,
				Delta: map[string]*OIDeltaData{
					"1h": {OIDelta: deltaBase, OIDeltaValue: deltaValue, OIDeltaPercent: deltaPct},
				},
			},
		}
	}

	// Liquidations: public all-market force-order stream (keyless), 24h
	// rolling window maintained by market.LiquidationStats. Cold start
	// reports nothing rather than zeros.
	if lw, ok := market.LiquidationStats(symbol); ok {
		data.Liquidation = &lw
	}

	// News enrichment via the OpenBB sidecar (user directive 2026-09-25):
	// headline feed for this base symbol; absent when the sidecar is down —
	// the enrichment layer is optional infrastructure, never a dependency.
	if openbb.Available() {
		base := strings.ToUpper(symbol)
		base = strings.TrimSuffix(strings.TrimSuffix(base, "USDT"), "USD")
		if items := openbb.NewsForSymbol(base); len(items) > 0 {
			data.News = items
		}
	}

	quantMu.Lock()
	quantCache[symbol] = quantSnapshot{data: data, at: time.Now()}
	quantMu.Unlock()
	return data, nil
}

// binanceOrderBookSpreadPct measures the live order-book spread as a percent
// of mid ((ask0 − bid0) / mid × 100, depth 5). 0 on any failure — the gate
// itself is trader-side and fails open; the snapshot value is evidence.
func binanceOrderBookSpreadPct(symbol string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var book struct {
		Bids [][2]string `json:"bids"`
		Asks [][2]string `json:"asks"`
	}
	if err := binanceGetFn(ctx, "/fapi/v1/depth?symbol="+symbol+"&limit=5", &book); err != nil {
		return 0
	}
	if len(book.Bids) == 0 || len(book.Asks) == 0 {
		return 0
	}
	bid, err1 := strconv.ParseFloat(book.Bids[0][0], 64)
	ask, err2 := strconv.ParseFloat(book.Asks[0][0], 64)
	if err1 != nil || err2 != nil || bid <= 0 || ask < bid {
		return 0
	}
	mid := (ask + bid) / 2
	if mid <= 0 {
		return 0
	}
	return (ask - bid) / mid * 100
}

// binanceBTC24hLivePct returns BTC's LIVE rolling-24h price-change percent
// from the same fapi ticker batch the coin-side PriceChange24hLivePct derives
// from — closing the closed-BTC-vs-live-coin window asymmetry in BTC_WEAK_LONG
// (review 2026-10-06 P2-5). ok=false on any failure: the caller falls back to
// the closed-bar computation. Reuses binanceTopTickers' 60s cache — no extra
// request on the hot path.
func binanceBTC24hLivePct() (pct float64, ok bool) {
	tickers, err := binanceTopTickers(context.Background())
	if err != nil {
		return 0, false
	}
	for _, t := range tickers {
		if t.Symbol == "BTCUSDT" {
			return t.PriceChangePercent, true
		}
	}
	return 0, false
}

// binanceBTC4hCloses returns the last `limit` closed 4h BTC closes (cached 5
// min, shared with the 1h variant's cache shape). The BTC long-side filter
// reads TRUE 4h bars — the same convention as the short scan's bull
// predicate — instead of phase-arbitrary 1h aggregates (2026-10-03 review).
// BTC4hTrendCloses provides the same closed-bar BTC regime input to execution.
func BTC4hTrendCloses(limit int) []float64 { return binanceBTC4hCloses(limit) }

var (
	btc4hLastAttempt time.Time
	// btc4hInflight is non-nil while one fetch is in flight. Review
	// 2026-10-06 P2-6: the fetch used to run OUTSIDE the lock with no
	// singleflight — a cold-start second caller returned nil while the first
	// request was still running, and a warm-cache expiry let N callers all
	// hit Binance. Waiters now block on this channel and re-read the cache.
	btc4hInflight chan struct{}
)

func binanceBTC4hCloses(limit int) []float64 {
	readCache := func() []float64 {
		if len(btc4hClosesCache) >= limit && time.Since(btc4hClosesFetched) < 5*time.Minute {
			out := make([]float64, limit)
			copy(out, btc4hClosesCache[len(btc4hClosesCache)-limit:])
			return out
		}
		return nil
	}
	btcClosesMu.Lock()
	if out := readCache(); out != nil {
		btcClosesMu.Unlock()
		return out
	}
	if btc4hInflight != nil {
		ch := btc4hInflight
		btcClosesMu.Unlock()
		<-ch
		btcClosesMu.Lock()
		out := readCache()
		btcClosesMu.Unlock()
		return out
	}
	if len(btc4hClosesCache) == 0 && time.Since(btc4hLastAttempt) < 30*time.Second {
		// Cold cache, recent FAILED attempt, nothing in flight — back off.
		btcClosesMu.Unlock()
		return nil
	}
	btc4hLastAttempt = time.Now()
	btc4hInflight = make(chan struct{})
	btcClosesMu.Unlock()
	defer func() {
		btcClosesMu.Lock()
		ch := btc4hInflight
		btc4hInflight = nil
		btcClosesMu.Unlock()
		if ch != nil {
			close(ch)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var candles [][]interface{}
	if err := binanceGetFn(ctx, "/fapi/v1/klines?symbol=BTCUSDT&interval=4h&limit="+strconv.Itoa(limit+1), &candles); err != nil {
		return nil
	}
	closes := make([]float64, 0, len(candles))
	for i := 0; i < len(candles)-1; i++ { // drop the forming candle
		if c, ok := candles[i][4].(string); ok {
			if v, err := strconv.ParseFloat(c, 64); err == nil {
				closes = append(closes, v)
			}
		}
	}
	btcClosesMu.Lock()
	btc4hClosesCache = closes
	btc4hClosesFetched = time.Now()
	btcClosesMu.Unlock()
	out := closes
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// CachedQuoteVolume24hUSD is for execution/pending rechecks. It NEVER
// initiates another REST request; missing or >5m snapshots fail open.
func CachedQuoteVolume24hUSD(symbol string) float64 {
	tickersMu.Lock()
	defer tickersMu.Unlock()
	if time.Since(tickersFetched) > rankingCacheTTL {
		return 0
	}
	for _, t := range tickersCache {
		if t.Symbol == market.Normalize(symbol) {
			return t.QuoteVolume
		}
	}
	return 0
}
