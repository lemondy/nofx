package trader

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

func fqWriteDailyJSON(t *testing.T, dir, filename string, start time.Time, closes []float64) {
	t.Helper()
	rows := make([][]any, 0, len(closes))
	for i, c := range closes {
		d := start.AddDate(0, 0, i).Format(factorDateFmt)
		rows = append(rows, []any{d, c * 0.995, c * 1.01, c * 0.99, c, 1000.0})
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

func fqSeriesLinear(n int, start, step float64) []float64 {
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		wiggle := 0.25 * float64((i%5)-2)
		v := start + step*float64(i) + wiggle
		if v < 1 {
			v = 1
		}
		out[i] = v
	}
	return out
}

func fqWriteTargetsMap(t *testing.T, dir, market, asOf string, horizon int, baseWeights map[string]float64, pct map[string]float64, k *float64) string {
	t.Helper()
	doc := map[string]any{
		"version":       1,
		"market":        market,
		"status":        "ok",
		"reason":        nil,
		"generated_at":  asOf + "T00:05:00Z",
		"as_of":         asOf,
		"run_id":        "run-" + asOf,
		"horizon":       horizon,
		"rule":          map[string]string{"name": "ls_invvol_protected_v1"},
		"factor_ids":    []string{"f1"},
		"universe_size": len(pct),
		"weights":       baseWeights,
		"excluded":      map[string]string{},
		"pct":           pct,
	}
	if k != nil {
		doc["k"] = *k
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "quant_targets.json")
	if err := os.WriteFile(p, raw, 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFQRegimeBullBearChopAndDisabled(t *testing.T) {
	// 50 symbols evenly spaced in pct (0.01 .. 1.00).
	const nSyms = 50
	pct := make(map[string]float64, nSyms)
	baseWeights := map[string]float64{
		"SYM01USDT": -0.05, "SYM02USDT": -0.05,
		"SYM49USDT": 0.05, "SYM50USDT": 0.05,
	}
	start := fqDay("2026-06-01")
	nDays := 120
	asOf := start.AddDate(0, 0, nDays-1).Format(factorDateFmt)

	for _, tc := range []struct {
		name        string
		benchCloses []float64
		tiltEnabled *bool
		wantState   string
		wantApplied bool
		wantLongs   int
		wantShorts  int
	}{
		{
			name:        "bull_20L_10S",
			benchCloses: fqSeriesLinear(nDays, 50, 0.8),
			wantState:   fqRegimeBull,
			wantApplied: true,
			wantLongs:   20,
			wantShorts:  10,
		},
		{
			name:        "bear_20S_10L",
			benchCloses: fqSeriesLinear(nDays, 150, -0.8),
			wantState:   fqRegimeBear,
			wantApplied: true,
			wantLongs:   10,
			wantShorts:  20,
		},
		{
			name: "chop_keeps_base",
			// Uptrend then dip below EMA20 while EMA20 still > EMA50 => chop.
			benchCloses: append(fqSeriesLinear(nDays-5, 50, 0.9), 115, 110, 106, 102, 99),
			wantState:   fqRegimeChop,
			wantApplied: false,
			wantLongs:   2,
			wantShorts:  2,
		},
		{
			name:        "unknown_insufficient_bars",
			benchCloses: fqSeriesLinear(30, 50, 1.0),
			wantState:   fqRegimeUnknown,
			wantApplied: false,
			wantLongs:   2,
			wantShorts:  2,
		},
		{
			name:        "tilt_disabled_keeps_base",
			benchCloses: fqSeriesLinear(nDays, 50, 0.8),
			tiltEnabled: func() *bool { b := false; return &b }(),
			wantState:   fqRegimeBull,
			wantApplied: false,
			wantLongs:   2,
			wantShorts:  2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dailyDir := filepath.Join(root, "daily")
			benchStart := start
			if len(tc.benchCloses) < nDays {
				benchStart = start.AddDate(0, 0, nDays-len(tc.benchCloses))
			}
			fqWriteDailyJSON(t, dailyDir, "BTCUSDT.json", benchStart, tc.benchCloses)
			for i := 1; i <= nSyms; i++ {
				sym := fmt.Sprintf("SYM%02dUSDT", i)
				pct[sym] = float64(i) / float64(nSyms)
				// Realistic drift correlated with rank so only top-pct symbols have top-decile ret7.
				closes := make([]float64, nDays)
				for d := 0; d < nDays; d++ {
					closes[d] = 100.0 + float64(d)*0.002*float64(i) + float64((d%4)-1)*0.5
				}
				fqWriteDailyJSON(t, dailyDir, sym+".json", start, closes)
			}

			path := fqWriteTargetsMap(t, root, "crypto", asOf, 3, baseWeights, pct, nil)
			loaded, err := loadQuantTargets(path, "crypto")
			if err != nil {
				t.Fatal(err)
			}
			cfg := &store.FactorQuantConfig{Market: "crypto", Horizon: 3, TargetsPath: path, RegimeTilt: tc.tiltEnabled}
			eff, info := fqResolveRegimeTargets(loaded, cfg, path)
			if info.State != tc.wantState || info.Applied != tc.wantApplied {
				t.Fatalf("regime = %+v, want state=%s applied=%v", info, tc.wantState, tc.wantApplied)
			}
			if info.LongCount != tc.wantLongs || info.ShortCount != tc.wantShorts {
				t.Fatalf("counts = %dL/%dS, want %dL/%dS (info=%+v)", info.LongCount, info.ShortCount, tc.wantLongs, tc.wantShorts, info)
			}
			if tc.wantApplied {
				wantTotalGross := 1.0
				gotTotalGross := info.LongGross + info.ShortGross
				if math.Abs(gotTotalGross-wantTotalGross) > 1e-4 {
					t.Fatalf("total gross = %.6f, want ~1.0", gotTotalGross)
				}
				if tc.wantState == fqRegimeBull && math.Abs(info.LongGross-2.0/3.0) > 1e-4 {
					t.Fatalf("bull long gross = %.6f, want ~0.6667", info.LongGross)
				}
				if tc.wantState == fqRegimeBear && math.Abs(info.ShortGross-2.0/3.0) > 1e-4 {
					t.Fatalf("bear short gross = %.6f, want ~0.6667", info.ShortGross)
				}
				// Verify every long weight > 0 came from top pct and short weight < 0 from bottom pct.
				for sym, w := range eff.Weights {
					if w > 0 && pct[sym] <= 0.6 {
						t.Errorf("unexpected long %s pct=%.2f", sym, pct[sym])
					}
					if w < 0 && pct[sym] > 0.4 {
						t.Errorf("unexpected short %s pct=%.2f", sym, pct[sym])
					}
				}
			} else {
				if len(eff.Weights) != len(baseWeights) {
					t.Fatalf("weights modified when not applied: got %d want %d", len(eff.Weights), len(baseWeights))
				}
			}
		})
	}
}

func TestFQRegimeCryptoShortExclusionsAndNoLookahead(t *testing.T) {
	root := t.TempDir()
	dailyDir := filepath.Join(root, "daily")
	start := fqDay("2026-06-01")
	nDays := 120
	asOf := start.AddDate(0, 0, nDays-1).Format(factorDateFmt)

	// Add 10 future crash bars AFTER asOf; no-lookahead must ignore them and stay bull.
	bench := fqSeriesLinear(nDays, 50, 0.8)
	for i := 0; i < 10; i++ {
		bench = append(bench, 20.0)
	}
	fqWriteDailyJSON(t, dailyDir, "BTCUSDT.json", start, bench)

	const nSyms = 30
	pct := make(map[string]float64, nSyms)
	for i := 1; i <= nSyms; i++ {
		sym := fmt.Sprintf("C%02dUSDT", i)
		pct[sym] = float64(i) / float64(nSyms)
		switch i {
		case 1:
			// Short candidate with < 90 listing days -> must be excluded from shorts.
			fqWriteDailyJSON(t, dailyDir, sym+".json", start.AddDate(0, 0, nDays-50), fqSeriesLinear(50, 100, 0.1))
		case 2:
			// Short candidate with huge 7-day surge (>= 90th percentile ret7) -> must be excluded.
			closes := make([]float64, nDays)
			for d := 0; d < nDays; d++ {
				closes[d] = 100.0
				if d >= nDays-7 {
					closes[d] = 220.0
				}
			}
			fqWriteDailyJSON(t, dailyDir, sym+".json", start, closes)
		default:
			closes := make([]float64, nDays)
			for d := 0; d < nDays; d++ {
				closes[d] = 100.0 + float64(d)*0.002*float64(i) + float64((d%4)-1)*0.5
			}
			fqWriteDailyJSON(t, dailyDir, sym+".json", start, closes)
		}
	}

	path := fqWriteTargetsMap(t, root, "crypto", asOf, 3, map[string]float64{"C30USDT": 0.5, "C03USDT": -0.5}, pct, nil)
	loaded, err := loadQuantTargets(path, "crypto")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &store.FactorQuantConfig{Market: "crypto", Horizon: 3, TargetsPath: path}
	eff, info := fqResolveRegimeTargets(loaded, cfg, path)
	if info.State != fqRegimeBull || !info.Applied || info.AsOf != asOf {
		t.Fatalf("lookahead leaked or bull not detected: %+v", info)
	}
	if _, ok := eff.Weights["C01USDT"]; ok {
		t.Fatalf("short with <90 listing days was not excluded")
	}
	if _, ok := eff.Weights["C02USDT"]; ok {
		t.Fatalf("short with top-decile 7d surge was not excluded")
	}
}

func TestFQRegimeUSPerpFilterAndVolScaleK(t *testing.T) {
	root := t.TempDir()
	usDir := filepath.Join(root, "factor_mining", "us")
	dailyDir := filepath.Join(usDir, "daily")
	cacheDir := filepath.Join(root, "cache")
	start := fqDay("2026-06-01")
	nDays := 120
	asOf := start.AddDate(0, 0, nDays-1).Format(factorDateFmt)

	// SPY in clear bear trend -> up to 20 shorts / max 10 longs.
	fqWriteDailyJSON(t, dailyDir, "SPY.json", start, fqSeriesLinear(nDays, 500, -1.5))

	const nSyms = 30
	pct := make(map[string]float64, nSyms)
	type symEntry struct {
		Underlying string `json:"underlying"`
		PerpSymbol string `json:"perp_symbol"`
	}
	var symEntries []symEntry
	for i := 1; i <= nSyms; i++ {
		under := fmt.Sprintf("STK%02d", i)
		sym := under + "BUSDT"
		pct[sym] = float64(i) / float64(nSyms)
		closes := make([]float64, nDays)
		for d := 0; d < nDays; d++ {
			closes[d] = 100.0 + float64((d%4)-1)*0.5
		}
		fqWriteDailyJSON(t, dailyDir, under+".json", start, closes)
		perp := under + "USDT"
		if i == 1 {
			perp = "" // STK01 has no perpetual short venue -> must be skipped from shorts
		}
		symEntries = append(symEntries, symEntry{Underlying: under, PerpSymbol: perp})
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatal(err)
	}
	perpRaw, err := json.Marshal(map[string]any{"version": 2, "symbols": symEntries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "usstock_symbols.json"), perpRaw, 0644); err != nil {
		t.Fatal(err)
	}

	k := 0.5
	path := fqWriteTargetsMap(t, usDir, "us", asOf, 5, map[string]float64{"STK30BUSDT": 0.25, "STK02BUSDT": -0.25}, pct, &k)
	loaded, err := loadQuantTargets(path, "us")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &store.FactorQuantConfig{Market: "us", Horizon: 0, TargetsPath: path, USShortMode: "paper"}
	eff, info := fqResolveRegimeTargets(loaded, cfg, path)
	if info.State != fqRegimeBear || info.Benchmark != "SPY" || !info.Applied {
		t.Fatalf("US regime = %+v", info)
	}
	if _, ok := eff.Weights["STK01BUSDT"]; ok {
		t.Fatalf("US short without perp_symbol should be excluded")
	}
	if info.LongCount != 10 || info.ShortCount != 19 { // 20 short candidates minus STK01 without perp
		t.Fatalf("counts = %dL/%dS, want 10L/19S", info.LongCount, info.ShortCount)
	}
	// With k = 0.5, total gross should be ~0.5 instead of ~1.0.
	if math.Abs((info.LongGross+info.ShortGross)-0.5) > 1e-3 {
		t.Fatalf("volscale k=0.5 not applied: longGross=%.4f shortGross=%.4f", info.LongGross, info.ShortGross)
	}
}

func TestFQRegimeEndToEndRebalanceAndRiskStatus(t *testing.T) {
	root := t.TempDir()
	dailyDir := filepath.Join(root, "daily")
	start := fqDay("2026-06-01")
	nDays := 120
	asOf := start.AddDate(0, 0, nDays-1).Format(factorDateFmt)

	// Bear market on BTCUSDT => expects 20 shorts, 10 longs.
	fqWriteDailyJSON(t, dailyDir, "BTCUSDT.json", start, fqSeriesLinear(nDays, 150, -0.8))

	const nSyms = 40
	pct := make(map[string]float64, nSyms)
	prices := make(map[string]float64, nSyms)
	for i := 1; i <= nSyms; i++ {
		sym := fmt.Sprintf("E%02dUSDT", i)
		pct[sym] = float64(i) / float64(nSyms)
		prices[sym] = 100.0
		closes := make([]float64, nDays)
		for d := 0; d < nDays; d++ {
			closes[d] = 100.0 + float64(d)*0.002*float64(i) + float64((d%4)-1)*0.5
		}
		fqWriteDailyJSON(t, dailyDir, sym+".json", start, closes)
	}
	path := fqWriteTargetsMap(t, root, "crypto", asOf, 3, map[string]float64{"E40USDT": 0.5, "E01USDT": -0.5}, pct, nil)

	at := fqPaperTrader(t, fqTestStore(t), "fq-regime-e2e", "crypto", &paperTestPrices{mark: 100})
	cfg := fqEnableRisk(at)
	cfg.TargetsPath = path
	f := newFakeFutures(10000, prices)
	broker := newFQFuturesBroker(f, 3, 0.2, 0.2)

	now := start.AddDate(0, 0, nDays).Add(2 * time.Hour)
	at.fqNow = func() time.Time { return now }
	at.factorQuantTick(broker, now)

	longPos, shortPos := 0, 0
	for _, p := range f.pos {
		if p.side == "long" {
			longPos++
		} else if p.side == "short" {
			shortPos++
		}
	}
	if longPos != 10 || shortPos != 20 {
		t.Fatalf("opened positions = %dL/%dS, want 10L/20S", longPos, shortPos)
	}

	rs := at.factorQuantRiskStatus()
	if rs.FactorQuant == nil || rs.FactorQuant.Regime == nil {
		t.Fatalf("RiskStatus missing FactorQuant.Regime: %+v", rs.FactorQuant)
	}
	reg := rs.FactorQuant.Regime
	if reg.State != fqRegimeBear || reg.Benchmark != "BTCUSDT" || !reg.Applied || reg.LongCount != 10 || reg.ShortCount != 20 {
		t.Fatalf("unexpected RiskStatus.Regime: %+v", reg)
	}

	recs := fqRecords(t, at)
	if len(recs) != 1 {
		t.Fatalf("decision record count = %d, want 1", len(recs))
	}
	if !strings.Contains(recs[0].CoTTrace, "市场趋势=熊市") || !strings.Contains(recs[0].CoTTrace, "多 10 只") || !strings.Contains(recs[0].CoTTrace, "空 20 只") {
		t.Fatalf("CoTTrace missing regime summary:\n%s", recs[0].CoTTrace)
	}
	joinedLog := strings.Join(recs[0].ExecutionLog, "\n")
	if !strings.Contains(joinedLog, "市场趋势=熊市") {
		t.Fatalf("ExecutionLog missing regime summary:\n%s", joinedLog)
	}
}

func TestFQRegimeUSMissingPerpMapFallsBack(t *testing.T) {
	root := t.TempDir()
	usDir := filepath.Join(root, "factor_mining", "us")
	dailyDir := filepath.Join(usDir, "daily")
	start := fqDay("2026-06-01")
	nDays := 120
	asOf := start.AddDate(0, 0, nDays-1).Format(factorDateFmt)

	fqWriteDailyJSON(t, dailyDir, "SPY.json", start, fqSeriesLinear(nDays, 500, -1.5))

	const nSyms = 30
	pct := make(map[string]float64, nSyms)
	for i := 1; i <= nSyms; i++ {
		under := fmt.Sprintf("U%02d", i)
		sym := under + "BUSDT"
		pct[sym] = float64(i) / float64(nSyms)
		closes := make([]float64, nDays)
		for d := 0; d < nDays; d++ {
			closes[d] = 100.0 + float64((d%4)-1)*0.5
		}
		fqWriteDailyJSON(t, dailyDir, under+".json", start, closes)
	}

	baseWeights := map[string]float64{"U30BUSDT": 0.5, "U02BUSDT": -0.5}
	path := fqWriteTargetsMap(t, usDir, "us", asOf, 5, baseWeights, pct, nil)
	loaded, err := loadQuantTargets(path, "us")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &store.FactorQuantConfig{Market: "us", Horizon: 0, TargetsPath: path, USShortMode: "paper"}
	eff, info := fqResolveRegimeTargets(loaded, cfg, path)
	if info.Applied {
		t.Fatalf("expected Applied=false when usstock_symbols.json is missing, got %+v", info)
	}
	if len(eff.Weights) != len(baseWeights) || eff.Weights["U30BUSDT"] != 0.5 || eff.Weights["U02BUSDT"] != -0.5 {
		t.Fatalf("base weights altered despite missing perp map: %+v", eff.Weights)
	}
}

func TestFQRegimeFavoredSleeveCapacityFallback(t *testing.T) {
	root := t.TempDir()
	dailyDir := filepath.Join(root, "daily")
	start := fqDay("2026-06-01")
	nDays := 120
	asOf := start.AddDate(0, 0, nDays-1).Format(factorDateFmt)

	// Bear market: 20 short candidates, 10 long candidates.
	fqWriteDailyJSON(t, dailyDir, "BTCUSDT.json", start, fqSeriesLinear(nDays, 150, -0.8))

	const nSyms = 30
	pct := make(map[string]float64, nSyms)
	for i := 1; i <= nSyms; i++ {
		sym := fmt.Sprintf("S%02dUSDT", i)
		pct[sym] = float64(i) / float64(nSyms)
		// Exclude 18 of the 20 short candidates (<90 listing days), leaving only 2 eligible shorts (cap 0.2) vs 10 longs (0.5).
		if i <= 18 {
			fqWriteDailyJSON(t, dailyDir, sym+".json", start.AddDate(0, 0, nDays-40), fqSeriesLinear(40, 100, 0.1))
			continue
		}
		closes := make([]float64, nDays)
		for d := 0; d < nDays; d++ {
			closes[d] = 100.0 + float64(d)*0.002*float64(i) + float64((d%4)-1)*0.5
		}
		fqWriteDailyJSON(t, dailyDir, sym+".json", start, closes)
	}

	baseWeights := map[string]float64{"S30USDT": 0.5, "S19USDT": -0.25, "S20USDT": -0.25}
	path := fqWriteTargetsMap(t, root, "crypto", asOf, 3, baseWeights, pct, nil)
	loaded, err := loadQuantTargets(path, "crypto")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &store.FactorQuantConfig{Market: "crypto", Horizon: 3, TargetsPath: path}
	eff, info := fqResolveRegimeTargets(loaded, cfg, path)
	if info.Applied {
		t.Fatalf("expected Applied=false when favored sleeve cap (0.2) < counter sleeve (0.5), got %+v", info)
	}
	if len(eff.Weights) != len(baseWeights) || eff.Weights["S30USDT"] != 0.5 {
		t.Fatalf("expected fallback to base weights, got %+v", eff.Weights)
	}
}

func TestFQShortLegInverseVarianceWeighting(t *testing.T) {
	root := t.TempDir()
	dailyDir := filepath.Join(root, "daily")
	start := fqDay("2026-06-01")
	nDays := 120
	asOf := start.AddDate(0, 0, nDays-1).Format(factorDateFmt)

	fqWriteDailyJSON(t, dailyDir, "BTCUSDT.json", start, fqSeriesLinear(nDays, 50, 0.8))

	const nSyms = 30
	pct := make(map[string]float64, nSyms)
	for i := 1; i <= nSyms; i++ {
		sym := fmt.Sprintf("V%02dUSDT", i)
		pct[sym] = float64(i) / float64(nSyms)
		closes := make([]float64, nDays)
		amp := 1.0
		// V01 (short) and V30 (long) have 2x the daily return swing of V02 (short) and V29 (long).
		if i == 1 || i == 30 {
			amp = 2.0
		}
		for d := 0; d < nDays; d++ {
			drift := math.Pow(1.0+0.0001*float64(i), float64(d))
			if d%2 == 0 {
				closes[d] = 100.0 * drift * (1.0 + 0.01*amp)
			} else {
				closes[d] = 100.0 * drift * (1.0 - 0.01*amp)
			}
		}
		fqWriteDailyJSON(t, dailyDir, sym+".json", start, closes)
	}

	path := fqWriteTargetsMap(t, root, "crypto", asOf, 3, map[string]float64{"V30USDT": 0.5, "V01USDT": -0.5}, pct, nil)
	loaded, err := loadQuantTargets(path, "crypto")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &store.FactorQuantConfig{Market: "crypto", Horizon: 3, TargetsPath: path}
	eff, info := fqResolveRegimeTargets(loaded, cfg, path)
	if !info.Applied || info.State != fqRegimeBull {
		t.Fatalf("expected bull applied, got %+v", info)
	}
	// Longs use 1/sigma (ratio ~2x), shorts use 1/sigma^2 (ratio ~4x).
	longRatio := eff.Weights["V29USDT"] / eff.Weights["V30USDT"]
	shortRatio := math.Abs(eff.Weights["V02USDT"]) / math.Abs(eff.Weights["V01USDT"])
	if longRatio < 1.8 || longRatio > 2.2 {
		t.Fatalf("long 1/sigma ratio = %.3f, want ~2.0", longRatio)
	}
	if shortRatio < 3.6 || shortRatio > 4.4 {
		t.Fatalf("short 1/sigma^2 ratio = %.3f, want ~4.0", shortRatio)
	}
}

func TestFQAdaptiveInitialStopAndChandelierTrailingStop(t *testing.T) {
	root := t.TempDir()
	dailyDir := filepath.Join(root, "daily")
	start := fqDay("2026-06-01")
	nDays := 60
	asOf := start.AddDate(0, 0, nDays-1).Format(factorDateFmt)

	// LOWVOLUSDT: ~1% daily vol => 2.5 * 0.01 * sqrt(3) = 4.3% -> clamped to 8% stop.
	lowCloses := make([]float64, nDays)
	// HIGHVOLUSDT: ~7.5% daily vol => 2.5 * 0.075 * sqrt(3) = 32.4% -> clamped to 28% stop.
	highCloses := make([]float64, nDays)
	for d := 0; d < nDays; d++ {
		if d%2 == 0 {
			lowCloses[d] = 100.0 * 1.005
			highCloses[d] = 100.0 * 1.038
		} else {
			lowCloses[d] = 100.0 * 0.995
			highCloses[d] = 100.0 * 0.962
		}
	}
	fqWriteDailyJSON(t, dailyDir, "LOWVOLUSDT.json", start, lowCloses)
	fqWriteDailyJSON(t, dailyDir, "HIGHVOLUSDT.json", start, highCloses)

	path := fqWriteTargetsMap(t, root, "crypto", asOf, 3, map[string]float64{"LOWVOLUSDT": 0.5, "HIGHVOLUSDT": -0.5}, map[string]float64{"LOWVOLUSDT": 0.9, "HIGHVOLUSDT": 0.1}, nil)
	cfg := &store.FactorQuantConfig{Market: "crypto", Horizon: 3, TargetsPath: path}

	// 1. Low-vol long initial stop should clamp to 8% (stop = 92 on entry 100).
	stopLow, trailingLow := fqComputeAdaptiveStopPrice("LOWVOLUSDT", fqSideLong, 100, 100, 100, 0.20, cfg, asOf)
	if trailingLow || math.Abs(stopLow-92.0) > 1e-6 {
		t.Fatalf("low-vol initial stop = %v (trailing=%v), want 92.0", stopLow, trailingLow)
	}
	// 2. High-vol short initial stop should clamp to 28% (stop = 128 on entry 100).
	stopHigh, trailingHigh := fqComputeAdaptiveStopPrice("HIGHVOLUSDT", fqSideShort, 100, 100, 100, 0.20, cfg, asOf)
	if trailingHigh || math.Abs(stopHigh-128.0) > 1e-6 {
		t.Fatalf("high-vol initial stop = %v (trailing=%v), want 128.0", stopHigh, trailingHigh)
	}
	// 3. When VolAdaptiveStop is disabled, both revert to fixed 20% (80 and 120).
	off := false
	cfgOff := &store.FactorQuantConfig{Market: "crypto", Horizon: 3, TargetsPath: path, VolAdaptiveStop: &off}
	stopOff, _ := fqComputeAdaptiveStopPrice("LOWVOLUSDT", fqSideLong, 100, 125, 125, 0.20, cfgOff, asOf)
	if math.Abs(stopOff-80.0) > 1e-6 {
		t.Fatalf("disabled VolAdaptiveStop = %v, want 80.0", stopOff)
	}

	// 4. End-to-end Chandelier trailing ratchet on a crypto position via factorQuantTick:
	at := fqPaperTrader(t, fqTestStore(t), "fq-chandelier", "crypto", &paperTestPrices{mark: 100})
	at.config.StrategyConfig.FactorQuantConfig.TargetsPath = path
	fqSetOwned(t, at, asOf)
	f := newFakeFutures(10000, map[string]float64{"LOWVOLUSDT": 100})
	f.pos["LOWVOLUSDT:long"] = &fakePos{sym: "LOWVOLUSDT", side: fqSideLong, qty: 10, entry: 100}
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	b.at = at

	// Rally to 120 (+20% >= 15% trigger): tick should ratchet STOP_MARKET above entry (120 - 0.25*20 = 115, since 2*ATR14 ~ 4.4 < 0.25*20=5.0).
	f.prices["LOWVOLUSDT"] = 120
	now := start.AddDate(0, 0, nDays).Add(2 * time.Hour)
	at.factorQuantTick(b, now)
	ratcheted := f.stops["LOWVOLUSDT:LONG"]
	if ratcheted <= 108.0 {
		t.Fatalf("expected trailing stop ratcheted above entry (>108), got %v", ratcheted)
	}
	// Pullback to 116 (above ratcheted stop): stop must NOT loosen.
	f.prices["LOWVOLUSDT"] = 116
	at.factorQuantTick(b, now.Add(5*time.Minute))
	if f.stops["LOWVOLUSDT:LONG"] < ratcheted-1e-6 {
		t.Fatalf("trailing stop loosened on pullback: before=%v after=%v", ratcheted, f.stops["LOWVOLUSDT:LONG"])
	}
	// Drop to 110 (below ratcheted stop): tick closes position at market!
	f.prices["LOWVOLUSDT"] = 110
	at.factorQuantTick(b, now.Add(10*time.Minute))
	if _, stillHeld := f.pos["LOWVOLUSDT:long"]; stillHeld {
		t.Fatalf("position was not closed when mark dropped below trailing stop %v", ratcheted)
	}
}

func TestFQUSStockPaperStopLossAndTrailing(t *testing.T) {
	quotes := map[string]float64{"AAPLBUSDT": 100}
	fqStubUS(t, quotes)
	st := fqTestStore(t)
	at := fqPaperTrader(t, st, "fq-us-stop", "us", &paperTestPrices{mark: 100})
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.USFeeBps, cfg.USSlippageBps = fqPtr(0), fqPtr(0)
	dir := t.TempDir()
	cfg.TargetsPath = writeTargetsFile(t, dir, targetsJSON("us", "ok", "2026-11-01", 5, `{"AAPLBUSDT":0.5}`))
	b, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	day := fqDay("2026-11-02")
	at.fqNow = func() time.Time { return day }
	at.factorQuantTick(b, day) // opens AAPLBUSDT @ 100 (50% of equity = 5000)

	// Rally to 120 (+20% gain -> Chandelier trailing stop ratchets to 110).
	quotes["AAPLBUSDT"] = 120
	at.factorQuantTick(b, day.Add(5*time.Minute))
	open, _ := st.StockPaper().ListOpen(at.id)
	if len(open) != 1 {
		t.Fatalf("expected AAPLBUSDT still open at 120, got %+v", open)
	}

	// Pull back to 108 (<= 110 trailing stop): US paper stop must close AAPLBUSDT on the 5-minute tick!
	quotes["AAPLBUSDT"] = 108
	at.factorQuantTick(b, day.Add(10*time.Minute))
	open, _ = st.StockPaper().ListOpen(at.id)
	if len(open) != 0 {
		t.Fatalf("expected AAPLBUSDT closed by US paper trailing stop at 108, still open: %+v", open)
	}
	acct, _ := st.StockPaper().GetAccount(at.id)
	// Started 10000, bought 50 shares @ 100, sold 50 shares @ 108 => cash = 10400 (locked in +400 profit!).
	fqNear(t, acct.Cash, 10400, "US paper trailing stop locked profit")
}

func TestFQRollingAlphaDecayEarlyExitAndReplacement(t *testing.T) {
	st := fqTestStore(t)
	at := fqPaperTrader(t, st, "fq-rolling", "crypto", &paperTestPrices{mark: 100})
	cfg := at.config.StrategyConfig.FactorQuantConfig
	off := false
	cfg.RegimeTilt = &off // test rolling exit in isolation from regime tilt
	dir := t.TempDir()

	// Day 0 (as_of = 2026-10-28, horizon = 3): open GOODLONG (0.25), BADLONG (0.25), BADSHORT (-0.50).
	pctDay0 := map[string]float64{"GOODLONGUSDT": 0.90, "BADLONGUSDT": 0.85, "BADSHORTUSDT": 0.05, "NEWLONGUSDT": 0.55}
	weightsDay0 := map[string]float64{"GOODLONGUSDT": 0.25, "BADLONGUSDT": 0.25, "BADSHORTUSDT": -0.50}
	cfg.TargetsPath = fqWriteTargetsMap(t, dir, "crypto", "2026-10-28", 3, weightsDay0, pctDay0, nil)

	f := newFakeFutures(10000, map[string]float64{
		"GOODLONGUSDT": 100,
		"BADLONGUSDT":  100,
		"BADSHORTUSDT": 100,
		"NEWLONGUSDT":  100,
	})
	b := newFQFuturesBroker(f, 3, 0.2, 0.2)
	b.at = at

	day0 := fqDay("2026-10-29")
	at.factorQuantTick(b, day0)
	if len(f.pos) != 3 {
		t.Fatalf("day0 positions = %+v, want 3", f.pos)
	}

	// Day 1 (as_of = 2026-10-29, still within 3-day too_soon window):
	// BADLONG decayed to pct=0.20 (<0.50), BADSHORT decayed to pct=0.75 (>0.50),
	// GOODLONG stays strong (pct=0.92), NEWLONG is top (pct=0.95, weight=0.60).
	pctDay1 := map[string]float64{"GOODLONGUSDT": 0.92, "BADLONGUSDT": 0.20, "BADSHORTUSDT": 0.75, "NEWLONGUSDT": 0.95}
	weightsDay1 := map[string]float64{"GOODLONGUSDT": 0.30, "NEWLONGUSDT": 0.60}
	cfg.TargetsPath = fqWriteTargetsMap(t, dir, "crypto", "2026-10-29", 3, weightsDay1, pctDay1, nil)

	day1 := fqDay("2026-10-30")
	at.factorQuantTick(b, day1)

	// BADLONG and BADSHORT must be closed immediately!
	if _, ok := f.pos["BADLONGUSDT:long"]; ok {
		t.Fatalf("decayed long BADLONGUSDT was not closed early")
	}
	if _, ok := f.pos["BADSHORTUSDT:short"]; ok {
		t.Fatalf("decayed short BADSHORTUSDT was not closed early")
	}
	// GOODLONG must still be held!
	if _, ok := f.pos["GOODLONGUSDT:long"]; !ok {
		t.Fatalf("surviving long GOODLONGUSDT was unexpectedly closed")
	}
	// NEWLONG must be opened at 1/H = 1/3 of its target weight (0.60 / 3 = 0.20 => notional 2000 => qty 20 @ 100)!
	newPos, ok := f.pos["NEWLONGUSDT:long"]
	if !ok {
		t.Fatalf("replacement tranche NEWLONGUSDT was not opened")
	}
	fqNear(t, newPos.qty*100, 2000, "1/H rolling replacement notional")

	// LastAsOf must remain 2026-10-28 so the scheduled Day 3 full rebalance is preserved.
	state, _ := st.FactorQuant().Get(at.id)
	if state == nil || state.LastAsOf != "2026-10-28" {
		t.Fatalf("LastAsOf changed during rolling exit: %+v", state)
	}
	rs := at.factorQuantRiskStatus()
	if rs.FactorQuant == nil || rs.FactorQuant.LastRollingAsOf != "2026-10-29" || rs.FactorQuant.LastTick.Code != "rolling_rebalance" {
		t.Fatalf("unexpected run status after rolling exit: %+v", rs.FactorQuant)
	}

	// Next 5-minute tick with same as_of=2026-10-29 must not repeat rolling rebalance.
	callsBefore := len(f.calls)
	at.factorQuantTick(b, day1.Add(5*time.Minute))
	if len(f.calls) != callsBefore {
		t.Fatalf("same intermediate as_of repeated rolling orders: %v", f.calls[callsBefore:])
	}
}

func TestFQTieredDrawdownScale(t *testing.T) {
	cfg := &store.FactorQuantConfig{Market: "crypto"}
	for _, tc := range []struct {
		dd, want float64
	}{
		{0.00, 1.00},
		{0.09, 1.00},
		{0.10, 0.70},
		{0.14, 0.70},
		{0.15, 0.40},
		{0.19, 0.40},
		{0.20, 0.25},
		{0.24, 0.25},
	} {
		if got := fqDrawdownScale(tc.dd, cfg); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("fqDrawdownScale(%v) = %v, want %v", tc.dd, got, tc.want)
		}
	}
	off := false
	cfgOff := &store.FactorQuantConfig{Market: "crypto", DrawdownScale: &off}
	if got := fqDrawdownScale(0.18, cfgOff); got != 1.0 {
		t.Fatalf("disabled DrawdownScale returned %v, want 1.0", got)
	}

	// End-to-end rebalance at 16% drawdown (peak=10000, equity=8400 => dd=16% => scale=0.40):
	at, f, b := fqRiskTrader(t, "fq-dd-scale")
	fcfg := at.config.StrategyConfig.FactorQuantConfig
	fcfg.DailyLossPct = fqPtr(0)
	now := fqDay("2026-11-01")
	at.factorQuantTick(b, now) // seeds peak = 10000

	f.equity = 8400 // 16% drawdown -> 0.40x scale -> budget = 8400 * 0.40 = 3360
	fcfg.TargetsPath = writeTargetsFile(t, filepath.Dir(fcfg.TargetsPath), targetsJSON("crypto", "ok", "2026-11-03", 3, `{"NEWUSDT":0.5}`))
	at.factorQuantTick(b, fqDay("2026-11-04"))
	pos := f.pos["NEWUSDT:long"]
	if pos == nil {
		t.Fatalf("expected NEWUSDT opened with scaled budget")
	}
	// Target weight 0.5 * budget 3360 = 1680 notional (@ price 100 => qty 16.8).
	fqNear(t, pos.qty*100, 1680, "tiered drawdown scaled notional (0.40x)")
	if scale := at.factorQuantRiskStatus().FactorQuant.DrawdownScale; math.Abs(scale-0.40) > 1e-6 {
		t.Fatalf("RiskStatus DrawdownScale = %v, want 0.40", scale)
	}
}

func TestFQRollingFailedCloseRetriesAndCapsReplacement(t *testing.T) {
	at, f, b := fqRiskTrader(t, "rolling-retry-cap")
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.DailyLossPct = fqPtr(0)
	at.factorQuantTick(b, fqDay("2026-11-01"))
	f.pos["NEWUSDT:long"] = &fakePos{"NEWUSDT", fqSideLong, 98, 100}
	f.pos["XUSDT:long"] = &fakePos{"XUSDT", fqSideLong, 2, 100}
	cfg.TargetsPath = fqWriteTargetsMap(t, t.TempDir(), "crypto", "2026-11-01", 3, map[string]float64{"NEWUSDT": .5, "YUSDT": .5}, map[string]float64{"NEWUSDT": .9, "XUSDT": .1, "YUSDT": .95}, nil)
	f.failClose["XUSDT"] = true
	at.factorQuantTick(b, fqDay("2026-11-02"))
	if at.fq.lastRollingAsOf != "" {
		t.Fatal("failed rolling marked complete")
	}
	f.failClose["XUSDT"] = false
	at.factorQuantTick(b, fqDay("2026-11-02").Add(5*time.Minute))
	if _, ok := f.pos["XUSDT:long"]; ok {
		t.Fatal("failed decay exit not retried")
	}
	gross := 0.0
	for _, p := range f.pos {
		gross += p.qty * 100
	}
	if gross > 10000.01 {
		t.Fatalf("replacement exceeded budget: %g", gross)
	}
}

func TestFQAdaptiveStopUSFollowFileHorizon(t *testing.T) {
	root := t.TempDir()
	dailyDir := filepath.Join(root, "daily")
	start := fqDay("2026-08-01")
	const nDays = 60
	asOf := start.AddDate(0, 0, nDays-1).Format(factorDateFmt)

	// ~2.0% daily vol US stock:
	//   h=5  => 2.5 * 0.02 * sqrt(5)  ~ 11.2% (clamped to [6%, 18%])
	//   h=28 => 2.5 * 0.02 * sqrt(28) ~ 26.5% (clamped to [12%, 30%])
	closes := make([]float64, nDays)
	for d := 0; d < nDays; d++ {
		if d%2 == 0 {
			closes[d] = 100.0 * 1.01
		} else {
			closes[d] = 100.0 * 0.99
		}
	}
	fqWriteDailyJSON(t, dailyDir, "NVDA.json", start, closes)
	path := fqWriteTargetsMap(t, root, "us", asOf, 28, map[string]float64{"NVDABUSDT": 0.5}, map[string]float64{"NVDABUSDT": 0.95}, nil)

	// Follow-file config: Horizon == 0.
	cfgFollow := &store.FactorQuantConfig{Market: store.FactorQuantMarketUS, Horizon: 0, TargetsPath: path}

	// Explicit 5d fallback (when targetHorizon omitted) should stay tight (~11.2% -> ~88.8).
	stop5, _ := fqComputeAdaptiveStopPrice("NVDABUSDT", fqSideLong, 100, 100, 100, 0.20, cfgFollow, asOf)
	if stop5 < 85.0 || stop5 > 92.0 {
		t.Fatalf("fallback h=5 stop = %v, want in [85, 92]", stop5)
	}

	// Passing loaded targetHorizon=28 widens stop to ~26.5% (~73.5) within [12%, 30%].
	stop28, _ := fqComputeAdaptiveStopPrice("NVDABUSDT", fqSideLong, 100, 100, 100, 0.20, cfgFollow, asOf, 28)
	if stop28 < 70.0 || stop28 > 78.0 {
		t.Fatalf("follow-file h=28 stop = %v, want in [70, 78]", stop28)
	}

	// End-to-end US paper trader with Horizon=0 and targets.horizon=28: a -15% dip must NOT stop out.
	quotes := map[string]float64{"NVDABUSDT": 100}
	fqStubUS(t, quotes)
	st := fqTestStore(t)
	at := fqPaperTrader(t, st, "fq-us-h28-stop", "us", &paperTestPrices{mark: 100})
	cfg := at.config.StrategyConfig.FactorQuantConfig
	cfg.Horizon = 0
	cfg.USFeeBps, cfg.USSlippageBps = fqPtr(0), fqPtr(0)
	cfg.TargetsPath = path
	b, err := at.newFactorQuantBroker()
	if err != nil {
		t.Fatal(err)
	}
	day := start.AddDate(0, 0, nDays)
	at.fqNow = func() time.Time { return day }
	at.factorQuantTick(b, day)

	if gotH := at.fqTargetsHorizon(); gotH != 28 {
		t.Fatalf("fqTargetsHorizon() = %d, want 28", gotH)
	}

	// Drop 15% (to 85): would have stopped out under old h=5 (~88.8), must stay open under h=28 (~73.5).
	quotes["NVDABUSDT"] = 85
	at.factorQuantTick(b, day.Add(5*time.Minute))
	open, _ := st.StockPaper().ListOpen(at.id)
	if len(open) != 1 {
		t.Fatalf("expected NVDABUSDT still open after -15%% dip under h=28 stop, got %+v", open)
	}
}

