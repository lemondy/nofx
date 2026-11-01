## A. 确认的 Bug

确认 **9 项问题，其中 2 项可导致超限下单**。以下触发场景均为离线构造，未连接交易所，也未调整风险参数。

### A1. 执行价格变化后，未重新检查组合风险｜P0

**位置：** [trader/auto_trader_risk.go:2172](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_risk.go:2172)、`trader/auto_trader_orders.go:221–354`、`trader/auto_trader_fill_risk.go:52–54`

**触发场景：**

权益 1,000 USDT，已有止损风险 185 USDT；新多单名义金额 1,000，快照价 100、SL=99。

- 组合闸预留新风险 10，总风险 195，放行。
- 执行器重新取价得到 101，数量变为 `1000/101`。
- 实际止损风险变为约 19.802，总风险 **204.802，超过账户上限 200**。
- 单笔风险仍低于 30，因此成交后单笔校验也放行。

**证据：**

```go
candidateRisk := at.candidateStopRiskUSD(d, entryPx, equity)
if capPct > 0 && existingPct+candidatePct+reservedPct > capPct {
    return true, ...
}
```

执行器随后重新取价、计算数量，但只检查单笔预算：

```go
quantity := actualPositionSize / marketData.CurrentPrice
// ...
if err := at.actualFillRisk(decision, fillPrice, quantity, equity); err != nil {
    return at.rejectMarketFill(...)
}
```

`actualFillRisk` 没有检查账户总风险、净方向风险或单币累计风险。

**修复建议：** 在最终价格、数量量化完成后，重新执行组合准入并更新预留；成交后根据实际成交量、成交价再次核对组合预算。当前仅市价转限价时重算预留，覆盖不了上述路径。

### A2. 网格新增订单绕过公共组合风险闸｜P0

**位置：** [trader/auto_trader_grid_orders.go:170](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_grid_orders.go:170)、`trader/auto_trader_grid.go:655–660`

**触发场景：**

启用网格策略且与其他策略共用账户：

- 权益 1,000，其他币已有止损风险 185。
- 网格新挂单名义金额 500，止损距离 5%，新增风险 25。
- 网格自身投资额度和账户保证金检查均通过，订单实际提交。
- 账户潜在止损风险达到 **210 > 200**。

模拟适配器复现中，`PlaceLimitOrder` 确实被调用。

**证据：**

```go
allowed, currentValue, maxValue := at.checkTotalPositionLimit(d.Symbol, orderValue)
// ...
if blocked, reason := at.marginBudgetBlocksOpen(...); blocked {
    return fmt.Errorf("grid %s", reason)
}
// ...
result, err := gridTrader.PlaceLimitOrder(req)
```

`checkTotalPositionLimit` 只检查网格目标币的名义金额；该路径没有调用 `accountRiskExposureBlocks`。公共硬闸主要识别 `open_*`，网格使用的是 `place_buy_limit/place_sell_limit`。

**修复建议：** 所有增加账户风险的入口共享同一个组合准入函数。网格投资额度检查继续保留，同时执行账户总风险、方向风险及账户熔断检查。

### A3. 连亏封禁起点算错，且滑动查询会使封禁提前消失｜P1

**位置：** [trader/auto_trader_lossstreak.go:52](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_lossstreak.go:52)、`trader/auto_trader_lossstreak.go:134–148`

**触发场景：**

某币依次在第 1 天 UTC 00:00、01:00、23:00 连亏。规则在 23:00 才触发，应封禁至第 2 天 23:00。

数据库按时间倒序返回 `[23:00, 01:00, 00:00]`，代码却取第三条，得到第 2 天 00:00 到期，**只封禁 1 小时**。此后旧亏损离开最近 24 小时查询范围，记录不足三条，又直接解除封禁。

**证据：**

```go
if maxLosses <= 0 || len(positions) < maxLosses {
    return 0, time.Time{}
}
// positions 为 newest-first
bannedUntil := time.UnixMilli(positions[maxLosses-1].ExitTime).
    Add(lossStreakBanFor)
```

