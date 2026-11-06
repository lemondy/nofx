# 暂停美股因子挖掘（2026-11-06，用户决定）

## 决定

结束美股因子挖掘。美股量化维持“动量 + 风控”，保持现有两条在线线路：

- S2：`fq-us-momentum-ls-v1`；
- 波动率缩放配对：`fq-us-volscale-pair-v1`，从 as_of 2026-12-01 开始。

## 依据

美股的量价因子和基本面因子都已检验过（fq_us 账本 K = 5），在 2012–2026 年的美股大盘股里，没有任何单因子能同时通过训练、验证、前向三段：

| 检验 | 结果 | PR |
|---|---|---|
| 宽池经典量价因子 | 0/30 | #80 |
| 挖掘器多样化父代 | 无增量 | #81 |
| 风格中性化 | fail | #87 |
| 基本面因子（SEC 按时点数据） | 0/18 | #91 |

近期为正的只有 12-1 动量和 SUE，两者都只在 2022 年以后有效。SUE 暂不预注册。

## 夜间任务的变化

`scripts/factor_mining/run_nightly.sh`：

- **美股挖掘默认关闭**（`FM_US_MINING=0`）。设 `FM_US_MINING=1` 可恢复。
- **美股日线照常拉取**：只做 fetch-only，重试次数至少为 1，因为动量和 volscale 目标步骤要读这些日线。
- **加密货币挖掘不受影响。**
- **美股目标步骤显式传入 `--perp-map`**，默认 `$FM_ROOT/../cache/usstock_symbols.json`。原因：launchd 运行的是 `nofx-autoresearch` 里的副本，`quant_targets` 的默认路径 `REPO_ROOT/data/cache` 在那里不存在。

## 同时发现的部署问题

launchd 执行的是 `/Users/zhangyun/workspace/nofx-autoresearch` 里由 `deploy.sh` 同步的代码副本。上次同步在 11-01，所以 PR #81–#89 都没有部署到夜间任务。具体后果：

- **S2 目标文件被覆盖。** 11-06 凌晨，旧版挖掘器用旧规则 `long_invvol_v1` 覆盖了 `us/quant_targets.json`，状态写成 `no_validated_factors`。交易员遇到非 ok 状态时保持现有持仓、不调仓，所以持仓没有受影响，但 S2 下次调仓依赖正确的目标文件。
- **两个目标步骤和配对报告都不在旧副本里。** 夜间不会生成 volscale 目标文件，配对到期也不会启动。

修复：

- `deploy.sh` 改为同步 `scripts/backtest` 下的 Python/JSON 模块，因为目标步骤要导入 `fq_us_momentum_study` 和 `fq_us_volscale_study`。同步时不加 `--delete`。
- 合并后运行 `scripts/factor_mining/deploy.sh`，并重新生成美股两个目标文件。
