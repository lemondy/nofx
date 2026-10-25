package stockengine

import (
	"nofx/market/usstock"
	"nofx/store"
	"testing"
)

func TestConfigDefaultsAndOverrides(t *testing.T) {
	for _, cfg := range []*store.StockConfig{nil, {}} {
		p := ResolvePreset(cfg)
		if p.Name != store.StockPresetSwing || p.TrendTF != usstock.TF1d || p.EntryTF != usstock.TF1h || p.MaxHoldDays != 30 || len(p.DecisionTimes) != 7 || p.DecisionTimes[0] != "09:45" || p.DecisionTimes[1] != "10:30" || p.DecisionTimes[3] != "12:30" || p.DecisionTimes[6] != "15:30" {
			t.Fatal(p)
		}
		near(t, p.StopATRMin, 1.5)
		near(t, p.StopATRMax, 3)
		if !allowRegular(cfg) || allowPreMarket(cfg) || allowAfterHours(cfg) || !yahooFallback(cfg) || !isPaper(cfg) {
			t.Fatal("boolean defaults")
		}
		near(t, effectiveMaxDivergencePct(cfg), 1)
		near(t, effectiveMaxPositionPct(cfg), 20)
		near(t, effectiveMaxTotalExposurePct(cfg), 80)
		near(t, effectiveRiskPerTradePct(cfg), 1)
		if effectiveMaxPositions(cfg) != 5 {
			t.Fatal("max positions default")
		}
	}
	f := false
	cfg := &store.StockConfig{Preset: store.StockPresetPosition, Sessions: store.StockSessions{Regular: &f, PreMarket: true, AfterHours: true}, DataFallbackYahoo: &f, PaperTrading: &f,
		MaxDivergencePct: -1, MaxPositionPct: 15, MaxTotalExposurePct: 60, MaxPositions: 3, RiskPerTradePct: 0.5, StopATRMin: 2.5}
	p := ResolvePreset(cfg)
	if p.TrendTF != usstock.TF1w || p.EntryTF != usstock.TF1d || p.MaxHoldDays != 0 || len(p.DecisionTimes) != 1 || p.DecisionTimes[0] != "15:30" {
		t.Fatal(p)
	}
	near(t, p.StopATRMin, 2.5)
	near(t, p.StopATRMax, 4)
	if allowRegular(cfg) || !allowPreMarket(cfg) || !allowAfterHours(cfg) || yahooFallback(cfg) || isPaper(cfg) {
		t.Fatal("overrides")
	}
	near(t, effectiveMaxDivergencePct(cfg), -1)
	near(t, effectiveMaxPositionPct(cfg), 15)
	near(t, effectiveMaxTotalExposurePct(cfg), 60)
	near(t, effectiveRiskPerTradePct(cfg), 0.5)
	if effectiveMaxPositions(cfg) != 3 {
		t.Fatal("max positions override")
	}
	cfg.StopATRMin, cfg.StopATRMax = 0, 3.5
	p = ResolvePreset(cfg)
	near(t, p.StopATRMin, 2)
	near(t, p.StopATRMax, 3.5)
}

func TestDecisionTimesDefaultHourlyAndOverride(t *testing.T) {
	want := []string{"09:45", "10:30", "11:30", "12:30", "13:30", "14:30", "15:30"}
	got := ResolvePreset(&store.StockConfig{}).DecisionTimes
	if len(got) != len(want) {
		t.Fatalf("swing default %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("swing default %v", got)
		}
	}
	// returned slice must not alias shared state
	got[0] = "00:00"
	if ResolvePreset(&store.StockConfig{}).DecisionTimes[0] != "09:45" {
		t.Fatal("default slots must be fresh per call")
	}
	old := ResolvePreset(&store.StockConfig{DecisionTimes: []string{"15:30", "09:45", "12:30", "12:30", "bad", "25:00", "9:5x", ""}}).DecisionTimes
	if len(old) != 3 || old[0] != "09:45" || old[1] != "12:30" || old[2] != "15:30" {
		t.Fatalf("override should be sorted/deduped/validated: %v", old)
	}
	if d := ResolvePreset(&store.StockConfig{DecisionTimes: []string{"nope"}}).DecisionTimes; len(d) != 7 {
		t.Fatalf("all-invalid override falls back to the preset default: %v", d)
	}
	pos := ResolvePreset(&store.StockConfig{Preset: store.StockPresetPosition}).DecisionTimes
	if len(pos) != 1 || pos[0] != "15:30" {
		t.Fatalf("position preset unchanged: %v", pos)
	}
}
