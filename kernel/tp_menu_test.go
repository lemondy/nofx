package kernel

import (
	"strings"
	"testing"
	"time"

	"nofx/store"
)

// menuTFs builds a signal whose resistance ladder gives `levels` qualifying
// (RR ≥ minRR at the given stopPct) non-BOLL targets for a long from entry.
func menuSig(symbol string, levels []float64) *SymbolSignal {
	return &SymbolSignal{
		Symbol: symbol,
		Price:  100,
		Timeframes: map[string]*TFSignal{
			"15m": {StructuralResistance: levels, ATRPct: 1},
			"1h":  {ATRPct: 2},
			"4h":  {ATRPct: 4},
		},
	}
}

// The TP menu (user directive 09-29): among the qualifying non-BOLL targets
// the scan must present near/mid/far — option 1 IS first_rr_ge_target with
// the Default flag, RRs are ascending, and BOLL-sourced levels never enter
// the menu (a decaying band must not become the adopted TP).
func TestScanRRMenuNearMidFar(t *testing.T) {
	sig := menuSig("BTCUSDT", []float64{106, 110, 116, 124, 130})
	sig.Timeframes["1h"].StructuralResistance = []float64{140}
	sh := 138.0
	sig.Timeframes["4h"].StructureHigh = &sh
	sig.touchHighs = []float64{141, 107, 125}
	sig.touchLows = []float64{95, 99, 101}
	sig.touchWindowDays = 30

	out := scanRRForSymbol(sig, 100, "live_price", 5, 95, true, 1)
	if !out.Usable || out.FirstRRGeTarget != 106 {
		t.Fatalf("usable=%v first=%v, want first=106", out.Usable, out.FirstRRGeTarget)
	}
	if len(out.Options) != 3 {
		t.Fatalf("options = %d (%+v), want 3 (near/mid/far)", len(out.Options), out.Options)
	}
	if out.Options[0].Level != 106 || !out.Options[0].Default {
		t.Fatalf("option 1 = %+v, want level 106 with Default", out.Options[0])
	}
	if out.Options[1].Level != 124 || out.Options[2].Level != 140 {
		t.Fatalf("mid/far = %g/%g, want 124/140 (mid qualifying index + global structure extreme)", out.Options[1].Level, out.Options[2].Level)
	}
	for i := 1; i < len(out.Options); i++ {
		if out.Options[i].RR <= out.Options[i-1].RR {
			t.Fatalf("option RR not ascending: %v", out.Options)
		}
		if out.Options[i].Default {
			t.Fatalf("only option 1 may carry Default: %v", out.Options)
		}
	}
	if got := touchCount(sig.touchHighs, sig.touchLows, 106, true); got != 3 {
		t.Fatalf("touchCount(106) = %d, want 3 (bars 141/107/125 all reach)", got)
	}
	if got := touchCount(sig.touchHighs, sig.touchLows, 126, true); got != 1 {
		t.Fatalf("touchCount(126) = %d, want 1 (only the 141 bar reaches)", got)
	}
	if got := touchCount(sig.touchHighs, sig.touchLows, 95, false); got != 1 {
		t.Fatalf("short-side touchCount(95) = %d, want 1 (only the low-95 bar reaches)", got)
	}
	if got := touchCount(sig.touchHighs, sig.touchLows, 101, false); got != 3 {
		t.Fatalf("short-side touchCount(101) = %d, want 3 (all lows at/below)", got)
	}
	if out.Options[0].TouchCount != 3 {
		t.Fatalf("option 1 touch count = %d, want 3", out.Options[0].TouchCount)
	}
	if out.Options[0].TouchWindowDays != 30 {
		t.Fatalf("touch window = %d, want 30", out.Options[0].TouchWindowDays)
	}
	// 140 sits above the 4h structure_high (138) → the far option must flag
	// "no historical reference".
	if !out.Options[2].BeyondStructure {
		t.Fatalf("far option 140 above structure_high 138 must be flagged beyond_structure")
	}
}

// Two qualifying targets → exactly two options; one → one. Never pad.
func TestScanRRMenuShapeFollowsQualifyingCount(t *testing.T) {
	two := scanRRForSymbol(menuSig("BTCUSDT", []float64{106, 112}), 100, "live_price", 5, 95, true, 1)
	if len(two.Options) != 2 || two.Options[0].Level != 106 || two.Options[1].Level != 112 {
		t.Fatalf("two-target menu = %+v, want [106 112]", two.Options)
	}
	one := scanRRForSymbol(menuSig("BTCUSDT", []float64{106}), 100, "live_price", 5, 95, true, 1)
	if len(one.Options) != 1 || !one.Options[0].Default {
		t.Fatalf("single-target menu = %+v, want [106] with Default", one.Options)
	}
	// RR below min_rr → nothing qualifying → no menu, unusable (wait).
	none := scanRRForSymbol(menuSig("BTCUSDT", []float64{103}), 100, "live_price", 5, 95, true, 1)
	if none.Usable || len(none.Options) != 0 {
		t.Fatalf("sub-minRR scan must stay unusable with no menu, got usable=%v opts=%v", none.Usable, none.Options)
	}
}

