# 加密宽因子池组合：预声明（2026-11-03）

这是增量评估框架（`docs/architecture/INCREMENTAL_EVAL_DESIGN_2026-11-03.md`）的第一个用例。

- 交易线：`fq_crypto`。
- 改动类型：E，即替换打分/排序；持仓规则不变。
- 本文在运行任何结果之前提交，看到结果后不修改。

## 动机

当前因子量化-加密（基线）的做法：

- 用 `crypto/ensemble.json` h=3 的 14 个 accepted 因子；
- 各因子做横截面 z 分数后等权平均；
- 按 `ls_invvol_protected_v1` 规则每 3 天调仓。

成熟多因子做法用几十个相关性低的因子一起打分。本地 zoo 里 h=3 的因子情况：

| 状态 | 数量 | 说明 |
|---|---:|---|
| accepted | 14 | 训练段 t ≥ 4.6、前后两半同号，验证段也通过 |
| candidate | 3307 | 训练段同样通过，验证段未通过（多数验证 t 在 3–5 之间） |
| redundant | 329 | 训练段同样通过，与已选因子相关性过高 |

注意：训练段 t < 4.6 的弱因子在挖掘时没有保存，所以本研究的池子是"训练段强、验证段未必过"的因子，不是弱因子池。

已知的数据污染：

- 前向区间（2026-05-04 起）已被 `2026-11-01-ensemble-forward-study` 用于基线 ensemble 的信号检验。
- 本研究的动机部分来自那次结果。
- 本研究只检验"新打分 − 基线打分"的成对差；两边用同一前向区间、同一规则，污染对两边相同，但结论仍需打折。

## 输入（冻结）

- `data/factor_mining/crypto/zoo.json`：sha256 `fffb42c16ddb013829371f9998f282b741752435b0d95f046ba667071667011a`
- `data/factor_mining/crypto/ensemble.json`：sha256 `b1b899851724f7c09ee44b055b01fa0256659692ae08ae6f57f364353001f4e1`
- 面板：`panel.load_panels('crypto')`，使用核心流动性池掩码，与 `quant_targets` 一致。

## 方法（只用训练段信息）

1. **池子**：zoo 中 h=3 且 status ∈ {accepted, candidate, redundant} 的全部 3650 个因子。
   - 这三类的区分用到了验证段，但取并集后与验证段无关。
   - rejected 的不入池。
2. **排序**：按 `train.icir` 从高到低；同分时按 id。
3. **去相关贪心**：
   - 依次考察，如果它与已选每个因子在训练段的横截面秩相关绝对值都 < 0.5，就入选；
   - 相关性的日期取训练段日期的固定子样本，复用 `evaluate.cross_corr` 的口径，种子 20261103；
   - 选满 N = 40 个即停。
   - 入选因子的表达式和方向，取 zoo 里记录的 `expr` 与 `direction`。
4. **主权重（唯一计入账本的变体）**：
   - w_i = 0.5 × (1/N) + 0.5 × (train.ic_i / Σ train.ic)，即按训练段 IC 加权，再向等权收缩一半。
   - 打分 = Σ w_i · z_i / Σ_{有值} w_i。z_i 是与 `ensemble.ensemble_scores` 相同的横截面标准化；某因子当日缺值时不计入分母。
5. **持仓规则与成交**：完全沿用 `quant_targets`。
   - `_historical_targets` 每 3 天生成 `ls_invvol_protected_v1` 目标；
   - `scripts.backtest.quantjourney.run_weights` 用 Backtrader 撮合；
   - 手续费 0.0005，滑点 0.0003，计入真实资金费。
   - 基线组走同一路径，只是打分换成当前冻结的 14 因子等权 ensemble。

## 主检验

- **区间**：前向 2026-05-04 至面板最后一天。
- **主统计量**：每日组合收益的配对差 d_t = r_new,t − r_base,t。
  - 均值 Δ；
  - 按 UTC 周一所在周聚类求 SE（G/(G−1) 修正）；
  - z = Δ / SE。
- **S1 判定**（K 为账本里 `fq_crypto` 的累计尝试数，含本次；z_K = Φ⁻¹(1 − 0.05/(2K))）：

| 判定 | 条件 |
|---|---|
| `invalid` | 配对日 < 100，或任一组回测失败，或入选因子 < 20 |
| `strong` | z ≥ z_K，且前向区间前/后两半 Δ 都 > 0 |
| `screen_pass` | z ≥ 1.0，且两半 Δ 都 > 0 |
| `fail` | 其余 |

  组合类改动没有"少交易"问题，不设 `fewer_trades`，但会报告两组的换手率比。

- 结果追加到 `data/evolution/ledger.jsonl`：
  - track = `fq_crypto`，change_type = `E`，stage = `S1`；
  - baseline_version = 基线 ensemble.json 的 sha256 前 16 位 + 规则名。

## 辅助（只描述，不改判定）

- 验证段（2025-05-04 至 2026-04-30）同样的配对差。两边因子的选择都没用到验证段的标签，但基线成分是用验证段挑出来的，所以这里对基线有利。
- 前向区间两组的每日 RankIC 及其配对差，NW t（滞后 3）。
- 两组的年化收益、Sharpe、最大回撤、换手、多头腿与空头腿分别的贡献。
- **对照变体**：入选的 40 个等权，不计入账本。
- **安慰剂**：从池子里随机抽 40 个（不去相关）等权组合，重复 200 次，种子 20261103，只算前向 RankIC 均值，报告主变体的分位。
- 入选因子清单：id、表达式、族、训练段 IC/ICIR、权重，以及族分布。

## 后续

- `strong` / `screen_pass`：进入线上配对。
  - 用 `create_pair --mode incremental` 预登记，候选交易员使用新打分。
  - 是否另做 LLM 参考字段（A 类，离线重放）另行决定。
- `fail`：记账并结束，不调整 N、相关阈值或权重后重试。如需重试，必须是新的预声明，K 加 1。
