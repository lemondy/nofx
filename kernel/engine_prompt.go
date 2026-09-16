package kernel

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/market"
	"nofx/provider/nofxos"
	"nofx/store"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// Prompt Building - System Prompt
// ============================================================================

// BuildSystemPrompt builds System Prompt according to strategy configuration
func (e *StrategyEngine) BuildSystemPrompt(accountEquity float64, variant string) string {
	var sb strings.Builder
	riskControl := e.config.RiskControl
	promptSections := e.config.PromptSections

	// 0. Data Dictionary & Schema (ensure AI understands all fields)
	lang := e.GetLanguage()
	schemaPrompt := GetSchemaPrompt(lang)
	sb.WriteString(schemaPrompt)
	sb.WriteString("\n\n")
	sb.WriteString("---\n\n")

	// 1. Role definition (editable)
	if promptSections.RoleDefinition != "" {
		sb.WriteString(promptSections.RoleDefinition)
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("# You are a professional cryptocurrency trading AI\n\n")
		sb.WriteString("Your task is to make trading decisions based on provided market data.\n\n")
	}

	// 2. Trading mode variant
	switch strings.ToLower(strings.TrimSpace(variant)) {
	case "aggressive":
		sb.WriteString("## Mode: Aggressive\n- Prioritize capturing trend breakouts, can build positions in batches when confidence ≥ 70\n- Allow higher positions, but must strictly set stop-loss and explain risk-reward ratio\n\n")
	case "conservative":
		sb.WriteString("## Mode: Conservative\n- Only open positions when multiple signals resonate\n- Prioritize cash preservation, must pause for multiple periods after consecutive losses\n\n")
	case "scalping":
		sb.WriteString("## Mode: Scalping\n- Focus on short-term momentum, smaller profit targets but require quick action\n- If price doesn't move as expected within two bars, immediately reduce position or stop-loss\n\n")
	}

	// 3. Hard constraints (risk control)
	btcEthPosValueRatio := riskControl.BTCETHMaxPositionValueRatio
	if btcEthPosValueRatio <= 0 {
		btcEthPosValueRatio = 5.0
	}
	altcoinPosValueRatio := riskControl.AltcoinMaxPositionValueRatio
	if altcoinPosValueRatio <= 0 {
		altcoinPosValueRatio = 1.0
	}

	sb.WriteString("# Hard Constraints (Risk Control)\n\n")
	sb.WriteString("## CODE ENFORCED (Backend validation, cannot be bypassed):\n")
	sb.WriteString(fmt.Sprintf("- Max Positions: %d coins simultaneously\n", riskControl.MaxPositions))
	sb.WriteString(fmt.Sprintf("- Position Value Limit (Altcoins): max %.0f USDT (= equity %.0f × %.1fx)\n",
		accountEquity*altcoinPosValueRatio, accountEquity, altcoinPosValueRatio))
	sb.WriteString(fmt.Sprintf("- Position Value Limit (BTC/ETH): max %.0f USDT (= equity %.0f × %.1fx)\n",
		accountEquity*btcEthPosValueRatio, accountEquity, btcEthPosValueRatio))
	sb.WriteString(fmt.Sprintf("- Max Margin Usage: ≤%.0f%%\n", riskControl.MaxMarginUsage*100))
	sb.WriteString(fmt.Sprintf("- Min Position Size: ≥%.0f USDT\n", riskControl.MinPositionSize))
	// Margin-budget reality check (audit 09-13): the value-ratio limits and
	// the margin budget bind at different points — state the binding one so
	// the numbers can't imply more concurrent full-size positions than the
	// budget allows.
	budget := riskControl.MaxMarginUsage
	if budget <= 0 {
		budget = 0.9
	}
	btcEthLev := float64(riskControl.BTCETHMaxLeverage)
	altLev := float64(riskControl.AltcoinMaxLeverage)
	if btcEthLev <= 0 {
		btcEthLev = 20
	}
	if altLev <= 0 {
		altLev = 20
	}
	worstMargin := accountEquity * altcoinPosValueRatio / altLev
	if m := accountEquity * btcEthPosValueRatio / btcEthLev; m > worstMargin {
		worstMargin = m
	}
	effPositions := 0
	if worstMargin > 0 {
		effPositions = int(budget * accountEquity / worstMargin)
	}
	sb.WriteString(fmt.Sprintf("- Margin-budget reality: one max-size position needs ~%.0f USDT margin; the ≤%.0f%% budget (~%.0f USDT) holds about %d full-size position(s) — plan concurrent opens by the budget, it binds before Max Positions does\n\n",
		worstMargin, budget*100, budget*accountEquity, effPositions))

	sb.WriteString("## AI GUIDED (Recommended, you should follow):\n")
	sb.WriteString(fmt.Sprintf("- Trading Leverage: Altcoins max %dx | BTC/ETH max %dx\n",
		riskControl.AltcoinMaxLeverage, riskControl.BTCETHMaxLeverage))
	sb.WriteString(fmt.Sprintf("- Risk-Reward Ratio: ≥1:%.1f (take_profit / stop_loss)\n", riskControl.MinRiskRewardRatio))
	sb.WriteString(fmt.Sprintf("- Min Confidence: ≥%d to open position\n\n", riskControl.MinConfidence))

	// Position sizing guidance — ONE formula, matching the 程序强制缩仓 rule in
	// the params block (risk budget ÷ stop distance, then clamped). The old
	// confidence-tier percentages of the Position Value Limit contradicted it
	// (review 2026-09-07: two "mandatory" sizing methods → model picks either).
	riskPctDefault := riskControl.RiskPerTradePct
	if riskPctDefault <= 0 {
		riskPctDefault = 1.5
	}
	sb.WriteString("## Position Sizing (single formula, CODE ENFORCED)\n")
	sb.WriteString(fmt.Sprintf("`position_size_usd` = notional value = equity × %.1f%% (risk budget) ÷ stop_distance%%, then clamped:\n", riskPctDefault))
	sb.WriteString(fmt.Sprintf("- Lower bound: Min Position Size (%.0f USDT) — below it, skip the setup\n", riskControl.MinPositionSize))
	sb.WriteString("- Upper bound: the Position Value Limit above\n")
	// Example derives from the SAME live equity the Hard Constraints block
	// prints — hardcoded example numbers drifted from reality (audit 09-13 #1).
	sb.WriteString(fmt.Sprintf("- Example: equity %.0f, stop distance 6.48%% → %.0f × 1.5 ÷ 6.48 ≈ %.1f USDT notional (risk at stop = %.0f × 1.5%% ≈ %.2f USDT)\n",
		accountEquity, accountEquity, accountEquity*1.5/6.48, accountEquity, accountEquity*0.015))
	sb.WriteString("- Wider stop → smaller position. `confidence` decides WHETHER to open, never a multiplier on position value — do NOT size from Position Value Limit percentages\n")
	sb.WriteString("- **DO NOT** just use available_balance as position_size_usd\n\n")

	// 4. Trading frequency (editable)
	if promptSections.TradingFrequency != "" {
		sb.WriteString(promptSections.TradingFrequency)
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("# Trading Frequency\n\n")
		sb.WriteString("- No frequency cap: trade as often as independent, evidence-backed setups appear. Every decision is judged on its own quality (trend alignment, RR, confirmation) — never on how many trades already happened this hour or this cycle.\n")
		sb.WriteString("- Multiple symbols per cycle are independent decisions — judge each on its own evidence.\n\n")
	}

	// 5. Entry standards (editable)
	if promptSections.EntryStandards != "" {
		sb.WriteString(promptSections.EntryStandards)
		sb.WriteString("\n\nYou have the following indicator data:\n")
		e.writeAvailableIndicators(&sb)
		sb.WriteString(fmt.Sprintf("\n**Confidence ≥ %d** required to open positions.\n\n", riskControl.MinConfidence))
	} else {
		sb.WriteString("# 🎯 Entry Standards (Strict)\n\n")
		sb.WriteString("Only open positions when multiple signals resonate. You have:\n")
		e.writeAvailableIndicators(&sb)
		sb.WriteString(fmt.Sprintf("\nFeel free to use any effective analysis method, but **confidence ≥ %d** required to open positions; avoid low-quality behaviors such as single indicators, contradictory signals, sideways consolidation, reopening immediately after closing, etc.\n\n", riskControl.MinConfidence))
	}

	// 6. Decision process (editable)
	if promptSections.DecisionProcess != "" {
		sb.WriteString(promptSections.DecisionProcess)
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("# 📋 Decision Process\n\n")
		sb.WriteString("1. Check positions → Should we take profit/stop-loss\n")
		sb.WriteString("2. Scan candidate coins + multi-timeframe → Are there strong signals\n")
		sb.WriteString("3. Write chain of thought first, then output structured JSON\n\n")
	}

	// 7. Output format
	sb.WriteString("# Output Format (Strictly Follow)\n\n")
	sb.WriteString("**Must use XML tags <reasoning> and <decision> to separate chain of thought and decision JSON, avoiding parsing errors**\n\n")
	sb.WriteString("## Format Requirements\n\n")
	sb.WriteString("<reasoning>\n")
	sb.WriteString("Your chain of thought analysis...\n")
	sb.WriteString("- Briefly analyze your thinking process \n")
	sb.WriteString("</reasoning>\n\n")
	sb.WriteString("<decision>\n")
	sb.WriteString("Step 2: JSON decision array\n\n")
	sb.WriteString("```json\n[\n")
	// Use the actual configured position value ratio for BTC/ETH in the example
	examplePositionSize := accountEquity * btcEthPosValueRatio
	if riskControl.LimitEntryEnabled {
		sb.WriteString(fmt.Sprintf("  {\"symbol\": \"BTCUSDT\", \"action\": \"open_short_limit\", \"price\": 100500, \"leverage\": %d, \"position_size_usd\": %.0f, \"stop_loss\": 97000, \"take_profit\": 91000, \"confidence\": 85, \"risk_usd\": 300},\n",
			riskControl.BTCETHMaxLeverage, examplePositionSize))
	} else {
		sb.WriteString(fmt.Sprintf("  {\"symbol\": \"BTCUSDT\", \"action\": \"open_short\", \"leverage\": %d, \"position_size_usd\": %.0f, \"stop_loss\": 97000, \"take_profit\": 91000, \"confidence\": 85, \"risk_usd\": 300},\n",
			riskControl.BTCETHMaxLeverage, examplePositionSize))
	}
	sb.WriteString("  {\"symbol\": \"ETHUSDT\", \"action\": \"close_long\"}\n")
	sb.WriteString("]\n```\n")
	sb.WriteString("</decision>\n\n")
	sb.WriteString("## Field Description\n\n")
	actions := "open_long | open_short | close_long | close_short | adjust_stop_loss | partial_close_long | partial_close_short | hold | wait"
	if riskControl.LimitEntryEnabled {
		actions += " | open_long_limit | open_short_limit"
	}
	sb.WriteString("- `action`: " + actions + "\n")
	sb.WriteString(fmt.Sprintf("- `confidence`: 0-100 (opening recommended ≥ %d)\n", riskControl.MinConfidence))
	sb.WriteString("- Required when opening: leverage, position_size_usd, stop_loss, take_profit, confidence, risk_usd; 限价开仓另需 `price`\n")
	sb.WriteString("- **IMPORTANT**: All numeric values must be calculated numbers, NOT formulas/expressions (e.g., use `27.76` not `3000 * 0.01`)\n")
	if riskControl.LimitEntryEnabled {
		maxCycles := riskControl.LimitEntryMaxCycles
		if maxCycles <= 0 {
			maxCycles = 3
		}
		anchorCfg := AnchorOffsetFromRiskControl(&riskControl).normalized()
		sb.WriteString(fmt.Sprintf("- **开仓默认用限价单,不要用市价**:做多输出 `open_long_limit`、做空输出 `open_short_limit`,`price` 字段直接复制该币快照里预计算好的 `limit_buy_price`/`limit_sell_price`,禁止自己另算或改动这个值。锚点偏移随该币波动率缩放(偏移 = %.2f×ATR(1h),夹在 %.2f%%~%.2f%%;实际值见各币 JSON 的 `limit_entry_offset_pct`):低波动币锚点更紧,高波动币呼吸空间更宽。限价单让你在回踩/反弹到更优价位时才成交,避免追高滑点和假突破;系统挂 GTC 限价单,%d 个周期未成交会自动撤销并在下轮重评,你不需要重复挂单;成交后系统自动按你给的 stop_loss/take_profit 挂保护单(以成交价=触发价精确锚定)。SL/TP/杠杆/仓位等其余字段与市价开仓完全一致。\n", anchorCfg.ATRMult, anchorCfg.MinPct, anchorCfg.MaxPct, maxCycles))
		sb.WriteString("- **锚点被抑制 = 禁止开仓(硬规则,优先级高于 entry_rule_triggered)**: 若某币快照里 `limit_buy_price` 或 `limit_sell_price` 为 0(被程序抑制,warnings 会有 \"anchor ... suppressed\" 说明),该币该方向本周期不可开仓——即便 entry_rule_triggered=true、directional_score 很高也一样:action 只能输出 wait(无持仓)或 hold(有持仓),decision_stage 相应降为 WATCH/READY,no_trade_reason 写明\"挂单锚点被抑制\"。严禁把 0 当作 price 输出,也严禁自己另算价格顶替——系统会直接把此类决策降级为 wait 并记录\n")
		sb.WriteString("- `open_long` / `open_short`(市价,三类例外,其余情况必须用限价): **例外一(突破追入)**: 当且仅当以下条件**全部**满足——① `breakout.status`=\"confirmed\"(仅此值;approach/broken_unconfirmed/retest_hold 均不算)② `breakout.volume_confirmation`=true ③ `breakout.oi_confirmation`=true ④ `directional_score`≥80 ⑤ `signal_conflict.directional_conflict`=false ⑥ 该币未处于连亏禁开仓期。**例外二(布林上轨骑行,只做多)**: 程序预计算标志 `bb_ride.ride`=true(短期量能暴增 + 15m 连续 ≥3 根阳线收盘且高点贴上轨,见快照 `bb_ride.windows`/`volume_surge`/`upper_band`)且你的 confidence≥80。**例外三(布林下轨骑行,只做空)**: 程序预计算标志 `short_ride.ride`=true(量能暴增 + 15m 连续 ≥3 根阴线收盘且低点贴下轨,见快照 `short_ride.windows`/`volume_surge`/`lower_band`)且你的 confidence≥80——急跌行情里做空限价锚点常因贴近 swing-low 被程序抑制(hard_entry_gate.short.failed 仅含 LIMIT_ANCHOR_SUPPRESSED 即此情形),此时不得干等反弹,可按本例外 `open_short` 市价追入。**任一例外成立才允许 `open_long`/`open_short` 市价追入(例外二仅多头、例外三仅空头)**,reasoning 逐条列出成立条件;例外不满足仍必须用限价单等回踩,市价承担滑点与假突破成本。突破触发单(stop-entry)系统当前不支持,不要输出其他动作类型。\n")
	}
	sb.WriteString("- **wait 必须区分三类,禁止混淆**: ① NO TRADE(无方向优势): directional_score 弱/多空证据均衡,wait_bias 留空;② WAIT_LONG / WAIT_SHORT(方向明确但无合规入场点): 方向证据成立(directional_score 同号、结构共振),但锚点被抑制/时机闸门未过/当前价位 RR 不足/破位未确认等,wait_bias 填 \"long\"/\"short\";③ 方向被禁: 连亏熔断或方向硬门拦截,wait_bias 填被禁方向。**no_trade_reason 只描述拦路的客观条件,禁止输出反向方向判断**——\"不看多\"/\"bearish\" 在方向证据为多时是错误表述。例(方向多但限价锚点被抑制): wait_bias=\"long\",理由只写\"挂单锚点被抑制\"——这是 WAIT_LONG,绝不是\"不看多\"\n")
	sb.WriteString("- `no_trade_reason`: hold/wait 决策必填数组(2-4 条,中文短语,每条≤25字),只写客观拦截条件——如 挂单锚点被抑制/微趋势range/RR不足/贴近阻力/拥挤度过高/连亏熔断/数据不足。这是无交易统计的数据源,缺失会被视为分析不完整。**长度纪律**: 输出预算有限,推理段不要逐币罗列完整理由数组再在 JSON 里重复一遍——推理只写关键判断(每币一行以内),完整理由只在 JSON 的 no_trade_reason 里出现一次。输出被截断的响应会作废,宁可少写推理也不要丢掉结尾的 JSON\n")
	sb.WriteString("- `decision_stage`: open_*/close_*/partial_close_*/hold 决策必填,枚举: NO_SETUP / WATCH / READY / TRIGGERED / IN_POSITION / EXIT。映射: open_*/open_*_limit→TRIGGERED; close_*/partial_close_*→EXIT; adjust_stop_loss→IN_POSITION; 有持仓的 hold→IN_POSITION; 无持仓的 hold→NO_SETUP。**wait 决策不要输出 decision_stage**——程序从 wait_bias + blocking_factors 自动推导(NO_SETUP/WATCH/READY),省下的输出预算留给推理\n")
	sb.WriteString("- **持仓管理动作(浮盈/结构变化时用,优先于全平)**:\n")
	sb.WriteString("  - `adjust_stop_loss`(移动止损): 输出新的 `stop_loss` 价,程序把该持仓的交易所止损单移过去。**只允许收紧**:做多要求新 SL 高于当前 SL 且低于现价,做空镜像——放宽或穿越现价的移动会被程序拒绝。用途: 结构变化后按新结构位保护利润(如 15m 结构抬高时把 SL 收到结构位上方)、浮盈未到程序 1R 锁盈线但想先保本\n")
	sb.WriteString("  - `partial_close_long` / `partial_close_short`(部分平仓): 输出 `close_fraction`(0<frac≤0.5)平掉对应比例,用于按结构位分批止盈/减仓;每仓位累计部分平仓 ≤75%(程序强制),全平用 close_*;受最短持仓/提前平仓门约束(同 close)\n")
	sb.WriteString("  - 程序自动机制仍在: 1R 减仓 50%+保本、1.5R 跟踪止损、25% 全平、回撤保护——这些动作是**补充**,不是替代\n")
	sb.WriteString("- `wait_bias`: wait 决策的方向语义,枚举 \"long\"/\"short\"/留空——见上三类分类;它承载方向判断,directional_score 是它的证据,no_trade_reason 不承载方向判断\n")
	sb.WriteString("- **`wait_state` + `next_trigger`(wait 决策必填,交易状态机)**: wait_state 逐字枚举 BLOCKED|WATCH_LONG|WATCH_SHORT|READY_LONG|READY_SHORT,以该币 `hard_entry_gate` 程序判定为准: 两个方向都 allowed=false 且无可主张的市价例外→BLOCKED;方向成立但有入场拦路(锚点/闸门/RR 等,failed 非空)→WATCH_*;全部硬门通过只差价格触发事件→READY_*。`next_trigger` 对 WATCH_*/READY_* 必填: 一句话写\"触发事件 + RECHECK_ALL_HARD_GATES\"——触发事件只是**重评条件**,事件发生后一切硬门(止损结构/RR≥min/时点/锚点呼吸/min_size/数据质量)必须重新全过,它绝不是开仓许可;禁止只写\"等15m转down\"这类单事件表述(转down≠可开仓)。**时间语义纪律**: 决策在下一周期快照自动重评,禁止输出任何以天/周为尺度的搁置结论(\"下周重评\"\"本周不再关注\"等均为错误措辞)\n")
	sb.WriteString("- **`management_quality` + `management_flags`(IN_POSITION hold 必填,数据集字段)**: management_quality 是你对\"继续持有\"这个判断的诚实自评 0-100(90+: 趋势完好+结构无损+浮盈保护已到位;70-89: 持有理由成立但需盯一个风险;50-69: 边缘,理由在弱化;<50: 该考虑离场——此时应输出 close/partial 而不是低分 hold)。management_flags 固定枚举(逐字): BREAKEVEN_WARRANTED|PARTIAL_WARRANTED|TRAIL_SUFFICIENT|TREND_INTACT|STRUCTURE_WEAKENING|CHOP_RISK|VOL_SPIKE|EVENT_RISK。**若你认为该保本/该部分止盈,正确动作是输出 adjust_stop_loss / partial_close_*,而不是 hold+flag**——hold+flag 的语义是\"我判断了,但程序阶梯/时机还没到,暂不动作\"\n")
	sb.WriteString("- **`entry_quality` + `blocking_factors`(open_* 与 wait 决策必填,数据集字段)**: entry_quality 是你对自己偏好方向入场质量的诚实自评 0-100——90+: 多周期共振+RR≥3+确认齐全;80-89: 强设置(RR≥2+至少两项确认);70-79: 方向对但缺一项关键条件;60-69: 有雏形缺多项;<60: 仅有雏形。blocking_factors 只能用固定枚举(逐字): RR_LOW|ANCHOR_SUPPRESSED|TIMING_GATE|BREAKOUT_UNCONFIRMED|RANGE_NO_DIRECTION|CONFLICT_UNRESOLVED|CROWDING_HIGH|LOSS_STREAK_BAN|VOL_EXTREME|DATA_INSUFFICIENT|MIN_SIZE|STRUCTURE_CONFLICT|WAIT_PULLBACK。其中 `LOSS_STREAK_BAN` **只允许用于快照 JSON 里有 `loss_streak` 字段的币**——那是程序按成交记录算出的连亏禁开期;快照没有该字段 = 程序判定未熔断,给这样的币标 LOSS_STREAK_BAN 属于标签造假(09-15 审计:模型曾给刚连胜两笔的币标此标签 17 次)。两者必须自洽(所有阻塞标签解除时 entry_quality 应≥80)。这是质量→胜率回测数据集的原始数据——评分诚实度决定这套数据有没有价值,不许为凑高分虚报\n")
	sb.WriteString("- **STRICT JSON**: Output raw JSON only — no placeholders (`?`, `？`, `N/A`, `—`) or trailing commas for unknown values. If a value is unknown, use `0` or omit the field entirely\n")
	sb.WriteString("- **`0` 的语义例外(价格字段)**: 对 `price` / `stop_loss` / `take_profit`,以及输入里的 `limit_buy_price` / `limit_sell_price`,`0` 严格等于\"不可交易/被抑制\",绝不是占位符——这些字段绝不能输出 0,也不确定时省略字段并把原因写进 no_trade_reason\n\n")

	// 8. Custom Prompt
	if e.config.CustomPrompt != "" {
		sb.WriteString("# 📌 Personalized Trading Strategy\n\n")
		sb.WriteString(e.config.CustomPrompt)
		sb.WriteString("\n\n")
		sb.WriteString("Note: The above personalized strategy is a supplement to the basic rules and cannot violate the basic risk control principles.\n")
	}

	return sb.String()
}

