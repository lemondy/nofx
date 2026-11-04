# 因子量化-加密：时点 top 60 全量币池 + BTC 对冲空头腿：预声明（2026-11-04）

这是增量评估框架（`docs/architecture/INCREMENTAL_EVAL_DESIGN_2026-11-03.md`）在 `fq_crypto` 交易线上的第 2 次尝试，K = 2。

- 改动由两部分组成，作为一个整体评估：
  1. **币池（E 类）：** 因子筛选和交易都改用含已下架币的时点 top 60；
  2. **空头腿（C 类，仓位规则）：** 单币做空改为 BTC beta 对冲。
- 本文在写任何评估代码、看到任何结果之前提交，看到结果后不修改。
- 实现脚本另行提交（`scripts/factor_mining/pit60_hedge_study.py`）。它必须引用本文件路径，并校验下面的冻结哈希。

## 动机与已知证据

1. **币池有幸存者偏差。** 现有挖掘池是固定的 100 币名单，只收录今天仍在交易的币（见 `2026-11-03-crypto-universe-expansion.md`）。
   - 在 train/val 上重算发现：换成含已下架币的时点 top 60 后，因子 IC 的 t 普遍提高 2–3。
   - 加回已下架币后，各周期多空收益都比“仅现存币”好。
   - 扩到 N=100、150 后多空收益下降，所以 N 定为 60。
2. **亏损集中在空头腿。**
   - 11-01 的 ensemble 前向研究和 11-03 的宽因子池研究（PR #72）都发现：基线的多头腿接近 0，空头腿亏损。
   - 宽因子池研究中，前向区间每 1000 初始资金：空头腿 −140，多头腿 +8。
   - 现行规则 `ls_invvol_protected_v1` 已经排除了 7 日涨幅前 10% 和上市不足 90 天的币，并按反波动率加权，仍然挡不住右尾拉盘。
   - 所以这次不再修补单币空头，而是让空头腿只承担市场对冲：做空 BTC，名义金额按多头篮子的 beta 计算。

### 已知的数据污染（必须打折看待 S1 结果）

- **前向区间已被看过。** 前向区间（2026-05 起）的基线腿级结果，在上面两次研究里已经看过。“空头腿亏、改为对冲”这个想法正是由那次结果引出的。因此，前向 S1 对 BTC 对冲有利。
- **N=60 的选择看过 val。** 11-03 的币池研究只用了 train/val，没有在全量池上看前向；但 N=60 这个选择看过 val 段的多空结果。
- **假设集来自旧币池。** 待重新筛选的 3866 个 h=3 表达式，是挖掘程序在旧币池上生成的（变异亲本来自旧池的 accepted 因子）。所以这一步是“在新币池上重新验收已有的假设集”，不是从头重新挖掘。
- **结论：** S1 只用来决定能否进入线上配对（S2）。决定性证据只能来自 2026-11-04 之后的新数据（S2）。

## 冻结输入

| 输入 | 路径 | sha256 |
|---|---|---|
| zoo 快照（nightly 2026-11-03T18:35Z） | `data/factor_mining/crypto_full/prereg_20261104/zoo.json` | `aef7c307efb23c01272131497c0fe012b119be42ec76d919c266cee74dee6e3e` |
| ensemble 快照（h=3 共 11 个因子） | `data/factor_mining/crypto_full/prereg_20261104/ensemble.json` | `f53fb2a5c15f5cf246b251350cc365bf5b862b756c8b952fd220c31af1870eb0` |

- **数据：**
  - 基线面板：`panel.load_panels('crypto')`，即现有的 100 币和时点 top 60。
  - 全量面板：`data/factor_mining/crypto_full`（685 个币，含已下架；`full_universe.py` 已构建并校验）。用同一个加载器，`universe_n=60`、`min_history=60`，只在进程内设置。
  - 两个面板都截止到 2026-10-31：这是资金费和 premium 月度归档的最后一天。
  - 实现脚本记录两个面板的摘要，以及 `crypto_full/validation.json` 的 sha256。
