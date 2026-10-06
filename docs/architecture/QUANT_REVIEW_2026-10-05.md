# NOFX 策略、执行、风控与自动进化深度审查

审查日期：2026-10-05。基线：`dev`，`4cf16c408c31b9e58127f358b884872a9db4869a`。本次只增加审查文档和复现证据，没有修改交易逻辑。

## 1. 结论与优先级

当前仓库已形成“扫描候选 → 确定性信号/门控 → AI 决策 → 价格与仓位校验 → 成交后保护 → 持仓管理”的完整框架。结构止损、ATR 噪声底线、账户风险预留、部分成交恢复和单调移动止损都有实际实现，不能简单评价为仅靠提示词交易。

但框架完整不等于执行闭环可靠。本次识别 **16 项问题：8 项 P1、8 项 P2**。P1 指在相应配置或交易所路径启用时，可能绕过禁开规则、错误处理成交或留下止损保护缺口；P2 指影响结构交易语义、数据一致性或进化验证可信度的问题。10 个新增边界用例复现了其中 9 类问题，其余结论来自代码调用链或方法审查，详见证据标记。

| 关注点 | 判断 | 首要动作 |
| --- | --- | --- |
| 做多/做空候选 | 多种信号来源有覆盖，但高分不等于有可执行优势；BTC 周期接线和做空门控存在错误 | 修复 F02/F03/F10，再分开验证趋势延续与反转做空 |
| 入场位 | 多头突破回踩有结构依据；普通多空锚点仍主要是价格的 ATR 偏移 | 保留结构绝对价，统一刷新和挂单失效规则 |
| 止盈止损 | 结构 + ATR 的基础合理；成交平移会破坏原结构，保护闭环有缺口 | 先修复保护，再试验按市场状态使用布林线 |
| 风控 | 覆盖开仓、账户、挂单和持仓多个层次，但不同执行路径及适配器尚未一致 | 优先 F01/F03–F08；统一保护覆盖量和熔断后的撤单 |
| 自动进化 | 有时间留出、成本扣减和部分防泄漏措施；不足以确认真实交易策略变好 | 保持短权重调优默认关闭；改为真实执行回放和现行参数对照 |

**建议顺序：执行与保护缺陷 → 数据与回放一致性 → 基准交易回测 → ATR/布林组合 → 自动参数发布。** 当前证据不支持直接扩大自动调参范围或宣称某个 ATR/布林参数最优。

## 2. 具体发现

### F01 · P1 · 不利滑点方向计算反了

位置：[adverseSlippageBps](/Users/zhangyun/workspace/nofx/trader/auto_trader_orders.go:364)。证据：`TestAudit05AdverseSlippageDirection`。

多头使用 `checkedPrice-fillPrice`，空头使用 `fillPrice-checkedPrice`，均与“不利成交”相反。校验价 100，多头成交 102、空头成交 98，应为 200bps 不利滑点，目前返回 0；多头成交 98、空头成交 102 的改善成交却返回 200bps。

当 `max_entry_slippage_bps > 0` 时，结果是放过更差成交、紧急平掉更好成交。应改为多头 `fill-checked`、空头 `checked-fill`，再与零取最大值；同时验证紧急平仓失败后的保护状态。此项需要先于任何入场参数优化修复。

### F02 · P1 · BTC “4h” 门控实际输入 1h 数据

位置：[信号选项接线](/Users/zhangyun/workspace/nofx/kernel/engine_prompt.go:1598)、[btc4hShape](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:1625)、[已存在的真实 4h 获取函数](/Users/zhangyun/workspace/nofx/kernel/engine_data_binance.go:726)。证据：静态调用链；`binanceBTC4hCloses` 当前无消费者。

`BtcTrendCloses` 传入 `binanceBTC1hCloses(300)`，下游直接按真实 4h 收盘序列计算 EMA20/50 和 BTC 市场状态。`btc4hShape` 的六根收益被命名为 24h，实际变成六小时收益；随后与标的真实 24h 涨跌幅比较。做空强牛市门控也使用该序列。

这不是阈值争议，而是周期单位错误：候选扫描器的真实 4h BTC 环境与执行内核可能相互矛盾。应接入真实已闭合 4h 数据，并增加从生产选项构建到门控的集成测试，验证周期和收益区间。

### F03 · P1 · 做空禁开规则未覆盖所有下单动作

位置：[absoluteBanCode](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:2257)、[方向门控消费](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:2354)、[日线逆势做空分支](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:2552)。证据：`TestAudit05ShortBansMustReachMarketExecution`、`TestAudit05DailyUptrendBanMustCoverLimitShort`。

