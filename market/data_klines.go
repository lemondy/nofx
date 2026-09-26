package market

import (
	"context"
	"fmt"
	"net/url"
	"nofx/logger"
	"nofx/provider/coinank/coinank_api"
	"nofx/provider/coinank/coinank_enum"
	"nofx/provider/hyperliquid"
	"nofx/provider/openbb"
	"strconv"
	"strings"
	"time"
)

// Note: Kline data now uses free/open API (coinank_api.Kline) which doesn't require authentication

// getKlinesFromCoinAnk fetches kline data from CoinAnk API (replacement for WSMonitorCli)
func getKlinesFromCoinAnk(symbol, interval, exchange string, limit int) ([]Kline, error) {
	// Map interval string to coinank enum
	var coinankInterval coinank_enum.Interval
	switch interval {
	case "1m":
		coinankInterval = coinank_enum.Minute1
	case "3m":
		coinankInterval = coinank_enum.Minute3
	case "5m":
		coinankInterval = coinank_enum.Minute5
	case "15m":
		coinankInterval = coinank_enum.Minute15
	case "30m":
		coinankInterval = coinank_enum.Minute30
	case "1h":
		coinankInterval = coinank_enum.Hour1
	case "2h":
		coinankInterval = coinank_enum.Hour2
	case "4h":
		coinankInterval = coinank_enum.Hour4
	case "6h":
		coinankInterval = coinank_enum.Hour6
	case "8h":
		coinankInterval = coinank_enum.Hour8
	case "12h":
		coinankInterval = coinank_enum.Hour12
	case "1d":
		coinankInterval = coinank_enum.Day1
	case "3d":
		coinankInterval = coinank_enum.Day3
	case "1w":
		coinankInterval = coinank_enum.Week1
	default:
		return nil, fmt.Errorf("unsupported interval: %s", interval)
	}

	// Map exchange string to coinank enum
	var coinankExchange coinank_enum.Exchange
	switch strings.ToLower(exchange) {
	case "binance":
		coinankExchange = coinank_enum.Binance
	case "bybit":
		coinankExchange = coinank_enum.Bybit
	case "okx":
		coinankExchange = coinank_enum.Okex
	case "bitget":
		coinankExchange = coinank_enum.Bitget
	case "gate":
		coinankExchange = coinank_enum.Gate
	case "hyperliquid":
		coinankExchange = coinank_enum.Hyperliquid
	case "aster":
		coinankExchange = coinank_enum.Aster
	default:
		return nil, fmt.Errorf("unsupported exchange for market data: %s", exchange)
	}

	// Call CoinAnk free/open API (no authentication required)
	ctx := context.Background()
	ts := time.Now().UnixMilli()
	// Use "To" side to search backward from current time (get historical klines)
	coinankKlines, err := coinank_api.Kline(ctx, symbol, coinankExchange, ts, coinank_enum.To, limit, coinankInterval)
	if err != nil || len(coinankKlines) == 0 {
		if err != nil {
			return nil, fmt.Errorf("CoinAnk %s API error: %w", exchange, err)
		}
		return nil, fmt.Errorf("CoinAnk returned no %s candles for %s", exchange, symbol)
	}

	// Convert coinank kline format to market.Kline format
	klines := make([]Kline, len(coinankKlines))
	for i, ck := range coinankKlines {
		klines[i] = Kline{
			OpenTime:  ck.StartTime,
			Open:      ck.Open,
			High:      ck.High,
			Low:       ck.Low,
			Close:     ck.Close,
			Volume:    ck.Volume,
			CloseTime: ck.EndTime,
		}
	}

	return klines, nil
}

// getKlinesFromHyperliquid fetches kline data from Hyperliquid API for xyz dex assets
func getKlinesFromHyperliquid(symbol, interval string, limit int) ([]Kline, error) {
	// Remove xyz: prefix if present for the API call
	baseCoin := strings.TrimPrefix(symbol, "xyz:")

	// Map interval to Hyperliquid format
	hlInterval := hyperliquid.MapTimeframe(interval)

	// Create Hyperliquid client
	client := hyperliquid.NewClient()

	// Fetch candles
	ctx := context.Background()
	candles, err := client.GetCandles(ctx, baseCoin, hlInterval, limit)
	if err != nil {
		return nil, fmt.Errorf("Hyperliquid API error: %w", err)
	}

	// Convert to market.Kline format
	klines := make([]Kline, len(candles))
	for i, c := range candles {
		open, _ := strconv.ParseFloat(c.Open, 64)
		high, _ := strconv.ParseFloat(c.High, 64)
		low, _ := strconv.ParseFloat(c.Low, 64)
		closePrice, _ := strconv.ParseFloat(c.Close, 64)
		volume, _ := strconv.ParseFloat(c.Volume, 64)

		klines[i] = Kline{
			OpenTime:  c.OpenTime,
			Open:      open,
			High:      high,
			Low:       low,
			Close:     closePrice,
			Volume:    volume,
			CloseTime: c.CloseTime,
		}
	}

	return klines, nil
}

