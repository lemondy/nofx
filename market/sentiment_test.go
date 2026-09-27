package market

import (
	"strings"
	"testing"
)

// Pins for the sentiment composite (user directive 2026-09-27):
// render includes all sources + the absent-source note; empty when all failed.
func TestMarketSentimentRender(t *testing.T) {
	m := &MarketSentiment{
		Crypto:  &CryptoFG{Value: 70, Classification: "Greed", YesterdayValue: 74},
		Stock:   &StockFG{Score: 64, Components: []StockFGComponent{{Name: "VOLATILITY", Val: 58, Wt: 25}}},
		Binance: &BinanceCrowd{BTCLS: 1.28, BTCAFunding: 0.000057},
	}
	out := m.Render()
	for _, want := range []string{"70 Greed", "较昨日回落", "CNN 口径): 64", "BTC 多空账户比 1.28", "年化约 +6.2%"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}

	// All sources failed → the block disappears (no empty shell).
	empty := &MarketSentiment{Notes: []string{"crypto_fg_fetch_failed"}}
	if empty.Render() != "" {
		t.Fatalf("all-failed composite must render empty, got %q", empty.Render())
	}

	// Partial failure discloses the missing source.
	partial := &MarketSentiment{Crypto: m.Crypto, Notes: []string{"stock_fg_fetch_failed"}}
	if !strings.Contains(partial.Render(), "美股情绪缺数") {
		t.Fatal("partial failure must disclose the absent source")
	}
}
