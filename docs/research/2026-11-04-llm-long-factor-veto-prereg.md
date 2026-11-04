# LLM 加密交易员：因子弱币做多否决（B 类）：预声明（2026-11-04）

这是增量评估框架（`docs/architecture/INCREMENTAL_EVAL_DESIGN_2026-11-03.md`）在 `crypto_llm` 交易线上的第 1 次尝试，K = 1，z_K = 1.960。

- **改动（B 类，开仓过滤）：** LLM 交易员准备做多时，如果该币的因子组合分数在开仓前一日处于全币池最低 20%，就否决这次做多。做空、平仓、仓位和出场都不变。
- 本文在写任何评估代码、看到任何“交易 R 按因子分数分组”的结果之前提交，看到结果后不修改。
- 实现脚本另行提交（`scripts/factor_mining/llm_long_veto_study.py`）。它必须引用本文件路径，并校验下面的冻结哈希。

## 动机与已知证据

1. **因子能识别弱币，但单独做空兑现不了。**
   - 在 fq_crypto 的 S1 研究（`2026-11-04-pit60-btchedge-study.md`）中，用含已下架币的时点 top 60 重新验收出 20 个因子，记为 F1。
   - F1 的前向（2026-05-07 → 10-31）RankIC 为 0.190（NW t 9.0）。
   - 这部分信息集中在“谁会跌”：单币空头在 val 段赚 +59%，在前向段亏 −32%（拉盘尾部加负资金费）；多头腿则跑不赢 BTC。
2. **LLM 交易员的多头本来就是亏的。** 实盘约 −0.077R/笔（2026-11-01）。LLM 常做多高波动的小币，而这正是 F1 判为弱的一侧。
3. **假设：** 用因子否决“做多弱币”，在不增加任何空头暴露的情况下利用这部分信息。被否决的做多，每笔 R 应低于整体。

### 已知的数据污染（必须打折看待 S1 结果）

- **评估区间被看过。** LLM 交易所在的区间（2026-09-17 → 10-31）落在 F1 的前向区间内，F1 在全币池上的前向 RankIC、腿级收益都已看过，本预声明正是由此引出的。
  - 但据检索 `docs/research/`，此前**没有**任何研究把 LLM 真实交易的 R 按因子分数分组看过。
  - 写本文前只看过：各交易员做多笔数、AI 归因笔数，以及各候选币池下“开仓前一日有分数”的覆盖率（见下）。这些都不涉及 R。
- **F1 选出时用过 val。** F1 的验收与排序只用了 train/val（2022-05 → 2026-05-01），没有用到 LLM 交易区间。
- **币池 N 的选择看过覆盖率。** 打分币池的选择看过覆盖率，没看过任何结果：

  | 币池 | 覆盖率 |
  |---|---|
  | 现有 100 币时点 top 60 | 42% |
  | 全量时点 top 60 | 52% |
  | top 100 | 69% |
  | top 150 | 80% |
  | 不设上限 | 91% |

  选“不设上限”，理由是 LLM 交易的币大多不在 top 60 内。
- **结论：** S1 只用来决定是否进入 S2。决定性证据只能来自 2026-11-04 之后的新交易（S2）。

## 冻结输入

| 输入 | 路径 | sha256 |
|---|---|---|
| F1 因子清单（20 个，h=3） | `data/factor_mining/crypto_full/prereg_20261104b/f1_selected.json` | `68bfeffe293f11f8ba373e58362d310ba900f9c6168daa236535e805a0dacf04` |
| F1 来源报告 | `data/factor_mining/crypto_full/study/pit60_btchedge/report.json` | `615d8e3e9d04de69465056a5363b3c65fc038eefb8b82520c13143bab2ab7cab` |
| F2 ensemble 快照（h=3 共 11 个因子，只作描述） | `data/factor_mining/crypto_full/prereg_20261104/ensemble.json` | `f53fb2a5c15f5cf246b251350cc365bf5b862b756c8b952fd220c31af1870eb0` |
| 全量面板校验 | `data/factor_mining/crypto_full/validation.json` | `5c0fdc770abff256f2ccce92e8ba25a5a84bdf596abd99ed9db6ebce6dc1e2a5` |

- F1 中 18 个方向为 −1，族为 volatility / other / price_volume / momentum。
- `f1_selected.json` 是从来源报告的 `reacceptance.selected` 原样抽取的 `id, expr, family, horizon, direction, status, train_t`。
- **交易数据：** `data/data.db` 只读打开（`mode=ro`）。
  - 脚本把导出的交易和掩码各写成一个 jsonl 文件，记录其 sha256。
  - 正式运行与预演运行的交易文件哈希必须相同；若不同（数据库在两次之间有新平仓），以正式运行为准，并在报告中列出差异。
