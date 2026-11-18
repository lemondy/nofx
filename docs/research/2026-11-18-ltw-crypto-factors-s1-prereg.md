# Liu, Tsyvinski & Wu (2022)《Common Risk Factors in Cryptocurrency》复现与 `fq_crypto` 增量检验 — S1 预注册（2026-11-18）

本文件写于编写研究主逻辑及计算任何 LTW 因子收益、IC 或增量回测指标**之前**。一经提交并锁定 `PREREG_SHA256`，下列样本切分、因子定义、对照设计、成本口径与判定阈值一律冻结，跑完后不得修改。

## 1. 背景、文献依据与诚实声明

- **文献来源**：Yukun Liu, Aleh Tsyvinski, Xi Wu (2019/2022), *Common Risk Factors in Cryptocurrency*, NBER Working Paper 25882 / *The Journal of Finance* (77:2, pp. 1133–1177)。
- **论文核心主张**：
  1. 加密货币截面存在 **9 个显著异象**（共检验 24 个）：市值（`SIZE`）、币价（`PRC`）、21 日最高价（`MAXDPRC`）、成交额（`PRCVOL`）、成交额波动率（`STDPRCVOL`）以及 1–4 周时间序列动量（`r7`, `r14`, `r21`, `r28`）。
  2. 市场、市值与 3 周动量构成三因子定价模型 **`(CMKT, CSMB, CMOM)`**，解释显著截面溢价；其中 `CMOM`（`r21`）具有高 Sharpe 且与市值规模存在明显异质性。
- **与 `nofx` 现状的对照**：
  - 当前 `fq_crypto` 生产主链 `h=3`（`data/factor_mining/crypto/ensemble.json`）由 7 个**波动率/振幅/资金费率/溢价/量价相关性**因子构成（`f_0ea14e92`, `f_25a5a0db`, `f_880af0cb`, `f_3afb2c9b`, `f_0ce8b2ee`, `f_7616ba04`, `f_709d7407`），**完全不含纯截面收益率动量（`CMOM`）因子**。
  - `K=13, 14` 已否决单纯按市值/流动性切分层（`mid_small` 与 `50-50 dual sleeve`），因此本实验**不重复**市值切分层，而是检验 **Change Type `E`（信号模型增强）**：将 LTW 3 周截面动量 `r21` 作为正交信号分量加入现有 7 因子 `h=3` ensemble。
- **诚实声明与已知偏差**：
  1. **幸存者偏差（必披露）**：NAS MariaDB `crypto_klines` 与本地 `data/factor_mining/crypto/daily` 对应当前在架的 100 个 Binance USDT-M 永续合约，缺失历史下架/归零小币；因此 Part A 复现结果属于“活存主流永续合约池”下的严格样本外（2020–2026，完全晚于论文 2014–2018 样本）复现。
  2. **Size 代理口径**：永续合约 OHLCV 不含链上流通市值（Market Cap），`SIZE` / `CSMB` 统一采用因果 **20 日均成交额 `ADV20 = mean(quote_volume, 20)`**（与现有 `tiers.py` 口径一致）。
  3. **`val` / `holdout` 已读披露**：`data/factor_mining/crypto` 的 `val` 与 `holdout` 区间此前已被 `K=1..14` 多次读取；因此本轮仅作为 **S1 离线筛选**，即使通过（`screen_pass` / `strong`），仍须经独立 S2 线上配对检验（≥30 天、≥50 笔平仓）方能进入 S3。
  4. **账本预算 `K`**：当前 `data/evolution/ledger.jsonl` 中 `fq_crypto` 累计 `K = 14`；本预注册仅提交 **1 个主变体（`K = 15`）**。判定强阈值：`z_K(15) = Φ⁻¹(1 - 0.05 / (2 * 15)) ≈ 2.9352`。

## 2. 冻结输入、数据来源与统一回测引擎