- **h=3 切分：** 与快照一致，脚本断言一致后才继续。
  - train：2022-05-08 → 2025-05-01；
  - val：2025-05-07 → 2026-05-01；
  - holdout 从 2026-05-07 开始。

## 四组（所有组的成交与费用都相同）

| 组 | 币池（筛选和交易） | 打分 | 持仓规则 | 用途 |
|---|---|---|---|---|
| **B** 基线 | 现有 100 币时点 top 60 | 快照 ensemble h=3 的 11 个因子等权 | `ls_invvol_protected_v1` | 对照 |
| **UH** 主变体 | 全量时点 top 60 | 新币池上重新验收后的组合（见下） | `long_invvol_btchedge_v1` | **唯一计入账本** |
| U | 全量时点 top 60 | 同 UH | `ls_invvol_protected_v1` | 拆解：只换币池（描述，不计账） |
| H | 现有 100 币时点 top 60 | 同 B | `long_invvol_btchedge_v1` | 拆解：只换空头腿（描述，不计账） |

所有组的共同设定：

- 每 3 天调仓一次，与 `quant_targets._historical_targets` 的节奏相同。
- 用 `scripts.backtest.quantjourney.run_weights` 撮合，手续费 0.0005，滑点 0.0003，计入真实资金费。
- 不加单币止损，与 `quant_targets.backtest` 和宽因子池研究一致。

### UH/U 的打分：在全量时点 top 60 上重新验收（只用 train/val）

1. **候选：** 快照 zoo 中 horizon = 3 的全部 3866 个表达式（不论原状态），沿用记录里的 `expr`。方向在新面板的 train 段上按 `evaluate_factor` 的规则重新确定。
2. **评估：** 在全量面板上用冻结评估器 `evaluate.evaluate_factor` 计算 train/val 指标，成本 8 bp。
3. **验收门槛：** 用 `evaluate.classify`，K = 12636 + 3866 = 16502，t_req = 4.669。旧池上的尝试与本次重新评估合并计数。
4. **去冗余：** 与 `miner._classify_and_correlate` 相同。
   - 按 (−train.t, first_seen, run_id, id) 排序；
   - 用 train∪val 日期每隔 5 天取一个样本，计算与已保留 accepted 因子的横截面秩相关；
   - 相关 ≥ 0.8（`CORR_REDUNDANT`）判为冗余。
5. **组合：** 用 `ensemble.select_ensemble_factors`，按 train t 取前 20 个 accepted。`FM_GREEDY_ENSEMBLE` 关闭，与 nightly 一致。用 `ensemble.ensemble_scores` 等权合成。
6. **无效条件：** 重新验收后的 accepted 少于 5 个时，判为 `invalid`，不计入 K。

### 新规则 `long_invvol_btchedge_v1`

- **多头腿：** 与 `ls_invvol_protected_v1` 的多头完全相同。
  - 取可交易池（掩码内、分数有限、30 日波动有限且 > 0）中分数最高的 20%；
  - 按带上限的反波动率加权（`_capped_invvol`），多头总额 0.5，单币上限 0.1；
  - 可交易名数 < 15 时，本期不出目标，沿用上期。
- **空头腿：** 只做空 BTCUSDT。
  - 名义金额 h = min(0.5, Σ_i w_i·β_i)。
  - β_i 是截至 as_of 的最近 30 个日收益对 BTC 日收益的 OLS 斜率。有效观测少于 20 个时取 1；β 截断在 [0, 3]。
  - 如果多头篮子里本来就有 BTC，BTC 的净权重为 w_BTC − h。
- **其余：** 不做单币空头，也不用 7 日涨幅和上市天数的空头排除，因为已经没有单币空头。
- **总敞口：** 总额 ≤ 1.0，与基线相同。

## S0 复现闸门（不通过即判为 invalid）

- B 组在前向区间的每日净值，必须与 `quant_targets.backtest` 在同一快照、同一区间、`ls_invvol_protected_v1` 下的净值逐日一致（相对误差 ≤ 1e-9）。
- H 组在 β 恒等于 0 时，必须等于“只有多头腿、空头为 0”的回测（单元测试覆盖）。

