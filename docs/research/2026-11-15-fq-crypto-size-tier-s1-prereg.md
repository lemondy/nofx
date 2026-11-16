# fq_crypto 市值/流动性分层护栏（M1 限仓 / M2 过滤 / M3 分层重选）——S1 预注册（2026-11-15）

本文件写于实现研究脚本和计算任何回测收益之前。冻结参数一经运行不得修改；若脚本报错可修复代码 bug，但下列参数、切分、对照与判定阈值一律不变。

## 背景与动机

- **问题发现（2026-11-15）：**
  1. 在 `2026-11-15` 现行生产 `crypto/quant_targets.json`（`h=3`, `as_of=2026-11-13`, 7 因子 ensemble）中，20 个持仓里 `mid_small`（`ADV20 < $100M`）占 **11 个、名义暴露 61.22%**；空头腿 0.50 总暴露中，`mid_small` 高达 **0.4244（占空头 84.9%）**，`mega_large` 仅占 **0.0756（15.1%）**。
  2. Hidden-Sharpe 去偏分析显示，加密 `h=3` 因子动物园在全池（`theta0=0.0239, z=1.17`）与 `mega_large`（`theta0=0.0337, z=1.53, H1_L_SR=2.39`）仍有微弱正结构信号，而在 `mid_small` 子池几乎全为噪声（`theta0=0.0020, z=0.14`）。
  3. 交易端（Go `fqApplySizeTierGuardrails`）与因子消费端（Python `quant_targets.build_targets(..., tier=...)`）已实现分层限仓与过滤能力，现需在 Backtrader S1 离线筛选中检验：加入分层护栏相对当前 `tier='all'` 基线是否有**去除降杠杆错觉与随机选币安慰剂后的真实增量收益**。
- **诚实声明：**
  - `val`（`2025-05-04..2026-04-30`）与 `forward`（`2026-05-04..2026-11-06`）已被多项 `fq_crypto` 研究读取过（`K=9` 之前研究）。因此本检验仅作为 **S1 离线筛选**，不能当作未见过数据的样本外证明；若通过，必须经过独立 S2 线上配对验证（≥30 天、≥50 笔平仓）。
  - 当前 `fq_crypto` 账本累计 `K=9`（见 `data/evolution/ledger.jsonl`）。本次共检验 3 个主变体（M1, M2, M3），分别记为 `K=10, 11, 12`，判定强阈值统一按最保守的 `K=12` 计算：`z_K = Φ⁻¹(1 - 0.05 / (2 * 12)) ≈ 2.8652`。

## 冻结输入（只读）

| 输入 | 路径 | sha256 / 说明 |
|---|---|---|
| 加密面板快照（303 文件） | `data/factor_mining/fq_exit_funding/snapshot_20261108/crypto/` | `02de1af2f12421e123739256732071a7632532b33e9a5985522c2965dc59c316`（面板截止 `2026-11-06`） |
| 小时线（盘中 S20 止损校验） | `data/volrix/klines/<SYM>_1h.json` | 100 币，切片 `2025-05-01 00:00 UTC..2026-11-07 00:00 UTC`，运行前校验并记录 `hourly_sha256` |
| 主分析因子组（当前生产 7 因子 `h=3` ensemble） | 冻结自 `2026-11-15` `data/factor_mining/crypto/ensemble.json` + `zoo.json`（表达式见下表） | 7 个因子按 `ensemble.ensemble_scores` 等权合成，不读取可变生产目录 |
| 次要描述组（不参与判定） | `['f_0ce8b2ee']` | 与 `2026-11-08` `fq_shortexit_funding_study.py` 主因子口径对齐，仅输出描述表 |

**主分析 7 因子冻结定义（`horizon=3, direction=-1`）：**

1. `f_d462040d`: `(CSRank(EMA(Log(($high/$low)),40))-CSRank(IdxMax($close,20)))`
2. `f_25a5a0db`: `Corr(($vwap/Ref($close,1)),Log((($volume/Ref($volume,3))+1)),60)`
3. `f_880af0cb`: `(CSRank(Mean((($high-$low)/$close),30))+CSRank(Slope($close,20)))`
4. `f_3afb2c9b`: `(CSRank($premium)+CSRank(Mean((($high-$low)/$close),30)))`
5. `f_0ce8b2ee`: `(CSRank(EMA(Std(($high/$low),10),30))-CSRank(EMA($funding,30)))`
6. `f_7b6ef396`: `Mean((CSRank(EMA(($high/$low),30))-CSRank((Quantile($open,10,0.9)/$vwap))),40)`
7. `f_a40b13e0`: `Mean((CSRank(Max(($high/$low),20))-CSRank((Quantile($open,7,0.9)/$close))),90)`

