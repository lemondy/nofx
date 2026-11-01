# 因子量化交易员（程序化、不经过大模型）设计（2026-11-01）

## 目标

因子挖掘的输出分成两路：

1. **大模型参考（已有）**：`data/factor_mining/{market}/live_scores.json` → `kernel/factor_ensemble.go` → 写进提示词的 `factor_ensemble` 字段。这一路不改。
2. **程序化量化交易（新增）**：每晚挖掘结束后，由 Python 生成 `data/factor_mining/{market}/quant_targets.json`，即目标权重。新策略类型 `factor_quant` 的交易员按调仓周期读取它，在**模拟账户**上按目标权重调仓。全程不调用大模型。

为什么用“Python 产出权重、Go 只负责执行”的分工：
- 组合构建所需的数据（波动率、近 7 日收益、上市天数、流动性池）都在 Python 面板里；
- 研究回测和实盘目标由同一份代码产生，口径一致；
- Go 侧只是一个薄执行器，便于审查和测试。

## 用户确认的决策（2026-11-01）

- **美股**：交易员先建好。只要 `quant_targets.json` 的 status 不是 `ok`（目前美股没有通过验证的因子），就保持空仓，等因子出现后自动开始交易。
- **加密**：多空中性加双向止损保护。做多因子排名前 20%、做空后 20%，按波动率倒数加权；空头排除近 7 日暴涨和上市不足 90 天的币；设单币上限；多头和空头均加止损，默认偏离持仓均价 20%。
- **调仓周期**：加密用 3 天，对应 h=3 的 ensemble。
- **账户**：只允许模拟账户，加密用 paper 合约账户，美股用 bStock 模拟账本。代码层强制，不能接实盘。
- 代码审查通过后重启后端，再创建两个交易员。

## quant_targets.json（v1）

```json
{
  "version": 1,
  "market": "crypto",
  "status": "ok",
  "reason": null,
  "generated_at": "2026-11-01T19:00:00+00:00",
  "as_of": "2026-10-31",
  "run_id": "crypto-…",
  "horizon": 3,
  "rule": {
    "name": "ls_invvol_protected_v1",
    "quantile": 0.2,
    "long_gross": 0.5,
    "short_gross": 0.5,
    "max_name_weight": 0.1,
    "vol_window": 30,
    "short_exclude_ret7_quantile": 0.9,
    "min_listing_days": 90,
    "long_only": false
  },
  "factor_ids": ["f_…"],
  "universe_size": 60,
  "weights": {"AAVEUSDT": 0.0412, "AGTUSDT": -0.0388},
  "excluded": {"AKEUSDT": "short_ret7_top_decile"},
  "pct": {"AAVEUSDT": 0.80}
}
```

- **status**：
  - `ok`：可以交易；
  - `no_validated_factors`：该 market/horizon 没有可用的 ensemble 分数，美股目前是这种情况；
  - `insufficient_universe`：合格标的太少；
  - `stale_scores`：分数日期落后。

  只要不是 `ok`，`weights` 就必须为 `{}`，交易员不开新仓；已有自有持仓超过自动清仓阈值后清仓。
- **weights**：账户权益的名义比例。加密时正为多、负为空，多头合计 ≈ +0.5，空头合计 ≈ −0.5，毛敞口 1。美股只做多，合计 ≈ 1.0。key 的写法：
  - 加密用永续合约 symbol，例如 `AAVEUSDT`；
  - 美股用 bStock 现货对，即标的代码加 `BUSDT`，例如 `AAPLBUSDT`。
- 文件写入必须原子（同目录临时文件 + rename）；Go 侧读到损坏或不完整的 JSON 时不按文件调仓，仍检查自有持仓是否需要过期清仓。

### 加密组合规则 `ls_invvol_protected_v1`（预声明，模拟盘就是它的前向检验）

信号日 t = `as_of`，分数来自 `live_scores` 的 h=3 ensemble。合格标的需同时满足：在流动性池（panel mask）内、分数有限、`vol_window` 日波动率有限且 >0。

1. 按 ensemble 分数排序：前 `floor(n*0.2)` 个进多头候选，后 `floor(n*0.2)` 个进空头候选（至少各 1 个，两边不得重叠）。
2. **空头排除**（从空头候选中剔除，不递补）：
   - 近 7 日收益（close[t]/close[t−7]−1）位于全体合格标的的前 10%（≥ 0.9 分位）；
   - 有效历史不足 `min_listing_days`=90 天。

   被剔除的币写进 `excluded`，注明原因。
3. **权重**：每条腿内按 1/σ（30 日日收益标准差）加权，归一化到该腿毛敞口 0.5。单币上限 0.1，超出部分按比例分给同腿其他币，迭代到收敛；若一条腿全部顶到上限，则接受该腿毛敞口 < 0.5。
4. 空头腿为空时，多头腿也不交易，状态为 `insufficient_universe`，以保持市场中性。

