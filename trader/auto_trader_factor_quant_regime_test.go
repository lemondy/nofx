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
