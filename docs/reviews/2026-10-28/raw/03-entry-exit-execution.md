只读审查完成，仓库无改动，未访问网络。确认的问题主要集中在：减仓后 TP 数量、平仓幂等、交易所止损单调性，以及舍入后的风险复核。

## A. 确认的 Bug

### 1. 部分减仓后 TP 不缩量，会提前平掉整个 runner | P0

**位置：** [trader/auto_trader_orders.go:759](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_orders.go:759)、[trader/auto_trader_vol.go:505](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_vol.go:505)；`trader/binance/futures_orders.go:349–368、1385–1395`。

**触发场景：** trend 仓位 10，`tp_close_fraction=0.5`，入场时挂出数量 5 的 TP。随后 1R 锁利先减仓 5，交易所 TP 仍为 5；价格到达 TP 后，剩余仓位全部平掉。提示词明确规定平“触发时剩余仓位”的 50%，此时应平 2.5、保留 2.5。

**证据：**

```go
// 入场时固定 TP 数量
tpQty := quantity * tpFractionForMode(mode, at.effectiveTPCloseFraction())

// 1R 减仓没有同步修改 TP
if _, err := at.reducePosition(symbol, side, qty*0.5); err == nil {
```

Binance 的显式部分平仓路径保留原保护单，未重新计算 TP 数量。产品契约见 `kernel/engine_prompt.go:406`。

**修复建议：** 每次已确认减仓后，根据实际余仓重新计算、量化并核验 TP；SL 使用 `closePosition=true` 可以继续覆盖余仓，数量型 TP 必须单独同步。

### 2. 平仓提交超时没有 UNKNOWN 状态，重试会重复减仓 | P0

**位置：** [trader/binance/futures_orders.go:335](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/binance/futures_orders.go:335)、[trader/auto_trader_vol.go:505](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_vol.go:505)；空头对应 `futures_orders.go:411`。

**触发场景：** 1R 减仓请求实际成交 5，但响应丢失。`CloseLong` 返回错误，`r1TrimDone` 保持 false。账户流更新余仓为 5；下一次快速检查再次减半，提交 2.5。最终减掉初始仓位的 75%，而非计划的 50%。

**证据：**

```go
// 每次调用生成新 ID，错误后直接返回
NewClientOrderID(getBrOrderID()).
Do(context.Background())

// 仅在返回成功时消耗一次性减仓标记
if _, err := at.reducePosition(symbol, side, qty*0.5); err == nil {
    at.r1TrimDone[posKey] = true
```

入场已有 `submitEntryOrder` 的 UNKNOWN 查回机制，平仓未复用对应机制。

**修复建议：** 为减仓持久化操作 ID、目标减仓量及 client order ID；响应未知时先查原订单、核对累计成交，确认后再决定补足数量。

### 3. 波动率减仓后继续使用旧快照，可能同轮全部平仓 | P0

**位置：** [trader/auto_trader_vol.go:351](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_vol.go:351)、[trader/auto_trader_vol.go:363](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_vol.go:363)。

**触发场景：** 当前数量 100，波动率目标对应数量 50；同轮首次达到 profit lock，且止损已保本。波动率分支先卖 50，随后 R 规则仍从原 `pos` 读取 100，再卖 50，实际全部平仓。按当前余仓减半应只再卖 25。

**证据：**

```go
reduceQty := qty - target/markPrice
if _, err := at.reducePosition(symbol, side, reduceQty); err == nil {
    at.markVolResize(posKey)
}
// pos 的 positionAmt 未更新
if at.processRProfitRules(pos, markPrice) {
```

`processRProfitRules` 在 `:389` 重新读取这个旧快照，在 `:505` 提交 `qty*0.5`。

**修复建议：** 同轮发生减仓后，先取得确认成交量并更新快照，再执行后续规则；也可规定每个仓位每轮只执行一个数量变更动作。

### 4. “只紧不松”只检查内存，仍会撤掉交易所更紧的止损 | P0

