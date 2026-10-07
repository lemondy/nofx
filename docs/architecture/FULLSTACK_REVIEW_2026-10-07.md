# 全栈深度审查 · 2026-10-07（实盘数据驱动）

源码基线：`e364d7ec`（dev，工作树干净）。本轮与前五轮不同：**先用 `data/data.db` 的实盘记录（327 笔平仓、6552 个决策周期、2615 条门控反事实）评估方案效果，再回到源码找原因**。只新增本报告，未修改业务代码、配置或进程。两个 P1 已用临时单测复现，测试文件运行后已删除。

## 0. 结论速览

| 级别 | 编号 | 问题 | 影响 |
| --- | --- | --- | --- |
| P1 | N1 | 平仓 PnL/手续费重复累计（`357eb802` 引入） | 10-03 后所有平仓：开仓费翻倍；分批平仓的前序 PnL 翻倍。复盘、版本绩效、前端历史全部失真 |
| P1 | N2 | 持仓归属判定用 tradeId 对 orderId，永远不匹配（全部 9 个交易所同步） | 10-03 后 AI 仓位在 `trader_positions` 全部标为非 AI → **连亏熔断（loss_streak_ban）实际失效** |
| ~~P1~~ 已证伪 | S1 | ~~出场参数把盈利单压到 0.46R，期望值转负~~ → 回放验证（§9）：现行出场参数不是亏损原因，近期转负来自入场质量下降 | 09-29 后 expR −0.03（之前 +0.39）；盈利单平均 0.46R（之前 1.41R） |
| P1 | S2 | RR 门槛被远端目标满足，不约束可达空间 | 计划 RR 均值 3.46、止盈距离 17.9%；实际 MFE 均值 2.86%，**109 笔仅 2 笔触及计划止盈** |
| P2 | N3 | SSRF 校验在代理+本地 DNS 抖动时整轮失败 | 10-04 起 88 个决策周期失败（≈10%） |
| P2 | N4 | 风控参数校验仍缺上界和参数间关系 | `risk_per_trade_pct` 等可写入任意值；出场档位可配出自相矛盾组合 |
| P2 | F1 | 前端完全不展示后端已有的风控状态 | MAE/MFE(R)、exit_mode、挂单、连亏禁开、日内熔断都看不到 |
| P2 | F2 | K 线图每 5 秒拉 1500 根 K 线 + 200 条订单，后台标签不暂停 | 浪费交易所权重和后端负载；控制台持续打印大对象 |
| P3 | 其他 | 见 §5 | |

## 1. 实盘表现画像（数据口径说明）

- R 值统一用**价格**计算：`(exit−entry)/|entry−initial_stop|`，不受 N1 的 PnL 污染。仅统计 `initial_stop_loss>0` 且止损距离 <50% 的 144 笔。
- 09-29 是分界点：之后启用了 R 档位出场（`breakeven_arm_r=0.5`、`peak_drawdown_arm_r=1`、`tp_trim_at_r=1.2`）和 exit_mode。

| 区间 | 笔数 | 期望 R | 胜率 | 平均盈利 R | 平均亏损 R | 平均 MFE R | 平均止损距离 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 09-29 之前 | 82 | **+0.391** | 54% | **1.41** | −0.79 | 0.46 | 5.72% |
| 09-29 之后 | 62 | **−0.033** | 53% | **0.46** | −0.59 | 0.62 | 5.43% |

胜率几乎不变，MFE 反而更高，**差别完全来自盈利单被截短**。09-29 后按出场原因：

| 出场原因 | 笔数 | 平均实现 R | 平均 MFE R |
| --- | --- | --- | --- |
| ai_close | 19 | −0.29 | 0.26 |
| trailing_stop | 16 | 0.28 | 0.77 |
| stop_loss | 9 | −0.99 | 0.26 |
| drawdown_protect | 7 | 0.63 | 1.23 |

另：多头 224 笔累计 −37.27U，空头 103 笔 +14.73U；全周期手续费约 17.3U，接近全部净亏损（−22.5U，未修正 N1）。小账户（≈130U）下手续费是一阶因素。

## 2. 新发现的代码缺陷

### N1 · P1 · 平仓 PnL 与手续费重复累计