// BOLL-sourced levels stay visible for best_rr but never become menu options
// (QUANT_REVIEW C1: a decaying band must not anchor the adopted TP).
func TestScanRRMenuExcludesBOLLOnly(t *testing.T) {
	sig := menuSig("BTCUSDT", []float64{120})
	tf := sig.Timeframes["15m"]
	tf.Resistance = []float64{106, 120}
	tf.BOLLSourced = map[float64]bool{106: true, 120: false}
	tf.StructuralResistance = nil // manual signals: displayed levels only

	out := scanRRForSymbol(sig, 100, "live_price", 5, 95, true, 1)
	for _, opt := range out.Options {
		if opt.Level == 106 {
			t.Fatalf("BOLL-sourced 106 must not be a menu option: %+v", out.Options)
		}
	}
	if len(out.Options) != 1 || out.Options[0].Level != 120 {
		t.Fatalf("menu = %+v, want only the real structure 120", out.Options)
	}
}

// validateDecision hygiene (09-29): unknown exit_mode strips to trend;
// out-of-menu tp_option resets to 0 (= option 1); in-menu values survive.
func TestValidateDecisionExitModeAndOption(t *testing.T) {
	gs := &GateState{
		LongTPMenu: []TPOption{{Level: 106}, {Level: 116}, {Level: 140}},
	}
	norm := func(d *Decision) {
		_ = validateDecision(d, 1000, 5, 3, 0.5, 0.3, 5, false, gs)
	}

	d := &Decision{Symbol: "BTCUSDT", Action: "open_long", ExitMode: "sideways", TPOption: 7}
	norm(d)
	if d.ExitMode != ExitModeTrend {
		t.Fatalf("unknown exit_mode = %q, want stripped to trend", d.ExitMode)
	}
	if d.TPOption != 0 {
		t.Fatalf("beyond-menu tp_option = %d, want reset to 0", d.TPOption)
	}

	d = &Decision{Symbol: "BTCUSDT", Action: "open_long", ExitMode: "quick", TPOption: 3}
	norm(d)
	if d.ExitMode != ExitModeQuick || d.TPOption != 3 {
		t.Fatalf("valid fields mangled: mode=%q opt=%d", d.ExitMode, d.TPOption)
	}

	d = &Decision{Symbol: "BTCUSDT", Action: "open_long"}
	norm(d)
	if d.ExitMode != ExitModeTrend {
		t.Fatalf("absent exit_mode must default to trend, got %q", d.ExitMode)
	}
}

// resolveTakeProfitOptions + correctTakeProfitToPlan: choosing option N must
// land the executed TP on THAT menu level (the snap target follows the
// choice); absent choice keeps option 1 — the legacy first_rr_ge_target.
func TestResolveTakeProfitOptionSnapsToChoice(t *testing.T) {
	freshGates := func() map[string]*GateState {
		return map[string]*GateState{"BTCUSDT": {
			LongStopPlanPrice: 95,
			LongTakeProfit:    106, // option 1
			LongTPMenu:        []TPOption{{Level: 106, RR: 1.2}, {Level: 116, RR: 2.1}, {Level: 140, RR: 4.5}},
		}}
	}

	// Model picks option 2 and echoes a sloppy 115.8 — snap must land on 116.
	gates := freshGates()
	decisions := []Decision{{Symbol: "BTCUSDT", Action: "open_long", TPOption: 2, StopLoss: 95, TakeProfit: 115.8}}
	resolveTakeProfitOptions(decisions, gates)
	correctTakeProfitToPlan(decisions, gates, StopPlanTolerancePct)
	if decisions[0].TakeProfit != 116 {
		t.Fatalf("tp_option=2 TP = %v, want snapped to menu level 116", decisions[0].TakeProfit)
	}
	if gates["BTCUSDT"].LongTakeProfit != 116 {
		t.Fatalf("gate TP = %v, want re-pointed to 116", gates["BTCUSDT"].LongTakeProfit)
	}

	// Placeholder-0 TP fills from the chosen option too.
	gates = freshGates()
	decisions = []Decision{{Symbol: "BTCUSDT", Action: "open_long", TPOption: 3, StopLoss: 95}}
	resolveTakeProfitOptions(decisions, gates)
	correctTakeProfitToPlan(decisions, gates, StopPlanTolerancePct)
	if decisions[0].TakeProfit != 140 {
		t.Fatalf("tp_option=3 placeholder TP = %v, want 140", decisions[0].TakeProfit)
	}

	// No tp_option: option 1 stays the snap target.
	gates = freshGates()
	decisions = []Decision{{Symbol: "BTCUSDT", Action: "open_long", StopLoss: 95, TakeProfit: 999}}
	resolveTakeProfitOptions(decisions, gates)
	correctTakeProfitToPlan(decisions, gates, StopPlanTolerancePct)
	if decisions[0].TakeProfit != 106 {
		t.Fatalf("default TP = %v, want option 1 = 106", decisions[0].TakeProfit)
	}
}

