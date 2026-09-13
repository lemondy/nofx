package nofxos

import (
	"strings"
	"testing"
)

// Full rows only for interesting symbols; the rest collapse to headlines.
func TestRankingInterestingFilter(t *testing.T) {
	oi := &OIRankingData{Duration: "1h",
		TopPositions: []OIPosition{
			{Rank: 1, Symbol: "ZENUSDT", OIDeltaValue: 1.38e6, OIDeltaPercent: 8.5},
			{Rank: 2, Symbol: "AAAUSDT", OIDeltaValue: 9e5, OIDeltaPercent: 6.1},
		},
	}
	out := FormatOIRankingForAI(oi, LangChinese, map[string]bool{"AAAUSDT": true})
	if !strings.Contains(out, "AAAUSDT |") || !strings.Contains(out, "← 候选/持仓") {
		t.Fatalf("interesting symbol must get a full row:\n%s", out)
	}
	if !strings.Contains(out, "其余前列: ZENUSDT") {
		t.Fatalf("non-interesting must collapse to headline:\n%s", out)
	}
	if strings.Contains(out, "| ZENUSDT |") {
		t.Fatalf("non-interesting must not get a full row:\n%s", out)
	}

	pr := &PriceRankingData{Durations: map[string]*PriceRankingDuration{
		"1h": {Top: []PriceRankingItem{
			{Symbol: "ZENUSDT", Price: 5, PriceDelta: 0.09},
			{Symbol: "BBBUSDT", Price: 2, PriceDelta: 0.05},
		}},
	}}
	out = FormatPriceRankingForAI(pr, LangChinese, map[string]bool{"BBBUSDT": true}, nil)
	if !strings.Contains(out, "← 候选/持仓") && strings.Contains(out, "| ZENUSDT |") {
		t.Fatalf("non-interesting price row should be collapsed:\n%s", out)
	}
	if !strings.Contains(out, "BBBUSDT") && !strings.Contains(out, "其余前列") {
		t.Fatal("expected interesting row or headline")
	}
}
