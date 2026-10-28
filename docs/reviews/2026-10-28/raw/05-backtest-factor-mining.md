# 研究基础设施审查报告（回测模块 + 因子挖掘 + 晋升闭环）

范围：`scripts/backtest/`、`scripts/factor_mining/`、`market/breakout/{backtest,shorttuner}.go`、`cmd/cycleic`、`scripts/promotion/`。只读审查，未改仓库。
已跑：`scripts/backtest/tests` 255 项通过（5 skipped）、`scripts/factor_mining/tests` 145 项通过、`go test ./cmd/cycleic ./market/breakout` 通过。
下面的数字除特别注明外，都是我在本机 `data/factor_mining`、`data/backtest_cache`、`data/data.db`（只读）上实测的。

总体判断：评估器冻结、h+2 purge、train/val/holdout 滚动切分、Bonferroni、预注册、holdout 花费回执，这套纪律在同类自研系统里算好的。真正的硬伤不在统计公式，而在**数据口径**（币池）和**成本口径**（资金费率）。这两处直接影响“accepted 因子是否真实”。

---

## A. 确认的 Bug / 缺陷

### A1. 因子挖掘的 crypto 币池是 2026-10-18 的快照，不是时点币池（幸存者与选择偏差）｜P1
- 位置：`scripts/factor_mining/data_fetch.py:253-255`（`crypto_universe()` 读 `data/alt_data/universe_src.json`），`scripts/factor_mining/panel.py:112-117`（只在这 100 个币里按 20 日成交额取前 60）。
- 证据：
  - `universe_src.json` 修改时间为 2026-10-18，里面是 AGT、Q、ICNT、HYPE、PUMP、SIREN 等 2025 年才上市且至今存活的币，没有任何已退市币（LUNA、FTT 之类）。
  - 把 `panel.mask` 按年平均币数统计：2022 年 34.4、2023 年 44.1、2024 年 57.3、2025 年起 60。2022 年的“前 60”其实是“活到今天的 34 个币”。
  - `docs/research/factor-mining-design.md` 只承认美股幸存者偏差，没有提 crypto 的快照问题，还写成“点时刻按成交额取前 N”。
- 触发场景与后果：训练窗 2022-05 至 2025-04 里，“波动大、日内振幅大”的币在幸存者样本里必然显得未来收益高。现有 41 个 accepted 因子大部分属于这一类（`Mean($low/$close,60)`、`Mean((high-low)/close,30)`、`Min($low,10)/$close`、`Corr($close,Log($volume+1),5)` 等）。
- 我排除了“HAC 滞后选小导致 t 虚高”这个解释：对 `Mean($low/$close,60)`（h=3）把 NW 滞后从 3 改到 40，t 在 5.5–5.9 之间几乎不动；月度 IC 为正的比例 86%（36 个月，月度 t=5.4）。所以这是样本本身的系统性溢价，不是统计量算错。
- 指纹：`zoo.json` 里 crypto 每个 horizon 的试验数 K=8306、`t_req=4.53`，但通过 train 门槛的有 h=1：3889 candidate + 117 redundant + 5 accepted，约 48%。Bonferroni 在这里几乎不起约束，说明样本里存在大量“免费”的截面溢价。
- 前向校验：41 个 accepted 因子，0 个 confirmed、40 个 inconclusive、1 个 failed。但前向 IC 均值 +0.038、40/41 为正，只是天数少（约 175 天）t 不够。这个证据不否定也不确认因子；而且前向窗口同样落在 10-18 快照之内，也不是干净样本外。
- 修复：
  1. 每日用 `exchangeInfo` + 历史 `onboardDate`/`deliveryDate` 重建全量 USDT 永续时点币池，含已下架合约；`universe_n` 仍按 t 时刻成交额排序。
  2. 币安公开数据 `data.binance.vision` 有已下架合约的历史 klines/fundingRate（需联网确认，我这里无网络）。
  3. 不能全量重建时，至少做敏感性实验：只保留 2022 年已上市的币，看 accepted 数量与 IC 是否塌缩。
  4. 把快照币池的偏差写进页面“已知局限”。

