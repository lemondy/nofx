# fq_us 市值/流动性分层拆分（U1 超大盘 / U2 中小盘 / U3 50-50 双袖套拆分）—— S1 预注册（2026-11-15）

本文件写于实现研究脚本和计算任何分拆回测收益之前。冻结参数一经运行不得修改；若脚本报错可修复代码 bug，但下列参数、切分、对照与判定阈值一律不变。

## 背景与研究目标

- **对应现状：** 当前美股程序化因子交易员 `1025e717`（基础动量）与 `490f8b28`（动量+波动率缩放）均采用全股票池统一横截面排序（`tier='all'`），在统一 20%/80% 分位里把超大盘（`ADV20 >= $500M`）与中小盘（`$10M <= ADV20 < $500M`）混合排序与配对。
- **核心问题（按 `INCREMENTAL_EVAL_DESIGN_2026-11-03.md`）：** 不看候选策略单独绝对收益，只检验**相对当前线上规则 `B`（`mom_ls_invvol_v1`, `tier='all'`）的配对增量 $\Delta = r_{\text{Arm}} - r_{B}$**——拆分成超大盘独立交易员（`U1`）、中小盘独立交易员（`U2`）或 50/50 双袖套并跑（`U3`）后，是否相对现行单池系统有稳定正向增益？
- **诚实声明：** `D2`（bStock90）与 `D3`（S&P 500 当前成分）均在前期 `fq_us` 研究中使用过且含现存成分幸存者偏差（未模拟退市与历史借券可得性）。本次检验严格仅作 **S1 离线筛选**；`fq_us` 账本当前累计 `K=8`，本次登记 3 个变体（`U1, U2, U3`），记 `K=9, 10, 11`，强阈值按保守 `K=11` 计算：$z_K = \Phi^{-1}(1 - 0.05 / (2 \times 11)) \approx 2.8364$。

## 冻结输入（只读）

| 数据集 | 角色 | 路径与预期哈希（复用 `fq_us_volscale_study.py`） |
|---|---|---|
| `D3`（S&P 500，498 股，2011-11-04..2026-11-03，176 期） | **主判定数据集** | `data/factor_mining/us_broad/daily` (`2e621c842446cb82ddb1f6dd8744b333bacd8817f0e6ef4ff39543a8f9fc4814`) |
| `D2`（bStock90，90 股，2020-11-04..2026-11-03，63 期） | **现行可交易池一致性对照** | `data/factor_mining/us_momentum/snapshot_20261105/daily` (`5bd711af9dbeef6d2131005aea89a5fc65897e3d47485145bd3868448d3e8042`) |

- 本研究使用与 `fq_us_volscale_study.py`、`fq_us_momentum_study.py` 完全一致的输入校验、SPY 交易日历、有效日线校验与双腿 Backtrader 引擎（`set_coc(True)`，多头 13 bp、空头 7 bp，初始资金多空各 `$50,000`，20 交易日调仓，50/50 调仓日等权重置 `stress.equalized_path`）。
- 强制不变式：在 `D2` 上基准 `B` 的独立双腿累计净收益必须精确复现 `+387.8078%`（`n=62`，容差 `5e-7`），否则拒绝继续。

## 无前视分层定义（Strict Causal ADV20）

在每个调仓日 `t`（仅使用 `<= t` 的日线数据）：

1. 先按基线 `s1.build_features` 判定基础合格股票集合 `eligible(t)`：`valid_count >= 253`、`close >= 5.0`、`20日成交额中位数 >= $10M`，且 `signal` 与 `volatility` 有限且 `> 0`。
2. 排除 `tiers.US_ETFS` 中的 ETF/杠杆工具（仅对股票分层，避免如 `SPY/QQQ/SOXL` 混入）；对剩余股票计算截至 `t` 的 20 个自然交易日均值 `ADV20(t) = mean(close * volume, window=20, min_periods=20)`：
   - **`mega_large`：** `eligible(t)` 且不在 `US_ETFS` 且 `ADV20(t) >= $500,000,000`。
   - **`mid_small`：** `eligible(t)` 且不在 `US_ETFS` 且 `ADV20(t) < $500,000,000`（`ADV20` 缺失不纳入分层）。

## 试验组与对照设计

1. **`B`（当前线上基线）：** `mom_ls_invvol_v1`（`tier='all'`，`min_eligible=15`，前/后 `floor(0.20 * N)`，`30日 1/\sigma` 加权，单名上线 `0.20` 腿内 / `0.10` 组合，调仓日多空各 0.50 等权重置）。
2. **`U1`（`fq-us-mega-large-only-v1`, Change Type `E`, `K=9`）：** 完全复用 `B` 的信号与权重规则，仅将候选池限定为 `mega_large`。当期合格数 `< 15` 时当期空仓持有现金。
3. **`U2`（`fq-us-mid-small-only-v1`, Change Type `E`, `K=10`）：** 完全复用 `B` 的信号与权重规则，仅将候选池限定为 `mid_small`。当期合格数 `< 15` 时当期空仓持有现金（`D2` 中期数 `< 15` 出现 10/63 期，主表严格沿用 `min_eligible=15`，并在描述表中单列 `min_eligible=10` 敏感性）。
4. **`U3`（`fq-us-tier-dual-sleeve-50-50-v1`, Change Type `E`, `K=11`）：** 独立运行 `U1`（50% 权益）与 `U2`（50% 权益），在每 20 个交易日调仓日收盘按 50/50 权益重置（复用 `stress.equalized_path`），实现与 `B` 相同的 100% 名义总敞口但多空配对完全隔离在各自市值层内部。

