# 全局深度审查（第五轮）· 2026-10-06

> ## ⚠️ 勘误（同日独立复核，以 [CODE_REVIEW_2026-10-06_RECHECK.md](CODE_REVIEW_2026-10-06_RECHECK.md) 为准）
>
> 本报告经独立单线复核后，以下结论被推翻或修正（原文保留为历史记录，不原地改写）：
>
> 1. **P0-1 成立但影响面收窄**：触发条件是 **AI 管理的负数量非零空头仓**（典型 Binance），不是"所有适配器的任何空头"；故障阻断新增风险+可撤存量入场挂单+跳过受影响空头的保护恢复/紧急退出——已有保护单不失效、平仓入口不封锁，"启动即自锁"仅适用于已有此类持仓时。镜像用例（long +1 / short −1 / short +1）已复现。
> 2. **P2×11 拆解**：6 条成立（1 C2 残留、2 BTC 双层分歧、5 窗口不对称、6 缓存静默窗、7 调用放大、11 panic 缺口）；**5 条被推翻或降级**——P2-3（我漏读了 `requestLimits` 120/min/IP 桶 + `scanComputeMu.TryLock` 串行 + 2min 缓存三层防护，且扫描集固定 `TopVolumeSymbols(30)`，"limit×~6" 描述错误）、P2-4（QUANT_FIXES §2 已写明 20bps 与新公式，"未标注"不成立）、P2-8（72 = 100/lev×0.9×0.8，出处在我漏读的 validateOpenRisk :681-688 注释）、P2-9（工作区事项非代码缺陷）、P2-10（reset-password 实为 410 禁用，无恢复流程）。
> 3. **P2-5 我把方向写反了**：比较是 `coin_live < btc_closed → 拦`，BTC 刚上涨时是**误放**中间强度币、刚回落时**误拦**——复核有数值复现。
> 4. **§4.1 的"并发合流"撤回**：`binanceBTC4hCloses` 锁内判断后解锁再发 HTTP（:731-748），无 singleflight/inflight——那是 agent 报告未经验证的结论，我未核实就收录。
> 5. **F01–F16 裁决改写**："15 项无条件合格"应为主要修复成立；F04 明确失败（符号回归），F05/F08 的负数量空头路径依赖同一修复，只能条件裁决。
> 6. **生产事实修正**：`go version -m` 显示 vcs.revision=4cf16c40 且 **vcs.modified=true**——应表述为"以 4cf16c40 为版本元数据的脏工作树构建"，不能等同纯净 4cf16c40；方法名探测支持 F04/F09 未进入该二进制。
> 7. **数据口径修正**：磨顶 45/0 缺统计窗口，复核按窗口给出 10-05 17:07→10-06 19:54 = **191 skipped / 0 入选**（当日 130/0），且是日志事件计数非独立交易机会；"13:25 skipped 66"应为 10-05 17:08:14 skipped 76（piggy floor 6/24）、10-06 13:25:16 skipped 69（8/24）。
> 8. panic recover 的引入提交实为 `6edb7f454`（10-04 06:51），代码注释写"10-03 review"系审查日期与提交日期混淆；"工程质量高于以往任何一轮"无评价指标，仅为主观判断。
>
> 可重跑证据见 [review-2026-10-06/evidence/](review-2026-10-06/evidence/README.md)。

- **基线**：`dev` @ `abba5978`（F01–F16 修复批次，10-06 19:26 提交，提交信息 "update code"）
- **生产进程**：PID 67272，二进制构建于 **10-05 17:07** —— **不含 `abba5978`**（F01–F16 修复写入于 10-05 20:30–21:43）。生产当前 = `4cf16c40`（含 10-04 两批审查修复），即本报告 §3 的 P0 在生产上**尚未生效**，采纳修复后重启才会带入（也会带入本 P0，必须先修）。
- **方法**：7 个领域并行审查（kernel 信号计算 / kernel 引擎与提示词契约 / trader 执行风控 / market 扫描回测 / 交易所适配器 / store-api-security 基建 / web 前端），全部发现经人工逐条读码核实后收录。受并发配额限制，适配器、基建、web、kernel-diff、trader-diff 由主线人工完成并已在文中标注。
- **与前轮的关系**：10-04 两批（`11ee538c`/`6817bf8d`，见 `docs/_review-attachment/README.md` 对照表）与 10-05 QUANT_REVIEW（F01–F16，`docs/architecture/QUANT_REVIEW_2026-10-05.md`）的发现不重复收录；本轮验证其修复质量（§5）并收录其未覆盖的新问题。