### 美股规则 `long_invvol_v1`

只做多前 20%，1/σ 加权，合计 1.0，单只上限 0.1，没有空头。目前没有通过验证的因子，所以输出 `no_validated_factors`。

## 策略类型 `factor_quant`（Go）

`store.StrategyConfig.StrategyType = "factor_quant"`，配置写在 `FactorQuantConfig`（json `factor_quant_config`）：

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `market` | string | 必填 | `crypto` 或 `us` |
| `horizon` | int | crypto 3；us 0 | 必须等于文件的 `horizon`；美股填 0 表示接受文件给出的值 |
| `capital_fraction` | float | 1.0 | 用多大比例的权益执行权重（0, 1] |
| `leverage` | int | 3 | 仅加密：模拟合约杠杆，用来保证保证金充足 |
| `rebalance_band_pct` | float | 0.10 | 某个币的目标名义与当前持仓之差小于目标的 10% 时不交易（减少手续费） |
| `min_order_usdt` | float | 10 | 单笔最小名义 |
| `long_stop_pct` | float | 0.2 | 仅加密：止损（多），触发价 = 持仓均价 ×(1−N)；省略/null 用默认 20%，显式 0 关闭；有限值，范围 [0,1) |
| `short_stop_pct` | float | 0.2 | 仅加密：止损（空），触发价 = 持仓均价 ×(1+N)；省略/null 用默认 20%，显式 0 关闭；有限值，范围 [0,10] |
| `us_fee_bps` | float | 10 | 仅美股：每笔成交手续费（基点，10 = Binance 现货 0.1%），从模拟现金扣除并记在持仓行 `fee`；填 0 关闭；范围 0–100 |
| `us_slippage_bps` | float | 3 | 仅美股：每笔成交滑点（基点）。买入价 = 报价×(1+bps)，卖出价 = 报价×(1−bps)；填 0 关闭；范围 0–100 |
| `max_stale_days` | int | horizon+2 | `as_of` 落后今天（UTC）超过这么多天，就不按它调仓，并告警 |
| `flatten_after_days` | int | horizon+max_stale_days（加密默认 8 天） | 上次调仓的 `as_of` 距今超过阈值且本轮无法调仓时，自动清仓全部自有持仓；0 用默认，-1 关闭，否则 1–60 |
| `targets_path` | string | `$FACTOR_MINING_DIR/{market}/quant_targets.json`，未设置时为 `<repo>/data/factor_mining/...` | 覆盖路径，测试用 |

**交易成本（2026-11-01 补充）**

- 加密：模拟合约撮合已计入 taker 手续费与滑点（见 paper 账户配置），并且从 2026-11-01 起结算永续**资金费率**：持仓每跨过一次币安资金费结算（00/08/16 UTC，个别币种 4h/1h），按「数量 × 当次标记价 × 费率」多头付、空头收（费率为负则相反），写入钱包与 `account_income`（FUNDING_FEE，账户类型 paper），平仓后体现在持仓的 `funding_fee` / `net_pnl`。
- 美股：成交按上表两个字段计滑点与手续费。买入的现金检查包含手续费；规划器按 `权益 ÷ (1 + (费率+滑点))` 定仓位规模，保证满仓目标在扣完成本后仍买得起。

### 运行循环

- 自有循环，每 5 分钟检查一次，不走大模型和 AI 决策周期。
- 读取 targets 文件，满足以下全部条件才调仓：
  - status = `ok`；
  - `as_of` 未过期；
  - `horizon` 匹配；
  - 距上次调仓的 `as_of` 已过 ≥ horizon 天，或者账户目前没有本策略持仓且从未调仓。

  上次调仓的 as_of 要持久化，重启后不能重复调仓。
- 调仓步骤：
  1. 算出目标名义 = 权重 × 权益 × capital_fraction；
  2. 先平掉不在目标中的持仓，或方向相反的持仓；
  3. 再减仓，最后加仓。减仓在前，是为了释放保证金；
  4. 全部用市价单；
  5. 加密两侧每次开仓、加仓或减仓成功后，按该侧当前完整数量和平均入场价重设止损；整轮调仓后再检查所有持仓，包括规划器留在调仓带内的持仓。检查结果追加到 `rep.Stops`，失败计入调仓失败数并触发既有重试。
- 失败处理：单笔失败记录下来并继续执行其余订单；整轮结果写入一条决策记录，前端可见，内容包括 as_of、规则名、每笔订单、跳过原因和失败原因。
- 权益快照沿用现有机制定期写入，供晋升报告和前端曲线使用。
- **只允许模拟**：
  - 加密：交易员的 exchange 必须是 `paper`；
  - 美股：必须是模拟账本模式，且绝不调用真实下单接口。

  这两条在构造交易员时就校验，不满足则拒绝启动，并且有测试覆盖。

