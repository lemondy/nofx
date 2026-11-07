# fq_crypto 信号层：多特征机器学习打分与等权因子合成（S1 预注册，2026-11-07）

本文件写于实现任何代码、计算本检验任何收益或 IC 之前。先提交再运行；运行之后，下面的参数一律不得修改。写本文件前只做过一件事：用 `validate_expr` 检查下文 38 个特征表达式的语法，没有对任何数据求值。

## 背景

- **思路来源：** Kelly 与 Xiu 的综述《Financial Machine Learning》。核心主张是“复杂度的美德”：用岭回归或随机特征，把很多特征**一起**拿来预测截面收益，样本外反而更好（第 3–4 章；评估报告 `docs/research/2026-11-07-kelly-xiu-financial-ml-assessment.md`）。
- **和 nofx 现状的差距：** 当前挖掘是逐条搜索公式，每个因子单独评估，接受的因子再等权合成。没有任何一个模型同时学习多个特征。
- **和 H1 的区别：** H1（`fq-crypto-msrr-ridge-v1`，S1 fail）只改组合权重，信号仍是那 7 个因子。H1 的结论是问题出在信号层。本检验换的正是**信号层**：用模型分数替换等权合成分数，组合构建与 B 完全相同。

**诚实声明：**

- 验证段和前向段已被至少 5 项研究看过（ensemble-forward、wide-pool、pit60、style-neutral、msrr）。本检验只是离线筛选（S1），不构成样本外证据。
- 特征清单由主 agent 事先拟定，拟定时已知前几轮的结论：低波动和低 beta 风格有效、空头腿亏钱、资金费率有信息。这些先验会体现在特征选择里，属于研究者自由度。
- 后 7 个特征是线上 ensemble 的成分因子，它们是挖掘器在训练段上挑出来的。训练段的样本内表现会偏乐观，所以样本内只作描述。

**先验：** 中低。理由：
- 截面很窄，每日可用币数的中位数只有 41。
- 前几轮看到的有效信号大多是低波动风格，模型很可能只学到同一种风格。
- Alpha101 在加密货币上有 IC，但被换手吃掉了。

## 数据（只读，哈希冻结）

与 H1 使用同一快照，不重新下载。

| 输入 | 路径 | sha256 |
|---|---|---|
| 加密快照（共 303 个文件） | `data/factor_mining/style_neutral/snapshot_20261105/crypto/` | `f60602b5feb8061702afc4a2c53296e3c87ab077669477c9635a0da9826cc4f0`（按路径以 `LC_ALL=C` 排序后拼接内容） |
| zoo.json | 同上 | `cd9de8374450155e0f575da9e0ae8967722728910b16c064fb8f5eb0a17ad96e` |
| ensemble.json | 同上 | `5e3599f89dcaf4b73d4712a674c720004f25cd77eaaf08fb9c0f441a083feeb1` |
| 特征清单（下表 38 行，逐行 `名称\t表达式\n` 拼接） | 本文件 | `5511b1596ff4fb4679f01bfcf89c430a379bfca316f5f79d1eeae16a1ccc4e42` |

- 面板用 `panel.load_panels('crypto', root)` 读取，可用池用面板自带的 `mask`。
- 依赖只用现有 venv 里的 numpy 和 pandas。LightGBM、sklearn 都没有安装，本检验不安装任何新依赖，所以不做树模型。

## 时间切分

与 H1 相同，由 `panel.make_splits(index, 3, windows)` 给出：

| 段 | 区间 | 用途 |
|---|---|---|
| 训练 | 2022-05-09 → 2025-05-02 | 只在这一段拟合模型、选择收缩参数 |
| 验证 | 2025-05-08 → 2026-04-30 | 判定用 |
| 前向 | 2026-05-04 → 2026-11-03 | 判定用 |

## 特征（共 45 个）

### 通用特征（38 个，事先固定）

在全部币上用 `evaluate_expr(expr, panels.fields)` 求值。`CSDemean` 在面板全部有值的列上计算，不限可用池；可用池只在下一步截面排序时使用。