**位置：** [trader/auto_trader_vol.go:69](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_vol.go:69)、[trader/binance/futures_orders.go:1292](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/binance/futures_orders.go:1292)。

**触发场景：** 多仓内存 SL=90，用户已在交易所把该仓位全平止损收紧到 110，行情为 120，看门狗尚未同步。程序计算新 SL=102，因高于内存 90 而允许。Binance 返回已有保护单的 `-4130` 后，适配器撤掉 110、重新挂 102，实际放松了止损。

**证据：**

```go
currentSL := at.GetRecordedStopLoss(symbol, side)
// 单调性只比较 currentSL
old, err := at.trader.GetOpenOrders(symbol)
...
at.trader.SetStopLoss(symbol, strings.ToUpper(side), qty, newSL)
```

适配器随后：

```go
for _, old := range originals {
    if cerr := t.CancelProtectiveOrder(symbol, old); cerr != nil {
        return ...
    }
}
// 按新价格重新创建
```

`old` 已经读到，但调用前未据此阻止向不利方向移动；`originals` 也未限定为程序订单。

**修复建议：** 用交易所当前有效、足量覆盖的止损作为单调性约束，并同步内存。更紧的手工止损应保留；替换时明确订单归属。

### 5. 风险复核使用未量化 SL，实际下单后可超预算 | P0

**位置：** [trader/auto_trader_fill_risk.go:23](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_fill_risk.go:23)、[trader/binance/futures.go:312](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/binance/futures.go:312)；`trader/auto_trader_risk.go:960–966`。

**触发场景：**

- 权益 1,000 USDT，单笔预算 1.5%，即 15 USDT。
- 多单入场 100，计划 SL=99.74，往返成本 12 bps。
- 风险 sizing 后数量向下舍入为 39.473。
- tickSize=0.1，SL 被舍入到 99.7。

计划风险为约 15 USDT，实际风险变成：

```text
39.473 × (100 − 99.7 + 0.12) = 16.57866 USDT
```

超预算约 **10.5%**，但实际成交风险检查仍使用 99.74。舍入差约 0.0401%，又小于保护核验的 0.1% 价格容差，因此会被认定覆盖成功。

**证据：**

```go
risk, reward := fill-d.StopLoss, d.TakeProfit-fill
...
if quantity*(risk+cost) > budget*(1+fillRiskBudgetTolerance)+1e-8 {
```

```go
q := math.Round(price/tickSize) * tickSize
```

**修复建议：** 下入场单前先生成最终 tick/step 合法订单计划，再计算风险、净 RR 和数量；成交后按交易所实际保护价格复核。结构止损需要向外舍入时，相应减小数量。

### 6. TP 舍入尾差被当作缺失保护，补挂可能变成全仓止盈 | P0

**位置：** [trader/auto_trader_orders.go:850](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_orders.go:850)、[trader/binance/futures_orders.go:1385](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/binance/futures_orders.go:1385)；覆盖核验在 `auto_trader_orders.go:935`。

**触发场景：** 持仓 3、TP 比例 50%、stepSize/minQty 均为 1。计划 TP=1.5，适配器正确向下舍入为 1；上层仍要求覆盖 1.5，因此报保护不足。重试补 0.5 时，数量格式化失败，适配器改发 `closePosition=true`，部分止盈变成全仓止盈。

**证据：**

```go
missing := quantity - protectiveCoverage(orders, side, kind, price)
return t.SetTakeProfit(symbol, side, missing, price)
```

```go
qtyStr, qerr := t.FormatQuantity(symbol, quantity)
if qerr == nil && qty > 0 {
    algo = algo.Quantity(qtyStr)
} else {
    algo = algo.ClosePosition(true)
}
```

该 fallback 还覆盖了所有 `FormatQuantity` 错误，并非仅数量下溢。

**修复建议：** 覆盖核验使用已量化目标量。尾差应按明确的余仓策略处理；规则读取失败、MAX_QTY 等错误必须返回错误，不能改变平仓范围。

### 7. split TP 挂出即标记 runner，之后丢单不会补挂 | P1

