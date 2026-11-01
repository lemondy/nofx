# 量化系统全面审查（2026-10-28）

范围：数据源/指标/候选排序、LLM prompt 与决策解析、入场与止盈止损执行、风控、回测、因子挖掘、前端。
方式：六路只读审查并行（分工见文末），主审逐条回到代码核对。下文“已核实”表示主审亲自读代码确认过；其余结论来自子审查的离线复现（`/tmp` overlay 测试、只读生产库查询），未在实盘验证。
原始分报告：[raw/](raw/)。

---

## 一、总体判断

1. **架构方向是对的。** 程序预计算 stop_plan / rr_scan / tp_options，再让模型做选择题；执行侧把 SL/TP 吸附回程序计划，并做仓位下压、候选池校验和硬门。2026-10-01 以来 867 条开仓决策里，没有一条违反 `allowed=false`。研究侧也有冻结评估器、h+2 purge、holdout 回执、预注册，这套纪律在自研系统里算好的。
2. **主要风险在“状态一致性”，不在策略公式。** 仓位数量一旦变化（1R 减仓、波动率减仓、部分成交、超时），保护单、内存标记和风险口径没有统一重算。多条 P0/P1 都出在这里。
3. **风控有几处 fail-open。** 数据库或行情查询失败时，好几个闸门把“未知”当成“安全”放行。另外，网格单和执行时重新取价这两条路径绕过了账户级组合风险闸。
4. **同一个量在不同模块有不同口径。** ATR 至少有 5 份实现，EMA 在截断后的展示窗口上算，“4h 涨幅”“60m 涨幅”实际窗口随相位漂移，OI 的 15m 变化其实取自小时数据。模型读到的数和硬门用的数可能不是同一个。
5. **最关键的战略问题还没有答案：LLM 到底有没有增量价值。** 已知 PF≈1.0 / NO_EDGE（见 2026-10 回测结论）。当前只记录被拦截方向的影子结果；放行集合里被 LLM 否决的 90.9% 没有后验结果，所以无法回答“LLM 挑选是否优于按 directional_score 机械选”。
6. **研究结论的可信度被两处数据口径削弱。** 一是 crypto 因子币池是 2026-10-18 的存活快照（幸存者偏差）；二是多空收益没算资金费。现有 41 个 accepted 因子大多是“高波动/低位”类，正好是幸存者偏差最会抬高的类型。

---

## 二、按优先级排列的问题清单

严重度：**P0** 可能直接造成资金损失或错误下单；**P1** 结果错误、风控失效或研究结论失真；**P2** 次要。下文严重度是主审校准后的结果，可能与分报告不同。

### P0

| # | 模块 | 问题 | 位置 | 状态 |
|---|---|---|---|---|
| 1 | 执行 | **“止损只紧不松”只比较内存值。** 用户在交易所手动收紧的 SL 会被程序撤掉，换成更松的价位（`-4130` 后撤旧挂新，且 `originals` 不区分订单归属）。账户上有手动交易，所以这个场景是现实的。 | `trader/auto_trader_vol.go:69`、`trader/binance/futures_orders.go:1292` | 子审查复现 |
| 2 | 风控 | **网格新增订单绕过账户总风险 / 方向风险闸**，只检查网格自身额度和保证金。与其他策略共用账户时会超过账户风险上限。（仅在启用网格时成立） | `trader/auto_trader_grid_orders.go:170` | 子审查复现 |

### P1：执行与风控（实盘相关，建议优先修）