| 名称 | 表达式 |
|---|---|
| `mom_1` | `$ret` |
| `mom_3` | `$close / Ref($close, 3) - 1` |
| `mom_7` | `$close / Ref($close, 7) - 1` |
| `mom_14` | `$close / Ref($close, 14) - 1` |
| `mom_30` | `$close / Ref($close, 30) - 1` |
| `mom_60` | `$close / Ref($close, 60) - 1` |
| `mom_120` | `$close / Ref($close, 120) - 1` |
| `mom_7_30` | `Ref($close, 7) / Ref($close, 30) - 1` |
| `vol_7` | `Std($ret, 7)` |
| `vol_30` | `Std($ret, 30)` |
| `vol_90` | `Std($ret, 90)` |
| `skew_30` | `Skew($ret, 30)` |
| `kurt_30` | `Kurt($ret, 30)` |
| `max_30` | `Max($ret, 30)` |
| `min_30` | `Min($ret, 30)` |
| `hl_14` | `Mean(($high - $low) / $close, 14)` |
| `beta_60` | `Cov($ret, ($ret - CSDemean($ret)), 60) / Var(($ret - CSDemean($ret)), 60)` |
| `ivol_30` | `Std(CSDemean($ret), 30)` |
| `dvol_30` | `Log(Mean($close * $volume, 30))` |
| `vtrend` | `Log(Mean($volume, 7) / Mean($volume, 60))` |
| `amihud_30` | `Mean(Abs($ret) / ($close * $volume), 30)` |
| `vcv_30` | `Std($volume, 30) / Mean($volume, 30)` |
| `pvcorr_30` | `Corr($close, $volume, 30)` |
| `avcorr_30` | `Corr(Abs($ret), $volume, 30)` |
| `rpos_30` | `($close - Min($low, 30)) / (Max($high, 30) - Min($low, 30))` |
| `rpos_90` | `($close - Min($low, 90)) / (Max($high, 90) - Min($low, 90))` |
| `dhigh_60` | `$close / Max($high, 60) - 1` |
| `vwapdev` | `$close / $vwap - 1` |
| `madev_20` | `$close / Mean($close, 20) - 1` |
| `idxmax_30` | `IdxMax($high, 30)` |
| `body` | `($close - $open) / ($high - $low)` |
| `body_7` | `Mean(($close - $open) / $open, 7)` |
| `fund_1` | `$funding` |
| `fund_7` | `Mean($funding, 7)` |
| `fund_30` | `Mean($funding, 30)` |
| `fund_chg` | `Mean($funding, 7) - Mean($funding, 30)` |
| `prem_1` | `$premium` |
| `prem_7` | `Mean($premium, 7)` |

### 线上因子（7 个）

ensemble.json 中 h=3 的 7 个成分 `f_e09eaaab, f_501e330c, f_3b5aa5c2, f_6a1f7d05, f_0f777dd8, f_ee103900, f_d14097e5`。表达式和方向取自 zoo.json，值乘以方向。把它们放进特征，是为了让模型至少拿到 B 的全部信息：模型若不如 B，就说明多特征学习本身没有带来增量。

### 预处理（每个日期 t 单独做）

1. 只取当日可用池 `mask[t]` 内的币。
2. 每个特征在可用池内做截面平均秩，映射为 x = (rank − 0.5)/n − 0.5，落在 (−0.5, 0.5)。非有限值（NaN、±inf）不参与排序，排序后填 0（截面中位）。
3. 再乘以 √12，使每列在截面上的方差约为 1。
4. 可用池少于 10 个币的日期不进训练，也不打分。不打分的日期分数为 NaN，`rank_weights` 会把当天处理为全部现金。报告会列出验证段和前向段里这类日期的天数。

## 标签

- y = `panel.forward_target(panels, 3)` 的超额收益，即开盘 t+1 买入、开盘 t+4 卖出，减去可用池均值或基准。
- 在可用池内对有限值做截面平均秩，映射到 (−0.5, 0.5)。用秩是为了不让少数暴涨币主导拟合。
- 训练样本：训练段 `splits['train']` 内的全部日期 × 当日可用且标签有限的币，每天都用，标签允许重叠。

## 模型（两个判定臂，事先固定）

两个臂都是带岭收缩的闭式解，没有截距：特征和标签在截面上近似零均值。

### R：线性岭回归