## 0. 结论摘要

| 级别 | 数量 | 一句话 |
| --- | --- | --- |
| P0 | 1 | F04/F05 重写的保护看门狗对空头仓 `qty <= 0` 判错（Binance 空头 positionAmt 为负），任何 AI 空头仓会让账户级保护故障常驻 → 停止一切新增风险。修复批次引入，测试未覆盖。 |
| P1 | 0 | 两项初判经影响面复核降入 P2（无直接资金损失路径）。 |
| P2 | 11 | + 扫描器 panic 恢复三缺口（scanning 不复位/worker 裸奔/无告警）。其余同前：C2 残留、BTC 门双层分歧、/breakout/scan 无认证、sizing 标注、闭合-实时不对称、BTC 缓存静默窗、GetOpenOrders 放大、72 魔数、stash 遗留、auth 护栏。P3 简录 4 项见 §3.1。 |
| 修复裁决 | F01–F16 | 15 项修复合格（§4 逐项 + §4.1 验证记录），1 项（F04）引入上述 P0。 |

整体判断：**修复方向全对，工程质量高于以往任何一轮**（fail-closed 状态机、结构位保留、成本入净 RR、调参降级为研究提议都是正确设计），但 `qty <= 0` 这一个符号错误恰好落在「空头仓位 + 保护状态机」的交点上，是典型的"多仓思维写保护逻辑"回归；扫描器 panic 恢复的三个缺口（P2-11）则是 10-03"防进程死亡"修复只做了一半的残段。**修一行 + 一个空头镜像测试 + `scanning` 复位，即可部署 `abba5978`。**

---

## 1. P0 —— 必须先修再部署 `abba5978`

### P0-1 · 保护看门狗对空头仓立起常驻保护故障 → 账户级自锁

**位置**：`trader/auto_trader_risk.go:1600-1604`（processProtectionWatchdog 重写，F04/F05 修复引入）

```go
qty, _ := pos["positionAmt"].(float64)
...
if mark <= 0 || qty <= 0 || math.IsNaN(mark) || math.IsInf(mark, 0) || math.IsNaN(qty) || math.IsInf(qty, 0) {
    at.setProtectionFault(key, "live mark or quantity unavailable")
    continue
}
```

**机制**（三段证据链）：
1. Binance 适配器**原样保留 positionAmt 符号**：`trader/binance/futures_positions.go:63` 直接 `strconv.ParseFloat(pos.PositionAmt, 64)`，空头为负；同包 `futures_orders.go:199` 的老代码自证：`quantity = -pos["positionAmt"] // Short position quantity is negative, take absolute value`。
2. 同文件老代码 `auto_trader_risk.go:190`（drawdown monitor）明确处理了符号：`if quantity < 0 { quantity = -quantity } // Short position quantity is negative, convert to positive` —— 新看门狗漏了同款处理。
3. fault 的消费方全部是硬阻断：`protectionFaultReason()`（`auto_trader_protection.go:89`，遍历**所有账户 peers**）→ `entryExecutionBlocked`（`auto_trader_pending_risk.go:198`）、`pendingAccountHaltReason`（:18，连带撤存量挂单）、`applyHardRiskGates`（`auto_trader_risk.go:2248`）。

**影响**：只要存在任何一个 `isAIManaged` 的空头仓位，watchdog 每 30 秒立起 `SYMBOL_short` fault → **该账户所有策略停止新增开仓 + 存量入场挂单被撤**，且该仓的保护核验/补挂也被 `continue` 跳过（缺保护不补）。多仓不受影响——所以 mac-nofx（当前仅 CRWVUSDT 多仓）无症状，但空军一号（做空策略）启动即自锁。`clearProtectionFaults(live)` 救不了：clear 在前、逐仓 set 在后，每轮复立。

