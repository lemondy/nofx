# fq_us 波动率缩放在线配对：设计与预注册（2026-11-05）

依据：`docs/research/2026-11-05-fq-us-volscale-prereg.md`，S1 判 screen_pass（PR #86，fq_us K=4）。本文件提交于任何配对数据产生之前。下文的判定规则，在配对开始后不得修改。

## 目的

在真正尚未发生的行情里，检验 `mom_ls_invvol_volscale_v1` 相对现行 S2 规则 `mom_ls_invvol_v1`，能否同时做到两点：
- 风险调整后收益不变差；
- 回撤更小。

## 两组

| 组 | 交易员 | 规则 | 目标文件 |
|---|---|---|---|
| 对照 | 现行 “因子量化-美股（程序化模拟）” 1025e717 | `mom_ls_invvol_v1`，S2 已在运行，不做任何改动 | `us/quant_targets.json` |
| 候选 | 新建 “因子量化-美股-波动率缩放（模拟）” | `mom_ls_invvol_volscale_v1` | `us/quant_targets_volscale.json`，配置里用 `targets_path` 指向它 |

**候选与对照只差缩放系数 k：**
- 选股、剔除（`not_listed`、`no_perp`）、按 1/σ 加权、单只上限的规则完全相同；
- 候选的权重 = 对照当晚目标权重 × k_t；
- 其余配置也相同：`us_short_mode=paper`、`max_stale_days=4`、模拟资金 10000、费率和熔断用默认值。

**k_t 的计算：** 照搬 S1 预注册的定义。
- u_d：未缩放规则不计成本的持仓漂移日收益，从 bStock 日线的全部历史计算；
- σ̂：最近 126 个 u_d 的标准差；
- σ_ref：σ̂ 的扩展中位数，至少 252 个值；
- k = min(1, σ_ref / σ̂)，只在调仓时生效。

S1 数据中 2026-10-15 的 k 是 0.51，也就是说，候选目前大约只用对照一半的敞口。

**对齐调仓日：**
- Go 端按“两次目标 as_of 相差 ≥ 28 天”判断是否调仓。对照的首次调仓 as_of 是 2026-11-03，下一次在 as_of ≥ 2026-12-01 的那个晚上。
- 候选目标文件在 as_of < 2026-12-01 时写入 `status=not_started`。Go 遇到非 ok 状态会跳过调仓，而且候选没有持仓，所以不会触发其他动作。
- 从 as_of ≥ 2026-12-01 起，候选目标文件正常输出。两边读的是同一晚、同一个 as_of 的文件，因此首次调仓发生在同一天，之后每 28 天同步调仓。
- 这一步不需要修改 Go 代码。

## 判定（交易线 fq_us，stage S2，change_type C，idea_id `fq-us-volscale-pair-v1`）

**样本：**
- 公共窗口从候选首次调仓当天的收盘开始；
- 两组都按纽约证交所工作日，取每个 UTC 日的最后一个净值快照，计算日收益。口径与 `scripts/promotion/fq_us_s2_report.py` 相同。

**统计量（两组分别计算，都限定在公共窗口内）：**
- 年化夏普 = 日均收益 ÷ 标准差 × √252；
- 最大回撤；
- 是否触发 25% 回撤熔断。

**提前结束：**
- 对照触发 25% 熔断而候选没有 → `pass`；
- 候选触发熔断 → `fail`。

**第一阶段：** 达到 126 个交易日，且两组共同调仓 ≥ 6 次时判定。
- `pass`：Sharpe_候选 ≥ Sharpe_对照，并且 MaxDD_候选 < MaxDD_对照；
- `fail`：Sharpe_候选 < Sharpe_对照，并且 MaxDD_候选 ≥ MaxDD_对照；
- 其他情况为 `inconclusive`，延长一次。

**延长阶段：** 到 252 个交易日时，按 `pass` 的两个条件判定；两项不同时满足，即为 `fail`。

**检验力提醒：**
- 当前 k 约 0.5，候选回撤更小几乎是机械性的结果。真正有信息量的是夏普是否不降。
- 在 126 天、6 个周期里，夏普之差的检验力很低，`inconclusive` 是最可能的第一阶段结果。
- 本配对的判定与对照自身的 S2 判定（`fq-us-momentum-ls-v1`）互相独立。

## 实现（批准后另行分派）

1. **`scripts/factor_mining/quant_targets.py`**
   - 新增 us 规则变体 `mom_ls_invvol_volscale_v1`，由命令行参数 `--rule volscale` 写入 `us/quant_targets_volscale.json`。
   - 文件里记录 k、σ̂、σ_ref、as_of 和起始日期 `start_as_of=2026-12-01`。
   - k 的计算复用 `scripts/backtest/fq_us_volscale_study.py` 的 `unscaled_returns` 和 `volatility_signal`，数据换成线上日线。
   - 测试要求：在 S1 的 D2 快照上，各调仓日的 k 与 S1 报告一致。
2. **`run_nightly.sh`**：在生成 us 目标文件之后，再生成 volscale 文件。
3. **`scripts/promotion/fq_us_s2_report.py`**：增加配对模式（`--control`、`--candidate`），按本文规则输出判定，附测试。
4. **运维**（主 agent，需用户批准合并后执行）：
   - 备份数据库；
   - 新建候选的策略和交易员，配置复制对照，另设 `targets_path`；
   - 生成目标文件，确认状态为 `not_started`；
   - 重启后端；
   - 在 `data/promotion/pairs.json` 登记配对，在主账本记一行 S2 开始，状态 running。