// calculateTimeframeSeries calculates series data for a single timeframe
func calculateTimeframeSeries(klines []Kline, timeframe string, count int) *TimeframeSeriesData {
	if count <= 0 {
		count = 10 // default
	}

	data := &TimeframeSeriesData{
		Timeframe:   timeframe,
		Klines:      make([]KlineBar, 0, count),
		MidPrices:   make([]float64, 0, count),
		EMA20Values: make([]float64, 0, count),
		EMA50Values: make([]float64, 0, count),
		MACDValues:  make([]float64, 0, count),
		RSI7Values:  make([]float64, 0, count),
		RSI14Values: make([]float64, 0, count),
		Volume:      make([]float64, 0, count),
		BOLLUpper:   make([]float64, 0, count),
		BOLLMiddle:  make([]float64, 0, count),
		BOLLLower:   make([]float64, 0, count),
	}

	// Get latest N data points based on count from config
	start := len(klines) - count
	if start < 0 {
		start = 0
	}

	for i := start; i < len(klines); i++ {
		// Store full OHLCV kline data
		data.Klines = append(data.Klines, KlineBar{
			Time:   klines[i].OpenTime,
			Open:   klines[i].Open,
			High:   klines[i].High,
			Low:    klines[i].Low,
			Close:  klines[i].Close,
			Volume: klines[i].Volume,
		})

		// Keep MidPrices and Volume for backward compatibility
		data.MidPrices = append(data.MidPrices, klines[i].Close)
		data.Volume = append(data.Volume, klines[i].Volume)

		// Calculate EMA20 for each point
		if i >= 19 {
			ema20 := calculateEMA(klines[:i+1], 20)
			data.EMA20Values = append(data.EMA20Values, ema20)
		}

		// Calculate EMA50 for each point
		if i >= 49 {
			ema50 := calculateEMA(klines[:i+1], 50)
			data.EMA50Values = append(data.EMA50Values, ema50)
		}

		// Calculate MACD for each point
		if i >= 25 {
			macd := calculateMACD(klines[:i+1])
			data.MACDValues = append(data.MACDValues, macd)
		}

		// Calculate RSI for each point
		if i >= 7 {
			rsi7 := calculateRSI(klines[:i+1], 7)
			data.RSI7Values = append(data.RSI7Values, rsi7)
		}
		if i >= 14 {
			rsi14 := calculateRSI(klines[:i+1], 14)
			data.RSI14Values = append(data.RSI14Values, rsi14)
		}

		// Calculate Bollinger Bands (period 20, std dev multiplier 2)
		if i >= 19 {
			upper, middle, lower := calculateBOLL(klines[:i+1], 20, 2.0)
			data.BOLLUpper = append(data.BOLLUpper, upper)
			data.BOLLMiddle = append(data.BOLLMiddle, middle)
			data.BOLLLower = append(data.BOLLLower, lower)
		}
	}

	// Calculate ATR14
	data.ATR14 = calculateATR(klines, 14)

	return data
}

// calculatePriceChangeByBars calculates how many K-lines to look back for price change based on timeframe.
// currentPrice is the live ticker price: the kline vendor's forming candle close does not
// tick in real time (it carries over the previous close), so the series tail must not
// anchor the "current" leg of the change.
func calculatePriceChangeByBars(klines []Kline, timeframe string, targetMinutes int, currentPrice float64) float64 {
	if len(klines) < 2 {
		return 0
	}

	// Parse timeframe to minutes
	tfMinutes := parseTimeframeToMinutes(timeframe)
	if tfMinutes <= 0 {
		return 0
	}

	// Calculate how many K-lines to look back
	barsBack := targetMinutes / tfMinutes
	if barsBack < 1 {
		barsBack = 1
	}

	if currentPrice <= 0 {
		currentPrice = klines[len(klines)-1].Close
	}
	idx := len(klines) - 1 - barsBack
	if idx < 0 {
		idx = 0
	}

	oldPrice := klines[idx].Close
	if oldPrice > 0 {
		return ((currentPrice - oldPrice) / oldPrice) * 100
	}
	return 0
}

