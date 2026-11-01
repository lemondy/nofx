# 研究 / 统计口径核实结果

核实日期：2026-10-28。代码工作目录为 `/Users/zhangyun/workspace/nofx-wt-ver-research`，HEAD `aa8c361d`。生产数据来自 `/Users/zhangyun/workspace/nofx/data/data.db`，通过 SQLite `file:…?mode=ro` 连接读取；未写生产库、研究数据或业务代码。下文代码位置均相对于工作目录。

生产摘录冻结于 **2026-10-28 15:16:31 UTC（北京时间 23:16:31）**。近 14 天窗口为 **10-14 15:16:31 至 10-28 15:16:31 UTC**。摘录包含 592 条持仓（573 CLOSED、19 OPEN）、23,964 个权益快照，以及时间范围内的 9,356 条决策记录。决策查询使用 trader/time 索引、时间上下界及 LIMIT；没有扫描全部历史决策或读取整份日志。

研究文件当前快照：crypto zoo 生成于 `2026-10-27T18:34:49.695103+00:00`，US zoo 生成于 `2026-10-27T18:59:05.905980+00:00`。每条因子试验的计数单位为 **(id, horizon)**，不把相同表达式的不同 horizon 合并计数。

验证材料保存在 [/private/tmp/verify20261028_research](/private/tmp/verify20261028_research)。新增的临时测试仅有：

- `scripts/factor_mining/tests/test_verify20261028_research.py`：5 个 unittest，分别验证摘录/订单匹配、ledger、每日前向 IC、日志提交时刻、历史基线。
- `store/verify20261028_stats_test.go`：2 个测试，验证基线影响、OPEN 排除，以及只读快照上的实际 Go 统计函数耗时。
- `kernel/verify20261028_freshness_test.go`：1 个测试，验证 h=1 在第 3 天仍放行的边界。

上述测试均通过。Python 使用指定 venv；Go 使用 `GOCACHE=$PWD/.gocache`，完成后删除该缓存。未运行已知失败的 trader 包基线测试，未执行 commit / stash / checkout。

## 1. 05 B1：信号 t0 是否早于 LLM 返回

**结论：REFUTED（当前 Binance USDT-M 路径）。** 决策元素的 timestamp 不是周期开始时间。近 14 天精确“提交请求时刻”的完整分布仍为 **INCONCLUSIVE**：库中相关字段实际记录成交时间，不能改名冒充提交时间。

**证据**：

1. `trader/auto_trader_loop.go:173` 调用 LLM 并等待返回；随后经过执行账户刷新、规则检查和风险门禁，到 `:392–407` 才为每个元素设置 `Timestamp: time.Now().UTC()` 并执行。因此此 timestamp 晚于 LLM 返回。`trader/auto_trader_decision.go:46–50` 又在保存整条记录时生成 record timestamp，通常更晚。
2. `scripts/backtest/signals.py:112` 优先使用元素 timestamp，只有缺失/无效时才回退整条记录时间；`:153` 将它作为链的 t0。`exitsim.py:18–39` 对齐到该分钟，并从下一分钟起撮合。没有把 LLM 请求耗时从时间戳中减掉。
3. `test_01_production_snapshot` 按 `(trader_id, entry_order_id/order_id, symbol)` 匹配订单。**Binance 必须通过 trader_fills 桥接**：`trader_orders.exchange_order_id` 在该同步路径存的是 fill/trade ID（`trader/binance/order_sync.go:487`），真正交易所订单 ID 在 `trader_fills.exchange_order_id`（`:510`）。直接连接两个 order_id 字段会错误地得到零匹配。

关键查询形状如下；实际测试逐交易员执行，均有 LIMIT：

```sql
SELECT id,trader_id,timestamp,decisions,ai_request_duration_ms
FROM decision_records
WHERE trader_id=:trader AND timestamp>=:since AND timestamp<=:as_of
LIMIT 100000;

SELECT o.created_at,o.exchange_order_id,f.exchange_order_id AS actual_order_id
FROM trader_orders o JOIN trader_fills f ON f.order_id=o.id
WHERE o.trader_id=:trader AND o.created_at>=:since_ms
LIMIT 100000;
```

分布单位为秒；Δ元素 = `trader_orders.created_at − 元素 timestamp`，Δ记录 = `trader_orders.created_at − record timestamp`：