**位置：** [trader/auto_trader_orders.go:794](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_orders.go:794)、[trader/auto_trader_risk.go:1798](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_risk.go:1798)。

**触发场景：** split TP 成功挂出，尚未成交，之后被撤销或失效。看门狗发现没有 TP，但因 `tpRunnerDone=true` 跳过补挂，原定的分批止盈消失。

**证据：**

```go
if tpErr == nil && decision.TakeProfit > 0 {
    if tpQty < quantity {
        at.markTPRunner(...)
    }
}
```

```go
wantTP := !at.tpRunnerDone(key) && tp > 0 && ...
```

**修复建议：** 区分“TP 已挂出”“TP 已成交”“余仓由追踪止损接管”。根据订单成交回执推进状态，并持久化，不能用一个内存布尔值代表三种状态。

### 8. 自动全平收到成功响应即清状态，部分成交残仓丢失快速保护与原始 R | P1

**位置：** [trader/auto_trader_stream.go:421](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_stream.go:421)、[trader/auto_trader_risk.go:452](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_risk.go:452)。

**触发场景：** 回撤保护提交全平 10，订单仅成交 4 后终止，或返回提交成功时尚未全部成交。`closeProfitSnapshot` 清除止损及初始 R 状态；快速循环据此删除仓位缓存。剩余 6 暂停快速管理。

若下一次看门狗从交易所恢复已经移动到保本的 SL，还会在 `auto_trader_risk.go:1765` 将其写成内存初始止损，后续 R 计算可能使用错误锚点。

**证据：**

```go
if _, err := at.reducePosition(symbol, side, qty); err != nil {
    return err
}
at.ClearRecordedStopLoss(symbol, side)
at.ClearInitialStopLoss(symbol, side)
at.ClearExitMode(symbol, side)
```

快速循环在 `auto_trader_stream.go:394–395` 因 recorded SL 为零而删除缓存。现存交易所 SL 可能仍有效，因此这里不等于立即失去交易所保护。

**修复建议：** 自动全平路径也采用 `executeCloseLong/ShortWithRecord` 已有的“新鲜快照确认归零”契约；确认前保留状态和快速管理，并跟踪未完成平仓。

### 9. 同一组 K 线有两种 Wilder ATR，计划与执行尺度不一致 | P2

**位置：** [kernel/signal_layer.go:2897](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/kernel/signal_layer.go:2897)、[trader/auto_trader_risk.go:936](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/auto_trader_risk.go:936)。

**触发场景：** 60 根已收盘 K 线，收盘均为 100；首根 high/low=120/80，其余为 101/99。源码抽取复算得到：

```text
信号层 wilderATR：          2.089775974
ATR 序列 / 执行层初始化：   2.000000000
```

**证据：**

```go
// 标量 ATR 包含 tr[0]
a := tr[0]
```

```go
// 序列和执行层使用 tr[1:15]
for i := 1; i <= 14; i++ {
    a += tr[i]
}
a /= 14
```

这会影响结构止损缓冲、ATR floor、追踪距离，以及用另一套 ATR 序列计算的百分位。

**修复建议：** 统一初始化、预热长度和已收盘过滤，信号与执行共用实现，并用固定参考序列校验。

### 10. 历史重建把零 PnL 的 hedge 平仓当作开仓 | P2

**位置：** [trader/position_rebuild.go:69](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/position_rebuild.go:69)。

**触发场景：** `BUY LONG 2@100`，随后 `SELL LONG 1@100`、`RealizedPnL=0`。后者被加入开仓列表，未生成平仓记录，并污染后续 FIFO 匹配。

**证据：**

```go
if trade.RealizedPnL == 0 {
    state.OpenTrades = append(state.OpenTrades, ...)
    state.TotalQty += trade.Quantity
}
```

**修复建议：** Hedge 模式按 `PositionSide + Side` 判定开平，已有 `OrderAction` 时优先使用。当前 Binance OrderSync 的 `determineOrderAction` 已采用正确判定。

