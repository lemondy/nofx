# 加密 AI 决策周期的账户执行锁收窄（2026-10-30）

## 问题

同一币安账户下的所有 trader（实盘 mac-nofx、us-stock，以及网格）共用 `executionMutex()`
（`trader/account_execution.go`）。PR #46 之后 us_stock 保护 tick 改为排队等锁，生产日志
在约 21 小时里出现 38 次 `us_stock protection tick waited ...`（平均 73s，最长 108s），
全部落在加密周期的整点/5 分钟时刻。

注意：加密周期的 **LLM 调用本身早已在锁外**（`runCycleWithDecision` 里
`withoutExecutionLock(mu, decide)`，d7b5c200 起），返回后 `refreshExecutionAccount`
在锁内重读账户。真正长时间持锁的是 `buildTradingContext` 里的**纯行情取数**。
2026-10-30 21:00 周期的 mac-nofx 日志：

| 阶段 | 时间 | 耗时 |
|---|---|---|
| 持锁开始（周期日志） | 21:00:00 | |
| 候选币 `GetCandidateCoins`（AI500 + breakout/short scan） | → 21:00:25 | ~25s |
| `FetchQuantDataBatch` | → 21:00:43 | ~18s |
| `FetchOIRankingData` | → 21:01:17 | ~34s |
| `FetchNetFlowRankingData` | → 21:01:17 | ~0s |
| `FetchPriceRankingData` | → 21:02:02 | ~45s |
| 释放锁进入 LLM | 21:02:02 | |
| us_stock 保护 tick 拿到锁 | 21:02:08（等待 88.7s） | |

即每个周期约 2 分钟的持锁时间是行情 I/O（受 Binance 权重限流的低优先级请求排队），
与账户状态无关。同一时间 mac-nofx 自己的 30s 保护巡检和回撤监控也被同一把锁挡住。

## 方案（沿用 PR #45 us_stock 的模式）

把 `buildTradingContext` 拆成两段：

1. `buildAccountContext()` —— **锁内**。余额、持仓、交易所保护单（GetOpenOrders）、
   `positionFirstSeenTime`/峰值/止损记录清理、AI 托管标记、规则/近期成交/统计/连亏封禁
   等 DB 读。这些会读写 trader 的执行状态或决定账户快照，保持在锁内，行为不变。
2. `enrichMarketContext(ctx)` —— **锁外**。候选币、量化数据、OI/NetFlow/价格排行。
   只写入 `ctx` 的行情字段，不读写执行状态。`buildTradingContext()` 保留为两段的组合
   （现有测试/调用者不变）。

`runCycleWithDecision` 的新顺序：

```
lock
  stopped? → return
  pending 对账 / vol-target+trailing / 保护 watchdog / exit 元数据 / 孤儿行
  auth 封禁 / 配置漂移 / 风控暂停 / 日 P&L 重置
  ctx = buildAccountContext()
  equity 快照 / 日基线锚定 / 日亏锁存          ← 用锁内账户快照，不变
  promptPositions = copy(ctx.Positions)       ← AI 将看到的持仓
unlock
  enrichMarketContext(ctx)                    ← 新增的锁外窗口（~2 分钟）
lock
  stopped? → return                            ← 新增：Stop() 可能在窗口内已执行
  空候选且无持仓无挂单 → 跳过（pending 键为重新加锁后的实时值）
  candidateMeta
unlock
  decide(ctx)                                  ← 原有的锁外窗口
lock
  stopped? → return
  refreshExecutionAccount(ctx)                 ← 原有：锁内重读余额/持仓/保护单
  ... safe mode / 空候选过滤 ...
  STATE_CHANGED 过滤（新增，见下）
  规则检查 / 硬风控闸门 / 逐条执行 / 预留结算 / 记录
unlock
```

两个锁外窗口之间的重新加锁只做内存操作（空候选判断、candidateMeta），很短。
`withoutExecutionLock` 在 panic 时也会重新加锁，与外层 `defer mu.Unlock()` 配对不变。

## STATE_CHANGED（新增）

行情窗口加长后，AI 看到的持仓（`promptPositions`，取自 `buildAccountContext`）与执行时
`refreshExecutionAccount` 读到的实时持仓之间的间隔变长，期间保护巡检/回撤监控/止损成交/
用户手动操作都可能改变持仓。这个风险今天在 LLM 窗口里已经存在，现在一并补上：

按 (symbol, side) 比较数量，容差与 us_stock 相同（相对 1e-6，绝对下限 1e-9）。

| 动作 | 拒绝条件 |
|---|---|
| `open_long(_limit)` / `open_short(_limit)` | 同向数量与 AI 看到的不同（含新出现/消失）。新开仓：AI 没看到持仓但现在有了（挂单成交、手动开仓）→ 拒绝，避免叠仓；加仓：底仓已被减/平 → 拒绝 |
| `partial_close_long/short` | 对应方向 AI 看到的数量为 0、实时为 0，或数量变化 |
| `adjust_stop_loss` | 该 symbol 任一方向的数量变化（含出现/消失） |
| `close_long` / `close_short` | **不拦**：意图是平掉该方向，执行器按实时数量平；纯降风险，拦下反而留风险 |
| `hold` / `wait` | 不拦 |

被拒的决策：从执行列表移除，`setFilterReason(d, "STATE_CHANGED: ...")` 写入 CoT 摘要原因，
`record.ExecutionLog` 记一行 `rejected: STATE_CHANGED`，日志 Warn。位置在每周期
`at.filterReasons` 重置之后、规则检查和硬风控闸门（风险预留）之前，所以被拒的开仓
不会占用本周期风险预留。

## 不变的安全不变量

- pending 入场对账、vol-target/trailing、保护 watchdog、孤儿行清理仍在周期开头锁内执行。
- `refreshExecutionAccount` 仍是执行前唯一的账户/保护单事实来源；失败即放弃本周期执行。
- `Stop()`：设置 `isRunning=false` 后抢锁取消挂单/暂停网格。周期每次重新加锁后都先检查
  `isRunning`，Stop 之后不会再下单。
- 保护监控（`auto_trader_risk.go` 的 `safeProtectionPass`/`safeCheckPositionDrawdown`、
  `auto_trader_stream.go` 的实时保护）和网格周期的锁用法不改。
- us_stock 周期/保护 tick 不改。
- 日亏锁存、equity 快照仍用锁内账户快照，与今天一致。

## 不在本次范围

- `classifyAndEnrichExits` 的 MAE/MFE 回放（1m K 线，日志约 5s）仍在锁内；需要时可另行挪出。
- 周期末尾的 DB 写/Telegram 推送仍在锁内（毫秒到秒级）。
- 网格周期 `RunGridCycle` 的取数阶段。