| 交易员 | 成功开仓 / 匹配数 | Δ元素中位数 | Δ元素 P90 | Δ记录中位数 | Δ记录 P90 |
|---|---:|---:|---:|---:|---:|
| mac-nofx | 40 / 40 | 9.463 | 14.856 | -3.097 | -2.576 |
| 因子 ensemble 候选 | 36 / 36 | 4.381 | 8.147 | -6.832 | -3.260 |
| 因子 ensemble 对照 | 32 / 32 | 4.556 | 9.336 | -7.372 | -4.396 |
| 提前减仓候选 | 17 / 17 | 2.754 | 4.690 | -4.951 | -2.768 |
| 提前减仓对照 | 14 / 14 | 2.278 | 4.122 | -4.612 | -2.549 |

主交易员 record 比元素晚的中位数 / P90 为 **12.795 / 18.074 秒**；同批 AI 请求耗时为 **192.395 / 260.663 秒**。这个量级的 LLM 耗时已发生在元素 timestamp 之前。

**必须区分的限制**：`trader/binance/order_sync.go:482–500` 把 exchange trade execution time 写入 `created_at/filled_at`；所以表中的主交易员 Δ元素是“到首次成交”的延迟。当前日志只覆盖 10-25 晚以来，`test_04_submission_logs` 仅匹配到 1 次主交易员成功开仓的提交成功日志：元素到提交成功日志 **7.945 秒**，提交成功日志到整条记录保存约 **3.190 秒**，且日志仅有秒精度。缺少 14 天的真实请求/ACK 持久化时间，不能给出可靠的 14 天提交分布。

主交易员 40 笔中，9 笔的“下一分钟边界”早于首次成交，说明分钟撮合仍可能忽略数秒执行延迟；这不支持“t0 早于 LLM 返回”的原假设。数据见 [timing.json](/private/tmp/verify20261028_research/timing.json)、[submission_logs.json](/private/tmp/verify20261028_research/submission_logs.json)。

另有范围例外：**股票执行路径**在 `trader/auto_trader_stock_cycle.go:515–517` 冻结周期 now，`:624` 调用 LLM，`:663` 仍把旧 now 放进元素 timestamp。不能把本节对 Binance 的反证推广到股票路径；本次未得到股票订单时间的有效匹配。

**实际影响**：当前主交易员和 paper 币安路径不存在原先怀疑的 LLM 级时间前视；分钟粒度撮合和执行延迟仍会影响离线入场对齐，不改变实盘下单。

**建议修复范围**：持久化 signal_created_at、submit_started_at、submit_ack_at、first_fill_at，分别用于回测与延迟审计；股票路径另行改用 LLM 返回后的元素时间。不要直接把现有成交时间视作提交时间。

## 2. 05 B2：每晚重评估与 K 只计首次试验

**结论：CONFIRMED。** 同一试验每晚重新计算与分类，但 K 不因重评估增加。状态变化很常见；**变化率不是 FWER，也不能全部解释为跨夜重复检验导致的变化**。

**证据**：`test_02_ledger_transitions` 读取当前 ledger 和 zoo：

| 市场 | ledger 唯一试验 | 当前 zoo 保留 | ledger 首次 accepted | 该批当前仍 accepted | 当前全部 accepted |
|---|---:|---:|---:|---:|---:|
| crypto | 24,918 | 9,262 | 308 | **19** | 41 |
| US | 29,988 | 8,490 | 1 | 0 | 2 |

crypto 首次 accepted 的 308 条当前去向：

| 当前状态 | 数量 |
|---|---:|
| accepted | 19 |
| redundant | 213 |
| candidate | 31 |
| rejected | 13 |
| zoo 不再保留 | 32 |

因此 **289/308 = 93.83% 已不在当前 accepted 集合**，其中 32 条没有当前标签；直接可见标签已经改变的是 **257/308 = 83.44%**。原分报告把“308 首次 accepted”与“41 当前 accepted”直接相减会混淆队列：当前 41 条中 **22 条首次是 candidate**，仅 19 条来自原 accepted 队列。

crypto 现存条目中 **2,185/9,262 = 23.59%** 有跨 run 的 status_history；其中 **108 条**保留了 accepted→其他状态的记录，**86 条**保留了其他状态→accepted 的记录。这些是历史记录仅保留最近 5 条时的可观察下界；不是完整历史转移总数。US 当前 2 条 accepted 也都首次为 candidate。

代码链：

- `scripts/factor_mining/miner.py:256–258`：`seen` 来自 ledger，`K = Counter(horizon for ledger)`。
- `:281–316`：每晚刷新保留因子的 train/val 指标，**没有 K++**。
- `:380–388`：新 (h,id) 不在 seen、且指标可评估时，才 `K[h] += 1`。
- `:400–405`：仅首次试验 append ledger；crypto 每个 h 的 K 均为 8,306，US 均为 9,996。
- `:452`、`:559–569`：以当晚窗口和最终 K 重新 classify，再做相关性去重；`:454–458` 将跨 run 状态变化写入 zoo，最多留 5 条。