## 共同执行口径（所有组完全一致）

- **时间切分（各自空仓起步，不跨段持仓）：**
  - `val`: `2025-05-04` 至 `2026-04-30`
  - `forward`: `2026-05-04` 至 `2026-11-06`
- **调仓时钟：** 每 3 个日历日（`dates[::3]`）在收盘 `t` 产生信号，`t+1 00:00 UTC` 开盘调仓；`status != 'ok'` 时沿用上一期目标权重。
- **执行与成本（Backtrader 原生）：**
  - `quantjourney.run_weights`，初始资金 `1000`；
  - 手续费 `0.0005`（5 bp/边），滑点 `0.0003`（3 bp）；
  - `CarryBroker` 按面板 `funding * 3` 逐日结算资金费率；
  - `PerNameExitPolicy(long_stop=0.20, short_stop=0.20)`（S20 固定止损），使用 `hourly` 小时线逐小时盘中触发；小时线覆盖缺口须 `<= 2%` 且与日线收盘偏差 `<= 0.2%` 的比例 `>= 99%`，否则整轮 `invalid`。
- **无前视分层判定（Strict Causal ADV20）：**
  - 在信号日 `t`，仅使用 `<= t` 的面板历史。
  - Go 执行端护栏（M1, M2）使用与 `fqCalcADV20` 完全一致的最近 20 个有效日线 bar 的 `close * volume` 均值，阈值 `$100,000,000`（`>= 1e8` 为 `mega_large`，`< 1e8` 为 `mid_small`；不足 20 根有效 bar 视为未分级）。
  - Python 目标层分层重选（M3）使用 `tiers.tier_mask(panels.loc[:t], 'crypto', 'mega_large')`（20 bar `vwap * volume` 均值 `>= $100M`）。

## 试验组与对照设计

### 基线 `B`（当前生产规则 `ls_invvol_protected_v1`）

- 60 币 core mask，前后 20% 分位，按 30 日波动率 `1/σ` 加权，多空各 0.5 总权重，单币上限 0.10；空头剔除 7 日收益前 10% 与上市 `< 90` 天币。不做任何市值/流动性分层限制。

### 候选组（均相对 `B` 检验）

1. **`M1`（`max_mid_small_exposure_pct = 25`；Change Type `B`；`K=10`；idea_id `fq-crypto-max-mid-small-25-v1`）**
   - 复现 Go `fqApplySizeTierGuardrails(..., MaxMidSmallExposurePct=25.0)`：
   - 对 `B` 在每个调仓日 `t` 的目标权重，统计所有因果判定为 `mid_small` 的绝对权重之和 `mid_gross`。
   - 若 `mid_gross > 0.25`，将全部 `mid_small` 权重按 `scale = 0.25 / mid_gross` 等比例缩减（截断到 `1e-6`，归零项剔除），`mega_large` 与未分级权重不变。
2. **`M2`（`size_tier = "mega_large"` 执行层过滤；Change Type `B`；`K=11`；idea_id `fq-crypto-mega-large-filter-v1`）**
   - 复现 Go `fqApplySizeTierGuardrails(..., SizeTier="mega_large")`：
   - 对 `B` 在每个调仓日 `t` 的目标权重，仅保留因果判定为 `mega_large` 的持仓（其余权重清零，保留原权重值不重新归一）。
3. **`M3`（`tier = "mega_large"` 目标层重选；Change Type `E`；`K=12`；idea_id `fq-crypto-mega-large-rebuild-v1`）**
   - 调用 `quant_targets.build_targets('crypto', ..., tier='mega_large')`：
   - 先把截面候选池严格限制在因果 `mega_large` 掩码内，再取前后 20% 分位、执行空头保护与 `1/σ` 加权（多空目标上限各 0.50、单币 0.10、合格币 `< 15` 时沿用上期）。