- **行源 K 线**：局域网 NAS MariaDB `nofx.crypto_klines`（`market='binance_um'`, `interval='1d'`, 只读接口 `scripts/backtest/nas_klines.py:fetch_crypto_bars`），覆盖 `data/factor_mining/crypto/daily/*.json` 对应的 100 个标的（含 `BTCUSDT`），时间范围 `2020-01-01` 至 NAS 最新已收盘日（`<= 2026-11-14`）。
- **面板与屏蔽掩码**：`scripts/factor_mining/panel.py:load_panels("crypto", tier="all")` 的因果 `mask` 与 7 因子定义（`data/factor_mining/crypto/zoo.json`, `ensemble.json`）；**`evaluate.py` 保持只读不改**。
- **统一回测引擎**：全部分析统一使用 `scripts/backtest/nt_engine.py:run_portfolio`（NautilusTrader 1.222.0 + QuantStats 0.0.86）：
  - `market="crypto"`, `initial_capital=100_000.0`, `execution="next_open"`, `commission=0.0005`（5 bps/边）, `slippage=0.0003`（3 bps）, `periods_per_year=365`。

## 3. 实验结构（Part A 非 K 描述 + Part B1 跨越回归 + Part B2 预注册 S1）

### Part A：LTW (2022) 9 大显著异象样本外（2020–2026）独立复现（非 K）

- **调仓周期**：每周 1 次（每 7 个日历日 `t`，`t+1 00:00 UTC` 开盘成交），样本期 `2020-02-01` 至 `2026-11-13`（分全期、LTW 后半段 2020–2022 熊牛、2023–2026 阶段描述）。
- **准入条件**：在 `t` 日拥有至少 30 根有效日线且 `close > 0, ADV20 > 0`；每期合格币种 `< 15` 则空仓跳过。
- **9 个单因子多空五分位组合（Top 20% Long − Bottom 20% Short，按论文正溢价方向）：**
  1. `SIZE`：`-ADV20`（低成交额多、高成交额空）
  2. `PRC`：`-log(close)`（低名义价多、高名义价空）
  3. `MAXDPRC_21`：`-max(close, 21)`（低过去 21 日最高价多）
  4. `PRCVOL_20`：`-(close * volume)` 20 日均值（低美元成交额多）
  5. `STDPRCVOL_20`：`-std(close * volume, 20)`（低成交额波动多）
  6. `r7`：7 日动量 `close_t / close_{t-7} - 1`（高者多、低者空）
  7. `r14`：14 日动量 `close_t / close_{t-14} - 1`
  8. `r21`：21 日动量 `close_t / close_{t-21} - 1`（LTW 核心 `CMOM`）
  9. `r28`：28 日动量 `close_t / close_{t-28} - 1`
- **补充敏感性描述（非 K）**：
  - 比较 **等权（EW，多空各 +0.5/-0.5）** 与 **`ADV20` 加权（VW，多空各 +0.5/-0.5，单币上限 0.25）**；
  - 将 `r21` 按当期 `ADV20` 中位数拆分为 **Above-Median（大流动性半池）** 与 **Below-Median（中小流动性半池）**，检验论文动量市值异质性是否仍成立。

### Part B1：LTW 三因子模型对 `nofx` 现有 `fq_crypto` 基线的跨越检验（非 K）

- 构建每周调仓、经 NautilusTrader 扣费滑点执行的 3 因子收益序列（2022-05-22 至 2026-11-13）：
  - `CMKT`：全合格池 `ADV20` 加权（单币上限 0.25）纯多头日收益序列；
  - `CSMB`：按当期 `ADV20` 五分位 Bottom 20% 减 Top 20% 等权多空组合；
  - `CMOM`：按当期 `r21` 五分位 Top 20% 减 Bottom 20% 等权多空组合（及 `ADV20` 2×3 独立双排序 `(SH+BH)/2 - (SL+BL)/2` 对照）。
- 对 `fq_crypto` 现行 `h=3` 7 因子基线 `B` 的 NautilusTrader 净日收益做 OLS 跨越回归（Newey-West HAC 7 滞后标准误）：
  $$r_{B,t} = \alpha + \beta_{\text{MKT}}\text{CMKT}_t + \beta_{\text{SMB}}\text{CSMB}_t + \beta_{\text{MOM}}\text{CMOM}_t + \varepsilon_t$$
  - 输出年化 $\alpha$、$t(\alpha)$、各因子暴露与 $R^2$。

### Part B2：预注册 S1 增量检验（`track = fq_crypto`, `K = 15`, `change_type = E`）

