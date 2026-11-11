# Kronos 开仓否决 S1 结果（H1，crypto_llm）

- 日期：2026-11-11
- 阶段：S1 离线筛选，B 类（过滤器），track `crypto_llm`，预注册 K=5（z_K = 2.576）
- 预注册：`docs/research/2026-11-11-kronos-entry-veto-s1-prereg.md`（sha256 `e53d8cd3…0572530031`，提交于 cbd41b5f，运行前未改动）
- 研究脚本：`scripts/backtest/kronos_entry_veto_study.py`，测试 `scripts/backtest/tests/test_kronos_entry_veto_study.py`（提交 90f752b0；运行时 HEAD 记录在 `data/kronos_veto/run_commit.txt`）
- 上游评估：`docs/research/2026-11-10-kronos-kit-assessment.md`
- 用户批准：“跑 H1”（经父会话转达）

## 1. 结论

**判定 `fewer_trades`，不进入 S2，Kronos 不接入。**

- 规则剔除了 292 / 439 笔（66.5%），超过框架 0.5 的上限，按框架顺序直接判为 fewer_trades。
- 即使不看剔除率，方向也是反的：剔除之后保留交易的平均 R 从 −0.036 降到 −0.158，ΔavgR = **−0.122**，z = **−2.66**，安慰剂分位 0.003（比 99.7% 的同比例随机剔除都差）。前半、后半两段都为负（−0.062、−0.187）。
- 同笔数的零成本代理（窗口 z 分数）ΔavgR = **−0.205**，也是负的。Kronos 比代理少亏一些，但两者都在恶化结果，`s2_eligible = false`。
- 无预测笔数 **0**（G2 占比 0%）。
- 只运行一次，CPU，未调参、未换模型/阈值/周期重跑。

档位：**实现正确**（G1–G4 全过）；增量筛选**未通过**；结论为“没有值得接入的内容”。

## 2. 输入与冻结

| 项 | 值 |
|---|---|
| 交易 | `data/llm_trim_offline/export_v1/trades.jsonl`，sha256 `8abfa110…57382121`，439 笔（做空 309 / 做多 130），2026-09-29 至 11-08 |
| R | `data/llm_trim_offline/s1_v1/results.json` 的 `no_trim.r`（sha256 `ddce670b…5d0b5`），敏感性用 `baseline.r` |
| K 线 | Binance USDT-M 5m，入场前 512 根已收盘 K 线；`data/kronos_veto/kl5m_pre/`（439 个文件，manifest sha256 `7cd2907d…5651d1304`），另有 `kl5m_pre/post/` 仅作描述 |
| 模型 | Kronos-small（权重 sha256 `b082dfcb…3e020`），tokenizer（`59d85f6a…6bee`），代码 commit `67b630e` |
| 采样 | T=1，top_p 0.9，16 条路径，pred_len 60（5 小时），seed 20261111+trade_id，CPU |
| 规则 | p_adverse ≥ 0.75（16 条中 ≥ 12 条不利）则否决 |
| 代理 | 按 side·z_last 从大到小剔除同样笔数（292） |

数据保留在 `data/kronos_veto/`，不进 git。

## 3. S0 闸门

| 闸门 | 结果 |
|---|---|
| G1 复现性（10 笔，种子 7） | 通过，0 处不一致 |
| G2 无预测占比 ≤ 5% | 通过，0 / 439 |
| G3 50 条无漂移 GBM，平均 p_up ∈ [0.35, 0.65] | 通过，0.509（单条 p_up 大多是 0 或 1，见第 5 节） |
| G4 剔除笔数 ≥ 20 | 通过，292 |

## 4. 主结果（框架 `filter_statistics` / `decide_verdict`）

| 指标 | 值 |
|---|---|
| 全部平均 R | −0.0364 |
| 保留平均 R | −0.1585 |
| 被剔除平均 R | +0.0250（95% CI −0.063 ~ +0.113，6 周聚类 SE 0.045） |
| ΔavgR | −0.1220（SE 0.0458） |
| z | −2.662 |
| 安慰剂分位 | 0.003 |
| 前半 / 后半 Δ | −0.062 / −0.187 |
| 总 R 变化 | −7.31 R |
| 剔除率 | 0.665 → `fewer_trades` |
| 代理 ΔavgR | −0.205（与 Kronos 剔除重合 266 / 292 笔） |

分方向：