内核产生 `BTC_4H_STRONGBULL`、`SHORT_TOP_CONFIRM_MISSING` 后，市价动作只检查 `absoluteBanCode` 的名单，而该名单缺少这两个码。关闭限价入场模式时，[marketExceptionGate](/Users/zhangyun/workspace/nofx/trader/auto_trader_orders.go:106) 又直接放行，因此相应“硬禁开”不能可靠阻断市价做空。

另一处 `BlockShort1dUptrend` 只匹配 `open_short`，没有匹配 `open_short_limit`；日线上行时限价空单可绕过该开关。

应由统一策略对象明确每个门控的阻断语义与可例外条件，覆盖市价、限价、动作转换和挂单剩余部分。测试矩阵至少包含“禁开码 × 多空 × 市价/限价 × fallback × 首次下单/待成交/部分成交”。

### F04 · P1 · 看门狗把“有止损单”当成“足额保护”

位置：[missingProtection](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:1443)、[看门狗提前退出](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:1637)。证据：`TestAudit05WatchdogMustRepairInsufficientSLCoverage`。

函数只按类型和部分持仓方向字段判断，不验证数量、触发价有效性、`reduceOnly`，也未在 `BOTH` 情况下核对平仓买卖方向。1 单位多仓仅有 0.1 单位止损及足额 TP 时，看门狗认定两条保护腿均存在，不修复其余 0.9 单位。

仓库已经有 [protectiveCoverage](/Users/zhangyun/workspace/nofx/trader/auto_trader_orders.go:697)，但看门狗没有共用这套数量判定。应统一保护核对：正确方向、正确类型、有效触发价、实际覆盖量、全平标志和减仓语义；TP 的期望量按退出模式计算。`ClosePosition` 的全仓保护应单独处理，不能把所有交易所都视作数量型保护。

### F05 · P1 · 止损缺失且价格已越过记录止损时，只告警

位置：[wrong-side recorded SL 分支](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:1668)。证据：`TestAudit05CrossedMissingStopMustExit`。

AI 管理多仓原止损 95，现价 90，交易所止损已被取消。看门狗因 95 已处于错误触发侧而跳过补挂，只发送人工核查告警。该路径没有执行紧急减仓/平仓，也没有因此阻止账户继续增加风险。

应明确故障状态：止损已越过 → 执行减仓退出；未越过但补挂失败 → 有限重试并限制新风险；退出失败 → 持久化恢复任务并持续重试。该硬退出不应依赖 AI 是否主动提出平仓，也不应被最短持仓或趋势持有规则阻断。

### F06 · P1 · 日内熔断后，已有入场挂单仍可成交

位置：[NEW 挂单维护](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:545)、[日内损失门控](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:2422)。证据：`TestAudit05DailyHaltMustCancelPendingRisk`。

日内熔断消费发生在新决策过滤阶段；已有 NEW 挂单只检查“现价越过 SL”及存续时间。基准权益 1000、当前权益 900、日损阈值 5% 时，熔断已触发，原入场挂单仍保留。账户回撤熔断、禁开规则变化、市场环境反转也没有统一进入挂单剩余风险的复核。

熔断必须同时取消账户内所有增加风险的挂单，并保留已成交部分的保护计划。每次维护应使用新账户状态与新信号重新评估剩余量；不能仅在下次 AI 产出新开仓动作时才消费熔断。

### F07 · P1 · 多交易所保护单回读契约不一致

证据：静态适配器和消费者调用链；未进行真实交易所下单测试。影响范围限定如下。

| 适配器 | 确定的代码问题 | 后果 |
| --- | --- | --- |
| [Hyperliquid](/Users/zhangyun/workspace/nofx/trader/hyperliquid/trader_account.go:541) | 用 `order.Coin != symbol` 直接过滤，持仓返回的 `BTCUSDT` 与订单中的 `BTC` 不一致；回读还固定 `Type=LIMIT`、`StopPrice=0` | 保护单在通用核对器中不可见，可造成补挂、验证失败或错误恢复 |
| [Gate](/Users/zhangyun/workspace/nofx/trader/gate/trader_orders.go:638) | 仅按 `Trigger.Rule == 2` 标记 TP；按该适配器自己的下单定义，空仓 SL 使用 Rule2、空仓 TP 使用 Rule1 | 空仓 SL/TP 在回读时互换，影响补挂与保护验证 |
| [KuCoin](/Users/zhangyun/workspace/nofx/trader/kucoin/trader_orders.go:766) | `sell` 回读为 `PositionSide=SHORT`，但平多保护本应 `SELL/LONG`；触发单统一标为 STOP，数量回读为 lots 而非统一的基础资产数量 | 平多保护归到空侧、TP 不可识别、覆盖量单位不一致 |

建议先建立统一 `OpenOrder` 契约和适配器录制响应测试，涵盖 LONG/SELL、SHORT/BUY、单向模式、TP/SL、数量单位、触发价、reduceOnly/closePosition。适配器未满足契约时，明确限制其自动保护能力；不能复用 Binance 的成功测试推断其他交易所可靠。Aster 当前保护下单已带 `reduceOnly`，本报告不重复把旧版本缺失该字段列为现存问题。