| # | 问题 | 位置 | 状态 |
|---|---|---|---|
| 3 | 1R 减仓 50% 后，TP 算法单数量没有缩。TP 触发时把 runner 整个平掉，违反 prompt 里“按触发时剩余仓位”的约定（`engine_prompt.go:406`）。看门狗用 `enoughProtection(>=)` 判断，也不会把单子缩回去。 | `auto_trader_orders.go:759`、`auto_trader_vol.go:505`、`auto_trader_risk.go:1798` | 已核实 |
| 4 | 波动率减仓后，同一轮的 `processRProfitRules` 仍用减仓前的 `pos` 快照，可能再减 50%，等于全平。 | `auto_trader_vol.go:351-363` | 已核实 |
| 5 | split TP 一挂出就标记 `tpRunnerDone`。之后单子被撤或失效，看门狗不会补挂。 | `auto_trader_orders.go:794`、`auto_trader_risk.go:1798` | 已核实 |
| 6 | TP 补挂的尾差小于 stepSize 时，`FormatQuantity` 得到 0，退化成 `ClosePosition(true)`，部分止盈变成全仓止盈。`FormatQuantity` 的其他错误也走这条分支。 | `binance/futures_orders.go:1385`、`auto_trader_orders.go:850` | 已核实 |
| 7 | 平仓或减仓请求超时后没有 UNKNOWN 查回（入场有）。下一轮会再减一次，最终减掉 75% 而不是 50%。 | `binance/futures_orders.go:335`、`auto_trader_vol.go:505` | 子审查推演 |
| 8 | 成交后的风险复核用的是未按 tick 舍入的 SL，粗 tick 的小价币实际风险可能超预算 5–10%。 | `auto_trader_fill_risk.go:23` | 已核实（量级取决于 tick） |
| 9 | 执行器重新取价后只复核单笔预算，不复核账户总风险、方向风险。 | `auto_trader_risk.go:2172`、`auto_trader_orders.go:221-354` | 子审查复现 |
| 10 | 连亏封禁到期时间取 `positions[maxLosses-1]`。排序是最新在前，这一条是三笔亏损里最早的一笔，和注释说的“触发笔”相反，所以封禁会提前结束；旧亏损滑出 24h 窗口后，封禁也会直接解除。 | `auto_trader_lossstreak.go:77` | 已核实 |
| 11 | `isBTCETH` 用前缀判断，ETHFI、BTCDOM 等会拿到主流币的仓位上限。 | `auto_trader_risk.go:538` | 已核实 |
| 12 | fail-open 集合：连亏查询失败时放行（:130）；日熔断锁存读取失败时当作“未熔断”（:2525）；AI 归属查询失败时被当成手动仓，看门狗跳过止损穿越检查（`store/ai_managed.go:112`）；平仓闸在行情缺失时**阻止减仓**（:1178）。 | 见左 | 子审查复现 |
| 13 | 市价默认模式下，`EXTENDED_PUMP_UNCONFIRMED` 在执行侧没有镜像拦截，只靠 prompt。目前 0 次违规，属于纵深防御缺口。 | `auto_trader_risk.go:2552`、`auto_trader_orders.go:136` | 子审查推演 |

### P1：数据与 LLM 层

| # | 问题 | 位置 | 状态 |
|---|---|---|---|
| 14 | **429/500 不在 LLM 重试列表里。** 10-27 全天 1326 个周期中 323 个（24%）首次 429 就放弃，连续 3 次失败进 safeMode。退避是线性的，没有抖动，也不读 `Retry-After`。 | `mcp/client.go:29-44` | 已核实 |
| 15 | 决策 JSON 解析是“全有或全无”：`leverage:3.0`、`+1.1`、`"3"`，或 reasoning 里的中文引号，都会让整个数组作废，持仓管理决策一起丢失。 | `kernel/engine_analysis.go:550-680` | 子审查复现 |
| 16 | 出现多个 `<decision>` 块时取**第一个**，推理里的草稿可能被当成最终决策；content 为空时还会退回去解析 reasoning。 | `engine_analysis.go:557` | 子审查复现 |
| 17 | 信号层的 EMA 在截断后的展示窗口（约 59 根）上重算，EMA50 只递推了 9 步。趋势分类会随“展示条数”配置变化，同一信号块里的 `ema_slow` 与 `regime_line.ema50` 有 4% 的上下关系相反。 | `kernel/signal_layer.go:2573`、`market/data_klines.go:184` | 已核实 |
| 18 | ATR 至少有 5 份实现（market、signal_layer 的标量与序列、breakout、stockengine），种子不同：`wilderATR` 把 tr[0]=H−L 算进去。同一组 K 线算出的止损缓冲不一样。 | `signal_layer.go:2885/2910`、`market/data_indicators.go:89` | 已核实 |
| 19 | “4h 涨幅”取上一根 4h K 线的收盘价，实际窗口在 0–4h 之间漂移。`CHASE_MOVE4H` 门可能漏掉刚发生的暴涨，BTC 行的“4h”也一样。排名层的 4h 实际是“本根开盘至今”。 | `market/data.go:234`、`engine_data_binance.go:132-164` | 已核实 |
| 20 | OI 的 15m 变化用的是**小时**历史数据，再按本地抓取时间重复采样，激增门会漏检。 | `market/data.go:663`、`market/oi_risk.go:95` | 子审查复现 |
| 21 | 排名缓存键里没有 limit 和周期，后来的策略会拿到先来策略的浅层或错周期结果。 | `kernel/engine_data_binance.go:405,492` | 子审查复现 |
| 22 | `bnweight` 的 data/funding 限频桶只统计、不准入。 | `market/bnweight/meter.go:257-366` | 子审查复现 |
| 23 | 执行侧的 `RegimeLine` / `ExecutionMicroTrend` 用执行时钟判断“是否已收盘”，旧快照里正在形成的 K 线会被当成已收盘。 | `kernel/regime_line.go:33,64` | 子审查复现 |
| 24 | MIN_NOTIONAL 换算出的数量没有按 stepSize 向上取整，给出的“最小可交易金额”可能下不了单。 | `market/symbol_lot_filter.go:97` | 子审查复现 |