### A2. 因子多空“净收益”不含资金费率，成本是固定 8bp 单边｜P1
- 位置：`scripts/factor_mining/evaluate.py:80`（`ret = r[j,long].mean() - r[j,short].mean() - one_way_cost*2*(tl+ts)`），`config.py:4`（`one_way_cost=0.0008`）。
- 触发场景：永续合约持有 h 天，多头付/空头收资金费，本仓库自己已有 `$funding` 日序列（`panel.py` alt 字段）。
  - 与资金费相关的因子（`FUND_*`、`PREM_*`、高波动小币）的 `ls_ret` 被系统性高估或低估。
  - `classify()`（`evaluate.py:139`）要求 `val.ls_ret>0 and train.ls_ret>0` 才 accepted，所以这个遗漏直接进入准入判断。
  - 小币进入 Q1/Q5 极端桶时，固定 8bp 也低估冲击成本。
- 幅度：未量化。典型 ±0.01%/8h，7 天持有单腿约 0.2%，与 8bp×2×换手同量级。需在 `long_short` 里用 `fields['funding']` 回补后重算 41 个 accepted 的 `ls_ret`。
- 修复：`long_short` 增加资金费项（多头付 `Σ funding`，空头收）；成本按 t 时刻成交额分档；`evaluate.py` 是冻结文件，改它等于新实验，需要 bump 版本并重置 ledger/K（设计文档本身就要求这样）。

### A3. `晋升报告` 的 `ready` 只比较点估计，纯噪声也能过｜P1（研究→实盘唯一闸门）
- 位置：`scripts/promotion/report.py:336-356`（`decide_verdict`）。
- 触发场景：候选与对照配置完全一致、无任何真实差异时，三个条件“候选净盈亏>0”“期间收益率不低于对照”“日配对差均值≥0”各约 50% 概率且正相关，同时满足的概率估计在 20–30%（粗估，未仿真）。7 天、20 笔的最低门槛下，噪声即可得到 `ready`。
- `paired_daily` 和 `weekly_avg_r` 的 CI 已经算出来（`ci95`），但 verdict 完全不用。
- `CAVEAT` 文案承认了，文档也要求人工复核，所以我标 P1 而非 P0。风险在于：它是目前唯一把“研究结论”和“上线”隔开的自动判定，且输出 `ready` 字样。
- 修复：`ready` 要求 `ci95` 下界 > -ε（非劣效）并且配对日 ≥ 30、两侧平仓 ≥ 50；新增 `pairs` 个数的多重比较折扣；LLM 随机性大，建议同一市场快照下两侧使用相同 seed/温度 0 做配对。

### A4. `ml-eval` 走步检验没有 purge/embargo，置换检验也不保留时序依赖｜P1（方向是“容易误放行”）
- 位置：`scripts/backtest/mleval.py:69-81`（`walk_forward` 用 `d.X[:a]`/`y[:a]` 训练），`mleval.py:119-121`（`rng.permutation(d.n)` 独立打乱标签），`mleval.py:138-166`（nested 用 `d.y[:a]`）。
- 触发场景：标签 `r_base/r_plain` 的持仓期最长 72h，信号约 14 个/天（420 个信号/30 天）。训练集末尾 72h 内的约 40 个信号，其标签在测试折开始时还没有实现，且与测试窗口共享同一段行情。模型相当于学到了测试窗口的市场状态，OOS lift 虚高。iid 置换的零分布没有这种时序聚集，p 值偏乐观。
- 当前结论是 `NO DEPLOYABLE FILTER`，所以现在没出事；但门禁方向是“漏判会放行”，建议补上。
- 修复：训练集剔除 `t0_ms + horizon > test_start` 的行并加 embargo；置换改成块置换/循环平移；或直接用 `sklearn.model_selection.TimeSeriesSplit(gap=...)` / `skfolio` 的 CombinatorialPurgedCV。