### 11. FIFO 部分平仓不扣减剩余开仓费，费用重复分摊 | P2

**位置：** [trader/position_rebuild.go:142](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/trader/position_rebuild.go:142)。

**触发场景：** 开仓数量 2、开仓费 2；分两次各平 1。第一次分摊费用 1，但剩余开仓记录仍保存 Fee=2、Quantity=1；第二次分摊 2。总计分摊 3，而实际仅支付 2。

**证据：**

```go
totalEntryFee += ot.Fee * (matchQty / ot.Quantity)
...
ot.Quantity -= matchQty
// ot.Fee 未扣减
```

**修复建议：** 先计算本次分摊额，同时扣减 `ot.Quantity` 与 `ot.Fee`。

第 10、11 项已通过抽取源码复算确认；全仓搜索未发现 `RebuildPositionsFromTrades` 的生产调用，因此未将其认定为当前 Binance 实盘记账故障。

## B. 疑似问题（需进一步验证）

| 疑点 | 代码依据 | 还需验证 |
|---|---|---|
| 入场成功后、计划落库前崩溃，可能丢失原始保护计划；孤儿撤单也未查最终成交 | `auto_trader_pending.go:405–435` 在 POST 成功后保存 pending；`auto_trader_reconcile.go:205–218` 孤儿撤单后未查成交残量 | 故障注入：POST 已受理、shadow row 未写入即退出；重启后让孤儿单部分成交，再检查归属、SL 和原始 R 恢复 |
| 追踪止损用 MARK 判断合法，但默认 SL 用 CONTRACT 触发 | `auto_trader_stream.go:385` 使用 mark；`binance/futures_orders.go:1220–1238` 对多数标的选择 CONTRACT_PRICE | 构造多单 `MARK=100.2、CONTRACT=99.9、新SL=100.1`，核对拒单、原保护保留和重试行为；同时验证 MARK_PRICE 条件单 |
| 市价最小名义金额与真实规则脱节 | `binance/futures_positions.go:223–225` 固定返回 10；限价路径却读取 `rules.MinNotional`，数量规则也仅读取 LOT_SIZE | 核对当前交易标的的 MIN_NOTIONAL、MARKET_LOT_SIZE；若不同，市价路径会出现本地校验通过、交易所拒单。此次未联网核验实际规则 |

## C. 可复用的成熟方案（替代自写轮子）

以下为离线选型建议，未核验候选库的当前版本和维护状态。

| 本仓库位置 | 建议 | 收益、成本与风险 |
|---|---|---|
| `futures_positions.go`、`futures.go` 的价格/数量计算 | 使用项目已依赖的 `shopspring/decimal`，或整数 tick/step 表示最终订单计划 | 降低浮点与格式化边界风险；改动成本中等。库不会替代舍入方向、风险复核及尾差策略 |
| `signal_layer.go` 与 `auto_trader_risk.go` 重复 ATR | 先统一实现，以 TA-Lib 固定样本作基准；指标继续扩展时可评估 `go-talib` | 当前直接收益是消除口径差异。替换时必须固定 seed、warm-up、缺数与闭合 K 线语义 |
| 入场/出场研究 | 继续使用已有 `scripts/backtest/backtrader_adapter.py` | 仓库已经采用 Backtrader，无需再写撮合器。现有适配器 `:74–100` 会按余仓重挂出场单，可用于发现实盘 TP 不缩量的偏差；仍需补 Binance 条件单、部分成交与触发价模型 |
| 入场、平仓和保护单提交 | 复用已有 go-binance SDK 查询、撤单封装，统一 UNKNOWN 查回机制 | 协议层复用成本低于另建适配器；业务操作日志、幂等状态仍需本系统维护 |

**这里自写是合理的：** `methodStopPlan` 的结构失效位、LLM 菜单约束、方向门，以及账户级保护状态机属于本系统业务语义。成熟指标库或 SDK 无法直接替代；应减少重复数学和协议封装，保留这些业务规则。

## D. 优化建议（按收益排序）