// parseTimeframeToMinutes parses timeframe string to minutes
func parseTimeframeToMinutes(tf string) int {
	switch tf {
	case "1m":
		return 1
	case "3m":
		return 3
	case "5m":
		return 5
	case "15m":
		return 15
	case "30m":
		return 30
	case "1h":
		return 60
	case "2h":
		return 120
	case "4h":
		return 240
	case "6h":
		return 360
	case "8h":
		return 480
	case "12h":
		return 720
	case "1d":
		return 1440
	case "3d":
		return 4320
	case "1w":
		return 10080
	default:
		return 0
	}
}

// calculateIntradaySeries calculates intraday series data
func calculateIntradaySeries(klines []Kline) *IntradayData {
	data := &IntradayData{
		MidPrices:   make([]float64, 0, 10),
		EMA20Values: make([]float64, 0, 10),
		MACDValues:  make([]float64, 0, 10),
		RSI7Values:  make([]float64, 0, 10),
		RSI14Values: make([]float64, 0, 10),
		Volume:      make([]float64, 0, 10),
	}

	// Get latest 10 data points
	start := len(klines) - 10
	if start < 0 {
		start = 0
	}

	for i := start; i < len(klines); i++ {
		data.MidPrices = append(data.MidPrices, klines[i].Close)
		data.Volume = append(data.Volume, klines[i].Volume)

		// Calculate EMA20 for each point
		if i >= 19 {
			ema20 := calculateEMA(klines[:i+1], 20)
			data.EMA20Values = append(data.EMA20Values, ema20)
		}

		// Calculate MACD for each point
		if i >= 25 {
			macd := calculateMACD(klines[:i+1])
			data.MACDValues = append(data.MACDValues, macd)
		}

		// Calculate RSI for each point
		if i >= 7 {
			rsi7 := calculateRSI(klines[:i+1], 7)
			data.RSI7Values = append(data.RSI7Values, rsi7)
		}
		if i >= 14 {
			rsi14 := calculateRSI(klines[:i+1], 14)
			data.RSI14Values = append(data.RSI14Values, rsi14)
		}
	}

	// Calculate 3m ATR14
	data.ATR14 = calculateATR(klines, 14)

	return data
}

// calculateLongerTermData calculates longer-term data
func calculateLongerTermData(klines []Kline) *LongerTermData {
	data := &LongerTermData{
		MACDValues:  make([]float64, 0, 10),
		RSI14Values: make([]float64, 0, 10),
	}

	// Calculate EMA
	data.EMA20 = calculateEMA(klines, 20)
	data.EMA50 = calculateEMA(klines, 50)

	// Calculate ATR
	data.ATR3 = calculateATR(klines, 3)
	data.ATR14 = calculateATR(klines, 14)

	// Calculate volume
	if len(klines) > 0 {
		data.CurrentVolume = klines[len(klines)-1].Volume
		// Calculate average volume
		sum := 0.0
		for _, k := range klines {
			sum += k.Volume
		}
		data.AverageVolume = sum / float64(len(klines))
	}

	// Calculate MACD and RSI series
	start := len(klines) - 10
	if start < 0 {
		start = 0
	}

	for i := start; i < len(klines); i++ {
		if i >= 25 {
			macd := calculateMACD(klines[:i+1])
			data.MACDValues = append(data.MACDValues, macd)
		}
		if i >= 14 {
			rsi14 := calculateRSI(klines[:i+1], 14)
			data.RSI14Values = append(data.RSI14Values, rsi14)
		}
	}

	return data
}

// GetBoxData fetches 1h klines and calculates box data for a symbol
func GetBoxData(symbol string) (*BoxData, error) {
	symbol = Normalize(symbol)

	// Fetch 500 1h klines
	var klines []Kline
	var err error

	if IsXyzDexAsset(symbol) {
		klines, err = getKlinesFromHyperliquid(symbol, "1h", LongBoxPeriod)
	} else {
		klines, err = getKlinesFromCoinAnk(symbol, "1h", "binance", LongBoxPeriod)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get 1h klines: %w", err)
	}

	if len(klines) == 0 {
		return nil, fmt.Errorf("no kline data available")
	}

	currentPrice := klines[len(klines)-1].Close

	return calculateBoxData(klines, currentPrice), nil
}

