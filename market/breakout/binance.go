package breakout

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"nofx/security"
)

const (
	defaultFAPIBase = "https://fapi.binance.com"
	defaultSPOTBase = "https://api.binance.com"
)

var binanceHTTP = security.SafeHTTPClient(15 * time.Second)

func fapiBase() string {
	if v := os.Getenv("BINANCE_FAPI_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultFAPIBase
}

func spotBase() string {
	if v := os.Getenv("BINANCE_SPOT_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultSPOTBase
}

// Kline is one candle with taker-flow detail.
type Kline struct {
	OpenTime     int64
	Open         float64
	High         float64
	Low          float64
	Close        float64
	Volume       float64 // base volume
	QuoteVolume  float64 // quote turnover
	Trades       int64
	TakerBuyBase float64
	TakerBuyQty  float64 // quote
}

// LongShortRatio fetches the global long/short account ratio samples.
func (b *binanceDS) LongShortRatio(period string, limit int) ([]LongShortPoint, error) {
	if limit > 500 {
		limit = 500
	}
	u := fmt.Sprintf("%s/futures/data/globalLongShortAccountRatio?symbol=%s&period=%s&limit=%d",
		fapiBase(), b.symbol, period, limit)
	var raw []struct {
		LongShortRatio string `json:"longShortRatio"`
		LongAccount    string `json:"longAccount"`
		ShortAccount   string `json:"shortAccount"`
		Timestamp      int64  `json:"timestamp"`
	}
	if err := fetchJSON(u, &raw); err != nil {
		return nil, err
	}
	out := make([]LongShortPoint, 0, len(raw))
	for _, r := range raw {
		ratio, _ := strconv.ParseFloat(r.LongShortRatio, 64)
		lp, _ := strconv.ParseFloat(r.LongAccount, 64)
		sp, _ := strconv.ParseFloat(r.ShortAccount, 64)
		out = append(out, LongShortPoint{Ratio: ratio, LongPct: lp * 100, ShortPct: sp * 100, TS: r.Timestamp})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty long/short ratio for %s", b.symbol)
	}
	return out, nil
}

// DepthSnapshot carries the ±1% book imbalance around mid price.
type DepthSnapshot struct {
	Mid        float64
	BestBid    float64
	BestAsk    float64
	SpreadPct  float64 // (ask-bid)/mid × 100
	BidQty1Pct float64
	AskQty1Pct float64
	Imbalance  float64 // (bid-ask)/(bid+ask), -1..+1
}

// OIPoint is one open-interest history sample.
type OIPoint struct {
	OI    float64
	Value float64 // notional
	TS    int64
}

// FundingPoint is one funding rate sample.
type FundingPoint struct {
	Rate float64
	TS   int64
}

// FundingInfo is the current (forward-looking) funding state for a symbol:
// the rate the NEXT settlement will pay, and that settlement's interval —
// which is NOT always 8h on Binance (volatile listings settle every 1h/4h).
type FundingInfo struct {
	NextRate      float64 // premiumIndex lastFundingRate (settles at next timestamp)
	IntervalHours float64 // actual settlement interval derived from recent history
}

// LongShortPoint is one global long/short account ratio sample.
type LongShortPoint struct {
	Ratio    float64 // long accounts / short accounts (>1 = long-skewed)
	LongPct  float64 // share of long accounts, 0-100
	ShortPct float64 // share of short accounts, 0-100
	TS       int64
}

// DataSource abstracts the market data needed by the engine (mockable in tests).
type DataSource interface {
	Klines(interval string, limit int) ([]Kline, error)                // USDT-M futures
	SpotKlines(interval string, limit int) ([]Kline, error)            // spot
	Depth1Pct() (*DepthSnapshot, error)                                // ±1% book imbalance
	OIHistory(period string, limit int) ([]OIPoint, error)             // 5m open-interest history
	FundingHistory(limit int) ([]FundingPoint, error)                  // funding rate history
	FundingInfo() (*FundingInfo, error)                                // next-settlement rate + real interval
	LongShortRatio(period string, limit int) ([]LongShortPoint, error) // global account L/S ratio
}

// binanceDS implements DataSource against Binance public endpoints.
type binanceDS struct{ symbol string }

// NewBinanceDS creates a Binance public-market data source for symbol.
func NewBinanceDS(symbol string) DataSource { return &binanceDS{symbol: symbol} }

func (b *binanceDS) Klines(interval string, limit int) ([]Kline, error) {
	return fetchKlines(fapiBase()+"/fapi/v1/klines", b.symbol, interval, limit)
}

func (b *binanceDS) SpotKlines(interval string, limit int) ([]Kline, error) {
	return fetchKlines(spotBase()+"/api/v3/klines", b.symbol, interval, limit)
}

func (b *binanceDS) Depth1Pct() (*DepthSnapshot, error) {
	u := fmt.Sprintf("%s/fapi/v1/depth?symbol=%s&limit=100", fapiBase(), b.symbol)
	var raw struct {
		Bids [][]interface{} `json:"bids"`
		Asks [][]interface{} `json:"asks"`
	}
	if err := fetchJSON(u, &raw); err != nil {
		return nil, err
	}
	if len(raw.Bids) == 0 || len(raw.Asks) == 0 {
		return nil, fmt.Errorf("empty depth book")
	}
	bestBid := toF(raw.Bids[0][0])
	bestAsk := toF(raw.Asks[0][0])
	mid := (bestBid + bestAsk) / 2
	if mid <= 0 {
		return nil, fmt.Errorf("invalid mid price")
	}
	band := mid * 0.01
	var bidQty, askQty float64
	for _, lv := range raw.Bids {
		p, q := toF(lv[0]), toF(lv[1])
		if p >= mid-band {
			bidQty += q
		}
	}
	for _, lv := range raw.Asks {
		p, q := toF(lv[0]), toF(lv[1])
		if p <= mid+band {
			askQty += q
		}
	}
	imb := 0.0
	if bidQty+askQty > 0 {
		imb = (bidQty - askQty) / (bidQty + askQty)
	}
	spreadPct := (bestAsk - bestBid) / mid * 100
	return &DepthSnapshot{
		Mid: mid, BestBid: bestBid, BestAsk: bestAsk, SpreadPct: spreadPct,
		BidQty1Pct: bidQty, AskQty1Pct: askQty, Imbalance: imb,
	}, nil
}

func (b *binanceDS) OIHistory(period string, limit int) ([]OIPoint, error) {
	u := fmt.Sprintf("%s/futures/data/openInterestHist?symbol=%s&period=%s&limit=%d",
		fapiBase(), b.symbol, period, limit)
	var raw []struct {
		SumOpenInterest    string `json:"sumOpenInterest"`
		SumOpenInterestVal string `json:"sumOpenInterestValue"`
		Timestamp          int64  `json:"timestamp"`
	}
	if err := fetchJSON(u, &raw); err != nil {
		return nil, err
	}
	out := make([]OIPoint, 0, len(raw))
	for _, r := range raw {
		oi, _ := strconv.ParseFloat(r.SumOpenInterest, 64)
		val, _ := strconv.ParseFloat(r.SumOpenInterestVal, 64)
		out = append(out, OIPoint{OI: oi, Value: val, TS: r.Timestamp})
	}
	return out, nil
}

// FundingInfo combines the forward rate (premiumIndex) with the settlement
// interval measured from recent settlement timestamps.
func (b *binanceDS) FundingInfo() (*FundingInfo, error) {
	// Forward-looking rate: what the next settlement charges.
	var px struct {
		LastFundingRate string `json:"lastFundingRate"`
	}
	if err := fetchJSON(fapiBase()+"/fapi/v1/premiumIndex?symbol="+b.symbol, &px); err != nil {
		return nil, err
	}
	rate, _ := strconv.ParseFloat(px.LastFundingRate, 64)

	info := &FundingInfo{NextRate: rate, IntervalHours: 8}
	// Measure the real interval from the last few settlements (some listings
	// settle every 1h or 4h — a fixed ×3/day annualization would understate
	// them by 3-8×).
	var raw []struct {
		FundingTime int64 `json:"fundingTime"`
	}
	if err := fetchJSON(fapiBase()+"/fapi/v1/fundingRate?symbol="+b.symbol+"&limit=4", &raw); err == nil && len(raw) >= 2 {
		gaps := []float64{}
		for i := 1; i < len(raw); i++ {
			h := float64(raw[i].FundingTime-raw[i-1].FundingTime) / 3.6e6
			if h > 0 && h <= 24 {
				gaps = append(gaps, h)
			}
		}
		if len(gaps) > 0 {
			sum := 0.0
			for _, g := range gaps {
				sum += g
			}
			info.IntervalHours = sum / float64(len(gaps))
		}
	}
	return info, nil
}

func (b *binanceDS) FundingHistory(limit int) ([]FundingPoint, error) {
	u := fmt.Sprintf("%s/fapi/v1/fundingRate?symbol=%s&limit=%d", fapiBase(), b.symbol, limit)
	var raw []struct {
		Rate      string `json:"fundingRate"`
		FundingTS int64  `json:"fundingTime"`
	}
	if err := fetchJSON(u, &raw); err != nil {
		return nil, err
	}
	out := make([]FundingPoint, 0, len(raw))
	for _, r := range raw {
		v, _ := strconv.ParseFloat(r.Rate, 64)
		out = append(out, FundingPoint{Rate: v, TS: r.FundingTS})
	}
	return out, nil
}

// fetchKlines downloads and parses a Binance kline payload (futures or spot share the format).
func fetchKlines(baseURL, symbol, interval string, limit int) ([]Kline, error) {
	u := fmt.Sprintf("%s?symbol=%s&interval=%s&limit=%d", baseURL, symbol, interval, limit)
	var raw [][]interface{}
	if err := fetchJSON(u, &raw); err != nil {
		return nil, err
	}
	out := make([]Kline, 0, len(raw))
	for _, r := range raw {
		if len(r) < 11 {
			continue
		}
		out = append(out, Kline{
			OpenTime:     int64(toF(r[0])),
			Open:         toF(r[1]),
			High:         toF(r[2]),
			Low:          toF(r[3]),
			Close:        toF(r[4]),
			Volume:       toF(r[5]),
			QuoteVolume:  toF(r[7]),
			Trades:       int64(toF(r[8])),
			TakerBuyBase: toF(r[9]),
			TakerBuyQty:  toF(r[10]),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no klines returned for %s %s", symbol, interval)
	}
	return out, nil
}

func fetchJSON(rawURL string, out interface{}) error {
	resp, err := binanceHTTP.Get(rawURL)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read body failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode failed: %w", err)
	}
	return nil
}

func toF(v interface{}) float64 {
	switch x := v.(type) {
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	case float64:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

// isASCIIAlnum reports whether s consists only of ASCII letters and digits.
func isASCIIAlnum(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TopVolumeSymbols returns the top N USDT-M perps by 24h quote volume
// (filters out non-USDT and delivery contracts, and dead pairs).
func TopVolumeSymbols(limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}
	u := fapiBase() + "/fapi/v1/ticker/24hr"
	var raw []struct {
		Symbol      string `json:"symbol"`
		QuoteVolume string `json:"quoteVolume"`
	}
	if err := fetchJSON(u, &raw); err != nil {
		return nil, err
	}
	type pair struct {
		sym string
		vol float64
	}
	var pairs []pair
	for _, t := range raw {
		if !strings.HasSuffix(t.Symbol, "USDT") || strings.Contains(t.Symbol, "_") {
			continue
		}
		// Skip non-tradable localized names (e.g. 龙虾USDT): ASCII letters/digits only.
		if !isASCIIAlnum(t.Symbol[:len(t.Symbol)-4]) {
			continue
		}
		v, _ := strconv.ParseFloat(t.QuoteVolume, 64)
		if v < 10_000_000 { // skip illiquid pairs (< $10M/day)
			continue
		}
		pairs = append(pairs, pair{t.Symbol, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].vol > pairs[j].vol })
	out := make([]string, 0, limit)
	for _, p := range pairs {
		out = append(out, p.sym)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// BoardLists derives the three Binance-app leaderboards from one 24hr ticker
// fetch (user directive 2026-09-12, piggy-dash second universe): 热门榜 =
// top trade-count (popularity proxy — no public hot-list API), 涨幅榜 =
// top 24h gain, 跌幅榜 = top 24h loss. Same USDT-perp/ASCII/liquidity
// filters as the volume universe; each board returns at most its N.
func BoardLists(hotN, gainN, loseN int) (hot, gain, lose []string, err error) {
	if hotN <= 0 {
		hotN = 20
	}
	if gainN <= 0 {
		gainN = 20
	}
	if loseN <= 0 {
		loseN = 20
	}
	u := fapiBase() + "/fapi/v1/ticker/24hr"
	var raw []struct {
		Symbol             string `json:"symbol"`
		PriceChangePercent string `json:"priceChangePercent"`
		QuoteVolume        string `json:"quoteVolume"`
		Count              int    `json:"count"` // 24h trade count — the popularity axis
	}
	if err := fetchJSON(u, &raw); err != nil {
		return nil, nil, nil, err
	}
	type row struct {
		symbol string
		chg    float64
		vol    float64
		count  int
	}
	var all []row
	for _, t := range raw {
		if !strings.HasSuffix(t.Symbol, "USDT") || strings.Contains(t.Symbol, "_") {
			continue
		}
		if !isASCIIAlnum(t.Symbol[:len(t.Symbol)-4]) {
			continue
		}
		v, _ := strconv.ParseFloat(t.QuoteVolume, 64)
		if v < 10_000_000 { // same liquidity floor as the volume universe
			continue
		}
		chg, _ := strconv.ParseFloat(t.PriceChangePercent, 64)
		all = append(all, row{t.Symbol, chg, v, t.Count})
	}
	byCount := append([]row(nil), all...)
	sort.Slice(byCount, func(i, j int) bool { return byCount[i].count > byCount[j].count })
	for _, r := range byCount {
		if len(hot) < hotN {
			hot = append(hot, r.symbol)
		}
	}
	byGain := append([]row(nil), all...)
	sort.Slice(byGain, func(i, j int) bool { return byGain[i].chg > byGain[j].chg })
	for _, r := range byGain {
		if len(gain) < gainN {
			gain = append(gain, r.symbol)
		}
	}
	byLose := append([]row(nil), all...)
	sort.Slice(byLose, func(i, j int) bool { return byLose[i].chg < byLose[j].chg })
	for _, r := range byLose {
		if len(lose) < loseN {
			lose = append(lose, r.symbol)
		}
	}
	return hot, gain, lose, nil
}
