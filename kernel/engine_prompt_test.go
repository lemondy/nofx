package kernel

import (
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

// The short-scan funding crowding threshold configured in the strategy UI
// must be injected into the user prompt (both percent and decimal forms) so
// the AI's entry rules follow the config instead of hard-coded numbers.
func TestBuildUserPromptInjectsFundingThreshold(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.CoinSource.SourceType = "short_scan"
	cfg.CoinSource.UseShortScan = true
	cfg.CoinSource.ShortScanFundingRatePct = 0.05

	engine := NewStrategyEngine(cfg)
	ctx := &Context{MarketDataMap: map[string]*market.Data{}}

	prompt := engine.BuildUserPrompt(ctx)
	i := strings.Index(prompt, "Strategy Parameters")
	if i < 0 {
		t.Fatalf("Strategy Parameters block missing from prompt:\n%s", prompt)
	}
	seg := prompt[i:]
	// Annualized figure (0.05% × 3×365 = 54.8%), the per-settlement form, the
	// either/or structure, and the near_high exemption must all render.
	for _, want := range []string{"0.0500%", "54.8%", "funding_rollover", "near_high"} {
		if !strings.Contains(seg, want) {
			t.Fatalf("prompt block missing %q:\n%s", want, seg[:400])
		}
	}
}

// Unset threshold falls back to the built-in default (0.03%).
func TestBuildUserPromptFundingThresholdDefault(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.CoinSource.SourceType = "short_scan"

	engine := NewStrategyEngine(cfg)
	prompt := engine.BuildUserPrompt(&Context{MarketDataMap: map[string]*market.Data{}})
	if !strings.Contains(prompt, "0.0300%") {
		t.Fatalf("default funding threshold 0.0300%% not injected:\n%s", prompt)
	}
}

// The block must not appear for strategies that don't use the short scan.
func TestBuildUserPromptNoFundingBlockWithoutShortScan(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.CoinSource.SourceType = "ai500"

	engine := NewStrategyEngine(cfg)
	if strings.Contains(engine.BuildUserPrompt(&Context{MarketDataMap: map[string]*market.Data{}}), "做空资金费率拥挤阈值") {
		t.Fatalf("funding block injected for a non-short-scan strategy")
	}
}

// The per-coin prompt is JSON-only: no Summary narrative, no Quantitative
// Data text block — the structured fields carry everything.
func TestPerCoinPromptIsJSONOnly(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.Indicators.EnableQuantOI = true
	engine := NewStrategyEngine(cfg)

	now := time.Now()
	bars := 40
	tfData := &market.TimeframeSeriesData{Timeframe: "1h"}
	p := 5.0
	for i := 0; i < bars; i++ {
		p *= 1.002
		tfData.Klines = append(tfData.Klines, market.KlineBar{
			Time: now.Add(time.Duration(i-bars) * time.Hour).UnixMilli(),
			Open: p * 0.999, High: p * 1.002, Low: p * 0.998, Close: p, Volume: 1000 + float64(i),
		})
	}
	data := &market.Data{
		Symbol: "TESTUSDT", CurrentPrice: p,
		TimeframeData: map[string]*market.TimeframeSeriesData{"1h": tfData},
	}
	out := engine.formatMarketData(data, &QuantData{
		Symbol:      "TESTUSDT",
		PriceChange: map[string]float64{"24h": 0.09},
		OI:          map[string]*OIData{"binance": {CurrentOI: 1234.5}},
	}, nil, nil)

	if strings.Contains(out, "## Summary") || strings.Contains(out, "Quantitative Data") {
		t.Fatalf("Summary/Quant text must be gone:\n%s", out)
	}
	if !strings.Contains(out, `"volume":`) || !strings.Contains(out, `"volume_change_pct":`) {
		t.Fatal("volume fields missing from JSON")
	}
	if !strings.Contains(out, `"price_change_24h_live_pct":`) {
		t.Fatal("24h fold into derivatives missing")
	}
	if !strings.Contains(out, `"oi_current_base":1234.5`) {
		t.Fatal("OI current fold missing")
	}
	// The conflict-resolution instruction is part of the static legend; the
	// flag itself only appears on real conflicts (all-up structure → none).
	if !strings.Contains(out, "MUST resolve the conflict explicitly") {
		t.Fatal("conflict instruction legend missing")
	}
	if strings.Contains(out, `"directional_conflict":true`) {
		t.Fatal("all-up structure must not flag a conflict")
	}
}

// A data-incomplete symbol carries the DO-NOT bar — its quantitative numbers
// must be hidden too, so a flashy "+56% 24h" cannot tempt an override.
func TestIncompleteSymbolHidesQuantData(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.Indicators.EnableQuantOI = true
	engine := NewStrategyEngine(cfg)

	now := time.Now()
	// Only 3 bars → data incomplete for this timeframe.
	var bars []market.KlineBar
	for i := 0; i < 3; i++ {
		c := 1.0 + float64(i)*0.001
		bars = append(bars, market.KlineBar{
			Time: now.Add(time.Duration(i-3) * time.Hour).UnixMilli(),
			Open: c, High: c * 1.001, Low: c * 0.999, Close: c, Volume: 1000,
		})
	}
	data := &market.Data{
		Symbol: "MARSCOINUSDT", CurrentPrice: 1.002,
		TimeframeData: map[string]*market.TimeframeSeriesData{
			"1h": {Timeframe: "1h", Klines: bars},
		},
	}
	out := engine.formatMarketData(data, &QuantData{
		Symbol:      "MARSCOINUSDT",
		PriceChange: map[string]float64{"1h": 0.068688, "24h": 0.565920},
	}, nil, nil)

	if !strings.Contains(out, "DO NOT trade this symbol this cycle") {
		t.Fatalf("expected the DO-NOT bar, got: %q", out)
	}
	if strings.Contains(out, "Quantitative Data") {
		t.Fatalf("quant numbers must be hidden for barred symbols, got: %q", out)
	}
}

// The stop band wording must match validateOpenRisk EXACTLY: the cap is
// max(2×ATR(4h), 8%) — wide-of-the-two, never min — and the noise floor
// clause only renders when the strategy actually enables one (SLMinATRMult>0;
// the executor enforces no floor at mult<=0). The old wording listed
// "且 d ≤ 8%" AND "上限取 max(...)" simultaneously — mathematically
// contradictory (min vs max) and it flipped VTHOUSDT's verdict depending on
// which sentence the model believed.
func TestStopBandWordingMatchesExecutor(t *testing.T) {
	// Strategy WITH a noise floor (1.5×ATR(1h), e.g. Balanced/做空).
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.SLMinATRMult = 1.5
	engine := NewStrategyEngine(cfg)
	prompt := engine.BuildUserPrompt(&Context{MarketDataMap: map[string]*market.Data{}})
	if !strings.Contains(prompt, "d ≥ 1.5×ATR(1h)(噪声下限)且 d ≤ 上限") {
		t.Fatalf("floor variant missing floor clause:\n%s", extractStopLine(prompt))
	}
	for _, want := range []string{"上限 = max(2×ATR(4h), 8%)", "取宽不取窄"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("floor variant missing %q:\n%s", want, extractStopLine(prompt))
		}
	}
	if strings.Contains(prompt, "且 d ≤ 8%") {
		t.Fatalf("contradictory min-style clause still present:\n%s", extractStopLine(prompt))
	}

	// Strategy WITHOUT a floor (SLMinATRMult=0, e.g. Conservative): the prompt
	// must NOT invent the 1.5 default — the executor enforces no floor.
	cfg0 := &store.StrategyConfig{}
	engine0 := NewStrategyEngine(cfg0)
	prompt0 := engine0.BuildUserPrompt(&Context{MarketDataMap: map[string]*market.Data{}})
	if !strings.Contains(prompt0, "只需满足: d ≤ 上限") || !strings.Contains(prompt0, "未启用噪声下限") {
		t.Fatalf("floor-less variant wrong:\n%s", extractStopLine(prompt0))
	}
	if strings.Contains(prompt0, "d ≥ 1.5×ATR(1h)") {
		t.Fatalf("floor-less strategy must not show a phantom 1.5×ATR floor:\n%s", extractStopLine(prompt0))
	}
	if strings.Contains(prompt0, "且 d ≤ 8%") {
		t.Fatalf("contradictory min-style clause still present:\n%s", extractStopLine(prompt0))
	}
}

