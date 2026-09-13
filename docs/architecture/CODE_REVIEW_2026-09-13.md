# NoFx 代码审查报告（2026-09-13）

审查范围：`user_prompt` 数据计算与构建、程序强制风控闸门、止盈止损计算逻辑。方法：全文通读核心源文件（非抽样 grep），交叉核对 `*_test.go` 已钉死的不变量，并对每个可疑点回溯到实际调用链确认是否为"活代码路径"。

> 本报告只做发现与定性，不包含代码修改；严重级别标注供排期参考。
>
> **更新（2026-09-13，同日）**：第 6 节汇总表中的 4 个核心发现（#1-#4）已修复，详见各条目后的"状态"列与文末的[修复记录](#8-修复记录2026-09-13)。

---

## 目录

1. [项目速览](#1-项目速览)
2. [架构总览](#2-架构总览)
3. [user_prompt 数据计算审查](#3-user_prompt-数据计算审查)
4. [风控闸门审查](#4-风控闸门审查)
5. [止盈止损计算审查](#5-止盈止损计算审查)
6. [问题清单汇总（按严重级别）](#6-问题清单汇总按严重级别)
7. [值得保留的好设计](#7-值得保留的好设计)
8. [修复记录（2026-09-13）](#8-修复记录2026-09-13)

---

## 1. 项目速览

NoFx 是一个用大语言模型（LLM）做交易决策、但把所有硬风控约束都写死在 Go 代码里执行的自动化交易机器人（支持 Binance 合约、Binance 代币化股票 bstock，以及其他交易所适配层）。核心设计哲学贯穿整个代码库：

- **LLM 只负责"判断"，不负责"算数"**：所有数值（ATR、锚点价格、风险预算、仓位大小上限）都在 Go 里预计算好，LLM 只读结果、不做推导。
- **LLM 的决策不是最终指令，而是提案**：每条 `open_*`/`close_*` 决策在真正下单前都要过一层 Go 硬闸门（`applyHardRiskGates` + `validateOpenRisk` + 若干专项 gate），闸门拒绝的决策直接丢弃，LLM 无法绕过。
- **配置驱动、默认值内聚**：几乎所有风控参数（止损宽度、止盈阶梯、连亏熔断、点差门等）都有"0/未配置 = 默认值，负数 = 禁用"的统一约定，并且默认值被单一函数（如 `kernel.EarlyCloseHours`、`kernel.MaxSpreadPct`）收拢，供 prompt 渲染和执行闸门共享，理论上杜绝"prompt 说一套、代码做另一套"的配置漂移。

代码规模：kernel（决策引擎/prompt 构建）+ trader（下单执行/风控闸门）两大模块约 2.5 万行 Go 代码，前端为 React/TypeScript 仪表盘。

---

## 2. 架构总览

```
                         ┌─────────────────────┐
                         │   kernel.Context     │  ← 账户/持仓/候选币/历史统计快照
                         └──────────┬───────────┘
                                    │
                    ┌───────────────┴────────────────┐
                    │      kernel/engine_prompt.go     │  实际生效的 prompt 构建路径
                    │  BuildSystemPrompt (风控规则/     │
                    │  仓位公式/输出格式)               │
                    │  BuildUserPrompt (账户/持仓/候选  │
                    │  币多周期信号/扫描器证据)          │
                    └───────────────┬──────────────────┘
                                    │  LLM 调用
                                    ▼
                         ┌─────────────────────┐
                         │   kernel.Decision     │  LLM 输出的结构化 JSON
                         └──────────┬───────────┘
                                    │
                    ┌───────────────┴────────────────┐
                    │   trader.applyHardRiskGates      │  程序强制闸门（LLM 无法绕过）
                    │   - 连亏熔断 / 入场时点 / regime  │
                    │   - 最短持仓 / 提前平仓 / 破位保护 │
                    └───────────────┬──────────────────┘
                                    │
                    ┌───────────────┴────────────────┐
                    │  trader/auto_trader_orders.go    │  下单执行
                    │  validateOpenRisk / 点差门 /      │
                    │  保证金预算门 / 仓位价值比例       │
                    └───────────────┬──────────────────┘
                                    │
                    ┌───────────────┴────────────────┐
                    │  trader/auto_trader_vol.go +     │  持仓期风控（周期性后台任务）
                    │  auto_trader_risk.go             │
                    │  - 波动率目标调仓 / 跟踪止损       │
                    │  - 1R 保本+减仓 / TP 阶梯 / 回撤保护│
                    │  - 保护单看门狗（补挂缺失SL/TP）   │
                    └──────────────────────────────────┘
```

**关键发现**：kernel 目录里存在**两套互相独立的 prompt 构建系统**：

- **活的一套**：`kernel/engine_prompt.go`（`StrategyEngine.BuildSystemPrompt`/`BuildUserPrompt`），被 `kernel/engine_analysis.go:117-120` 的主决策循环调用，是唯一真正发给 LLM 的 prompt。
- **死代码一套**：`kernel/prompt_builder.go`（`PromptBuilder` 结构体）+ `kernel/formatter.go` 的绝大部分（`FormatContextForAI`/`formatContextData` 及其一系列 `format*ZH`/`format*EN` 函数）。全仓搜索确认除测试文件外**没有任何生产代码调用这套路径**。详见 [3.1](#31-两套-prompt-构建系统仅一套是活代码)。

---

## 3. user_prompt 数据计算审查

### 3.1 两套 prompt 构建系统，仅一套是活代码

**文件**：`kernel/prompt_builder.go`（全文 376 行）、`kernel/formatter.go`（648 行中约 600+ 行）
**严重级别**：中（维护风险，非当前功能性 bug）

`kernel/schema.go` 定义的 `TradingRules` 结构体（含最大保证金使用率 30%、单笔止损 -5%、止盈阶梯 3%/5%/8% 分批平仓、单仓位上限 15% 权益等**样例性质的静态规则**）与实际执行逻辑中的真实默认值明显不一致：

| 字段 | schema.go `TradingRules` 中的值 | 实际执行默认值 |
|---|---|---|
| 最大保证金使用率 | 30% | `MaxMarginUsage` 默认 90%（`auto_trader_risk.go:699`） |
| 止盈阶梯 | 3%/5%/8% 分批 33/50/100% | `TpTrimProfitPct`/`TpFullProfitPct` 默认 10%/25%（`anchor_offset.go:167-179`） |
| 单笔止损 | 固定 -5% | ATR 动态区间 `[floor, max(2×ATR(4h), 8%)]`（`validateOpenRisk`） |
| 单仓位上限 | 15% 权益 | BTC/ETH 5x 权益、山寨币 1x 权益（`enforcePositionValueRatio`） |

**好消息**：经全仓 `grep -rn "TradingRules\."` 确认，`TradingRules` 结构体的字段从未被任何函数读取或渲染——`GetSchemaPrompt` 只遍历 `DataDictionary`（字段名/单位/公式的说明性文档，本身准确），不触碰 `TradingRules`。同样，`PromptBuilder.BuildSystemPrompt/BuildUserPrompt` 和 `formatter.go` 里那批 `format*ZH/EN` 函数也未被生产代码调用（`api/strategy.go` 等只调用 `engine.BuildSystemPrompt`/`engine.BuildUserPrompt`，这是 `StrategyEngine` 而非 `PromptBuilder` 的方法）。

**结论**：这不是一个当前会误导 LLM 的活 bug，而是约 1000 行**过时的死代码**，其中夹杂着与当前风控数值严重不符的硬编码规则。风险在于：未来有人不了解这段历史，误把 `TradingRules`/`PromptBuilder`/`formatter.go` 当作"另一个可能被用到的路径"去修改或重新接入，会立刻引入过时的风控数值。建议要么删除，要么加顶部注释明确标注"DEPRECATED / 未接入主流程"。

### 3.2 单位契约整体清晰，且有专门机制防止 LLM 误读

`kernel/signal_layer.go` 里的 `funding_rate`（原始小数）与 `funding_annualized_pct`（已按真实结算间隔年化的百分比）并存，且 prompt 文本（`engine_prompt.go:1266`）显式用一句话把两者的语义边界钉死："`funding_rate` 原始费率小数，不是百分比…勿再×100或当百分比读"。`PriceChange1h`/`PriceChange4h` 等字段命名和单位在 `market/types.go` 也有行内注释。这类"防误读注释直接写进发给 LLM 的文本里"的做法在整个 prompt 构建中反复出现（如 `role_tfs`/`execution_filter`/`breakout.status`/`directional_score` 的枚举含义说明），是这份代码库最值得称道的地方之一，参见 [第 7 节](#7-值得保留的好设计)。

### 3.3 仓位公式示例文本会自解释但依赖调用方传入正确的 equity

`engine_prompt.go:111-119` 的仓位计算说明文本用**当前真实 equity** 动态生成示例（`accountEquity` 参数），而不是硬编码数字——这是 2026-09-13 之前的一次修复（注释标注"审计 09-13 #1"），此前的写法是硬编码示例数字，会与真实账户权益脱节。目前看这个点已经被修复且没有回归，值得作为"曾经犯过的错误类型"记录在案，供未来类似 prompt 文本改动时对照检查。

### 3.4 `formatCurrentPositionsZH/EN`（死代码路径）里的止损/回撤提示阈值写死，不读配置

**文件**：`kernel/formatter.go:260-267`, `527-533`
**严重级别**：低（因为是死代码，不影响生产）

```go
if drawdown < -0.30*pos.PeakPnLPct && pos.PeakPnLPct > 0.02 {
    // 硬编码 30% 回撤提示
}
if pos.UnrealizedPnLPct < -4.0 {
    // 硬编码 -4%/-5% 止损提示
}
```

这两处直接写死 30%/-4%/-5%，完全不读 `RiskControl.PeakDrawdownMaxDDPct`（活代码路径 `drawdownProtectThresholds` 默认是 5/55，见 4.3）。因为整个文件不在活路径上所以不构成当前风险，但如果之后要清理死代码或者误接回主流程，这是最先会咬人的地方。

---

## 4. 风控闸门审查

### 4.1 硬闸门总览：LLM 决策必须依次穿过多层校验才能落地

主循环收到 LLM 的 `[]Decision` 后，顺序经过：

1. `applyHardRiskGates`（`auto_trader_risk.go:1296`）：股票周末禁开、连亏熔断、入场时点闸门（子小时趋势对齐）、regime 线守卫、1d 逆势做空拦截、破位保护平仓拦截、最短持仓/提前平仓锁。
2. 各 `execute*WithRecord` 内部（`auto_trader_orders.go`）：点差门 → 已有同向持仓检查 → `validateOpenRisk`（强制止损/RR/止损宽度区间）→ 仓位价值比例上限 → 风险反推仓位缩放 → 可用保证金缩放 → 最小下单量 → 保证金预算门。

每一层都是"闸门拒绝就整条决策丢弃"，不存在 LLM 输出可以覆盖 Go 判断的路径——这个设计目标本身达成得很扎实。

### 4.2 多空对称性：入场时点闸门已对称，但要注意历史教训

`auto_trader_risk.go:1336-1382` 的入场时点闸门（`EntryTimingGate`）明确写了"对称策略（审计 09-13）"：多头允许 up/pullback，空头允许 down/rally（下跌趋势中的反弹），range 两边都拦。这与记忆中记录的"09-13 用户裁定 3 优先于 2，classifyTrend 重构为对称四象限"的历史修复一致，目前代码里确实是对称的，**没有发现新的多空不对称回归**。

### 4.3 回撤保护阈值来源可信，但 `checkPositionDrawdown` 里有一处潜在的 panic 风险已被显式防御

**文件**：`trader/auto_trader_risk.go:96-110`

```go
symbol := pos["symbol"].(string)
side := pos["side"].(string)
entryPrice := pos["entryPrice"].(float64)
```

这几行是无 `, ok` 的强制类型断言，如果交易所返回的 map 缺失这些 key 或类型不符会直接 panic，拖垮整个监控 goroutine（虽然 `entryPrice <= 0` 之后有防御，但那是数值防御，不是类型断言防御）。同一文件后续大部分代码（如 `enforceMaxPositions`、`usedMarginOf` 等）都改用了 `, ok` 的安全断言模式，这里是个例外。**严重级别：中**——真触发需要交易所返回异常 map（缺字段或类型错位），发生概率低但后果是整个回撤监控协程崩溃且不会自动重启（`startDrawdownMonitor` 里没有 `recover`）。

### 4.4 止盈阶梯与 1R 锁盈的优先级切换逻辑正确，但两套机制共享状态字段容易在未来产生耦合陷阱

`kernel/anchor_offset.go:188-198` 的 `TpTierAction` 里，当 `ProfitLockRMult(rc) > 0`（默认开启）时会直接跳过 ROE 止盈阶梯的"trim"档（`return ""`），只保留"full"档（全平）。这是合理的设计：1R 锁盈已经在更早的浮盈阶段减了 50% 仓位，不应该再被 ROE 阶梯二次瓜分剩余仓位。但两套机制共享同一批状态 map（`tpTrimDone`/`r1TrimDone` 互相在对方逻辑里被置位，如 `auto_trader_risk.go:159-161` 的 TP full 分支同时置位两个 flag），这种"用另一套机制的状态位来抑制自己"的写法虽然当前测试用例覆盖到位，但后续任何一方逻辑改动都容易在不经意间破坏另一方的假设。建议至少在两个 map 定义处互相加注释指明这层隐式耦合（目前只在调用点的行内注释里提到，定义处没有）。

### 4.5 保证金预算门（`marginBudgetBlocksOpen`）与仓位价值比例上限（`enforcePositionValueRatio`）是两套独立上限，prompt 文本已经做了"哪个先触顶"的说明，但执行顺序值得关注

**文件**：`auto_trader_orders.go:120-156`

执行顺序是：先按仓位价值比例上限（`enforcePositionValueRatio`）砍仓位 → 再按风险预算/止损距离砍仓位（`clampSizeToRisk`）→ 再按可用余额砍仓位（`marginFactor` 那段）→ 最后才检查保证金预算门（`marginBudgetBlocksOpen`）。

这个顺序意味着：如果前三步已经把仓位砍到很小，但账户已经因为**其他持仓**占用了大量保证金，最后一步的保证金预算门仍然可能因为"存量占用 + 新单"超预算而**整单拒绝**，而不是"再砍小一点去满足预算"。也就是说这不是一个连续收窄的单调过程，而是"砍到某个值后一票否决"。行为本身是安全的（宁可拒单不多开），但如果未来产品需求变成"尽量开一个不超预算的最大仓位"而不是"超预算就直接拒绝"，这里的顺序和实现方式需要重新设计成迭代收敛而非线性砍量。**当前不算 bug，但值得记录为架构决策点**。

### 4.6 `spreadBlocksOpen` 和其它 fail-open 闸门存在"数据源不可用 = 直接放行"的一致设计，但也意味着这些闸门在数据源故障时形同虚设

点差门（`topOfBookSpreadPct` 返回 0 或订单簿接口不存在时直接放行）、保证金预算门（`GetPositions` 报错时放行）、连亏熔断（store 为 nil 时放行）等全部采用 fail-open 策略。这是刻意的设计取舍（注释明确写了"fail-open: 闸门不能因为看不到数据就把仓位卡死"），对于"数据缺失不应阻塞正常交易"这个目标是合理的，但需要清楚：**这些闸门在依赖的数据源（订单簿接口、交易所持仓查询、本地 store）故障时会静默失效而不是报警**。建议后续在这些 fail-open 分支里增加一次性的监控告警（目前部分路径有日志但没有 Telegram 通知，例如点差门数据获取失败只是 `return false, ""`，没有任何日志痕迹）。

---

## 5. 止盈止损计算审查

### 5.1 止损宽度公式：下限用 1h ATR、上限用 4h ATR，代码与 prompt 文案逐字对齐

`validateOpenRisk`（`auto_trader_risk.go:398-471`）实现的止损区间：

- **下限（噪声地板）**：`SLMinATRMult × ATR(1h)`，仅在 `SLMinATRMult > 0` 时生效。
- **上限（离群止损帽）**：`max(2 × ATR(4h), 8%)`，恒定生效。

这与 `kernel/engine_prompt.go:457-466` 里发给 LLM 的止损区间说明文本逐字对应（连"取宽不取窄"这种措辞都对上了），且代码注释明确提到这是为了修复"VTHOUSDT 曾把可执行区间 `[-, 14.29%]` 误报成 `[12%, 14.29%]` 而否掉整笔交易"的历史 bug——目前看这次对齐是稳固的，测试文件 `anchor_atr_test.go` 也钉死了这个行为。

### 5.2 1R 保本止损 + 50% 减仓机制自限性设计正确，但依赖一个不太直观的隐式不变量

**文件**：`trader/auto_trader_vol.go:199-244`、`kernel/anchor_offset.go:216-243`

每个周期都会重新调用 `ProfitLockTargets(side, entry, initialSL, initialSL, markPrice, lockR)`——注意第三个参数（`currentSL`）被直接传成 `initialSL`，而不是"当前实际生效的止损价"。乍看像 bug（应该传当前止损），但实际是自洽的：一旦保本止损第一次触发，`SetRecordedStopLoss` 会把 `at.positionStopLoss[posKey]` 更新为 `entry`；下个周期读到的 `initialSL`（其实是 `GetRecordedStopLoss` 的当前值）就已经等于 `entry`，此时 `ProfitLockTargets` 内部的 `initialDist := entry - initialSL` 算出来是 0，函数在 `initialDist <= 0` 分支直接返回 `(false, false)`，天然不会重复触发保本移动。

**这个自限性没有问题，但代码的参数命名（`initialSL` 被复用去表示"当前止损"）容易让后来者误读，认为传参写错了。** 建议要么加一行注释说明"读到的是当前记录的止损，不是开仓时的原始止损；一旦保本触发后二者相等，函数自然短路"，要么显式重命名变量。**严重级别：低（正确性没问题，可读性/可维护性问题）**。

### 5.3 跟踪止损 `trailingDecision` 的"只紧不松"约束是纯函数级别强制的，测试覆盖到位

`trader/auto_trader_vol.go:75-105` 的 `trailingDecision` 函数对多空两个方向分别用 `math.Max`/`math.Min` 确保新止损不会比当前止损更差，且要求候选止损相对当前止损的改善幅度 ≥ `trailMinImprove`（0.1%）才会真正移动，避免频繁小幅调整刷单。`TestTrailingDecision` 覆盖了"未到激活距离不移动"等边界。这是一个值得称道的模式：约束写在纯函数签名和返回值里，而不是散落在调用方的 if 判断里，便于单测覆盖全部分支。

### 5.4 `adjust_stop_loss`（AI 主动调整止损）的"只收紧"约束是硬 Go 校验，不是仅靠 prompt 文案

`stopMoveTightens`（`auto_trader_risk.go:825-833`）在执行层再校验一遍"新止损必须比当前止损更紧、且不能穿越现价"，即使 LLM 输出了一个更宽松或者穿价的止损，`executeAdjustStopLossWithRecord` 也会在真正调用 `moveStopExchange` 之前拒绝并返回错误（`auto_trader_risk.go:897-900`）。这是"prompt 说了但仍然代码兜底"的双保险模式，是文档里反复强调的设计原则的具体落地。

### 5.5 保护单看门狗补挂逻辑正确处理了与 TP-runner 的竞争，但"错边"检测只记日志不报警

`processProtectionWatchdog`（`auto_trader_risk.go:980-1063`）在补挂止盈前会检查 `!at.tpRunnerDone(posKey)`，避免在跟踪止损已经接管出场逻辑后又把已撤销的固定止盈单重新挂回去——这个互斥关系处理正确。但当补挂前发现"记录的止损/止盈价格在错误的一侧"（例如 `sl` 大于等于多头 markPrice）时，代码只是 `logger.Infof` 记一条警告日志，**没有任何 Telegram 告警**，而这种情况恰恰意味着这个仓位当前完全没有交易所保护（既没有旧单也没有新单），属于应该主动报警的场景。**严重级别：中**。相比之下，`placeProtectiveOrders` 在开仓失败时是会发 Telegram 告警的（`auto_trader_orders.go:361`），看门狗这里的静默降级和开仓路径的处理方式不一致，值得对齐。

### 5.6 `executePartialCloseWithRecord` 的累计部分平仓上限校验正确，且与"结构位分批止盈"的产品语义吻合

`auto_trader_risk.go:910-947`：每次部分平仓前检查 `already + decision.CloseFraction > 0.75` 直接拒绝，保证单个仓位的累计部分平仓不超过 75%，剩余至少 25% 必须走 `close_*` 完整平仓路径退出。这个 75% 硬顶配合"部分平仓不影响 SL/TP 挂单"（订单是 reduce-only 市价平仓，不触碰交易所保护单）的实现，逻辑清晰，`partialTrimmed` 状态在 `ClearPeakPnLCache`（仓位清空时）被正确重置，没有发现状态泄漏。

---

## 6. 问题清单汇总（按严重级别）

| # | 严重级别 | 位置 | 问题 | 状态 |
|---|---|---|---|---|
| 1 | 中 | `kernel/schema.go` `TradingRules`；`kernel/prompt_builder.go` 全文；`kernel/formatter.go` 大部分函数 | 约 1000 行死代码，包含与当前实际风控默认值（保证金上限/止盈阶梯/止损方式/仓位上限）严重不符的硬编码规则；未来若被误接回主流程或被当作文档参考会直接引入过时数值 | **已修复**——`prompt_builder.go`/`prompt_builder_test.go`/`formatter.go` 整文件删除；`schema.go` 中的 `TradingRules`/`BilingualRuleDef`/`CommonMistake`/`CommonMistakes` 一并删除，保留仍在活路径上的 `DataDictionary`/`OIInterpretation`/`GetSchemaPrompt` |
| 2 | 中 | `trader/auto_trader_risk.go:96-99`（`checkPositionDrawdown`） | 交易所持仓 map 的类型断言无 `, ok` 保护，缺字段/类型错位会 panic 拖垮回撤监控协程且无 `recover` | **已修复**——全部改为 `, ok` 安全断言、缺字段时记日志并 `continue` 跳过该条持仓；另加 `safeCheckPositionDrawdown` 包装函数在监控循环里兜底 `recover()`，防止未来任何回归重新引入的 panic 拖垮整个协程 |
| 3 | 中 | `trader/auto_trader_risk.go:1033-1052`（`processProtectionWatchdog`） | 检测到止损/止盈"挂在错误一侧"（等价于该仓位当前完全无保护）时只记日志不发 Telegram 告警，与同文件其它保护失败路径的告警力度不一致 | **已修复**——新增 `alertUnprotectedPosition` 辅助函数（复用 `gateNotifyRecord` 去重窗口，避免刷屏），在"止损错边""无止损单且无记录止损""止损重挂失败"三种止损侧无保护场景下发送 Telegram ALERT |
| 4 | 低 | `trader/auto_trader_vol.go:201`（`processVolTargetAndTrailing`） | `ProfitLockTargets` 调用把 `currentSL` 参数传成 `initialSL`；行为正确（利用 `initialDist<=0` 自然短路防止重复触发），但变量命名易被误读为传参错误 | **已修复**——调用点新增 `currentStop` 局部变量与详细注释说明自限性机制来源；`kernel/anchor_offset.go` 中 `ProfitLockTargets` 的函数级文档同步补充同一份说明，防止未来任何一处被孤立修改时破坏幂等假设 |
| 5 | 低 | `kernel/formatter.go:260-267, 527-533`（死代码路径） | `formatCurrentPositionsZH/EN` 硬编码 30%/-4%/-5% 提示阈值，不读取 `RiskControl` 配置 | **已随 #1 一并解决**——文件整体删除，问题不再存在 |
| 6 | 信息 | `auto_trader_orders.go` 开仓仓位缩放链路 | 仓位价值比例上限 → 风险反推缩放 → 可用余额缩放 → 保证金预算门，是"线性砍量 + 最后一票否决"而非"迭代收敛到预算内的最大可行仓位"；当前行为安全（宁可拒单），但产品需求若变化需重新设计 | 架构记录，非缺陷，未改动 |
| 7 | 信息 | `kernel/anchor_offset.go` `TpTierAction` / `auto_trader_vol.go` 1R 逻辑 | ROE 止盈阶梯与 1R 锁盈共享 `tpTrimDone`/`r1TrimDone` 状态位实现互斥，隐式耦合较强，建议在两个 map 定义处补充说明 | 架构记录，非缺陷，未改动 |

**总体结论**：三个重点审查方向中，**风控闸门与止盈止损的核心执行逻辑质量很高**——止损区间公式、跟踪止损、1R 锁盈、部分平仓上限、保护单看门狗与 TP-runner 的互斥，都有清晰的纯函数实现和对应单测，且"只收紧不放宽""tighten-only"这类关键不变量是 Go 硬校验而非仅靠 prompt 文案约束。**核心发现的 4 个问题（#1-#4）已于 2026-09-13 当天全部修复**，详见 [第 8 节](#8-修复记录2026-09-13)；#6/#7 属于架构层面的取舍记录，未作改动。

---

## 7. 值得保留的好设计

- **单位契约写进 prompt 正文，而不是只写在代码注释里**：`funding_rate` vs `funding_annualized_pct`、`price_change_60m_live_pct` vs `prev_hour_close_change_pct` 等易混淆字段，都在发给 LLM 的文本里显式声明"不要混用/不要重新换算"，把维护者才看得到的注释变成了 LLM 也能看到的运行时契约。
- **风控参数默认值单点收拢**：`kernel.EarlyCloseHours`、`kernel.MaxSpreadPct`、`kernel.TpTrimProfitPct`/`TpFullProfitPct`、`kernel.ProfitLockRMult` 等函数把"0/未配置=默认值，负数=禁用"的解析逻辑收拢到单一位置，prompt 渲染和执行闸门共享同一个函数得到的值，理论上不可能出现"文案说 A、代码执行 B"的漂移。
- **止损/止盈/仓位调整的关键不变量是硬 Go 校验，不是仅靠 prompt 文案约束**：`stopMoveTightens`（止损只收紧不放宽不穿价）、`validateOpenRisk`（止损区间/RR/双向合理性）、`executePartialCloseWithRecord`（累计部分平仓 ≤75%）等，即使 LLM 输出违反约束的值，执行层都会在下单前拒绝。
- **状态机重启自愈能力**：`ReconcilePendingEntries`（限价入场孤儿单清理/离线成交补挂保护单）、`processProtectionWatchdog`（从交易所挂单回填内存中丢失的止损止盈记录）都专门处理了"进程重启导致内存状态丢失"的场景，而不是假设进程永不重启。
- **纯函数 + 单测覆盖的实现风格**：`trailingDecision`、`TpTierAction`、`ProfitLockTargets`、`lossStreakVerdict`、`topOfBookSpreadPct` 等核心风控计算都以不依赖运行时状态的纯函数形式实现，配有对应 `*_test.go`，边界条件（ATR=0、equity=0、盘口异常、连亏边界）都在测试里显式覆盖，便于后续修改时快速验证不变量是否被破坏。
- **扫描器输出被明确框定为"证据"而非"结论"**：`scanner_hint` JSON 附带"程序化扫描,仅辅助证据,非交易结论"的措辞，避免了 LLM 把量化打分误当作可以不加判断直接执行的信号，这个设计取舍在多处 prompt 文本中反复强调，形成了一致的产品语言。

---

## 8. 修复记录（2026-09-13）

以下 4 项修复均已通过 `go build ./...`、`go vet ./...`、`go test ./...` 全量验证（含 kernel/trader 及全部交易所适配层子包），无编译错误、无 vet 警告、无测试回归。

### 8.1 修复 #2：回撤监控类型断言加固 + 协程级 recover 兜底

**文件**：`trader/auto_trader_risk.go`

- `checkPositionDrawdown` 内 `symbol`/`side`/`entryPrice`/`markPrice`/`quantity` 五个字段的类型断言全部从 `v := m["k"].(T)` 改为 `v, ok := m["k"].(T)`，任一字段缺失或类型不符时记录 `logger.Warnf` 并 `continue` 跳过该条持仓，不再影响其余持仓的正常监控。
- 新增 `safeCheckPositionDrawdown` 包装函数，在 `defer recover()` 中调用真正的 `checkPositionDrawdown`；`startDrawdownMonitor` 的定时循环改为调用这个包装函数。这是纵深防御的第二层——即使未来的修改重新引入了某处未加保护的断言或其它 panic，也只会丢失当前这一分钟的检查，不会杀死整个监控协程。

### 8.2 修复 #3：保护单看门狗新增无保护仓位告警

**文件**：`trader/auto_trader_risk.go`

新增 `alertUnprotectedPosition(symbol, side, reason string)` 辅助函数：复用已有的 `gateNotifyRecord` 去重机制（30 分钟窗口内同一 key 只推一次，避免每个决策周期重复告警刷屏），推送一条 Telegram `ALERT` 级消息。接入三个此前只记日志的止损侧无保护场景：

1. 记录的止损价格在错误的一侧（止损失效，等价于无保护）；
2. 交易所无止损挂单，且内存里也没有记录的止损价可回填；
3. 止损重挂调用本身失败（网络/交易所错误）。

止盈侧（TP）的对应场景未接入告警——止盈缺失不会导致仓位无限亏损，风险等级低于止损缺失，维持原有仅记日志的处理，避免告警噪音。

### 8.3 修复 #4：1R 保本止损参数自限性机制显式化

**文件**：`trader/auto_trader_vol.go`、`kernel/anchor_offset.go`

- 调用点（`auto_trader_vol.go`）引入具名局部变量 `currentStop := initialSL`，并附多行注释解释：`initialSL` 本身读的就是"当前记录止损"（`GetRecordedStopLoss` 返回的是实时值，不是开仓时的原始止损）；一旦保本止损触发过一次，下个周期两者自然相等，`ProfitLockTargets` 内部 `initialDist <= 0` 分支会短路返回 `(false, false)`，这正是该函数每周期重复调用却不会重复触发保本移动的机制来源。
- `kernel/anchor_offset.go` 中 `ProfitLockTargets` 的函数级文档同步补充了同一份解释，明确告诫"不要为了看起来更规范而把 `initialSL`/`currentSL` 拆成两个独立来源的值，那会破坏这里的幂等性"，防止未来任何一侧被孤立重构时悄悄引入重复触发的回归。

### 8.4 修复 #1：清理约 1000 行未接入主流程的死代码

**文件**：`kernel/schema.go`（部分）、`kernel/prompt_builder.go`（整体删除）、`kernel/prompt_builder_test.go`（整体删除）、`kernel/formatter.go`（整体删除）

删除前逐一确认了每个待删符号在生产代码（非测试）中的引用方，只删除全仓搜索确认零外部引用的部分：

- **整体删除** `kernel/prompt_builder.go`（`PromptBuilder` 结构体及其 `BuildSystemPrompt`/`BuildUserPrompt` 等方法）与配套的 `kernel/prompt_builder_test.go`（约 400 行测试，仅测试即将删除的死代码本身）。
- **整体删除** `kernel/formatter.go`（`FormatContextForAI`/`FormatContextDataOnly`/`formatContextData` 及全部 `format*ZH`/`format*EN`/`getOIInterpretation*` 函数，含本报告 5.4/表格 #5 提到的硬编码 30%/-4%/-5% 提示阈值）。
- **从 `kernel/schema.go` 中删除** `BilingualRuleDef` 类型、`TradingRules` 变量（含与实际风控默认值冲突的保证金/止盈/止损/仓位数字）、`CommonMistake` 类型与 `CommonMistakes` 变量。
- **明确保留** `schema.go` 中仍处于活路径的部分：`DataDictionary`/`BilingualFieldDef`（被 `GetSchemaPrompt` 使用）、`OIInterpretationType`/`OIInterpretation`（被 `getSchemaPromptZH/EN` 内部使用，此前误判为死代码专属，复核后确认是活代码的依赖）、`GetSchemaPrompt`/`getSchemaPromptZH/EN`/`formatFieldDefZH/EN`（`kernel/engine_prompt.go:27` 与已删除的 `formatter.go` 均引用过，删除 `formatter.go` 后此函数继续被 `engine_prompt.go` 使用，保留）。

删除后跑了一次全仓符号引用检查（`grep -rn` 逐一排查 `PromptBuilder`/`FormatContextForAI`/`TradingRules`/`CommonMistakes`/`BilingualRuleDef` 等标识符），确认除被删除文件自身外没有任何遗留引用，随后 `go build ./...` 一次性编译通过、`go test ./...` 全量测试通过，未发生因误删活代码导致的回归。

---

## 9. 七门深挖补充审查与修复记录（2026-09-13，第三轮）

针对 `auto_trader_risk.go` 七类风控闸门（验仓/止损窗口/点差/保证金/平仓/看门狗/1R 锁盈）的逐门专项复核。核心机制验证无误，但发现 6 项缺口并已全部修复（`go build ./...`、`go vet ./...`、`go test ./...` 全量通过）：

| # | 严重级别 | 问题 | 修复 |
|---|---|---|---|
| 1 | P0 | **账户级熔断 `AccountMaxDrawdownPct` 无任何执行代码**——配置有定义、prompt 向 AI 宣称"程序强制拦截一切新开仓"，但没有任何代码把 equity 与 initialBalance×(1−pct) 比较。与 09-13 审计 #2（保证金预算门）同类缺口 | `applyHardRiskGates` 新增 open_* 分支：回撤 ≥ 阈值时拦截全部新开仓 + Telegram 告警（`gateNotifyRecord` 去重）；平仓/SL/TP 不受影响 |
| 2 | P1 | 市价开仓路径只数 `len(positions)` 不计其他币的挂单槽位（限价路径已用 `nextSlotCount`）；挂单不预留保证金——N 张限价单各自单独通过保证金检查后同时成交可合计超预算 | 市价路径（open_long/open_short）改用 `nextSlotCount`；`marginBudgetBlocksOpen` 计入 `pendingMarginReserved`（其他币挂单 notional÷leverage，leverage 不可读时按 1x 保守估算） |
| 3 | P2 | **无 TP 可整体绕过 min-RR 门**：`validateOpenRisk` 的 RR 校验包在 `TakeProfit > 0` 条件里，省略 take_profit 的开仓跳过全部收益侧校验且交易所无 TP 保护单 | `validateOpenRisk` 新增 take_profit 必填检查（与 stop_loss 同级强制） |
| 4 | P2 | **挂单版 1R 锁盈是从未接线的死代码**：`maintainR1TrimOrder`/`cleanupR1TrimFor`/`r1LockPrice`/`symbolOfKey`/`r1OrderID`/`r1PriceCache` 及 `kernel.R1Price` 零调用者；其注释声称的"tick 级触价"优势实际不存在（生效的是 vol.go 市价轮询版） | 全部删除（选择删除而非接线：接线是较大的行为变更，删除可逆且消除心智负担；市价版 1R 逻辑不变） |
| 5 | P3 | `usedMarginOf` 静默跳过 mark/leverage 不可读的持仓（低估已用保证金）；点差门 `GetOrderBook` 失败静默放行零日志 | 两处均加日志（fail-open 语义保留，只消除静默性） |
| 6 | P3 | `positionFirstSeenTime`（min-hold/early-close 门的数据源）只在本地兜底分支写入——重启后已有 DB EntryTime 的持仓 map 为空，两道平仓门对其**永久 fail-open**（"age unknown — don't block"），而非仅重置计时 | `auto_trader_loop.go` 持仓快照处新增幂等回填：updateTime（DB EntryTime / 交易所 createdTime）> 0 且 map 无记录时回种 |

**顺带核实无虞**：三个后台维护流程（pending/vol/watchdog）串行执行无竞态；`moveStopExchange` 撤挂窗口由看门狗下周期自愈；`closeRejectBreakoutBlocks` 仅拦浮亏且 15m 结构未破（与 prompt 一致）；1R 锁盈重启后 `initialDist=0` 自然停用的降级路径正确。

---

*本报告基于 2026-09-13 dev 分支（含大量未提交改动）的代码快照，未涉及前端 React 代码、交易所适配层细节（bybit/okx/gate 等）及数据库 schema 层面的审查。第 8 节为第二轮修复（首次审查的 4 项核心发现），第 9 节为第三轮修复（七门专项深挖的 6 项缺口），均于同日完成。*
