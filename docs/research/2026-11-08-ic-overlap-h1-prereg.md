# H1 预注册：zoo 准入 t 改用均匀核 HAC + 小样本换算

日期：2026-11-08。本文件先提交，之后才实现和计算。

## 起因

审计结论见 `docs/research/2026-11-08-ic-overlap-audit.md`（PR #103）。现行冻结评估器 `scripts/factor_mining/evaluate.py` 的 sha256 是 `f6e392c5…8c850a`。它用 Bartlett NW、滞后 = h 来算每日 RankIC 均值的 t。对慢因子，|t|>1.96 的空模型误判率是 9%–11%。用户在 2026-11-08 说"Do H1"。

## 1. 新口径（冻结定义）

对训练段或验证段的每日 RankIC 序列，沿用 `nw_t` 的约定：去掉 NaN 后按日期顺序压紧，n<20 时返回 0。

1. 设 m = 均值，x = 去均值后的序列，γ_k = Σ x_{t+k}·x_t / n。
2. 方差 V = γ_0 + 2·Σ_{k=1..h} γ_k。这是均匀核，滞后取到 h，比 Hansen-Hodrick 的 h−1 多一阶，用来吸收收益的一阶横截面自相关。
3. 如果 V ≤ 1e-30，改用 Bartlett NW、滞后 2h 的方差（半正定）。仍然 ≤ 1e-30 时，沿用 `nw_t` 的饱和约定。
4. t_raw = m / √(V/n)。
5. 自由度 df = max(1, ⌊n/h⌋ − 1)，即不重叠窗口数减 1。
6. 输出 z = sign(t_raw) · Φ⁻¹(1 − S_df(|t_raw|))，S_df 是 Student t 的生存函数。生存函数下溢到 0 时取 z = t_raw。h=1 时 df ≈ n−1，z ≈ t_raw。

z 写回 `segment_metrics` 的 `t` 字段，所以下游的 `t_required(K)`、`VAL_MIN_T=1.65`、前向 `verdict` 的 1.65 门槛都不用改，仍然在正态尺度上比较。

实现约束：

- 不引入 scipy。Student t 的生存函数用正则化不完全 Beta（连分式）实现，并对照已知值做单测。
- `nw_t` 保留，其他研究脚本还在用。
- `greedy_ensemble.py` 的选择统计量同步改为新口径。
- `long_short`、`classify`、`t_required`、拆分方式和标签都不变。

## 2. 空模型校准（判定新口径能否采用）

- 用 `scripts/factor_mining/ic_overlap_audit.py` 增加新口径列，其余设置与审计完全相同：crypto 和 us 各 200 个种子（20261108+s），iid 与 AR(1) 0.98 两种随机因子，各 horizon 的训练段，共 12 组。
- **通过条件：** 12 组的 |z|>1.96 比例都落在 [2.0%, 8.0%]，并且 z 的标准差都在 [0.85, 1.15]。
- **不通过时的备选口径 B：** 偏移 0 的不重叠普通 t，df = 样本数 − 1，用同样方式换成 z，按同样条件校准。
- **B 也不通过：** 停止，不改评估器，如实报告。

## 3. zoo 重分类干跑（只写临时目录，不碰 `data/factor_mining` 里的实盘文件）

- **数据：** 以运行当时的 `data/factor_mining/{crypto,us}` 为输入，只读。K 取各 market 的 ledger 行数，与夜间任务相同。
- **对每个保留因子各算两遍：**
  - 旧口径：在干跑脚本里把新函数临时替换为 `nw_t(lag=h)`，不改冻结文件；
  - 新口径。
- 两遍都调用 `Miner._classify_and_correlate`，并按 `live_scores.select_ensemble_factors` 列出每个 horizon 的入选因子。
- **报告内容：**
  - 每个 horizon 的 accepted / candidate / redundant / rejected 数量（旧 vs 新）；
  - 新旧 accepted 名单的差异；
  - 加密 h=3 的入选因子是否变化（实盘模拟 `quant_targets` 用 h=3），以及 fallback 触发次数。
- **事先写下的预期：** 加密 h=7 的 accepted 大部分或全部掉出，美股 accepted 清零；加密 h=1 和 h=3 基本不变。

## 4. 部署规则

- 校准通过后才合并评估器改动。合并与部署（`deploy.sh` 同步到 `~/workspace/nofx-autoresearch`）都需要用户另行确认。
- **部署前：** 备份当时的 `data/factor_mining/{crypto,us}/zoo.json`、`ensemble.json`、`live_scores.json`、`quant_targets.json`。
- **部署后：** 夜间任务会自动用新口径重评估并重分类，评估器 SHA 写入 `status.json` 和 `runs.jsonl`，可以看出新口径从哪次运行开始生效。
- **如果干跑显示加密 h=3 的入选因子变化：** 模拟盘目标会随之改变，部署前单独告知用户。
- 这是评估方法修正，不是策略想法，不计入任何交易线的 K。

## 5. 已知事项

- 第 4 节审计时已经看过各准入因子在不重叠 t 下的数值，所以新口径对这些因子的结果不是盲测。口径的选择只依据空模型校准，不依据哪些因子会被保留。
- 选择均匀核滞后 h、以及用不重叠窗口数做小样本换算，是在本文件里事先确定的。如果校准不过，只允许退到备选 B，不再尝试其他核或滞后长度。
- 200 个种子只校准 1.96 附近。门槛 4.65 附近的尾部靠 df 换算，没有做模拟验证。