#### 加密双向止损与存量迁移

`LongStopPct`、`ShortStopPct` 均为指针配置：nil（JSON 省略/null）默认 0.2，显式 0 关闭对应方向。多头止损价为平均入场价 ×0.8，空头为平均入场价 ×1.2；自定义比例时用相应公式。编辑器展示默认 20%，输入值为比例 0.2；清空输入恢复显式默认值，0 保留并显示为关闭。

加密循环启动时，在执行互斥锁内、订单同步和首次 tick 的调仓检查之前调用一次 `ensureStops`，无需等待下一次 3 天调仓或有效目标文件。已有空头旧的 1.5 倍止损会按新配置替换为 1.2 倍；已有多头补放 0.8 倍止损。日志汇总已放置、已止损平仓、失败数量；失败只告警，不阻止循环。重启/配置重载应用当前配置；显式配置的旧 0.5 仍是自定义值，需改为 0.2 才按 20% 迁移。配置为 0 时取消该方向已有 STOP_MARKET，不影响另一方向。

若当前标记价已经达到或越过新止损价（多：mark ≤ stop；空：mark ≥ stop），不放立即触发的挂单，而是用 `CloseLong/CloseShort(symbol, 0)` 市价全平。结果记 `Exited: true` 和“止损价已越过，已市价平仓”，仅平仓失败视为错误；决策记录和启动日志区分补放与止损退出。

Paper 模拟器既有语义：`SetStopLoss` 按 symbol、position side 替换同类 STOP_MARKET；LONG 发 SELL，SHORT 发 BUY，均为 `ClosePosition=true`、数量 0 的全仓保护单。开仓/加仓取消同侧保护单；减仓保留原保护单，但它触发时关闭当前剩余全仓，执行器仍重设以刷新均价和数量；完全平仓时 `paper.execute` 自动取消同侧保护单，不留旧止损。无需修改 paper 语义。

**已知缺口：美股 bStock 模拟账本仍没有止损。** 本次双向止损、迁移和补放仅适用于加密 paper futures；美股路径、规划器、目标权重、调仓门禁不变。

#### 失败重试

调仓执行后仍持久化 `last_as_of`，有失败订单（包括止损（多/空）放置失败）时另设 `retry_as_of`、`retry_attempts=0`；全部成功则清除重试状态。后续每 5 分钟检查一次：仅当门禁为 `too_soon`、重试与上次调仓及当前文件的 `as_of` 一致、文件状态为 `ok` 且未过期时，每 tick 重试一轮，最多 6 次（约 30 分钟）。每轮从当前持仓重新规划，已成交且在调仓带内的订单不重复发送；方向切换时若旧方向平仓失败而新方向已成交，重试只平旧方向并按新方向当前持仓计算差额；加密每轮重试后仍调用 `ensureStops` 检查双向止损（即使调仓前置检查失败），模拟器替换同侧旧止损，操作幂等。

重试次数每轮持久化；成功、次数耗尽或文件切换 `as_of` 后清除重试。下单或发生失败的重试轮写决策记录并注明“重试第 N 次”；耗尽时告警，并在最后一条记录中注明耗尽一次。文件切换只记录日志，正常调仓仍沿用原门禁。

#### 目标过期自动清仓

未调仓的每个 tick 都检查过期清仓，包括文件缺失、不可读、状态非 `ok`、过期等情况。仅管理 `last_as_of` 非空的本策略自有持仓；从未调仓的账户持仓不自动清仓。`flatten_after_days=-1` 关闭；显式值为 1–60 天；0 默认取有效 horizon 加 `EffectiveMaxStaleDays(fileHorizon)`，horizon 优先用配置，其次用文件，两者未知时用 3（加密默认 3+5=8 天）。

当 UTC 日期距 `last_as_of` **超过**阈值且账户有持仓时，以空目标权重重用规划器、市价平掉全部持仓，同时清除待重试状态。模拟任一方向完全平仓后由模拟器取消同侧止损。首次尝试按 `flatten|<last_as_of>` 去重写一条决策记录，零失败完成时也写记录（首次即成功则合并为一条）；失败时每个 tick 继续尝试并告警，不重复刷首次记录。清仓不修改 `last_as_of`；新鲜 `ok` 目标到达且 as_of 间隔满足 horizon 时，正常门禁恢复调仓。

## 两路输出的关系

- 两路共用同一份 ensemble 分数；`live_scores.json` 不改。
- `quant_targets.json` 在 `export_live_scores` 之后生成。即使生成失败，也不能影响 `live_scores` 和挖掘主流程（异常记录在日志里）。
- 程序化交易员的成交、权益和决策记录都单独存，模拟盘结果就是这个规则的前向检验；研究记录模块要登记一个 `running` 条目。