```go
sinceMs := time.Now().Add(-lossStreakWindow).UnixMilli()
positions, err := at.store.Position().GetClosedPositionsSince(at.id, sinceMs)
```

**修复建议：** 按时间正序识别首次满足连亏条件的事件，持久化 `trigger_trade_id/banned_until`。触发后独立维持完整封禁期，不能依赖“当前窗口仍有三条亏损”。现有测试也固化了错误的倒序索引预期，需要一起纠正。

### A4. 归属查询失败被当成手动仓，已有持仓保护被跳过｜P1

**位置：** [store/ai_managed.go:112](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/store/ai_managed.go:112)、`trader/auto_trader.go:1059–1063`、`trader/auto_trader_risk.go:1745–1777`

**触发场景：**

数据库已有明确的 AI 持仓归属；记录 SL=95，当前价格跌至 94，需要看门狗补救退出。此时 `ai_managed_positions` 查询失败：

1. `Count` 错误被忽略，返回 `false`。
2. 看门狗将其当作非 AI 仓跳过。
3. 后面的止损穿越检查和紧急平仓均不执行。

离线故障注入复现中，紧急平仓调用次数为零。

**证据：**

```go
var n int64
s.db.Model(&AIManagedPosition{}).
    Where(...).
    Count(&n) // 未检查 Error
return n > 0
```

```go
if symbol == "" || side == "" || !at.isAIManaged(symbol, side) {
    continue
}
// 此后才检查 mark 是否穿越 sl
```

**修复建议：** 将归属结果区分为“已确认 AI / 已确认手动 / 查询未知”。保留已有持仓最后一次可信归属，查询失败时继续管理已确认的 AI 仓；未知状态应告警并阻止新增风险，不能静默变成手动仓。

### A5. 日熔断锁存读取失败时，可能重新开放风险｜P1

**位置：** [trader/auto_trader_risk.go:2525](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_risk.go:2525)、`trader/auto_trader_risk.go:2489–2507`

**触发场景：**

账户当天已经触发日熔断，数据库保存了锁存状态。新实例取得日初基准 1,000 后，读取锁存状态发生错误；当前权益已回升至 950。

代码把“查询失败”当作“未熔断”，再按当前亏损 5% 判断，返回允许开仓，破坏“触发后当天持续停开”的规则。

**证据：**

```go
haltedAt, found, err := at.store.RiskState().
    DayHalt(at.dailyBaselineKey(), day)
if err != nil || !found {
    return "", false
}
```

调用方无法区分“没有锁存记录”和“锁存状态不可读”。

**修复建议：** 传播读取错误；日基准已知但锁存状态未知时，拒绝新增风险，保留平仓与保护单维护。

### A6. 连亏记录查询失败时，执行闸直接放行｜P1

**位置：** [trader/auto_trader_lossstreak.go:130](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_lossstreak.go:130)、同文件 `89–98`

**触发场景：**

某币实际已满足连亏封禁，但 `trader_positions` 查询失败，而规则表、账户和行情查询成功。提示侧得到空封禁表，执行侧也返回“不封禁”，允许继续开仓。

全库不可用时可能被其他闸拦住，但**单表故障或局部查询失败**仍可触发此问题。

**证据：**

```go
positions, err := at.store.Position().GetClosedPositionsSince(at.id, sinceMs)
if err != nil {
    return false, ""
}
```

提示侧还明确记录了：

```go
logger.Infof("... query failed (fail-open): %v", err)
return nil
```

**修复建议：** 连亏状态不可验证时停止该开仓请求；返回明确的 `LOSS_HISTORY_UNAVAILABLE`，不要用空集合表达读取失败。

### A7. BTC/ETH 前缀判断误分类，执行前权益下降时仓位上限失效｜P1

**位置：** [trader/auto_trader_risk.go:538](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_risk.go:538)、同文件 `557–570`