func (e *StrategyEngine) writeAvailableIndicators(sb *strings.Builder) {
	indicators := e.config.Indicators
	kline := indicators.Klines

	sb.WriteString(fmt.Sprintf("- %s price series", kline.PrimaryTimeframe))
	if kline.EnableMultiTimeframe {
		sb.WriteString(fmt.Sprintf(" + %s K-line series\n", kline.LongerTimeframe))
	} else {
		sb.WriteString("\n")
	}

	if indicators.EnableEMA {
		sb.WriteString("- EMA indicators")
		if len(indicators.EMAPeriods) > 0 {
			sb.WriteString(fmt.Sprintf(" (periods: %v)", indicators.EMAPeriods))
		}
		sb.WriteString("\n")
	}

	if indicators.EnableMACD {
		sb.WriteString("- MACD indicators\n")
	}

	if indicators.EnableRSI {
		sb.WriteString("- RSI indicators")
		if len(indicators.RSIPeriods) > 0 {
			sb.WriteString(fmt.Sprintf(" (periods: %v)", indicators.RSIPeriods))
		}
		sb.WriteString("\n")
	}

	if indicators.EnableATR {
		sb.WriteString("- ATR indicators")
		if len(indicators.ATRPeriods) > 0 {
			sb.WriteString(fmt.Sprintf(" (periods: %v)", indicators.ATRPeriods))
		}
		sb.WriteString("\n")
	}

	if indicators.EnableBOLL {
		sb.WriteString("- Bollinger Bands (BOLL) - Upper/Middle/Lower bands")
		if len(indicators.BOLLPeriods) > 0 {
			sb.WriteString(fmt.Sprintf(" (periods: %v)", indicators.BOLLPeriods))
		}
		sb.WriteString("\n")
	}

	if indicators.EnableVolume {
		sb.WriteString("- Volume data\n")
	}

	if indicators.EnableOI {
		sb.WriteString("- Open Interest (OI) data\n")
	}

	if indicators.EnableFundingRate {
		sb.WriteString("- Funding rate\n")
	}

	if len(e.config.CoinSource.StaticCoins) > 0 || e.config.CoinSource.UseAI500 || e.config.CoinSource.UseOITop {
		sb.WriteString("- AI500 / OI_Top filter tags (if available)\n")
	}

	if indicators.EnableQuantData {
		sb.WriteString("- Quantitative data (institutional/retail fund flow, position changes, multi-period price changes)\n")
	}
}

