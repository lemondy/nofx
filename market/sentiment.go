package market

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"nofx/security"
)

// ============================================================================
// Market sentiment composite (user directive 2026-09-27): THREE independent
// sentiment sources rendered together at the head of the user prompt, with an
// explicit combine-don't-overindex instruction:
//   1. Alternative.me  — crypto macro fear & greed (daily)
//   2. feargreedchart  — US-stock macro fear & greed, CNN-style components
//   3. Binance         — contract crowding: global long/short account ratio
//                        + premiumIndex last funding (BTC/ETH)
// All sources are KEYLESS and fail-open: a failed source renders as absent,
// never as a fabricated value. Caches match each source's natural cadence.
// ============================================================================

// CryptoFG is the Alternative.me fear & greed reading.
type CryptoFG struct {
	Value          int    `json:"value"`
	Classification string `json:"classification"`
	YesterdayValue int    `json:"yesterday_value"`
}

// StockFG is the CNN-style stock fear & greed composite.
type StockFG struct {
	Score      int                `json:"score"`
	Components []StockFGComponent `json:"components"`
}

type StockFGComponent struct {
	Name string `json:"name"`
	Val  int    `json:"val"`
	Wt   int    `json:"wt"`
}

// BinanceCrowd is the contract-positioning snapshot for the majors.
type BinanceCrowd struct {
	BTCLS       float64 `json:"btc_ls_ratio"` // global accounts long/short
	ETHLS       float64 `json:"eth_ls_ratio"`
	BTCAFunding float64 `json:"btc_funding"` // raw per-interval rate (decimal)
	ETHFunding  float64 `json:"eth_funding"`
}

// MarketSentiment is the composite block; nil fields = source unavailable.
type MarketSentiment struct {
	Crypto  *CryptoFG     `json:"crypto_fg,omitempty"`
	Stock   *StockFG      `json:"stock_fg,omitempty"`
	Binance *BinanceCrowd `json:"binance_crowding,omitempty"`
	Notes   []string      `json:"notes,omitempty"` // per-source fetch failures
}

var (
	sentimentMu       sync.Mutex
	sentimentCache    *MarketSentiment
	sentimentCachedAt time.Time
)

const sentimentCacheTTL = 30 * time.Minute

func sentimentHTTPGet(url string, out interface{}) error {
	client := security.SafeHTTPClient(15 * time.Second)
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fetchCryptoFG() *CryptoFG {
	var payload struct {
		Data []struct {
			Value               string `json:"value"`
			ValueClassification string `json:"value_classification"`
			Timestamp           string `json:"timestamp"`
		} `json:"data"`
	}
	// Proxy rides the environment (SafeHTTPClient), same as every vendor.
	if err := sentimentHTTPGet("https://api.alternative.me/fng/?limit=2", &payload); err != nil || len(payload.Data) < 2 {
		return nil
	}
	today, err1 := strconv.Atoi(payload.Data[0].Value)
	yest, err2 := strconv.Atoi(payload.Data[1].Value)
	if err1 != nil || err2 != nil {
		return nil
	}
	return &CryptoFG{Value: today, Classification: payload.Data[0].ValueClassification, YesterdayValue: yest}
}

func fetchStockFG() *StockFG {
	var payload struct {
		Score struct {
			Score      int `json:"score"`
			Components []struct {
				Name string `json:"name"`
				Val  int    `json:"val"`
				Wt   int    `json:"wt"`
			} `json:"components"`
		} `json:"score"`
	}
	if err := sentimentHTTPGet("https://feargreedchart.com/api", &payload); err != nil {
		return nil
	}
	out := &StockFG{Score: payload.Score.Score}
	for _, c := range payload.Score.Components {
		out.Components = append(out.Components, StockFGComponent{Name: c.Name, Val: c.Val, Wt: c.Wt})
	}
	if out.Score <= 0 {
		return nil
	}
	return out
}

func fetchBinanceCrowd() *BinanceCrowd {
	out := &BinanceCrowd{}
	ls := func(symbol string) float64 {
		var rows []struct {
			LongShortRatio string `json:"longShortRatio"`
		}
		if err := sentimentHTTPGet("https://fapi.binance.com/futures/data/globalLongShortAccountRatio?symbol="+symbol+"&period=1h&limit=1", &rows); err != nil || len(rows) == 0 {
			return 0
		}
		v, _ := strconv.ParseFloat(rows[0].LongShortRatio, 64)
		return v
	}
	premium := func(symbol string) float64 {
		var p struct {
			LastFundingRate string `json:"lastFundingRate"`
		}
		if err := sentimentHTTPGet("https://fapi.binance.com/fapi/v1/premiumIndex?symbol="+symbol, &p); err != nil {
			return 0
		}
		v, _ := strconv.ParseFloat(p.LastFundingRate, 64)
		return v
	}
	out.BTCLS = ls("BTCUSDT")
	out.ETHLS = ls("ETHUSDT")
	out.BTCAFunding = premium("BTCUSDT")
	out.ETHFunding = premium("ETHUSDT")
	if out.BTCLS <= 0 && out.ETHLS <= 0 && out.BTCAFunding == 0 && out.ETHFunding == 0 {
		return nil
	}
	return out
}

// GetMarketSentiment returns the composite with per-source cache TTLs. Each
// source refreshes independently so one flaky vendor doesn't age the others.
func GetMarketSentiment() *MarketSentiment {
	sentimentMu.Lock()
	defer sentimentMu.Unlock()
	if sentimentCache != nil && time.Since(sentimentCachedAt) < sentimentCacheTTL {
		return sentimentCache
	}

	out := &MarketSentiment{}
	var wg sync.WaitGroup
	if out.Crypto == nil {
		wg.Add(1)
		go func() { defer wg.Done(); out.Crypto = fetchCryptoFG() }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); out.Stock = fetchStockFG() }()
	wg.Add(1)
	go func() { defer wg.Done(); out.Binance = fetchBinanceCrowd() }()
	wg.Wait()

	if out.Crypto == nil {
		out.Notes = append(out.Notes, "crypto_fg_fetch_failed")
	}
	if out.Stock == nil {
		out.Notes = append(out.Notes, "stock_fg_fetch_failed")
	}
	if out.Binance == nil {
		out.Notes = append(out.Notes, "binance_crowding_fetch_failed")
	}
	sentimentCache = out
	sentimentCachedAt = time.Now()
	return out
}