### P1：研究基础设施

| # | 问题 | 位置 | 状态 |
|---|---|---|---|
| 25 | **crypto 因子币池是 2026-10-18 的存活快照**：2022 年平均只有 34 个币，没有任何退市币。高波动类因子会被系统性抬高。 | `scripts/factor_mining/data_fetch.py:253` | 已核实 |
| 26 | 因子多空净收益没算资金费，成本固定为单边 8bp，并且这个结果直接进入 accepted 判定。 | `scripts/factor_mining/evaluate.py:80` | 已核实 |
| 27 | 晋升 `ready` 只看点估计（净盈亏>0、收益不低于对照、日配对差≥0），已经算好的 CI 没有用上。纯噪声大约 20–30% 的概率也能拿到 `ready`。 | `scripts/promotion/report.py:336` | 已核实 |
| 28 | `ml-eval` 的走步检验没有 purge/embargo，置换检验是 iid 打乱，会误放行。 | `scripts/backtest/mleval.py:69-166` | 子审查推演 |

### P2（摘要，详见分报告）

- Backtrader 适配器：入场那根 K 线内的 SL 被忽略；限价单“触价即成交”（实盘未成交的信号里有 22% 在模拟中成交）；1m→5m 拼接最多丢 4 分钟。三处都朝乐观方向偏。
- `breakout/backtest.go` 用信号那根 K 线的收盘价作入场价，没算资金费；`shortscan` 的研究样本用旧的小时收盘价当入场价。
- 秩相关没处理并列（`quant_audit` 会误判马丁格尔）；因子 ledger 每次追加都重写整个文件。
- LLM：`reasoning` 字段被移出 contract 后，复盘和告警的理由为空；流式路径不检查 `finish_reason=length`；prompt 与代码对“0”的语义相反。
- 前端：持仓历史的“净盈亏”卡**重复扣了一次手续费**（已核实，`PositionHistory.tsx:604`）；equity 时间戳不带时区，UTC+8 的用户看到的曲线错位 8 小时；小价币在决策卡和 K 线头部显示成 0；“Sharpe”实际是按每笔 USDT 金额算的均值/标准差，没有年化；EquityChart 回退到虚构的 1000 基线；`/api/statistics` 每 30s 轮询但从未渲染。
- 安全：`store/strategy.go:1189`、`provider/nofxos/client.go:19` 硬编码了 NofxOS key；token 存在 localStorage。

---

## 三、哪些可以用成熟方案替代，哪些应保留自写

| 领域 | 建议 | 理由 |
|---|---|---|
| 技术指标（5 份 ATR、多份 EMA/RSI） | **先统一成一份实现**，用 TA-Lib 固定样例做黄金值单测；是否换成 `go-talib`/`cinar/indicator` 是次要的 | 问题在口径分叉和预热长度，不在公式本身。直接换库会让所有阈值整体漂移 |
| 价格/数量量化 | 用仓库已依赖的 `shopspring/decimal`，或用整数 tick/step 生成最终订单计划，再基于这份计划算风险 | 修复第 6、8、24 条这一类问题 |
| LLM 输出解析 | 用各 provider 的结构化输出（`response_format=json_schema` / tool_use），再加逐对象宽松解码 | 可以删掉约 300 行正则修补，整类消除第 15、16 条 |
| 重试退避 | 补全可重试状态码，加指数退避、抖动和 `Retry-After`，再加 provider 级并发信号量；不必引库 | 第 14 条是当前可用性损失最大的一项 |
| 回测撮合 | **保持 Backtrader**（项目政策），在适配器里做三处保守化 | vectorbt 处理部分减仓不自然；nautilus_trader 更真实，但迁移成本高，可作为长期选项 |
| 多变体比较 | `arch.bootstrap.SPA/StepM`（White Reality Check / Hansen SPA），配对差加标准误 | 48 个出场变体目前只有 sumR，没有多重比较 |
| 时序 CV | `sklearn.TimeSeriesSplit(gap=)` 或 `skfolio.CombinatorialPurgedCV`；置换检验改为块置换（`arch` StationaryBootstrap） | 第 28 条 |
| 因子评估核心 | **保留自写**（向量化，每晚 8k+ 表达式），用 statsmodels HAC 和 alphalens 在测试里做一次 oracle 对照；PSR/DSR 约 30 行自写即可 | alphalens 慢一个数量级，评估器被哈希冻结 |
| 表达式 DSL / Alpha158 | **保留自写**（已有 audit_qlib 核对，AST 白名单是 LLM 安全边界） | — |
| 历史币池 | `data.binance.vision`（含已下架合约的 klines/funding）+ `exchangeInfo` 的 onboardDate | 修第 25 条；需要联网确认字段 |
| 绩效指标 | Sharpe/MDD 按权益曲线的日收益计算，口径对齐 empyrical/quantstats；先构造扣除入出金的收益序列 | 现在的“Sharpe”和 MDD 只看已平仓序列 |
| 前端表单 / 格式化 | zod 镜像后端校验；价格统一用 `utils/format.ts` | 后端校验已较完整，这是体验补强 |
| 应保留的自写业务逻辑 | stop_plan / rr_scan / tp 菜单、pubcache、bnweight 优先级、保护单状态机、手动仓归属、预注册封存机制 | 都是本系统特有的交易语义，通用库替代不了 |