位置：[handleClose](/Users/zhangyun/workspace/nofx/store/position_builder.go:174) 计算 `totalPnL = position.RealizedPnL + realizedPnL`、`totalFee = position.Fee + fee`（已是累计值），再传给 [ClosePositionFully](/Users/zhangyun/workspace/nofx/store/position.go:453)，后者在 `357eb802`（10-03 23:56）改为 `realized_pnl + ?`、`fee + ?` 的 SQL 累加。

结果：
- 每笔平仓：`fee = 2×(开仓费 + 前序分批平仓费) + 末笔费`。
- 分批平仓：`realized_pnl = 2×前序分批 PnL + 末笔 PnL`。

实盘核对（`trader_fills` 汇总 vs `trader_positions` 存储）：#294 之前全部一致；之后不一致，例如 #316 CAPUSDT 成交汇总 −1.8806 / 费 0.0476，存储 −3.6481 / 0.0932；#300 MANAUSDT −1.8331 存储为 −3.4113；#309 PENDLE −1.9018 存储为 −3.2239。

复现：开 100@1.00（费 0.02）→ 平 60（PnL −1.2，费 0.03）→ 平 40（PnL −0.8，费 0.02），结果 `realized_pnl=-3.20`（应 −2.00）、`fee=0.12`（应 0.07）。

影响面：`trade_journal`、复盘页、[版本绩效 StatsForWindow](/Users/zhangyun/workspace/nofx/store/strategy_version.go:384)、PositionHistory、POOR_HISTORY 类统计、AI 提示词里的近期交易。账户权益来自交易所，不受影响。

修复：二选一且只选一处累加——`ClosePositionFully` 接收**本笔增量**（`realizedPnL`, `fee`）并保留 SQL 累加；或保持调用方累计、改回绝对写入但加乐观锁。补“开→分批平→全平”的端到端回归，并写一次性脚本按 `trader_fills` 重算 10-03 之后的行与对应 journal。

### N2 · P1 · AI 归属判定 ID 口径错位，连亏熔断失效

AI 下单后 [markAIManaged](/Users/zhangyun/workspace/nofx/trader/auto_trader.go:819) 记录的是**交易所 orderId**（`ai_entry_orders`）；而 [Binance 同步](/Users/zhangyun/workspace/nofx/trader/binance/order_sync.go:282) 传给 `ProcessTrade` 的是 **tradeId**（[GetTradesForSymbol](/Users/zhangyun/workspace/nofx/trader/binance/futures_account.go:189) 构造 `TradeRecord` 时丢弃了 `at.OrderID`）。[handleOpen](/Users/zhangyun/workspace/nofx/store/position_builder.go:67) 的 `OwnsEntry(orderID)` 因此永远 false；`mark()` 中 `entry_order_id = orderID` 的补写同样永远匹配不到。`IsLegacyMarked` 只认 `entry_order_id=''` 的旧标记，而新标记都带 orderId；`MarkedAfter` 又要求标记晚于行创建。三条路全断。

实盘：#294（10-03 21:22）起全部 AI 仓位 `ai_managed=0`，`ai_entry_orders` 中 28 个 orderId 与持仓 `entry_order_id`（tradeId）无一相同。全部 9 个交易所同步都传 trade/exec ID。

影响：[lossStreakBannedMap / lossStreakBlocks](/Users/zhangyun/workspace/nofx/trader/auto_trader_lossstreak.go:109) 只统计 `p.AIManaged`，**配置 `loss_streak_ban_enabled=true, max_losses=3` 目前等于关闭**。运行时 `isAIManaged`（查 `ai_managed_positions` 注册表）不受影响，所以移动止损/看门狗仍工作。

修复：`TradeRecord` 增加 `OrderID`，各适配器填交易所订单号，`ProcessTrade` 用 orderId 做归属、tradeId 仅做去重；回填脚本用 `ai_entry_orders` ↔ 交易所 userTrades 的 orderId 修正历史行。补“AI MarkEntry(orderId) → 同步 tradeId 成交 → 行 ai_managed=true → 连亏计数生效”的集成测试。

### N3 · P2 · SSRF 校验让 AI 调用随本地 DNS 抖动整轮失败

[ValidateURL](/Users/zhangyun/workspace/nofx/security/url_validator.go:147) 在配置代理且本地 DNS 失败时直接拒绝。10-04 起 `open.bigmodel.cn` 因此失败 88 个周期。目标是内置的服务商域名而非用户输入，这里 fail-closed 的收益很小。

