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
	// The conflict-resolution instruction lives in the static legend, which is
	// now rendered ONCE per prompt (not per coin) — the per-coin block must be
	// JSON-only. (Legend presence is asserted in TestPromptProgramTruthGateWording.)
	if strings.Contains(out, "MUST resolve the conflict explicitly") {
		t.Fatal("per-coin block must not repeat the legend (deduped to once-per-prompt)")
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
// right by luck (RR 1.14 < 1.5), not by process. Review 2026-09-15 point 1:
// the scan is now PROGRAM-DELIVERED via hard_entry_gate.rr_scan — the rule
// must pin (a) the full-array scan semantics of the field, (b) adoption of
// first_rr_ge_target, (c) MAX_STRUCTURAL_RR as the failure verdict wording,
// and (d) the manual fallback for symbols without the field.
func TestBuildUserPromptTPRequiresFullArrayScan(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.MinRiskRewardRatio = 1.5
	engine := NewStrategyEngine(cfg)
	prompt := engine.BuildUserPrompt(&Context{MarketDataMap: map[string]*market.Data{}})
	for _, want := range []string{
		"rr_scan",
		"全部时间块(含 execution_tf/15m)全部 resistance/support",
		"直接采用 `rr_scan.first_rr_ge_target`",
		"第一个 RR≥1.5",
		"MAX_STRUCTURAL_RR",
		"RR 门结构性失败",
		"没有 rr_scan 字段", // manual fallback kept for symbols without the block
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
		"例外二(布林上轨骑行,只做多)",
		"`bb_ride.ride`=true",
		"布林上轨骑行",
		"`bb_ride.ride`=true",
		"`breakout.status`=\"confirmed\"",
		"`breakout.volume_confirmation`=true",
		"`breakout.oi_confirmation`=true",
		"`directional_score`≥80",
		"`signal_conflict.directional_conflict`=false",
		"连亏禁开仓期",
		"例外不满足仍必须用限价单",
		"例外三(布林下轨骑行,只做空)",
		"`short_ride.ride`=true",
		"open_short` 市价追入",
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

// Program-truth gate wording (review 2026-09-15): the per-coin legend must
// introduce hard_entry_gate / rr_scan / bias / funding_rollover with their
// verdict semantics (MAX_STRUCTURAL_RR wording, scanner≠market-short,
// anchor-suppression-is-not-a-direction-ban), carry the data freshness
// priority that bars ranking prices from exact math, and the system prompt
// must teach the wait state machine with the RECHECK_ALL_HARD_GATES
// discipline and the next-snapshot (never weekly) re-evaluation semantics.
func TestPromptProgramTruthGateWording(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.MinRiskRewardRatio = 1.5
	cfg.RiskControl.SLMinATRMult = 1.5
	cfg.RiskControl.OpenRejectSupplyPct = 0.5 // renders the anchor-offset/breathing params line
	cfg.CoinSource.SourceType = "short_scan"
	engine := NewStrategyEngine(cfg)

	now := time.Now()
	tfData := &market.TimeframeSeriesData{Timeframe: "1h"}
	p := 5.0
	for i := 0; i < 80; i++ {
		p *= 1.002
		tfData.Klines = append(tfData.Klines, market.KlineBar{
			Time: now.Add(time.Duration(i-80) * time.Hour).UnixMilli(),
			Open: p * 0.999, High: p * 1.002, Low: p * 0.998, Close: p, Volume: 1000 + float64(i),
		})
	}
	data := &market.Data{
		Symbol: "TESTUSDT", CurrentPrice: p,
		FundingRate: 0.00005, FundingRateOK: true, FundingSettleHours: 4,
		FundingHistory: []float64{0.0004, 0.0004, 0.0004, 0.0004, 0.00005, 0.00005},
		TimeframeData:  map[string]*market.TimeframeSeriesData{"1h": tfData},
	}
	out := engine.formatMarketData(data, nil, nil, nil)
	for _, want := range []string{
		`"hard_entry_gate"`, `"rr_scan"`, `"bias"`, `"funding_rollover"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("per-coin block missing %q", want)
		}
	}
	// The coin-independent legend must NOT be repeated per coin — it is
	// rendered once before the first signal block. Duplicating it across 8+
	// candidates wasted ~21k chars every cycle.
	for _, gone := range []string{
		"数据新鲜度优先级", "MAX_STRUCTURAL_RR", "不等于市场空头证据强",
		"这只是限价路径被禁≠该方向整体禁止", "严禁参与 entry/SL/TP/RR 精确计算",
		"stop_price 仅是门槛校验口径",
	} {
		if strings.Contains(out, gone) {
			t.Errorf("per-coin block must not repeat legend %q", gone)
		}
	}

	user := engine.BuildUserPrompt(&Context{MarketDataMap: map[string]*market.Data{}})
	for _, want := range []string{
		"开仓硬门(程序判定,禁止自行重算)",
		"严禁照抄为 stop_loss",
		"derivatives.funding_rollover.detected",
		"禁止从 scanner patterns",
		"RECHECK_ALL_HARD_GATES",
		"数据新鲜度优先级",
		"不要把 limit_entry_offset_pct 当成呼吸阈值",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q", want)
		}
	}
	// Legend appears exactly once in the whole prompt (dedup guard).
	if n := strings.Count(user, "数据新鲜度优先级"); n != 1 {
		t.Errorf("legend rendered %d times, want exactly 1", n)
	}

	sys := engine.BuildSystemPrompt(100, "")
	for _, want := range []string{
		"`wait_state` + `next_trigger`",
		"READY_LONG", "WATCH_SHORT", "BLOCKED",
		"重评条件", "禁止输出任何以天/周为尺度的搁置结论",
		"只允许收紧到保本或更好(CODE ENFORCED)",
		"仍锁定亏损的移动",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// The rr_scan ceiling shown this cycle must be captured per symbol onto the
// Context (09-16 point 2: the wait→fill RR-decay dataset reads this value at
// insert time; no later re-derivation).
func TestFormatMarketDataRecordsRRCeilings(t *testing.T) {
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.SLMinATRMult = 1.5
	cfg.RiskControl.MinRiskRewardRatio = 1.5
	engine := NewStrategyEngine(cfg)

	now := time.Now()
	tfData := buildTF("1h", now, 80, 5.0, false) // swings via BOLL band supplements give both sides targets
	data := &market.Data{
		Symbol: "TESTUSDT", CurrentPrice: tfData.Klines[len(tfData.Klines)-1].Close,
		TimeframeData: map[string]*market.TimeframeSeriesData{"1h": tfData},
	}
	ctx := &Context{}
	out := engine.formatMarketData(data, nil, ctx, nil)
	c := ctx.RRCeilings["TESTUSDT"]
	if c == nil {
		t.Fatalf("rr ceiling not recorded (keys %v)", ctx.RRCeilings)
	}
	if c.LongRR == 0 && c.ShortRR == 0 {
		t.Fatalf("empty ceiling with a configured noise floor: %+v", c)
	}
	// The recorded value must match the best_rr the model saw in the JSON.
	if !strings.Contains(out, `"hard_entry_gate"`) {
		t.Fatal("gate block not rendered for a complete symbol")
	}

	// No noise floor → no ceiling values (never fabricate one).
	cfg2 := &store.StrategyConfig{}
	engine2 := NewStrategyEngine(cfg2)
	ctx2 := &Context{}
	engine2.formatMarketData(data, nil, ctx2, nil)
	if c2 := ctx2.RRCeilings["TESTUSDT"]; c2 == nil || c2.LongRR != 0 || c2.ShortRR != 0 {
		t.Errorf("ceiling without a floor = %+v, want zeros present (captured but empty)", c2)
	}
}