---

## 四、优化路线（按预期收益排序）

1. **建立“LLM vs 规则基线”的反事实账本（战略最高优先级）。** 对 `allowed=true` 的方向也写影子记录（带 `llm_action`）。基线是：放行集合按 directional_score 取前 N 个，全部用程序的 stop_plan 和 tp①，走 Backtrader 重放，与 LLM 实际选择做配对比较，用块 bootstrap 给置信区间。在 PF≈1.0 的现状下，这是判断“继续调 prompt”还是“改信号源”的前提。附带一个观察：confidence 和结果没有可用关系（369/388 单落在 70–79），不应用它做仓位或例外判定（`MarketExceptionMinScore=80`）。
2. **统一仓位变更后的保护重算。** 1R 减仓、波动率减仓、AI 部分平仓、TP 成交都走同一个流程：确认实际余仓 → 重算并量化 TP → 核验 SL（以交易所现存的更紧 SL 为下限）→ 持久化状态。平仓同样引入 UNKNOWN 查回。修复第 1、3–7 条。
3. **统一账户准入函数，fail-closed。** 市价、限价、网格三条路径共用一个准入函数，基于最终数量和价格；返回“通过 / 拒绝 / 数据未知”三态，数据未知时禁止加风险，但允许减风险。修复第 2、9、12 条。风险参数数值保持不变。
4. **LLM 基础设施。** 429 重试加并发限流（立刻减少约 24% 的周期丢失）；宽松解码加“取最后一个块”；中期切到结构化输出；记录模型名、token、finish_reason 和解析路径。
5. **数据契约。** 计算用的历史和展示窗口分离（≥200 根），全仓库共用一套指标实现；涨跌幅按目标时间戳计算；OI 改用 5m 或实时数据，并校验源时间戳；排名缓存键补全。建议加一条一致性测试：“改变展示 count 不改变同一时点的信号”。
6. **减轻 LLM 负担、降低成本。** 双向硬拦截的候选和手动仓由程序直接合成 wait/hold；账户级阻断（保证金≥90%、熔断）在调用 LLM 之前就跳过（10-20 以来有 3.6% 的调用是这种空转）；仓位由程序给出 `size_plan_usd`，模型只选满仓、半仓或放弃；先 A/B 关闭原始 ohlcv（占 user prompt 的 32%）。
7. **研究口径修正（一次 bump 评估器版本，一起做完）。** 时点币池（先做“只留 2022 年前上市币”的敏感性对照，成本约半天）、资金费、按流动性分档的成本；晋升闸门改成 CI 下界的非劣效判定，并设最小样本量；出场变体加 SPA 检验。
8. **前端口径。** 所有盈亏聚合由后端给出 net 字段，前端只负责展示；equity 时间戳改为 RFC3339；缺失值显示“—”而不是 0。

---

## 五、审查分工与核实情况

| 子任务 | 执行者 | 主审核实 |
|---|---|---|
| 数据源 / 指标 / 排序 | Codex（gpt-6.1-sol, xhigh） | 已核实 EMA 窗口截断、ATR 种子分叉、4h 窗口漂移；其余为子审查的 overlay 复现 |
| LLM prompt / 解析 | Sonnet 5.5 | 已核实 429 不在重试列表；其他数字来自只读生产库统计（immutable 快照） |
| 入场 / 止盈止损 / 执行 | Codex | 已核实第 3–6、8 条 |
| 风控 | Codex | 已核实第 10、11 条；fail-open 各条为子审查故障注入复现 |
| 回测 / 因子挖掘 | Sonnet 5.5 | 已核实第 25–27 条 |
| 前端 | opencode GLM 5.3 | 已核实净盈亏重复扣费；node_modules 缺失，未跑 tsc/vitest |

限制：本机 Go 1.24.3 低于仓库要求的 1.25.3，整包测试没有跑全，只跑了部分包和 overlay 定向测试；没有联网、没有接触交易所、没有做 race 检测。生产库统计不含 WAL 中最近约 1 天的数据。审查期间子任务的 Go 命令曾改写 `go.mod`，已还原，仓库除本报告目录外没有改动。