### A5. Backtrader 适配器：开仓成交所在那根 K 线内的 SL/TP 被整根忽略｜P2（方向恒为乐观）
- 位置：`scripts/backtest/backtrader_adapter.py:117-118`（成交通知里只置 `refresh=True`）与 `:185-186`（`next()` 里才 `_place_exits`），保护单最早在**下一根**生效。
- 复现（本机跑 `run_trade`）：市价单 entry=100、SL=95，入场那根 low=90 后收回，后续触达 TP，结果 `how=tp, R=+2.0`；限价 99 成交、同根 low=90，结果 `tp, R=+2.75`。
- 频率：`signals.json` 中 187 个限价信号（有 1m 缓存）里，有 5 个（2.7%）的“触价那根”的区间穿过了 SL。
- README 已声明此限制，但没有量化；它恰好发生在最差的 adverse-selection 场景（闪崩穿过限价和止损）。
- 修复：入场根若区间穿过 SL，按保守原则记为 SL 出场（与同根“先止损”的约定一致）。

### A6. 限价单“触价即成交”，对实盘未成交信号过度成交｜P2（口径偏乐观，已部分披露）
- 位置：`backtrader_adapter.py:147-148, 202`（`Limit` + `slip_limit=False`），README 也承认“实盘未成交的信号也可能在模拟中成交”。
- 近似量化（我用 45 分钟窗内的 px0 触价粗判，忽略改价链）：417 个限价信号里实盘成交 148、未成交 269。模拟触价成交 180；实盘未成交的 269 个里有 60 个（22%）在模拟里成交，实盘成交的 148 个里有 28 个模拟没触价。
- 影响：`entry` 命令的 A（模拟限价）对 B（下一根市价）的比较，A 的样本池被不真实的成交撑大，且没有排队/穿价要求。
- 修复：增加“穿价至少 1 tick/成交额阈值才算成交”的保守模式，或用 `outcome=="filled"` 标签做分层并行汇报。

### A7. `fetch.py` 的 1m→5m 拼接最多丢 4 分钟｜P2
- 位置：`scripts/backtest/fetch.py:88`（`start = (b1[-1][0] // (5*MIN) + 1) * 5*MIN`）与 `klines.py:44-45`。
- 触发：最后一根 1m 的 open 为 12:57 时，5m 从 13:00 开始，12:58–13:00 的数据缺失；`build_trade` 只检查不重叠，不检查空洞，SL 在空洞里被触发且价格收回就看不见。
- 影响：约占 25 小时窗口的 0.1–0.3%，小但无条件乐观。
- 修复：5m 起点取最后一根 1m 的收盘时刻向上取整，或用 1m 补满到 5m 边界。

### A8. 因子挖掘 ledger 每条试验都重写整个文件｜P2（性能）
- 位置：`scripts/factor_mining/store.py:92-105`（`append_jsonl` 读全文件 + 写临时文件 + fsync + replace），`miner.py:404`（每个有效试验调一次）。
- 数据：crypto ledger 7.16 MB、US 8.67 MB；每晚约 765 个新表达式 × 3 个 horizon ≈ 2300 次追加，约 16 GB 读 + 16 GB 写 + 2300 次 fsync，单市场一晚耗时 765–1096 秒，有相当比例花在这里（未单独 profile）。
- 修复：ledger 是 append-only，直接 `open('a')` + 一次 flush/fsync 即可；崩溃最多留半行，读取时跳过坏行。