### 防错觉双对照

1. **等暴露对照 `B_x(Arm)`（每组每段独立计算）：**
   - 计算分段 `rho = mean_daily_gross(Arm) / mean_daily_gross(B)`，其中 `daily_gross = long_gross + short_gross`。
   - 将 `B` 的多空目标权重统一乘以 `rho`，在相同 Backtrader + 小时线条件下回测得到 `B_x(Arm)`。
   - 若 `mean_delta(Arm - B_x(Arm)) <= 0`，说明正收益差仅来自总体降仓位/少做亏钱基线，判为 `exposure_only`。
2. **随机分层安慰剂 `placebo`（`n = 200`，随机种子 `20261115 + s`）：**
   - **针对 M2：** 在每个调仓日 `t`，设 `B` 中被过滤掉的非 `mega_large` 持仓数为 `d_t`，从 `B` 当期全部有效持仓中无放回随机抽取 `d_t` 个剔除（仅重放与 M2 相同的权重清零规则）。
   - **针对 M1：** 在每个调仓日 `t`，设 `B` 中被判定为 `mid_small` 的持仓数为 `k_t`，从 `B` 当期全部有效持仓中无放回随机抽取 `k_t` 个作为伪 `mid_small`，应用完全相同的 `0.25 / pseudo_mid_gross` 缩减规则。
   - 计算 `placebo_pct = mean(placebo_mean_delta < arm_mean_delta)`；要求 `placebo_pct >= 0.90` 才算击败同比例随机剔除/限仓。
   - 注：M3 为整个候选池重构（Change Type E），与随机子集不直接同构，但仍可与 `B` 固定数量子集采样或跳过安慰剂？**预注册固定选择：** 对 M3 同样跑 `placebo = 200`：在每个调仓日 `t`，从 `B` 的 60 币 core mask 中无放回随机抽取与当期 `mega_large` 数量相同的候选子集，执行完全相同的 `build_targets` 20% 分位 + `1/σ` 重选，作为随机池子对照（`placebo_pct >= 0.90`）。

## 判定规则（`fq_crypto`, S1）

对每个主变体 `Arm in {M1, M2, M3}` 相对基线 `B`：

- 日差序列：$\Delta_d = r_{\text{Arm}, d} - r_{B, d}$（`val` 与 `forward` 拼接）。
- 统计量：$z = \overline{\Delta} / \text{SE}$，$\text{SE}$ 采用 `incremental_filter.weekly_statistics`（UTC 周一自然周聚类，带 $G/(G-1)$ 修正）。
- 按以下顺序取第一条满足的结论：

| 结论 | 条件 | 去向 |
|---|---|---|
| `invalid` | 小时线校验未通过；或变体在两段中从未改变任何一期权重 | 修数据/代码，不计入 `K` |
| `fail` | `z < 1.0`，或 `val_delta <= 0`，或 `forward_delta <= 0` | 记账，放弃，线上不改 |
| `exposure_only` | 上述通过，但 `mean_delta(Arm - B_x(Arm)) <= 0` | 记账（仅属降杠杆），不入 S2 |
| `fewer_trades` | 上述通过且击败 `B_x`，但 `placebo_pct < 0.90` | 记账（未击败随机分层），不入 S2 |
| `strong` | 全部通过（`val_delta > 0`, `forward_delta > 0`, 击败 `B_x`, `placebo_pct >= 0.90`），且 `z >= z_threshold(12)` | 优先推荐给用户决定是否开 S2 模拟对比 |
| `screen_pass` | 全部通过，且 `1.0 <= z < z_threshold(12)` | 推荐给用户作为 S2 候选 |

## 只做描述、不参与判定

1. 各组各段的累计收益 %、年化 %、Sharpe（简单日收益 × √365）、最大回撤 %、最差 3 日收益 %、换手率、手续费、滑点、多空分开资金费率、S20 止损次数、日均多空与总暴露。
2. 7 因子 vs `f_0ce8b2ee` 单因子的描述对比表（`secondary`）。
3. 交易端实盘/模拟盘策略不作任何自动修改；所有输出写入 `--out` 指定目录（快照目录外），仅在 `--write-ledger` 时追加 `data/evolution/ledger.jsonl`（3 条：`M1, M2, M3`）。
