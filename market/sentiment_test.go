package market

import (
	"strings"
	"testing"
)

// Pins for the sentiment composite (user directive 2026-09-27):
// render includes all sources + the absent-source note; empty when all failed.
func TestMarketSentimentRender(t *testing.T) {
	m := &MarketSentiment{
		Crypto:  &CryptoFG{Value: 70, Classification: "Greed", YesterdayValue: 74, SourceAt: "2026-09-27"},
		Stock:   &StockFG{Score: 64, Components: []StockFGComponent{{Name: "VOLATILITY", Val: 58, Wt: 25}}, MarketDate: "2026-09-25"},
		Binance: &BinanceCrowd{BTCLS: 1.28, BTCAFunding: 0.000057, RatioPeriodEnd: "2026-09-27 05:00 UTC", FundingAt: "2026-09-27 06:20 UTC"},
		FetchedAt: "2026-09-27 06:21 UTC",
	}
	out := m.Render()
	for _, want := range []string{"70 Greed", "较昨日回落", "FearGreedChart 独立模型,非CNN官方): 64", "BTC 多空账户比 1.28", "年化约 +6.2%",
		// Per-source as-of stamps + composite fetch time — never time.Now().
		"[数据日 2026-09-27]", "[数据日 2026-09-25 收盘]", "多空比窗口=2026-09-27 05:00 UTC", "funding=2026-09-27 06:20 UTC", "快照拉取",
		// Program regime enums (70/64 greed side, funding 6.2% ann + LS 1.28 → not crowded).
		"sentiment_regime: GREED_NOT_LEVERAGE_CROWDED", "sentiment_trade_effect: CONTEXT_ONLY"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "数据截至: ") {
		t.Errorf("render must not stamp a bare 数据截至 now-line:\n%s", out)
	}
	if strings.Contains(out, "CNN 口径") {
		t.Errorf("CNN-口径 label must stay gone:\n%s", out)
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

// Regime classification pins (user review 2026-09-27): extreme needs BOTH
// macro sources; crowding = |funding ann| ≥50% or L/S >2; effects map to the
// prompt's three combine rules.
func TestClassifySentimentRegime(t *testing.T) {
	cases := []struct {
		name           string
		m              *MarketSentiment
		regime, effect string
	}{
		{
			name: "greed, not crowded (2026-09-27 live shape)",
			m: &MarketSentiment{
				Crypto:  &CryptoFG{Value: 70},
				Stock:   &StockFG{Score: 64},
				Binance: &BinanceCrowd{BTCLS: 1.27, BTCAFunding: 0.0000366, ETHFunding: 0.0001},
			},
			regime: "GREED_NOT_LEVERAGE_CROWDED", effect: "CONTEXT_ONLY",
		},
		{
			name: "all-three extreme + crowded → conservative opens",
			m: &MarketSentiment{
				Crypto:  &CryptoFG{Value: 85},
				Stock:   &StockFG{Score: 80},
				Binance: &BinanceCrowd{BTCLS: 2.4, BTCAFunding: 0.0006},
			},
			regime: "EXTREME_GREED_LEVERAGE_CROWDED", effect: "CONSERVATIVE_OPENS",
		},
		{
			name: "divergent sources → structure wins",
			m: &MarketSentiment{
				Crypto:  &CryptoFG{Value: 85},
				Stock:   &StockFG{Score: 15},
				Binance: &BinanceCrowd{BTCLS: 1.0},
			},
			regime: "DIVERGENT_NOT_LEVERAGE_CROWDED", effect: "STRUCTURE_WINS",
		},
		{
			name: "fear side with negative funding crowding",
			m: &MarketSentiment{
				Crypto:  &CryptoFG{Value: 18},
				Stock:   &StockFG{Score: 22},
				Binance: &BinanceCrowd{BTCAFunding: -0.0006},
			},
			regime: "EXTREME_FEAR_LEVERAGE_CROWDED", effect: "CONTEXT_ONLY",
		},
		{
			name:   "binance absent → crowding unknown is disclosed",
			m:      &MarketSentiment{Crypto: &CryptoFG{Value: 70}, Stock: &StockFG{Score: 64}},
			regime: "GREED_CROWDING_UNKNOWN", effect: "CONTEXT_ONLY",
		},
		{
			name:   "neutral macro",
			m:      &MarketSentiment{Crypto: &CryptoFG{Value: 50}, Stock: &StockFG{Score: 50}, Binance: &BinanceCrowd{BTCLS: 1.0}},
			regime: "NEUTRAL", effect: "CONTEXT_ONLY",
		},
	}
	for _, tc := range cases {
		regime, effect := classifySentimentRegime(tc.m)
		if regime != tc.regime || effect != tc.effect {
			t.Errorf("%s: got (%s, %s), want (%s, %s)", tc.name, regime, effect, tc.regime, tc.effect)
		}
	}
}

// Funding annualization for the crowding check: raw per-interval decimals.
func TestFundingAnnualizedCrowdingThreshold(t *testing.T) {
	// 0.000325 per 8h ≈ 35.6% annualized — below the 50% crowding line.
	if fundingAnnualized(0.000325) >= 50 {
		t.Fatalf("0.000325 should annualize below 50, got %.1f", fundingAnnualized(0.000325))
	}
	// 0.00046 per 8h ≈ 50.4% — above the line.
	if fundingAnnualized(0.00046) <= 50 {
		t.Fatalf("0.00046 should annualize above 50, got %.1f", fundingAnnualized(0.00046))
	}
}