### A9. `quant_audit.flag_martingale` 与 `mleval._rank` 的秩没处理并列｜P2
- 位置：`quant_audit.py:395-400`（`_ranks` 按序号赋秩）、`quant_audit.py:360-370`；`mleval.py:33-34`（`argsort(argsort)`）。
- 复现：`sizes` 恒为 1.0（不可能是马丁格尔），随机 `rets`，200 次里有 20 次被判为 martingale（`rho>0.5`）；`streak` 里大量并列 0，序号秩与下标相关。`mleval` 对二元特征（`regime_chop`、`conflict`）：标签有轻微时间漂移时 |rho|≥0.10 的命中率 15%，tie-aware 为 8%。
- 修复：用平均秩（`scipy.stats.rankdata` 或 `pandas.rank`）。

### A10. Go 研究回测的入场价与成本口径（`market/breakout/backtest.go`）｜P2
- 位置：`backtest.go:213, 226-228`。
- `entry := series[len(series)-1].Close`，前瞻收益用同一根收盘价作为入场价，没有下一根开盘延迟；`btCostRoundTrip=0.20` 不含 24h 持有的资金费。
- 因为 `LIVE PUBLICATION BLOCKED` 锁住了，不会改实盘参数；但阈值选择（`bestCutoff`）用的就是这些偏乐观的净收益。
- 已披露且做对的部分：`sliceClosed` 按收盘时间切片、levels 每小时点时重建、训练样本按 24h 标签窗口 purge、`independentTrades` 单币单仓，我核对后没有发现前视。

---

## B. 疑似问题（需进一步验证）

1. **信号 `t0` 是否早于 LLM 返回时间**（`signals.py:112-118`，`exitsim.py:18-23`）。回测在 `t0` 所在分钟收盘后下单。若决策元素 `timestamp` 是周期开始而不是指令生成时刻，则回测入场比实盘早 30–120 秒（含 LLM 延迟）。需查 `decision_records` 里元素时间戳的生成点，对比 `trader_orders` 的实际下单时间。
2. **每晚重评估带来的重复检验**（`miner.py` 的 reevaluate 段）。K 只在首次评估时增加，之后每晚在滚动窗口上重新分类。crypto 有 2185/9262（24%）的因子发生过状态变化；ledger 里首次评估为 accepted 的有 308 个，当前仍是 accepted 的只有 41 个。设计文档承认“验证窗口反复使用，K 不能完全校正”，但没有量化。建议用 20–60 晚的滚动空数据（块 bootstrap 真实收益，保留 IC 自相关）测 FWER。
3. **前向校验的统计功效**。41 个 accepted 因子的前向 IC 均值 0.038、40 个为正，但按单因子 `t≥1.65 且 ls_ret>0` 判，h=7 约 172 天不够，所以全是 inconclusive。因子之间高度冗余（相关 <0.8 仍可能属同几个簇），41 个不是 41 次独立检验。需要簇级或 ensemble 级的前向 IC（设计文档说 ensemble 暂无前向校验）。
4. **`report.py` 的 R 统计样本选择**。`closed` 持仓中 224/563（40%）缺 `initial_stop_loss`，被排除出 `avg_r`。如果缺失集中在特定时期/类型，`avgR` 差会有偏。需看缺失的时间分布。
5. **`live_scores.json` 新鲜度**（`kernel/factor_ensemble.go` 放行 as_of 距今 ≤3 天）。1 日 horizon 因子在第 3 天仍当作当前证据给 LLM。且文件没有写入 evaluator hash / run_id，事后无法把某次 AI 决策对应到具体快照。
6. **LLM 的先验知识泄漏**。GLM 提案时已“知道”2022–2025 年哪些类因子有效（低波动、反转、量价背离），这部分泄漏无法用切分消除；建议只看 LLM 训练截止日之后的数据。
7. **美股组合回测无滑点**（`stocks.py` 的 `set_slippage_perc(0.0)`，佣金 0.1%）。币安 bStock 的价差未建模，结论更可能偏乐观；仅方向性怀疑，无实测。

---

## C. 可复用的成熟方案