**口径边界**：ledger 的 accepted 写于首次 classify、同批最终去重之前（`:390` 对比 `:452`）；后来同一 run 内就可能变成 redundant，最终 K 增大也可能改变标签。因此上述 93.83% 是“首次原始标签→当前可用集合”的变化率，**不是纯跨夜变动率或假阳性率**。要准确重建“首次夜末 accepted 后所有跨夜变化”，还缺每晚完整 zoo 快照；当前截断的 status_history 不够。本次未把简化 IC 模拟包装成完整 FWER，也未跑可选的块 bootstrap FWER 实验。

数据见 [ledger.json](/private/tmp/verify20261028_research/ledger.json)。

**实际影响**：影响研究准入与 paper 因子 A/B 的选择口径。生产配置查询显示，`mac-nofx` 的 `indicators.enable_factor_ensemble=0`，仅“因子 ensemble 参考字段-候选”为 1；未发现这些 accepted 直接驱动当前主交易员的因子参考字段。

**建议修复范围**：把首次批末准入、后续监控、重新晋升分开记录，保留不可变的全量状态事件；若允许反复晋升，需明确顺序检验/再消耗验证集的规则，再量化 FWER。

## 3. 05 B3：前向 IC 相关簇及 h=7 功效

**结论：CONFIRMED（冗余及部分功效不足）。** 但“41 个都因天数不足而未确认”被数据否定：20 个已达到 t≥1.65，**41 个前向净多空收益全部为负**。

**证据**：`test_03_forward_ic_clusters` 重新加载日面板和 41 个当前 accepted 因子的表达式，沿用冻结 direction、注册 start、`panel.forward_labels` 及 `cs_rank_ic`。逐条重新算出的 HAC t 与当前 forward.t 在小数点后 7 位一致。相关性使用**完整每日 IC 序列的 Pearson 相关**，不用最多 120 点的累计 curve；41 条共同完整日期为 172 天，相关矩阵按两两共同日期计算。

定义两种聚类，阈值均为 **ρ>0.8，非绝对值**：连通分量允许 A–B、B–C 连边但 A–C 不超过阈值；complete linkage 要求同簇所有成员两两超过阈值。后者更严格，不能把两种簇数混用。

| horizon | accepted 数 | ρ>0.8 的因子对数 | 连通簇数（含单例） | 连通簇大小 | 严格 complete-linkage 簇数 |
|---|---:|---:|---:|---|---:|
| 1 | 5 | 3 | 3 | 3,1,1 | 3 |
| 3 | 13 | 14 | 6 | 6,2,2,1,1,1 | 7 |
| 7 | 23 | 38 | 4 | 19,2,1,1 | 13 |
| 合计 | **41** | **55 / 820 对** | **13** | 其中 6 个多成员簇、7 个单例 | **23** |

所有超过 0.8 的边均在相同 horizon 内。h=7 最大连通簇有 19 个成员，但其最小两两相关只有 **0.377**，所以不能声称“19 个彼此都 >0.8”。该簇按共同日期等权平均每日 IC，均值 **0.03548**、HAC(7) t **1.655**；另三个 h=7 连通簇的 t 分别为 **1.504、2.129、3.867**。这些是事后描述性簇指标，簇数并非精确的独立检验数。

h=7 当前有 23 个因子，样本数 **172–179 天**，t 中位数 **1.514**。假设当前均值和长期方差稳定，使用

`N_required ≈ ceil(N_current × (1.65 / t_current)^2)`

估计未来预期 t 达阈值所需的**总有效日数**：

| h=7 子集 | 数量 | 天数估计 |
|---|---:|---|
| 当前已达 t≥1.65 | 7 | 已达到 t 门槛，仍未通过收益门槛 |
| IC>0 且 0<t<1.65 | 15 | 总天数中位数 **229**，再增加中位数 **57 天**；范围 **173–4,420 天** |
| 所有 IC>0 | 22 | 所需总天数中位数 **204.5（约205）**，范围 **32–4,420 天**；小于现有天数表示已达门槛 |
| IC≤0 | 1 | 沿当前负效应外推，没有有限的正向达标天数 |

例：`f_a601391e/h7` 当前 172 天、t=1.476，需要约 **215 天（再43天）**；`f_da0df7e5/h7` 当前179天、t=0.332，需要约 **4,420天**。这是效应不变时的点估计，**不是达到某个检验功效的保证**；若近似要求 80% 功效，应把 1.65 换成 1.65+0.8416，22 个正效应 h=7 因子的所需总天数中位数约 **464 天**，仍未作多重检验修正。