**触发场景：**

ETHFIUSDT 的 1,000 USDT 提案在权益 1,000 时通过解析。AI 等待期间账户权益降至 950，尚未触发账户熔断。

执行器本应按山寨币 `1×权益` 压至 950，却把 ETHFI 当成 ETH，仍允许 1,000。解析层虽按精确符号分类，但它使用的是较早的权益，不能替代执行时的正确分类。

**证据：**

```go
func isBTCETH(symbol string) bool {
    symbol = strings.ToUpper(symbol)
    return strings.HasPrefix(symbol, "BTC") ||
        strings.HasPrefix(symbol, "ETH")
}
```

**修复建议：** 使用规范化后的完整符号匹配，与本仓库 `thinRiskMajor`、解析层保持一致。ETHFI、BTCDOM 等应走山寨币上限。

### A8. 退出行情缺失时，提前平仓闸反而阻止减险｜P1

**位置：** [trader/auto_trader_risk.go:1178](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_risk.go:1178)、同文件 `1211–1244`

**触发场景：**

持有多仓 1 小时，入场 100、当前 99、SL=90；AI 请求平仓，但行情数据为 `nil` 或缺少 1h 序列。

函数将缺失数据转换成“反向 K 线数量为零”，继而阻止退出，直到持仓时间达到门槛或满足其他例外。这与函数注明的“缺数据不困住持仓”行为不一致。

**证据：**

```go
if data == nil {
    return 0
}
```

```go
evidence := oneHAgainstCandles(data, side)
if evidence >= 2 {
    return false, ""
}
return true, ...
```

**修复建议：** 计数同时返回数据是否有效；数据未知时允许减仓和平仓，不能将“未知”解释成“没有反转证据”。

### A9. 看板忽略日熔断锁存，权益回升后显示未熔断｜P2

**位置：** [trader/risk_status.go:118](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/risk_status.go:118)

**触发场景：**

日初权益 1,000，盘中跌至触发线，随后回升到 950。执行器正确保留当天熔断，但看板显示 `DailyHalted=false`。

**证据：**

```go
out.DailyLossPct = (out.DayStartEquity-out.Equity) /
    out.DayStartEquity * 100
out.DailyHalted = out.DailyLossCapPct > 0 &&
    out.DailyLossPct >= out.DailyLossCapPct
```

这里没有读取内存或持久化的日熔断锁存。

**修复建议：** 使用只读的统一熔断状态查询，同时展示“当前亏损率”和“当天是否已锁存”；查询失败应显示未知状态。

## B. 疑似问题（需进一步验证）

| 问题与位置 | 已确认的实现及具体风险 | 尚需验证 |
|---|---|---|
| 真实账户身份与锁的范围：`trader/account_execution.go:23–45`、`store/exchange.go:201` | 锁和挂单共享按 `userID:exchange记录ID` 分组；同一 Binance 账户配置成两个 exchange 记录，或运行两个进程，会得到独立执行锁。两个实例可能同时通过预算检查。 | 当前部署是否只有一个执行进程；是否存在同一真实账户的重复配置、不同 API Key。需要以真实账户/子账户身份统一归组。 |
| 未登记的交易所开仓挂单：`trader/account_execution.go:80–160` | 当前预留来自内存 pending、持久化 pending 和网格 checkpoint，没有全量合并交易所侧的未知开仓挂单。手工或外部程序下出的限价单可能不计入预留。 | 实盘是否允许外部程序或手工挂开仓单；这些订单是否通过其他流程登记。 |
| “最大回撤”的业务口径：`trader/auto_trader_risk.go:2671–2675`、`trader/auto_trader.go:481` | 主交易器相对 `initialBalance` 判断，并非历史权益峰值。初始 1,000→峰值 1,500→当前 1,190，不会触发该闸。自动初始化的字段列表还缺少 Binance 的 `totalEquity`，通常取 wallet。 | “最大回撤”是否确实指初始资本跌幅；实盘是否手工设置 initial balance，启动时是否有浮盈亏、入出金。网格另有持久化峰值，二者语义不同。 |
| 持仓记账固定数量精度：`store/position.go:292–294、317–335` | 加减仓统一保留四位小数，并用固定 `0.0001` 判定清仓。若合法数量步长更细，会改错数量、均价，甚至提前关闭数据库持仓。 | 当前交易品种及适配器是否存在更细 stepSize；需要针对实际规则缓存验证。不能仅据此断言当前 Binance 品种已受影响。 |