| 现状（自写） | 替代 | 判断 |
|---|---|---|
| `evaluate.nw_t` / `cs_rank_ic` / `long_short`（`evaluate.py`） | `statsmodels` HAC（`cov_type='HAC'`）、`alphalens-reloaded`、`empyrical-reloaded` | **保留自写**：向量化，每晚要评估 8k+ 因子×3 周期，alphalens 是 pandas 逐因子，慢一个数量级；且文件被哈希冻结。建议只在测试里把 statsmodels HAC / alphalens 当 oracle 交叉核对一次（venv 里没装 statsmodels，本次未能核对）。 |
| `expr.py` DSL + `zoo.py` Alpha158 | `qlib` | **保留自写**：已有 `audit_qlib/` 对 qlib 算子做数值核对（124/158 一致，其余有说明）；AST 白名单对 LLM 输出的安全边界重要；qlib 的二进制数据格式对 7×24 币种不友好。 |
| `backtrader_adapter.py`（每笔单独 Cerebro） | `vectorbt`、`nautilus_trader` | **保留 Backtrader**（项目政策 + 部分平仓/OCO/追踪语义）；`vectorbt` 的 sl/tp 是向量化近似，部分减仓不自然；`nautilus_trader` 更真实但迁移成本高。Backtrader 上游基本停更，且每笔新建 Cerebro 开销大，`exitsim.summarize` 是串行（`ict_bt.run_jobs` 已用多进程，可复用）。 |
| `mleval.py` 手写 walk-forward + 置换检验 | `sklearn.TimeSeriesSplit(gap=)`、`skfolio.CombinatorialPurgedCV`、`arch.bootstrap`（StationaryBootstrap） | **建议替换**切分与置换部分；模型本身（logit/ridge/HGB）已经是 sklearn。 |
| 48 个出场变体的逐个比较（`exitsim.run_variants`） | `arch.bootstrap.SPA` / `StepM`（White Reality Check、Hansen SPA） | **建议引入**：现在只有 sumR/avgR，没有标准误，没有多重比较。 |
| `quant_audit` 的 CAGR/Sharpe/MDD | `empyrical-reloaded` / `quantstats` | 可选，收益很小。真正缺的是 PSR/DSR（Bailey & López de Prado），30 行自写即可，不必引入商业版 `mlfinlab`。 |
| `data_fetch.py` / `fetch.py` 的 urllib 抓取 | `ccxt` 或 `binance-futures-connector`；历史全量用 `data.binance.vision` | **建议**：后者能补已下架合约和长于 30 天的 OI/多空比历史（`alt_accum` 目前自己积累，起步晚）。需联网确认可用字段。 |
| `miner.mutate` 的 AST 变异 | `gplearn` / `DEAP` | 不必换：现有变异空间受 DSL 约束且已哈希去重，换库收益小。 |

明确合理的自写：冻结评估器与表达式引擎（审计过）、`promotion/report.py`（纯标准库、只读 SQLite）、`threepush_study` 等预注册研究的封存与指纹机制。

---

## D. 优化建议（按预期收益排序）