- ŷ = x'β，β = (X'X/N + λI)⁻¹ X'y/N，P = 45。
- λ 网格：{10⁻⁴, 10⁻³, 10⁻², 10⁻¹, 1, 10} × trace(X'X/N)/P。

### RF：随机傅里叶特征岭回归（Kelly、Malamud、Zhou 的“复杂度的美德”设定）

- 随机特征：s(x) = √(1/Q)·[sin(γ ω_q'x), cos(γ ω_q'x)]，q = 1..Q，Q = 2048，共 P = 4096 维。
- ω_q 独立取自 N(0, I₄₅)，随机种子固定为 20261107，只抽一次。
- γ = 1/√45，按“中位距离”经验法则定带宽，不调参。
- 在 s(x) 上做岭回归，λ 网格与 R 相同（以 trace(S'S/N)/P 为尺度）。
- Gram 矩阵 S'S 按样本分块累加，不要求一次放进内存。

### 选 λ（两个臂各自独立）

- 训练段的日期按时间顺序分成 5 个连续块，做 5 折分块交叉验证。
- 每折用其余 4 块拟合；评估块两侧各剔除 5 个交易日（h+2）作为隔离。
- 评分：评估块内逐日的截面 Spearman 秩相关（预测对标签）的均值，5 个评估块的日 IC 拼接后求均值。
- 选 IC 均值最高的 λ；平局取较大的 λ。
- 选定 λ 后，用全部训练样本重新拟合，**之后冻结**，在验证段和前向段不再重估。

### 噪声组（每个臂各一个）

- N_R、N_RF：与对应臂完全相同（同特征、同网格、同选 λ 流程、RF 同一组 ω），只是训练标签在每个日期内对币随机置换。置换种子固定为 20261108。
- 用途：确认 Δ 来自信号，而不是“任何不同于 B 的组合都比 B 好”。

## 组合与执行（与 B 完全相同）

- **B（基线）：** 与 H1、style-neutral 研究的 B 相同。用 `ensemble.ensemble_scores` 合成分数，再用 `ensemble_forward_study.backtest(panels, score, start, end, 3)` 回测：每 3 日调仓，前后各 20%，等权，多空各 0.5；手续费 5 bp，滑点 3 bp，按实际资金费结算。
- **R、RF、N_R、N_RF：** 把模型在 t 日的预测 ŷ 作为 score，代入同一个 `backtest` 调用，其余参数一律不变。
- 验证段和前向段各自独立回测，不跨段持仓。

## 判定（交易线 fq_crypto，S1，按增量评估框架）

**统计量（每个臂）：**

- Δ_d = 该臂的日收益 − B 的日收益，验证段和前向段合并。
- z = mean(Δ)/SE，SE 用按 ISO 周聚类的标准误，与 `style_neutral_study.weekly_statistics` 相同。
- z_noise：该臂日收益减去其噪声组日收益，用同一公式计算。

**K：** fq_crypto 账本已有 4 次 S1 评估（wide-pool、pit60、style-neutral、msrr）。R 记 K = 5，RF 记 K = 6。z_K = Φ⁻¹(1 − 0.05/(2K))，即 R 为 2.576，RF 为 2.638，与 `incremental_filter.z_threshold` 相同。

| 结论 | 条件（按顺序取第一个满足的） |
|---|---|
| `invalid` | 配对日少于 100；或任一特征在训练段可用池内的非有限比例超过 50%；或模型预测出现非有限值。修评估本身，不计入 K |
| `strong` | z ≥ z_K；且验证段、前向段 Δ 均值都 > 0；且 z_noise ≥ 1.0 |
| `screen_pass` | z ≥ 1.0；且验证段、前向段 Δ 均值都 > 0；且 z_noise ≥ 1.0 |
| `fail` | 其余 |

两个臂分别判定，分别记账。不因为一个臂通过而改判另一个臂。

## 只描述，不参与判定

1. **复杂度楔子：** 每个臂选中的 λ、每个 λ 的交叉验证 IC、训练段样本内 IC、验证段和前向段的日均截面秩 IC 及其 t 值。
2. **回测指标：** B、R、RF、N_R、N_RF 两段的年化收益、夏普、最大回撤、换手、手续费、资金费。
3. **多空拆解：** R、RF 的多头腿、空头腿分别回测的盈亏，检查空头腿是否仍是瓶颈。
4. **与 B 的相似度：** 模型分数与 B 分数的逐日截面 Spearman 相关均值。相关很高说明模型只是重学了 ensemble。
5. **风格暴露：** 模型分数与 `vol_30`、`beta_60`、`dvol_30` 的逐日截面相关均值，检查是否仍然只是低波动和小市值风格。
6. **R 的系数：** 按 |β| 排序的 45 个系数。
7. **重训对照：** 用训练段加验证段重新拟合（λ 沿用已选值），只在前向段回测，记作 R′、RF′，用来看模型陈旧的影响。只作描述，不计入 K。

报告会披露“本研究共跑了 R、RF、N_R、N_RF、R′、RF′ 六组”。K 只按两个判定臂记。

## 账本

记两行，`scripts/backtest/evolution_ledger.append_event`：

| 字段 | R | RF |
|---|---|---|
| track | fq_crypto | fq_crypto |
| stage | S1 | S1 |
| change_type | E | E |
| idea_id | `fq-crypto-ml-ridge-v1` | `fq-crypto-ml-rff-v1` |
| baseline_version | `ensemble@5e3599f8` | `ensemble@5e3599f8` |
| preregistered_K | 5 | 6 |

另附 z、z_noise、两段 Δ、选中的 λ、输入哈希和特征清单哈希。

## 结果的用途

- **`strong` 或 `screen_pass`：** 另写在线配对设计。候选是用冻结模型打分的新 fq 模拟交易员，对照是现行 fq_crypto 交易员。需要单独预注册，并经用户批准。
- **`fail`：** 记录结论，线上不做任何改动。多特征学习在 fq_crypto 当前数据上也不能补救信号层，这个方向关闭，不再换模型重试（例如树模型），除非出现新的数据来源。
- **不论结果如何：** 不改挖掘器、不改夜间任务、不改正式交易配置，不启动交易，也不下单。

## 运行规则

- 输入哈希或特征清单哈希不一致时拒绝运行。
- 只运行一次。
- 脚本出错可以修复后重跑，但上面的任何参数都不得修改。重跑要在报告里披露。