func extractStopLine(prompt string) string {
	i := strings.Index(prompt, "止损(程序校验)")
	if i < 0 {
		return "(stop-loss line missing)"
	}
	j := strings.Index(prompt[i:], "\n")
	if j < 0 {
		return prompt[i:]
	}
	return prompt[i : i+j]
}

// TP rule must force a FULL scan of the opposing-structure arrays across all
// role timeframes before any "no usable structure" verdict: on BZUSDT the
// model read resistance[0] (103.49), declared "再远无任何历史结构" while
// resistance[1] (104.297) sat right there in the 15m block — the skip was
// right by luck (RR 1.14 < 1.5), not by process.
func TestBuildUserPromptTPRequiresFullArrayScan(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.MinRiskRewardRatio = 1.5
	engine := NewStrategyEngine(cfg)
	prompt := engine.BuildUserPrompt(&Context{MarketDataMap: map[string]*market.Data{}})
	for _, want := range []string{
		"无条件适用", // scan is the PRIMARY algorithm, not a remedial branch (ZEC 09-13: nearer passing 15m level skipped for the trend_tf level)
		"15m/1h/4h **全部** resistance/support 数组元素",
		"15m 不在 role_tfs.trend_tf 里也必须纳入遍历",
		"第一个 RR≥1.5",
		"属于违规选位",
		"不只看数组第一项",
		"无可用结构位",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("TP rule missing %q", want)
		}
	}
}

