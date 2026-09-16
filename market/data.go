package market

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"nofx/logger"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FundingRateCache is the funding rate cache structure
// premiumIndex.lastFundingRate is the CONTINUOUSLY DRIFTING forward estimate
// for the next settlement — only the settlement event is 8h-bound, the rate
// value itself can halve within minutes on pumping symbols. A 1-hour cache
// fed the AI prompt stale figures (observed: scanner 117% vs signal 52%).
type FundingRateCache struct {
	Rate      float64
	UpdatedAt time.Time
}

var (
	fundingRateMap sync.Map // map[string]*FundingRateCache
	frCacheTTL     = 5 * time.Minute
)

// FundingIntervalCache holds the measured settlement interval per symbol.
type FundingIntervalCache struct {
	Hours     float64
	UpdatedAt time.Time
}

var (
	fundingIntervalMap sync.Map // map[string]*FundingIntervalCache
	fundingIntervalTTL = 24 * time.Hour
)

// fundingIntervalHours returns the symbol's real funding settlement interval
// in hours, measured from recent settlement timestamps (0 = unknown).
// The interval is fixed per listing (8h default; some new listings 4h/1h)
// and changes rarely, so it is cached for a day. The measurement rides on
// the same /fapi/v1/fundingRate call that feeds the rollover history
// (funding_history.go) — one endpoint call per 5-minute cache window serves
// both.
func fundingIntervalHours(symbol string) float64 {
	if cached, ok := fundingIntervalMap.Load(symbol); ok {
		c := cached.(*FundingIntervalCache)
		if time.Since(c.UpdatedAt) < fundingIntervalTTL {
			return c.Hours
		}
	}
	_, times, ok := fundingHistory(symbol)
	if !ok || len(times) < 2 {
		return 0
	}
	var gaps []float64
	for i := 1; i < len(times); i++ {
		h := float64(times[i]-times[i-1]) / 3.6e6
		if h > 0 && h <= 24 {
			gaps = append(gaps, h)
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	sum := 0.0
	for _, g := range gaps {
		sum += g
	}
	hours := sum / float64(len(gaps))
	fundingIntervalMap.Store(symbol, &FundingIntervalCache{Hours: hours, UpdatedAt: time.Now()})
	return hours
}

// Get retrieves market data for the specified token (uses Binance data by default)
func Get(symbol string) (*Data, error) {
	return GetWithExchange(symbol, "binance")
}

// GetWithExchange retrieves market data for the specified token using exchange-specific data
func GetWithExchange(symbol, exchange string) (*Data, error) {
	var klines3m, klines4h, klines1h []Kline
	var err error
	// Normalize symbol
	symbol = Normalize(symbol)

	// Check if this is an xyz dex asset (use Hyperliquid API)
	isXyzAsset := IsXyzDexAsset(symbol)

	// For hyperliquid exchange, also use Hyperliquid API
	useHyperliquidAPI := isXyzAsset || strings.ToLower(exchange) == "hyperliquid"

	// Get 3-minute K-line data (or 5-minute for xyz assets as 3m may not be available)
	if useHyperliquidAPI {
		// Use Hyperliquid API for xyz dex assets (use 5m since 3m may not be available)
		klines3m, err = getKlinesFromHyperliquid(symbol, "5m", 100)
		if err != nil {
			return nil, fmt.Errorf("Failed to get 5-minute K-line from Hyperliquid: %v", err)
		}
	} else {
		// Use CoinAnk for regular crypto assets with exchange-specific data
		klines3m, err = getKlinesFromCoinAnk(symbol, "3m", exchange, 100)
		if err != nil {
			return nil, fmt.Errorf("Failed to get 3-minute K-line from CoinAnk (%s): %v", exchange, err)
		}
	}

	// Data staleness detection: Prevent DOGEUSDT-style price freeze issues
	if isStaleData(klines3m, symbol) {
		logger.Infof("⚠️  WARNING: %s detected stale data (consecutive price freeze), skipping symbol", symbol)
		return nil, fmt.Errorf("%s data is stale, possible cache failure", symbol)
	}

	// Get 4-hour K-line data
	if useHyperliquidAPI {
		klines4h, err = getKlinesFromHyperliquid(symbol, "4h", 100)
		if err != nil {
			return nil, fmt.Errorf("Failed to get 4-hour K-line from Hyperliquid: %v", err)
		}
	} else {
		klines4h, err = getKlinesFromCoinAnk(symbol, "4h", exchange, 100)
		if err != nil {
			return nil, fmt.Errorf("Failed to get 4-hour K-line from CoinAnk (%s): %v", exchange, err)
		}
	}

	// 1-hour K-lines (best-effort): the stop-band FLOOR yardstick is
	// ATR(1h) and the vol-target/trailing yardsticks read it too — without
	// this series those gates silently skipped (the ATR helpers return 0 on
	// missing data). On failure the floor's fallback chain rides 4h: a
	// wider but still-enforced floor.
	if useHyperliquidAPI {
		klines1h, err = getKlinesFromHyperliquid(symbol, "1h", 100)
	} else {
		klines1h, err = getKlinesFromCoinAnk(symbol, "1h", exchange, 100)
	}
	if err != nil {
		logger.Infof("⚠️ Failed to get %s 1h K-line (%v) — stop-floor ATR falls back to 4h", symbol, err)
		klines1h = nil
	}

	// Check if data is empty
	if len(klines3m) == 0 {
		return nil, fmt.Errorf("3-minute K-line data is empty")
	}
	if len(klines4h) == 0 {
		return nil, fmt.Errorf("4-hour K-line data is empty")
	}

	// Calculate current indicators (based on 3-minute latest data)
	currentPrice := klines3m[len(klines3m)-1].Close
	// Live price: the kline vendor's forming candle close does not tick in
	// real time, so query the exchange ticker for the actual current price
	// (order placement / SL-TP anchor on this value). xyz dex assets are
	// Hyperliquid-listed and have no Binance futures ticker — keep the close.
	if !isXyzAsset {
		if livePrice, err := NewAPIClient().GetCurrentPrice(symbol); err == nil && livePrice > 0 {
			currentPrice = livePrice
		}
	}
	// Patch the forming 3m candle with the live price so the intraday series
	// and current indicators aren't anchored to the vendor's frozen close.
	if len(klines3m) > 0 {
		last := &klines3m[len(klines3m)-1]
		bar := KlineBar{Time: last.OpenTime, Open: last.Open, High: last.High, Low: last.Low, Close: last.Close}
		if applyLivePrice(&bar, "3m", currentPrice) {
			last.Open, last.High, last.Low, last.Close = bar.Open, bar.High, bar.Low, bar.Close
		}
	}
	currentEMA20 := calculateEMA(klines3m, 20)
	currentMACD := calculateMACD(klines3m)
	currentRSI7 := calculateRSI(klines3m, 7)

	// Calculate price change percentage
	// 1-hour price change = price from 20 3-minute K-lines ago
	priceChange1h := 0.0
	if len(klines3m) >= 21 { // Need at least 21 K-lines (current + 20 previous)
		price1hAgo := klines3m[len(klines3m)-21].Close
		if price1hAgo > 0 {
			priceChange1h = ((currentPrice - price1hAgo) / price1hAgo) * 100
		}
	}

	// 4-hour price change = price from 1 4-hour K-line ago
	priceChange4h := 0.0
	if len(klines4h) >= 2 {
		price4hAgo := klines4h[len(klines4h)-2].Close
		if price4hAgo > 0 {
			priceChange4h = ((currentPrice - price4hAgo) / price4hAgo) * 100
		}
	}

	// Get OI data
	oiData, err := getOpenInterestData(symbol)
	if err != nil {
		// OI failure doesn't affect overall result, use default values
		oiData = &OIData{Latest: 0, Average: 0}
	}

	// Get Funding Rate + measured settlement interval + settled history
	fundingRate, fundingErr := getFundingRate(symbol)
	fundingSettleHours := fundingIntervalHours(symbol)
	fundingRates, _, histOK := fundingHistory(symbol)
	if !histOK {
		fundingRates = nil
	}

	// Calculate intraday series data
	intradayData := calculateIntradaySeries(klines3m)

	// Calculate longer-term data
	longerTermData := calculateLongerTermData(klines4h)

	// TimeframeData drives every execution-side volatility yardstick: the
	// stop-band floor (1.5×ATR(1h)) and cap (max(2×ATR(4h), 8%)) plus the
	// vol-target/trailing ATR all read it. Leaving it nil silently disabled
	// the whole family — the cap degenerated to the fixed 8% and the floor
	// never enforced (09-16 AKEUSDT: a healthy 11.45% structural stop was
	// rejected by the degenerate 8% cap the prompt never promised).
	TimeframeData := executionTimeframeData(klines3m, klines1h, klines4h)

	return &Data{
		Symbol:             symbol,
		CurrentPrice:       currentPrice,
		PriceChange1h:      priceChange1h,
		PriceChange4h:      priceChange4h,
		CurrentEMA20:       currentEMA20,
		CurrentMACD:        currentMACD,
		CurrentRSI7:        currentRSI7,
		OpenInterest:       oiData,
		FundingRate:        fundingRate,
		FundingRateOK:      fundingErr == nil,
		FundingSettleHours: fundingSettleHours,
		FundingHistory:     fundingRates,
		IntradaySeries:     intradayData,
		LongerTermContext:  longerTermData,
		TimeframeData:      TimeframeData,
	}, nil
}

// GetWithTimeframes retrieves market data for specified multiple timeframes
// timeframes: list of timeframes, e.g. ["5m", "15m", "1h", "4h"]
// primaryTimeframe: primary timeframe (used for calculating current indicators), defaults to timeframes[0]
// count: number of K-lines for each timeframe
func GetWithTimeframes(symbol string, timeframes []string, primaryTimeframe string, count int) (*Data, error) {
	symbol = Normalize(symbol)

	if len(timeframes) == 0 {
		return nil, fmt.Errorf("at least one timeframe is required")
	}

	// If primary timeframe is not specified, use the first one
	if primaryTimeframe == "" {
		primaryTimeframe = timeframes[0]
	}

	// Ensure primary timeframe is in the list
	hasPrimary := false
	for _, tf := range timeframes {
		if tf == primaryTimeframe {
			hasPrimary = true
			break
		}
	}
	if !hasPrimary {
		timeframes = append([]string{primaryTimeframe}, timeframes...)
	}

	// Store data for all timeframes
	timeframeData := make(map[string]*TimeframeSeriesData)
	var primaryKlines []Kline

	// Check if this is an xyz dex asset (use Hyperliquid API)
	isXyzAsset := IsXyzDexAsset(symbol)

	// Get K-line data for each timeframe — request the strategy's configured
	// count (floored at 200 so short configs still have indicator history;
	// capped at 1500, the max most sources return per call).
	fetchCount := count
	if fetchCount < 200 {
		fetchCount = 200
	}
	if fetchCount > 1500 {
		fetchCount = 1500
	}

	for _, tf := range timeframes {
		var klines []Kline
		var err error

		if isXyzAsset {
			// Use Hyperliquid API for xyz dex assets
			klines, err = getKlinesFromHyperliquid(symbol, tf, fetchCount)
			if err != nil {
				logger.Infof("⚠️ Failed to get %s %s K-line from Hyperliquid: %v", symbol, tf, err)
				continue
			}
		} else {
			// Use CoinAnk for regular crypto assets (default to Binance)
			klines, err = getKlinesFromCoinAnk(symbol, tf, "binance", fetchCount)
			if err != nil {
				logger.Infof("⚠️ Failed to get %s %s K-line from CoinAnk: %v", symbol, tf, err)
				continue
			}
		}

		if len(klines) == 0 {
			logger.Infof("⚠️ %s %s K-line data is empty", symbol, tf)
			continue
		}

		// Save primary timeframe K-lines for calculating base indicators
		if tf == primaryTimeframe {
			primaryKlines = klines
		}

		// Calculate series data for this timeframe (use count from config)
		seriesData := calculateTimeframeSeries(klines, tf, count)
		timeframeData[tf] = seriesData
	}

	// If primary timeframe data is empty, return error
	if len(primaryKlines) == 0 {
		return nil, fmt.Errorf("Primary timeframe %s K-line data is empty", primaryTimeframe)
	}

	// Data staleness detection
	if isStaleData(primaryKlines, symbol) {
		logger.Infof("⚠️  WARNING: %s detected stale data (consecutive price freeze), skipping symbol", symbol)
		return nil, fmt.Errorf("%s data is stale, possible cache failure", symbol)
	}

	// Calculate current indicators (based on primary timeframe latest data)
	currentPrice := primaryKlines[len(primaryKlines)-1].Close

	// Live price: the kline vendor's forming candle close does not tick in
	// real time (it can sit at the previous close for the whole bar), so
	// query the exchange ticker for the actual current price. xyz dex assets
	// have no Binance futures ticker — keep the kline close for those.
	livePrice := 0.0
	if !isXyzAsset {
		if p, err := NewAPIClient().GetCurrentPrice(symbol); err == nil && p > 0 {
			livePrice = p
			currentPrice = p
		}
	}

	// Refresh each timeframe's forming candle with the live price. Without
	// this, consumers reading raw klines (the daily technical narrative, the
	// legacy timeframe dumps, grid Donchian levels) quote the vendor's frozen
	// forming-candle close while the structured signal JSON shows the live
	// price — a systematic lag that misleads the model on trending days.
	// Closed candles are never touched.
	vendorStaleness := 0.0
	if livePrice > 0 {
		for tf, sd := range timeframeData {
			if div, patched := refreshFormingCandle(tf, sd, livePrice); patched && tf == primaryTimeframe {
				vendorStaleness = div
			}
		}
	}

	currentEMA20 := calculateEMA(primaryKlines, 20)
	currentMACD := calculateMACD(primaryKlines)
	currentRSI7 := calculateRSI(primaryKlines, 7)

	// Calculate price changes
	priceChange1h := calculatePriceChangeByBars(primaryKlines, primaryTimeframe, 60, currentPrice)  // 1 hour
	priceChange4h := calculatePriceChangeByBars(primaryKlines, primaryTimeframe, 240, currentPrice) // 4 hours

	// Get OI data
	oiData, err := getOpenInterestData(symbol)
	if err != nil {
		oiData = &OIData{Latest: 0, Average: 0}
	}

	// Get Funding Rate + measured settlement interval + settled history
	fundingRate, fundingErr := getFundingRate(symbol)
	fundingSettleHours := fundingIntervalHours(symbol)
	fundingRates, _, histOK := fundingHistory(symbol)
	if !histOK {
		fundingRates = nil
	}

	return &Data{
		Symbol:             symbol,
		CurrentPrice:       currentPrice,
		PriceChange1h:      priceChange1h,
		PriceChange4h:      priceChange4h,
		VendorStalenessPct: vendorStaleness,
		CurrentEMA20:       currentEMA20,
		CurrentMACD:        currentMACD,
		CurrentRSI7:        currentRSI7,
		OpenInterest:       oiData,
		FundingRate:        fundingRate,
		FundingRateOK:      fundingErr == nil,
		FundingSettleHours: fundingSettleHours,
		FundingHistory:     fundingRates,
		TimeframeData:      timeframeData,
	}, nil
}

// TimeframeDuration returns the bar duration of a supported timeframe label,
// or 0 for unknown labels.
func TimeframeDuration(tf string) time.Duration {
	switch tf {
	case "1m":
		return time.Minute
	case "3m":
		return 3 * time.Minute
	case "5m":
		return 5 * time.Minute
	case "15m":
		return 15 * time.Minute
	case "30m":
		return 30 * time.Minute
	case "1h":
		return time.Hour
	case "2h":
		return 2 * time.Hour
	case "4h":
		return 4 * time.Hour
	case "6h":
		return 6 * time.Hour
	case "8h":
		return 8 * time.Hour
	case "12h":
		return 12 * time.Hour
	case "1d":
		return 24 * time.Hour
	}
	return 0
}

// applyLivePrice patches one still-forming candle in place with the live
// ticker price (Close refreshed, High/Low widened). Returns false — leaving
// the candle untouched — when the candle is already closed, the timeframe is
// unknown, or the ticker is implausibly far from the candle (>±80%, which
// indicates broken data rather than volatility; extreme movers are exactly
// where the frozen vendor close is most misleading and must be patched).
func applyLivePrice(bar *KlineBar, tf string, livePrice float64) bool {
	dur := TimeframeDuration(tf)
	if dur <= 0 || bar == nil || bar.Close <= 0 || livePrice <= 0 {
		return false
	}
	if !time.UnixMilli(bar.Time).Add(dur).After(time.Now()) {
		return false // candle already closed
	}
	if livePrice > bar.Close*1.8 || livePrice < bar.Close*0.2 {
		logger.Infof("⚠️ %s forming candle close %.6g vs live price %.6g too far apart, keeping vendor close", tf, bar.Close, livePrice)
		return false
	}
	if livePrice > bar.High {
		bar.High = livePrice
	}
	if livePrice < bar.Low {
		bar.Low = livePrice
	}
	bar.Close = livePrice
	return true
}

// refreshFormingCandle patches the forming candle of a timeframe series with
// the live ticker price so every consumer quoting "the current price" from
// raw klines agrees with Data.CurrentPrice.
func refreshFormingCandle(tf string, sd *TimeframeSeriesData, livePrice float64) (divergencePct float64, patched bool) {
	if sd == nil || len(sd.Klines) == 0 {
		return 0, false
	}
	preClose := sd.Klines[len(sd.Klines)-1].Close
	if applyLivePrice(&sd.Klines[len(sd.Klines)-1], tf, livePrice) {
		last := sd.Klines[len(sd.Klines)-1]
		if len(sd.MidPrices) == len(sd.Klines) {
			sd.MidPrices[len(sd.MidPrices)-1] = (last.High + last.Low) / 2
		}
		if preClose > 0 {
			divergencePct = (livePrice - preClose) / preClose * 100
		}
		return divergencePct, true
	}
	return 0, false
}

// getOpenInterestData retrieves OI data
func getOpenInterestData(symbol string) (*OIData, error) {
	// Real average from 30 hourly history points — the previous placeholder
	// (Average = Latest × 0.999) made oi_vs_avg_pct a constant ~0.1001% for
	// every symbol, which the model could read as genuine crowding data.
	var hist []struct {
		SumOpenInterest string `json:"sumOpenInterest"`
	}
	if err := binanceGetJSON("/futures/data/openInterestHist?symbol="+symbol+"&period=1h&limit=30", &hist); err != nil {
		return nil, err
	}
	if len(hist) == 0 {
		return nil, fmt.Errorf("empty OI history for %s", symbol)
	}

	latest, _ := strconv.ParseFloat(hist[len(hist)-1].SumOpenInterest, 64)
	var sum float64
	for _, h := range hist {
		v, _ := strconv.ParseFloat(h.SumOpenInterest, 64)
		sum += v
	}
	avg := sum / float64(len(hist))

	return &OIData{
		Latest:  latest,
		Average: avg,
	}, nil
}

// binanceGetJSON GETs a public Binance fapi endpoint and decodes the JSON body.
func binanceGetJSON(path string, out interface{}) error {
	apiClient := NewAPIClient()
	resp, err := apiClient.client.Get(binanceFAPIBase + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

const binanceFAPIBase = "https://fapi.binance.com"

// getFundingRate retrieves funding rate (optimized: uses 1-hour cache)
func getFundingRate(symbol string) (float64, error) {
	// Check cache (1-hour validity)
	// Funding Rate only updates every 8 hours, 1-hour cache is very reasonable
	if cached, ok := fundingRateMap.Load(symbol); ok {
		cache := cached.(*FundingRateCache)
		if time.Since(cache.UpdatedAt) < frCacheTTL {
			// Cache hit, return directly
			return cache.Rate, nil
		}
	}

	// Cache expired or doesn't exist, call API
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/premiumIndex?symbol=%s", symbol)

	apiClient := NewAPIClient()
	resp, err := apiClient.client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result struct {
		Symbol          string `json:"symbol"`
		MarkPrice       string `json:"markPrice"`
		IndexPrice      string `json:"indexPrice"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingTime int64  `json:"nextFundingTime"`
		InterestRate    string `json:"interestRate"`
		Time            int64  `json:"time"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}

	if result.Symbol != symbol {
		// A valid-JSON error envelope (e.g. Binance -1121 invalid symbol)
		// unmarshals into empty fields — parsing it would cache a fake 0 for
		// an hour. Only a response that echoes the symbol is real data.
		return 0, fmt.Errorf("premiumIndex response did not echo %s", symbol)
	}

	rate, _ := strconv.ParseFloat(result.LastFundingRate, 64)

	// Update cache — SUCCESSFUL fetches only; failures must retry next call
	// instead of poisoning the hour.
	fundingRateMap.Store(symbol, &FundingRateCache{
		Rate:      rate,
		UpdatedAt: time.Now(),
	})

	return rate, nil
}

// Format formats and outputs market data
func Format(data *Data) string {
	var sb strings.Builder

	// Format price with dynamic precision
	priceStr := formatPriceWithDynamicPrecision(data.CurrentPrice)
	sb.WriteString(fmt.Sprintf("current_price = %s, current_ema20 = %.3f, current_macd = %.3f, current_rsi (7 period) = %.3f\n\n",
		priceStr, data.CurrentEMA20, data.CurrentMACD, data.CurrentRSI7))

	sb.WriteString(fmt.Sprintf("In addition, here is the latest %s open interest and funding rate for perps:\n\n",
		data.Symbol))

	if data.OpenInterest != nil {
		// Format OI data with dynamic precision
		oiLatestStr := formatPriceWithDynamicPrecision(data.OpenInterest.Latest)
		oiAverageStr := formatPriceWithDynamicPrecision(data.OpenInterest.Average)
		sb.WriteString(fmt.Sprintf("Open Interest: Latest: %s Average: %s\n\n",
			oiLatestStr, oiAverageStr))
	}

	sb.WriteString(fmt.Sprintf("Funding Rate: %.2e\n\n", data.FundingRate))

	if data.IntradaySeries != nil {
		sb.WriteString("Intraday series (3‑minute intervals, oldest → latest):\n\n")

		if len(data.IntradaySeries.MidPrices) > 0 {
			sb.WriteString(fmt.Sprintf("Mid prices: %s\n\n", formatFloatSlice(data.IntradaySeries.MidPrices)))
		}

		if len(data.IntradaySeries.EMA20Values) > 0 {
			sb.WriteString(fmt.Sprintf("EMA indicators (20‑period): %s\n\n", formatFloatSlice(data.IntradaySeries.EMA20Values)))
		}

		if len(data.IntradaySeries.MACDValues) > 0 {
			sb.WriteString(fmt.Sprintf("MACD indicators: %s\n\n", formatFloatSlice(data.IntradaySeries.MACDValues)))
		}

		if len(data.IntradaySeries.RSI7Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI indicators (7‑Period): %s\n\n", formatFloatSlice(data.IntradaySeries.RSI7Values)))
		}

		if len(data.IntradaySeries.RSI14Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI indicators (14‑Period): %s\n\n", formatFloatSlice(data.IntradaySeries.RSI14Values)))
		}

		if len(data.IntradaySeries.Volume) > 0 {
			sb.WriteString(fmt.Sprintf("Volume: %s\n\n", formatFloatSlice(data.IntradaySeries.Volume)))
		}

		sb.WriteString(fmt.Sprintf("3m ATR (14‑period): %.3f\n\n", data.IntradaySeries.ATR14))
	}

	if data.LongerTermContext != nil {
		sb.WriteString("Longer‑term context (4‑hour timeframe):\n\n")

		sb.WriteString(fmt.Sprintf("20‑Period EMA: %.3f vs. 50‑Period EMA: %.3f\n\n",
			data.LongerTermContext.EMA20, data.LongerTermContext.EMA50))

		sb.WriteString(fmt.Sprintf("3‑Period ATR: %.3f vs. 14‑Period ATR: %.3f\n\n",
			data.LongerTermContext.ATR3, data.LongerTermContext.ATR14))

		sb.WriteString(fmt.Sprintf("Current Volume: %.3f vs. Average Volume: %.3f\n\n",
			data.LongerTermContext.CurrentVolume, data.LongerTermContext.AverageVolume))

		if len(data.LongerTermContext.MACDValues) > 0 {
			sb.WriteString(fmt.Sprintf("MACD indicators: %s\n\n", formatFloatSlice(data.LongerTermContext.MACDValues)))
		}

		if len(data.LongerTermContext.RSI14Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI indicators (14‑Period): %s\n\n", formatFloatSlice(data.LongerTermContext.RSI14Values)))
		}
	}

	// Multi-timeframe data (new)
	if len(data.TimeframeData) > 0 {
		// Output sorted by timeframe
		timeframeOrder := []string{"1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "8h", "12h", "1d", "3d", "1w"}
		for _, tf := range timeframeOrder {
			if tfData, ok := data.TimeframeData[tf]; ok {
				sb.WriteString(fmt.Sprintf("=== %s Timeframe ===\n\n", strings.ToUpper(tf)))
				formatTimeframeData(&sb, tfData)
			}
		}
	}

	return sb.String()
}

// formatTimeframeData formats data for a single timeframe
func formatTimeframeData(sb *strings.Builder, data *TimeframeSeriesData) {
	// Use OHLCV table format if kline data is available
	if len(data.Klines) > 0 {
		sb.WriteString("Time(UTC)      Open      High      Low       Close     Volume\n")
		for i, k := range data.Klines {
			t := time.Unix(k.Time/1000, 0).UTC()
			timeStr := t.Format("01-02 15:04")
			marker := ""
			if i == len(data.Klines)-1 {
				marker = "  <- current"
			}
			sb.WriteString(fmt.Sprintf("%-14s %-9.4f %-9.4f %-9.4f %-9.4f %-12.2f%s\n",
				timeStr, k.Open, k.High, k.Low, k.Close, k.Volume, marker))
		}
		sb.WriteString("\n")
	} else if len(data.MidPrices) > 0 {
		// Fallback to old format for backward compatibility
		sb.WriteString(fmt.Sprintf("Mid prices: %s\n\n", formatFloatSlice(data.MidPrices)))
		if len(data.Volume) > 0 {
			sb.WriteString(fmt.Sprintf("Volume: %s\n\n", formatFloatSlice(data.Volume)))
		}
	}

	// Technical indicators
	if len(data.EMA20Values) > 0 {
		sb.WriteString(fmt.Sprintf("EMA20: %s\n", formatFloatSlice(data.EMA20Values)))
	}

	if len(data.EMA50Values) > 0 {
		sb.WriteString(fmt.Sprintf("EMA50: %s\n", formatFloatSlice(data.EMA50Values)))
	}

	if len(data.MACDValues) > 0 {
		sb.WriteString(fmt.Sprintf("MACD: %s\n", formatFloatSlice(data.MACDValues)))
	}

	if len(data.RSI7Values) > 0 {
		sb.WriteString(fmt.Sprintf("RSI7: %s\n", formatFloatSlice(data.RSI7Values)))
	}

	if len(data.RSI14Values) > 0 {
		sb.WriteString(fmt.Sprintf("RSI14: %s\n", formatFloatSlice(data.RSI14Values)))
	}

	if data.ATR14 > 0 {
		sb.WriteString(fmt.Sprintf("ATR14: %.4f\n", data.ATR14))
	}

	sb.WriteString("\n")
}

// formatPriceWithDynamicPrecision dynamically selects precision based on price range
// This perfectly supports all coins from ultra-low price meme coins (< 0.0001) to BTC/ETH
func formatPriceWithDynamicPrecision(price float64) string {
	switch {
	case price < 0.0001:
		// Ultra-low price meme coins: 1000SATS, 1000WHY, DOGS
		// 0.00002070 → "0.00002070" (8 decimal places)
		return fmt.Sprintf("%.8f", price)
	case price < 0.001:
		// Low price meme coins: NEIRO, HMSTR, HOT, NOT
		// 0.00015060 → "0.000151" (6 decimal places)
		return fmt.Sprintf("%.6f", price)
	case price < 0.01:
		// Mid-low price coins: PEPE, SHIB, MEME
		// 0.00556800 → "0.005568" (6 decimal places)
		return fmt.Sprintf("%.6f", price)
	case price < 1.0:
		// Low price coins: ASTER, DOGE, ADA, TRX
		// 0.9954 → "0.9954" (4 decimal places)
		return fmt.Sprintf("%.4f", price)
	case price < 100:
		// Mid price coins: SOL, AVAX, LINK, MATIC
		// 23.4567 → "23.4567" (4 decimal places)
		return fmt.Sprintf("%.4f", price)
	default:
		// High price coins: BTC, ETH (save tokens)
		// 45678.9123 → "45678.91" (2 decimal places)
		return fmt.Sprintf("%.2f", price)
	}
}

// formatFloatSlice formats float64 slice to string (using dynamic precision)
func formatFloatSlice(values []float64) string {
	strValues := make([]string, len(values))
	for i, v := range values {
		strValues[i] = formatPriceWithDynamicPrecision(v)
	}
	return "[" + strings.Join(strValues, ", ") + "]"
}

// xyz dex assets that should NOT get USDT suffix
var xyzDexAssets = map[string]bool{
	// Stocks
	"TSLA": true, "NVDA": true, "AAPL": true, "MSFT": true, "META": true,
	"AMZN": true, "GOOGL": true, "AMD": true, "COIN": true, "NFLX": true,
	"PLTR": true, "HOOD": true, "INTC": true, "MSTR": true, "TSM": true,
	"ORCL": true, "MU": true, "RIVN": true, "COST": true, "LLY": true,
	"CRCL": true, "SKHX": true, "SNDK": true,
	// Forex
	"EUR": true, "JPY": true,
	// Commodities
	"GOLD": true, "SILVER": true,
	// Index
	"XYZ100": true,
}

// IsXyzDexAsset checks if a symbol is an xyz dex asset
func IsXyzDexAsset(symbol string) bool {
	base := strings.ToUpper(symbol)
	// Remove any prefix/suffix
	base = strings.TrimPrefix(base, "XYZ:")
	for _, suffix := range []string{"USDT", "USD", "-USDC"} {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	return xyzDexAssets[base]
}

// Normalize normalizes symbol
// For crypto: ensures it's a USDT trading pair
// For xyz dex assets (stocks, forex, commodities): uses xyz: prefix without USDT suffix
func Normalize(symbol string) string {
	symbol = strings.ToUpper(symbol)

	// Check if this is an xyz dex asset
	if IsXyzDexAsset(symbol) {
		// Remove any xyz: prefix (case-insensitive) and USDT suffix, then add xyz: prefix
		base := symbol
		// Handle both lowercase and uppercase xyz: prefix
		if strings.HasPrefix(strings.ToLower(base), "xyz:") {
			base = base[4:] // Remove first 4 characters ("xyz:")
		}
		for _, suffix := range []string{"USDT", "USD", "-USDC"} {
			if strings.HasSuffix(base, suffix) {
				base = strings.TrimSuffix(base, suffix)
				break
			}
		}
		return "xyz:" + base
	}

	// Remove exchange-specific separators (Gate uses BTC_USDT, OKX uses BTC-USDT-SWAP)
	symbol = strings.ReplaceAll(symbol, "_", "")
	symbol = strings.ReplaceAll(symbol, "-SWAP", "")
	symbol = strings.ReplaceAll(symbol, "-", "")

	// For regular crypto assets
	if strings.HasSuffix(symbol, "USDT") {
		return symbol
	}
	return symbol + "USDT"
}

// parseFloat parses float value
func parseFloat(v interface{}) (float64, error) {
	switch val := v.(type) {
	case string:
		return strconv.ParseFloat(val, 64)
	case float64:
		return val, nil
	case int:
		return float64(val), nil
	case int64:
		return float64(val), nil
	default:
		return 0, fmt.Errorf("unsupported type: %T", v)
	}
}

// BuildDataFromKlines constructs market data snapshot from preloaded K-line series.
func BuildDataFromKlines(symbol string, primary []Kline, longer []Kline) (*Data, error) {
	if len(primary) == 0 {
		return nil, fmt.Errorf("primary series is empty")
	}

	symbol = Normalize(symbol)
	current := primary[len(primary)-1]
	currentPrice := current.Close

	data := &Data{
		Symbol:            symbol,
		CurrentPrice:      currentPrice,
		CurrentEMA20:      calculateEMA(primary, 20),
		CurrentMACD:       calculateMACD(primary),
		CurrentRSI7:       calculateRSI(primary, 7),
		PriceChange1h:     priceChangeFromSeries(primary, time.Hour),
		PriceChange4h:     priceChangeFromSeries(primary, 4*time.Hour),
		OpenInterest:      &OIData{Latest: 0, Average: 0},
		FundingRate:       0,
		IntradaySeries:    calculateIntradaySeries(primary),
		LongerTermContext: nil,
	}

	if len(longer) > 0 {
		data.LongerTermContext = calculateLongerTermData(longer)
	}

	return data, nil
}

func priceChangeFromSeries(series []Kline, duration time.Duration) float64 {
	if len(series) == 0 || duration <= 0 {
		return 0
	}
	last := series[len(series)-1]
	target := last.CloseTime - duration.Milliseconds()
	for i := len(series) - 1; i >= 0; i-- {
		if series[i].CloseTime <= target {
			price := series[i].Close
			if price > 0 {
				return ((last.Close - price) / price) * 100
			}
			break
		}
	}
	return 0
}

// isStaleData detects stale data (consecutive price freeze)
// Fix DOGEUSDT-style issue: consecutive N periods with completely unchanged prices indicate data source anomaly
func isStaleData(klines []Kline, symbol string) bool {
	if len(klines) < 5 {
		return false // Insufficient data to determine
	}

	// Detection threshold: 5 consecutive 3-minute periods with unchanged price (15 minutes without fluctuation)
	const stalePriceThreshold = 5
	const priceTolerancePct = 0.0001 // 0.01% fluctuation tolerance (avoid false positives)

	// Take the last stalePriceThreshold K-lines
	recentKlines := klines[len(klines)-stalePriceThreshold:]
	firstPrice := recentKlines[0].Close

	// Check if all prices are within tolerance
	for i := 1; i < len(recentKlines); i++ {
		priceDiff := math.Abs(recentKlines[i].Close-firstPrice) / firstPrice
		if priceDiff > priceTolerancePct {
			return false // Price fluctuation exists, data is normal
		}
	}

	// Additional check: MACD and volume
	// If price is unchanged but MACD/volume shows normal fluctuation, it might be a real market situation (extremely low volatility)
	// Check if volume is also 0 (data completely frozen)
	allVolumeZero := true
	for _, k := range recentKlines {
		if k.Volume > 0 {
			allVolumeZero = false
			break
		}
	}

	if allVolumeZero {
		logger.Infof("⚠️  %s stale data confirmed: price freeze + zero volume", symbol)
		return true
	}

	// Price frozen but has volume: might be extremely low volatility market, allow but log warning
	logger.Infof("⚠️  %s detected extreme price stability (no fluctuation for %d consecutive periods), but volume is normal", symbol, stalePriceThreshold)
	return false
}

// executionTimeframeData builds the TimeframeData map the execution-side
// risk gates read (3m/4h always fetched; 1h best-effort — its absence makes
// the stop-floor fall back to 4h, a wider but still-enforced floor).
func executionTimeframeData(klines3m, klines1h, klines4h []Kline) map[string]*TimeframeSeriesData {
	tf := map[string]*TimeframeSeriesData{}
	if len(klines3m) > 0 {
		tf["3m"] = calculateTimeframeSeries(klines3m, "3m", len(klines3m))
	}
	if len(klines1h) > 0 {
		tf["1h"] = calculateTimeframeSeries(klines1h, "1h", len(klines1h))
	}
	if len(klines4h) > 0 {
		tf["4h"] = calculateTimeframeSeries(klines4h, "4h", len(klines4h))
	}
	return tf
}