## C. 可复用的成熟方案（替代自写轮子）

| 具体位置 | 建议、收益 | 成本与风险 |
|---|---|---|
| `trader/auto_trader_risk.go:915` 的 ATR；`kernel/market_regime.go:265、293、326` 的 ATR 分位、布林宽度、ADX | 用 TA-Lib 或 Go 指标实现建立独立数值基准，再决定是否替换重复实现。首要收益是统一 seed、预热期、闭合 K 线及索引口径。 | 直接替换会改变门控结果；先做固定 OHLC 样本对照。库不会代替本仓库的闭合时间、鲜度和异常数据检查。 |
| `store/position.go:292、317`；Binance tick/step 处理 | 仓库已有 `shopspring/decimal`，可用于成交记账和交易规则量化；也可采用 tick/step 整数单位。 | 需处理存量数据库精度和舍入方向。指标计算继续使用 float64 即可，无需全仓替换。 |
| `store/position_query.go:233` 的 Sharpe；`auto_trader_shadow.go` 的评估结果 | 用 empyrical/QuantStats 对标准账户收益序列做独立核验；用 vectorbt 或事件驱动回放做风控消融对照。 | 必须先构造包含浮盈亏、手续费、资金费及入出金调整的收益序列。不能直接把逐笔 USDT 盈亏当收益率输入。 |

**这里自写是合理的：**

- 挂单未知提交、撤单与成交竞态、分批成交保护、人工仓归属，均依赖本系统的交易语义；通用回测框架不能直接替代。
- `volatile_gate.go` 的非重叠滚动基线和 OI 时间容差属于明确策略规则，保留小型纯函数合理。
- 仓库已使用 `go-binance/v2`，不值得仅为更换 SDK 重写整个实盘适配层；应保留对 Algo 单、过滤器和成交终态的专门核验。

## D. 优化建议（按收益排序）

1. **统一账户准入和风险状态。**  
   从 `applyHardRiskGates`、市价执行、限价执行、网格执行中抽出账户级准入：使用最终数量、价格、权益、当前持仓及全部预留；返回明确的通过、拒绝、数据未知。A1/A2 表明，当前按执行路径分散检查已经产生实际覆盖缺口。参数数值可以保持现状。

2. **区分初始 R、当前权益回吐风险和市场方向暴露。**  
   `accountRiskExposureBlocks:2121–2127` 使用 `qty×|entry−SL|`。例如多仓 entry=100、mark=150、SL=101、qty=10，代码只计风险 10，但当前权益到止损可能回吐 490。初始 R 应继续保留，另计算从当前 mark 到 SL 的损失。无止损仓按 8% 估算也不是损失上界，尤其是空仓，应明确标为估算或未知风险。

   现有净方向风险在 `2194–2205` 只是多空**止损金额相减，没有 beta 调整**。而总风险和净方向上限同为 20% 时，后者在当前非负风险口径下不会比总风险闸更早生效。优先增加 BTC/山寨相关性分组及市场冲击场景，观察跨币空仓到底提供多少对冲。

3. **先补样本外与成本验证，再增加模型复杂度。**  
   `docs/volatile-coin-risk-control.md:58–66` 的依据是 60 天摘要和后续最大摆动，文档已说明未计交易 PnL、费用、滑点和机会成本。`auto_trader_shadow.go:197–225` 又采用简化成交及同柱止损优先假设。应按时间划分校准期/验证期，对 48h 标签清除重叠，并按日期或市场事件分块估计置信区间；多个阈值共同搜索时保留独立最终验证集。

