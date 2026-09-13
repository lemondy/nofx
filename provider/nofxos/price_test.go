package nofxos

import (
	"strings"
	"testing"
)

// Zero-value flow/OI columns carry no information and read as "nothing
// changed" — they must be dropped entirely from the ranking tables.
func TestRankingDropsZeroColumns(t *testing.T) {
	data := &PriceRankingData{
		Durations: map[string]*PriceRankingDuration{
			"1h": { // no flow, no OI anywhere
				Top: []PriceRankingItem{{Symbol: "AAAUSDT", Price: 1, PriceDelta: 0.05}, {Symbol: "BBBUSDT", Price: 1, PriceDelta: 0.03}},
				Low: []PriceRankingItem{{Symbol: "CCCUSDT", Price: 1, PriceDelta: -0.04}},
			},
		},
	}
	out := formatPriceRankingZH(data, nil, nil)
	// Column headers must be gone (the interpretive footer mention is fine).
	if strings.Contains(out, "| 资金流 |") || strings.Contains(out, "OI变化 |") {
		t.Fatalf("zero-value columns must be dropped:\n%s", out)
	}
	// EN renderer too.
	outEN := formatPriceRankingEN(data, nil, nil)
	if strings.Contains(outEN, "Fund Flow |") || strings.Contains(outEN, "OI Change |") {
		t.Fatalf("EN renderer keeps zero-value columns:\n%s", outEN)
	}
}

// An interesting row sitting AFTER 3+ non-interesting ones must not corrupt
// the table: full table first, headline on its own line, no "++" doubles.
func TestRankingStructureWithLateInteresting(t *testing.T) {
	oi := &OIRankingData{Duration: "1h",
		TopPositions: []OIPosition{
			{Rank: 1, Symbol: "TSTUSDT", OIDeltaValue: 1e5, OIDeltaPercent: 134834.6},
			{Rank: 2, Symbol: "4USDT", OIDeltaValue: 2e5, OIDeltaPercent: 414363.8},
			{Rank: 3, Symbol: "TUTUSDT", OIDeltaValue: 3e5, OIDeltaPercent: 293404.9},
			{Rank: 4, Symbol: "BRUSDT", OIDeltaValue: -3.44e6, OIDeltaPercent: -10.47},
		},
	}
	out := FormatOIRankingForAI(oi, LangChinese, map[string]bool{"BRUSDT": true})
	lines := strings.Split(out, "\n")
	headerIdx, rowIdx := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "| 排名 |") {
			headerIdx = i
		}
		if strings.Contains(l, "BRUSDT") && strings.Contains(l, "← 候选/持仓") {
			rowIdx = i
		}
	}
	if headerIdx == -1 || rowIdx == -1 || rowIdx != headerIdx+2 { // header, separator, row
		t.Fatalf("table broken:\n%s", out)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "其余前列:") {
			if strings.Contains(l, "|") {
				t.Fatalf("headline must be on its own line:\n%s", out)
			}
			if strings.Contains(l, "++") {
				t.Fatalf("double sign in headline:\n%s", out)
			}
		}
	}
}

