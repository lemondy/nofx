# NoFx 代码审查报告（2026-09-13）

审查范围：`user_prompt` 数据计算与构建、程序强制风控闸门、止盈止损计算逻辑。方法：全文通读核心源文件（非抽样 grep），交叉核对 `*_test.go` 已钉死的不变量，并对每个可疑点回溯到实际调用链确认是否为"活代码路径"。

> 本报告只做发现与定性，不包含代码修改；严重级别标注供排期参考。
>
> **更新（2026-09-13，同日）**：第 6 节汇总表中的 4 个核心发现（#1-#4）已修复，详见各条目后的"状态"列与文末的[修复记录](#8-修复记录2026-09-13)。
>
> **更新（2026-09-20，第四轮全量复审）**：针对 09-13 之后累积的 52 个 commit 做了全量重审（重点：user prompt 数据获取/指标计算、仓位计算、风控闸门、止盈止损与开仓单计算、网页策略配置项生效链路），发现 5 项 P1 / 10 项 P2，详见[第 10 节](#10-第四轮全量复审2026-09-20)。**其中全部 P1（R4-1~R4-5）与全部 P2（R4-6~R4-15）已于同日修复**，见 [10.4 修复记录](#104-修复记录2026-09-20同日)；R4-16~R4-29（P3/信息）保持记录状态。

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
9. [七门深挖补充审查与修复记录（2026-09-13，第三轮）](#9-七门深挖补充审查与修复记录2026-09-13第三轮)
10. [第四轮全量复审（2026-09-20）](#10-第四轮全量复审2026-09-20)

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

## 10. 第四轮全量复审（2026-09-20）

**审查范围**：09-13 复审之后的 52 个 commit（`94a363d1`→`f01cb6f8`）+ 核心链路全量重审。**重点**（用户指定）：user prompt 数据获取、数据指标计算、仓位计算、风险控制、止盈止损/开仓单计算逻辑、风控闸门、网页策略配置项是否正确生效。**方法**：三路并行深查（A prompt 数据链路 / B 闸门与订单执行 / C 网页配置生效链路）+ 主审对全部 P1/P2 结论及两路矛盾处逐条亲读源码裁决。本轮**只审不改**。

### 10.0 裁决记录（两路深查结论矛盾、由主审亲读定案）

1. **`validateDecision` 的 RR 校验**：一路结论"配置 min_rr<3 时模型输出在 parse 阶段整批报错、3 次后误入 SAFE MODE"；另一路结论"恒真死校验"。亲读 `kernel/engine_position.go:260-285`：虚拟 entry 固定取 SL→TP 区间的 20% 分位，risk 恒为区间 20%、reward 恒为 80%，RR 恒等于 4.0 → `< 3.0` **永不触发**。死校验成立，"整批报错"不成立（见 R4-9）。
2. **4h 数据是否恒可用**：一路称"默认/激进预设缺 4h → 全候选 DATA_INSUFFICIENT"；另一路称"GetWithExchange 恒拉 3m/4h 不受预设影响"。亲读裁决：**kernel 决策路径走 `GetWithTimeframes`（`market/data.go:272-323`），只拉策略 `SelectedTimeframes` 列表内的周期、不强制补 4h**；恒拉 3m/4h/1h 的是执行路径的 `GetWithExchange`（`data.go:100-223`，供挂单/止损带/vol 门用）。预设缺 4h 的封锁结论**成立**，但仅影响 kernel 侧（见 R4-3）。

### 10.1 问题清单汇总（按严重级别）

| # | 级别 | 位置 | 问题 | 一句话影响 |
|---|---|---|---|---|
| R4-1 | **P1** | `kernel/engine_analysis.go:209-211` | `correctStopLossToPlan`（2a76d4bf 承诺的 post-parse 止损吸附）**无生产调用**，全仓唯一调用在 `stop_plan_test.go:234` | 模型自算止损≠plan 时豁免失效，噪声地板拒单循环（ZEC/AVAX 同型故障）仍在 |
| R4-2 | **P1** | `kernel/signal_layer.go:619-622` + `kernel/engine_data_binance.go:643` | `price_change_24h_live_pct` 携带小数而非百分数：`QuantData.PriceChange["24h"]` 约定是小数（0.0723=7.23%），signal 层直接透传未 ×100；隔壁 `price_change_60m_live_pct` 是真百分数（`rolling1hChangePct` 尾部 `×100`，signal_layer.go:2033），同块 JSON 单位差 100 倍 | 模型每周期把所有币的 24h 变动读成 ~0.0x%，24h 上下文整体失真 |
| R4-3 | **P1** | `kernel/signal_layer.go:876,895-897` + `store/strategy.go:547` + `api/handler_user.go:308` | DataQuality 硬性要求 15m/1h/4h 各 ≥60 bars（缺失→`sufficient=false`→`DATA_INSUFFICIENT` 双向 no-exception 阻断），但默认策略模板 `["5m","15m","1h"]` 与激进预设 `["3m","15m","1h"]` 均无 4h，`fetchMarketDataWithStrategy`（`engine_analysis.go:235-244`）只在列表为空时才补 `LongerTimeframe` | 这两类配置下**每个候选 4h bars=0 → 全部 DATA_INSUFFICIENT → 整轮 regime skip 合成 wait、零 LLM 调用**。当前实盘 Conservative 预设（`handler_user.go:291`，含 4h）不受影响 |
| R4-4 | **P1** | `trader/auto_trader_risk.go:1078-1105` vs `:307-322` | `executePartialCloseWithRecord`（决策周期 goroutine）无锁读写 `at.partialTrimmed`；回撤监控 goroutine 的 `ClearPeakPnLCache`（`:243`→`:320`）在 `tpTrimMutex` 下 delete 同一 map | 并发 map 读写 = Go runtime fatal（**整个进程崩溃**，非单协程）。触发窗口窄（partial_close 与回撤保护平仓同刻），但后果是级联的 |
| R4-5 | **P1** | `web/src/pages/StrategyStudioPage.tsx:414-432` + `api/strategy.go:258-272,330-355` + `trader/auto_trader_configcheck.go:57-58` | UI 保存 = 页面打开时的 GET 快照整体覆盖（前端显式携带全字段；后端 merge 语义救不了）；**且保存即 RemoveTrader+reload，`loadedConfigHash` 以回滚后的值重算 → 漂移告警恒通过** | 09-14 `sl_min_atr_mult 1.5→0` 事故的完整路径仍然在，且这次连 config-drift 告警都不响（告警只覆盖"DB 被改但进程没 reload"，恰好不覆盖"保存即回滚+reload"） |
| R4-6 | P2 | `market/data.go:393-396` + `kernel/engine_analysis.go:287-294` | OI 拉取失败被构造成非 nil 零值 `&OIData{0,0}`，下游 `data.OpenInterest != nil` 把"数据缺失"当"真实 OI=0"，日志打"OI value too low (0.00M)"剔除 | openInterestHist 单点故障 = 当周期全部非持仓候选被静默清空（与 ticker 全失败→VENDOR_DIVERGENCE_UNKNOWN 同族的基础设施单点） |
| R4-7 | P2 | `trader/auto_trader_vol.go:372-384`（写）+ `:280`、`auto_trader_risk.go:1220`（读） | `tpRunnerDoneMap` 全仓无任何 delete（`ClearPeakPnLCache` 清 tpTrimDone/r1TrimDone/partialTrimmed，独独漏它） | 同 symbol_side 平仓后重开的新仓**永远不会再转 trend-run 出场**（vol.go:280 守卫恒假）；出场管理退化但不丢保护（新开仓的固定 TP 仍在） |
| R4-8 | P2 | `trader/auto_trader_risk.go:213-221` | TP ladder trim 档：执行（`:208-212`）后无论成败置位 `tpTrimDone`，失败不回滚（full 档无 done 门会每周期重试，自愈；trim 档一次失败永久跳过） | 1/3 减仓利润获取缺失（非风险放大） |
| R4-9 | P2 | `kernel/engine_position.go:260-285` | RR 校验死代码：虚拟 entry 推导下 RR 恒 4.0，`<3.0` 永不触发；且硬编码 3.0 不读 `min_risk_reward_ratio` 配置。prompt（`engine_prompt.go:87`）宣称"程序双重校验,不可放宽"——kernel 层实际是 no-op，真实强制只有 rr_scan 门 + 执行端 `checkRR`（这两处正确且读配置） | 误导性死代码 + prompt 承诺与 kernel 现实不符（执行侧无资金风险） |
| R4-10 | P2 | `web/src/components/strategy/RiskControlEditor.tsx:137-162` + `kernel/anchor_offset.go:211-237` | `tp_trim_profit_pct` 表单可配但默认**永不生效**：`profit_lock_at_r` 缺省=0→解析为 1.0（1R 锁开启）→ `TpTierAction` trim 档恒让位（`:215-217`）；要启用 trim 必须把 `profit_lock_at_r` 设为负，而该字段**无表单、无 TS 类型**（`web/src/types/strategy.ts` 缺） | UI 调 trim 是无效配置且无提示（1R 锁压制 trim 是已记录的设计，问题在 UI 暴露了死配置项） |
| R4-11 | P2 | `web/src/components/strategy/RiskControlEditor.tsx:482` vs `kernel/anchor_offset.go:297-304` | `max_vendor_divergence_pct` 前端提示"默认 2"，后端 0→**1**% | 展示层与执行默认不符 |
| R4-12 | P2 | `kernel/engine_prompt.go:71,83,84` | Hard Constraints 三行渲染无 ≤0 fallback：`MaxPositions=0` 渲染"0 coins"（执行端回落 3，`auto_trader_risk.go:392`）、`MaxMarginUsage=0` 渲染"≤0%"（执行端 0.9，`:809`）、`MinPositionSize=0` 渲染"≥0 USDT"（执行端 12） | 字段为 0（JSON 导入/直改 DB）时 prompt 向模型宣告与执行矛盾的硬约束；同文件相邻行（:60-67、:93-96）都有 fallback，属遗漏 |
| R4-13 | P2 | `market/breakout/gainer_history.go:86-100` + `kernel/engine.go:947` | `SetShortScanHistoryConfig` 是包级全局单值，每个 trader 的 engine 每周期覆盖写入——多 trader 不同 `short_scan_history_days/max` 时 last-writer-wins | 单 trader（当前部署）闭环正常；多 trader 时配置串扰无检测 |
| R4-14 | P2 | `trader/auto_trader_risk.go:1252-1300` | 裸仓看门狗 `placeComputedProtection`（c2f65c7d 的核心新行为：从实时数据算 SL=mark∓1.5×ATR(1h)、TP=mark±2×dist 并补挂）**零测试覆盖**；`processProtectionWatchdog` 整体同样无测试 | 09-19 用户指令的直接实现无回归网 |
| R4-15 | P2 | `trader/binance/futures.go:239-248`（`calculatePrecision`）+ `auto_trader_risk.go:1273-1281` | tick 取整按 tickSize 的**小数位数**而非 tick 量化——对 10 的幂次 tick 等价（Binance 绝大多数），非 10 幂 tick（如 0.025）不保证是 tick 整数倍；另 `placeComputedProtection` 用 `math.Pow10` 自行取整、不走 `formatTriggerPrice`、无 drift guard、取整后未重验 SL 相对 mark 的边 | -1111 理论可复现的边缘 + 看门狗路径取整口径与挂单路径不一致 |
| R4-16 | P3 | `market/data.go:337-386` | `GetWithTimeframes` 的 `primaryKlines`（局部变量）未被 live price patch（`refreshFormingCandle` 只 patch `timeframeData` 副本）→ BTC 头行 EMA/MACD/RSI7（`engine_prompt.go:379-381`）用 vendor 冻结 forming close | 仅影响 BTC 概览行与遗留字段，各币 Structured Signal 自算不受影响（legend 已声明优先级） |
| R4-17 | P3 | `kernel/signal_layer.go:1088` vs `trader/auto_trader_risk.go:608-610` | 1h 数据缺失时两侧 floor 口径不一致：kernel 端 `STOP_PLAN_NO_STRUCTURE` 双向封锁（fail-closed），执行端本有 1h→2h→4h 降级链 | 方向保守（安全），但 1h best-effort 失败 = 该币当周期无条件禁交易，两侧不同尺 |
| R4-18 | P3 | `kernel/signal_layer.go:932-938` vs `kernel/anchor_offset.go:117-132` | prompt 端 breathing 阈值经 `primaryTFSignal`（15m 缺失回退最长 TF=4h），执行端 `ExecutionATRPct` 精确 15m（缺失→0→fixed）——`anchor_offset.go:121-123` 注释声称两侧同约定，实际 `ComputeSymbolSignals` 内部不走 `ExecutionATRPct` | GetWithExchange 恒聚合 15m 故窗口窄；触发时 prompt 端过度抑制（≈8× 宽），`LIMIT_ANCHOR_SUPPRESSED` 误报 |
| R4-19 | P3 | `trader/auto_trader_orders.go:188-189` vs `:362` | 市价开仓先 `SetRecordedStopLoss/SetInitialStopLoss` 落锚、后 `reanchorProtectivePrices` 按滑点平移才发交易所——滑点时内存记录≠交易所触发价（min-hold 硬退出旁路、看门狗补挂价均用内存值）；限价路径无此问题 | 滑点窗口内内存锚轻微失真 |
| R4-20 | P3 | `trader/auto_trader_pending.go:301-315` | 限价开仓路径缺市价路径的"可用保证金缩放"步骤（orders.go:132-146 有）——可用不足时限价单被交易所以保证金不足拒绝，非风控拦截 | 有 `marginBudgetBlocksOpen` 90% 线兜底，覆盖区间不同 |
| R4-21 | P3 | `trader/auto_trader_risk.go:1252-1300` + `vol.go:280` | `placeComputedProtection` 不检查 `tpRunnerDone`（理论窗口极小）；裸仓判定从实时 mark 计算（交易所止损刚触发、仓位残留瞬间可能误挂，下单被拒或立即触发）；操作员手工撤单想手动管理会被强制重挂 | c2f65c7d 有意行为（用户指令"保护裸仓"），记录边缘 |
| R4-22 | P3 | `trader/binance/futures_orders.go:242+` | `moveStopExchange` 的 `CancelStopLossOrders(symbol)` 按 symbol 撤**所有** STOP 单不筛方向——对冲模式（同币双向持仓）下会连带撤掉对侧止损，靠看门狗下轮补挂 | 当前单向持仓模式无影响 |
| R4-23 | P3 | `trader/auto_trader_vol.go:161,262-263` | trailing 的 arm 阈值用 live recorded stop 而非开仓 R 锚（6d241a25 只改了 1R 锚，trail 的 arm 仍随收紧收缩——breakeven 后 dist≈0 恒 armed） | 提前武装方向（更保护），与 1R 修复口径不一致且未注释声明 |
| R4-24 | P3 | `kernel/engine_analysis.go:139-178` | regime skip 不覆盖 `adjust_stop_loss`：全部持仓 close-locked + 全候选 blocked 时合成 wait，跳过了本可合法收紧的止损动作 | 错失收紧机会，不放大风险 |
| R4-25 | P3 | `trader/auto_trader_risk.go:1172`、`:187`、`:196-201` | ①看门狗首见冻结在"DB 行未落库+重启后止损已收紧"场景会把已收紧的止损冻成 R 锚（legacy 折衷，注释自认）；②`:187` 传 `&at.config.StrategyConfig.RiskControl` 无 nil 检查（panic 被 `safeCheckPositionDrawdown` 的 recover 吞掉→监控每分钟空转）；③TP full/1R dust 全平不清 peakPnLCache（靠下周期差集自愈） | 三处边缘，均有自愈或兜底 |
| R4-26 | P3 | `kernel/signal_layer.go:1571` + `market/data.go:548-550` | `if macdLine := ...; macdLine != 0` 用 0 兼作失败哨兵（MACD 线恰为 0 时字段静默消失，罕见）；`getFundingRate` 内"1-hour cache"注释陈旧（实际 TTL 5min） | 纯边缘/注释漂移 |
| R4-27 | P3 | `web/src/components/strategy/RiskControlEditor.tsx:117,151,181,53,366,513` + `CoinSourceEditor.tsx:749-757` + `api/strategy.go:300-322,249-250` | 前端杂项：①`pump_guard_4h_pct/tp_trim_profit_pct/tp_full_profit_pct` 是后端 float64 却用 `parseInt`（7.5→7）；②`|| 默认值` 使 UI 无法表达 0（`min_risk_reward_ratio` 后端 0=禁用 RR 门，UI 永远设不出）；③`short_scan_history_max` 输入框 min=1 回不到 0=默认 30；④token 超限检查在写库+reload **之后**才返回 400；⑤`is_public/config_visible` 非 pointer，非 UI 客户端 PUT 会把公开状态重置 false | 单独都小，合起来是"表单语义 ≠ 后端语义"的同一族问题 |
| R4-28 | 信息 | `web/src/types/strategy.ts:187-245,93-121` | TS 类型落后于后端：缺 `max_spread_pct`、`profit_lock_at_r`、`stock_weekend_no_open`、`limit_entry_offset_mode/_atr_mult/_min_pct/_max_pct`、`use_hyper_all/use_hyper_main/hyper_main_limit` | 运行时靠 JS spread round-trip 不丢值，但类型缺位 = 这些字段永远进不了表单（R4-10 同根） |
| R4-29 | 信息 | `kernel/engine_prompt.go:379-381` + `kernel/schema.go` | BTC 头行字段（3m 遗留序列指标）与各币 Structured Signal（TFSignal 自算）口径并列但来源不同，且受 R4-16 影响；schema.go 数据字典偏旧但不喂错误口径（legend 已声明 Structured Signal 优先） | 文案已缓解，记录在案 |

### 10.2 各主题详细核查结论

#### A. user prompt 数据获取与指标计算

**验证无误**（关键项，各一行）：

- `GetWithExchange` 恒拉 3m/4h（失败即整体失败，不静默降级）+ 1h best-effort（失败置 nil 记日志，floor 回退链吃 4h）+ 15m 由 3m 本地聚合（`market/data.go:970-1028`，5:1 对齐桶边界）——执行侧止损带/vol 门数据源成立（7c0aebd4/d1981253 修复在位）。
- live price：Binance ticker，xyz 资产保留 kline close；±80% 崩坏不 patch；vendor divergence 测量 `*float64`，nothing-to-patch 也产出 ~0 测量值，缺失时 `VENDOR_DIVERGENCE_UNKNOWN` fail-closed 拦双方向（09-19 审计六修复在位）。
- funding：5min 缓存、失败不缓存、成功须回显 symbol 才 Store（防 -1121 错误信封缓存假 0）、`FundingRateOK` 区分真 0（bstock）与失败、结算间隔实测年化（未知回退 8h×3）、rate=0 省略 annualized——全部在位且有测试。
- 指标：ATR Wilder 仅 closed bars、各周期口径一致；RSI 平盘=50、stochRSI 零头不入随机窗+hi==lo→50、MACD 注释明示是线非柱、classifyTrend 对称四象限（`signal_layer.go:1704-1730`）、finestSubHourTrend 取最细子小时 TF、OI USD delta=base change×latest price（测试钉死）、`funding_rate` 原始小数 vs `funding_annualized_pct` 百分数的 prompt 契约在位。
- stop_plan：结构位 step-out 不 clamp（过紧跳下一结构、超 cap break）、缓冲取宽端（空 0.5/多 0.4×ATR(1h)）、RR 门口径统一到 plan（gate/采纳/执行端 checkRR 同源）、`StopPlanTolerancePct=0.05` 与执行端豁容差一致。
- prompt 示例数字：仓位/风险预算示例全部 `fmt.Sprintf` 自实时 equity+配置 `risk_per_trade_pct`（`TestRiskBudgetSingleSource` 钉死 3.5% 流转）；止损带/限价偏移/TP 阶梯/点差/连亏等参数行全部从共享 resolve 函数取值，未见新的手写数字。
- 限价锚点：offset=ATRMult×ATR(1h) clamp 带宽、方向正确（买单阻力下方/卖单支撑上方）、抑制 fail-closed（`!LimitAllowed && 无市价例外证据`→`LIMIT_ANCHOR_SUPPRESSED`）、被抑制锚点上的限价开仓降级 wait 不落交易所。

**缺陷**：R4-2（24h 单位）、R4-3（预设缺 4h）、R4-6（OI 零值）、R4-9（RR 死校验）、R4-16/17/18/26。

#### B. 止盈止损/开仓单计算与执行

**验证无误**：

- `validateOpenRisk` 六子门：SL/TP 必填、边 sanity、双锚 RR（decision.Price 与 live 价各查一次）、cap=max(2×ATR(4h),8%)、floor=SLMinATRMult×ATR(1h)；plan-parity 豁免（stop==gated plan ±0.05% 免 floor、RR 仍复检）+ `cycleGateStates` 捕获时机修复（86523dfd：prompt build 之后、执行循环之前赋值，测试含 nil-map 回归）——在位。
- algo 触发价 tick 取整 + drift guard（f01cb6f8）：`formatTriggerPrice` 按 tickSize 精度渲染、roundtrip 偏移>1% 拒单，SetStopLoss/SetTakeProfit 均接入，UNIUSDT 型 -1111 修复成立。
- 限价寿命 `max(30min, N×scan interval)` 时间口径、锚点穿越市价回退、supply-zone 呼吸阈值用执行 TF 15m ATR（`ExecutionATRPct` 精确查找无跨周期回退，parity 测试钉死）。
- `adjust_stop_loss` 三闸：无仓跳过、tighten-only（currentSL≤0 时正确侧新止损=裸仓补挂放行）、保本闸（多 newSL≥开仓价/空镜像，entry 缺失时跳保本闸保留 tighten+WARN）。
- 裸仓看门狗计算保护（c2f65c7d）：ATR(1h) 来源/双侧 sanity/成功后 Set 锚+TP 记录使下游按计划处理——数学正确（测试缺口见 R4-14）。

**缺陷**：R4-1（吸附未接线）、R4-15（tick 量化边缘）、R4-19/20/21/22。

#### C. 仓位计算

**验证无误**（含一次口径澄清）：

- `clampSizeToRisk`：`maxNotional = equity × riskPct% / distPct%`——**不除以杠杆，且这是正确的**：`PositionSizeUSD` 全链按名义价值使用（qty=size/price、保证金=size/lev、止损时损失=notional×dist%），令损失=风险额反解即得该式。"÷杠杆"得出的是保证金口径，两者等价、代码无误。
- 链路顺序：价值比例上限（BTC/ETH 5x、山寨 1x 权益）→ 风险反推 → 可用保证金缩放（marginFactor=1.01/lev+0.001，超限缩至 98%）→ 最小下单量（读配置 `min_position_size`，与 kernel `EffectiveMinPositionSize` 同源）→ 保证金预算门（Σ已用+挂单预留+新单 ≤ budget×equity，lev 不可读按全额保守计）。
- `risk_per_trade_pct` 四个消费点（clampSizeToRisk / vol-target / prompt 公式与示例 / signal_layer 预计算）全部 `≤0→1.5` 同语义，单一解析无分叉。
- vol-target：target=equity×(riskPct/100)/(atrPct/100)，80/120 滞后带、只减不加、target<min size 整体平仓、1h 冷却——单位正确。

**缺陷**：R4-4（partialTrimmed 竞争）、R4-8（trim 失败不回滚）、R4-7（tpRunnerDoneMap 残留）。

#### D. 风控闸门

**验证无误**：

- extended-pump 做多守卫（794ad4e7）：4h ReturnPct ≥ `PumpGuard4h`（0=20/负=关，单一 resolver 三处共用）+ 确认需 15m LastClose>EMA20 且 swing low 抬高，缺 15m 在 extended 状态下 fail-closed；仅拦做多（做空侧对等惩罚在 short 扫描器 extended penalty，单向为设计）。
- regime-level LLM skip：全候选 HardBlocked（缺失 GateState 视为 blocked）+ 全持仓 close-locked（年龄未知 fail-open 保留调用）→ 合成 wait；渲染候选计数=实际渲染数；锁定持仓覆盖（587066fc）。
- 连亏熔断（24h 窗、从第 N 笔亏损平仓时刻起算）、账户级回撤熔断（拦 open 不拦 close）、股票周末禁开（nil/true=拦）、max_positions/nextSlotCount（含挂单占位）、点差门 fail-open+必留日志——语义与 09-13 报告一致，无回归。
- 1R 锚（6d241a25）：三路径落锚（市价/限价成交/离线成交）+ 看门狗首见冻结，write-once 双层（内存 if≤0 + DB `WHERE initial_stop_loss=0`），`ProfitLockTargets` 幂等性文档在位。
- decision_stage/wait_state 程序派生（456578f0/b4bf7552）：模型声明一律覆写、wait 剥离冗余字段、派生词表齐全。
- drawdown-protect 5/55 默认、Peak-PnL 单调更新、`trailingDecision` 只紧不松+0.1% 最小改善（纯函数+单测）。

**缺陷**：R4-14/21/22/23/24/25。

#### E. 网页策略配置项生效链路

**链路总评**：

- **保存→生效闭环完整**：PUT `/api/strategies/{id}` → DB 更新 → 立即 `RemoveTrader`+`LoadUserTradersFromStore`+`EnsureTraderStarted`（`api/strategy.go:330-355`）→ **下周期生效，无需重启**；运行中每周期不重读 DB，靠 `checkConfigDrift` 比对哈希告警（30min 去重，不阻断）。
- **prompt 渲染与执行闸门共享 resolve 函数**：EarlyCloseHours/MaxSpreadPct/TpTrim/TpFull/ProfitLock/PumpGuard4h/EffectiveMaxVendorDivergencePct/EffectiveMinPositionSize/ResolveShortScanHistory* 等确认单一解析（例外=R4-12 三行渲染遗漏）。
- **两个新字段闭环确认**：`risk_per_trade_pct`（f8daeff5）全链 ✅；`short_scan_history_days/max`（1f543406）单 trader 全链 ✅（prompt 图例走同一 resolve，无手写默认；残留=R4-13 全局串扰、R4-27③ max 回不到默认）。
- **无表单但被后端读取的字段**（靠 GET/PUT round-trip 存活，一旦页面快照过期再保存就会被回滚——R4-5 的作用面）：`sl_min_atr_mult`、`profit_lock_at_r`、`min_hold_minutes`、`max_spread_pct`、`stock_weekend_no_open`、`block_short_1d_uptrend`、`entry_timing_gate`、`limit_entry_enabled`、`limit_entry_max_cycles`、`limit_entry_offset_mode/_atr_mult/_min_pct/_max_pct`、`vol_target_enabled`、`trailing_stop_enabled`、`close_reject_breakout_pct`、`open_reject_supply_pct`、`loss_streak_ban_enabled/_max_losses`、`account_max_drawdown_pct`。
- **表单有但无效/误导的配置项**：`tp_trim_profit_pct`（R4-10，默认死配置）；`max_vendor_divergence_pct`（R4-11 提示值错）。

**缺陷**：R4-5（回滚+告警盲区）、R4-10/11/12/13、R4-27/28。

### 10.3 修复建议（按优先级排序，未实施）

1. **R4-1**：`engine_analysis.go:210` 在 `correctLimitAnchors` 旁补一行 `correctStopLossToPlan(decision.Decisions, ctx.GateStates, kernel.StopPlanTolerancePct)`——`ctx.GateStates` 已就绪，一行接线，测试现成。
2. **R4-2**：`signal_layer.go:621` 改 `v := pc * 100; d.PriceChange24hLivePct = &v`，补数值断言测试。
3. **R4-4**：`executePartialCloseWithRecord` 三处 map 访问套 `tpTrimMutex`（与 ClearPeakPnLCache 同锁）。
4. **R4-3**：预设/默认模板补 4h，或 `minBars` 按策略实配 TF 收缩（后者更治本：只对"配置里有的周期"提 60 bars 要求，4h 单独豁免为"有则须 ≥60"）。
5. **R4-5**：保存前重新 GET 比对（前端）或 `updated_at` 乐观锁（后端 API，`api/strategy.go` req 加版本字段，不匹配返回 409）；至少把 `checkConfigDrift` 的告警从"reload 后恒过"改为记录旧哈希迁移链。
6. P2 批次：R4-6（OI 失败显式 UNKNOWN 而非零值）、R4-7（ClearPeakPnLCache 补 delete tpRunnerDoneMap）、R4-8（trim 失败回滚 flag）、R4-9（删恒真块或改读配置+真实价）、R4-10（trim 输入旁标注"1R 锁开启时无效"或补 profit_lock_at_r 表单）、R4-12（三行补 fallback）、R4-14（看门狗补测试）。

### 10.4 修复记录（2026-09-20，同日）

10.1 表中 **R4-1 ~ R4-15（全部 P1 + 全部 P2）已全部修复**，`go build ./...`、`go vet ./...`、`go test ./...` 全绿（含 kernel/trader/market/api/binance 及全部交易所适配层），前端 `tsc --noEmit` 通过。R4-16 ~ R4-29（P3/信息级）保持记录状态未改动。

**P1 修复**：

| # | 修复内容 | 文件 |
|---|---|---|
| R4-1 | `correctStopLossToPlan` 接入 parse 成功路径（`correctLimitAnchors` 之后），模型回显止损偏离 plan >0.05% 时吸附到 gated plan——plan-parity 豁免的前提真正成立 | `kernel/engine_analysis.go` |
| R4-2 | `price_change_24h_live_pct` 在 signal 层 ingest 时 ×100（Quant 层小数 → _pct 百分数），字段注释同步；`TestPerCoinPromptIsJSONOnly` 增加数值断言（0.09 → 渲染 `9`）钉死单位 | `kernel/signal_layer.go`、`kernel/engine_prompt_test.go` |
| R4-3 | `SignalOptions` 新增 `ConfiguredTimeframes`，`computeCoinSignal` 从策略配置填充（含 fetch 路径的空列表 legacy 展开）；DataQuality 的 `minBars` 改为 {15m,1h} 恒需 + 实配周期各 ≥60，仅当调用方未携带配置（执行侧重算路径）才保留历史 4h 硬要求；n==0 时 shortfall 现在写明缺失周期名。新测试 `TestDataQualityRespectsConfiguredTimeframes` 钉死三态（无 4h 配置可交易 / 配置了但没拉到仍 fail-closed / legacy 行为不变） | `kernel/signal_layer.go`、`kernel/engine_prompt.go`、`kernel/signal_layer_test.go` |
| R4-4 | `executePartialCloseWithRecord` 的 `partialTrimmed` 三处访问全部套 `tpTrimMutex`（nil 初始化移入锁内）；字段声明注明锁约定 | `trader/auto_trader_risk.go`、`trader/auto_trader.go` |
| R4-5 | 后端 `handleUpdateStrategy` 请求体新增 `base_updated_at`（RFC3339Nano），与 DB 行 `UpdatedAt` 不一致返回 **409** + 中文提示 + 当前值（前端随后刷新）；前端保存携带 `base_updated_at`，409 时提示并自动重新 GET 刷新编辑器。空值 = legacy 客户端不设防。同批顺带修 R4-27d/e（见下） | `api/strategy.go`、`web/src/pages/StrategyStudioPage.tsx` |

**P2 修复**：

| # | 修复内容 | 文件 |
|---|---|---|
| R4-6 | `market.Data` 新增 `OpenInterestOK`；两个构造路径走 `fetchOIDataOrZero`（失败保留零值结构体防 nil-panic，但打 WARN 且 ok=false）；kernel 流动性过滤器对 OI-unknown 改为跳过检查 + 日志（候选宇宙在扫描层已有 min-OI 门槛，不再因一次接口故障静默清空全宇宙）；legacy prompt 的 OI 行加同守卫 | `market/types.go`、`market/data.go`、`kernel/engine_analysis.go`、`kernel/engine_prompt.go` |
| R4-7 | `ClearPeakPnLCache` 补 `delete(tpRunnerDoneMap, posKey)`（volResizeMu 下）——重开仓不再继承 "runner done"；字段声明注明锁与生命周期约定。新测试 `TestClearPeakPnLCacheClearsTPRunner` | `trader/auto_trader_risk.go`、`trader/auto_trader.go`、`trader/protection_test.go` |
| R4-8 | TP 阶梯 trim 档与 full 档的 `tpTrimDone/r1TrimDone` 均改为**成功后才置位**——失败下周期重试（full 档原本就无 done 门、行为对齐；trim 档不再永久跳过） | `trader/auto_trader_risk.go` |
| R4-9 | 删除 `validateDecision` 中恒真的 RR 校验块（虚拟 entry 使 RR≡4.0），留注释指明真实双门 = rr_scan + 执行端 checkRR（均读配置）；消除硬编码 3.0 漂移炸弹 | `kernel/engine_position.go` |
| R4-10 | 策略页新增 `profit_lock_at_r` 表单（step 0.1，含"当前生效"行）；`tp_trim_profit_pct` 旁在 1R 锁开启时显示 ⚠️ 提示（"ROE 减仓档不生效"）；两字段 desc 同步补充说明；TS 类型补 `profit_lock_at_r` | `web/src/components/strategy/RiskControlEditor.tsx`、`web/src/types/strategy.ts`、`web/src/i18n/strategy-translations.ts` |
| R4-11 | vendor divergence 前端"当前生效"提示与 TS 注释：默认 2 → **1**（与 `EffectiveMaxVendorDivergencePct` 一致） | `web/src/components/strategy/RiskControlEditor.tsx`、`web/src/types/strategy.ts` |
| R4-12 | Hard Constraints 三行补 ≤0 fallback（MaxPositions→3、MaxMarginUsage→90%、MinPositionSize→12），与执行端 `enforceMaxPositions`/预算门/`EffectiveMinPositionSize` 同值 | `kernel/engine_prompt.go` |
| R4-13 | `SetShortScanHistoryConfig` 值变化时打 WARN（10 分钟去重）——UI 合法改值提示一次，多 trader 异配置 ping-pong 每 10 分钟暴露一次；注释声明进程级全局语义 | `market/breakout/gainer_history.go` |
| R4-14 | 裸仓看门狗计算核心提纯为 `computedProtectionLevels(side, mark, atrAbs)` 纯函数（方向/正数/双侧 sanity 全覆盖）；新测试 `TestComputedProtectionLevels` 钉死多空数学与 1:2 RR 比例及全部拒绝分支 | `trader/auto_trader_risk.go`、`trader/protection_test.go` |
| R4-15 | binance 新增 `symbolTickSize`（原始 tickSize 字符串）与 `formatAlgoTriggerPrice`：先按 **tick 网格量化**（`quantizeToTick`，非 10 幂 tick 如 0.025 也能得到 tick 整数倍），tickSize 不可用时回退原 `formatTriggerPrice` 小数位格式；>1% drift guard 两路均保留。新测试 `TestQuantizeToTick` 四用例 | `trader/binance/futures.go`、`futures_positions.go`、`futures_orders.go`、`futures_trigger_test.go` |
| R4-27d/e | token 超限检查移到 DB 写入**之前**（超限返回 400 时配置不再已生效）；`is_public/config_visible` 改 `*bool`——省略即保留现值，非 UI 客户端 PUT 不再把公开状态重置 false | `api/strategy.go` |
| R4-27②③ | `min_risk_reward_ratio` 输入允许 0（0=禁用 RR 门，`|| 3` 改显式 NaN 判断 + 0 值提示行）；`short_scan_history_max` 输入 min 1→0（可回到 0=默认 30） | `web/src/components/strategy/RiskControlEditor.tsx`、`CoinSourceEditor.tsx` |
| R4-27① | `pump_guard_4h_pct/tp_trim_profit_pct/tp_full_profit_pct/profit_lock_at_r` 全部 `parseInt` → `parseFloat`（后端 float64，7.5 不再截成 7） | `web/src/components/strategy/RiskControlEditor.tsx` |

**有意不修**（记录在案）：R4-17/18（kernel/执行端 ATR 口径差异，两侧均 fail-closed/保守方向，统一属行为变更需拍板）；R4-19~R4-26、R4-28/29（P3 边缘与信息级，均有自愈或文案缓解）。

**部署提示**：本批修复涉及 prompt 渲染（R4-2/12）、决策解析（R4-1/9）、执行闸门（R4-4/7/8）、UI 保存（R4-5），**需重启进程生效**；R4-2 修复后模型看到的 24h 涨跌幅恢复真实量级（此前 100× 偏小）。

---

*本报告基于 2026-09-13 dev 分支（含大量未提交改动）的代码快照，未涉及前端 React 代码、交易所适配层细节（bybit/okx/gate 等）及数据库 schema 层面的审查。第 8 节为第二轮修复（首次审查的 4 项核心发现），第 9 节为第三轮修复（七门专项深挖的 6 项缺口），均于同日完成。第 10 节为第四轮全量复审（2026-09-20，dev 分支 HEAD=`f01cb6f8`，工作区干净），覆盖前端策略配置链路与 09-14 之后全部新增风控逻辑；其全部 P1/P2 已于同日修复（10.4 节），P3 及以下保持记录。*