建议：对内置 provider 域名白名单跳过远端解析校验，或对 DNS 失败做短重试；用户自定义 URL 保持现状。

### N4 · P2 · 参数合法性校验仍不完整

[Validate](/Users/zhangyun/workspace/nofx/store/strategy_validation.go:11) 仍不限制 `risk_per_trade_pct`、`account_max_drawdown_pct`、`daily_max_loss_pct`、`max_stop_distance_pct` 的上界，也不检查出场档位关系（如 `breakeven_arm_r < peak_drawdown_arm_r < tp_trim_at_r`、`peak_drawdown_giveback_r∈(0,1)`）。10-05 审查已提出，仍未处理。

## 3. 方案层评估（选币 / 开仓价 / 止盈止损 / 风控 / 参数迭代）

### 3.1 标的选择

当前：mixed = AI500(5) + 猪头冲刺(8) + 做空扫描(8)，每周期约 15–21 个候选全部进入 LLM。

- **扫描器评分没有区分度。** [breakout_backtest.json](/Users/zhangyun/workspace/nofx/data/breakout_backtest.json)：756 个信号中 weak 752、medium 4、strong 0；weak 胜率 44%。`strong_threshold=80` 实际从未触发，分档是摆设。自动调参只在 weak 档内部移动中心值，不解决分布塌缩。建议按分位数重标定（如 top 10%=strong），或直接改为输出连续分数供排序。
- **门控反事实：拦截方向对，区分度弱。** 2557 条被拦设置，按“TP 先到=+plan_rr / SL 先到=−1 / 8h 超时按收盘 R”再扣 0.05R 成本，总体 −0.063R。单独拦截时：`MICRO_TREND_NOT_SHORT` −0.19R（±0.09，有效）；`MICRO_TREND_NOT_LONG` 0.00R、`CONSENSUS_OPPOSED` +0.05R（±0.13，接近中性，可能误杀）。结论：**候选池整体边际接近 0，再加门槛收益有限，改善应放在出场几何与成本上**。
- mixed 候选经 `map` 汇总后顺序随机（[engine.go:932](/Users/zhangyun/workspace/nofx/kernel/engine.go:932)），提示词顺序每周期不同：影响 LLM 位置偏好、复现性和提示缓存命中。建议按来源优先级 + 分数稳定排序。
- 多头 −37U vs 空头 +15U。建议把多头 breakout-retest 与空头 topping/breakdown 分家族统计期望 R，按家族而非整体调权重。

### 3.2 开仓价格

当前 `limit_entry_enabled=false`，开仓以市价为主；LLM 平均延迟 120–190s、最长约 10 分钟，决策价与成交价之间有数分钟漂移。执行端会在实时价重算净 RR（[validateOpenRisk](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:707)），这点是对的；但结构、门控不随之刷新（10-05 审查 3.2 已提出）。

建议：在 S1/S2 解决后再评估开启限价回踩入场。市价入场 + 5% 止损 + 0.5R 保本线的组合，等于用 taker 费率和点差换取一个很难留住的仓位。

### 3.3 止损与止盈价位

止损（[methodStopPlan](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:1384)）：结构 ± 0.4/0.5×ATR(1h)，落在 [1.5×ATR(1h), max(2×ATR(4h),8%)] 内，方法可解释，实盘平均 5.4%。止损执行时实际亏损基本是 −1R（stop_loss 9 笔均 −0.99R），保护链路可靠。

止盈（[scanRRWindowWithCost](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:2034)）是 **S2** 的根源：`first_rr_ge_target` 跳过所有 RR 不足的近端结构，取第一个满足 `min_rr` 的远端结构。被跳过的近端结构就是价格最先遇到的阻力。实盘结果是计划 TP 平均 17.9% 远、触达率 1.8%，RR 门槛不再筛掉任何“前方有墙”的交易。

建议：
1. 增加 **first-obstacle RR**：最近一个结构目标的 RR 必须 ≥ 阈值（如 0.8–1.0R），否则视为空间不足；远端目标只用于持仓管理的跑赢部分。
2. RR 门槛用**经验可达性**校准：按历史 MFE 分布估计“到达 xR 的概率”，以 `P(hit)×R − (1−P)×1 − 成本` 作为开仓期望，而非几何 RR。
3. 展示/记录第一障碍价位，复盘时可以直接对照 MFE。