| 方向 | 笔数 | 剔除 | 被剔除平均 R | 保留平均 R | Δ |
|---|---|---|---|---|---|
| 做多 | 130 | 92 | −0.108 | −0.251 | −0.101 |
| 做空 | 309 | 200 | +0.086 | −0.126 | −0.138 |

## 5. 只作描述（不影响判定，不计 K）

- 阈值敏感性：11/16 时 Δ = −0.110，13/16 时 Δ = −0.128，与 12/16 同向。
- 用 `baseline.r`（含现行提前减仓）时 Δ = −0.118，结论不变。
- p_adverse 分布极端：16/16 有 220 笔，0/16 有 55 笔，两端合计 63%。16 条路径基本同向，和合成数据上看到的过度自信一致。
- 方向命中率（16 条路径平均终点价相对入场价的方向，对比 5 小时后的实际方向；与交易方向无关）0.476，低于抛硬币。
- side·z_last 与 p_adverse 的 Spearman ρ = **0.746**；side·过去 5 小时收益与 p_adverse 的 ρ = −0.017；side·预测均值与 R 的 ρ = −0.080。这说明在真实交易上，Kronos 的读数主要也是“窗口内区间位置 → 反转”，和合成诊断（相关 −0.78）一致。
- 平均每笔 28.3 秒，预测部分合计约 3.5 小时；账本 `recorded_at` 2026-11-11T05:47:32Z。

## 6. 解读

- 机制：做多时价格已处在 512 根窗口的高位（做空时在低位），Kronos 就预测回落，从而否决。可这些“已经走出来”的交易事后反而更好：被剔除的做空平均 +0.086R，保留的只有 −0.126R。这与此前的结论一致：美股反转排序 S1 失败、动量延续（`2026-11-05` 研究）；factor 否决作为 LLM 过滤器也失败（`crypto-universe-expansion`）。
- Kronos 比代理少亏 0.083R，可能是它在区间位置之外多带了一点信息，也可能只是两组剔除有 26 笔不同带来的噪声。按预注册，第 2 条（必须胜过代理）只在 verdict 通过时才有意义，这里不再展开。
- z 显著为负，**不能**反过来当作“只做 Kronos 认为不利的交易”的证据：这条反向规则没有预注册；剔除率又在 2/3，它其实是另一种规则（顺延伸方向开仓）；样本已被提前减仓、DVOL、相关性状态研究反复使用过。若想检验，必须另写预注册，作为新的 K 计入。

## 7. 局限

- 只有 439 笔、6 周、单一 LLM 交易员版本，并且做空占 70%。
- 零样本 Kronos-small，没有微调。Kronos-base 或微调版本没有测，评估文档里已说明不建议为此投入。
- R 复用冻结的 Backtrader no_trim 结果，不重新模拟成交。
- 样本与此前几项研究重叠（预注册中已披露）。

## 8. 账本

`data/evolution/ledger.jsonl` 新增 1 行（第 26 行，该 idea 只有这一行）：

```
track=crypto_llm stage=S1 change_type=B idea_id=kronos-entry-veto-v1 baseline_version=no-entry-veto
verdict=fewer_trades K=5 preregistered_K=5 z=-2.662 delta=-0.1220 placebo_pct=0.003
dropped_n=292/439 proxy_delta=-0.2050 s2_eligible=false trials=1 counts_toward_k=true
recorded_at=2026-11-11T05:47:32Z
```

## 9. 下一步

- 不接入 Kronos，H1 关闭。H2、H3（评估文档中的备选）没有新证据支持，不建议启动。
- 未改任何交易配置，未下单，数据库只读。

## 10. 复现命令

```bash
env -u PYTHONPATH PYTHONDONTWRITEBYTECODE=1 HF_HUB_OFFLINE=1 <venv-kronos>/bin/python -m scripts.backtest.kronos_entry_veto_study run \
  --trades data/llm_trim_offline/export_v1/trades.jsonl \
  --results data/llm_trim_offline/s1_v1/results.json \
  --klines-dir data/kronos_veto/kl5m_pre --kronos-repo <Kronos@67b630e> --weights <weights dir> \
  --out data/kronos_veto/s1_v1 --write-ledger --device cpu
```

输出：`data/kronos_veto/s1_v1/{forecasts.jsonl,results.json,trades.jsonl,mask.jsonl,report.md}`，日志 `data/kronos_veto/run.log`。