### F08 · P1 · 无记录裸仓的 ATR 兜底仍用零数量下保护

位置：[placeComputedProtection](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:1745)、[OKX 数量校验](/Users/zhangyun/workspace/nofx/trader/okx/trader_orders.go:420)。证据：静态调用链。

有记录止损的修复路径已查询真实持仓数量，但无记录裸仓的兜底分支仍调用 `SetStopLoss(..., 0, sl)` 与 `SetTakeProfit(..., 0, tp)`。例如 OKX 会按零合约数量拒绝；零数量不具有跨交易所“全平”的统一语义。

还存在状态提交问题：SL 成功、TP 失败时函数直接返回，直到两腿都成功才记录止损和初始 R。建议读取实际数量，按退出模式计算 TP 量，分别验证并持久化每条腿；SL 保护失败的处置优先级高于 TP 补挂失败。裸仓兜底只是事故恢复，不能替代原计划失效后的退出判断。

### F09 · P2 · 成交后平移 SL/TP，破坏结构价位

位置：[市价平移](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:795)、[限价平移](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:667)。证据：公式与调用链。

当前所有保护价都加上 `fill-ref`，几何 RR 保持不变，但策略要求的结构位置改变了。例：入场 100，支持位 95，结构止损 94，阻力目标 112；成交 103 后变成 SL97/TP115。SL 已移到支持位上方，TP 越过原阻力；“RR 不变”掩盖了结构失真。改善成交也会反向平移绝对结构。

应把“绝对结构价/布林快照”和“纯波动距离价”区分。前者固定其市场坐标，并以实际成交价重算净 RR、单位风险和仓位；超限则减仓/退出。后者可以重算距离，但必须显式采用对应策略。不要通过移动目标保证每次成交 RR 看起来合格。

### F10 · P2 · 扫描器确认信号使用未闭合 K 线

位置：[fetchKlines](/Users/zhangyun/workspace/nofx/market/breakout/binance.go:267)、[Analyze](/Users/zhangyun/workspace/nofx/market/breakout/breakout.go:175)、[AnalyzeShort](/Users/zhangyun/workspace/nofx/market/breakout/shortscan.go:158)。证据：数据解析至消费者调用链。

扫描数据保留 API 返回的最后一根 K 线，没有利用 closeTime 或 `OpenTime+duration` 排除形成中的柱。做空算法注释称“last 3 closed 1h candles”，实际直接取尾部三根；RSI、EMA、影线、成交量和部分顶部确认因此可在收盘前变化。内核 [computeTFSignal](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:2222) 则采用闭合柱，两套信号口径不一致。

盘中信号可以用于预警，但 `Confirmed` 与硬门控应明确只使用已闭合数据。建议同时记录 `observed_at`、`bar_close_at`、`confirmed_at`，在线与回放使用同一 as-of 规则；为“盘中突破、收盘回落”增加集成测试。

### F11 · P2 · 回测合成 1h 柱未按交易所时钟对齐

位置：[resample1h](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:301)。证据：`TestAudit05ResampleMustAlignToExchangeHours`。

函数从返回数组第零项起每四根聚合，不按 UTC 整点分组。如果 15m 数据从 00:15 开始，合成小时变成 00:15–01:15、01:15–02:15；线上交易所小时柱为整点周期。滚动获取 1400 根数据时，起始相位会随获取时刻变化。

应以时间戳对齐，验证四根连续且已闭合；丢弃首尾残缺桶，或直接使用切片到信号时刻的真实小时数据。也应把 `BTSignal.Time` 统一为信号可用的收盘时刻；当前使用最终 close 建模，却记录该柱 openTime，时间留出和市场状态查询会出现口径偏移。

### F12 · P2 · 参数保存失败仍改内存，且未向调度器返回失败

位置：[ApplyParamsChecked](/Users/zhangyun/workspace/nofx/market/breakout/params.go:151)、[tuneWalkForward 保存处理](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:508)、[TuneFromBacktest 返回](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:406)。证据：`TestAudit05PersistFailureMustKeepLiveParams`；错误传播为静态验证。

`ApplyParamsChecked` 先写 `currentParams` 再保存文件，保存失败时生产内存已经改变；重启会恢复旧文件。`tuneWalkForward` 只把错误追加到 `Changes` 文本，外层仍返回 `nil` error，调度器仍按成功处理，可能写入周级成功标记。

应先验证和持久化新版本，再原子发布；失败必须保持原内存参数并返回结构化 error。版本中同时记录数据窗、评估指标、基准参数、变更、状态和回滚目标；不要让 `ApplyParams` 忽略持久化错误后继续日志宣告更新成功。