当前 41 条的前向 IC 均值 **0.03819**，40 条为正；20 条 t≥1.65；**0 条 ls_ret>0**（ls_ret 为年化净多空收益估计）；最终为 0 confirmed、40 inconclusive、1 failed。`forward.py:30–37` 明确同时要求 IC>0、t≥1.65 和 ls_ret>0，因此单纯延长天数不能保证获得 confirmed。

还有时间口径：`forward.py:15–20` 冻结的是当时未读 holdout 的起点。样本可追溯到 4 月，而首次 accepted 距核实时仅 **1–10 天（中位1天）**。172–186 天是历史封存窗口的前向展示样本，不等于注册后积累了这么多实盘日。

材料：[forward.json](/private/tmp/verify20261028_research/forward.json)、[每日 IC](/private/tmp/verify20261028_research/forward_daily_ic.csv)、[相关矩阵](/private/tmp/verify20261028_research/forward_ic_correlations.csv)。

**实际影响**：研究报告若把 41 条当成独立的样本外支持，或把未通过全部解释为“时间不够”，会高估证据强度。目前主要影响因子 paper 候选；主交易员未开启该字段。

**建议修复范围**：预注册簇/ensemble 的校验单位，分别显示 t 门槛与净收益门槛的失败原因；另外单列 accepted 后真正新增的日期与指标。

## 4. 05 B4：R 统计缺失止损的比例与分布

**结论：CONFIRMED。** 缺失明显集中于老历史和主交易员，不能当作随机缺失；但目前四个 paper A/B 交易员没有缺 initial_stop_loss。

**证据**：`report.py:105–123` 在 stop 缺失/≤0、价格无效等情况下跳过 R 样本，但仍将对应盈亏计入 nets。`test_01_production_snapshot` 对全部 CLOSED 摘录调用实际 `trade_statistics`，结果：

- **224/573 = 39.09%** 缺有效 initial_stop_loss。
- R 样本实际为 **348**，排除 **225**：224 条价格/止损无效，另有 us-stock 的 position **387** quantity=0。
- 全量 avg_r 为 **0.051740**，这是保留子集的均值，不能解释成全部 573 笔的平均 R。

按退出时间分周（UTC、周一开周），并列出主交易员的周内缺失：

| 周起点 | 全部 CLOSED | 缺失数 / 比例 | mac-nofx 缺失 / CLOSED |
|---|---:|---:|---:|
| 08-24 | 31 | 31 / 100% | 20 / 20 |
| 08-31 | 47 | 47 / 100% | 44 / 44 |
| 09-07 | 42 | 42 / 100% | 42 / 42 |
| 09-14 | 38 | 20 / 52.63% | 20 / 38 |
| 09-21 | 71 | 9 / 12.68% | 9 / 71 |
| 09-28 | 62 | 13 / 20.97% | 13 / 62 |
| 10-05 | 46 | 20 / 43.48% | 20 / 46 |
| 10-12 | 77 | 34 / 44.16% | 34 / 74 |
| 10-19 | 116 | 7 / 6.03% | 7 / 35 |
| 10-26（未满周） | 43 | 1 / 2.33% | 1 / 2 |

| 交易员 | CLOSED | 缺失数 | 缺失率 |
|---|---:|---:|---:|
| mac-nofx | 434 | 210 | **48.39%** |
| 已不在 traders 表的旧 GLM（id 后缀 1788058432） | 14 | 14 | 100% |
| us-stock | 4 | 0 | 0% |
| 因子 ensemble 候选 / 对照 | 41 / 44 | 0 / 0 | 0% / 0% |
| 提前减仓候选 / 对照 | 19 / 17 | 0 / 0 | 0% / 0% |

224 条缺失中，**137 条在 09-17 前开仓**，但后续时期仍有缺失，并非只有最早导入段。全部缺失行的 source 都是 `sync`，exit_mode 都为空；close_reason 分布为 **sync 157、external 66、netting_reconcile_stale 1**。这支持“旧同步/外部记录及元数据不完整类型集中缺失”，不能仅从数据库标签进一步断言实际是手动还是 AI。

在同一 mac-nofx 交易员内，缺止损子集的平均净金额为 **+0.06823 USDT/笔**，有止损子集仅 **+0.00170 USDT/笔**。两组的净金额分布已经不同；因缺失组没有可核对的初始风险，不能据此算出 avgR 偏差的准确大小或方向。

