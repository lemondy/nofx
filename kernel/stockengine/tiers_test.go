package stockengine

import (
	"math"
	"strings"
	"testing"
	"time"

	"nofx/market/usstock"
	"nofx/store"
)

func TestClassifyAndMatchSizeTier(t *testing.T) {
	if got := ClassifySizeTier("ETF", 1_000); got != TierETF {
		t.Fatalf("ETF = %q, want %q", got, TierETF)
	}
	if got := ClassifySizeTier("etf", 0); got != TierETF {
		t.Fatalf("etf with 0 adv = %q, want %q", got, TierETF)
	}
	if got := ClassifySizeTier("stock", 500_000_000); got != TierMegaLarge {
		t.Fatalf("500M = %q, want %q", got, TierMegaLarge)
	}
	if got := ClassifySizeTier("stock", 499_999_999); got != TierMidSmall {
		t.Fatalf("<500M = %q, want %q", got, TierMidSmall)
	}
	for _, adv := range []float64{0, -100, math.NaN(), math.Inf(1)} {
		if got := ClassifySizeTier("stock", adv); got != "" {
			t.Fatalf("adv=%v gave %q, want empty", adv, got)
		}
	}

	if !MatchesSizeTier("", TierMidSmall, "stock") || !MatchesSizeTier("all", TierMidSmall, "stock") {
		t.Fatal("empty/all filter must match everything")
	}
	if !MatchesSizeTier(TierMegaLarge, TierMegaLarge, "stock") || !MatchesSizeTier(TierMegaLarge, TierETF, "ETF") || !MatchesSizeTier(TierMegaLarge, "", "ETF") {
		t.Fatal("mega_large filter must match mega_large and ETFs")
	}
	if MatchesSizeTier(TierMegaLarge, TierMidSmall, "stock") {
		t.Fatal("mega_large filter must exclude mid_small")
	}
	if !MatchesSizeTier(TierMidSmall, TierMidSmall, "stock") {
		t.Fatal("mid_small filter must match mid_small stock")
	}
	if MatchesSizeTier(TierMidSmall, TierMegaLarge, "stock") || MatchesSizeTier(TierMidSmall, TierETF, "ETF") || MatchesSizeTier(TierMidSmall, TierMidSmall, "ETF") {
		t.Fatal("mid_small filter must exclude mega_large and ETFs")
	}
}

func TestFilterBySizeTier(t *testing.T) {
	orig := &ScanReport{
		Results: []ScanResult{
			{Symbol: "AAPLBUSDT", Universe: UniverseRow{Class: "stock", AvgDollarVol20: 1e9, Tier: TierMegaLarge}},
			{Symbol: "SPYBUSDT", Universe: UniverseRow{Class: "ETF", AvgDollarVol20: 4e9, Tier: TierETF}},
			{Symbol: "SMOLBUSDT", Universe: UniverseRow{Class: "stock", AvgDollarVol20: 5e6, Tier: TierMidSmall}},
			{Symbol: "UNKBUSDT", Universe: UniverseRow{Class: "stock"}},
		},
	}
	if got := orig.FilterBySizeTier(""); got != orig {
		t.Fatal("empty tier filter should return original pointer untouched")
	}
	if got := orig.FilterBySizeTier("all"); got != orig {
		t.Fatal("all tier filter should return original pointer untouched")
	}

	mega := orig.FilterBySizeTier(TierMegaLarge)
	if mega == orig || len(mega.Results) != 2 || mega.Results[0].Symbol != "AAPLBUSDT" || mega.Results[1].Symbol != "SPYBUSDT" {
		t.Fatalf("mega_large filtered = %+v", mega)
	}
	if len(orig.Results) != 4 {
		t.Fatal("original ScanReport must not be mutated")
	}

	mid := orig.FilterBySizeTier(TierMidSmall)
	if len(mid.Results) != 1 || mid.Results[0].Symbol != "SMOLBUSDT" {
		t.Fatalf("mid_small filtered = %+v", mid)
	}

	var nilReport *ScanReport
	if nilReport.FilterBySizeTier(TierMegaLarge) != nil {
		t.Fatal("nil report must stay nil")
	}
}