// ============================================================================
// Prompt Building - User Prompt
// ============================================================================

// BuildUserPrompt builds User Prompt based on strategy configuration
func (e *StrategyEngine) BuildUserPrompt(ctx *Context) string {
	var sb strings.Builder

	// System status
	sb.WriteString(fmt.Sprintf("Time: %s | Period: #%d | Runtime: %d minutes\n\n",
		ctx.CurrentTime, ctx.CallCount, ctx.RuntimeMinutes))

	// BTC market
	if btcData, hasBTC := ctx.MarketDataMap["BTCUSDT"]; hasBTC {
		sb.WriteString(fmt.Sprintf("BTC: %.2f (1h: %+.2f%%, 4h: %+.2f%%) | MACD: %.4f | RSI: %.2f\n\n",
			btcData.CurrentPrice, btcData.PriceChange1h, btcData.PriceChange4h,
			btcData.CurrentMACD, btcData.CurrentRSI7))
	}

	// Account information
	sb.WriteString(fmt.Sprintf("Account: Equity %.2f | Balance %.2f (%.1f%%) | PnL %+.2f%% | Margin %.1f%% | Positions %d\n\n",
		ctx.Account.TotalEquity,
		ctx.Account.AvailableBalance,
		(ctx.Account.AvailableBalance/ctx.Account.TotalEquity)*100,
		ctx.Account.TotalPnLPct,
		ctx.Account.MarginUsedPct,
		ctx.Account.PositionCount))

	// Recently completed orders (placed before positions to ensure visibility)
	if len(ctx.RecentOrders) > 0 {
		sb.WriteString("## Recent Completed Trades\n")
		for i, order := range ctx.RecentOrders {
			resultStr := "Profit"
			if order.RealizedPnL < 0 {
				resultStr = "Loss"
			}
			sb.WriteString(fmt.Sprintf("%d. %s %s | Entry %.4f Exit %.4f | %s: %+.2f USDT (%+.2f%%) | %s→%s (%s)\n",
				i+1, order.Symbol, order.Side,
				order.EntryPrice, order.ExitPrice,
				resultStr, order.RealizedPnL, order.PnLPct,
				order.EntryTime, order.ExitTime, order.HoldDuration))
		}
		sb.WriteString("\n")
	}

	// Historical trading statistics (helps AI understand past performance)
	if ctx.TradingStats != nil && ctx.TradingStats.TotalTrades > 0 {
		// Get language from strategy config
		lang := e.GetLanguage()

	// Win/Loss ratio
	var winLossRatio float64
	if ctx.TradingStats.AvgLoss > 0 {
		winLossRatio = ctx.TradingStats.AvgWin / ctx.TradingStats.AvgLoss
	}

	// Stats window label: the numbers below only cover trades closed within
	// the window (config stats_window_days); 0 = full history.
	windowLabel := "全部历史"
	if ctx.TradingStats.WindowDays > 0 {
		windowLabel = fmt.Sprintf("近%d天", ctx.TradingStats.WindowDays)
	}

	if lang == LangChinese {
		sb.WriteString("## 历史交易统计\n")
		sb.WriteString(fmt.Sprintf("统计窗口: %s | 总交易: %d 笔 | 盈利因子: %.2f | 夏普比率: %.2f | 盈亏比: %.2f\n",
			windowLabel,
			ctx.TradingStats.TotalTrades,
			ctx.TradingStats.ProfitFactor,
			ctx.TradingStats.SharpeRatio,
			winLossRatio))
		sb.WriteString(fmt.Sprintf("总盈亏: %+.2f USDT | 平均盈利: +%.2f | 平均亏损: -%.2f | 最大回撤: %.1f%%\n",
			ctx.TradingStats.TotalPnL,
			ctx.TradingStats.AvgWin,
			ctx.TradingStats.AvgLoss,
			ctx.TradingStats.MaxDrawdownPct))

		// Performance hints based on profit factor, sharpe, and drawdown
		// ⑲ machine-readable edge status: NEGATIVE_EDGE tightens the
		// trade-selection bar instead of relying on prose encouragement.
		edge := "POSITIVE_EDGE"
		if ctx.TradingStats.ProfitFactor < 0.9 {
			edge = "NEGATIVE_EDGE"
		} else if ctx.TradingStats.ProfitFactor < 1.1 {
			edge = "NO_EDGE"
		}
		sb.WriteString(fmt.Sprintf("strategy_health: %s (PF %.2f, expectancy_r %+.2f, 窗口 %s)\n", edge, ctx.TradingStats.ProfitFactor, expectancyR(ctx.TradingStats.WinRate, ctx.TradingStats.AvgWin, ctx.TradingStats.AvgLoss), windowLabel))
		if edge == "NEGATIVE_EDGE" {
			sb.WriteString(fmt.Sprintf("⚠️ 当前策略整体无正期望(窗口 %s 内 PF<0.9):只做证据极强、多周期共振且 RR 明显占优的设置,其余一律 hold 并在 no_trade_reason 写明\n", windowLabel))
		}
			if ctx.TradingStats.ProfitFactor >= 1.5 && ctx.TradingStats.SharpeRatio >= 1 {
				sb.WriteString("表现: 良好 - 保持当前策略\n")
			} else if ctx.TradingStats.ProfitFactor < 1 {
				sb.WriteString("表现: 需改进 - 提高盈亏比，优化止盈止损\n")
			} else if ctx.TradingStats.MaxDrawdownPct > 30 {
				sb.WriteString("表现: 风险偏高 - 减少仓位，控制回撤\n")
			} else {
				sb.WriteString("表现: 正常 - 有优化空间\n")
			}
		} else {
			enWindow := "all history"
			if ctx.TradingStats.WindowDays > 0 {
				enWindow = fmt.Sprintf("last %d days", ctx.TradingStats.WindowDays)
			}
			sb.WriteString("## Historical Trading Statistics\n")
			sb.WriteString(fmt.Sprintf("Window: %s | Total Trades: %d | Profit Factor: %.2f | Sharpe: %.2f | Win/Loss Ratio: %.2f\n",
				enWindow,
				ctx.TradingStats.TotalTrades,
				ctx.TradingStats.ProfitFactor,
				ctx.TradingStats.SharpeRatio,
				winLossRatio))
			sb.WriteString(fmt.Sprintf("Total PnL: %+.2f USDT | Avg Win: +%.2f | Avg Loss: -%.2f | Max Drawdown: %.1f%%\n",
				ctx.TradingStats.TotalPnL,
				ctx.TradingStats.AvgWin,
				ctx.TradingStats.AvgLoss,
				ctx.TradingStats.MaxDrawdownPct))

			// Performance hints based on profit factor, sharpe, and drawdown
			if ctx.TradingStats.ProfitFactor >= 1.5 && ctx.TradingStats.SharpeRatio >= 1 {
				sb.WriteString("Performance: GOOD - maintain current strategy\n")
			} else if ctx.TradingStats.ProfitFactor < 1 {
				sb.WriteString("Performance: NEEDS IMPROVEMENT - improve win/loss ratio, optimize TP/SL\n")
			} else if ctx.TradingStats.MaxDrawdownPct > 30 {
				sb.WriteString("Performance: HIGH RISK - reduce position size, control drawdown\n")
			} else {
				sb.WriteString("Performance: NORMAL - room for optimization\n")
			}
		}
		sb.WriteString("\n")
	}

	// Review-derived rules (hard rules + soft lessons from past trade reviews)
	if ctx.RulesText != "" {
		sb.WriteString(ctx.RulesText)
	}

	// Structured Signal field dictionary — coin-independent, so it is rendered
	// exactly once here (before the first signal block) rather than once per
	// coin. Both the positions and candidate sections below embed signal JSON.
	sb.WriteString("## Structured Signal 字段说明（适用于下方每一个 Structured Signal 块）\n")
	sb.WriteString(signalBlockLegend)
	sb.WriteString("\n")

	// Position information
	if len(ctx.Positions) > 0 {
		sb.WriteString("## Current Positions\n")
		for i, pos := range ctx.Positions {
			sb.WriteString(e.formatPositionInfo(i+1, pos, ctx))
		}
	} else {
		sb.WriteString("Current Positions: None\n\n")
	}

	// Candidate coins (exclude coins already in positions to avoid duplicate data)
	positionSymbols := make(map[string]bool)
	for _, pos := range ctx.Positions {
		// Normalize symbol to handle both "ETH" and "ETHUSDT" formats
		normalizedSymbol := market.Normalize(pos.Symbol)
		positionSymbols[normalizedSymbol] = true
	}

	// Strategy parameters for short-scan strategies: surface the configured
	// funding-rate crowding threshold so the AI's entry rules follow the UI
	// config instead of numbers hard-coded in prompt text.
	cs := e.config.CoinSource
	{
		var params strings.Builder
		if cs.SourceType == "short_scan" || (cs.SourceType == "mixed" && cs.UseShortScan) {
			frPct := cs.ShortScanFundingRatePct
			if frPct <= 0 {
				frPct = 0.03
			}
			// Annualize with the 8h×3×365 convention; non-8h-settlement coins
			// convert with their real interval (scanner_hint.funding_annualized_pct
			// already carries the properly annualized figure).
			annPct := frPct * 1095
			params.WriteString(fmt.Sprintf("- 做空资金费率拥挤(二选一,均为支撑证据而非必要条件): ① 费率年化 > %.1f%%(8h 结算口径即每期费率 > %.4f%%;非 8h 结算的币按真实结算间隔换算)——多头拥挤进行时,以各币 derivatives.funding_annualized_pct 实时值判断; 或 ② funding_rollover 费率刚从高位回落,以各币 derivatives.funding_rollover.detected=true 为唯一依据(程序按结算历史计算: 前 3 个结算期费率高于该阈值、当前前瞻费率已回落;from_annualized_pct 为回落前高点年化)——**禁止从 scanner patterns 里的\"资金费率回落\"字样认定条件②**,hint 是扫描时刻快照,且策略规定 hint 仅作辅助证据;快照没有 funding_rollover 字段 = 历史拉取失败 = 条件② UNKNOWN,按不满足处理(两者都不满足时不要仅因费率理由做空)。磨顶宇宙(universe=\"near_high\")的候选豁免此条件——缓慢磨顶的币费率通常已正常化,其确认信号是 4h 顶背离\n", annPct, frPct))
		}
		if e.config.RiskControl.EntryTimingGate {
			params.WriteString("- 入场时点(程序强制): 最细子小时周期(15m/30m)趋势必须与方向一致——做多需 up/pullback,做空需 down/rally(下跌趋势中的反弹=空头入场窗);range 无动能,顺势入场同样会被拦截\n")
		}
		params.WriteString("- 开仓硬门(程序判定,禁止自行重算): 各币快照 `hard_entry_gate.long/short` 已把该方向所有程序可判的拦路条件评完(微趋势时点/限价锚点/结构RR上限/数据充分性/最小仓位死区/连亏熔断/股票周末),`failed` 即阻断码完整列表,allowed=true 表示全部通过。open_* 只允许出现在 allowed=true 的方向;allowed=false 时输出 wait/hold,no_trade_reason 逐项对应 failed 写客观事实,不要凭感觉增减拦截理由。例外路径只有一条: failed 仅含 LIMIT_ANCHOR_SUPPRESSED(限价路径被禁)时,策略规定的市价单例外(突破追入/布林上轨骑行/布林下轨骑行做空)条件成立仍可主张;failed 含 RR_MAX/MICRO_TREND/LOSS_STREAK_BANNED/MIN_SIZE_DEAD_ZONE/DATA_INSUFFICIENT/STOCK_WEEKEND 任一项时不存在任何例外\n")
		// 下单公式(框架五): 结构止损 + 风险反推仓位 + 结构位止盈
		rc := e.config.RiskControl
		riskPct := rc.RiskPerTradePct
		if riskPct <= 0 {
			riskPct = 1.5
		}
		// 止损区间措辞必须与 validateOpenRisk 逐字对齐:上限 = max(2×ATR(4h), 8%)
		// 取宽不取窄;噪声下限仅在 SLMinATRMult>0 时存在(执行端 mult<=0 不查
		// 下限, prompt 不得虚构 1.5 默认值——曾在 VTHOUSDT 上把可执行的
		// [-, 14.29%] 区间误报成 [12%, 14.29%] 而否掉整笔交易)。
		stopBand := "上限 = max(2×ATR(4h), 8%)——缓冲与下限看 1h 节奏,宽止损上限看 4h 波动,二者取宽不取窄;ATR 用各币 JSON 里对应时间块的 atr_pct;结构位落在区间外时放弃该设置,不要硬凑。重要: 快照 rr_scan 里的 stop_price/stop_distance_pct 只是\"最紧允许止损\"口径的 RR 上限校验参考,严禁照抄为 stop_loss——实际止损必须按本方法论(结构位+缓冲)重新定位,通常比噪声下限更宽,实际 RR 也因此低于 best_rr;1h atr_percentile>80 时更不许贴下限的紧止损"
		if floorMult := rc.SLMinATRMult; floorMult > 0 {
			params.WriteString(fmt.Sprintf("- 止损(程序校验): 结构位(最近 support/resistance)外加 0.3-0.5×ATR(1h) 缓冲(方向性微调: 空单——尤其反弹追空/急跌追空——取上半段 0.4-0.5,挤压行情的上影线更长;多单回调入场取下半段 0.3-0.4),止损距离 d%% 必须同时满足: d ≥ %.1f×ATR(1h)(噪声下限)且 d ≤ 上限,其中 %s\n", floorMult, stopBand))
		} else {
			params.WriteString(fmt.Sprintf("- 止损(程序校验): 结构位(最近 support/resistance)外加 0.3-0.5×ATR(1h) 缓冲(方向性微调: 空单——尤其反弹追空/急跌追空——取上半段 0.4-0.5,挤压行情的上影线更长;多单回调入场取下半段 0.3-0.4),止损距离 d%% 只需满足: d ≤ 上限,其中 %s(本策略未启用噪声下限)\n", stopBand))
		}
		params.WriteString(fmt.Sprintf("- 仓位(程序强制缩仓): 风险金额 = 权益 × %.1f%%;仓位名义价值 = 风险金额 ÷ 止损距离%%;保证金 = 名义价值 ÷ 杠杆。例: 权益100U、止损距离3%% → 风险金额1.5U → 名义价值50U → 3x杠杆保证金≈16.7U。position_size_usd 填名义价值,不是风险金额。注意: 实盘下单量按交易所步长取整,小账户+宽止损时实际风险可能偏离理论值——名义价值低于最小下单量时放弃该设置。各币快照已按当前权益与策略配置 min_position_size(策略页可改)预计算 `min_size` 块: `max_stop_pct_for_min_size` 是能凑够最小仓位的最大止损距离(d%% 超过它名义价值必然不足),`feasible=false` 表示连噪声下限都超出该上限——该币结构性无法开仓;两种情况都直接 wait+MIN_SIZE,不要再花预算算仓位\n", riskPct))
		params.WriteString(fmt.Sprintf("- 止盈(结构选位已由程序完成): 各币快照 `hard_entry_gate[direction].rr_scan` 就是\"从近到远遍历全部时间块(含 execution_tf/15m)全部 resistance/support 元素\"的程序结果——开仓时 `take_profit` 直接采用 `rr_scan.first_rr_ge_target`(该方向从近到远第一个 RR≥%.1f 的结构位,按 rr_scan.entry_price 口径计算);`rr_scan.usable=false` 表示连最窄允许止损下 RR 上限 best_rr 都 <%.1f,该方向 RR 门结构性失败,输出 wait 并在 blocking_factors 标 RR_LOW、no_trade_reason 引用 `MAX_STRUCTURAL_RR=best_rr`——禁止只看最近一个结构位就下\"无可用结构位\"结论,也不许跳到更远目标。个别币没有 rr_scan 字段(如未启用噪声下限)时退回手工规则: 逐项遍历全部数组取第一个 RR≥%.1f。该比例仍是程序硬门槛(开仓时按决策价与成交价双重校验 RR)\n", rc.MinRiskRewardRatio, rc.MinRiskRewardRatio, rc.MinRiskRewardRatio))
		var tpParts []string
		if lockR := ProfitLockRMult(&e.config.RiskControl); lockR > 0 {
			tpParts = append(tpParts, fmt.Sprintf("浮盈达 %.0fR(1×初始止损距离)程序自动市价减仓 50%%,同时止损移至开仓价保本;剩余 50%% 奔向结构位止盈", lockR))
		}
		if tpFull := TpFullProfitPct(&e.config.RiskControl); tpFull > 0 {
			tpParts = append(tpParts, fmt.Sprintf("≥%.0f%%(杠杆后)程序自动全部平仓", tpFull))
		}
		if len(tpParts) > 0 {
			params.WriteString("- 程序自动止盈阶梯(强制,独立于你的 TP 规划): " + strings.Join(tpParts, ";") + "。这些由程序按周期自动执行,你无需输出 close 来实现;你的止盈规划仍按结构位给出\n")
		}
		if sp := MaxSpreadPct(&e.config.RiskControl); sp > 0 {
			params.WriteString(fmt.Sprintf("- 点差门(程序强制): 盘口买卖价差 > %.2f%%(占中间价)的币种,任何 open_*/open_*_limit 都会被程序拒单——薄盘口的点差会吃掉限价优势并抬高市价成本,这类币直接放弃\n", sp))
		}
		params.WriteString("- 注意: 1h atr_percentile>80 时禁止贴下限的紧止损;资金费率极端拥挤时禁止逆势扛单;目标位越过 structure_high/low(历史新高新低区)时注明无历史阻力参考、不确定性大\n")
		if e.config.RiskControl.VolTargetEnabled {
			params.WriteString("- 波动率调仓(程序自动): 每周期按 权益×单笔风险%÷ATR(1h)% 重算目标仓位,80/120 滞后带外才调——>120% 程序自动减仓,<80% 时你可在信号仍有效的前提下评估加仓。不要因波动率变化去动 SL/TP\n")
		}
		if e.config.RiskControl.TrailingStopEnabled {
			params.WriteString("- 移动止损/止盈延展(程序自动): 浮盈达 1.5×初始止损距离启动 2×ATR(1h) 跟踪止损(只紧不松);到达止盈距离后固定止盈单撤除、由跟踪止损接管让利润奔跑。初始 SL/TP 开仓后即固定,你不可也不需要修改它们;提前离场的唯一合法理由是结构破坏\n")
		}
		if e.config.RiskControl.CloseRejectBreakoutPct > 0 {
			params.WriteString(fmt.Sprintf("- 浮亏平仓限制(程序强制): 持仓浮亏时,若最近的反向结构位(做多看上方 resistance/structure_high、做空看下方 support)距离现价 < %.1f%% 且 15m 结构未破坏(未破支撑/未破阻力),\"被阻力拒绝/被支撑拒绝\"不构成平仓理由,该 close 会被程序拦截——给突破留空间;你的合法离场路径是 15m 结构实际破位、止损触发,或浮盈状态下的正常止盈\n", e.config.RiskControl.CloseRejectBreakoutPct))
		}
		params.WriteString("- 保护单看门狗(程序强制): 每周期核对全部持仓的止损/止盈挂单,缺失时按开仓计划价自动补挂(止盈距离走完转跟踪止损的除外)——保护单由程序保障,你只负责按结构规划输出 SL/TP 数值\n")
		if EarlyCloseHours(&e.config.RiskControl) > 0 {
			params.WriteString(fmt.Sprintf("- 提前平仓限制(程序强制): 持仓不足 %dh 时,close 需要该币 1h 出现至少 2 根逆持仓方向的已收盘 K 线(1h 节奏出现趋势转变的证据)才会放行,浮盈浮亏一视同仁;止盈/止损触发单与回撤保护平仓由程序自动执行,不受此限。持仓满 %dh 后正常平仓\n", EarlyCloseHours(&e.config.RiskControl), EarlyCloseHours(&e.config.RiskControl)))
		}
		if e.config.RiskControl.MinHoldMinutes > 0 {
			params.WriteString(fmt.Sprintf("- 最短持仓限制(程序强制): 持仓不足 %d 分钟时,close/partial_close 会被程序拦截(现价已触及记录止损的硬退出除外)——不要在时间未到且无 1h 逆势证据时输出平仓动作\n", e.config.RiskControl.MinHoldMinutes))
		}
		if e.config.RiskControl.OpenRejectSupplyPct > 0 {
			breathCfg := AnchorOffsetFromRiskControl(&e.config.RiskControl).normalized()
			params.WriteString(fmt.Sprintf("- 限价开仓锚点位置(程序强制): 做多挂单价距上方 15m/1h 阻力/structure_high 太近(买进供给区)、做空挂单价距下方 support/structure_low 太近(卖出支撑正上方)的开仓会被拒单——锚点与结构位之间必须留有呼吸空间。呼吸空间阈值随执行周期波动率缩放: %.2f×ATR(执行周期),夹在 %.2f%%~%.2f%%(固定模式恒为 %.1f%%)。注意: 各币 JSON 的 limit_entry_offset_pct 是挂单『偏移』(按 ATR(1h) 缩放),与这个呼吸阈值刻意用不同尺度的 ATR(偏移看 1h 稳定波动、呼吸看执行周期贴单价噪声),不要把 limit_entry_offset_pct 当成呼吸阈值。呼吸不满足时程序直接把该方向锚点置 0(limit_buy_price/limit_sell_price=0),你据此放弃即可,无需自己算阈值;没有干净空间的设置直接放弃,不要输出会被拒的单\n", breathCfg.ATRMult, breathCfg.MinPct, breathCfg.MaxPct, e.config.RiskControl.OpenRejectSupplyPct))
		}
		if e.config.RiskControl.AccountMaxDrawdownPct > 0 {
			params.WriteString(fmt.Sprintf("- 账户级熔断(程序强制): 账户净值自初始值回撤 ≥ %.1f%% 时进入只减仓模式,一切新开仓被程序拦截;此时优先保护本金,减少交易频率\n", e.config.RiskControl.AccountMaxDrawdownPct))
		}
		if noOpen := e.config.RiskControl.StockWeekendNoOpen; noOpen == nil || *noOpen {
			params.WriteString("- 股票类代币周末禁开新仓(程序强制): DELL/SKHY 等 bstock 标的周末(美东周六/周日)波动率与胜率都低——候选里出现股票类代币时,本周末只允许 hold/close,不输出任何 open_*\n")
		}
		if e.config.RiskControl.LossStreakBanEnabled {
			maxLosses := e.config.RiskControl.LossStreakMaxLosses
			if maxLosses <= 0 {
				maxLosses = 3
			}
			params.WriteString(fmt.Sprintf("- 连亏熔断(程序强制): 某币在 24h 内连续 %d 笔亏损平仓后,该币接下来 24h 禁止再开仓,开仓决策会被程序直接拦截。熔断判定与解除由程序按成交记录计算:当前熔断中的币会在其快照 JSON 里标注 `loss_streak` 块(含 until_utc 解除时间);**快照没有 `loss_streak` 字段的币一律视为未熔断**,不要自行推测某个币\"应该被熔断了\"。近期交易里已经连亏的币不要尝试抄底翻本,把机会让给趋势健康的标的\n", maxLosses))
		}
		if params.Len() > 0 {
			sb.WriteString("## Strategy Parameters\n")
			sb.WriteString(params.String())
			sb.WriteString("\n")
		}
	}

	sb.WriteString(fmt.Sprintf("## Candidate Coins (%d coins)\n\n", len(ctx.MarketDataMap)))
	sb.WriteString("> scanner_hint / 扫描评分 / patterns 均为程序化扫描的辅助证据,不是交易结论,且为扫描时刻的快照(见 generated_at_utc)。方向、时机、是否交易由你综合全部数据独立判断——可以采信、质疑或推翻扫描结果,但必须在推理中给出自己的依据。资金费率尤其如此:暴涨币的 funding 可能在几分钟内漂移数倍,当前状态以各币 Structured Signal 的 derivatives.funding_annualized_pct 为准(程序已按该币真实结算间隔 funding_settle_hours 年化,无需自行换算;与 hint 数字冲突时以 Structured Signal 为准)。\n")
	sb.WriteString("> **short_scan 候选的默认姿态(稳定规则,勿逐次重判)**: short_scan 按涨幅大入选,候选的 1h/4h 结构天然还是多头——scanner 说可空、结构说多头不是偶发冲突,是该引擎的常态。默认姿态: 顶部确认信号(顶背离/假突破/破 EMA20/费率回落——后者只认 derivatives.funding_rollover.detected)之外,**还必须 execution_filter.short_allowed=true(15m 微趋势已转)才允许做空**;仅凭确认信号而 15m 仍 up → 输出 wait + wait_bias=short + wait_state=WATCH_SHORT,触发事件写\"15m 微趋势转 down + RECHECK_ALL_HARD_GATES\"(转 down 只是重评条件,届时 RR/锚点/资金费率等一切硬门重新全过)。entry_timing_gate 开启时这同时是硬规则(15m 逆势 open_short 会被程序拒单)\n\n")
	// History-demotion: candidates where this trader has been repeatedly
	// losing recently render LAST — the ordering itself is soft evidence.
	ordered := make([]CandidateCoin, 0, len(ctx.CandidateCoins))
	var demoted []CandidateCoin
	for _, coin := range ctx.CandidateCoins {
		if recentHistoryNote(ctx, coin.Symbol) != "" {
			demoted = append(demoted, coin)
		} else {
			ordered = append(ordered, coin)
		}
	}
	ordered = append(ordered, demoted...)
	displayedCount := 0
	for _, coin := range ordered {
		// Skip if this coin is already a position (data already shown in positions section)
		normalizedCoinSymbol := market.Normalize(coin.Symbol)
		if positionSymbols[normalizedCoinSymbol] {
			continue
		}

		marketData, hasData := ctx.MarketDataMap[coin.Symbol]
		if !hasData {
			continue
		}
		displayedCount++

		sourceTags := e.formatCoinSourceTag(coin.Sources)
		sb.WriteString(fmt.Sprintf("### %d. %s%s\n\n", displayedCount, coin.Symbol, sourceTags))
		// Scanner output is EVIDENCE, not a conclusion: neutral structured
		// hint, no prescriptive direction instruction.
		for _, src := range coin.Sources {
			if src != "short_scan" || coin.ShortScore <= 0 {
				continue
			}
			hint := map[string]interface{}{
				"source":                 "short_scanner",
				"direction_bias":         "short",
				"score":                  coin.ShortScore,
				"confidence":             coin.ShortGrade,
				"patterns":               coin.ShortReasons,
				"funding_annualized_pct": coin.ShortFundingAnn,
			}
			if coin.ShortUniverse != "" {
				hint["universe"] = coin.ShortUniverse
			}
			if coin.ShortScanAtMs > 0 {
				hint["generated_at_utc"] = time.UnixMilli(coin.ShortScanAtMs).UTC().Format("2006-01-02T15:04:05Z")
			}
			hintJSON, _ := json.Marshal(hint)
			sb.WriteString("scanner_hint(程序化扫描,仅辅助证据,非交易结论): " + string(hintJSON) + "\n\n")
			break
		}
		// Concentration + own-history annotations — neutral evidence lines,
		// same "可推翻但需给依据" contract as scanner_hint.
		wroteNote := false
		for _, w := range concentrationWarnings(ctx, coin.Symbol) {
			sb.WriteString("⚠️ " + w + "\n")
			wroteNote = true
		}
		if note := recentHistoryNote(ctx, coin.Symbol); note != "" {
			sb.WriteString("⚠️ " + note + "\n")
			wroteNote = true
		}
		if wroteNote {
			sb.WriteString("\n")
		}
		var quantData *QuantData
		if ctx.QuantDataMap != nil {
			quantData = ctx.QuantDataMap[coin.Symbol]
		}
		sb.WriteString(e.formatMarketData(marketData, quantData, ctx, &coin))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// Get language for market data formatting
	nofxosLang := nofxos.LangEnglish
	if e.GetLanguage() == LangChinese {
		nofxosLang = nofxos.LangChinese
	}

	// OI Ranking data (market-wide open interest changes)
	if ctx.OIRankingData != nil {
		sb.WriteString(nofxos.FormatOIRankingForAI(ctx.OIRankingData, nofxosLang, interestingSymbols(ctx)))
		sb.WriteString("\n> ⚠️ 边界: 以上排行仅为市场整体情绪/资金轮动的宏观参考(聚合口径,条目未必可交易,且价格为最多5分钟前的快照)。**实际可交易标的仅限上方 Candidate Coins 里带完整结构化信号的 symbol** —— 不要对榜单中出现但不在候选池里的币做任何决策,也不要把榜单数字当作这些币的完整行情;榜单价格严禁参与 entry/SL/TP/RR 精确计算(与 Structured Signal 现价冲突时,一律以 Structured Signal 为准)。\n")
	}

	// NetFlow Ranking data (market-wide fund flow)
	if ctx.NetFlowRankingData != nil {
		sb.WriteString(nofxos.FormatNetFlowRankingForAI(ctx.NetFlowRankingData, nofxosLang, interestingSymbols(ctx)))
		sb.WriteString("\n> ⚠️ 边界: 以上资金流榜单仅为宏观情绪参考(价格为快照值)。实际可交易标的仅限 Candidate Coins 中带完整结构化信号的 symbol;榜单价格严禁参与 entry/SL/TP/RR 精确计算,冲突时以 Structured Signal 现价为准。\n")
	}

	// Price Ranking data (market-wide gainers/losers)
	if ctx.PriceRankingData != nil {
		sb.WriteString(nofxos.FormatPriceRankingForAI(ctx.PriceRankingData, nofxosLang, interestingSymbols(ctx), fresh60mForInteresting(ctx)))
		sb.WriteString("\n> ⚠️ 边界: 以上涨跌幅榜单仅为市场情绪/资金轮动的宏观参考(价格为最多5分钟前的快照)。实际可交易标的仅限 Candidate Coins 中带完整结构化信号的 symbol;榜单价格严禁参与 entry/SL/TP/RR 精确计算,冲突时以 Structured Signal 现价为准。\n")
	}

	sb.WriteString("---\n\n")
	sb.WriteString("Now please analyze and output your decision (Chain of Thought + JSON)\n")

	return sb.String()
}

func (e *StrategyEngine) formatPositionInfo(index int, pos PositionInfo, ctx *Context) string {
	var sb strings.Builder

	holdingDuration := ""
	if pos.UpdateTime > 0 {
		durationMs := time.Now().UnixMilli() - pos.UpdateTime
		durationMin := durationMs / (1000 * 60)
		if durationMin < 60 {
			holdingDuration = fmt.Sprintf(" | Holding Duration %d min", durationMin)
		} else {
			durationHour := durationMin / 60
			durationMinRemainder := durationMin % 60
			holdingDuration = fmt.Sprintf(" | Holding Duration %dh %dm", durationHour, durationMinRemainder)
		}
	}

	positionValue := pos.Quantity * pos.MarkPrice
	if positionValue < 0 {
		positionValue = -positionValue
	}

	// Display the ticker last price (same snapshot as the signal block) so
	// positions and signals never quote two different "current" prices; the
	// exchange mark price stays available as Mark.
	displayPrice := pos.MarkPrice
	priceLabel := "Mark"
	if marketData, ok := ctx.MarketDataMap[pos.Symbol]; ok && marketData.CurrentPrice > 0 {
		displayPrice = marketData.CurrentPrice
		priceLabel = "Last"
	}

	sb.WriteString(fmt.Sprintf("%d. %s %s | Entry %.4f %s %.4f | Qty %.4f | Position Value %.2f USDT | Margin ROI %+.2f%% | Price Return %+.2f%% | Unrealized PnL %+.2f USDT | Peak PnL %.2f%% (margin basis) | Leverage %dx | Margin %.0f | Liq Price %.4f%s\n\n",
		index, pos.Symbol, strings.ToUpper(pos.Side),
		pos.EntryPrice, priceLabel, displayPrice, pos.Quantity, positionValue, pos.UnrealizedPnLPct, pos.PriceReturnPct, pos.UnrealizedPnL, pos.PeakPnLPct,
		pos.Leverage, pos.MarginUsed, pos.LiquidationPrice, holdingDuration))

	if marketData, ok := ctx.MarketDataMap[pos.Symbol]; ok {
		var quantData *QuantData
		if ctx.QuantDataMap != nil {
			quantData = ctx.QuantDataMap[pos.Symbol]
		}
		sb.WriteString(e.formatMarketData(marketData, quantData, ctx, nil))
		sb.WriteString("\n")
	}

	return sb.String()
}

// fresh60mForInteresting: this cycle's rolling-60m change computed from each
// interesting symbol's OWN klines — full ranking rows for these symbols use
// the fresh, signal-aligned figure instead of the up-to-5-minutes-stale
// market-wide snapshot (violent movers used to show 4x divergent values).
func fresh60mForInteresting(ctx *Context) map[string]float64 {
	out := make(map[string]float64, len(ctx.MarketDataMap))
	for sym := range interestingSymbols(ctx) {
		if data, ok := ctx.MarketDataMap[sym]; ok {
			out[sym] = rolling1hChangePct(data, time.Now())
		}
	}
	return out
}

// interestingSymbols: candidates + open positions — the only symbols that get
// full rows in the market-wide ranking tables (the rest collapse to headlines
// to keep the context budget for actionable data).
func interestingSymbols(ctx *Context) map[string]bool {
	out := make(map[string]bool, len(ctx.CandidateCoins)+len(ctx.Positions))
	for _, c := range ctx.CandidateCoins {
		out[strings.ToUpper(market.Normalize(c.Symbol))] = true
	}
	for _, p := range ctx.Positions {
		out[strings.ToUpper(market.Normalize(p.Symbol))] = true
	}
	return out
}

func (e *StrategyEngine) formatCoinSourceTag(sources []string) string {
	if len(sources) > 1 {
		// Multiple signal source combination
		hasAI500 := false
		hasOITop := false
		hasOILow := false
		hasHyperAll := false
		hasHyperMain := false
		for _, s := range sources {
			switch s {
			case "ai500":
				hasAI500 = true
			case "oi_top":
				hasOITop = true
			case "oi_low":
				hasOILow = true
			case "hyper_all":
				hasHyperAll = true
			case "hyper_main":
				hasHyperMain = true
			}
		}
		if hasAI500 && hasOITop {
			return " (AI500+OI_Top dual signal)"
		}
		if hasAI500 && hasOILow {
			return " (AI500+OI_Low dual signal)"
		}
		if hasOITop && hasOILow {
			return " (OI_Top+OI_Low)"
		}
		if hasHyperMain && hasAI500 {
			return " (HyperMain+AI500)"
		}
		if hasHyperAll || hasHyperMain {
			return " (Hyperliquid)"
		}
		return " (Multiple sources)"
	} else if len(sources) == 1 {
		switch sources[0] {
		case "ai500":
			return " (AI500)"
		case "oi_top":
			return " (OI_Top OI increase)"
		case "oi_low":
			return " (OI_Low OI decrease)"
		case "piggy_dash":
			return " (Piggy Dash breakout engine)"
		case "static":
			return " (Manual selection)"
		case "hyper_all":
			return " (Hyperliquid All)"
		case "hyper_main":
			return " (Hyperliquid Top20)"
		}
	}
	return ""
}

// ============================================================================
// Technical Narrative (图形化技术面叙事)
// ============================================================================

// formatTechnicalNarrative renders the daily chart of a candidate as a
// human-readable "看图说话" narrative (headline character, dated key moves,
// volume anomalies, MA position, and a pattern judgment with resistance /
// box levels), mirroring how a trader would verbally walk through the chart.
// Returns "" when daily history is insufficient.
func formatTechnicalNarrative(data *market.Data) string {
	tf, ok := data.TimeframeData["1d"]
	if !ok || len(tf.Klines) < 10 {
		return ""
	}
	all := tf.Klines // full history (MA20 needs >= 20 closes)
	bars := all
	if len(bars) > 15 {
		bars = bars[len(bars)-15:]
	}
	n := len(bars)
	day := func(i int) string { return time.UnixMilli(bars[i].Time).Format("1/2") }

	// Period extremes.
	hi, hiIdx := math.Inf(-1), 0
	lo, loIdx := math.Inf(1), 0
	for i, b := range bars {
		if b.High > hi {
			hi, hiIdx = b.High, i
		}
		if b.Low < lo {
			lo, loIdx = b.Low, i
		}
	}
	last := bars[n-1].Close
	amp := (hi - lo) / lo * 100
	mid := (hi + lo) / 2

	// MA5 / MA20 of daily closes (computed on the full history).
	ma5, ma20 := smaLast(closesBars(all), 5), smaLast(closesBars(all), 20)

	var sb strings.Builder

	// ── 标题：波动特征 + 结构定性 ──
	maxDailyAbs := 0.0
	for i := 1; i < n; i++ {
		if bars[i-1].Close > 0 {
			if p := math.Abs(bars[i].Close/bars[i-1].Close-1) * 100; p > maxDailyAbs {
				maxDailyAbs = p
			}
		}
	}
	volWord := "窄幅整理"
	switch {
	case amp >= 40 || maxDailyAbs >= 8:
		volWord = "剧烈波动"
	case amp >= 20:
		volWord = "波动较大"
	}
	structWord := "宽幅震荡"
	highEarly := hiIdx < n-3 // 冲高不在最近 3 根内 = 冲高发生在窗口前段
	switch {
	case last >= hi*0.97:
		structWord = "强势上行"
	case highEarly && last < mid:
		structWord = "高位宽幅震荡"
	case loIdx >= n/2 && last <= lo*1.05:
		structWord = "弱势下行"
	case math.Abs(last-mid) <= (hi-lo)*0.1:
		structWord = "箱体整理"
	}
	sb.WriteString(fmt.Sprintf("技术面：%s、%s\n", volWord, structWord))
	sb.WriteString(fmt.Sprintf("近%d个交易日走势：\n", n))

	// ── 要点 1：窗口内最猛的单日涨/跌 ──
	worstIdx, worstPct, bestIdx, bestPct := -1, 0.0, -1, 0.0
	for i := 1; i < n; i++ {
		if bars[i-1].Close <= 0 {
			continue
		}
		pct := (bars[i].Close/bars[i-1].Close - 1) * 100
		if pct < worstPct {
			worstPct, worstIdx = pct, i
		}
		if pct > bestPct {
			bestPct, bestIdx = pct, i
		}
	}
	bigMove := func(idx, pct int, dir string) {
		if idx <= 0 {
			return
		}
		word := "大涨"
		if dir == "down" {
			word = map[bool]string{true: "暴跌", false: "大跌"}[pct <= -8]
		}
		sb.WriteString(fmt.Sprintf("· %s 单日%s %.1f%%（%s→%s）%s\n",
			day(idx), word, math.Abs(float64(pct)), fmtPx(bars[idx-1].Close), fmtPx(bars[idx].Close),
			volumeWord(bars[idx], bars)))
	}
	if bestIdx > 0 && bestPct >= 5 && (worstIdx < 0 || bestIdx < worstIdx) {
		bigMove(bestIdx, int(bestPct), "up")
	}
	if worstIdx > 0 && worstPct <= -5 {
		bigMove(worstIdx, int(worstPct), "down")
	}
	if bestIdx > 0 && bestPct >= 5 && worstIdx > 0 && bestIdx > worstIdx {
		bigMove(bestIdx, int(bestPct), "up")
	}

	// ── 要点 2：冲高极值与回落 ──
	sb.WriteString(fmt.Sprintf("· %s 冲高 %s 后", day(hiIdx), fmtPx(hi)))
	if loIdx > hiIdx {
		sb.WriteString(fmt.Sprintf("，%s 回落至 %s（较高点 -%.1f%%）", day(loIdx), fmtPx(lo), amp))
	}
	sb.WriteString("\n")

	// ── 要点 3：近期震荡区间与量能 ──
	rangeBars := bars
	if hiIdx > 2 {
		rangeBars = bars[hiIdx:]
	}
	rLo, rHi := math.Inf(1), math.Inf(-1)
	for _, b := range rangeBars {
		if b.Low < rLo {
			rLo = b.Low
		}
		if b.High > rHi {
			rHi = b.High
		}
	}
	rAmp := 0.0
	if rLo > 0 {
		rAmp = (rHi - rLo) / rLo * 100
	}
	if rHi > rLo && len(rangeBars) >= 3 {
		ampWord := map[bool]string{true: "振幅极大", false: "振幅较大"}[rAmp >= 20]
		sb.WriteString(fmt.Sprintf("· 随后在 %s-%s 之间来回拉锯，%s\n", fmtPx(rLo), fmtPx(rHi), ampWord))
	}
	// 放量日（量能 ≥ 窗口均量 1.8 倍且为最放量的一根）。
	volMaxIdx, volAvg := 0, 0.0
	for i, b := range bars {
		volAvg += b.Volume
		if b.Volume > bars[volMaxIdx].Volume {
			volMaxIdx = i
		}
	}
	volAvg /= float64(n)
	if volMaxIdx >= 0 && volAvg > 0 && bars[volMaxIdx].Volume >= volAvg*1.8 {
		trend := "拉升"
		if bars[volMaxIdx].Close < bars[volMaxIdx].Open {
			trend = "下杀"
		}
		sb.WriteString(fmt.Sprintf("· %s 放量%s至 %s（量能为均量 %.1f 倍）\n",
			day(volMaxIdx), trend, fmtPx(bars[volMaxIdx].Close), bars[volMaxIdx].Volume/volAvg))
	}

	// ── 要点 4：今日位置与均线关系 ──
	maPart := "均线数据不足"
	if ma5 > 0 && ma20 > 0 {
		switch {
		case last >= ma5 && last >= ma20:
			maPart = "站上 5 日线和 20 日线"
		case last >= ma5:
			maPart = "站上 5 日线但仍在 20 日线下方"
		case last >= ma20:
			maPart = "5 日线下方但仍站在 20 日线上"
		default:
			maPart = "5 日线和 20 日线下方"
		}
	}
	sb.WriteString(fmt.Sprintf("· 今日报 %s，%s\n", fmtPx(last), maPart))

	// ── 形态判断 ──
	pressureLo, pressureHi := pressureZone(bars, hi, hiIdx, last)
	sb.WriteString("形态判断：")
	switch structWord {
	case "高位宽幅震荡":
		sb.WriteString("高位宽幅震荡+冲高回落结构")
	case "强势上行":
		sb.WriteString("上升趋势加速结构，追多风险与回撤风险同步放大")
	case "弱势下行":
		sb.WriteString("冲高回落+弱势下行结构")
	default:
		sb.WriteString("箱体整理结构")
	}
	if pressureLo > 0 {
		sb.WriteString(fmt.Sprintf("，上方 %s-%s 为压力/套牢区", fmtPx(pressureLo), fmtPx(pressureHi)))
	}
	posWord := "下方"
	if math.Abs(last-mid) <= (hi-lo)*0.1 {
		posWord = "附近"
	} else if last > mid {
		posWord = "上方"
	}
	sb.WriteString(fmt.Sprintf("。当前价位于箱体中枢 %s %s", fmtPx(mid), posWord))
	if ma20 > 0 && last < ma20 {
		sb.WriteString("，短线性质是超跌反弹而非反转")
	} else if ma5 > 0 && last > ma5 && ma20 > 0 && last > ma20 {
		sb.WriteString("，短线趋势仍偏多")
	}
	sb.WriteString("。\n")

	return sb.String()
}

// pressureZone finds the overhead supply zone: the swing highs above the
// current price within the window (falls back to the period high itself).
func pressureZone(bars []market.KlineBar, periodHi float64, hiIdx int, last float64) (lo, hi float64) {
	if periodHi <= last {
		return 0, 0
	}
	second := math.Inf(-1)
	for i, b := range bars {
		if i == hiIdx {
			continue
		}
		if b.High > second && b.High <= periodHi*0.97 {
			second = b.High
		}
	}
	if second > last && second < periodHi {
		return second, periodHi
	}
	return periodHi * 0.97, periodHi
}

func closesBars(bars []market.KlineBar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

func smaLast(xs []float64, period int) float64 {
	if len(xs) < period {
		return 0
	}
	s := 0.0
	for _, v := range xs[len(xs)-period:] {
		s += v
	}
	return s / float64(period)
}

func volumeWord(bar market.KlineBar, bars []market.KlineBar) string {
	sum, cnt := 0.0, 0
	for _, b := range bars {
		sum += b.Volume
		cnt++
	}
	if cnt == 0 || sum/float64(cnt) <= 0 {
		return ""
	}
	avg := sum / float64(cnt)
	switch {
	case bar.Volume >= avg*1.8:
		return "，伴随放量"
	case bar.Volume <= avg*0.6:
		return "，但量能萎缩"
	default:
		return ""
	}
}

// fmtPx formats a price with magnitude-appropriate precision for the
// narrative text.
func fmtPx(v float64) string {
	switch {
	case v >= 1000:
		return strconv.FormatFloat(v, 'f', 1, 64)
	case v >= 100:
		return strconv.FormatFloat(v, 'f', 2, 64)
	case v >= 1:
		return strconv.FormatFloat(v, 'f', 4, 64)
	default:
		return strconv.FormatFloat(v, 'f', 6, 64)
	}
}

// ============================================================================
// Market Data Formatting
// ============================================================================

func (e *StrategyEngine) formatMarketData(data *market.Data, quantData *QuantData, ctx *Context, coin *CandidateCoin) string {
	// Signal layer: structured, normalized feature block replaces the raw
	// candle dump. Falls back to the legacy text dump only if computation fails.
	opt := SignalOptions{
		Now:                     time.Now(),
		PrimaryTF:               e.config.Indicators.Klines.PrimaryTimeframe,
		CurrentPrice:            data.CurrentPrice,
		LimitEntryOffsetPct:     e.config.RiskControl.LimitEntryOffsetPct,
		LimitEntryOffsetMode:    e.config.RiskControl.LimitEntryOffsetMode,
		LimitEntryOffsetATRMult: e.config.RiskControl.LimitEntryOffsetATRMult,
		LimitEntryOffsetMinPct:  e.config.RiskControl.LimitEntryOffsetMinPct,
		LimitEntryOffsetMaxPct:  e.config.RiskControl.LimitEntryOffsetMaxPct,
		SupplyZonePct:           e.config.RiskControl.OpenRejectSupplyPct,
		// Hard-entry gate inputs — the program pre-evaluates the strategy's
		// own gates per direction (review 2026-09-15).
		MinRR:             e.config.RiskControl.MinRiskRewardRatio,
		LimitEntryEnabled: e.config.RiskControl.LimitEntryEnabled,
		EntryTimingGate:   e.config.RiskControl.EntryTimingGate,
	}
	opt.Quant = quantData
	{
		frPct := e.config.CoinSource.ShortScanFundingRatePct
		if frPct <= 0 {
			frPct = 0.03
		}
		opt.ShortFundingCrowdPctP = frPct
		// STOCK_WEEKEND hard block: same predicate as the executor gate
		// (policy enabled && bstock symbol && US weekend) — surfaced in
		// hard_entry_gate.failed instead of only as a prose bullet.
		if noOpen := e.config.RiskControl.StockWeekendNoOpen; (noOpen == nil || *noOpen) &&
			market.IsBStockSymbol(data.Symbol) && market.IsUSMarketWeekend(opt.Now) {
			opt.StockWeekendBlock = true
		}
	}
	// Per-symbol enrichment (all programmatic, no AI judgment):
	if coin != nil {
		for _, src := range coin.Sources {
			if src == "short_scan" && coin.ShortScore > 0 {
				opt.ScannerBias = "short"
				break
			}
		}
		if coin.ScannerConflict {
			opt.ScannerConflict = true
		}
	}
	opt.QuoteVolume24hUsd = binanceQuoteVolume24h(data.Symbol)
	if m, err := binanceLongShortMetrics(data.Symbol); err == nil {
		opt.LongShortAccountRatio = m.AccountRatio
		opt.TopTraderPositionRatio = m.TopPosRatio
		opt.TakerBuySellRatio = m.TakerBuySell
	}
	opt.BtcCloses = binanceBTC1hCloses(72)
	if data.VendorStalenessPct != 0 {
		v := data.VendorStalenessPct
		opt.VendorStalenessPct = &v
	}
	if ctx != nil && len(ctx.SymbolStats) > 0 {
		if st, ok := ctx.SymbolStats[strings.ToUpper(data.Symbol)]; ok {
			opt.TraderHistory = st
		}
	}
	// Program-computed circuit-breaker verdict + minimum-notional feasibility
	// (09-15: the model was self-declaring LOSS_STREAK_BAN on just-won symbols
	// and re-deriving the small-account dead zone every cycle — both now
	// precomputed here).
	if ctx != nil {
		if until, ok := ctx.LossStreakBanned[strings.ToUpper(data.Symbol)]; ok {
			opt.LossStreakBannedUntil = until
		}
		if eq := ctx.Account.TotalEquity; eq > 0 {
			opt.EquityUSDT = eq
			// The binding minimum is the strategy-config min_position_size
			// (web strategy page) — the same value enforceMinPositionSize
			// rejects orders under. Config unset → executor default 12.
			opt.MinPositionSizeUSDT = e.config.RiskControl.MinPositionSize
			if opt.MinPositionSizeUSDT <= 0 {
				opt.MinPositionSizeUSDT = MinPositionSizeDefaultUSDT
			}
			riskPct := e.config.RiskControl.RiskPerTradePct
			if riskPct <= 0 {
				riskPct = 1.5
			}
			opt.RiskPct = riskPct
		}
	}
	// The stop noise floor is pure strategy config — independent of the
	// equity context above (it also gates the hard-entry gate's rr_scan).
	opt.SLMinATRMult = e.config.RiskControl.SLMinATRMult
	if quantData != nil {
		for _, oi := range quantData.OI {
			if oi == nil || oi.Delta == nil {
				continue
			}
			if d1h, ok := oi.Delta["1h"]; ok && d1h != nil {
				v := d1h.OIDeltaPercent
				opt.OI1hPct = &v
				break
			}
		}
	}
	if sig, err := ComputeSymbolSignals(data.Symbol, data, opt); err == nil {
		// Naming consistency: the JSON symbol MUST match the candidate heading
		// and the decision symbol — market data may carry a prefixed exchange
		// name (e.g. "XYZ:INTC") while the candidate list uses the normalized
		// form. Decisions route through the candidate name.
		if coin != nil && coin.Symbol != "" {
			sig.Symbol = strings.ToUpper(coin.Symbol)
		}
		// exit_rule_triggered is only meaningful when the trader actually
		// holds this symbol — otherwise it reads as a phantom exit signal.
		if ctx != nil {
			held := false
			for _, p := range ctx.Positions {
				if market.Normalize(p.Symbol) == market.Normalize(data.Symbol) {
					held = true
					break
				}
			}
			if !held {
				sig.ExitTriggered = false
			}
		}
		// Remember the anchors the model is being shown, so post-parse
		// compliance can snap open_*_limit prices back to the pre-computed
		// values when the model does its own math.
		if ctx != nil {
			if ctx.LimitAnchors == nil {
				ctx.LimitAnchors = make(map[string]*LimitAnchor)
			}
			ctx.LimitAnchors[market.Normalize(data.Symbol)] = &LimitAnchor{
				LimitBuy:  sig.LimitBuyPrice,
				LimitSell: sig.LimitSellPrice,
			}
			// Capture the rr_scan ceilings shown this cycle (09-16 point 2:
			// the wait→fill RR-decay dataset needs the program value from the
			// decision that waited, not a later re-derivation).
			if ctx.RRCeilings == nil {
				ctx.RRCeilings = make(map[string]*RRCeiling)
			}
			rc := &RRCeiling{}
			if sig.HardGate != nil {
				if sig.HardGate.Long != nil && sig.HardGate.Long.RR != nil {
					rc.LongRR, rc.LongUsable = sig.HardGate.Long.RR.BestRR, sig.HardGate.Long.RR.Usable
				}
				if sig.HardGate.Short != nil && sig.HardGate.Short.RR != nil {
					rc.ShortRR, rc.ShortUsable = sig.HardGate.Short.RR.BestRR, sig.HardGate.Short.RR.Usable
				}
			}
			ctx.RRCeilings[market.Normalize(data.Symbol)] = rc
		}
		// Data-incomplete symbols are barred from trading — nothing beyond
		// the DO-NOT block is rendered for them.
		return e.renderSignalBlock(sig)
	}

	var sb strings.Builder
	indicators := e.config.Indicators

	// Clearly label the coin symbol
	sb.WriteString(fmt.Sprintf("=== %s Market Data ===\n\n", data.Symbol))
	sb.WriteString(fmt.Sprintf("current_price = %.4f", data.CurrentPrice))

	if indicators.EnableEMA {
		sb.WriteString(fmt.Sprintf(", current_ema20 = %.3f", data.CurrentEMA20))
	}

	if indicators.EnableMACD {
		sb.WriteString(fmt.Sprintf(", current_macd = %.3f", data.CurrentMACD))
	}

	if indicators.EnableRSI {
		sb.WriteString(fmt.Sprintf(", current_rsi7 = %.3f", data.CurrentRSI7))
	}

	sb.WriteString("\n\n")

	if indicators.EnableOI || indicators.EnableFundingRate {
		sb.WriteString(fmt.Sprintf("Additional data for %s:\n\n", data.Symbol))

		if indicators.EnableOI && data.OpenInterest != nil {
			sb.WriteString(fmt.Sprintf("Open Interest: Latest: %.2f Average: %.2f\n\n",
				data.OpenInterest.Latest, data.OpenInterest.Average))
		}

		if indicators.EnableFundingRate {
			sb.WriteString(fmt.Sprintf("Funding Rate: %.2e\n\n", data.FundingRate))
		}
	}

	if len(data.TimeframeData) > 0 {
		timeframeOrder := []string{"1m", "3m", "5m", "15m", "30m", "1h", "2h", "4h", "6h", "8h", "12h", "1d", "3d", "1w"}
		for _, tf := range timeframeOrder {
			if tfData, ok := data.TimeframeData[tf]; ok {
				sb.WriteString(fmt.Sprintf("=== %s Timeframe (oldest → latest) ===\n\n", strings.ToUpper(tf)))
				e.formatTimeframeSeriesData(&sb, tfData, indicators)
			}
		}
	} else {
		// Compatible with old data format
		if data.IntradaySeries != nil {
			klineConfig := indicators.Klines
			sb.WriteString(fmt.Sprintf("Intraday series (%s intervals, oldest → latest):\n\n", klineConfig.PrimaryTimeframe))

			if len(data.IntradaySeries.MidPrices) > 0 {
				sb.WriteString(fmt.Sprintf("Mid prices: %s\n\n", formatFloatSlice(data.IntradaySeries.MidPrices)))
			}

			if indicators.EnableEMA && len(data.IntradaySeries.EMA20Values) > 0 {
				sb.WriteString(fmt.Sprintf("EMA indicators (20-period): %s\n\n", formatFloatSlice(data.IntradaySeries.EMA20Values)))
			}

			if indicators.EnableMACD && len(data.IntradaySeries.MACDValues) > 0 {
				sb.WriteString(fmt.Sprintf("MACD indicators: %s\n\n", formatFloatSlice(data.IntradaySeries.MACDValues)))
			}

			if indicators.EnableRSI {
				if len(data.IntradaySeries.RSI7Values) > 0 {
					sb.WriteString(fmt.Sprintf("RSI indicators (7-Period): %s\n\n", formatFloatSlice(data.IntradaySeries.RSI7Values)))
				}
				if len(data.IntradaySeries.RSI14Values) > 0 {
					sb.WriteString(fmt.Sprintf("RSI indicators (14-Period): %s\n\n", formatFloatSlice(data.IntradaySeries.RSI14Values)))
				}
			}

			if indicators.EnableVolume && len(data.IntradaySeries.Volume) > 0 {
				sb.WriteString(fmt.Sprintf("Volume: %s\n\n", formatFloatSlice(data.IntradaySeries.Volume)))
			}

			if indicators.EnableATR {
				sb.WriteString(fmt.Sprintf("3m ATR (14-period): %.3f\n\n", data.IntradaySeries.ATR14))
			}
		}

		if data.LongerTermContext != nil && indicators.Klines.EnableMultiTimeframe {
			sb.WriteString(fmt.Sprintf("Longer-term context (%s timeframe):\n\n", indicators.Klines.LongerTimeframe))

			if indicators.EnableEMA {
				sb.WriteString(fmt.Sprintf("20-Period EMA: %.3f vs. 50-Period EMA: %.3f\n\n",
					data.LongerTermContext.EMA20, data.LongerTermContext.EMA50))
			}

			if indicators.EnableATR {
				sb.WriteString(fmt.Sprintf("3-Period ATR: %.3f vs. 14-Period ATR: %.3f\n\n",
					data.LongerTermContext.ATR3, data.LongerTermContext.ATR14))
			}

			if indicators.EnableVolume {
				sb.WriteString(fmt.Sprintf("Current Volume: %.3f vs. Average Volume: %.3f\n\n",
					data.LongerTermContext.CurrentVolume, data.LongerTermContext.AverageVolume))
			}

			if indicators.EnableMACD && len(data.LongerTermContext.MACDValues) > 0 {
				sb.WriteString(fmt.Sprintf("MACD indicators: %s\n\n", formatFloatSlice(data.LongerTermContext.MACDValues)))
			}

			if indicators.EnableRSI && len(data.LongerTermContext.RSI14Values) > 0 {
				sb.WriteString(fmt.Sprintf("RSI indicators (14-Period): %s\n\n", formatFloatSlice(data.LongerTermContext.RSI14Values)))
			}
		}
	}

	return sb.String()
}

// signalBlockLegend documents the Structured Signal JSON fields. It is coin-independent,
// so it is rendered ONCE before the first signal block (positions/candidates) instead of
// once per coin — duplicating it across 8+ candidates wasted ~21k chars every cycle.
const signalBlockLegend = `Naming: each timeframe block reports ITS BAR GRANULARITY only. trend_window_return_pct = change over return_window_hours (window ≠ bar length). price_change_60m_live_pct / price_change_24h_live_pct = LIVE price vs 60m/24h ago. prev_hour_close_change_pct = last FULLY CLOSED hour, close-to-close. Do not mix them. All fields you need are in this JSON — read it carefully.
Naming addendum: macd_hist = MACD LINE (EMA12−EMA26) ÷ price ×100 — NOT the signal histogram; macd_trend = line slope vs previous bar.
Derived fields (program-computed, use directly — do not re-derive): role_tfs(execution/trend/regime 时间框架分工) | execution_filter(微趋势对齐预判: long_allowed/short_allowed, 开启入场时点闸门时为硬规则) | hard_entry_gate(程序对每方向开仓硬门的最终判定: allowed/entry_price/limit_allowed/stop_floor_pct/rr_scan/failed 阻断码列表——open_* 只允许输出在 allowed=true 的方向;wait/hold 的客观理由直接引用 failed,不要再自行组装三道门) | rr_scan(程序已遍历全部时间块的全部 S/R 结构位: best_rr=按噪声下限止损距离算出的 RR 上限,usable=false ⇒ 该方向 RR 门结构性不可能通过——结论措辞用 MAX_STRUCTURAL_RR=best_rr,而不是"最近阻力位 RR 不足";first_rr_ge_target=规则要求的最近达标结构位,开仓时 take_profit 直接采用它;但 stop_price 仅是门槛校验口径,不是给你的止损值——实际 stop_loss 按止损方法论另算,见"止损(程序校验)") | bias(方向判读的三个来源 scanner/structure/execution——"scanner=short" 只表示扫描器快照姿态偏空,不等于市场空头证据强,三者可以相反且都合法) | funding_rollover(程序按结算历史计算的"费率刚从高位回落": detected 是做空条件②的唯一可验证依据,字段缺失=历史拉取失败=UNKNOWN 按不满足处理) | limit_buy_price/limit_sell_price(0=锚点被程序抑制,该方向禁止挂单开仓,与 entry_rule_triggered 无关;注意: 这只是限价路径被禁≠该方向整体禁止——方向可开性只看 hard_entry_gate.allowed) | bb_ride/short_ride(15m 上轨/下轨骑行市价证据, 程序预算: 量能暴增+连续≥3 根同向收盘贴带; 分别对应市价例外二(多)/例外三(空)) | breakout.status(below|approach|broken_unconfirmed|confirmed|fake_break|retest_hold|extended, 相对1h结构位的突破判定,含量能/OI确认;extended=早已越过且未回踩) | directional_score(-100..+100 净方向共识,已剔除range噪音) | signal_conflict.types(SCANNER_VS_STRUCTURE|TIMEFRAME_SPLIT|SCANNER_VS_SCANNER — 两个扫描器对同一币给出相反方向,必须先表态信哪个) | data_quality.sufficient(false=历史长度不足以支撑EMA50/MACD类长窗指标) | funding_rate(原始费率小数,不是百分比:0.0001 = 0.01% 每结算期,勿再×100或当百分比读) | funding_annualized_pct(费率年化百分比,已按 funding_settle_hours 实测结算间隔年化,拥挤度判断直接用它) | no_trade_reason(hold/wait必填)
数据新鲜度优先级: 同一字段冲突时取时间戳更新者——Structured Signal timestamp > 实时 derivatives/liquidity > scanner/rankings 快照。榜单与 hint 里的价格/费率是旧快照,严禁参与 entry/SL/TP/RR 精确计算(它们与结构化现价可差 1% 以上),只用于市场情绪/资金轮动语境。

If signal_conflict.directional_conflict is true, you MUST resolve the conflict explicitly with your own evidence in the reasoning before any trade decision.

`

// renderSignalBlock renders the structured signal block: a plain-language
// summary header (structure state, key levels, derivatives, rules) followed by
// the normalized per-timeframe JSON features.
func (e *StrategyEngine) renderSignalBlock(sig *SymbolSignal) string {
	var sb strings.Builder

	if !sig.DataComplete {
		sb.WriteString(fmt.Sprintf("=== %s — DATA INCOMPLETE ===\n", sig.Symbol))
		for _, w := range sig.Warnings {
			sb.WriteString("⚠ " + w + "\n")
		}
		sb.WriteString("DO NOT trade this symbol this cycle.\n\n")
		return sb.String()
	}

	sb.WriteString(fmt.Sprintf("=== %s Structured Signal (all times UTC, closed candles only) ===\n", sig.Symbol))
	sb.WriteString(RenderSignalJSON(sig))
	sb.WriteString("\n")
	return sb.String()
}
func boolZhEN(b bool) string {
	if b {
		return "TRIGGERED"
	}
	return "not triggered"
}

func formatLevels(levels []float64) string {
	parts := make([]string, 0, len(levels))
	for _, l := range levels {
		parts = append(parts, fmt.Sprintf("%.5g", l))
	}
	return strings.Join(parts, ", ")
}

func tfLabel(dummy string) string { return "primary" }

// ResistanceIncl / SupportIncl merge per-TF pivot lists with the primary TF's
// Bollinger-supplemented lists (already merged in the TF signal).
// NearestLevels merges S/R candidates from ALL timeframes and returns the
// closest resistance above and closest support below the current price.
func (sig *SymbolSignal) NearestLevels() (resistance, support float64) {
	resistance, support = 0, 0
	for _, tf := range sig.Timeframes {
		for _, r := range tf.Resistance {
			if r > sig.Price && (resistance == 0 || r < resistance) {
				resistance = r
			}
		}
		for _, s := range tf.Support {
			if s < sig.Price && (support == 0 || s > support) {
				support = s
			}
		}
	}
	return resistance, support
}

func (sig *SymbolSignal) ResistanceIncl() []float64 {
	if tf := primaryTFSignal(sig, sig.PrimaryTF); tf != nil {
		return tf.Resistance
	}
	return nil
}

func (sig *SymbolSignal) SupportIncl() []float64 {
	if tf := primaryTFSignal(sig, sig.PrimaryTF); tf != nil {
		return tf.Support
	}
	return nil
}

func (e *StrategyEngine) formatTimeframeSeriesData(sb *strings.Builder, data *market.TimeframeSeriesData, indicators store.IndicatorConfig) {
	if len(data.Klines) > 0 {
		sb.WriteString("Time(UTC)      Open      High      Low       Close     Volume\n")
		for i, k := range data.Klines {
			t := time.Unix(k.Time/1000, 0).UTC()
			timeStr := t.Format("01-02 15:04")
			marker := ""
			if i == len(data.Klines)-1 {
				marker = "  <- current"
			}
			sb.WriteString(fmt.Sprintf("%-14s %-9.4f %-9.4f %-9.4f %-9.4f %-12.2f%s\n",
				timeStr, k.Open, k.High, k.Low, k.Close, k.Volume, marker))
		}
		sb.WriteString("\n")
	} else if len(data.MidPrices) > 0 {
		sb.WriteString(fmt.Sprintf("Mid prices: %s\n\n", formatFloatSlice(data.MidPrices)))
		if indicators.EnableVolume && len(data.Volume) > 0 {
			sb.WriteString(fmt.Sprintf("Volume: %s\n\n", formatFloatSlice(data.Volume)))
		}
	}

	if indicators.EnableEMA {
		if len(data.EMA20Values) > 0 {
			sb.WriteString(fmt.Sprintf("EMA20: %s\n", formatFloatSlice(data.EMA20Values)))
		}
		if len(data.EMA50Values) > 0 {
			sb.WriteString(fmt.Sprintf("EMA50: %s\n", formatFloatSlice(data.EMA50Values)))
		}
	}

	if indicators.EnableMACD && len(data.MACDValues) > 0 {
		sb.WriteString(fmt.Sprintf("MACD: %s\n", formatFloatSlice(data.MACDValues)))
	}

	if indicators.EnableRSI {
		if len(data.RSI7Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI7: %s\n", formatFloatSlice(data.RSI7Values)))
		}
		if len(data.RSI14Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI14: %s\n", formatFloatSlice(data.RSI14Values)))
		}
	}

	if indicators.EnableATR && data.ATR14 > 0 {
		sb.WriteString(fmt.Sprintf("ATR14: %.4f\n", data.ATR14))
	}

	if indicators.EnableBOLL && len(data.BOLLUpper) > 0 {
		sb.WriteString(fmt.Sprintf("BOLL Upper: %s\n", formatFloatSlice(data.BOLLUpper)))
		sb.WriteString(fmt.Sprintf("BOLL Middle: %s\n", formatFloatSlice(data.BOLLMiddle)))
		sb.WriteString(fmt.Sprintf("BOLL Lower: %s\n", formatFloatSlice(data.BOLLLower)))
	}

	sb.WriteString("\n")
}

func formatFlowValue(v float64) string {
	sign := ""
	if v >= 0 {
		sign = "+"
	}
	absV := v
	if absV < 0 {
		absV = -absV
	}
	if absV >= 1e9 {
		return fmt.Sprintf("%s%.2fB", sign, v/1e9)
	} else if absV >= 1e6 {
		return fmt.Sprintf("%s%.2fM", sign, v/1e6)
	} else if absV >= 1e3 {
		return fmt.Sprintf("%s%.2fK", sign, v/1e3)
	}
	return fmt.Sprintf("%s%.2f", sign, v)
}

func formatFloatSlice(values []float64) string {
	strValues := make([]string, len(values))
	for i, v := range values {
		strValues[i] = fmt.Sprintf("%.4f", v)
	}
	return "[" + strings.Join(strValues, ", ") + "]"
}

// ============================================================================
// Portfolio-concentration & history annotations (candidate rows)
// ============================================================================

// pearsonOfReturns computes the Pearson correlation of two price series'
// 1-bar percent returns over the aligned tail (up to 28 samples — the same
// window the signal layer's BTC correlation uses). Returns 0 when either
// series is too short or degenerate.
func pearsonOfReturns(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n < 12 {
		return 0
	}
	a = a[len(a)-n:]
	b = b[len(b)-n:]
	var ra, rb []float64
	for i := 1; i < n; i++ {
		if a[i-1] > 0 && b[i-1] > 0 {
			ra = append(ra, a[i]/a[i-1]-1)
			rb = append(rb, b[i]/b[i-1]-1)
		}
	}
	if len(ra) < 10 {
		return 0
	}
	var sa, sb float64
	for i := range ra {
		sa += ra[i]
		sb += rb[i]
	}
	ma, mb := sa/float64(len(ra)), sb/float64(len(rb))
	var cov, va, vb float64
	for i := range ra {
		da, db := ra[i]-ma, rb[i]-mb
		cov += da * db
		va += da * da
		vb += db * db
	}
	if va == 0 || vb == 0 {
		return 0
	}
	return cov / (sqrtF(va) * sqrtF(vb))
}

func sqrtF(v float64) float64 {
	if v <= 0 {
		return 0
	}
	x := v
	for i := 0; i < 40; i++ { // Newton-Raphson, avoids importing math for one call
		x = 0.5 * (x + v/x)
		if x*x-v < 1e-12 && x*x-v > -1e-12 {
			break
		}
	}
	return x
}

// closes1hFromMarketData extracts the 1h close series from the shared market
// snapshot (nil when the symbol has no usable 1h data).
func closes1hFromMarketData(data *market.Data) []float64 {
	if data == nil {
		return nil
	}
	tf, ok := data.TimeframeData["1h"]
	if !ok || tf == nil || len(tf.Klines) < 12 {
		return nil
	}
	out := make([]float64, len(tf.Klines))
	for i, k := range tf.Klines {
		out[i] = k.Close
	}
	return out
}

// concentrationWarnings lists open positions whose 1h returns are highly
// correlated with the candidate's — the account stacking the same beta
// factor without noticing. Same-direction positions only (a hedge is not
// concentration). Pure over the shared snapshot: no extra API calls.
func concentrationWarnings(ctx *Context, symbol string) []string {
	cand := closes1hFromMarketData(ctx.MarketDataMap[symbol])
	if len(cand) == 0 {
		return nil
	}
	var out []string
	for _, p := range ctx.Positions {
		if p.Symbol == symbol {
			continue
		}
		posCloses := closes1hFromMarketData(ctx.MarketDataMap[p.Symbol])
		if len(posCloses) == 0 {
			continue
		}
		r := pearsonOfReturns(cand, posCloses)
		if r >= 0.75 {
			out = append(out, fmt.Sprintf("与持仓%s相关性%.2f(同向beta敞口,注意集中度)", p.Symbol, r))
		}
	}
	return out
}

// recentHistoryNote summarizes this trader's own closed-trade record on the
// symbol when it has been loss-heavy — the "same logic, same place, same
// loss" guard rail rendered as evidence next to the candidate.
func recentHistoryNote(ctx *Context, symbol string) string {
	st, ok := ctx.SymbolStats[market.Normalize(symbol)]
	if !ok || st == nil || st.ClosedTrades < 2 {
		return ""
	}
	if st.WinRatePct < 50 && st.RealizedPnL < 0 {
		return fmt.Sprintf("近期该币%d笔交易%d胜(胜率%.0f%%),净亏%.2fU——同位置重复亏损风险", st.ClosedTrades, st.Wins, st.WinRatePct, st.RealizedPnL)
	}
	return ""
}

// expectancyR estimates the average outcome in R multiples from aggregate
// stats: winners average (AvgWin/AvgLoss) R, losers are assumed ≈ -1R
// (risk-based sizing makes that approximately true). 0 when AvgLoss is 0 or
// no trades yet.
func expectancyR(winRatePct, avgWin, avgLoss float64) float64 {
	if avgLoss <= 0 || winRatePct <= 0 {
		return 0
	}
	w := winRatePct / 100
	return w*(avgWin/avgLoss) - (1 - w)
}