### F13 · P2 · 自动回测目标与真实交易策略不一致

位置：[RunBacktest](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:113)、[验证准入](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:474)、[当前成交量选池](/Users/zhangyun/workspace/nofx/market/breakout/scheduler.go:237)。证据：评估方法审查。

当前优化的是扫描信号后的 24h 方向收益，统一减 0.20% 往返成本。没有真实限价成交、结构锚点、SL/TP 触发顺序、部分成交、R 倍数退出、资金费率和账户并发约束。信号“24h 盈利”可能实际先止损，也可能限价从未成交。

留出验证只要求候选阈值收益为正且高于测试集总体均值，没有要求超过**现行阈值**。候选 0.6%、总体 0.4%、现行阈值 1.2% 的情形仍可能发布劣化参数。1400 根 15m 约 14.6 天；扣除 260 根预热和 96 根未来标签后，可评分时段约 10.9 天，每 45 分钟产生的 24h 标签高度重叠。当前成交量前 30 选历史池还引入当下选池偏差。

已有时间切分、24h purge、固定低分采集底线和冻结不可重放的评分中心，是合理改进；它们不构成对上述交易目标错位的解决。应使用真实执行回放，并在同一留出窗比较候选与现行参数的净 R、回撤和成交率。线上与回放的 BTC 状态分类也要共用函数：回放 [btcRegimeTimeline](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:234) 的 bear 要求 RSI≤40、较短预热，与在线共享分类仍有差异。

### F14 · P2 · 做空调权的“24h 标签”可能实际是数日后价格

位置：[RunShortTuner 成熟样本计算](/Users/zhangyun/workspace/nofx/market/breakout/shorttuner.go:231)。证据：标签计算调用链。

样本只检查年龄达到 24h，随后用本次 ticker 当前价格计算收益，没有查询 `TS+24h` 的历史价格。重启停机或请求失败后补评，可能将 48h/72h 结果当成 24h 标签。也没有扣交易成本和资金费；无报价样本直接排除，存在缺失结果与市场退市相关的选择偏差。

应固定标签终点，从历史已闭合数据取价并记录实际时间、缺失原因、成本假设。退市或缺失结果不能伪造为零，也不应不加说明地删除：单独报告其占比并采用可追溯的保守估计/缺失处理。否则即使暂时不调权，累积日志也不适合作为未来训练基准。

### F15 · P2 · 弱相关被命名为统计显著

位置：[调权阈值与算法](/Users/zhangyun/workspace/nofx/market/breakout/shorttuner.go:311)。证据：`TestAudit05WeakCorrelationMustNotBeCalledSignificant`。当前短权重调优默认关闭，风险主要在手动启用及解释这些结果时。

门槛是每因子 `n≥30` 且 `|r|≥0.15`，这只是效应大小筛选，不是显著性检验。按独立样本 Pearson 检验，`t=r×sqrt((n−2)/(1−r²))`；n30/r0.15 的 t 约 0.80，复现 n30/r0.16 约 0.86，远不足以支持通常 5% 双侧显著。实际小时采样与 24h 标签高度重叠，有效样本量更低，并且同时搜索九个因子。

建议用按时间/标的分块的置信区间、稳定性与多重比较控制，分市场状态训练并在独立时间段验证；只在净交易指标改善时发布。先 clamp 后整体归一化也不能严格保证最终各权重仍在 `[0.03,0.30]`，需要有边界的单纯形投影。增量游标已避免反复使用同一累计样本更新，是应保留的保护。

### F16 · P2 · 复盘硬规则接受未实现字段，随后静默不执行

位置：[ParseRuleCondition](/Users/zhangyun/workspace/nofx/kernel/rule_engine.go:44)、[评估器默认分支](/Users/zhangyun/workspace/nofx/kernel/rule_engine.go:194)。证据：`TestAudit05UnknownRuleFieldsMustBeRejected`。

解析器只检查字段非空与 operator 属于名单，不检查字段白名单、值类型和字段/operator 兼容性。`{"field":"funding_rate","op":">","value":0.03}` 可以解析成功，但评估器没有该字段，结果永不触发。AI 或用户创建的 hard/block 规则因此可能表现为“已启用”却不起作用。

应在保存前严格校验 schema，未知字段直接拒绝；数值、字符串、布尔的合法 operator 分别定义。复盘提案应返回被拒绝的具体原因和 saved/rejected 清单，而不是只报保存数量。当前规则提取与应用是两个 API、应用需用户操作，属于辅助复盘流程，不能称为自动验证策略进化。

## 3. 五个重点的方案评价

### 3.1 做多与做空候选筛选

