# fq_crypto 分层独立运行与 50/50 双袖套拆分（S1 50/50 拆分 / S2 中小盘独立袖套）—— S1 预注册（2026-11-15）

本文件写于计算 `mid_small` 独立袖套与 `50/50` 双袖套回测收益之前。冻结参数一经运行不得修改。

## 背景与研究目标

- **前期已验证结果（`K=10, 11, 12`, `2026-11-15-fq-crypto-size-tier-s1-study.md`）：**
  - 现行 `fq_crypto` 7 因子 `h=3` 规则（`B`：`tier='all'`）在 `val` 净收益 `+26.51%`、在 `forward` `-6.79%`。
  - 仅在 `mega_large` 上重选（`M3`：`tier='mega_large'`）在 `forward` 显著优于 `B`（`+9.87%` vs `-6.79%`），但在 `val` 落后于 `B`（`+11.90%` vs `+26.51%`），因两端不同号而未能通过 S1。
- **未检验的分拆假设（直接对应用户“大盘/中小盘能否拆成两个独立交易员/袖套，相对现行系统有增益即可融合”）：**
  - 把 `fq_crypto` 拆成**大盘独立袖套 `M3`（`mega_large`, 50% 权益）**与**中小盘独立袖套 `S2`（`mid_small`, 50% 权益）**，形成 **50/50 等权双袖套组合 `S1_split`**，是否既保留 `mid_small` 在 `val` 期的横截面分散/空头优势，又保留 `mega_large` 在 `forward` 期的稳定性，从而在 `val` 与 `forward` 两段同时击败现行单池系统 `B`？
- **账本计数：** `fq_crypto` 当前累计 `K=12`，本次登记 2 个变体（`S1_split`, `S2_mid`），记 `K=13, 14`，强阈值按保守 `K=14` 计算：$z_K = \Phi^{-1}(1 - 0.05 / (2 \times 14)) \approx 2.9136$。

## 冻结输入与执行口径（100% 复用 `fq_crypto_size_tier_study.py`）

- 冻结面板快照：`data/factor_mining/fq_exit_funding/snapshot_20261108/crypto/`（SHA256 `02de1af2f12421e123739256732071a7632532b33e9a5985522c2965dc59c316`）。
- 冻结小时线：`data/volrix/klines/*_1h.json`（S20 盘中固定止损校验）。
- 冻结主因子组：7 因子 `h=3` 当前线上 `ensemble`（`PRIMARY_FACTORS`，与 `fq_crypto_size_tier_study.py` 完全一致）。
- 切分与成本：`val`（`2025-05-04..2026-04-30`）与 `forward`（`2026-05-04..2026-11-06`），3 日调仓，5 bp 手续费 + 3 bp 滑点，Backtrader + `CarryBroker`（3x daily funding）+ 小时线 S20。
- 各袖套独立资金运行后，双袖套组合 `S1_split` 的日权益按每 3 日调仓期 50/50 等权复利拼接（完全复用 `equalized_path` 逻辑：在每个调仓日将 `M3` 与 `S2_mid` 权益比例重置为 `0.50 / 0.50`，且验证两腿合计总名义敞口与 `B` 同为 `~1.0`）。

## 试验组与防错觉对照

1. **`B`（当前线上基线）：** `quant_targets.build_targets('crypto', ..., tier='all')`。
2. **`S2_mid`（`fq-crypto-mid-small-rebuild-v1`, `K=13`, Change Type `E`）：** `quant_targets.build_targets('crypto', ..., tier='mid_small')`（完全镜像 `M3`，仅将池限定在因果 `mid_small`；当期合格数 `< 15` 沿用上期）。
3. **`S1_split`（`fq-crypto-tier-dual-sleeve-50-50-v1`, `K=14`, Change Type `E`）：** 独立运行的 `50% M3 (mega_large)` + `50% S2_mid (mid_small)` 3 日调仓等权双袖套系统。
- **防错觉双对照：**
  1. 等暴露对照 `B_x(Arm)`（按日均总敞口比 $\rho$ 缩放 `B`）。
  2. 随机分层安慰剂（`placebo = 200`，种子 `20261116 + s`）：在每个调仓日 `t` 将 60 币 core mask 中可交易币按与真实 `|mega_large(t)| / |mid_small(t)|` 完全相同的数量无放回随机划分为两半，分别重建伪 `M3` 与伪 `S2_mid` 后 50/50 合成，运行完整 Backtrader + `CarryBroker` + 小时线 S20，检验 `placebo_pct >= 0.90`。

## 判定规则（`fq_crypto`, S1）

与 `fq_crypto_size_tier_study.py` 100% 一致：

- 日差序列 $\Delta_d = r_{\text{Arm}, d} - r_{B, d}$（`val` 与 `forward` 拼接），$z = \bar{\Delta} / \text{SE}_{\text{weekly}}$。
- 按顺序判定：`invalid` -> `fail`（`z < 1.0` 或任一段 `delta <= 0`）-> `exposure_only`（未击败 `B_x`）-> `fewer_trades`（`placebo_pct < 0.90`）-> `strong`（`z >= z_K(14) = 2.9136`）/ `screen_pass`（`1.0 <= z < 2.9136`）。