4. **沿已有波动率减仓扩展，暂缓 Kelly 和完整风险平价。**  
   `auto_trader_vol.go:39–56` 已有单币 ATR 目标及滞后带，值得保留。下一步优先加入相关性聚类预算和组合波动监测。VaR/ES 可先作为影子指标，并与跳空、相关性骤升的压力情景对照；目前材料不足以证明可靠的尾部估计。Kelly 需要稳定优势估计，完整风险平价需要稳定协方差，当前不宜直接驱动实盘仓位。

5. **监测保护延迟与锁等待，减少非执行工作占锁。**  
   `runCycleWithDecision:27–29` 获取账户锁；AI 等待会释放，但影子评估、行情读取和执行仍有多段同步工作。记录账户锁等待时间、最长持锁时间、成交到止损确认延迟、未知订单年龄。把 `evaluateGateShadowBlocks` 等研究性工作移出执行临界区，避免历史评估排队拖慢 30 秒保护监控。

6. **让配置和看板展示“实际有效规则”。**  
   `auto_trader_configcheck.go` 已有加载哈希，但零值默认、负值关闭、指针开关及 R/ROE 阶梯覆盖关系较多。应展示解析后的有效配置及规则是否被其他阶梯覆盖，并使看板直接读取执行器的只读风险状态，避免 A9 这类双份计算漂移。

## E. 本次审查覆盖的文件清单与未覆盖部分

**核心覆盖：**

- `trader/auto_trader_risk.go`
- `trader/auto_trader_lossstreak.go`
- `trader/auto_trader_closegate.go`
- `trader/auto_trader_pending_risk.go`
- `trader/auto_trader_fill_risk.go`
- `trader/risk_status.go`
- `trader/auto_trader_rules.go`
- `trader/auto_trader_freshness.go`
- `trader/auto_trader_configcheck.go`
- `trader/auto_trader_shadow.go`
- `trader/account_execution.go`
- `kernel/volatile_gate.go`
- `kernel/market_regime.go`
- `market/oi_risk.go`
- `docs/volatile-coin-risk-control.md`

**相关调用链与存储重点覆盖：**

- `auto_trader_loop.go`、`auto_trader_orders.go`、`auto_trader_pending.go`、`auto_trader_protection.go`
- `auto_trader_vol.go`、`auto_trader_grid.go`、`auto_trader_grid_orders.go` 的相关执行路径
- `store/strategy.go` 的 risk_control 结构、默认值和有效值解析
- `store/risk_state.go`、`store/position.go` 的数量、初始止损、减仓及平仓记账
- `store/position_query.go`、`position_builder.go`、`ai_managed.go`、`exchange.go` 的相关查询与归属路径
- Binance 权益计算、保护单覆盖和 tick 处理；解析层的仓位限制

仓库根目录的 `protectposition` 实际是 **Mach-O 可执行文件，并非目录**；审阅了 `cmd/protectposition/main.go`，未执行该工具，也未核验二进制与源码是否一致。

**验证结果：**

- 选定的风险、连亏、方向风险、鲜度、OI、波动异常和 regime 测试通过。
- `go vet ./trader ./kernel ./market ./store` 通过。
- 使用 `/tmp` Go overlay，10 个边界断言复现上述 9 项问题，其中连亏问题包含两个场景。见[临时复现测试](/private/tmp/nofx-readonly-risk-l32qq9h2/audit_test.go)。
- 未运行全量测试或 race 检查；未读取实盘数据库、连接网络或核验真实订单。复现属于函数级及模拟适配器验证。

**工作区说明：** 本次没有执行仓库写入、提交或依赖整理命令，Go 命令均使用 `-mod=readonly`，缓存和临时测试位于 `/tmp`。结束检查发现 `go.mod` 的 direct/indirect 分组与开始时不同，来源未确认，未回退该改动。