**修复**：`qty = math.Abs(qty)`（与 :190 老代码一致），并补一个空头镜像测试（构造 positionAmt<0 的持仓快照，断言不立 fault、保护核验照常执行）。live map 构建处（:1588，`qty != 0`）符号已正确，无需改。

---

## 2. P1

> 最终定稿时 P1 两项经复核并入 P2 清单（P2-3 `/breakout/scan` 无认证、P2-4 sizing 成本变更标注）——两项均无直接资金损失路径，维持清单完整性见 §3。P0 不变。

---

## 3. P2

> 原 P1-1（sizing 成本变更标注）与 P1-2（/breakout/scan 无认证）经影响面复核并入本清单为 P2-3/P2-4：两者均无直接资金损失路径。

1. **C2 修复的两处残留：bb_ride 与 pump_guard 仍无条件丢弃末根**（`kernel/bb_ride.go:61`、`kernel/signal_layer.go:526-528`）。10-04 C2 批次把 `computeTFSignal`/`computeBreakoutState`/`suppressAnchorsAgainstStructure` 统一到时间判定式 `ClosedKlines`，但这两处仍是 `bars := tf.Klines[:len-1]`——数据滞后时（最后一根其实已收盘）丢一根真收盘棒。bb_ride 影响更实质：`bbRideWalk` 多报一根 `ride=true`，而 `BBRide.Ride` 是 `marketExceptionEvidence` 输入（**为市价开仓放行**）；pump_guard 的陈旧 pivot 可能翻转 `HigherLow`，且其注释"same convention as the anchor suppression"所指的约定本批已改，失实证明显属遗漏而非有意。修复：两处改 `ClosedKlines(tf, now, dur)`，与同批其余路径一致。
2. **BTC 门在 kernel fail-open、在执行端 fail-closed，同一 nil 两种裁决**（`kernel/signal_layer.go:1846-1850/1660-1664` vs `trader/auto_trader_pending_risk.go:159-161`）。BTC 数据缺失（`btc4hClosesCache` 30s 失败节流期间确定性为 nil）时：kernel 的 `BTC_4H_STRONGBULL`/`BTC_4H_DOWNTREND`/`BTC_WEAK_LONG` 静默放行；执行端 pending 重算对同一 nil 硬拦（"pending BTC regime data unavailable"）。多头侧 fail-open 是文档化的设计；**空头侧 STRONGBULL 是 opt-in 硬停门**，BTC 拉取故障期间市价空单可经 kernel 放行（执行端只在 pending 路径拦，marketExceptionGate 路径依赖门码）。建议：门在 BTC 数据 unknown 时对空头侧发独立码（如 `BTC_REGIME_UNKNOWN`）或至少统一两层语义，并在日志可见。
3. **`/api/breakout/scan` 公开无认证，可被外部触发重量级扫描**（原 P1-2，降级归并）：公开组可循环触发 `limit×~6` 请求的实时扫描，烧共享出口 IP 的 fapi 权重（429 → 周期扫描降级有先例）。移入 protected 或加节流。（保留 P1 表中的编号引用，统一在此收录。）
4. **sizing 分母新增往返成本：行为变更，部署清单与回滚口径未标注**（原 P1-1，保留 P1 编号）：三处实现一致（prompt/validateOpenRisk/actualFillRisk），方向正确；上线当天与历史 sizing/RR 不可比，需部署标注 + 确认 config_versions diff 展示覆盖该字段。
5. **BTC_WEAK_LONG 的闭合-实时不对称**（`kernel/signal_layer.go:1638` vs `:1668`）：BTC 侧 ret24 止于最新已收盘 4h 棒（0-4h 滞后），币侧是实时滚动 24h ticker。BTC 刚拉一波时，币的实时 24h 落在 BTC 闭合值与实时值之间的会被误拦（BTC_WEAK_LONG 在 absoluteBanCode 是绝对禁开）；刚砸盘时弱币误放。上界 4h 且随结算临近收敛——F02 的残余半步，建议 BTC 侧也用实时滚动窗口（与 C5 同族）。
6. **`btc4hClosesCache` 失败节流的静默窗口**（`kernel/engine_data_binance.go:733-737`）：首次失败后 30s 内所有调用返回 nil 且零拉取尝试；与 #2 复合后门层表现为"BTC 过滤没拦"而无线索。建议节流期日志一行 + 与 #2 的 unknown 码一并处理。
7. **敞口行每仓一次 GetOpenOrders**（`trader/account_execution.go:210-222`）：多持仓时每周期 N×GetOpenOrders（缓存失效后是真实调用），5+ 仓接近每周期数十请求；建议合并为一次全 symbol 查询再本地分组。
8. **`actualFillRisk` 强平安全距离魔数 72**（`trader/auto_trader_fill_risk.go:31`）：`72/leverage` 是"维持保证金率+缓冲"近似，无出处注释、不分币种分档。方向保守低危；注释出处或改配置。
9. **stash@{0} 未完成工作**（api/server.go SystemPromptTemplate 透传 + market 监控改动）：标着 "wip before aligning to origin/dev"，对齐远端时易丢；其中模板名更新是功能需求，恢复完成或显式丢弃。
10. **api 路由保护缺护栏测试**：本轮抽查全部敏感端点在 protected 组内（公开组仅行情/只读），但无"非白名单路由必须挂 auth"的机制性测试，未来新增端点可静默漏挂。另 `/api/reset-password` 公开实现（验证码/一次性令牌/频控）本轮未深核，建议专项。
11. **扫描器 panic 恢复的三个缺口**（`market/breakout/scheduler.go:273-280`、`market/breakout/breakout.go:342-348`，10-03 引入，非本批）：① `runOnce` 的 recover 只记日志、**不复位 `scanning`**——recover 后 5min ticker 每轮 `if s.scanning { continue }` 永跳、`RefreshNow` 自旋到 deadline，piggy/short 双榜冻结到重启，kernel 的 staleness 门随后 fail-close 全部扫描源候选；② recover 不覆盖 `AnalyzeMany`/`ScanShorts` 的 **worker goroutine**（不同 goroutine 不经过 runOnce 的 defer）——注释宣称防"AnalyzeMany panic 杀进程"，实际 worker 内 panic 仍然杀进程；③ 每次恢复后应打 Telegram 告警而非仅日志。修复：defer 内 `s.scanning = false` + onDone 兜底，worker 闭包内加 per-goroutine recover。