可复核的 SQL 形状（实际分析从冻结摘录执行）：

```sql
SELECT trader_id,COUNT(*) n,
       SUM(CASE WHEN initial_stop_loss IS NULL OR initial_stop_loss<=0 THEN 1 ELSE 0 END) missing
FROM trader_positions WHERE status='CLOSED'
GROUP BY trader_id LIMIT 100;
```

完整 trader×week、入场日期和类型分布见 [promotion.json](/private/tmp/verify20261028_research/promotion.json)。

**实际影响**：跨时期比较主交易员 avgR 或拿主交易员与 paper 全量 R 比较会受到选择口径影响；当前四个 paper A/B 的这一项缺失率均为零，不能由全库 39.09% 直接推断这些 pair 已存在同等偏差。

**建议修复范围**：报告并列展示总样本、R 有效样本与各类排除比例；跨组比较限定同等覆盖时期。只有能回溯到当时真实计划时才补初始止损，不能用后来的移动止损填充。

## 5. 05 B5：live_scores 新鲜度与版本追踪

**结论：CONFIRMED。** h=1 没有独立的较短 TTL，第 3 个 UTC 日仍可进入参考字段；文件没有 evaluator hash / run_id。不过 as_of 会随字段传给 LLM，不能描述成完全隐藏了数据日期。

**证据**：

- `kernel/factor_ensemble.go:193–198` 仅检查 `asOfDate.Before(today-3天)`，等于第3天时放行；`:234` 对所有 horizon 使用相同结果，没有 h=1 特例。
- `TestVerify20261028DayThreeHorizonOne` 输出：`as_of=2026-10-25 h=1 accepted at 2026-10-28 23:59:59 UTC; rejected at 2026-10-29 00:00:00 UTC`。这是按 UTC 日期划分，不是精确的72小时生成时间 TTL。
- `scripts/factor_mining/live_scores.py:43–44` 写入 version、market、generated_at、as_of、universe_size；没有 evaluator hash 或 run_id。当前文件递归检查也没有 `run_id/evaluator_sha256/evaluator_hash/expr_sha256`。
- 当前实际文件：`as_of=2026-10-26`，`generated_at=2026-10-27T18:53:05.864141+00:00`，version=1；h=1/3/7 分别使用 **5/13/20** 个因子，forward 都是 inconclusive。核实时 as_of 已落后2个 UTC 日，生成时间约20小时前。h=1 在代码规则下还可延续到 10-29。
- 文件 SHA256：`bf180ccd79425b8827786ebb345c0d3ce1d596927a4d8bb745ffb7c05117f1de`。字段和元数据摘录见 [live_file.json](/private/tmp/verify20261028_research/live_file.json)。

`kernel/factor_ensemble.go:226` 保留 AsOf；`kernel/engine_prompt.go:257` 明确它只是参考证据、inconclusive/pending 未被样本外证明，不能绕过 hard gate。不存在“陈旧分数直接替代所有交易门禁”的证据。

**实际影响**：`kernel/signal_layer.go:1283–1284` 仅在 enable 开启时注入。生产库当前仅因子 paper 候选开启，mac-nofx 及其余交易员关闭。因此当前直接暴露于陈旧因子证据的是该 paper 候选，主 Binance 交易员未开启；审计追溯问题仍成立。

**建议修复范围**：按 horizon/最近应有闭合日判定可用性，并显示数据年龄；快照写入 evaluator/expr hash、run_id、数据/宇宙标识，决策记录保存实际使用的快照 hash。

## 6. 05 B7：美股组合回测无滑点及成本量级

**结论：INCONCLUSIVE（合理滑点幅度缺数据）；零滑点现状已 CONFIRMED。**

**证据**：`scripts/backtest/stocks.py:18` 默认 commission=0.001（每侧0.1%，10bp），`:323–324` 将该佣金传给 Backtrader 并固定 `set_slippage_perc(0.0)`。佣金可配置；不能把0.1%说成所有运行永远不变。

检查生产 data 目录中相关文件，找到 `data/cache/bstock_spot_rules.json`，未找到保存的 bid/ask、bookTicker 或订单簿价差序列。规则文件 fetched_at 为 **2026-10-28 20:13:35 +08:00**，包含 PRICE_FILTER/tickSize、LOT_SIZE、NOTIONAL 等规则，**没有可用于估计实际价差、深度或冲击成本的报价**。