- **面板：** `panel.load_panels` 读 `crypto_full` 布局，`universe_n=None`、`min_history=60`，只在进程内设置。截断到 2026-10-31（资金费和 premium 月度归档的最后一天）。

## 交易样本

- **交易员**（只含不向大模型提供因子字段的加密 LLM 交易员）：

  | trader_id 前缀 | 名称 | 模型 |
  |---|---|---|
  | `d7ff3764…deepseek_1787982996` | mac-nofx（实盘） | deepseek |
  | `a8eb1bdf…` | 因子 ensemble 参考字段-对照（模拟） | glm |
  | `ec830bb2…` | 提前减仓 0.8R-候选（模拟） | glm |
  | `895dc8b3…` | 提前减仓 0.8R-对照（模拟） | glm |

  - 排除 `a2dc5705…`（因子 ensemble 参考字段-候选）：它的开仓已经受因子信息影响。
  - 排除美股和 factor_quant 交易员。
  - `ec830bb2` 只改出场、不改入场，所以保留。
- **入选条件**（全部满足）：
  1. 用 `scripts.backtest.incremental_filter.read_closed_trades` 读取，即 `status='CLOSED'`，R 由 `report.trade_statistics` 计算，R 无效的排除；
  2. `side` 为做多（不区分大小写）；
  3. 入场 UTC 日期 ≤ 2026-10-31，没有起始日（R 有效的最早一笔约在 2026-09-17）；
  4. **AI 归因：** 同一交易员的 `decision_records` 中，存在一条 `action ∈ {open_long, open_long_limit}`、`success = true`、symbol 相同的决策，其 `timestamp` 在入场时间之前 0–24 小时内。
     - 不满足的视为用户手动单（实盘账户上用户会手动交易），排除。
     - 写本文时的统计：165 笔做多中 144 笔可归因，入场 ≤ 10-31 的有 138 笔（实盘 117、模拟 21）。
- **样本只含做多。** 安慰剂随机剔除也只在做多集合内进行，否则多空 avgR 的差异会混进统计量。

## 打分与否决规则

- **分数：** 用 F1 的 20 个因子，在全量面板（`universe_n=None`）上按 `ensemble.ensemble_scores` 等权合成，即每个因子乘方向后在当日掩码内 z-score，再取平均。
- **取值日：** 入场 UTC 日期为 D 时，取面板行 D−1，也就是最后一根已收盘的日线。
- **分位：** 在 D−1 当日掩码内，对分数有限的币按升序排位，pct = rank / count（`rank(pct=True)`）。
- **否决（drop = true）：** 该币在 D−1 的掩码内、分数有限，且 pct ≤ 0.20。
- **无分数**（不在面板、不在掩码内、或分数非有限）记 drop = false，即保留。这类交易计入样本，并单独报告笔数。
- **掩码文件：** 每笔样本交易都写一行 `{trade_id, drop}`，因此 `mask_coverage_error` 应为 0。

## S0 复现闸门（不通过即判为 invalid）

1. **F1 打分复现：** 用同一代码在全量时点 top 60 面板（`universe_n=60`）上，计算 F1 分数在前向区间 2026-05-07 → 10-31 的每日 RankIC 均值。它它必须等于来源报告的 `rank_ic.UH.mean`，并且与 `rank_ic.UH.daily` 逐日一致（绝对误差 ≤ 1e-9）。
2. **有分数覆盖率：** 样本中有分数的交易占比必须 ≥ 80%。写本文时测得约 91%。
3. **交易导出复现：** 导出交易的 R 必须与 `incremental_filter export-trades` 对同一交易员的输出逐笔一致。

## 主检验（S1）

- **工具：** `python -m scripts.backtest.incremental_filter evaluate`，参数如下：

  | 参数 | 取值 |
  |---|---|
  | `--track` | `crypto_llm` |
  | `--idea-id` | `llm-long-veto-f1-q20-v1` |
  | `--baseline-version` | `crypto_llm_hist_20260917_20261031` |
  | `--seed` | 20261103 |
  | `--placebo-n` | 2000 |
  | `--hypothesis` | 本文标题 |

- **主统计：** Δ = 保留集 avgR − 全体做多 avgR。SE 取自同剔除比例的随机剔除安慰剂分布，z = Δ / SE。安慰剂分位 = P(安慰剂 Δ < Δ)。
- **两半：** 按入场时间排序（时间相同时按 trade_id），前 ⌊n/2⌋ 笔为 dev，其余为 holdout。
- **判定**（按 `incremental_filter.decide_verdict` 的顺序，取第一个满足的）：

  | 判定 | 条件 |
  |---|---|
  | `invalid` | n < 100；或掩码缺失 > 5%；或剔除率 p ∈ {0, 1}；或 S0 不通过 |
  | `fewer_trades` | p > 0.5 |
  | `strong` | z ≥ 1.960，两半 Δ 都 > 0，且安慰剂分位 ≥ 90% |
  | `screen_pass` | z ≥ 1.0，两半 Δ 都 > 0，且安慰剂分位 ≥ 90% |
  | `fewer_trades` | 安慰剂分位 < 90%，但总 R 变好 |
  | `fail` | 其余 |

