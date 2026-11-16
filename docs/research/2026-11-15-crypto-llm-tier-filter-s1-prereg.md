# crypto_llm 候选币池与市值/流动性分层过滤（C1 / C2 / C3）—— S1 预注册（2026-11-15）

本文件写于计算任何 `r` / `pnl` / 胜率之前。已冻结的过滤条件、随机安慰剂次数与判定规则一经运行不得修改。

## 背景与研究目标

- **对应现行线上系统：** 主实盘 `mac-nofx`（`d7ff3764_..._deepseek_1787982996`）以及同币池配置的活跃加密 LLM 模拟盘（`a8eb1bdf`, `a2dc5705`, `ec830bb2`, `895dc8b3`）均采用统一混合币池 `coin_source = {ai500 + piggy_dash + short_scan, min_oi=5}`。
- **前期只查覆盖率（未读任何 PnL/R）发现的事实：**
  - `mac-nofx` 的 `445` 笔已平仓中有 `226` 笔具备完整 `initial_stop_loss` 以计算合法 R（`read_closed_trades`，`2026-09-17..2026-11-14`）；汇总 5 个同配置加密 LLM 交易员共 `575` 笔合法 R 交易（`2026-09-17..2026-11-15`）。
  - 在 `mac-nofx` 的 `226` 笔交易中，仅有 `78` 笔（`34.5%`）属于加密大盘（`ADV20 >= $100M`），`95` 笔（`42.0%`）属于可观测中小盘加密币，`24` 笔（`10.6%`）实际上是代币化美股/ETF/大宗商品/Pre-IPO（如 `AAPLBUSDT`, `SOXLUSDT`, `XAUUSDT`, `ANTHROPICUSDT`），还有 `29` 笔（`12.8%`）是缺乏 20 日日线历史的新币/微盘（如 `牛来USDT`, `USELESSUSDT`）。
- **核心问题（按 `INCREMENTAL_EVAL_DESIGN_2026-11-03.md` B 类）：** 不要求单独做正收益，直接检验**相对现行混合池基线**：
  1. 若过滤掉非加密资产与无 20 日历史微盘（`C1`），是否优于现行全池？
  2. 若仅保留加密主流 100 币面板（`C2`）或仅保留加密 `mega_large`（`C3`），每笔 R 是否显著提高并击败同比例随机剔除安慰剂？
- **账本计数：** `crypto_llm` 当前 `S1` 累计 `K=6`（上一次为 `limit-band-entry-c1-v1` `strong`）。本次 3 组变体记为 `K=7, 8, 9`，强阈值按保守 `K=9` 计算：$z_K = \Phi^{-1}(1 - 0.05 / (2 \times 9)) \approx 2.7729$。

## 冻结输入与无前视分层口径

- **主样本（实盘单账户）：** `mac-nofx`（`trader_id = 'd7ff3764_f6b845ba-f7b2-4b27-8501-fcf6af94f276_deepseek_1787982996'`），由 `scripts/backtest/incremental_filter.read_closed_trades` 读取 `status='CLOSED'` 且有合法 R 的 `226` 笔交易（`2026-09-17..2026-11-14`）。
- **次级交叉样本（仅作参考验证）：** 汇总 5 个同币池配置加密 LLM 交易员（新增 `a8eb1bdf...`, `a2dc5705...`, `ec830bb2...`, `895dc8b3...`），共 `575` 笔合法 R 交易（`2026-09-17..2026-11-15`）。
- **严格因果特征时点：** 对每笔开仓时间为 `entry_time`（UTC）的交易，只读取截止前一完整 UTC 日线收盘 `sig_day = entry_date_utc - 1 day` 的日线数据，绝不使用开仓当日或之后的价格与成交量。
- **数据源：**
  - `crypto/daily`（100 个主流加密合约日线面板，截止 `2026-11-13`）
  - `crypto/ext/daily`（155 个扩展加密合约日线面板，截止 `2026-11-13`）