### 3.4 持仓管理（S1）

> **更正（同日回放验证，见 §9）**：本节“保本线过早导致期望转负”的推断未被证实。145 笔实盘入场在 1 分钟 K 线上回放，现行参数期望 +0.117R，各替代方案差异均不显著；09-29 后的负期望在所有出场方案下都存在，原因在入场质量。下文保留原始推理供对照。

当前配置的实际行为（[processVolTargetAndTrailing](/Users/zhangyun/workspace/nofx/trader/auto_trader_vol.go:197)、[checkPositionDrawdown](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:156)）：

- 0.5R → 止损移到 entry+0.2R（`breakeven_arm_r=0.5`，offset 默认 0.2）
- 1R 峰值后回撤 50% → 平仓（`peak_drawdown_arm_r=1`, giveback 0.5）
- 1.2R → 减仓（`tp_trim_at_r`）
- 1.5R 才启动 2×ATR(1h) 跟踪

09-29 后 30 笔 MFE≥0.5R：13 笔以 ≤0.4R 被扫出，平均最终 0.42R 对比平均 MFE 1.05R，回吐约 60%。在 5% 级别止损的山寨币上，0.5R ≈ 2.5% 的波动就是噪声范围，保本线被正常回踩触发。

建议（需用影子回放验证后再改）：
- 先做**参数对照**而不是改代码：A=现行；B=`breakeven_arm_r` 关闭或 ≥1.0；C=B + `peak_drawdown_arm_r=1.5`、giveback 0.6。数据现成：`trader_positions` 的 MAE/MFE 加 K 线即可离线回放。
- 保本偏移按成本而非固定 0.2R：目标是“净保本”，偏移 = 往返费 + 滑点。
- （更正：回放显示 AI 平仓比规则出场平均多 +0.21R，14/19 笔更好，见 §9）`ai_close`（19 笔，−0.29R）需要单独审视：AI 平仓时平均 MFE 仅 0.26R、MAE 0.53R。它提前止损了，同时也可能砍掉了会回来的单，需要用反事实验证（记录 AI 平仓时价位，跟踪到原 SL/TP）。

### 3.5 风险控制

做得好的：结构止损定仓、实时价复核净 RR、强平距离校验、账户风险汇总含挂单预留、保护看门狗。

当前配置下的缺口：
- `loss_streak_ban` 因 N2 失效。
- **（勘误）** 初版写“`daily_max_loss_pct=0`、`max_account_risk_pct=0`、`max_spread_pct=0` 全部关闭”有误：这三项的 0 表示**使用默认值**（日内 10%、账户总风险 10%、点差 0.5%），均已生效；负数才是关闭。真正关闭的只有 `max_entry_slippage_bps=0`（入场滑点保护）。130U 账户、`max_positions=10`、单笔 2% 时，账户总风险上限 10% 是实际约束；如需更保守可下调到 6–8%。
- [clampSizeToRisk](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:913) 读 `EntryRoundTripCostBps`（原始值，≤0 时用 20），而 `checkNetRR` 读 `EffectiveEntryRoundTripCostBps()`；两处成本口径应统一走 Effective。
- 账户回撤熔断仍以 `initialBalance` 为基准，不是高水位（10-05 已提出）。

### 3.6 参数迭代

- `strategy_config_versions` **表为空**：基线只在用户打开“版本”页（lazy GET）或 UI 保存时写入。建议 trader 启动时写一次基线，否则版本时间线从第一次打开页面才开始。
- 版本绩效读 journal 的 `RealizedPnL−Fee`，10-03 后被 N1 污染；修 N1 前版本对比结论不可信。
- 平仓原因：327 笔中 254 笔为 `sync`（09-29 前未归因），出场规则无法回溯评估；09-29 后已改善。
- 自动调参只调扫描器阈值，最后一次是 10-01；**出场档位、保本线、RR 门槛等对期望值影响最大的参数没有任何回测/影子验证闭环**。`gate_shadow_blocks` 已经是很好的反事实框架，建议扩展到出场：每笔平仓后继续跟踪原 SL/TP 8–48h，得到“不同出场规则下的 R”。

## 4. 前端体验

