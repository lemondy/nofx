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
- **加密**：多空中性加空头保护。做多因子排名前 20%、做空后 20%，按波动率倒数加权；空头排除近 7 日暴涨和上市不足 90 天的币；设单币上限；空头另加灾难止损。
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

  只要不是 `ok`，`weights` 就必须为 `{}`，交易员维持现状、不开新仓。
- **weights**：账户权益的名义比例。加密时正为多、负为空，多头合计 ≈ +0.5，空头合计 ≈ −0.5，毛敞口 1。美股只做多，合计 ≈ 1.0。key 的写法：
  - 加密用永续合约 symbol，例如 `AAVEUSDT`；
  - 美股用 bStock 现货对，即标的代码加 `BUSDT`，例如 `AAPLBUSDT`。
- 文件写入必须原子（同目录临时文件 + rename）；Go 侧读到损坏或不完整的 JSON 时跳过本轮。

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
| `short_stop_pct` | float | 0.5 | 仅加密：空头灾难止损，触发价 = 入场价 ×(1+0.5)；填 0 关闭 |
| `max_stale_days` | int | horizon+2 | `as_of` 落后今天（UTC）超过这么多天，就不按它调仓，并告警 |
| `targets_path` | string | `$FACTOR_MINING_DIR/{market}/quant_targets.json`，未设置时为 `<repo>/data/factor_mining/...` | 覆盖路径，测试用 |

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
  5. 加密空头每次开仓或加仓后，重设该币的灾难止损。
- 失败处理：单笔失败记录下来并继续执行其余订单；整轮结果写入一条决策记录，前端可见，内容包括 as_of、规则名、每笔订单、跳过原因和失败原因。
- 权益快照沿用现有机制定期写入，供晋升报告和前端曲线使用。
- **只允许模拟**：
  - 加密：交易员的 exchange 必须是 `paper`；
  - 美股：必须是模拟账本模式，且绝不调用真实下单接口。

  这两条在构造交易员时就校验，不满足则拒绝启动，并且有测试覆盖。

## 两路输出的关系

- 两路共用同一份 ensemble 分数；`live_scores.json` 不改。
- `quant_targets.json` 在 `export_live_scores` 之后生成。即使生成失败，也不能影响 `live_scores` 和挖掘主流程（异常记录在日志里）。
- 程序化交易员的成交、权益和决策记录都单独存，模拟盘结果就是这个规则的前向检验；研究记录模块要登记一个 `running` 条目。