// Market entries are ONLY permitted as the breakout-chase exception with all
// six conditions spelled out (user spec 09-11): breakout.status confirmed,
// volume+OI confirmation, directional_score>=80, no directional conflict, no
// loss-streak ban. Renders with limit-entry enabled; absent when disabled
// (market is then unrestricted by prompt).
func TestBreakoutChaseExceptionWording(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.LimitEntryEnabled = true
	engine := NewStrategyEngine(cfg)
	prompt := engine.BuildSystemPrompt(100, "")
	for _, want := range []string{
		"例外一(突破追入)",
		"例外二(布林上轨骑行)",
		"`bb_ride.ride`=true",
		"布林上轨骑行",
		"`bb_ride.ride`=true",
		"`breakout.status`=\"confirmed\"",
		"`breakout.volume_confirmation`=true",
		"`breakout.oi_confirmation`=true",
		"`directional_score`≥80",
		"`signal_conflict.directional_conflict`=false",
		"连亏禁开仓期",
		"任一例外不满足仍必须用限价单",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("breakout-chase exception missing %q", want)
		}
	}

	cfgOff := &store.StrategyConfig{}
	cfgOff.RiskControl.LimitEntryEnabled = false
	engineOff := NewStrategyEngine(cfgOff)
	promptOff := engineOff.BuildSystemPrompt(100, "")
	if strings.Contains(promptOff, "仅限突破追入例外") {
		t.Fatal("breakout-chase exception must not render when limit entry is disabled")
	}
}

// TP ladder thresholds resolve: 0/unset → spec defaults (trim 10 / full 25),
// negative → tier off.
func TestTpTierAction(t *testing.T) {
	// Default rc: the 1R profit lock (1.0) is active → the ROE trim tier is
	// superseded; only the full-close tier remains.
	rc := &store.RiskControlConfig{}
	if got := TpTierAction(10, false, rc); got != "" {
		t.Fatalf("lock active: ROE trim tier must yield, got %q", got)
	}
	if got := TpTierAction(25, true, rc); got != "full" {
		t.Fatalf("25%% → full close backstop, got %q", got)
	}
	// Lock disabled (negative) → the ROE trim tier works again.
	offLock := &store.RiskControlConfig{ProfitLockAtR: -1}
	if got := TpTierAction(10, false, offLock); got != "trim" {
		t.Fatalf("lock disabled: 10%% with unspent trim → trim, got %q", got)
	}
	// Both tiers disabled via negative config.
	off := &store.RiskControlConfig{ProfitLockAtR: -1, TpTrimProfitPct: -1, TpFullProfitPct: -1}
	if got := TpTierAction(50, false, off); got != "" {
		t.Fatalf("disabled tiers must no-op, got %q", got)
	}
	// Prompt renders the ladder and drops the frequency-cap wording.
	engine := NewStrategyEngine(&store.StrategyConfig{})
	prompt := engine.BuildUserPrompt(&Context{MarketDataMap: map[string]*market.Data{}})
	for _, want := range []string{"程序自动市价减仓 50%", "止损移至开仓价保本", "程序自动全部平仓"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("TP ladder missing %q", want)
		}
	}
	sp := engine.BuildSystemPrompt(100, "")
	if strings.Contains(sp, "2 trades/hour = overtrading") || strings.Contains(sp, "Trading Frequency Awareness") {
		t.Fatal("frequency-cap wording must be gone")
	}
	if !strings.Contains(sp, "No frequency cap") {
		t.Fatal("no-frequency-cap statement missing")
	}
}

// Hold-on-position decisions must carry the management self-assessment
// contract (quality + flags), mirroring entry_quality for the backtest.
func TestBuildUserPromptManagementFields(t *testing.T) {
	engine := NewStrategyEngine(&store.StrategyConfig{})
	prompt := engine.BuildSystemPrompt(100, "")
	for _, want := range []string{
		"management_quality", "management_flags",
		"BREAKEVEN_WARRANTED", "PARTIAL_WARRANTED",
		"该考虑离场", "正确动作是输出 adjust_stop_loss / partial_close_*",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("management contract missing %q", want)
		}
	}
}

// Rally: a downtrend bounce must classify as rally; the timing-gate doc and
// the direction-aware buffer hint must reflect the symmetric policy.
func TestBuildUserPromptRallyWindow(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.EntryTimingGate = true
	engine := NewStrategyEngine(cfg)
	prompt := engine.BuildUserPrompt(&Context{MarketDataMap: map[string]*market.Data{}})
	for _, want := range []string{
		"做空需 down/rally(下跌趋势中的反弹=空头入场窗)",
		"空单——尤其反弹追空/急跌追空——取上半段 0.4-0.5",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("rally window prompt missing %q", want)
		}
	}
}