检查：`tsc --noEmit` 通过；`vitest` 7 文件 113 用例通过；**`eslint` 20 个 prettier 错误**（[VersionsPanel.tsx](/Users/zhangyun/workspace/nofx/web/src/components/strategy/VersionsPanel.tsx:294)、[StrategyStudioPage.tsx](/Users/zhangyun/workspace/nofx/web/src/pages/StrategyStudioPage.tsx:573)），`npm run lint` 的 `--max-warnings 0` 会失败，`npm run lint:fix` 可修。

| 级别 | 问题 | 位置 | 建议 |
| --- | --- | --- | --- |
| P2 | 风控状态不可见：后端已有 `mae_r/mfe_r/exit_mode/initial_stop_loss`、挂单、连亏禁开、日内基线、账户回撤，前端一处都没展示 | 全站 grep 无引用 | 面板加“风控状态”卡（连亏禁开名单与解禁时间、日内亏损/阈值、账户回撤/阈值、未保护仓位数）；持仓行加当前 R、初始止损、是否已保本、exit_mode；挂单列表 |
| P2 | K 线每 5s 拉 1500 根 + 200 订单，`document.hidden` 时也不停；每次 `JSON.stringify` 全部 markers 打印 | [AdvancedChart.tsx:982](/Users/zhangyun/workspace/nofx/web/src/components/charts/AdvancedChart.tsx:982) | 增量拉最近 2–3 根；订单 30–60s；页面隐藏时暂停；移除调试日志（全站 57 处 `console.log`） |
| P3 | 历史持仓行显示**毛** `realized_pnl`，颜色也按毛值判断；统计卡是净值口径 | [PositionHistory.tsx:252](/Users/zhangyun/workspace/nofx/web/src/components/trader/PositionHistory.tsx:252) | 行内显示净值（PnL−Fee），毛值放 tooltip |
| P3 | 版本页的 R/盈亏会展示 N1 污染后的数字 | VersionsPanel | 修 N1 后重算；修复前在页面标注“10-03 后数据待校正” |
| P3 | 大量内联硬编码颜色（ReviewPage 136 处、DataPage 87 处） | | 收敛到 Tailwind token，便于主题与可读性统一 |
| P3 | 风控编辑器 48 个数值项缺少“组合效果”提示，例如 0.5R 保本 + 1R 回撤保护的叠加含义 | RiskControlEditor | 加一个出场阶梯预览（按 R 画出各档位触发点），配合 N4 的后端关系校验 |

## 5. 其他 P3

- mixed 候选顺序随机（见 3.1）。
- [Binance GetTradesForSymbol](/Users/zhangyun/workspace/nofx/trader/binance/futures_account.go:189) 丢弃 `IsMaker`，同步时 `IsMaker=false`：无法统计 maker/taker 比例，对评估限价入场价值不利。
- 门控反事实以 8h 为唯一 horizon（`outcome_48h` 字段存在但未见统计使用）。

## 6. 建议执行顺序

1. **N1 + N2 修复 + 历史数据回填**（数据可信是一切调参的前提）。
2. 视情况下调 `max_account_risk_pct`（默认 10%）、开启 `max_entry_slippage_bps`；修 N3（可用性）。
3. 用现有 MAE/MFE + K 线离线回放出场参数 A/B/C（S1），选出后小仓位影子运行一周。
4. 引入 first-obstacle RR 与经验可达性（S2），先作为提示词证据和影子门，验证后再升为硬门。
5. 扫描器分档重标定；前端风控状态面板与 K 线轮询优化。

## 7. 复现与查询

- N1/N2：临时测试 `store/zz_review_tmp_test.go`（已删除），输出：`realized_pnl=-3.2000 (expect -2.0000) fee=0.1200 (expect 0.0700)`；`YUSDT ai_managed=false (AI-opened, expect true)`。
- N1 实盘核对：`trader_positions` 每行与 `trader_fills` 在 `[entry_time, exit_time]` 内同 symbol 的 `sum(realized_pnl)`、`sum(commission)` 对比。
- R 统计：`(case when side='LONG' then exit_price-entry_price else entry_price-exit_price end)/abs(entry_price-initial_stop_loss)`，分界 `entry_time > 1790611200000`（09-29 00:00 +08）。
- 门控反事实：`gate_shadow_blocks` where `outcome!=''`，tp_first=+plan_rr，sl_first=−1，timeout=8h 收盘 R，统一扣 0.05R。
- 局限：样本量小（09-29 后 62 笔），结论是方向性的；没有逐笔 tick 回放，出场参数建议需要离线回放验证后再上线。

