# Bailey & López de Prado（2014）《The Deflated Sharpe Ratio》深度评估与 `nofx` 因子库多重检验审计报告

## 1. 资料阅读记录与证据索引（100% 覆盖）

| 资料 | 作者 / 版本 | 页数 / 范围 | 阅读状态 |
|---|---|---|---|
| `05-研究可靠性/DeflatedSharpeRatio-2014-Bailey-LopezDePrado.pdf` | David H. Bailey & Marcos López de Prado, *Journal of Portfolio Management* (2014, 40:5) | 全文 13 页（正文章节 + Appendix A.1–A.3 Eq. 1–9；文本缓存 `scratch/papers/DeflatedSharpeRatio.txt`，983 行） | 已完整读完（100%） |

### 原文核心公式索引

1. **Expectation Max over $N$ 独立试次基准夏普门槛 $SR_0$**（Eq. 1）：
   $$SR_0 = \sqrt{\widehat{V}[\{SR_n\}]} \left[(1 - \gamma)\,\Phi^{-1}\!\left(1 - \frac{1}{N}\right) + \gamma\,\Phi^{-1}\!\left(1 - \frac{1}{N e}\right)\right], \qquad \gamma \approx 0.5772156649$$
2. **Deflated Sharpe Ratio（DSR）检验统计量（含非正态偏度/峰度惩罚，Eq. 2）**：
   $$\widehat{DSR} \equiv \widehat{PSR}(SR_0) = \Phi\!\left[\frac{(\widehat{SR} - SR_0)\sqrt{T - 1}}{\sqrt{1 - \hat{\gamma}_3 \widehat{SR} + \frac{\hat{\gamma}_4 - 1}{4}\widehat{SR}^2}}\right]$$
   其中 $\widehat{SR}$ 为未年化期夏普比，$T$ 为观测期数，$\hat{\gamma}_3$ 为偏度，$\hat{\gamma}_4$ 为**原始 Pearson 峰度**（正态为 `3`，非超额峰度）。判定标准通常为 $\widehat{DSR} \ge 0.95$。
3. **去冗余有效独立试次数 $\hat{N}_{\text{eff}}$（Appendix A.3 Eq. 8–9）**：
   $$\hat{N}_{\text{eff}} = \hat{\rho} + (1 - \hat{\rho}) M$$
   当 $M$ 个已评估候选因子/策略之间存在截面或收益序列平均相关性 $\hat{\rho} > 0$ 时，真正独立的搜索自由度应从**真实累计评估总试次数 $M$**（包含已被淘汰未写入 `zoo.json` 的候选，而非仅幸存子集）缩减为 $\hat{N}_{\text{eff}}$。

> **事实分类**：
> - **作者主张**：常规 Sharpe 与传统 Bonferroni 未同时考虑策略非正态性（$\hat{\gamma}_3, \hat{\gamma}_4$）、截面样本方差 $V[\{SR_n\}]$ 与真实搜索总试次相关性 $\hat{\rho}$。
> - **已有证据**：本报告编写 [`scripts/backtest/dsr_audit.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/dsr_audit.py) 与单元测试 [`test_dsr_audit.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/tests/test_dsr_audit.py)，完成：(1) 论文公式实现与 [`quantjourney.py:deflated_sharpe`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/quantjourney.py) 精度对账；(2) 从 `history.jsonl` 还原真实总评估试次 $M$（加密 `20,125`/周期、美股 `21,003`/周期）结合留存 `zoo.json`（`13,733` / `233` 个因子）估算相关性 $\hat{\rho}$ 与 $\hat{N}_{\text{eff}}$；(3) 现行生产加密 `h=3`（7 因子）与 `h=7`（`f_8dd9bbca`）在 **NAS MariaDB `crypto_klines` + `nt_engine.py`（NautilusTrader 1.222.0 + QuantStats 0.0.86，含 8h 永续资金费率与手续费/滑点）** 上的实盘口径 DSR 审计。

---

## 2. 对照 `nofx` 现有实现与实证审计结果（`dsr_audit.py`）

### 2.1 代码实现对账：`quantjourney.deflated_sharpe` 有实现但未接入因子挖掘主链

