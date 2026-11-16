package trader

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"nofx/kernel/stockengine"
	"nofx/market/usstock"
	"nofx/store"
)

func fqWriteTierDailyJSON(t *testing.T, dir, filename string, start time.Time, n int, closePx, vol float64, futureBoost bool) {
	t.Helper()
	rows := make([][]any, 0, n+5)
	for i := 0; i < n; i++ {
		d := start.AddDate(0, 0, i).Format(factorDateFmt)
		rows = append(rows, []any{d, closePx, closePx * 1.01, closePx * 0.99, closePx, vol})
	}
	if futureBoost {
		// Future bars after as_of with huge volume must be ignored (no lookahead).
		for i := 0; i < 5; i++ {
			d := start.AddDate(0, 0, n+i).Format(factorDateFmt)
			rows = append(rows, []any{d, closePx, closePx * 1.01, closePx * 0.99, closePx, vol * 10000})
		}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), raw, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestFactorQuantSizeTierGuardrailsUSAndCrypto(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	n := 70
	asOf := start.AddDate(0, 0, n-1).Format(factorDateFmt)

	t.Run("us_thresholds_and_no_lookahead", func(t *testing.T) {
		root := t.TempDir()
		dailyDir := filepath.Join(root, "daily")
		// SPY benchmark (uptrend)
		fqWriteDailyJSON(t, dailyDir, "SPY.json", start, fqSeriesLinear(n, 400, 1))
		// Mega/large US ($600M ADV20 >= $500M)
		fqWriteTierDailyJSON(t, dailyDir, "AAPL.json", start, n, 200, 3_000_000, false)
		// Mid/small US ($200M ADV20 < $500M), with huge future volume after asOf (must NOT leak)
		fqWriteTierDailyJSON(t, dailyDir, "SMOL.json", start, n, 50, 4_000_000, true)
		// Mid/small short ($100M ADV20)
		fqWriteTierDailyJSON(t, dailyDir, "TINY.json", start, n, 25, 4_000_000, false)

		base := &quantTargets{
			Version: 1,
			Market:  store.FactorQuantMarketUS,
			Status:  quantStatusOK,
			AsOf:    asOf,
			Weights: map[string]float64{
				"AAPLBUSDT": 0.40,
				"SMOLBUSDT": 0.25,
				"TINYBUSDT": -0.15,
				"NOADVUSDT": -0.10, // missing daily file
			},
		}
		targetsPath := fqWriteTargetsMap(t, root, store.FactorQuantMarketUS, asOf, 3, base.Weights, map[string]float64{}, nil)

		// 1. Default config: zero mutation parity + accurate tier diagnostics.
		fFalse := false
		defCfg := &store.FactorQuantConfig{
			Market:     store.FactorQuantMarketUS,
			RegimeTilt: &fFalse,
		}
		gotDef, infoDef := fqResolveRegimeTargets(base, defCfg, targetsPath)
		if gotDef != base || !reflect.DeepEqual(gotDef.Weights, base.Weights) {
			t.Fatalf("default config mutated targets: got=%+v", gotDef.Weights)
		}
		if infoDef.SizeTierFilter != "all" || math.Abs(infoDef.MegaLargeGross-0.40) > 1e-6 || math.Abs(infoDef.MidSmallGross-0.40) > 1e-6 {
			t.Fatalf("default tier diagnostics wrong: %+v", infoDef)
		}
		st := infoDef.toStatus()
		if st.SizeTierFilter != "all" || math.Abs(st.MegaLargeGross-0.40) > 1e-6 || math.Abs(st.MidSmallGross-0.40) > 1e-6 {
			t.Fatalf("status propagation wrong: %+v", st)
		}
		if sum := infoDef.summaryText(); !strings.Contains(sum, "分层敞口 大盘 40.0% / 中小盘 40.0%") {
			t.Fatalf("summary missing tier split: %s", sum)
		}

		// 2. SizeTier = mega_large: drops mid_small and unknown-ADV symbols.
		megaCfg := &store.FactorQuantConfig{
			Market:     store.FactorQuantMarketUS,
			RegimeTilt: &fFalse,
			SizeTier:   store.FactorQuantSizeTierMegaLarge,
		}
		gotMega, infoMega := fqResolveRegimeTargets(base, megaCfg, targetsPath)
		if !reflect.DeepEqual(gotMega.Weights, map[string]float64{"AAPLBUSDT": 0.40}) {
			t.Fatalf("mega_large weights = %+v", gotMega.Weights)
		}
		if infoMega.LongCount != 1 || infoMega.ShortCount != 0 || math.Abs(infoMega.MegaLargeGross-0.40) > 1e-6 || infoMega.MidSmallGross != 0 {
			t.Fatalf("mega_large info = %+v", infoMega)
		}
		if !strings.Contains(infoMega.summaryText(), "[tier=mega_large]") {
			t.Fatalf("summary missing active tier tag: %s", infoMega.summaryText())
		}

		// 3. SizeTier = mid_small: keeps only mid_small.
		midCfg := &store.FactorQuantConfig{
			Market:     store.FactorQuantMarketUS,
			RegimeTilt: &fFalse,
			SizeTier:   store.FactorQuantSizeTierMidSmall,
		}
		gotMid, infoMid := fqResolveRegimeTargets(base, midCfg, targetsPath)
		if !reflect.DeepEqual(gotMid.Weights, map[string]float64{"SMOLBUSDT": 0.25, "TINYBUSDT": -0.15}) {
			t.Fatalf("mid_small weights = %+v", gotMid.Weights)
		}
		if infoMid.MegaLargeGross != 0 || math.Abs(infoMid.MidSmallGross-0.40) > 1e-6 {
			t.Fatalf("mid_small info = %+v", infoMid)
		}

		// 4. MaxMidSmallExposurePct = 20: proportionally scales 0.40 -> 0.20 while preserving mega_large & unknown.
		capCfg := &store.FactorQuantConfig{
			Market:                 store.FactorQuantMarketUS,
			RegimeTilt:             &fFalse,
			MaxMidSmallExposurePct: 20,
		}
		gotCap, infoCap := fqResolveRegimeTargets(base, capCfg, targetsPath)
		if math.Abs(gotCap.Weights["AAPLBUSDT"]-0.40) > 1e-6 {
			t.Fatalf("mega_large altered by mid/small cap: %+v", gotCap.Weights)
		}
		if math.Abs(gotCap.Weights["SMOLBUSDT"]-0.125) > 1e-6 || math.Abs(gotCap.Weights["TINYBUSDT"]-(-0.075)) > 1e-6 {
			t.Fatalf("mid_small weights not proportionally scaled to 20%% cap: %+v", gotCap.Weights)
		}
		if infoCap.MidSmallGross > 0.20+1e-9 || math.Abs(infoCap.MidSmallGross-0.20) > 1e-6 {
			t.Fatalf("capped MidSmallGross = %v", infoCap.MidSmallGross)
		}
	})

	t.Run("crypto_100m_threshold", func(t *testing.T) {
		root := t.TempDir()
		dailyDir := filepath.Join(root, "daily")
		fqWriteDailyJSON(t, dailyDir, "BTCUSDT.json", start, fqSeriesLinear(n, 50000, 100))
		// >= $100M ADV20 -> mega_large ($150M)
		fqWriteTierDailyJSON(t, dailyDir, "ETHUSDT.json", start, n, 3000, 50_000, false)
		// < $100M ADV20 -> mid_small ($50M)
		fqWriteTierDailyJSON(t, dailyDir, "ALTUSDT.json", start, n, 10, 5_000_000, false)

		base := &quantTargets{
			Version: 1,
			Market:  store.FactorQuantMarketCrypto,
			Status:  quantStatusOK,
			AsOf:    asOf,
			Weights: map[string]float64{
				"ETHUSDT": 0.50,
				"ALTUSDT": -0.50,
			},
		}
		targetsPath := fqWriteTargetsMap(t, root, store.FactorQuantMarketCrypto, asOf, 3, base.Weights, map[string]float64{}, nil)
		fFalse := false
		cfg := &store.FactorQuantConfig{
			Market:                 store.FactorQuantMarketCrypto,
			RegimeTilt:             &fFalse,
			MaxMidSmallExposurePct: 15,
		}
		got, info := fqResolveRegimeTargets(base, cfg, targetsPath)
		if math.Abs(got.Weights["ETHUSDT"]-0.50) > 1e-6 || math.Abs(got.Weights["ALTUSDT"]-(-0.15)) > 1e-6 {
			t.Fatalf("crypto cap weights = %+v", got.Weights)
		}
		if math.Abs(info.MegaLargeGross-0.50) > 1e-6 || math.Abs(info.MidSmallGross-0.15) > 1e-6 {
			t.Fatalf("crypto tier info = %+v", info)
		}
	})
}

func TestStockCycleSelectionSizeTierAndSnapshotTier(t *testing.T) {
	fx := newStockFixtureCfg(t, true, stockTestNow, nil, func(c *store.StockConfig) {
		c.SymbolSource = store.StockSymbolSourceHybrid
		c.ScanTopN = 5
		c.ScanSizeTier = store.StockScanSizeTierMegaLarge
	})
	oldLookup := stockLookupSymbol
	oldGetSeries := stockGetSeries
	oldGetQuote := stockGetQuote
	oldGetUnderlying := stockGetUnderlyingSeries
	t.Cleanup(func() {
		stockLookupSymbol = oldLookup
		stockGetSeries = oldGetSeries
		stockGetQuote = oldGetQuote
		stockGetUnderlyingSeries = oldGetUnderlying
	})

	stockLookupSymbol = func(_ context.Context, sym string) (usstock.SymbolInfo, bool) {
		return usstock.SymbolInfo{Symbol: sym, Underlying: strings.TrimSuffix(sym, "BUSDT")}, true
	}
	fx.at.stock.cfg.Symbols = []string{"MANMIDBUSDT"}

	report := &stockengine.ScanReport{
		GeneratedAt: stockTestNow,
		Results: []stockengine.ScanResult{
			{
				Symbol:      "AAPLBUSDT",
				DataThrough: "2026-10-13",
				Cycle:       stockengine.CycleAssessment{LongSetup: true, Score: 90, Phase: stockengine.PhaseWedgePop},
				Universe:    stockengine.UniverseRow{Symbol: "AAPLBUSDT", Class: "stock", AvgDollarVol20: 1e9, Tier: stockengine.TierMegaLarge},
			},
			{
				Symbol:      "SPYBUSDT",
				DataThrough: "2026-10-13",
				Cycle:       stockengine.CycleAssessment{LongSetup: true, Score: 85, Phase: stockengine.PhaseEMACrossback},
				Universe:    stockengine.UniverseRow{Symbol: "SPYBUSDT", Class: "ETF", AvgDollarVol20: 4e9, Tier: stockengine.TierETF},
			},
			{
				Symbol:      "SMOLBUSDT",
				DataThrough: "2026-10-13",
				Cycle:       stockengine.CycleAssessment{LongSetup: true, Score: 99, Phase: stockengine.PhaseWedgePop},
				Universe:    stockengine.UniverseRow{Symbol: "SMOLBUSDT", Class: "stock", AvgDollarVol20: 1e6, Tier: stockengine.TierMidSmall},
			},
		},
	}
	stockGetScanReport = func(context.Context, time.Duration) (*stockengine.ScanReport, error) { return report, nil }

	state := &stockState{Holdings: map[string]*stockHolding{"HELDMIDBUSDT": {Owned: 1}}}
	list, entries, _ := fx.at.stockCycleSelection(context.Background(), fx.at.stock.cfg, state, func(string, ...interface{}) {})

	want := []string{"HELDMIDBUSDT", "MANMIDBUSDT", "AAPLBUSDT", "SPYBUSDT"}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("stockCycleSelection with ScanSizeTier=mega_large = %v, want %v", list, want)
	}
	if entries["SMOLBUSDT"] {
		t.Fatal("mid_small scan candidate SMOLBUSDT should be excluded")
	}

	// Verify buildStockSnapshots populates Tier from fresh report and falls back to 20d ADV20.
	fx.at.stock.cfg.Symbols = []string{"AAPLBUSDT", "SPYBUSDT", "FALLBACKBUSDT"}
	bars20 := make([]usstock.Bar, 25)
	for i := range bars20 {
		bars20[i] = usstock.Bar{OpenTime: stockTestNow.AddDate(0, 0, i-25), Open: 100, High: 101, Low: 99, Close: 100, Volume: 100_000} // 10M ADV20 -> mid_small
	}
	stockGetUnderlyingSeries = func(context.Context, string, string, int) (*usstock.Series, error) {
		return nil, os.ErrNotExist
	}
	stockGetSeries = func(_ context.Context, sym, tf string, need int, allowYahoo bool) (*usstock.Series, error) {
		if tf == usstock.TF1d {
			return &usstock.Series{Timeframe: tf, Source: usstock.SourceBStock, Bars: bars20}, nil
		}
		return &usstock.Series{Timeframe: tf, Source: usstock.SourceBStock, Bars: bars20[:10]}, nil
	}
	stockGetQuote = func(_ context.Context, sym string) (*usstock.Quote, error) {
		return &usstock.Quote{BStockPrice: 100, RefPrice: 100, RefFresh: true, RefTime: stockTestNow, Session: usstock.SessionRegular}, nil
	}

	snaps, _, _ := fx.at.buildStockSnapshots(context.Background(), fx.at.stock.cfg, stockengine.ResolvePreset(fx.at.stock.cfg), stockTestNow, report, func(string, ...interface{}) {})
	gotTier := map[string]string{}
	for _, s := range snaps {
		gotTier[s.Symbol] = s.Tier
	}
	if gotTier["AAPLBUSDT"] != stockengine.TierMegaLarge || gotTier["SPYBUSDT"] != stockengine.TierETF || gotTier["FALLBACKBUSDT"] != stockengine.TierMidSmall {
		t.Fatalf("snapshot tiers = %+v", gotTier)
	}
}