## 8. 修复状态（2026-10-07 同日）

| 编号 | 处理 | 位置 / 测试 |
| --- | --- | --- |
| N1 | 已修：`ClosePositionFully` 改为只接收本笔增量；孤儿对账路径传 0/0（原来同样翻倍） | `store/position.go`、`store/position_builder.go`、`trader/auto_trader_orphan.go`；`TestReview07CloseDoesNotDoubleCountPnLOrFee` 等 3 例 |
| N1 数据 | 回填工具已就绪，**未执行**：`go run ./cmd/repair20261007`（默认只预览，`-apply` 前自动 `VACUUM INTO` 备份）。预览：31 行盈亏/手续费待修 | `store/repair_20261007.go`；`TestReview07RepairCloseAccumulation` |
| N2 | 已修：`TradeRecord` 增加 `OrderID`/`IsMaker`，9 个交易所同步改用交易所订单号做持仓归属；币安成交记录写入真实 maker 标记 | `trader/types/interface.go` + 各适配器；`TestReview07OwnershipByExchangeOrderID`、`TestTradeRecordPositionOrderID` |
| N2 数据 | 同一工具（只读查询币安 userTrades）：预览 43 行换回真实订单号，其中 28 行 AI 仓位恢复 `ai_managed=1`；10-06 晚的手动 CAPUSDT 交易仍判为非 AI | `TestReview07ReattributeEntryOrder` |
| N3 | 已修：DNS 失败先重试一次；仍失败时仅对内置公共 API 域名放行，用户自定义域名仍拒绝 | `security/url_validator.go`；`url_validator_dns_test.go` 3 例 |
| N4 | 已修：上界 + 出场档位关系校验，遵循“0=默认、负数=关闭”约定；4 个现有策略配置全部通过 | `store/strategy_validation.go`；`TestReview07ValidateBoundsAndLadder` |
| S2 | 已加证据字段 `rr_scan.first_obstacle / first_obstacle_rr` 并写入 prompt 字段说明；**不是硬门**，需积累数据后再评估 | `kernel/signal_layer.go`；`TestRRScanReportsFirstObstacle` |
| S1 | 已验证（§9）：不建议修改现行出场参数 | `docs/architecture/s1-exit-replay-2026-10-07/` |
| 成本口径 | 已统一：仓位计算改用 `EffectiveEntryRoundTripCostBps()`（行为不变） | `trader/auto_trader_risk.go` |
| 候选顺序 | 已修：mixed 候选按“多来源优先、再按币名”稳定排序 | `kernel/engine.go`；`TestSortMixedCandidatesDeterministic` |
| 版本基线 | 已修：trader 启动时写入版本 1 基线 | `trader/auto_trader_configcheck.go` |
| F1 | 已做：新接口 `GET /api/risk-status`（只读）+ 面板“风控状态”（日内亏损/账户回撤进度条、连亏禁开、挂单、持仓当前 R 与止损锁定 R）；历史持仓新增 R 列（悬停看 MFE/MAE、出场模式） | `trader/risk_status.go`、`RiskStatusPanel.tsx`；`TestRiskStatusPositionR`、`realizedR.test.ts` |
| F2 | 已修：刷新只拉最近 3 根 K 线并合并；订单标记 60s 一次；标签页隐藏时暂停；删除调试日志 | `AdvancedChart.tsx`；`mergeKlines.test.ts` |
| 历史行净值 | 已修：行内显示净盈亏，毛值放悬停 | `PositionHistory.tsx` |
| lint | 已修：20 个 prettier 错误 | — |

验证：`go build ./...`、`go test ./... -count=1` 全部通过；前端 `tsc`、`eslint --max-warnings 0`、`vitest`（118 例）、`vite build` 通过。

**上线顺序**：先重新编译并重启进程（旧二进制仍会继续写出翻倍的行和错误归属），再执行 `go run ./cmd/repair20261007 -apply`（幂等，可重复执行）。

## 9. S1 出场参数回放验证（2026-10-07）