### 3.1 P3（简录，不阻塞）

1. `breakout.go:979` `volSlotMultiple` 读 `k[n-2]`：F10 丢弃 forming 棒后量能确认整体滞后一根 15m/1h——但**顺带消除了 live-vs-replay 的既有分歧**（回测一直读 n-2）；方便时改 `k[n-1]`+`idx := n-1-d*step`。
2. `backtest.go:107 vs :168` 回放注释声称"α/β/confirm 已在 TF 分内"，实际 `sh := &shared{}` 下 α≡1、β≡1、OI/Funding 维重归一剔除——回放分与实盘分尺度系统性偏移，阈值标定有偏；今天被 `LIVE PUBLICATION BLOCKED` 拦住，但注释会误导下一个解除发布锁的人。
3. `shorttuner.go:189-231` `shortTunerMu` 跨最多 50 样本×2 次串行 HTTP（各 15s 超时）持有——网络停滞时 journaling/`SetShortTuningPath` 阻塞数分钟；锁内收集、锁外拉取、回锁提交即可。
4. `gainer_history.go:149-150` 历史池保留硬编码 9 天，而 `ResolveShortScanHistoryDays` 接受任意正数——配 30 天的策略静默只得 9 天，建议校验旋钮或对齐保留期。

---

## 4. F01–F16 修复批次逐项裁决（`abba5978`）

