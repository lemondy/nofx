# Ye 等（2026）《AlphaIllusion: Why LLM Trading Alpha Is Not Deployment Evidence》深度评估与 `nofx` 实盘路径审计报告

## 1. 资料阅读记录与证据索引（100% 覆盖）

| 资料 | 作者 / 版本 | 页数 / 范围 | 阅读状态 |
|---|---|---|---|
| `04-LLM交易/AlphaIllusion-LLM交易alpha不可作部署证据-2026-arXiv-2605.16895.pdf` | Ye et al., arXiv:2605.16895 (2026-05) | 正文 Sections 1–6 + 附录 Appendices A–E（文本缓存 `scratch/papers/AlphaIllusion.txt`，843 行） | 已完整读完（100%） |

### 原文关键公式与命题索引

1. **P1 信息泄露（Information Leakage）**（Section 2.1）：基于固定发布日期的大语言模型存在参数记忆（parametric memory）污染历史回测的风险，历史窗口越早，记忆越深。
2. **P2 标的池错配（Asset Universe Mismatch）**（Section 2.2）：多数公开基准只涵盖少数高市值高流动性主流标的，与真实交易标的分布（或长尾小币/新股）存在严重结构差异。
3. **P3 反事实收益率缺口（Counterfactual Return Gap）**（Section 2.3）：LLM 输出“不交易/观望”或触发执行拒绝后，实际资金利用率、等待成本与错失机会构成了相对连续信号组合的真实缺口。
4. **P4 认知过度自信（Epistemic Overconfidence, ECE Eq. 3）**（Section 3.1）：
   $$\text{ECE} = \sum_{m=1}^M \frac{|B_m|}{N} \left|\text{acc}(B_m) - \text{conf}(B_m)\right|$$
   论文实证发现 LLM 自我报告的置信度存在系统性高估（Overconfidence），且内部置信度排序与单笔收益几乎无单调相关，**不可直接用作开仓门控或仓位缩放依据**。
5. **P5 总收益到净收益退化与推理成本拖累（Gross-to-Net + Inference Cost, Eq. 1）**（Section 2.4 & 3.2）：
   $$R_{\text{true\_net}} = R_{\text{gross}} - \text{Fees} - \text{Slippage} - C_{\text{LLM\_tokens}}$$
   论文强调小账户下 LLM API 调用成本极易吞噬毛利润的绝大部分甚至转为净亏。
6. **P6 / 2.4 统计窗不确定性与 Lo (2002) 夏普标准误（Eq. 2）**（Section 2.4）：
   $$\widehat{\text{SE}}(\widehat{SR}) = \sqrt{\frac{1 + \frac{1}{2}\widehat{SR}^2}{T}}, \qquad \text{MinTRL} = 1 + \left(1 - \hat{\gamma}_3 \widehat{SR} + \frac{\hat{\gamma}_4 - 1}{4}\widehat{SR}^2\right)\left(\frac{z_{1-\alpha}}{\widehat{SR}}\right)^2$$
   短期 1–3 个月的实盘或模拟盘正收益完全落在 $95\%$ 置信区间内，不能作为“已稳定盈利”的部署证据。