// Full-row (interesting) symbols get THIS cycle's fresh 60m change instead of
// the stale snapshot; rows without OI data render an explicit dash, never a
// fake +0.00.
func TestPriceRankingFreshOverrideAndOIDash(t *testing.T) {
	pr := &PriceRankingData{Durations: map[string]*PriceRankingDuration{
		"1h": {Top: []PriceRankingItem{
			{Symbol: "ASTERUSDT", Price: 0.82, PriceDelta: -0.0439}, // stale snapshot: -4.39%
			{Symbol: "DASHUSDT", Price: 69.4, PriceDelta: -0.0202, OIDeltaValue: 5e5, OIDelta: 0.012},
		}},
	}}
	fresh := map[string]float64{"ASTERUSDT": -0.77} // this cycle's live value
	out := FormatPriceRankingForAI(pr, LangChinese, map[string]bool{"ASTERUSDT": true, "DASHUSDT": true}, fresh)
	if !strings.Contains(out, "-0.77%") {
		t.Fatalf("fresh 60m override not applied:\n%s", out)
	}
	if strings.Contains(out, "-4.39%") {
		t.Fatalf("stale snapshot value must be overridden for full rows:\n%s", out)
	}
	// DASH has real OI data → value shown; a hypothetical zero-OI row shows dash.
	if !strings.Contains(out, "500") {
		t.Fatalf("real OI value must render:\n%s", out)
	}
	// All rows missing OI → the whole column is dropped (best case).
	prZero := &PriceRankingData{Durations: map[string]*PriceRankingDuration{
		"1h": {Top: []PriceRankingItem{{Symbol: "AKEUSDT", Price: 0.02, PriceDelta: 0.0697}}},
	}}
	out = FormatPriceRankingForAI(prZero, LangChinese, map[string]bool{"AKEUSDT": true}, nil)
	if strings.Contains(out, "+0.00 |") || strings.Contains(out, "OI变化") {
		t.Fatalf("all-missing OI column must be dropped:\n%s", out)
	}

	// Mixed column: rows WITH data show values, rows WITHOUT show an explicit
	// dash — never a fake +0.00 next to real numbers.
	prMixed := &PriceRankingData{Durations: map[string]*PriceRankingDuration{
		"1h": {Top: []PriceRankingItem{
			{Symbol: "DASHUSDT", Price: 69.4, PriceDelta: -0.0202, OIDeltaValue: 5e5, OIDelta: 0.012},
			{Symbol: "AKEUSDT", Price: 0.02, PriceDelta: 0.0697},
		}},
	}}
	out = FormatPriceRankingForAI(prMixed, LangChinese, map[string]bool{"DASHUSDT": true, "AKEUSDT": true}, nil)
	if !strings.Contains(out, "— |") {
		t.Fatalf("missing-OI row in a mixed column must render a dash:\n%s", out)
	}
	if strings.Contains(out, "+0.00 |") {
		t.Fatalf("fake zero OI cell:\n%s", out)
	}
}

// Same structure guarantee for the price tables: an interesting row after 3+
// non-interesting ones must not let the headline text touch the table.
func TestPriceRankingStructureWithLateInteresting(t *testing.T) {
	pr := &PriceRankingData{Durations: map[string]*PriceRankingDuration{
		"1h": {Top: []PriceRankingItem{
			{Symbol: "MUBARAKUSDT", Price: 1, PriceDelta: 0.08},
			{Symbol: "MARSCOINUSDT", Price: 1, PriceDelta: 0.06},
			{Symbol: "VELVETUSDT", Price: 1, PriceDelta: 0.056},
			{Symbol: "4USDT", Price: 0.0283, PriceDelta: 0.0163},
			{Symbol: "CATIUSDT", Price: 1, PriceDelta: 0.031},
		}},
	}}
	out := FormatPriceRankingForAI(pr, LangChinese, map[string]bool{"4USDT": true}, map[string]float64{"4USDT": 1.63})
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "其余前列:") {
			if strings.Contains(l, "|") || strings.Contains(l, "4USDT |") {
				t.Fatalf("headline merged with table:\n%s", out)
			}
			if !strings.HasPrefix(lines[i+1], "\n") && lines[i+1] != "" && !strings.HasPrefix(lines[i+1], "**解读") && i+1 != len(lines)-1 {
				// headline line must end its own line (next line blank or section end)
				t.Fatalf("headline not on its own line (next: %q)", lines[i+1])
			}
		}
	}
	if !strings.Contains(out, "| 4USDT | +1.63% |") {
		t.Fatalf("interesting row missing from table:\n%s", out)
	}
	// The table row must be preceded by the header on separate lines.
	if idx := strings.Index(out, "| 4USDT |"); idx >= 0 {
		before := out[:idx]
		if !strings.Contains(before, "|------|") {
			t.Fatalf("row without separator header above:\n%s", out)
		}
	}
}