- 经 [`test_dsr_audit.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/tests/test_dsr_audit.py) 与 `audit_quantjourney_parity()` 校验，[`scripts/backtest/quantjourney.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/quantjourney.py) 中已有的 `deflated_sharpe` 与论文 Eq. (1)–(2) **数值完全一致（绝对误差 `0.0`）**。
- **系统缺口**：然而冻结的 [`scripts/factor_mining/evaluate.py`](file:///Users/zhangyun/workspace/nofx/scripts/factor_mining/evaluate.py) 与 [`ensemble.py`](file:///Users/zhangyun/workspace/nofx/scripts/factor_mining/ensemble.py) **从未调用 `deflated_sharpe`**，也未估计因子挖掘的真实总试次 $M$、有效独立试次数 $\hat{N}_{\text{eff}}$ 或验证期夏普方差 $V[\{SR_n\}]$；仅在增量研究阶段 [`incremental_filter.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/incremental_filter.py) 采用了跨实验账本 Bonferroni 阈值 `z_threshold(K)`。

### 2.2 全量挖掘历史（`history.jsonl` + `zoo.json`）有效试次 $\hat{N}_{\text{eff}}$ 与 DSR 审计

使用 `history.jsonl` 累计的 `raw_evaluated_candidates` 还原每个持有期 `h` 的真实搜索总试次数 $M$（加密总计 `60,375` 次评估 = `20,125` 次/周期；美股总计 `63,009` 次评估 = `21,003` 次/周期），并用 [`data/factor_mining/crypto/zoo.json`](file:///Users/zhangyun/workspace/nofx/data/factor_mining/crypto/zoo.json)（`13,733` 个留存因子）与 [`data/factor_mining/us/zoo.json`](file:///Users/zhangyun/workspace/nofx/data/factor_mining/us/zoo.json)（`233` 个留存因子）月度 IC 序列估算两两平均相关 $\hat{\rho}$（抽样上限 `400` 因子/周期，固定种子 `20261118`；注：汇总级 JSON 无逐日收益高阶矩，下表采用正态矩上界近似，肥尾惩罚见 2.3 节 NautilusTrader 真实净值重放）：

| 市场 | 周期 `h` | 真实总试次 $M$（留存 `zoo` 数） | 月度 IC 平均相关 $\hat{\rho}$ | 有效独立试次 $\hat{N}_{\text{eff}}$ | 验证期期 Sharpe 方差 $V[SR_{\text{val}}]$ | 验证期最大单因子 $SR_{\text{val}}$ | 现行 Ensemble 验证年化 SR（期 SR） | `SR₀(N_eff)` | **摘要正态近似 DSR (`N_eff`)** |
|---|---:|---:|---:|---:|---:|---:|---|---|---:|
| **Crypto** | `h=1` | 20,125 (`9,117`) | `0.4045` | **11,985.7** | `0.001726` | `0.1327` | `0.028` (`0.00148`) | `0.1622` | **`0.0011`** (`z=-3.05`) |
| **Crypto** | `h=3` | 20,125 (`4,425`) | `0.7346` | **5,341.7** | `0.002852` | `0.1283` | `0.328` (`0.02975`) | `0.1978` | **`0.0334`** (`z=-1.83`) |
| **Crypto** | `h=7` | 20,125 (`191`) | `0.7523` | **4,985.1** | `0.019942` | `0.2696` | `0.708` (`0.09809`) | `0.5206` | **`0.0016`** (`z=-2.95`) |
| **US** | `h=5` | 21,003 (`192`) | **`0.8829`** | **2,459.5** | `0.004420` | `0.0804` | *(无在线 ensemble)* | — | — |
| **US** | `h=10` | 21,003 (`41`) | **`0.8864`** | **2,386.4** | `0.014017` | `0.0077` | *(无在线 ensemble)* | — | — |

> **关键发现**：
> 1. 若仅按 `zoo.json` 留存数（如 `h=7` 留存 `191` 个、美股留存 `233` 个）计试次，会因忽略 99% 已淘汰候选（Selection Bias）严重低估多重检验门槛；还原 `history.jsonl` 真实 `20,125` / `21,003` 次评估后，即便按 `0.73–0.89` 高相关性折算，加密 `h=3`/`h=7` 仍有 **`5,341.7` / `4,985.1`** 个有效独立试次，美股仍有 **`2,386–2,459`** 个有效试次且留存验证均值深负（`-0.195` / `-0.375`）。
> 2. 在 `N_eff = 5,341.7` 下，加密 `h=3` 验证期期望极值门槛 $SR_0 = 0.1978$ 远高于现行 7 因子 ensemble 实现的期夏普 $SR = 0.02975$。

### 2.3 NautilusTrader + NAS MariaDB 现行加密 `fq_crypto` 真实净值重放（`val + holdout`，含 8h 资金费率）

读取 NAS MariaDB `nofx.crypto_klines`（100 标的 `1d`，`2025-05-01`..`2026-11-14`）及本地历史资金费率，通过 [`scripts/backtest/nt_engine.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/nt_engine.py)（`execution="next_open"`, 手续费 `5 bps`, 滑点 `3 bps`, `CarryFundingModule` 每日 3 次 8h 永续资金费率结算）严格重放生产主链 `h=3`（7 因子，`ls_invvol_protected_v1`）与 `h=7`（`f_8dd9bbca`），跨越 `542` 天完整 `val + holdout` 样本外区间：

| 生产主链 | 总收益 | 年化 CAGR | QuantStats Sharpe | MaxDD | 累计手续费 / 资金费净支出 | 期偏度 $\hat{\gamma}_3$ | 期 Pearson 峰度 $\hat{\gamma}_4$ | 单次尝试 DSR ($N=1$) | **考虑 $\hat{N}_{\text{eff}}$ 后的 DSR** | 考虑原始 $M$ 后的 DSR |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| **`h=3` (7 因子)** | `-2.39%` | `-1.62%` | `0.123` | `-40.58%` | `$5,739.6` / `$7,251.2` | `-0.558` | `3.885` | `0.5663` | **`0.0067`** (`z=-2.47`) | **`0.0035`** (`z=-2.70`) |
| **`h=7` (`f_8dd9bbca`)** | `-6.36%` | `-4.34%` | `-0.066` | `-42.32%` | `$3,740.5` / `$2,332.8` | `+0.145` | `3.084` | `0.4708` | **`0.0000`** (`z=-4.61`) | **`0.0000`** (`z=-5.03`) |

*(注：日度收益峰度更极端，达 `7.41`（`h=3`）与 `6.77`（`h=7`），日频 $\hat{N}_{\text{eff}}$ DSR 分别为 `0.0061` 与 `0.0000`。计入真实 8h 永续资金费率后，`h=3` 净收益从不含资金费时的微正转为 `-2.39%`。)*

### 2.4 对照 `ledger.jsonl` S1 门槛（`K` 账本）

| 赛道 | 累计 `K` | 现行 Bonferroni `z_K` | 独立极值门槛 $E[\max z_K]$ | $\rho=0.5$ 相关极值门槛 | 评估 |
|---|---:|---:|---:|---:|---|
| `fq_crypto` | 15 | **`2.9352`** | `1.7707` | `1.4590` | 账本对少量 S1 试验的 Bonferroni 控制是**充分且偏保守**的 |
| `fq_us` | 11 | **`2.8376`** | `1.6222` | `1.3001` | 同上 |
| `crypto_llm` | 11 | **`2.8376`** | `1.6222` | `1.3001` | 同上 |

**核心系统诊断**：`nofx` 的真实盲区**不在 S1 账本门槛**（`z_K ≈ 2.84–2.94` 已非常严格），而在于 **S0/因子挖掘前端允许累计 `20,000+` 次/周期的模型搜索试错后直接选出 `ensemble.json` 进入生产**，此前从未对其施加基于真实总试次 $\hat{N}_{\text{eff}}$、永续资金费率与非正态峰度的 DSR 诊断。

---

## 3. 建议落地的系统性改进（最多 3 项，无需消耗 S1 交易策略 `K`）

1. **在 `factor_mining` 夜间例行与研究审计管线中前置接入离线 DSR + $\hat{N}_{\text{eff}}$ 诊断报告（已接入 `run_nightly.sh`）**：
   - 将 [`scripts/backtest/dsr_audit.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/dsr_audit.py) 作为 `factor_mining` 产出 `ensemble.json` 后的**强制只读质检输出**（记录真实 `M`, `N_eff`, `SR_0`, `DSR`, 日峰度 `kurt` 与永续资金费率），**不修改冻结的 `evaluate.py`**。
   - **失败/淘汰条件**：当生产新候选 `ensemble` 在 NautilusTrader `next_open` 扣费滑点及资金费率下 $\widehat{DSR}(\hat{N}_{\text{eff}}) < 0.95$ 时，必须标明为 `OVERFIT_WARNING（样本内选择偏差高风险）`，禁止盲目宣传“年化高夏普”。
2. **抑制动物园无效试次膨胀，从根本上提升有效自由度比**：
   - 相比在 20,000+ 同构代数变异里继续堆叠运算符号，应控制每日变异预算并强制新因子与现有因子月度 IC 相关性 `< 0.5`。
3. **非正态尾端与资金费率完整惩罚**：当前实盘 `h=3`/`h=7` 日峰度均在 `6.8–7.4` 且永续资金费率影响显著，未来评估新策略净值曲线时一律使用 [`nt_engine.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/nt_engine.py)（含 `CarryFundingModule`）输出的完整非正态矩计算 DSR。

---

## 4. 交付评级与复现命令

- **交付评级**：**历史有效 + 研究方法缺陷定位通过** — 论文公式在本地 100% 复现并与已有 `quantjourney.py` 达成 `0.0` 误差对账；还原 `history.jsonl` 真实总试次与 8h 永续资金费率后，NautilusTrader + NAS MariaDB 全量重放证实当前 `fq_crypto` 主链在 `val + holdout` 上 `DSR = 0.0067`（`h=3`）与 `0.0000`（`h=7`），精确定位了为何此前验证夏普看似正值却在模拟盘停滞。
- **复现命令**：
  ```sh
  PYTHONPATH=. PYTHONDONTWRITEBYTECODE=1 .venv/bin/pytest scripts/backtest/tests/test_dsr_audit.py -v
  PYTHONPATH=. PYTHONDONTWRITEBYTECODE=1 .venv/bin/python -m scripts.backtest.dsr_audit
  ```
- **局限性**：`history.jsonl` 仅记录被淘汰候选的总计数而未保存其月度 IC 序列，故两两相关 $\hat{\rho}$ 与截面方差 $V[SR_{\text{val}}]$ 由留存 `zoo.json` 子集估计（报告中已显式标注 `corr_var_basis = retained_zoo_subset`）；`nt_live_crypto_replay` 未叠加额外历史已下架币种。