- **记账：** 由工具追加到 `data/evolution/ledger.jsonl`，change_type = `B`，stage = `S1`。K 由工具按账本计算，预期为 1；若不是 1，照实记录并在报告中说明。
- **功效说明：** 以 n ≈ 130、p ≈ 0.15、单笔 R 标准差约 1 估算，安慰剂 SE 约 0.035R。只有被否决集的 avgR 比整体低约 0.4R 以上，才可能达到 z ≥ 2。z 在 1–2 之间只能作为进入 S2 的筛选依据。

## 辅助（只描述，不改判定）

- **剔除明细：** 被否决集与保留集的笔数、avgR、按周聚类的 95% CI、总 R 变化，以及按交易员分开的结果。
- **分数分组：** 有分数交易的 R 按 F1 分位五等分，给出每组笔数和 avgR，只描述。
- **其他阈值：** pct ≤ 0.10 和 ≤ 0.30 的同样统计。不得用来替换主阈值。
- **F2：** 快照 ensemble（h=3，11 个因子），分别在现有 100 币时点 top 60 和全量不设上限的面板上，用同样的否决规则。
- **对称做空否决：** 做空且 pct ≥ 0.80 时否决，在做空集合内做同样的统计。只描述；若想评估，必须另写预声明，K 加 1。
- **被否决交易的特征：** 币名、开仓前 20 日成交额中位数、30 日波动，以及是否为上市不足 90 天的新币。
- **无分数交易：** 笔数、币名（TradFi 永续、超出数据截止日或上市不足 60 天）和 avgR。

## 后续

- **`strong` 或 `screen_pass`：** 准备 S2，按下面的条件预先登记，但不自动执行。实现与上线都需要用户批准。
  - **线上实现：**
    - `crypto_full` 每日增量刷新；
    - 每日生成 F1 全币池分位文件；
    - kernel 新增闸门代码 `FACTOR_WEAK_LONG`，读取分位文件，pct ≤ 0.20 时拦截 `open_long` / `open_long_limit`。
  - **S2a 影子标记**（零风险，先上）：在全部加密 LLM 交易员上只记录、不拦截。
    - 被拦截的信号照常写入 `gate_shadow_blocks`；每笔真实做多记录开仓时的 F1 分位。
    - 累计 2026-11-05 之后新的 AI 归因做多 ≥ 100 笔后，用与本文完全相同的规则重跑 S1 统计，作为确认性检验。
    - 判定沿用上表，账本 stage 记为 S2。
  - **S2b 线上配对：** 只在 S2a 达到 `screen_pass` 及以上之后才启动。
    - 候选为新建的模拟盘，配置等于现行基线（`f803178330bc…`）加上 `FACTOR_WEAK_LONG` 闸门；对照为同配置、无闸门的模拟盘。
    - 主指标是 avgR 差，按周聚类。
    - 最短运行 30 个配对日，且两边各 ≥ 50 笔平仓。
    - 非劣效边际 m = 0.03R。
    - `harm_stop`、`promote`、`fewer_trades`、`extend` 的定义同设计文档。
    - 注意：开仓数比 < 0.67 时判 `fewer_trades`。如果 S1 测得的 p > 0.33，应预期会落到这一档。
- **`fail` 或 `fewer_trades`：** 记账并结束。不调整阈值、币池 N、因子集合或归因规则后重试；如要重试，必须另写预声明，K 加 1。
- **任何结果都不改变：** `config.py`、nightly 挖掘池、`zoo.json` / `ensemble.json`、kernel 闸门、任何交易员和实盘账户。

## 实现要求（给实现脚本）

- **文件：** `scripts/factor_mining/llm_long_veto_study.py`，测试放在 `scripts/factor_mining/tests/test_llm_long_veto_study.py`，全部离线。测试覆盖：
  - AI 归因窗口的边界（0 和 24 小时）；
  - 取值日为 D−1；
  - pct 的排位与 0.20 边界；
  - 无分数时保留；
  - 只在做多内做安慰剂；
  - 冻结哈希不符时拒绝运行。
- **输出：** `data/factor_mining/crypto_full/study/llm_long_veto/`，包括 `trades.jsonl`、`mask.jsonl`、`report.json`、`report.md`。
- **调用方式：** 先用 `--no-ledger` 预演一次，确认 S0 通过、输出完整，再正式运行写账本。两次的统计必须一致。
- **复用：** 复用 `incremental_filter`（读交易、统计、判定、账本）和 `ensemble.ensemble_scores`，不另写统计逻辑。