// getKlinesFromBinanceDirect pulls klines straight from Binance fapi — the
// fallback when CoinAnk (the primary vendor) fails. 2026-09-24: CoinAnk
// started returning 403 on ALL kline requests and every candidate went
// DATA_INSUFFICIENT + VENDOR_DIVERGENCE → fail-closed zero opens for hours.
// A single-vendor hard dependency was the real defect; Binance is the
// exchange we actually trade, its data is authoritative for this system.
// Proxy handling rides binanceGetJSON's client (SafeHTTPClient).
func getKlinesFromBinanceDirect(symbol, interval string, limit int) ([]Kline, error) {
	if limit <= 0 || limit > 1500 {
		limit = 100
	}
	var raw [][]interface{}
	if err := binanceGetJSON(fmt.Sprintf("/fapi/v1/klines?symbol=%s&interval=%s&limit=%d",
		url.QueryEscape(symbol), url.QueryEscape(interval), limit), &raw); err != nil {
		return nil, err
	}
	out := make([]Kline, 0, len(raw))
	for _, row := range raw {
		if len(row) < 11 {
			continue
		}
		k := Kline{
			OpenTime: toI64(row[0]), Open: toF64(row[1]), High: toF64(row[2]),
			Low: toF64(row[3]), Close: toF64(row[4]), Volume: toF64(row[5]),
			CloseTime: toI64(row[6]), QuoteVolume: toF64(row[7]),
			Trades: int(toF64(row[8])), TakerBuyBaseVolume: toF64(row[9]),
			TakerBuyQuoteVolume: toF64(row[10]),
		}
		if k.Close > 0 {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("binance direct: empty klines for %s %s", symbol, interval)
	}
	return out, nil
}

func toF64(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

func toI64(v interface{}) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

// getKlinesWithFallback: CoinAnk first (exchange-specific view), Binance
// direct when it fails. Every kline consumer in the system routes through
// this so a vendor outage degrades to the authoritative exchange instead of
// failing every symbol closed.
func getKlinesWithFallback(symbol, interval, exchange string, limit int) ([]Kline, error) {
	k, err := getKlinesFromCoinAnk(symbol, interval, exchange, limit)
	if err == nil && len(k) > 0 {
		return k, nil
	}
	if !strings.EqualFold(exchange, "binance") {
		return nil, fmt.Errorf("exchange-specific %s %s klines unavailable: %w", exchange, symbol, err)
	}
	k2, err2 := getKlinesFromBinanceDirect(symbol, interval, limit)
	if err2 == nil && len(k2) > 0 {
		if err != nil {
			logger.Infof("🔁 %s %s klines: CoinAnk unavailable (%v) — Binance direct fallback", symbol, interval, err)
		}
		return k2, nil
	}
	// Tier 3: OpenBB sidecar (yfinance) — majors only, altcoin coverage is
	// spotty there; 4h has no yfinance native and rides 1h bars as-is.
	if openbb.Available() {
		base := strings.ToUpper(symbol)
		base = strings.TrimSuffix(strings.TrimSuffix(base, "USDT"), "USD")
		rows, err3 := openbb.CryptoKlines(context.Background(), base, interval, limit)
		if err3 == nil && len(rows) > 0 {
			out := make([]Kline, 0, len(rows))
			for _, r := range rows {
				out = append(out, Kline{
					OpenTime: int64(r[0]), Open: r[1], High: r[2], Low: r[3],
					Close: r[4], Volume: r[5],
				})
			}
			logger.Infof("🔁 %s %s klines: CoinAnk + Binance direct both failed — OpenBB/yfinance fallback (%d bars)", symbol, interval, len(out))
			return out, nil
		} else if err3 != nil {
			return nil, fmt.Errorf("coinank: %v; binance direct: %v; openbb: %v", err, err2, err3)
		}
	}
	return nil, fmt.Errorf("coinank: %v; binance direct: %v", err, err2)
}

// getExecutionKlines never substitutes another venue or a spot-data sidecar
// for the market the trader will execute on. Binance's direct futures API is
// an acceptable same-venue fallback; other venues fail closed when their
// exchange-specific CoinAnk series is unavailable.
func getExecutionKlines(symbol, interval, exchange string, limit int) ([]Kline, error) {
	k, err := getKlinesFromCoinAnk(symbol, interval, exchange, limit)
	if err == nil && len(k) > 0 {
		return k, nil
	}
	if !strings.EqualFold(exchange, "binance") {
		return nil, fmt.Errorf("exchange-specific %s %s klines unavailable: %w", exchange, symbol, err)
	}
	k, fallbackErr := getKlinesFromBinanceDirect(symbol, interval, limit)
	if fallbackErr != nil || len(k) == 0 {
		return nil, fmt.Errorf("Binance execution klines unavailable (CoinAnk: %v; Binance: %v)", err, fallbackErr)
	}
	return k, nil
}