为给出可复核的尺度，按 96 个能匹配本地 US daily 数据的标的，用 `tickSize / Yahoo 最新调整收盘价 × 10000` 计算一 tick 的相对价格尺度：中位数 **0.753bp**、P90 **12.224bp**；例如 AAPL **0.291bp**、NVDA **0.463bp**、TSLA **0.255bp**（均使用10-26日收盘参考价及0.01 tick）。材料见 [bstock_tick_scale.json](/private/tmp/verify20261028_research/bstock_tick_scale.json)。

**这些数字只是价格网格尺度，既不是观察到的 bStock 价差，也不是滑点估计。** 本地日线为 Yahoo 来源，`stocks_cache.py:43–44` 会转换为 BUSDT 格式；它不能补充 bStock 真实流动性。缺少实际 bid/ask、订单量、交易时段和成交相对报价，无法据此支持“合理单边滑点就是若干 bp”。

**实际影响**：影响美股组合研究收益和排名的成本可比性；不影响当前 Binance USDT-M 主交易员的实际撮合。本次没有证据量化它是否足以改变某一策略优劣。

**建议修复范围**：保存 bStock 的分时段报价/深度和真实成交基准，再按半价差及订单量估计成本；拿到数据前，研究报告明确标注零滑点，并将非零成本情景标为假设而非实测。

## 7. 06 B1：“同步余额”重置 PnL 基线

**结论：CONFIRMED（功能影响链）。** 最近14天未观察到主交易员运行基线发生变化；缺少字段级审计，不能据此严格证明数据库没有发生过短暂改写。

**证据**：

1. `api/handler_trader_status.go:114–118` 读取交易所 total equity，`:141` 调用 `UpdateInitialBalance(actualBalance)`。`store/trader.go:147–150` 直接 UPDATE initial_balance，没有另存原始基线。
2. handler `:153` 异步 reload；`api/handler_trader_status.go:49–55` 移除旧 trader 再从库加载。`manager/trader_manager.go:780` 和 `trader/auto_trader.go:539` 把配置写入内存 initialBalance。因此新基线对账户接口的影响在 reload 完成后生效，不保证在 HTTP 返回瞬间完成。
3. `trader/auto_trader_decision.go:174–177`：`total_pnl = equity - initialBalance`，比例也用此基线。若同步时 equity=200、旧基线100，当前账户 PnL 从+100变为约0。
4. `web/src/components/charts/EquityChart.tsx:178–192` 对所有历史点重用当前 account.initial_balance，历史 PnL 统一重算为 `point.total_equity - initialBalance`，历史百分比的分母也随之改变。
5. `api/handler_order.go:298` 把 `trader.InitialBalance()` 传给 `GetFullStats`；`store/position_query.go:153–154、:268–280` 以该余额构造累计已实现权益曲线，历史 MDD 百分比随之改变。**不是删除历史交易，也不是将累计已实现净利润清零。**

`TestVerify20261028BaselineAndOpenLoss` 在临时库创建 +10、-20 两笔平仓和一条 OPEN，调用实际 UpdateInitialBalance 与 GetFullStats，关键输出：

```text
same closed net=-10;
MDD initial=100: 18.181818%; initial=200: 9.523810%;
fallback=18.181818%; OPEN row excluded
```

生产库主交易员当前 initial_balance 为 **135.18413468**，traders.updated_at 为 **10-25 11:09:19 UTC**；updated_at 不是“余额改动时间”，不能据此判定基线改过。无字段级变更表/trigger，当前日志自10-25晚以来也未找到 `Synced balance` / `Queried actual exchange balance` 记录。

进一步用 `test_05_baseline_history` 查询同一14日窗口、主交易员的 **3,503** 条决策，只提取提示词中的 `vs initial … USDT` 片段：**3,189 条可提取，全部为135.18**，首条 `id=8528 / 10-14 15:22:52 UTC`，末条 `id=17881 / 10-28 15:14:06 UTC`。`kernel/engine_prompt.go:558–563` 证明该值来自当时运行的 InitialBalanceUSDT（显示精度0.01）。因此未观察到活跃周期中的基线变化；其余314条及两位小数以下/周期间的改写无法由此排除。材料见 [baseline_history.json](/private/tmp/verify20261028_research/baseline_history.json)。

**实际影响**：当前实盘具备此行为，影响账户 PnL、历史图和统计 MDD。另有实际执行层依赖：`trader/auto_trader_risk.go:2209–2212` 的账户回撤门禁也使用 initialBalance；mac-nofx 当前 account_max_drawdown_pct=20，故同步基线还会改变它的阈值。现有证据没有显示最近14天已触发一次基线重置。账户入金在同步前会反映为 equity−旧基线的盈利。