1. **把 crypto 币池改成时点币池，含已下架合约**（对应 A1）。这是对全部 crypto 因子结论影响最大的一项。先做敏感性对照（只留 2022 年前上市的币，看 accepted 是否还在），成本一个下午；再做全量重建。
2. **在 `long_short` 里计入资金费率，并按流动性分档成本**（A2）。冻结评估器需整体 bump 版本、重置 K，一次性做完；同时把币池修正放进同一版本，避免两次重置。
3. **晋升闸门改成显著性/非劣效判定**（A3），并把 `weekly_avg_r` 的 CI 纳入。当前 `factor-ensemble` pair 仍是 `INSUFFICIENT_SAMPLE(2/48 天)`，说明闸门没跑通过，现在改成本最低。
4. **有效试验数与重复检验**（B2、B3）：用相关性聚类估计有效试验数代替“表达式个数 K”，对 ensemble/簇做前向 IC 与 DSR；`accepted` 状态改为“登记时判定一次”，之后只做衰减监控，不再升级。
5. **修 `mleval` 的 purge/embargo 与块置换**（A4）。或者承认 n≈420 的样本下该检验功效极低（SE(avgR)≈0.056，317 笔），降低它的投入。
6. **出场变体报告加标准误和配对检验**：同一批交易上 48 个变体对基线做配对差，输出 SE 与 SPA p 值。`data/backtest_cache/backtrader_mkt_72h.csv` 基线 avgR=-0.035，变体间差异多半在噪声内。
7. **Backtrader 适配器两处保守化**（A5、A6、A7）：入场根穿 SL 记止损、限价要求穿价、拼接补缝。每处改动小，但都是“恒乐观”方向，合在一起能让 A/B 对比更可信。
8. **ledger 改真追加**（A8）；`exitsim.summarize` 并行化。
9. **研究→实盘版本化**：`live_scores.json` 增加 `evaluator_sha256`、`run_id`、币池哈希，Go 在 `decision_records` 里记录所用快照；这样 `promotion` 的 pair 才能回溯到具体研究版本。现在只有 `version:1`。
10. **可观测性**：`runs.jsonl` 里加每夜耗时分段（fetch/评估/IO），先量化 A8 的占比再决定是否做更大的重构。

---

## E. 覆盖范围与未覆盖

已读（全文或关键路径）：
- `scripts/backtest/`：`backtrader_adapter.py`、`exitsim.py`、`entry.py`、`signals.py`、`klines.py`、`fetch.py`、`params.py`、`mldataset.py`、`mlfeatures.py`、`mleval.py`、`quant_audit.py`、`stocks.py`、`stocks_data.py`、`ict_bt.py`、`cli.py`（命令部分）、`README.md`、`threepush_study.py`（检测/特征/标签部分）。
- `scripts/factor_mining/`：`evaluate.py`、`panel.py`、`config.py`、`expr.py`、`miner.py`、`ensemble.py`、`greedy_ensemble.py`、`forward.py`、`judge.py`、`feedback.py`、`allocation.py`、`llm_memory.py`、`zoo.py`、`store.py`、`run_mining.py`、`data_fetch.py`、`live_scores.py`、`ext_universe.py`（前半）、`EXT_SCORING.md`、`run_nightly.sh`。
- `market/breakout/backtest.go`、`shorttuner.go`、`cmd/cycleic/study.go`（信号/前瞻收益/NW 部分）、`kernel/factor_ensemble.go`（读取侧）、`scripts/promotion/report.py`、`docs/research/factor-mining-design.md`、`docs/research/paper-promotion.md`、`quant-research-log.html`（只看目录结构）。
- 实测数据：`data/factor_mining/{crypto,us}/{zoo,ledger,ensemble}`、`runs.jsonl`、`data/backtest_cache/signals.json` 与 1m/5m 缓存、`data/data.db`（只读）。

未覆盖或只浅读：
- `brooks*`、`ict.py`、`ict_study.py`、`regime_study.py`、`short_features_study.py`、`breakdown*_study.py`、`breaker_state_study.py`、`us_candidate_study.py`、`stocks_report.py`、`stocks_regime.py` 的统计细节（只确认它们走 Backtrader 适配器）。
- `scripts/factor_mining/audit_qlib/`、`alt_accum.py`、`options.py`、`ext_universe.py` 后半、`scripts/promotion/create_pair.py`、`cmd/cycleic/main.go` 的报告渲染与组合换手部分。
- Go 侧 `market/breakout` 的评分函数本身（`computeTF`、`combine` 等）、`trader/` 里的下单与保护单逻辑。
- 环境限制：无网络，`data.binance.vision` 可用性与字段未验证；venv 缺 `sklearn`/`statsmodels`，所以 `mleval` 与 HAC 对照只做了逻辑推演/等价小实验，未跑 `ml-eval` 全流程。