> **事实分类**：
> - **作者主张**：LLM 在常规基准上看似可观的回测 alpha 大多无法承受 P1–P6（尤其 P4 校准失真 + P5 token 成本 + P6 Lo 标准误）的实盘检验。
> - **已有证据**：下文使用 [`scripts/backtest/alpha_illusion_audit.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/alpha_illusion_audit.py) 对 `nofx` 自身 [`data/data.db`](file:///Users/zhangyun/workspace/nofx/data/data.db) 的 `945` 笔已平仓交易与 `decision_records` 做了零修改实证审计（2026-11-19 已按 Go [`store/position.go`](file:///Users/zhangyun/workspace/nofx/store/position.go) 真实口径修正：`trader_positions.realized_pnl` 为税费前毛盈亏，真实交易净盈亏为 `net_pnl = realized_pnl - fee`，并以双键 `(trader_id, entry_order_id)` 关联决策置信度）。
> - **系统改进假设**：将 `MinConfidence` 硬门控与实盘置信度重新校准 / 消融，可去除无效决策摩擦并验证推理成本（token 费）净回收空间。

---

## 2. 对照 `nofx` 当前系统的实证审计（`alpha_illusion_audit.py`）

审计脚本：[`scripts/backtest/alpha_illusion_audit.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/alpha_illusion_audit.py)（单元测试 [`test_alpha_illusion_audit.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/tests/test_alpha_illusion_audit.py) 5/5 通过）。读取 `data/data.db`（严格 `mode=ro` 只读），覆盖 `945` 笔已平仓交易（其中 `515` 笔精确匹配 `(trader_id, entry_order_id)` 决策置信度）。

### 2.1 P4 认知校准审计（`kernel/` 硬门槛直接命中痛点！）

- **代码对照**：`nofx` Go 内核当前直接将 LLM 输出的 `confidence`（0–100）作为开仓硬门控：
  - 加密侧 [`kernel/engine_prompt.go`](file:///Users/zhangyun/workspace/nofx/kernel/engine_prompt.go#L105-L106) 与 [`store/strategy.go`](file:///Users/zhangyun/workspace/nofx/store/strategy.go#L1242)：由 `risk_control.min_confidence`（默认 `70`–`75`）硬拒绝低置信度开仓；
  - 美股侧 [`kernel/stockengine/validate.go`](file:///Users/zhangyun/workspace/nofx/kernel/stockengine/validate.go#L10-L12)：硬编码 `MinEntryConfidence = 80`，低于 `80` 返回 `"LOW_CONFIDENCE"` 拒绝。
- **真实数据库实证结果（`n = 515` 笔匹配已平仓 LLM 交易，按扣费后真实净盈亏计胜负与 `avgR`）**：
  - **10-bin ECE = `0.2462`**；平均自报胜率 **`73.36%`** vs 真实扣费后实现胜率 **`48.74%`**（**系统性高估 +24.62 个百分点**）。
  - **置信度与实际交易结果几近零相关**：Spearman $\rho(\text{conf}, \text{win}) = +0.0262$，$\rho(\text{conf}, \text{realized\_R}) = +0.0231$。

| 自报置信度区间 | 成交笔数 `n` | 平均自报置信度 | 真实扣费胜率 | 真实均值 `avgR` | 扣费净盈亏（USD） |
|---|---:|---:|---:|---:|---:|
| `[0, 70)` | 5 | 58.00% | 20.00% | `-0.1356R` | `+$1.75` |
| `[70, 75)` | 403 | 72.62% | 48.88% | `-0.0221R` | `-$46.98` |
| `[75, 80)` | 93 | 76.15% | 51.61% | `+0.0102R` | `+$97.92` |
| `[80, 85)` | 12 | 80.83% | 33.33% | `-0.0373R` | `-$26.17` |
| `[85, 101)` | 2 | 85.00% | 50.00% | `-0.1109R` | `-$1.81` |

> **关键发现**：在真实扣费成交上，主力档 `[70, 75)`（`403` 笔）扣费后已转为净亏损 `-$46.98`（`avgR = -0.0221R`）；而置信度 `>= 80`（恰恰是美股 `MinEntryConfidence = 80` 强制要求的门槛）的 `14` 笔交易扣费胜率只有 `35.71%`、净亏损 `-$27.98`，显著劣于 `[75, 80)`！这以 `nofx` 真实交易样本直接复现了 **AlphaIllusion Section 3.1（P4）**。

### 2.2 P5 扣费 + AI Token 成本净收益与 P6 Lo (2002) 窗口不确定性

对数据库全量核心交易员统计完整日历日净盈亏（按 UTC 平仓日聚合 `net_pnl = realized_pnl - fee`，空白日补 `0` 避免活跃日选择偏差）与 `ai_charges` Token 账单：

| 交易员 | 笔数 | 扣费胜率 | `avgR` | 毛盈亏 (`realized_pnl`) | 交易所手续费 | 扣费净盈亏 | AI 记账次数 | AI 记账成本 | **扣 AI 成本真净利** | 日历天数 | 年化 Sharpe ± SE (95% CI) | `MinTRL`（天） |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|
| `mac-nofx` | 464 | 50.22% | `+0.1215` | `+$201.85` | `$26.98` | `+$174.87` | 12,639 | **`$121.99`** | **`+$52.87`** | 82 (`08-29..11-18`) | `4.229 ± 2.135` `[0.044, 8.415]` | 30.7 |
| `因子 ensemble-对照` | 136 | 45.59% | `-0.0126` | `+$101.39` | `$76.36` | `+$25.02` | 6,260 | **`$62.60`** | **`-$37.58`** | 29 (`10-21..11-18`) | `0.599 ± 3.549` `[-6.356, 7.554]` | 2749.8 |
| `因子 ensemble-候选` | 100 | 47.00% | `-0.0248` | `-$25.43` | `$64.20` | `-$89.63` | 6,803 | **`$68.03`** | **`-$157.66`** | 29 (`10-20..11-17`) | `-1.846 ± 3.556` `[-8.816, 5.124]` | `inf` |
| `提前减仓 0.8R-候选` | 71 | 54.93% | `+0.0067` | `+$157.76` | `$41.99` | `+$115.77` | 4,007 | `$40.07` | `+$75.70` | 19 (`10-24..11-11`) | `3.092 ± 4.412` `[-5.555, 11.739]` | 90.5 |
| `提前减仓 0.8R-对照` | 67 | 58.21% | `+0.0276` | `+$94.38` | `$37.27` | `+$57.12` | 4,057 | `$40.57` | `+$16.55` | 19 (`10-24..11-11`) | `1.454 ± 4.389` `[-7.149, 10.057]` | 482.3 |
| `因子量化-加密（模拟）` | 50 | 40.00% | — | `-$55.14` | `$20.24` | `-$75.38` | 0 | `$0.00` | `-$75.38` | 16 (`11-03..11-18`) | `-0.429 ± 4.777` `[-9.792, 8.934]` | `inf` |

> **关键发现**：
> 1. **交易所手续费与 AI Token 成本双重侵蚀绝大部分毛利**：全库 `945` 笔已平仓合计毛盈亏 `+$410.75`，扣除交易所手续费 `$278.73` 后净交易盈利仅剩 **`+$132.02`**（手续费吞噬 `67.9%` 毛利）！即便是系统内表现最好的 `mac-nofx`，82 天毛利 `+$201.85` → 扣交易所费用后净利 `+$174.87` → 再扣除账单记录的 `$121.99` AI 费用后**仅剩 `+$52.87`（吞噬 69.8% 费后净利；注：即便剔除历史 Regime skip 幽灵记账约 `$47.3` 后，真实 ~7,910 次 LLM 调用仍耗去约 45% 费后净利）**；而 `因子 ensemble` 对照/候选组在计入交易费与 AI 费后双双转为净亏损（`-$37.58` / `-$157.66`）。
> 2. **P6 置信区间极宽**：`mac-nofx` 扣费后 95% CI 下界仅 `0.044`，其余全部 16–29 天的 S2 模拟对 `SE(SR)` 高达 `3.5–4.8`，`95% CI` 全部横跨 0（例 `[-6.36, 7.55]`），证实论文 P6 警告与 `paper-promotion.md` 7–30 天样本不足结论完全一致。

### 2.3 P2 标的池构成与 P3 行动漏斗分布

- **P2 标的池拆解（全量 `945` 笔，扣费口径）**：
  - `crypto_alt`（山寨永续）：`853` 笔，胜率 `50.06%`，`avgR = +0.0336`，毛利 `+$918.41`、手续费 `$253.32`、**扣费净赚 `+$665.09`**（`mac-nofx` 上 `423` 笔 `avgR = +0.1230`、净赚 `+$163.10`）；
  - `crypto_major`（15 个主流币）：`61` 笔，胜率 `45.90%`，`avgR = +0.3227`（少数高盈亏比小仓拉高均值），毛亏 `-$414.22`、**扣费净亏 `-$431.16`**（存在高杠杆/大仓亏损）；
  - `tokenized_us_etf`（代币化美股/ETF）：`31` 笔，扣费胜率 **`22.58%`**，`avgR = -0.2481`，毛亏 `-$93.43`、**扣费净亏 `-$102.31`**（在 `mac-nofx` 上 `9` 笔 `avgR = -0.5039`）。
- **P3 决策漏斗（`decision_records` 全量动作计数）**：`wait = 147,074`、`hold = 39,530` 占全部动作的 `98.6%`，实际触发开仓（含限价）仅 `1,957` 次（`1.03%`），且风控与合约拒绝（如 `ALREADY AT MAX POSITIONS`、TradFi 合约未签 `-4411`）进一步拦截，意味着绝大部分 LLM 调用花费在高频 `wait`/`hold` 空转上。

---

## 3. 可检验假设排序与后续 S1 建议

| 优先级 | 假设与改动机制 | 所需数据与口径 | 失败条件 |
|---|---|---|---|
| **P1（已在 `crypto_llm K=10..11` 检验）** | **降低无效周期调用频率 / 事件驱动触发（降 P5 推理成本）**：合并非高波动期间的多余轮询，或用轻量规则门控免除全阻断周期的 LLM 调用 | `data/data.db` 历史循环间隔波动率 + 真实 LLM 调用计价（见 [`2026-11-18-crypto-llm-cost-gating-s1-study.md`](file:///Users/zhangyun/workspace/nofx/docs/research/2026-11-18-crypto-llm-cost-gating-s1-study.md)） | 漏失收益 `Δ_R` 大于节省的 `C_LLM_tokens`，或未击败同跳过率随机对照（`K=10..11` 已判 `fewer_trades`；但定位出的 Go `Regime skip` 幽灵记账已直接作为工程 Bug 修复） |
| **P2（立项 `us_llm` / `crypto_llm` S1）** | **解除/下调未经校准的 `MinConfidence` 硬门控**：测试将美股 [`MinEntryConfidence = 80`](file:///Users/zhangyun/workspace/nofx/kernel/stockengine/validate.go#L12) 下调或改为事后经验胜率（Platt/Isotonic 校准），仅作为风险预算权重而非硬截断 | `decision_records` 中历史因 `LOW_CONFIDENCE` 被拒信号的反事实价格轨迹（需补齐影子上行追踪） | 下调后引入的尾端交易平均 `avgR` 显著为负且降低组合净夏普 |
| **P3（待样本累积）** | **禁用 LLM 路径下的 `tokenized_us_etf` 高成本边缘标的**：`31` 笔扣费胜率 `22.58%`、净亏 `-$102.31`，但在 `n=31 < 100` 下暂不足以单独跑 formal S1 | 待此类标的平仓累积至 `>= 100` 笔或直接归入既有 `crypto_llm` 范围控制 | 后续大样本胜率回升至 `> 45%` |

---

## 4. 交付评级与下一步

- **交付评级**：**历史有效（审计实证成立）** — 在不改动任何实盘交易配置的前提下，已使用真实数据库与单元测试 [`scripts/backtest/alpha_illusion_audit.py`](file:///Users/zhangyun/workspace/nofx/scripts/backtest/alpha_illusion_audit.py) 验证 `P2/P4/P5/P6` 全部四项结构性退化机制均在 `nofx` 真实存在（尤其 `ECE = 0.2462`、全库手续费吞噬 `67.9%` 毛利，以及 AI 成本显著侵蚀净利）。
- **复现命令**：
  ```sh
  PYTHONPATH=. PYTHONDONTWRITEBYTECODE=1 .venv/bin/pytest scripts/backtest/tests/test_alpha_illusion_audit.py -v
  PYTHONPATH=. PYTHONDONTWRITEBYTECODE=1 .venv/bin/python -m scripts.backtest.alpha_illusion_audit
  ```
- **局限性**：`decision_records` 中被 `LOW_CONFIDENCE` 拒单记录尚未接成完整影子 K 线反事实 PnL 流水，P3 反事实损失率尚未做逐币价格对齐；正式配置与 `data/data.db` 保持只读未改。