**建议修复范围**：区分原始起始资金、当前对账余额、风控基线和带现金流调整的绩效基线；同步动作记录前后值及有效时间，并明确图表与风险门禁各自使用哪个字段。

## 8. 06 B3：已平仓 MDD 与权益快照 MDD

**结论：CONFIRMED。** GetFullStats 只计 CLOSED 净盈亏，遗漏持仓期间浮亏；initialEquity≤0 时回退100也已复现。全历史原始权益回撤还混有大额转出，不能把它与已平仓口径的全部差额归因于浮亏。

**证据**：`store/position_query.go:91–109` 仅筛选 CLOSED、按 exit_time 排序，`:122–125` 使用 realized_pnl−fee；`:264–265` 将非正初始权益设为100，再按累计净盈亏计算峰谷回撤。`TestVerify20261028BaselineAndOpenLoss` 证明 OPEN 不计入和100回退。

`test_01_production_snapshot` 对 mac-nofx 的 **434 条平仓、11,025 个快照**计算：

| 数据 / 口径 | MDD |
|---|---:|
| 现有 API 平仓序列，配置基线135.18413468 | **32.6466%** |
| 平仓序列改用首个快照权益165.31303361起算 | **27.2514%** |
| 全历史原始 equity 快照 | **80.3683%** |

全历史快照峰值 **265.59095737（08-30 02:43:07 UTC）**，谷值 **52.14006810（09-15 11:41:03 UTC）**。但 `account_income` 的同一 exchange、`account_kind='live_futures'` 记录了 **08-31 转出244.97222755** 等资金流。故80.37%是原始账户权益回撤，不能直接作为策略收益回撤，也不能将80.37−32.65个百分点全部解释为未平仓损失。

再取**最后一笔已记录入金后**的相同窗口以隔离已知现金流：最后一笔为 **10-22 15:37:10 UTC，+100 USDT**；从随后第一个快照 **10-22 15:43:28 UTC** 起，初始权益258.82084750，至10-28 15:11:22 UTC：

| 相同窗口、相同初始权益 | 样本 | MDD |
|---|---:|---:|
| 已平仓净盈亏累计曲线 | 14条平仓 | **2.4484%** |
| total_equity 快照 | 1,351点 | **28.7331%** |

后者峰值 **271.44562551（10-23 00:26:14 UTC）**、谷值 **193.45082195（10-28 14:06:31 UTC）**，回撤金额77.99480356。末快照 balance=269.88335862、unrealized_pnl=**-73.83786741**、equity=196.04549121，直接支持浮亏是当前差距的主要来源；两条曲线还存在资金费、记账覆盖等口径差异，不能把差额精确等同于浮亏。

快照中位间隔 **338.9秒**，P90 **610.8秒**，全历史有17个超过1小时的空档；快照足以显示这一数量级差异，但不是逐 tick 的完整峰谷观测。数据见 [drawdown.json](/private/tmp/verify20261028_research/drawdown.json)。

**实际影响**：当前持仓历史 API/页面的 MDD 确实明显低于最近权益波动。**AI 上下文并非始终只看平仓 MDD**：`trader/auto_trader_loop.go:983–1032` 已在有快照时优先按同一滚动窗口计算权益回撤覆盖该值；账户回撤执行门禁则另用 initialBalance 对比 equity。不能把 API 缺陷概括成“所有实盘风控都忽略浮亏”。

**建议修复范围**：把 closed-trade drawdown 与含浮盈亏的账户权益 drawdown 分开命名；策略绩效回撤需做外部现金流调整，并提供峰谷日期、基线与数据缺口。无合法基线时避免静默使用100。

## 9. 06 B6/B7：统计错误丢弃、全量加载和当前耗时

**结论：CONFIRMED（代码机制）。** 当前数据量及只读快照测量**不支持“已经构成可感知负载”**的推断；这里也不是无条件扫描整个生产数据库。

**证据**：

- `api/handler_order.go:291–295` 会处理历史查询错误；但 `:298、301、304` 对三个统计查询均丢弃 error，`:306` 仍返回 HTTP 200。`GetFullStats/GetSymbolStats/GetDirectionStats` 出错时会返回 nil 与错误，所以前端可收到 stats=null；`PositionHistory.tsx:484、550` 用 `stats &&` 控制卡片，symbol/direction 的 `:346–347` 又回退空数组。统计失败没有对应的错误态。
- `store/position_query.go:107–109、:300–304、:451–455` 的确分别 Find 该交易员**全部 CLOSED 行**，再在 Go 聚合；GetSymbolStats 的 limit=10 只限制最终返回符号数，不限制输入行。
- 这些查询有 `WHERE trader_id=? AND status='CLOSED'`。快照保留生产索引，EXPLAIN 显示 `SEARCH trader_positions USING INDEX idx_positions_trader (trader_id=?)`，排序使用临时 B-tree。因此“每次读取该交易员全量历史”成立，“每次全库无索引全表扫描”不成立。