func TestBuildUniverseRowAndApplyFundamentalsAssignTier(t *testing.T) {
	// Close=1000, Volume=600_000 -> ADV20 = 600M -> mega_large
	megaBars := make([]usstock.Bar, 40)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range megaBars {
		megaBars[i] = usstock.Bar{OpenTime: start.AddDate(0, 0, i), Open: 1000, High: 1001, Low: 999, Close: 1000, Volume: 600_000}
	}
	row, ok := BuildUniverseRow("NVDAUSDT", "NVDA", megaBars)
	if !ok || row.Tier != TierMegaLarge {
		t.Fatalf("NVDA row = %+v ok=%v", row, ok)
	}
	// When fundamentals reveal it is an ETF, tier flips to etf.
	ApplyFundamentals(&row, usstock.Fundamentals{QuoteType: "ETF"}, time.Now())
	if row.Tier != TierETF {
		t.Fatalf("after ETF fundamentals, tier = %q, want %q", row.Tier, TierETF)
	}

	// SPY static ETF detection
	spyRow, ok := BuildUniverseRow("SPYBUSDT", "SPY", megaBars)
	if !ok || spyRow.Tier != TierETF {
		t.Fatalf("SPY row = %+v ok=%v", spyRow, ok)
	}

	// Mid/small stock (<500M ADV20)
	smallBars := universeTestBars(40, func(i int) float64 { return 50 }) // 50 * 1000 = 50k/day
	smallRow, ok := BuildUniverseRow("SMOLBUSDT", "SMOL", smallBars)
	if !ok || smallRow.Tier != TierMidSmall {
		t.Fatalf("SMOL row = %+v ok=%v", smallRow, ok)
	}
}

func TestPromptTierColumnAndDiscipline(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		scanCfg := &store.StockConfig{SymbolSource: store.StockSymbolSourceScan}
		sysScan := BuildSystemPrompt(scanCfg, ResolvePreset(scanCfg), lang)
		for _, tok := range []string{"mega_large", "mid_small", "adv20$"} {
			if !strings.Contains(sysScan, tok) {
				t.Fatalf("%s scan system prompt missing %q", lang, tok)
			}
		}

		shortCfg := &store.StockConfig{ShortMode: store.StockShortPaper}
		sysShort := BuildSystemPrompt(shortCfg, ResolvePreset(shortCfg), lang)
		for _, tok := range []string{"mega_large", "mid_small", "adv20$"} {
			if !strings.Contains(sysShort, tok) {
				t.Fatalf("%s short system prompt missing %q", lang, tok)
			}
		}

		ctx := universeTestContext(lang == "zh")
		user := BuildUserPrompt(ctx, lang)
		if !strings.Contains(user, "|phase(Kell)|tier\n") && !strings.Contains(user, "|phase|shortable|short_setup|tier\n") {
			t.Fatalf("%s user universe header missing trailing |tier:\n%s", lang, user)
		}
		if !strings.Contains(user, "|wedge_pop|mid_small\n") || !strings.Contains(user, "|ema_crossback|mega_large\n") {
			t.Fatalf("%s user universe rows missing expected tier suffix:\n%s", lang, user)
		}

		// Short-mode universe table also appends |tier at the end.
		ctx.Config.ShortMode = store.StockShortPaper
		ctx.Paper = true
		shortUser := BuildUserPrompt(ctx, lang)
		if !strings.Contains(shortUser, "phase|shortable|short_setup|tier\n") {
			t.Fatalf("%s short user universe header missing |tier:\n%s", lang, shortUser)
		}
		if !strings.Contains(shortUser, "|wedge_pop|false|false|mid_small\n") {
			t.Fatalf("%s short user universe row missing |tier:\n%s", lang, shortUser)
		}
	}
}