## 主检验（S1）

- **区间：** 前向 2026-05-07 → 2026-10-31，约 178 个配对日。
- **主统计量：** d_t = r_UH,t − r_B,t，即两组每日组合收益之差。
  - 统计量为均值 Δ；
  - SE 按 UTC 周一所在周聚类计算，用 `scripts.promotion.report.cluster_se`，带 G/(G−1) 修正；
  - z = Δ / SE。
- **两半：** 前向区间按天数对半分；天数为奇数时，中间一天归入前半。
- **判定**（z_K = Φ⁻¹(1 − 0.05/(2K))，K = 2 时为 2.241）：

| 判定 | 条件 |
|---|---|
| `invalid` | S0 不通过；或配对日 < 100；或任一组回测失败；或重新验收后 accepted < 5 |
| `strong` | z ≥ 2.241，且两半 Δ 都 > 0 |
| `screen_pass` | z ≥ 1.0，且两半 Δ 都 > 0 |
| `fail` | 其余 |

- 不设 `fewer_trades`：这是组合类改动，交易次数由调仓节奏决定。但会报告两组的换手率比和总敞口比。
- **记账：** 追加到 `data/evolution/ledger.jsonl`。
  - track = `fq_crypto`，change_type = `E+C`，stage = `S1`，K = 2，idea = `pit60-btchedge-h3-v1`，prereg = 本文路径；
  - baseline_version = ensemble 快照 sha256 前 16 位 + `:ls_invvol_protected_v1`。

## 辅助（只描述，不改判定）

- **验证段配对差。** 区间 2025-05-07 → 2026-05-01，同样的 d_t。两组的因子都是用 val 选出来的，结果对两组都偏乐观。
- **四组拆解。** U−B、H−B、UH−U、UH−H 的 Δ 和 z，用来判断改善来自币池还是空头腿；交互项 = (UH−B) − (U−B) − (H−B)。拆解结果不能单独促成上线：若想单独上线 U 或 H，必须另写预声明，K 加 1。
- **每组绩效。** 年化收益、Sharpe、最大回撤、换手；多头腿和空头腿各自的贡献（沿用 `wide_pool_study.leg_contributions` 的口径）；平均对冲比率 h、平均 β。
- **RankIC。** 前向区间 B 与 UH 的打分在各自掩码上的每日 RankIC，用 NW t（滞后 3）。
- **空头尾部。** B 与 U 的空头腿中，3 日涨幅 > 30% 的持仓占比，以及这些持仓占空头亏损的份额。
- **重新验收清单。** accepted / candidate / redundant 的数量；入选组合的 id、表达式、族、train/val IC 和 t；与快照 ensemble 的重合个数。
- **币池构成。** 前向区间 UH 掩码与 B 掩码的平均重合名数；UH 掩码中不在现有 100 币名单里的币。

## 后续

- **`strong` 或 `screen_pass`：** 准备 S2 线上配对，按下面的条件预先登记，但不自动执行。
  - **用户批准：** 需要用户批准后，才能实现线上部分：`crypto_full` 每日增量刷新、按全量池生成 live scores、新增目标规则 `long_invvol_btchedge_v1`。
  - **配对方式：** 候选为新建的 fq_crypto 模拟盘，对照为现有 fq_crypto 模拟盘（`ae795edd`）。
  - **主指标：** 每日净值收益配对差，按周聚类。
  - **最短运行：** 42 个配对日。
  - **`harm_stop`：** 第 14 天起，若 Δ < −2·SE，提前终止。
  - **`promote`：** Δ > 0，且单侧 95% 下界 > −0.05%/日。
  - **`extend`：** 若 SE > 0.15%/日，延长 2 周，最多一次。
  - 两组都保留现行的 20% 单币止损和 kill switch（BTC 对冲腿同样适用）。
- **`fail`：** 记账并结束。不调整 N、β 窗口、对冲比例上限或验收门槛后重试；如要重试，必须另写预声明，K 加 1。
- **任何结果都不改变：** `config.py`、nightly 挖掘池、`zoo.json` / `ensemble.json`、实盘账户。