冻结摘录：全库573条 CLOSED；mac-nofx434，因子候选/对照41/44，减仓候选/对照19/17，us-stock4，旧已删除交易员14。用于计时的 SQLite 摘录含全部592条持仓及原索引，大小 **380,928字节**，生产库本身没有执行性能循环。

`TestVerify20261028ReadOnlySnapshotStats` 以 `mode=ro` 打开这个快照，实际顺序调用三个 Go 函数（包含 GORM 解码及 Go 聚合），关键输出：

```text
first call three stats: 12.049 ms; closed=434 MDD=32.646590%
30 warm calls three stats median=8.008 ms P90=8.448 ms
```

辅助 Python/SQLite 测量执行 count + 三次 SELECT 并取回全部列：首轮 **3.222ms**，后30轮中位数 **2.848ms**、P90 **2.900ms**。二者不是同一计时层，不应混作同一个指标。统计 SQL 使用 LIMIT 100000 防护，远高于592行，没有裁掉样本。材料见 [load.json](/private/tmp/verify20261028_research/load.json)、[/private/tmp/verify20261028_go-tests.log](/private/tmp/verify20261028_go-tests.log)。

`PositionHistory.tsx:339` 30秒轮询时，单页面一次三统计约8ms，折算为本机每30秒约0.027%的串行时间；现有规模很小。该基准未测 HTTP、并发页面或生产写锁争用，不能将8.45ms直接称为生产端点P90，但也没有证据支持当前统计查询已经是可感知瓶颈。

**实际影响**：吞错误是当前真实可达的静默展示缺陷；正常查询时主交易员434条平仓尚未显示出显著负载。随历史增长和同时打开页面数增加，三次全量解码会近似线性增长。

**建议修复范围**：保留部分成功响应时也返回各统计的显式错误状态并记录日志；三个统计合并一次加载，或用 SQL 聚合/短缓存。按实测规模安排性能优化，无需因为目前573笔就先做重型改造。

## 汇总

| 条目 | 结论 | 关键事实 | 对当前实盘的影响 |
|---|---|---|---|
| 05 B1 时间戳 | **REFUTED**（Binance）；精确提交分布缺数据 | 元素时间在LLM返回后；40/40匹配，元素→成交中位9.46s/P90 14.86s | 原假设不成立；分钟撮合仍忽略执行延迟；股票路径另有旧now例外 |
| 05 B2 重评估 | **CONFIRMED** | 原308中仅19仍accepted；22个当前accepted首次为candidate；重评估不增K | 研究及因子paper选择口径；变化率不能当FWER |
| 05 B3 前向功效 | **CONFIRMED**，并纠正未通过原因 | 41条有13个连通簇/23个严格簇；h7未达阈值正效应组中位还需57天；全部前向ls_ret<0 | 因子证据不能解释成41份独立支持；主交易员未启用 |
| 05 B4 R缺失 | **CONFIRMED** | 224/573缺止损，mac-nofx210/434；四个paper均0缺失 | 主交易员跨时期R有选择口径，不能外推全库缺失率到当前paper pair |
| 05 B5 新鲜度/追踪 | **CONFIRMED** | h1第3日仍放行；当前as_of=10-26；无hash/run_id | 当前直接影响因子paper候选，主交易员关闭 |
| 05 B7 股票滑点 | **INCONCLUSIVE**（量级）；零滑点已确认 | commission默认10bp/侧、滑点0；本地只有tick规则无价差/深度 | 美股研究成本不完整；不影响Binance实盘撮合 |
| 06 B1 同步余额 | **CONFIRMED** | 重设账户/图表/MDD及账户风控基准；3,189条历史基线均135.18 | 功能可影响实盘，但近14天未观察到活跃基线变动 |
| 06 B3 MDD口径 | **CONFIRMED** | 已知入金后同窗：平仓2.45% vs 权益28.73%；全史80.37%混有转出 | API展示低估权益波动；AI上下文已有快照优先路径 |
| 06 B6/B7 错误/负载 | **CONFIRMED**（机制）；当前负载推断未获支持 | 错误丢弃；主434笔，三个Go统计中位8.01ms/P90 8.45ms | 静默失败缺陷成立，当前数据量尚无可感知负载证据 |