- **时间切分（沿用 `panel.make_splits(..., h=3)` 规范口径，NAS 截断至 `2026-11-13` 确保 `next_open` 完整成交）**：
  - `val`：`2025-05-21` 至 `2026-05-15`
  - `holdout`（仅前向增量评估，不调参）：`2026-05-21` 至 `2026-11-13`
  - （附加描述 `train` `2022-05-22..2025-05-15`，不参与 S1 `val+holdout` 主检验或可单独列示。）
- **基线 `B`（`ls_invvol_protected_v1_h3_7f`）**：
  - 现行 `h=3` 7 因子 `ensemble.ensemble_scores` 等权合成 `z_7f(t)`；
  - 每 3 个日历日通过 `quant_targets.build_targets("crypto", ...)` 生成多空权重（前后 20%、30 日 `1/σ` 加权、空头剔除 `ret7` 前 10%、上市 `<90` 天过滤、多空各 0.5 总权重、单币上限 0.10）；
  - 经 NautilusTrader 执行 `next_open`（5 bps 手续费 + 3 bps 滑点）。
- **主候选 `C1`（`idea_id = fq-crypto-ltw-cmom21-blend-v1`, `K = 15`, `change_type = E`）**：
  - 对每个调仓日 $t$ 的合格池 `eligible_t`（与基线 `B` 完全相同的 `panels.mask` + `vol > 0` 掩码）：
    1. 计算 `z_7f_norm = zscore_cs(z_7f)`（截面均值 0、标准差 1；若 std=0 置 0）；
    2. 计算 `r21 = close_t / close_{t-21} - 1`，并在同一 `eligible_t` 内做截面标准化 `z_cmom = zscore_cs(r21)`；
    3. 按冻结等权混合生成候选评分：`z_cand = 0.5 * z_7f_norm + 0.5 * z_cmom`（零调参权重）；
  - 将 `z_cand` 传入**完全相同**的 `quant_targets.build_targets("crypto", ...)` 与 NautilusTrader 执行引擎，得到候选日净收益序列 $r_{C1, d}$。
- **防错觉与随机安慰剂对照（200 次置换）**：
  - 在每个调仓日 $t$，保持 `z_7f_norm` 不变，将 `z_cmom` 在 `eligible_t` 内部按独立种子（`20261118 + s`）做 **随机截面置换** 后再以 `0.5 * z_7f_norm + 0.5 * z_cmom_perm` 送入相同的 `build_targets` + NautilusTrader 回测（共 200 次）。
  - 计算 `placebo_pct = mean(placebo_mean_delta < c1_mean_delta)`，要求 `placebo_pct >= 0.90` 以证明增量来自 `r21` 方向信息而非对 7 因子评分稀释/扰动带来的分散化运气。
- **统计量与判定规则**：
  - 主序列：$\Delta_d = r_{C1, d} - r_{B, d}$（`val` 与 `holdout` 拼接日收益差）。
  - $z = \overline{\Delta} / \text{SE}$，$\text{SE}$ 采用 `scripts/backtest/incremental_filter.py:weekly_statistics`（UTC 周一自然周聚类 + $G/(G-1)$ 修正）。
  - 按顺序取第一条满足的条件：
    1. `invalid`：数据缺失或候选与基线权重完全一致；不计入 `K`。
    2. `fail`：`z < 1.0`，或 `val_delta <= 0`，或 `holdout_delta <= 0`；记账放弃。
    3. `fewer_trades` / `placebo_fail`：上述通过但 `placebo_pct < 0.90`；记账放弃（用 `verdict = "fewer_trades"` 或 `"fail"` 对齐账本约定）。
    4. `strong`：`val_delta > 0`、`holdout_delta > 0`、`placebo_pct >= 0.90` 且 `z >= z_K(15) = 2.9352`；推荐进入 S2 线上配对队列。
    5. `screen_pass`：`val_delta > 0`、`holdout_delta > 0`、`placebo_pct >= 0.90` 且 `1.0 <= z < 2.9352`；列入 S2 候选。

## 4. 执行与交付边界

- 仅写研究脚本 `scripts/backtest/ltw_crypto_study.py`、测试 `scripts/backtest/tests/test_ltw_crypto_study.py`、研究报告与研究日志；
- **严禁**修改生产配置文件、策略参数、`data/data.db` 或 `scripts/factor_mining/evaluate.py`；
- 实验完毕后通过 `scripts/backtest/evolution_ledger.append_event` 将 `K = 15` 结果追加至 `data/evolution/ledger.jsonl`。