| 项 | 裁决 | 依据（人工读码核实） |
| --- | --- | --- |
| F01 滑点方向反 | ✅ 修复合格 | `adverseSlippageBps` 多空镜像修正（orders.go:376-390）；长/短两条执行路径对称 |
| F02 BTC 门用 1h 数据 | ✅ 修复合格 | `BtcTrendCloses: binanceBTC4hCloses(300)`；BTC4hTrendCloses 供执行端同源；30s 失败节流见 P2-6 |
| F03 禁开码未覆盖市价 | ✅ 修复合格 | `entryExecutionBlocked` 挂入市价/限价两条执行路径；absoluteBanCode 语义经由 GateStates 穿线（本轮抽查 entryExecutionBlocked 的 failed 匹配逻辑一致） |
| F04 看门狗足额保护 | ❌ **失败（引入 P0-1，复核镜像用例复现）** | 覆盖核验（状态/方向/触发价/数量/去重）语义正确（protection.go:1-50）；`qty <= 0` 符号回归见 §1 |
| F05 越过止损只告警 | ⚠️ 条件裁决：负数量空头路径被 P0-1 跳过 | protectionFailure 3 次预算 → markCloseIntent + emergencyClose；fault 保留至快照证实 |
| F06 熔断后挂单仍可成交 | ✅ 修复合格 | cancelAccountPendingRisk：活跃策略+停用策略持久行+网格预留全覆盖；撤后查成交、残量保护、未确认终态保持 fault（pending_risk.go:51-120） |
| F07 多所保护回读契约 | ✅ 修复合格 | KuCoin（乘数换算+avgDealPrice 优先+done 细分+closeOrder:false 且 lots≤0 守卫兜底）、Gate（Rule 1/2 修正+圆整拒绝+cancel 只动 reduce-only）、Bybit（positionIdx 推导+closeOnTrigger 并入）、Aster（ReduceOnly/ClosePosition 补齐）、Hyperliquid（FrontendOpenOrders+未识别触发 fail-closed）逐个核过 |
| F08 裸仓 ATR 零数量 | ⚠️ 条件裁决：同 P0-1 符号依赖 | watchdog 恢复读真实 qty；SL 先落库后 TP——负数量空头路径依赖 §1 修复 |
| F09 成交平移破坏结构 | ✅ 修复合格 | reanchorProtectivePrices 整体删除；结构 SL/TP 保留，actualFillRisk 复核不合格→撤余量+退出；RecoveryReason 持久化（store 迁移幂等已核，启动 AutoMigrate 自动加列） |
| F10 未闭合 K 线进确认 | ✅ 修复合格（已复核） | 仅 fetchKlines 改：`closeTime >= now → skip` 对 Binance openTime+dur−1ms 无 off-by-one；全包 6 条 kline 摄入路径均经它；残留 volSlot 滞后一根见 P3-1（且顺带消除 live-vs-replay 分歧） |
| F11 回测 1h 柱不对齐 | ✅ 修复合格（已复核） | resample1h 仅 UTC 整点对齐的 15m 开盘 + 0/15/30/45m 严格连续性 + 丢弃尾桶；信号 bar 标签非连续即跳过 |
| F12 参数保存失败仍改内存 | ✅ 修复合格（已复核） | CreateTemp→Chmod→Write→Sync→Close→Rename 各错误路径 defer 清理；cloneParams 对 ShortWeights/*bool 真深拷贝；ApplyParamsChecked 先持久化后发布内存，失败保旧；loadParamsLocked 读前重置默认 |
| F13 回测目标与实盘不一致 | ✅ 修复合格（已复核） | regimeAt 需 4h 棒已收盘、btcRegimeTimeline 无前瞻且与 applyRegimeAdjustment 系数逐字一致；walk-forward purge 切断跨界标签；在位者比较拒不如 incumbent 的候选。**注意**：回放 α≡1/β≡1 与实盘尺度有系统差（P3-2），发布锁是护栏 |
| F14 空头 24h 标签漂移 | ✅ 修复合格（已复核） | 标签=TS+24h 前最后一根全收盘 1m close，funding 窗 (TS, labelAt]、单次扣成本、NaN/解析全防、重试 2/4/8/16h 封顶不空转、180d prune 键 TS 不丢未打标样本、LabelVersion≠2 自动重打 |
| F15 弱相关命名显著 | ✅ 修复合格（已复核） | 研究提案文件零读者、不调 ApplyParams、不推进 LastTunedMs（重跑不复合）；唯一 live 路径需 ShortTunerEnabled+ShortWeightsValidated+显式手改 params——全路径无旁门 |
| F16 复盘硬规则静默跳过 | ✅ 修复合格 | rule_engine.ParseRuleCondition 严格化（字段白名单+操作符校验+NaN 拒绝）；畸形 hard rule 按 OnViolation 产生 block/warning 而非 continue |

**测试基线**：`go build ./...` + trader/market/breakout/kernel/store 四包 `go test` 全绿（10-06 本轮复跑）。注意：P0-1 恰好是测试盲区（现有测试全部构造多仓快照）——修复时补空头镜像测试的原因即此。

### 4.1 kernel 信号层逐点验证记录（agent 全文审查 + 人工复核）

以下经全文审查确认**正确**，后续轮次无需重审（除非对应代码变更）：

- **成本净 RR 三处同源且单位正确**：kernel rr_scan 的 `(dist−costPct)/(stopPct+costPct)` 与执行端 `checkNetRR`（risk.go:2593）、`actualFillRisk`（fill_risk.go:40）逐字同式——顺带闭环了此前"kernel 用毛 RR 打分、执行端强制净 RR"的口径差；`TestFix05TargetMenuUsesNetRR` 钉死。
- **min-size 含成本**：`maxStopPct = riskUSD/minSize×100 − costBps/100` 与 `clampSizeToRisk` 的 `distPct += costBps/100` 契合，负值 fail-closed；`TestFix05MinimumSizeIncludesRoundTripCost` 钉死。
- **BTC 4h 数据链**：BTCUSDT fapi `interval=4h&limit+1`、forming 棒丢弃、1h/4h 缓存分离、`btc4hLastAttempt` 锁内更新、并发合流；300 根满足 `BTC4hRegime` 60 根 known 门槛；共享分类器与 shortscan 折扣、回测同源。
- **computeBreakoutState 几何**：固定前高 `bars[start:end]`（end=n−8）、cross/backInside/retest 窗口互洽；`OIConfirm > 0.3` 读的是已 ×100 的 OIDeltaPercent（0.3 = 0.3%），单位正确。
- **硬门码完备性**：`BTC_4H_STRONGBULL`、`SHORT_TOP_CONFIRM_MISSING`、`BTC_4H_DOWNTREND`、`BTC_WEAK_LONG_`、`WIDE_STOP_`、`NEG_EDGE_*` 全部在执行端 `absoluteBanCode` 有对应；WIDE_STOP 的 bstock 1d 豁免与日尺度带一致；NEG_EDGE 带符号比较正确。
- **kernel 无 positionAmt 类符号消费者**（P0-1 的 bug 类在 kernel 不存在）；唯一 PnL 符号消费 `TraderHistory.RealizedPnL` 净费、负=亏损，`NEG_EDGE_LOSING_SYMBOL` 拦的是真亏损户。
- **其余**：funding 年化 `perDay` clamp [1,24]、rollover 阈值按结算间隔缩放（旧→新排序）、R4-2 的 24h 小数→百分比 ×100、DataQuality 动态 TF 清单、buildLongPullbackPlan 几何（抑制顺序不可能清零回踩锚）、`FundingRolloverDetected` 方向——全部正确。

---

## 5. 已上线效果验证（批次 1+2，生产 10-05 17:07 起）

- 候选池质量下限生效（勘误后口径，复核窗口核对）：10-05 17:08:14 首扫 `Short-scan quality floor: skipped 76`（piggy floor 6/24）；10-06 13:25:16 skipped 69（piggy floor 8/24）——下限按设计工作；批次 2 门码在该窗口日志未见匹配（仅能证明"该日志中未见"，不作为无候选的证明）。
- 10-04 诊断的"池空跳周期"根因（网络抖动三源全空）在 10-04 13:16 已自愈；此现象与代码无关。
- SHORT_TOP_CONFIRM_MISSING / BTC_4H_STRONGBULL 尚无线上拦截样本（10-05 以来无符合条件的候选），gate_shadow_blocks 积累后再校准阈值。
- 磨顶宇宙（#7 遗留）生产实证维持（勘误后口径）：10-05 17:07→10-06 19:54 窗口 **191 次 skipped / 0 次入选**（10-06 当日 130/0）——日志事件计数，含同候选反复扫描，非独立交易机会。**待拍板**：独立评分 vs 降门槛 vs 砍宇宙。

## 6. 跨切面主题

1. **fail-closed 状态机成为一等公民**：protectionFaults/pending: 前缀/recovery: 前缀/account 级分区清晰，`clearProtectionFaults` 的"快照证实才清除"语义正确。唯一缺口是 P0-1 的误报源——状态机本身值得保留，修复应落在输入侧（qty 归一化）而非放松状态机。
2. **成本入模（20bps）统一三处**：prompt 示例 / validateOpenRisk / actualFillRisk 同式同源，无口径漂移——这是历轮"prompt 与执行端数字分叉"教训的正确落地方式。
3. **调参与执行的隔离**：研究提议永不写 live 权重 + 显式晋升门槛，堵住了 09-22 以来的权重轨蹦问题（structure 0.15→0.0298 事故）的根源。
4. **历轮 bug 类清点**（本轮未发现新实例）：`:=` 遮蔽（shadow vet 清零）、滚动窗当固定前高、prompt 硬编码示例数、fail-open 约定冲突、ROE/R 单位混用——均已在前几轮修复且有回归测试。符号归一化是**新加入清单的类**（P0-1 即首例），建议在适配器层统一出 `positionQtyAbs(symbol, side)` 之类的辅助函数，消费方一律取绝对值。

## 7. 遗留决策清单（不阻塞，维持前轮状态）

- 磨顶宇宙三选一（§5）；做空评分"延伸准入×转向确认"两段式（需回测数据）；候选级前向收益日志；kernel `emaOf` 首值种子/BTC 24h 相位（C5，低）。
- trader 各适配器历史 `if err :=` 惯用式 shadow（良性，不改）。

## 附录 A · 覆盖度矩阵

| 领域 | 方式 | 覆盖 |
| --- | --- | --- |
| trader 执行/风控（F01-F09 diff + 常规） | 人工 | diff 全量逐 hunk；常规面抽查（并发 map、order 语义、fail-open 清点） |
| kernel signal_layer.go 全文 | agent 全文审查 + 人工复核 | ✅ 完成：3 项发现（P2-1/2/5）+ §4.1 验证记录；结论已逐条核实 |
| kernel engine/prompt/schema/契约 | 人工 diff + agent 全文审查（完成，勘误节第 4 条修正其一处结论） | F02/F10/F14/F15/F16 hunks 全量；契约抽查；"并发合流"结论错误已撤回 |
| market/breakout（F10-F15 diff + 常规） | 人工 diff + agent 全文审查 | ✅ 完成：11 项发现全核实；研究→live 权重全路径、打标/重试/prune、原子写、回测对齐、F10 closeTime、10-04 修复互洽全部验证 |
| 交易所适配器（F07 + 常规） | 人工 | kucoin/gate/bybit/aster/hyperliquid hunks 全量；binance 仓位符号链路 |
| store/api/security/manager | 人工 | 迁移幂等、路由保护面、SSRF 守卫（前轮已审）、stash 内容 |
| web/src | 人工（重点面） | 保存路径（展开保留+409 双保险已核）、轮询 heartbeat（F17 已落地）、新字段不丢 |

## 附录 B · 验证与复现

```bash
# P0-1：构造空头持仓快照（positionAmt<0）跑 processProtectionWatchdog，断言无 fault —— 修复后加入测试
grep -n "qty <= 0" trader/auto_trader_risk.go        # 1600-1604 一带
grep -n "Short position quantity is negative" trader/auto_trader_risk.go trader/binance/futures_orders.go  # 老代码自证

# 生产版本差：生产二进制(10-05 17:07)不含 abba5978
ls -la nofx; git show abba5978 --stat | head -5

# 修复批次回归基线（本轮 10-06 全绿）
go build ./... && go test ./trader/ ./market/breakout/ ./kernel/ ./store/
```