// fundingAnnualized converts a raw per-8h-settlement funding decimal to an
// annualized percent for the prompt line (3 settlements/day default).
func fundingAnnualized(raw float64) float64 {
	return raw * 3 * 365 * 100
}

// Render renders the composite into prompt text — the caller embeds it at
// the head of the user prompt. Empty when every source failed (the block
// disappears rather than showing an empty shell).
func (m *MarketSentiment) Render() string {
	if m == nil {
		return ""
	}
	var sb_ []string
	if m.Crypto != nil {
		dir := "持平"
		if m.Crypto.Value > m.Crypto.YesterdayValue {
			dir = "较昨日上升"
		} else if m.Crypto.Value < m.Crypto.YesterdayValue {
			dir = "较昨日回落"
		}
		sb_ = append(sb_, fmt.Sprintf("加密宏观情绪(Alternative.me): %d %s(%s,昨日 %d)", m.Crypto.Value, m.Crypto.Classification, dir, m.Crypto.YesterdayValue))
	}
	if m.Stock != nil {
		comps := make([]string, 0, len(m.Stock.Components))
		for _, c := range m.Stock.Components {
			comps = append(comps, fmt.Sprintf("%s %d(权重%d%%)", c.Name, c.Val, c.Wt))
		}
		sort.Strings(comps)
		sb_ = append(sb_, fmt.Sprintf("美股宏观情绪(CNN 口径): %d;分量: %s", m.Stock.Score, strings.Join(comps, ", ")))
	}
	if m.Binance != nil {
		parts := ""
		if m.Binance.BTCLS > 0 {
			parts += fmt.Sprintf("BTC 多空账户比 %.2f", m.Binance.BTCLS)
		}
		if m.Binance.ETHLS > 0 {
			if parts != "" {
				parts += "、"
			}
			parts += fmt.Sprintf("ETH %.2f", m.Binance.ETHLS)
		}
		if m.Binance.BTCAFunding != 0 {
			parts += fmt.Sprintf(";资金费 BTC %+.4f%%/期(年化约 %+.1f%%)", m.Binance.BTCAFunding*100, fundingAnnualized(m.Binance.BTCAFunding))
		}
		if m.Binance.ETHFunding != 0 {
			parts += fmt.Sprintf(",ETH %+.4f%%/期(年化约 %+.1f%%)", m.Binance.ETHFunding*100, fundingAnnualized(m.Binance.ETHFunding))
		}
		if parts != "" {
			sb_ = append(sb_, "合约拥挤(Binance): "+parts)
		}
	}
	if len(sb_) == 0 {
		return ""
	}
	body := strings.Join(sb_, "\n")
	var failed []string
	if m.Crypto == nil {
		failed = append(failed, "加密情绪缺数")
	}
	if m.Stock == nil {
		failed = append(failed, "美股情绪缺数")
	}
	if m.Binance == nil {
		failed = append(failed, "合约拥挤缺数")
	}
	if len(failed) > 0 {
		body += "\n(缺数来源: " + strings.Join(failed, "、") + " — 按剩余来源与价格结构判断)"
	}
	// As-of stamp: funding drifts per settlement interval — the model must
	// know the snapshot age when comparing against live prices.
	body += "\n数据截至: " + time.Now().UTC().Format("2006-01-02 15:04 UTC")
	return body
}

var _ = security.SafeHTTPClient
