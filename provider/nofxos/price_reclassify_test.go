package nofxos

import (
	"strings"
	"testing"
)

func TestReclassifyFreshFlips(t *testing.T) {
	jup := PriceRankingItem{Symbol: "JUPUSDT", PriceDelta: 0.05, Price: 0.2672}
	ray := PriceRankingItem{Symbol: "RAYSOLUSDT", PriceDelta: 0.079}
	mars := PriceRankingItem{Symbol: "MARSCOINUSDT", PriceDelta: -0.0339}
	flock := PriceRankingItem{Symbol: "FLOCKUSDT", PriceDelta: -0.066}

	t.Run("JUP prototype: fresh flip moves the row to the losers board", func(t *testing.T) {
		top, low := reclassifyFreshFlips(
			[]PriceRankingItem{jup, ray},
			[]PriceRankingItem{mars, flock},
			map[string]float64{"JUPUSDT": -2.9059},
			"1h",
		)
		for _, it := range top {
			if it.Symbol == "JUPUSDT" {
				t.Fatal("flipped item must leave the gainers board")
			}
		}
		found := false
		for _, it := range low {
			if it.Symbol == "JUPUSDT" {
				found = true
			}
		}
		if !found {
			t.Fatal("flipped item must land on the losers board")
		}
		if len(top) != 1 || top[0].Symbol != "RAYSOLUSDT" {
			t.Fatalf("unaffected gainers must stay: %+v", top)
		}
	})

	t.Run("reverse flip: fresh positive moves a loser to gainers", func(t *testing.T) {
		top, low := reclassifyFreshFlips(
			[]PriceRankingItem{ray},
			[]PriceRankingItem{mars},
			map[string]float64{"MARSCOINUSDT": 1.4},
			"1h",
		)
		if len(low) != 0 {
			t.Fatalf("losers board should be empty after the flip: %+v", low)
		}
		if len(top) != 2 {
			t.Fatalf("gainers board should hold both rows: %+v", top)
		}
	})

	t.Run("no duplicate when the symbol already sits on both boards", func(t *testing.T) {
		top, low := reclassifyFreshFlips(
			[]PriceRankingItem{jup},
			[]PriceRankingItem{jup, flock},
			map[string]float64{"JUPUSDT": -2.9},
			"1h",
		)
		count := 0
		for _, it := range low {
			if it.Symbol == "JUPUSDT" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected exactly one JUP row on losers, got %d", count)
		}
		if len(top) != 0 {
			t.Fatalf("gainers board must be empty: %+v", top)
		}
	})

	t.Run("non-1h durations pass through untouched", func(t *testing.T) {
		top := []PriceRankingItem{jup}
		outTop, outLow := reclassifyFreshFlips(top, []PriceRankingItem{flock}, map[string]float64{"JUPUSDT": -2.9}, "24h")
		if len(outTop) != 1 || outTop[0].Symbol != "JUPUSDT" {
			t.Fatal("24h board must pass through unchanged")
		}
		if len(outLow) != 1 || outLow[0].Symbol != "FLOCKUSDT" {
			t.Fatal("24h losers must pass through unchanged")
		}
	})

	t.Run("rendered output never shows a negative in 涨幅榜", func(t *testing.T) {
		data := &PriceRankingData{Durations: map[string]*PriceRankingDuration{
			"1h": {Top: []PriceRankingItem{jup}, Low: []PriceRankingItem{mars}},
		}}
		out := FormatPriceRankingForAI(data, LangChinese, map[string]bool{"JUPUSDT": true}, map[string]float64{"JUPUSDT": -2.9059})
		losersIdx := strings.Index(out, "跌幅榜")
		if losersIdx < 0 {
			t.Fatalf("losers board missing from output:\n%s", out)
		}
		if !strings.Contains(out[losersIdx:], "JUPUSDT") {
			t.Fatalf("JUP must render inside 跌幅榜:\n%s", out)
		}
		if g := strings.Index(out, "涨幅榜"); g >= 0 && g < losersIdx {
			if strings.Contains(out[g:losersIdx], "JUPUSDT") {
				t.Fatalf("JUP must not render inside 涨幅榜:\n%s", out[g:losersIdx])
			}
		}
	})
}