// Short mirror: the option resolves against ShortTPMenu/ShortTakeProfit.
func TestResolveTakeProfitOptionShortMirror(t *testing.T) {
	gates := map[string]*GateState{"BTCUSDT": {
		ShortStopPlanPrice: 105,
		ShortTakeProfit:    95,
		ShortTPMenu:        []TPOption{{Level: 95, RR: 1.1}, {Level: 88, RR: 2.4}},
	}}
	decisions := []Decision{{Symbol: "BTCUSDT", Action: "open_short", TPOption: 2, StopLoss: 105, TakeProfit: 88}}
	resolveTakeProfitOptions(decisions, gates)
	correctTakeProfitToPlan(decisions, gates, StopPlanTolerancePct)
	if decisions[0].TakeProfit != 88 {
		t.Fatalf("short tp_option=2 TP = %v, want 88", decisions[0].TakeProfit)
	}
}

func TestTimeStopHoursResolver(t *testing.T) {
	cases := []struct {
		v    int
		want int
	}{
		{0, 4},  // unset → spec default
		{-1, 0}, // negative → disabled
		{6, 6},
	}
	for _, c := range cases {
		rc := &store.RiskControlConfig{TimeStopHours: c.v}
		if got := TimeStopHours(rc); got != c.want {
			t.Fatalf("TimeStopHours(%d) = %d, want %d", c.v, got, c.want)
		}
	}
	if TimeStopHours(nil) != 4 {
		t.Fatal("nil config must default to 4h")
	}
}

// TPMenuEnabled defaults on; only an explicit false turns the menu off (the
// legacy verbatim contract renders again).
func TestTPMenuEnabledResolver(t *testing.T) {
	if !TPMenuEnabled(nil) {
		t.Fatal("nil config must enable the menu")
	}
	off := false
	if TPMenuEnabled(&store.RiskControlConfig{TPMenuEnabled: &off}) {
		t.Fatal("explicit false must disable the menu")
	}
	on := true
	if !TPMenuEnabled(&store.RiskControlConfig{TPMenuEnabled: &on}) {
		t.Fatal("explicit true must enable the menu")
	}
}

// The system prompt must teach the menu contract (option pick + exit modes)
// when enabled, and the verbatim contract when explicitly disabled.
func TestPromptMenuContract(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.MinRiskRewardRatio = 1.2
	engine := NewStrategyEngine(cfg)
	sp := engine.BuildSystemPrompt(100, "")
	for _, want := range []string{
		"rr_scan.tp_options",
		"tp_option",
		"exit_mode=trend",
		"exit_mode=range",
		"exit_mode=quick",
		"touch_count",
	} {
		if !strings.Contains(sp, want) {
			t.Fatalf("menu-on prompt missing %q", want)
		}
	}

	off := false
	cfg.RiskControl.TPMenuEnabled = &off
	legacy := NewStrategyEngine(cfg).BuildSystemPrompt(100, "")
	if !strings.Contains(legacy, "逐字复制 rr_scan.first_rr_ge_target") {
		t.Fatal("menu-off prompt must restore the verbatim first_rr_ge_target contract")
	}
	if strings.Contains(legacy, "exit_mode") {
		t.Fatal("menu-off prompt must not mention exit_mode")
	}
}

// compile-time guard: the exit-mode vocabulary stays closed.
func TestValidExitModesClosed(t *testing.T) {
	want := map[string]bool{ExitModeTrend: true, ExitModeRange: true, ExitModeQuick: true}
	if len(ValidExitModes) != len(want) {
		t.Fatalf("ValidExitModes = %v", ValidExitModes)
	}
	for _, m := range ValidExitModes {
		if !want[m] {
			t.Fatalf("unexpected exit mode %q", m)
		}
	}
	_ = time.Second // keep the time import honest if tests shrink
}
