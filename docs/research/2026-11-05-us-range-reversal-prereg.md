# 美股候选排序：52 周区间位置 / 6 个月反转 vs 现行 12-1 动量（S1 预注册，2026-11-05）

> 本文件在写任何评估代码、看到任何结果之前提交，提交后不再修改。实现与本文不一致时以本文为准，偏离须在结果文档中逐条列出。

## 来源与假设

- 来源：Yartseva (2025) *The Alchemy of Multibagger Stocks*（CAFE Working Paper 33）。论文的动态面板模型中，6 个月动量系数为负，股价越接近 12 个月高点，下一年收益越低，即"趋势快速反转"。
- 注意：论文样本是事后挑出的 10 倍股（按结果选样），这些系数不能直接当作选股规则。本研究只检验其中可在我们数据上复现的技术面部分。
- 现行做法：us_stock 扫描候选按 `TopMomentum`（`kernel/stockengine/scan.go`）选出：动量有效（≥253 根日线）且收盘价高于 SMA200 的标的中，按 mom12_1 从高到低取前 `scan_top_n=5`。
- 假设：在同一合格池内，按"52 周区间位置从低到高"或"6 个月动量从低到高"取前 5，未来 20 个交易日收益高于现行动量排序。
- 改动类型 E（候选池/排序），track `us_llm`。离线只做筛选（S1），通过也只进线上配对队列。

## 数据

- 路径：`/Users/zhangyun/workspace/nofx/data/factor_mining/us/daily/*.json`（只读）。101 个 bStock 标的的 Yahoo 复权日线，行格式 `[date, open, high, low, close, volume, typical]`，区间 2020-11-03 → 2026-11-02。
- 输入校验：按文件名排序后拼接全部文件字节的 sha256 必须等于 `67b64d0577f9d24ebd6908e51f17d85ec3559e6f77064b736d02b99d2337d82c`；不一致时拒绝运行，除非显式传入 `--allow-input-drift`（并在报告中记录）。
- **幸存者偏差**：标的是 2026-11 仍在 Binance 交易的 bStock，历史上已退市或被剔除的股票不在内。结论只适用于"现有 bStock 池内的排序"，不代表全市场。

## 币池（个股，90 只）

排除以下 11 个 ETF / 杠杆或反向 ETF（类型来自 Yahoo `instrumentType`，2026-11-05 核对）：

| 代码 | 说明 |
|---|---|
| SPY | S&P 500 ETF（基准） |
| SMH | 半导体 ETF |
| EWY | 韩国 ETF |
| TMF | 3 倍 20 年期美债 ETF |
| DRAM | 存储主题 ETF |
| KSTR | 科创 50 ETF |
| INTW | 2 倍做多 INTC ETF |
| MVLL | 2 倍做多 MRVL ETF |
| SKUU | 2 倍做多 SK 海力士 ETF |
| SKDD | 2 倍做空 SK 海力士 ETF |
| SNXX | 2 倍做多 SNDK ETF |

线上 `scan_exclude` 里的 MUU、KORU、SOXL、SOXS、TQQQ、SQQQ、UVXY 不在本数据集中。其余 90 个文件全部为个股（`instrumentType=EQUITY`）。

## 日历与对齐

- 主日历：SPY 的交易日（SPY 只用作日历，不进币池）。
- 标的在日期 t 有值，要求该标的在 t 当天有 bar；所有窗口都按该标的自己的 bar 序号计算（第 t 根往前数）。
- 前向收益 fwd_h(t) = close[t+h] / close[t] − 1，按该标的自己的 bar 序号；没有第 t+h 根时该标的在该日不参与。

## 信号（收盘 t 时可得）

| 名称 | 定义 | 排序 |
|---|---|---|
| B（基线） | mom12_1 = close[t−21] / close[t−252] − 1，要求至少 253 根 bar | 从高到低 |
| V1 | range_pos = (close[t] − min(close[t−251..t])) / (max(close[t−251..t]) − min(close[t−251..t]))，使用收盘价，窗口 252 根；max = min 时无值 | 从低到高 |
| V2 | mom6 = close[t] / close[t−126] − 1 | 从低到高 |

- 合格池（与 `TopMomentum` 一致）：mom12_1 有值，且 close[t] > SMA200（最近 200 根收盘均值），且该日 fwd20 有值。
- 同值按代码字母序打破。
- 每个日期在合格池内分别按 B、V1、V2 取前 5。合格池少于 10 只的日期跳过（保证两组前 5 可以完全不同）。

## 主检验（每个变体一条 S1）

- 每日差：Δ_t = 变体前 5 的 fwd20 等权均值 − B 前 5 的 fwd20 等权均值（原始收益，市场部分相减抵消；不计成本，换手单独报告）。
- 统计量：mean(Δ)，SE 用 Newey-West（Bartlett 核，滞后 19），z = mean / SE。
- 两半：按日期排序，前 ceil(n/2) 个日期为前半，其余为后半，分别给出 mean(Δ)。
- 判定（与 `scripts/backtest/replay_score.screen_verdict` 语义一致，首条命中为准）：

| 判定 | 条件 |
|---|---|
| `invalid` | 有效日期 < 100 |
| `strong` | z ≥ z_K，且两半均值都 > 0 |
| `screen_pass` | z ≥ 1.0，且两半均值都 > 0 |
| `fail` | 其余 |

- z_K = Φ⁻¹(1 − 0.05 / (2K))，K 由 `scripts/backtest/evolution_ledger.append_event` 在锁内按 track `us_llm` 计数。先记 V1，再记 V2（当前 us_llm 无记录，预期 K = 1、2）。
- 账本：主仓库 `data/evolution/ledger.jsonl`，每个变体一行：stage `S1`，change_type `E`，idea_id `us-range-pos-v1` / `us-mom6-reversal-v1`，baseline_version `us_stock:TopMomentum:mom12_1:top5`，字段含 verdict、delta、se、z、z_K、dev_delta、holdout_delta、n_days、trade_ratio（变体前 5 平均日换手 / B 前 5 平均日换手）、placebo_pct（见下）、输入哈希、本文路径。

## 辅助（只描述，不参与判定）

1. 全币池（不加合格池限制）每日截面 Spearman RankIC：range_pos、mom6、mom12_1 分别对 fwd5 / fwd10 / fwd20；报告均值与 Newey-West t（滞后 h−1）。
2. 不加 SMA200 限制的前 5 对比（同样的 Δ 与 z）。
3. 随机前 5 安慰剂：每个日期从合格池随机抽 5 只，重复 200 次（固定种子 20261105），得到 200 条 mean(Δ_随机 − B)，报告变体 mean(Δ) 在其中的分位。
4. 换手：每组前 5 相邻日期成员变化比例的均值。
5. 各组前 5 的 fwd20 绝对均值，以及与币池等权均值之差。

## 不做的事

- 不调整窗口长度、N、合格条件、持有期或判定门槛后重跑。
- 不读取 2026-11-02 之后的数据，不使用基本面数据。
- 无论结果如何，不修改线上扫描排序；`screen_pass` / `strong` 只进入线上配对队列，由用户决定。