方法：145 笔有开仓止损的实盘交易，入场价、开仓止损、计划止盈固定不变，用币安 1 分钟 K 线重放价格路径，只替换出场规则（反事实对比）。规则逐条对应源码：交易所 SL/TP 按 1 分钟高低价成交，同一根 K 线内双触按先止损（保守）；回撤保护和 1.2R 减仓每分钟按收盘价判断；保本线、1R 锁利、2×ATR(1h) 移动止损每 5 分钟判断；结构止盈平 50%（趋势模式）；不模拟 AI 主动平仓；72 小时后按收盘价平仓；统一扣 12bps 往返成本。脚本与输出：[s1-exit-replay-2026-10-07](/Users/zhangyun/workspace/nofx/docs/architecture/s1-exit-replay-2026-10-07/README.md)。

**模拟器校准**：09-30 后 28 笔由程序规则出场的交易，模拟 R 与实际 R 的相关系数 0.885，平均绝对误差 0.18R，22/28 笔误差在 ±0.25R 内。模拟整体比实际偏保守约 0.09R；主要偏差是 AI 期间主动收紧止损（未建模），对各方案影响一致。

**方案对比（全部 145 笔，成对 bootstrap 95% CI）**：

| 方案 | 期望 R | 胜率 | 平均盈利 R | 最大回撤 R | 相对现行 [95% CI] | 前半 / 后半样本 |
| --- | --- | --- | --- | --- | --- | --- |
| A 现行（0.5R 保本、1R 锁、1.2R 减 1/3、1R 回撤 50%、1.5R 起 2×ATR 跟踪） | **+0.117** | 64% | 0.73 | **−11.5** | — | — |
| B 关闭 0.5R 保本 | +0.141 | 54% | 1.08 | −15.3 | +0.024 [−0.060, +0.106] | +0.095 / −0.047 |
| C B + 回撤保护 1.5R/60% | +0.152 | 53% | 1.15 | −14.8 | +0.035 [−0.064, +0.137] | +0.086 / −0.015 |
| D 只留 SL/TP + 1R 锁 + 跟踪 | +0.089 | 52% | 1.05 | −19.1 | −0.029 [−0.265, +0.160] | −0.044 / −0.014 |
| E 纯 SL/TP 不管理 | +0.039 | 35% | 1.92 | −23.3 | −0.079 [−0.343, +0.158] | −0.103 / −0.054 |
| F 保本 1R / 减仓 1.5R / 回撤 2R | +0.172 | 52% | 1.21 | −15.7 | +0.054 [−0.055, +0.167] | +0.108 / +0.001 |
| G 现行但保本 0.8R | +0.099 | 57% | 0.93 | −14.1 | −0.018 [−0.085, +0.049] | −0.002 / −0.035 |

结论：

1. **没有方案稳定优于现行参数**：所有差值的置信区间跨过 0，B/C 在前后半样本方向相反。F 点估计最高但后半段优势归零，不足以支持上线。现行参数胜率最高、回撤最小。
2. **不管理更差**：纯 SL/TP（E）145 笔只有 2 笔触及计划止盈，佐证 S2（目标过远）；持仓管理本身是有效的。
3. **09-29 后负期望与出场无关**：该子样本（63 笔）在所有方案下均为负（A −0.098R，最好的 F 也只有 −0.073R），差异同样不显著。
4. **AI 主动平仓是加分项**：19 笔 AI 平仓实际 −0.314R，若交给规则 A 为 −0.524R，AI 平均多 +0.21R（14/19 更好，CI [−0.07, +0.46]）。初版 §3.4 对 ai_close 的怀疑不成立。
5. **入场质量下降是主因**（与出场无关的度量，入场后价格先到 +xR 还是先到 −1R）：+1R 先到的比例从 64%±10% 降到 49%±13%（盈亏平衡约 50%），+2R 从 41% 降到 26%（平衡约 33%）。区间部分重叠，属方向性证据。

**建议**：保持现行出场参数不变。改进重点转向入场：选币/门控按家族统计（§3.1），以及 S2 的第一障碍位证据积累后评估。出场参数今后若要调整，用本目录脚本在新增样本上复跑，要求差值置信区间不跨 0 且前后半样本同向再上线。

局限：样本 145 笔（近期 63 笔）；未模拟 AI 平仓与 AI 调整止损；以 1 分钟收盘近似标记价；决策周期按整 5 分钟近似（实际含 AI 延迟）；成本统一 12bps。