### 防错觉随机分层安慰剂对照（`placebo = 200`，固定种子 `20261115`）

为确认分拆增益来自市值/流动性分层而非随机缩小横截面池，对 `D3` 上每个候选组执行 `200` 次同规模无放回随机分层对照（仅使用与 `B` 一致的调仓期复利信号评估或在 Backtrader / 无交易费近似路径对比？**冻结选择：直接在 `D3` 上用快速无偏多空路径或 Backtrader 对比？为严格符合 `AGENTS.md` “涉及订单或盈亏的回测一律走 `scripts/backtest`（Backtrader）”，对每次安慰剂 `s=0..199` 均运行完整 Backtrader 双腿 `run_sleeve` 或通过预编译每日股票收益？注意 `176` 期 × `498` 股 Backtrader 跑 200 次会很慢；冻结精确加速口径：** 主表 `B, U1, U2, U3` 在 `D3` 和 `D2` 100% 走 Backtrader `run_sleeve`；随机安慰剂采用无交易费全清重配或与 Backtrader 同股数日频权重净值？为保证 100% Backtrader 且在 1–2 分钟内完成，我们先测试单次 Backtrader `D3` 耗时，若单次 <= 1s 则 `placebo=200` 并行全跑 Backtrader；若 > 2s，**明确预注册：** 安慰剂对照用 `--placebo 50` 或与 Backtrader 同成交价/手续费口径的周期精确权重结算并在报告核对 `B` 误差 `< 1e-6`；**更好方案：** 在 Backtrader 中只注册各期持仓目标并仅在 `n_workers=6` 上运行 `placebo=100` 次，先测基准时间后再跑。**为消除歧义，预注册冻结：`placebo_n = 100`（足够以 1% 分位精度检验 `>= 90%` 阈值），种子 `20261115 + s`，全部在 Backtrader `run_sleeve`（与主组完全相同的 `commission=0.0013/0.0007` 与 `50/50 equalized_path`）上执行！**）。

- 针对 `U1`/`U2` 安慰剂：在每个调仓日 `t`，从当日 `B` 的有效股票池（非 ETF）中无放回随机抽取数量等于 `|mega_large(t)|` 的股票作为伪 `U1` 池、剩余作为伪 `U2` 池，各自执行完全相同的 20%/80% + `1/\sigma` 规则。
- 针对 `U3` 安慰剂：取上述同一随机切分后的 50/50 双袖套等权组合。
- 记 `placebo_pct` = 安慰剂相对 `B` 的期均超额收益 `<` 观测 `Arm` 相对 `B` 的期均超额收益的比例；要求 `placebo_pct >= 0.90`。

## 判定规则（`fq_us`, S1，相对现行 `B` 增量对比）

在主数据集 **`D3`**（`n = 175` 个完整 20 日周期，`2012-11-07..2026-10-09`）上计算每个周期的收益差 $\Delta_k = r_{\text{Arm}, k} - r_{B, k}$：

- 前半段（`dev`，第 `1..87` 期）均值 $\bar{\Delta}_{\text{dev}}$；后半段（`holdout`，第 `88..175` 期）均值 $\bar{\Delta}_{\text{hold}}$。
- 合并统计量：$z = \bar{\Delta} / (\text{sd}(\Delta, \text{ddof}=1) / \sqrt{n})$。
- 按第一条满足顺序判定：

| 结论 | 条件 |
|---|---|
| `invalid` | `D2` 上 `B` 复现失败；或任一变体在 `D3` 上与 `B` 完全相同 |
| `fail` | `D3` 上 `z < 1.0`，或 `dev_delta <= 0`，或 `holdout_delta <= 0` |
| `fewer_trades` | 上述通过，但 `placebo_pct < 0.90`（未显著优于同规模随机拆池） |
| `strong` | `dev_delta > 0`、`holdout_delta > 0`、`placebo_pct >= 0.90`、`z >= z_K(11) = 2.8364`，且 `D2` 上 `mean_delta > 0` |
| `screen_pass` | `dev_delta > 0`、`holdout_delta > 0`、`placebo_pct >= 0.90`，且 `1.0 <= z < 2.8364`（或未在 `D2` 全满足） |

## 只做描述、不参与判定

1. `D3` 与 `D2` 上 `B, U1, U2, U3` 的累计净收益、年化收益、Sharpe、MaxDD、双边换手率、手续费、平均实际总敞口及分年收益。
2. 叠加现行波动率缩放规则 `V`（`k = clip(sigma_ref / sigma_hat, 0, 1)`，对应线上 `490f8b28`）后 `V(B), V(U1), V(U2), V(U3)` 的 Sharpe 与 MaxDD 对比。
3. `D2` 上 `U2` 采用 `min_eligible=10` 的敏感性检验。
4. `U1` 与 `U2` 周期收益的相关系数矩阵。