实际入口：[GetCandidateCoins](/Users/zhangyun/workspace/nofx/kernel/engine.go:701)。来源包含 static、AI500、OI top/low、piggy_dash、short_scan、Hyperliquid 和 mixed。mixed 已按标的去重、保留来源与部分扫描属性；但不同来源分数并非同一统计尺度。

**做多/趋势扫描。** [Analyze](/Users/zhangyun/workspace/nofx/market/breakout/breakout.go:175) 双向计算突破/跌破，组合 1h/15m、量价与衍生品信息，并对拥挤与过度延伸折扣；[getPiggyDashCoins](/Users/zhangyun/workspace/nofx/kernel/engine.go:1046) 有 12 分钟新鲜度、weak+ 和实际穿越形态底线。这些机制比单纯涨幅榜更合理。尚需解决：形成中柱用于确认、与内核周期差异、不同周期重复动量证据被当成独立共识，以及按扫描分数排序忽略可成交入场位及扣成本后的空间。

**做空扫描。** [AnalyzeShort](/Users/zhangyun/workspace/nofx/market/breakout/shortscan.go:158) 使用九因子：涨幅异常、超买、上影拒绝、量能衰退、均线延伸、拥挤、加速、背离和结构，覆盖涨幅池、历史池、慢顶与下跌延续。未确认 strong 降级以及强动量折扣有价值。[候选池](/Users/zhangyun/workspace/nofx/kernel/engine.go:1127) 在 top-N 前过滤已知低 OI，并对高流动性慢顶设置例外及保留席位。

仍有三点需要分别检验：