- **确定性分类（覆盖率 100%，`mask_coverage_error = 0% <= 5%`）：**
  - `in_cd`: `symbol` 存在于 `crypto/daily`。
  - `has_adv20`: `symbol` 存在于 `crypto/daily` 或 `crypto/ext/daily`，且 `<= sig_day` 至少有 20 根有效日线，计算最近 20 根 `close * volume` 均值 `adv20`。
  - `tier`:
    - `mega_large`: `has_adv20` 且 `adv20 >= $100,000,000`；
    - `mid_small`: `has_adv20` 且 `adv20 < $100,000,000`；
    - `non_crypto_or_unmapped`: 不满足 `has_adv20`（代币化美股/ETF/大宗商品 + 缺历史新币/微盘）。

## 试验变体与控制台规则（严格调用 `incremental_filter`）

固定参数：`seed = 20261103`，`placebo_n = 2000`，主指标 `delta = kept_avg_r - all_avg_r`，周聚类与前后两半时间切分完全由 `incremental_filter.filter_statistics` 与 `decide_verdict` 计算：

1. **`C1`（`crypto-llm-verified-crypto-pool-v1`, Change Type `B`, `K=7`）：**
   - 保留 `tier in {'mega_large', 'mid_small'}`（剔除代币化美股/商品/无 20 日历史微盘）。
   - 预检剔除率：`mac-nofx` 上 `53/226 = 23.5% <= 50%`；`pooled-5` 上 `97/575 = 16.9% <= 50%`。
2. **`C2`（`crypto-llm-core100-pool-v1`, Change Type `B`, `K=8`）：**
   - 保留 `in_cd`（`crypto/daily` 100 个成熟核心合约）。
   - 预检剔除率：`mac-nofx` 上 `120/226 = 53.1%`；`pooled-5` 上 `249/575 = 43.3%`。
3. **`C3`（`crypto-llm-mega-large-tier-v1`, Change Type `E/B`, `K=9`）：**
   - 保留 `tier == 'mega_large'`（剔除中小盘、微盘与代币化非加密标的）。
   - 预检剔除率：`mac-nofx` 上 `148/226 = 65.5% > 50%`（按 `INCREMENTAL_EVAL_DESIGN` B 类严格阈值 `p > 0.5` 会触发 `fewer_trades`）；因此我们除严格记录 `decide_verdict` 结论外，同时报告 `z`、`dev_delta`、`holdout_delta`、`placebo_pct` 及独立各层（`mega_large` vs `mid_small` vs `non_crypto_or_unmapped`）的 `avg_r` 与周聚类 95% CI，用于评估是否拆分成独立大盘交易员。

## 判定规则（`crypto_llm`, S1）

完全由 `incremental_filter.decide_verdict(stats, k=9)` 在主样本 `mac-nofx`（`N=226`）上自动判定（次级 `pooled-5` 仅作跨交易员稳健性描述）：

- `invalid`: `n < 100` 或 `mask_coverage_error > 0.05` 或 `p in (0, 1)`
- `fewer_trades`: `p > 0.5`；或 `placebo_pct < 0.90` 且 `total_r_change > 0`
- `strong`: `p <= 0.5`, `dev_delta > 0`, `holdout_delta > 0`, `placebo_pct >= 0.90`, `z >= z_K(9) ≈ 2.7729`
- `screen_pass`: `p <= 0.5`, `dev_delta > 0`, `holdout_delta > 0`, `placebo_pct >= 0.90`, `1.0 <= z < z_K(9)`
- `fail`: 其余情况

## 只做描述、不参与判定

1. `pooled-5`（`N=575`）上的同一套 `C1, C2, C3` 统计量与 `decide_verdict`。
2. `tier` 三分法（`mega_large` / `mid_small` / `non_crypto_or_unmapped`）各自的样本数、累计 R、笔均 R、`weekly_statistics` 周聚类 SE 与 95% CI、净盈亏 USDT，以及 `mega_large - mid_small` 差值描述。
3. 剔除掉代币化美股（`non_crypto_bstock_comm`）与未映射加密微盘（`unmapped_crypto`）的分项表现。
