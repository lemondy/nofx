package nofxos

import (
	"strings"
	"testing"
)

// The fund-flow tables follow the same two-pass rule: an interesting row
// after 3+ non-interesting ones must keep the markdown intact.
func TestNetFlowStructureWithLateInteresting(t *testing.T) {
	data := &NetFlowRankingData{Duration: "1h",
		InstitutionFutureTop: []NetFlowPosition{
			{Rank: 1, Symbol: "TSTUSDT", Amount: 1e6},
			{Rank: 2, Symbol: "4USDT", Amount: 8e5},
			{Rank: 3, Symbol: "TUTUSDT", Amount: 6e5},
			{Rank: 4, Symbol: "SOLUSDT", Amount: 2.02e6},
		},
	}
	out := FormatNetFlowRankingForAI(data, LangChinese, map[string]bool{"SOLUSDT": true})
	if !strings.Contains(out, "SOLUSDT | +2.02M") {
		t.Fatalf("interesting flow row missing:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "其余前列:") && strings.Contains(l, "|") {
			t.Fatalf("headline merged with table:\n%s", out)
		}
	}
	// EN too.
	outEN := FormatNetFlowRankingForAI(data, LangEnglish, map[string]bool{"SOLUSDT": true})
	if strings.Contains(outEN, "Others in the lead:") && strings.Contains(outEN[strings.Index(outEN, "Others in the lead:"):], "| Rank |") {
		t.Fatalf("EN headline merged with table:\n%s", outEN)
	}
	if !strings.Contains(outEN, "SOLUSDT") {
		t.Fatalf("EN interesting row missing:\n%s", outEN)
	}
}