1. `Confirmed` 是价格背离/假突破/均线破坏/**资金费回落**的 OR；资金费回落本身不证明价格顶部。对顶部反转空单，应以闭合柱的价格结构破坏为必要条件，资金费和 OI 作为背景，而下跌延续空单走独立规则。
2. OI 未知时允许入池并提示，属于明确的可用性取舍，但不能视为通过流动性核验。开仓前仍需直接价差、深度和冲击成本校验；缺少关键流动性数据时限制新风险。
3. 日线逆势禁空、BTC 牛市禁空等并非所有配置默认开启；不能把提示词中的交易偏好当成代码已经强制执行。已开启的规则则必须修复 F03，做到路径一致。

建议把选币分成“可交易性 → 信号家族 → 可执行交易计划 → 排名”四层。分别评估 long breakout-retest、short breakdown-retest、short topping-reversal、range mean-reversion。排序目标应至少包含净预期 R、限价成交率和组合新增风险，而不是强行要求多空数量均衡。标的历史低胜率硬禁开当前只需 5 笔且低于 35%，见 [POOR_HISTORY](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:1874)；建议增加样本置信度、有效期和重新评估机制，避免小样本禁开造成后续永远没有新样本。

### 3.2 开仓入场位

普通限价锚点是 `live×(1±offset)`，offset 由 1h ATR 和上下限得出，见 [锚点计算](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:1171)；遇到结构供给/需求障碍会抑制。多头另有 [突破回踩计划](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:1578)，将入场移到已突破阻力位。该设计把“信号强”与“现在追价”分开，是应保留的方向。

空头没有等价的通用跌破反抽结构计划，仍主要用 ATR/百分比反弹锚点。建议为下跌延续建立“旧支持转阻力”的反抽入场；顶部反转采用失败突破或结构破坏后的确认方案，不能只因 RSI 高就挂空。

执行还有三个需要统一的地方：

- AI 期间释放执行锁后只刷新账户，[refreshExecutionAccount](/Users/zhangyun/workspace/nofx/trader/account_execution.go:149) 没有重建候选、市场结构和门控；现价虽会再次获取，原门控及结构可能已过时。应在发送订单前用新 as-of 快照重验计划，尤其发生新柱闭合时。
- [限价锚点越过后的市价 fallback](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:263) 默认开启，直接进入市价执行函数。越过锚点可能是改善价格，也可能是原结构失效；除 SL 已越过检查外，还应验证结构、账户和执行模式，明确记录转换原因。
- 挂单存在时间为 `max(30min, maxCycles×ScanInterval)`，不是按循环计数立即过期；需要把时间失效与价格、市场状态、风控失效同时纳入生命周期。F06 优先修复。

入场计划建议保存：`signal_as_of`、数据源/周期、信号家族、绝对结构锚点、失效位、允许价格区间、最晚成交时间、参数版本。成交后重算净 RR，不能靠平移结构满足 RR。

### 3.3 止损、止盈与持仓退出

[methodStopPlan](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:1375) 是当前主体：加密资产用 15m–4h 结构、1h ATR 缓冲，多头 0.4ATR/空头 0.5ATR；股票代币采用 4h–1d 结构和日 ATR；太近结构向外寻找，而非随意夹紧到噪声底线。方向错误、过宽、过近及几何 RR 均有校验。这一基础方法有可解释性。

[scanRRWindow](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:1999) 给出结构目标菜单，布林带可显示但不作为正式方法目标；布林带也被明确排除于结构止损。现状不能称为已经完成 ATR+布林复合止盈止损。

需要改善：

- 目标算法可跳过 RR 不足的较近结构而选更远目标。远目标的几何 RR 合格，不代表前方阻力消失；应展示第一个障碍与到目标路径，并以实际命中概率验证，不应为了达标不断向远处找 TP。
- [checkRR](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:773) 只使用价差；手续费、点差、退出滑点和持仓资金费未统一进入净 RR 与风险仓位计算。“移动到入场价”也不是保证净保本。
- 当前有结构 TP、ATR trailing、R 倍数锁盈/减仓、ROE 峰值回撤以及 quick 时间退出。已持久化初始 R/减仓状态，并区分 trend/range/quick，是优点；仍应明确各规则优先级和累计减仓上限，让同一持仓由一个退出状态机维护目标数量和保护状态。
- 开仓按结构止损距离定仓位，后续 [vol-target](/Users/zhangyun/workspace/nofx/trader/auto_trader_vol.go:288) 则按 ATR 定目标名义金额，两者代表不同风险尺度。宽结构止损时可能持仓远低于波动目标，较窄结构止损时又可能很快触发减仓；应明确波动预算与止损预算双重上限，并在真实退出回放中共同验证。
- 先完成 F04/F05/F07/F08/F09，再验证止损参数。保护执行不可靠时，回测中的“风险 1R”不能代表真实最大损失。

### 3.4 风险控制的完备性

已有单笔按止损距离缩仓、最大持仓/杠杆/保证金、最低 RR、ATR 底线/过宽止损、日内损失、账户亏损、累计止损风险、净方向风险、挂单风险预留、连续亏损和数据质量门控。[账户风险汇总](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:1988) 会计入账户持仓和剩余挂单，循环内预留也防止多订单各自占满预算。

完备性缺口集中在这些边界：

| 风险维度 | 当前局限 | 建议 |
| --- | --- | --- |
| 保护真实性 | 存在单不等于足额、有效保护；适配器回读有差异 | 统一读写契约，补挂后回读，止损失效立即进入恢复/退出状态 |
| 账户风险快照 | AI 后刷新沿用此前持仓行的 StopLossPrice 等字段 | 同时刷新保护覆盖和实际止损，避免使用陈旧风险距离 |
| 无止损风险 | 固定按 8% 距离估算，不是真实损失上界 | 将无保护视为故障并限制新风险，增加跳空/强平压力情景 |
| 组合分散 | `abs(longStopRisk-shortStopRisk)` 只是方向金额差 | 保留 gross cap；另按相关性/BTC beta/资产类别做压力损失，不能把不相关多空当作天然对冲 |
| 账户回撤 | AI 路径相对 initialBalance；不是净值高水位回撤 | 分开命名“初始本金亏损熔断”和“峰值回撤熔断”，高水位持久化并处理出入金 |
| 日内熔断 | 达阈值后拒新决策但未撤存量单；净值恢复后可再次放行，与文字“until next UTC day”不同 | 明确是否锁存到日终，持久化 halt 状态并撤增加风险挂单 |
| 强平边界 | 近似杠杆距离不足以覆盖真实保证金模式/维持保证金档位 | 优先实际 liquidationPrice、保证金档位、标记价和维护保证金安全垫 |
| 数据失败 | 部分行情、价差、连续亏损或保护读数失败采取放行/跳过 | 对关键风控 unknown 禁止加风险，继续允许减仓和保护恢复 |
| 参数合法性 | [Validate](/Users/zhangyun/workspace/nofx/store/strategy_validation.go:11) 未全面限制 risk_per_trade、账户/日损阈值及退出参数关系 | 保留明确的负值关闭语义，但校验上界、量纲、阈值顺序和组合兼容性 |
| 成交到保护窗口 | 挂单通过约 30s 监控补保护，网络调用和执行锁还可延长窗口 | 优先原生 bracket/交易所成交事件；保留轮询作为恢复，对保护超时有明确退出策略 |

### 3.5 自动进化的准确性和理性

当前是三个不同机制，必须分开评价：

| 机制 | 实际行为 | 审查意见 |
| --- | --- | --- |
| 突破扫描器周级调参 | 当前成交量池回放，调 strong/medium 阈值，70/30 时间留出且 purge 24h 标签交叉 | 可作信号研究；F11/F12/F13 修复前不应视作真实交易优化 |
| 做空九因子在线调权 | 小时抽样，成熟标签，增量相关性更新；默认关闭 | 默认关闭合理；F14/F15 修复后仍需要独立留出、现行权重比较和发布回滚 |
| AI 复盘规则 | 最近最多 50 个已复盘日志产生提案，另一个 API 保存用户选定规则 | 属于人工监督的规则建议；需 F16 schema 校验及反事实回测，不能从少量亏损叙事推断普适规则 |

参数不是完全按策略隔离：[TunableParams](/Users/zhangyun/workspace/nofx/market/breakout/params.go:16) 是进程共享状态，策略配置版本记录并不能自动覆盖扫描器参数版本。应把每笔信号和交易关联到确切的 scorer/execution/risk 版本，否则事后收益改善无法归因。自动变更应有候选版本、验证版本、上线版本与回滚版本，不直接覆盖全局现行参数。

## 4. ATR + 布林线复合方案

**可以试验，建议沿用结构 + ATR，按市场状态增加布林线，而不是统一把上下轨当成买卖和止损位。** ATR 表示波动尺度、不提供方向，适合缓冲和风险归一化。[Fidelity ATR 说明](https://www.fidelity.com/learning-center/trading-investing/technical-analysis/technical-indicator-guide/atr)。布林带触轨本身不是买卖信号，趋势中价格会沿轨运行，20 周期/2 标准差只是默认值。[John Bollinger 使用规则](https://www.bollingerbands.com/bollinger-band-rules)。以下公式是建议验证的设计，尚无本仓库交易回测证明其优于现行方案。

### 4.1 数据和市场状态

加密资产可用已闭合 1h ATR14 与同周期 BB20/2 作为首个比较基准，15m 确认入场、4h 判断趋势环境；股票代币保留现有日波动尺度，不能混用日 ATR 与分钟布林轨道。

复用现有 [market_regime](/Users/zhangyun/workspace/nofx/kernel/market_regime.go:1) 的 EMA、ADX、ATR/BBWidth 分位及闭合柱一致性。布林与 ATR 的两个波动量相关，不宜把“双指标同意”直接解释为两个独立证据。信号时刻冻结入场 BB、ATR、结构和初始 R，避免成交后用新轨道扩大风险。

### 4.2 趋势策略

| 项目 | 多头 | 空头 |
| --- | --- | --- |
| 入场 | 已确认突破后，旧阻力回踩转支持；确认成交量与结构仍成立 | 已确认跌破后，旧支持反抽转阻力；顶部反转另用专门模型 |
| 初始止损 | `结构支持低点 − c×ATR` | `结构阻力高点 + c×ATR` |
| 布林作用 | 中轨斜率、带宽扩张、沿上轨运行用于状态和持有判断 | 中轨斜率、带宽扩张、沿下轨运行用于状态和持有判断 |
| 初始目标 | 下一结构障碍/结构目标，净 RR 合格才入场 | 下一结构障碍/结构目标，净 RR 合格才入场 |
| 后续止损 | 可验证 `max(原SL, 已闭合最高价−k×ATR)` 的单调推进 | 可验证 `min(原SL, 已闭合最低价+k×ATR)` 的单调推进 |

沿轨趋势不宜自动在第一次触及外轨时全部止盈。中轨可作为经确认后的退出/收紧条件，但不能在带宽扩张时把初始止损向不利方向推远。`c`、`k` 及部分止盈比例需要按净 R 和回撤选取；当前 0.4/0.5 缓冲与 2ATR trailing 可作为基准，不代表已优化参数。

### 4.3 震荡回归策略

只在状态判为 range、没有有效趋势突破时使用：多头等待下轨外试探后重新收回带内、支持位守住；空头等待上轨外试探失败、重新收回带内。单纯触轨不能入场。

- 多头候选 SL：`min(结构低点, 入场快照下轨) − c×ATR`。
- 空头候选 SL：`max(结构高点, 入场快照上轨) + c×ATR`。
- 初始 TP：入场快照中轨；只有路径与净 RR 经验证时，才用对侧轨或结构目标。中轨空间不足则跳过，不能为凑 RR 强行改成远目标。
- 触发带宽扩张/有效突破后使剩余入场挂单失效；持仓按预先定义的退出规则管理，不能临时扩大止损等均值回归。

### 4.4 用实际成交价统一净 RR 与仓位

设实际成交价 E、止损 S、目标 T，`C_loss`/`C_win` 是按基础资产单位估计的各自手续费、价差/滑点及持仓成本，则：

```text
单位预计风险 = |E−S| + C_loss
单位净目标收益 = |T−E| − C_win
净RR = 单位净目标收益 / 单位预计风险
数量Q ≤ 权益 × 单笔风险比例 / 单位预计风险
```

成本在各自结果中只计一次，使用 maker/taker、实际资金费周期与预估持仓时长；资金费可为收入但不应无条件按乐观值计入。还需再限制名义敞口、保证金、单标的和账户 gross/stress 风险。止损委托不能保证跳空中的成交损失等于上述预计风险，因此另需压力测试。

结构 S/T 固定，不随 fill 平移。成交后净 RR 不合格时按执行策略减仓/退出，初始 R 固定并持久化。所有后续移动止损只能收紧；TP/SL 修改必须回读验证数量和类型。

### 4.5 必须完成的对照验证

至少比较 A“现行结构+ATR”、B“结构+ATR+布林状态/退出”、C“独立震荡布林+ATR”，按趋势/震荡、多/空、币种/股票代币分组。使用相同时间窗、相同资产池、相同成本和风险预算，报告净期望 R、最大回撤、尾部损失、成交率、换手、资金费、分阶段稳定性。没有统计和执行优势时保留 A。

## 5. 自动进化的建议验收标准

1. **数据一致。** 真实 UTC 周期、已闭合柱、历史 as-of 资产池、历史 funding/OI、正确标签终点和数据版本。无法重放的特征明确标记，不用不同评分器的历史收益验证线上评分器。
2. **执行一致。** 共用入场和风险计划逻辑，回放限价可成交性、部分成交、价格精度与最小数量、保护延迟、SL/TP。同一柱内既触 SL 又触 TP 时使用更细数据，缺少细数据采用保守顺序并报告不确定性。
3. **目标一致。** 真实净 R/净收益和回撤约束优先；24h forward return 保留为信号诊断指标，不作为真实交易胜率。
4. **验证独立。** 多个滚动训练/验证/最终留出窗口，purge 标签与持仓交叉；按时间和标的分块估计置信区间，报告搜索次数，禁止反复针对同一最终留出窗改方案。
5. **比较现行版本。** 候选必须在同一留出样本、同一风险预算下改善现行版本，并满足尾部风险约束；“好于全集均值”不足以发布。
6. **发布可恢复。** 保存候选与当前版本、可解释变更、数据窗与指标；先离线/模拟影子验证，再受限发布。触发超限、保护错误或显著退化能自动回滚，同时停止增加风险。持久化失败不算成功。
7. **可归因。** 信号、挂单、成交、规则和退出日志都带参数/策略版本，区分评分器漂移、市场状态变更和执行成本变化。

## 6. 验证记录与边界

现有回归测试通过：

```text
go test ./kernel ./market/... ./trader/... ./store/... ./api ./manager
```

日志：[baseline-output.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-05/evidence/baseline-output.txt)。部分包无测试文件，日志如实保留；本次未做真实交易所集成、生产配置/账户审计、实时行情回放或收益证明。

为审查临时编写并执行的 10 个用例均在现有实现上触发预期失败；不是生产代码改动造成的新回归。其断言检查应达到的行为，失败用于证明当前缺口。

| 用例 | 对应发现 |
| --- | --- |
| AdverseSlippageDirection | F01 |
| ShortBansMustReachMarketExecution | F03 |
| DailyUptrendBanMustCoverLimitShort | F03 |
| WatchdogMustRepairInsufficientSLCoverage | F04 |
| CrossedMissingStopMustExit | F05 |
| DailyHaltMustCancelPendingRisk | F06 |
| UnknownRuleFieldsMustBeRejected | F16 |
| ResampleMustAlignToExchangeHours | F11 |
| PersistFailureMustKeepLiveParams | F12 |
| WeakCorrelationMustNotBeCalledSignificant | F15 |

临时 Go 测试已从源码目录移除，保存在 evidence 下的 `.go.txt`，避免改变常规测试结果。复现方法：[证据说明](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-05/evidence/README.md)。原始输出：[reproduction-output.txt](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-05/evidence/reproduction-output.txt)。

F02/F07/F08/F09/F10/F14 为静态路径/数据语义审查，F13 为验证方法缺口；它们的实际发生频率取决于生产配置、适配器、行情与数据失败情况。报告不推断当前账户已经发生损失。参数倍数与组合公式是待验证方案，不是已经测得的盈利结论。

## 7. 修复工作包与完成条件

| 顺序 | 工作包 | 完成条件 |
| --- | --- | --- |
| 1 | 成交与保护：F01/F04/F05/F07/F08 | 不利滑点方向正确；保护足额且可回读；失效/失败有确定性恢复或退出；各适配器契约测试通过 |
| 2 | 风控路径与数据：F02/F03/F06/F10/F16 | 同一禁开在所有动作生效；熔断撤余单；所有确认来自闭合柱；规则 schema 严格校验 |
| 3 | 结构与执行计划：F09，刷新、fallback、净 RR | 实际成交不篡改结构；用实际成本和数量复核风险；挂单有完整失效状态机 |
| 4 | 回放与发布：F11/F12/F13/F14/F15 | 线上/回放同周期同标签；对照现行版本；统计与回撤约束；版本提交原子且可回滚 |
| 5 | ATR/布林组合实验 | 三组对照在多状态留出窗中完成；报告净 R/回撤/成交率；只发布有证据的改进 |