1. **统一仓位变更后的保护目标。**  
   将 `processRProfitRules`、vol resize、AI partial close、TP 成交都汇入同一处理：确认实际余仓 → 重算 TP → 核验 SL → 持久化状态。优先解决 A1、A3、A6、A7，避免各路径分别修改布尔标记和数量。

2. **统一提交成功、成交成功、仓位归零三个状态。**  
   入场已有 UNKNOWN 处理，AI 全平已有 fresh-flat 检查，但自动减仓和紧急平仓未遵守相同契约。沿用现有内存 HTTP transport 测试设施，重点加入“成交但响应丢失”“部分成交后终止”“更紧交易所 SL”“TP 舍入尾差”四组故障用例。

3. **先验证回测与实盘执行一致，再调出场参数。**  
   已有 Backtrader 适配器会在减仓后按余仓重挂 TP，实盘不会；两者当前交易行为不同。`docs/architecture/s1-exit-replay-2026-10-07/README.md` 也已有 145 笔回放和成对 bootstrap 的记录，不能说本仓库完全没有统计验证。应先对齐实际事件序列，再评估 ATR、chandelier、分批 TP、时间止损的增量收益。

4. **入场比较使用同一机会集合与可执行时钟。**  
   `scripts/backtest/entry.py:48–64` 的收益均值排除了未成交限价，市价却覆盖全部信号；报告应同时给出每个原始机会的净收益、成交率和成交后的逆向选择。`limit_valid_min=45` 还需对齐实盘 `limitEntryLifetime=max(30min,N×ScanInterval)`，并纳入 AI 延迟和 crossed-anchor fallback。样本外按日期滚动，多个出场变体共同筛选时增加多重比较控制。

5. **让成本与保护延迟可测量。**  
   `actualFillRisk` 使用统一 bps 成本，研究默认使用 `TAKER=0.0005、SLIP=0.0003`；应按市价/限价、实际 maker/taker、持仓资金费和波动环境校准。至少记录成交到 SL 核验的延迟、UNKNOWN 持续时间、实际 TP 与余仓目标差、舍入后风险超额。`SetLeverage` 的固定 5 秒等待发生在执行链路中，也应评估其对同账户保护任务的阻塞。

## E. 本次审查覆盖的文件清单与未覆盖部分

重点文件均已阅读相关执行路径；大文件按相关函数审阅，不等同逐行审计：

- **Kernel：** `kernel/anchor_offset.go`、`kernel/engine_position.go`、`kernel/signal_layer.go`、`kernel/volatile_gate.go`。重点覆盖 stop plan、TP/RR、ATR、闭合 K 线和锚点规则。
- **Trader：** `auto_trader_orders.go`、`auto_trader_pending.go`、`auto_trader_pending_risk.go`、`auto_trader_protection.go`、`tp_cap.go`、`auto_trader_vol.go`、`auto_trader_fill_risk.go`、`auto_trader_stream.go`、`auto_trader_fast.go`、`auto_trader_exitmeta.go`、`auto_trader_orphan.go`、`auto_trader_reconcile.go`、`position_rebuild.go`。
- **Binance：** `trader/binance/futures_orders.go`、`order_sync.go`、`account_v3.go`。
- **关联核查：** `auto_trader_risk.go`、`auto_trader_loop.go`、`auto_trader_decision.go`、`auto_trader.go`、`binance/futures_positions.go`、`binance/futures.go`、规则缓存、策略校验及相关测试；另抽查 Backtrader 入场/出场适配和既有回放统计脚本。

**验证情况：** 整包测试被工具链版本阻止：本机 Go 1.24.3，仓库要求 ≥1.25.3。未下载工具链；在 `/tmp` 抽取源码纯函数完成了舍入、ATR、FIFO 费用和零 PnL 重建复算，这不替代整包测试或交易所故障注入。

**未覆盖：** React、其他交易所适配器、美股专用执行链路、研究系统全量审计；未访问实盘账户、生产配置、成交日志及当前交易所规则，未进行网络、压力或竞态测试。