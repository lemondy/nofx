# us_stock 增加做空（第一阶段：模拟盘）设计（2026-11-04）

## 背景与决定

us-stock 现在只做多 Binance bStock 现货（如 AAPLBUSDT）。用户希望多空都能做，以平衡方向。

2026-11-04 用只读接口核实了账户的实际费率：

| 品种 | maker | taker | 资金费 | 其他 |
|---|---|---|---|---|
| bStock 现货 | 0 | 0.10%（BNB 抵扣 75 折） | 无 | 1 倍，不会爆仓 |
| 美股 U 本位永续（`TRADIFI_PERPETUAL`，如 AAPLUSDT） | 0 | 0.04% | 每期结算；当前多为 0 或正值，即空头收钱 | 7×24 交易（周末也有成交），账户是双向持仓模式 |

另外两项事实：

- 当前 29 个候选 bStock，全部都有对应的美股永续合约。
- 合约账户和用户的手动交易、加密交易员共用。

用户的决定：

1. **做多继续用 bStock 现货。** 入场挂限价单争取 0 费率，止损走市价付 0.1%。
2. **做空用美股永续，先只在模拟盘运行。** 跑满预先登记的样本后，由用户决定是否转实盘。转实盘时另写合约下单代码，不在本期范围内。

## 范围

**本期做：**

- us_stock 引擎支持做空动作；
- 模拟账本支持空头；
- 候选列表加入做空候选；
- `create_pair.py` 支持 us_stock；
- `report.py` 支持本次配对需要的主指标。

**本期不做：**

- 实盘做空下单；
- 修改现有实盘 us-stock 交易员的配置；
- 前端新页面（只保证现有接口正确返回 `side`）。

## 方案总览

新增策略配置项 `stock_config.short_mode`：

| 取值 | 含义 |
|---|---|
| `off` | 默认值，行为与今天完全一致 |
| `paper` | 允许做空，空头只记在模拟账本 |

配置校验：

- `paper` 要求 `paper_trading=true`。本期不支持"多头实盘、空头模拟"的混合模式，避免实盘交易员的提示词和行为被改变。
- `live` 值在本期直接拒绝，报错写明"实盘做空未实现"。

线上验证用增量框架的 S2 配对。两个新模拟交易员都从实盘 us-stock 克隆，都设 `paper_trading=true`：

- **对照组：** `short_mode=off`。
- **候选组：** `short_mode=paper`。

现有实盘 us-stock 不改动。

## 1. 决策动作与校验（`kernel/stockengine`）

**新增动作：** `open_short`、`add_short`、`reduce_short`、`close_short`。

- `adjust_stop` 按持仓方向生效。
- `Decision` 不加 side 字段，方向由动作决定。
- `Position` 增加 `Side`（`LONG` / `SHORT`）。

**解析：** `validAction` 加入空头动作。修复逻辑（repair）覆盖 `open_short` / `add_short`。

**校验：** 空头逐条镜像多头规则。

| 规则 | 多头（现状） | 空头（新增） |
|---|---|---|
| 开关 | 无 | `short_mode=off` 时拒绝，原因 `SHORT_DISABLED` |
| 可做空 | — | 该股票须有 `TRADING` 状态的 EQUITY 永续，否则 `NOT_SHORTABLE` |
| 同一标的反向持仓 | — | 已持有多头时不能开空（反之亦然），原因 `OPPOSITE_HELD` |
| 限价 | ≤ 现价×1.01 | ≥ 现价×0.99 |
| 止损 | > 0 且 < 入场价 | > 入场价 |
| 止损距离 | (入场 − 止损) / ATR1d，落在预设区间 | (止损 − 入场) / ATR1d，同一区间 |
| 止盈 | > 入场价 | > 0 且 < 入场价 |
| 浮亏加仓 | 现价 ≤ 均价或 ≤ 初始止损时拒绝 | 现价 ≥ 均价或 ≥ 初始止损时拒绝 |
| 调整止损 | 须 < 现价，且不得低于原止损（只能上移） | 须 > 现价，且不得高于原止损（只能下移） |
| 信心门槛 | 80 | 80 |
| 持仓数上限 | `max_positions` | 多空共用 |

**仓位计算（`sizing.go`）：**

- 每笔风险 = |入场价 − 止损价| × 数量，风险比例和 regime 缩放与多头相同。
- **可用资金按 1 倍保证金计算：** 可用 = 权益 − 多头市值 − 空头名义价值，再减去本轮已占用的金额。
- 新增 `max_short_exposure_pct`，默认 0.5，限制空头名义价值占权益的比例。
- 单笔上限、总敞口上限对多空合计生效。

**舍入：** 空头止损按价格步长向上取整，多头仍然向下取整。

## 2. 提示词（`prompts.go`）

`short_mode=off` 时，提示词逐字节不变。由测试锁定。

`short_mode=paper` 时：

- **规则改写：** "只做多"改为"可做多（现货）也可做空（永续，1 倍）"，止损、止盈、调整止损的规则分方向写。
- **入场原则：**
  - 只在趋势周期向下或刚转弱时做空：日线 downtrend，或者反弹失败、重新跌破 EMA20 的 wedge_drop。
  - 结构止损放在最近确认的日线摆动高点上方。
  - 财报前同样避免开新仓。
- **持仓块：** 显示方向；浮盈百分比和 R 按方向计算。
- **周期行：** 同时显示 `long_setup` 和 `short_setup`。
- **候选表：** 保留规则加上"做空候选"，并标出每个标的是否可做空。
- **合约示例：** 增加一个 `open_short` 例子。

中文和英文提示词都要改。

## 3. 做空候选（`scan.go`、`cycle.go`、`auto_trader_stock_cycle.go`）

**判定：** `CycleAssessment` 增加 `ShortSetup`，同时满足以下条件为真：

- phase ∈ {downtrend, wedge_drop}；
- 价格低于下降的 EMA20；
- rs63_vs_SPY < 0。

只做判定，不打分。

**排序：** 新增 `TopShort(n, exclude)`，按以下规则取前 n 个：

1. 先取 `ShortSetup` 为真的标的；
2. 再取动量有效且在 SMA200 下方的标的；
3. 两组内部都按 Mom12_1 从低到高排序。

**数量：** 新配置项 `scan_short_top_n`，默认 3，只在 `short_mode=paper` 时使用。

**合并：** 多头候选和空头候选合并、去重后成为本轮可开仓集合。每个标的多空都可以开，由校验决定能否开空。

## 4. 价格与资金费

**标的映射：** 在 `market/usstock` 的注册表里补全"bStock 现货 → 永续"映射（AAPLBUSDT → AAPLUSDT），只收 `contractType=TRADIFI_PERPETUAL`、`underlyingType=EQUITY`、`status=TRADING` 的合约。

**空头报价：** 用永续的最新成交价（`/fapi/v1/ticker/price`）。决策时的报价同样要求不超过 30 秒。

**资金费：**

- 保护检查（protection tick）每次运行时，把上次处理时间之后已经结算的资金费计入空头。
- 数据来自 `/fapi/v1/fundingRate`，复用 `market/funding_history.go`。
- 计算：空头收到 = 费率 × 名义价值，费率为负时为支付。
- 金额累计写入持仓的 `funding` 字段，并计入现金。

## 5. 模拟账本（`store/stock_paper.go`）

**表结构：** `stock_paper_positions` 增加两列：

- `side`，默认 `LONG`，老数据自动视为多头；
- `funding`，默认 0。

每个（交易员, 标的）仍然只能有一条 OPEN 记录。

**新增按方向开平的函数：**

- **开空：** 现金 += 成交额 − 手续费，持仓记录负债。
- **平空：** 现金 −= 数量 × 平仓价 + 手续费；盈亏 = (均价 − 平仓价) × 数量 − 手续费 + 资金费。

**权益：** 现金 + 多头市值 − 空头数量 × 现价。

**手续费（两组相同，保证配对公平）：**

| 成交方式 | 现货多头 | 永续空头 |
|---|---|---|
| 限价成交 | 0 | 0 |
| 市价、止损触发 | 10 bp | 4 bp |

以前模拟多头不收手续费，这次改为收，只影响模拟交易员。

**止损止盈触发（每 60 秒检查一次，两组相同）：**

- **多头：** 价格 ≤ 止损时，按 min(止损, 现价) 成交，跳空按更差价格算；价格 ≥ 止盈时，按止盈价成交。
- **空头：** 价格 ≥ 止损时，按 max(止损, 现价) 成交；价格 ≤ 止盈时，按止盈价成交。
- **周末：** 空头 7×24 都检查。

**限价入场：**

- 多头：现价 ≤ 限价时立即成交。
- 空头：现价 ≥ 限价时立即成交。
- 都不成交则拒绝，与现状一致。

## 6. 其他需要按方向处理的地方

- `auto_trader_stock_state.go`：模拟持仓汇总、重新估值、权益计算。
- `auto_trader_stock_api.go` 和 `api/handler_usstock.go`：浮动盈亏按方向计算，`side` 返回真实方向，权益公式同上。
- `auto_trader_stock_regime.go`：regime 减仓本期只减多头，空头不受影响。在代码注释里写明。
- 每周回撤熔断：本来就按权益计算，不用改。
- 实盘路径：`short_mode` 只能在模拟盘开启，实盘路径不会出现空头。实盘执行器再加一道防御，收到空头动作直接拒绝。

## 7. 配对工具（`scripts/promotion`）

**`create_pair.py` 支持 us_stock：**

- 源交易员是 us_stock 时，两个克隆都保留原来的 binance 交易所引用，不新建模拟交易所，因为 us_stock 的模拟模式依赖 binance 行情。
- 两个克隆的配置都强制写入 `stock_config.paper_trading=true`。
- 候选组再写入特性 `stock_config.short_mode=paper`。
- 两个克隆的模拟现金相同，默认 1000 USDT，可以用参数指定。

**`report.py`：** prereg 增加两个可选字段，老配对行为不变：

- `metric`：默认 `avg_r`；可选 `daily_return`，即每日权益收益率的配对差，按周聚类计算 SE。
- `trade_ratio_max`：默认 1.5；设为 null 表示不设上限。

## 8. 预先登记（S2）

| 项 | 取值 |
|---|---|
| track | us_llm |
| change_type | E（新增交易方向，没法离线重放，直接进线上配对） |
| 主指标 | 每日权益收益率差（`metric=daily_return`），按周聚类 SE |
| 非劣效边际 | 每日 0.02% |
| 开仓数比 | 下限 0.67，不设上限（`trade_ratio_max=null`） |
| 最短运行 | 45 个美股交易日，且候选组平仓 ≥ 30 笔、其中空头 ≥ 15 笔 |
| 提前止损 | 14 个交易日后，若 Δ < −2·SE 则停止 |
| 辅助指标（只报告，不参与判定） | 空头自身的 avgR、候选组多头 avgR 与对照组之差、两组最大回撤、空头资金费合计 |

两点说明：

- 实盘 us-stock 3 周只开了 9 笔，所以没有沿用 50 笔的默认门槛。即使用 45 天、30 笔，统计功效也偏低，结论要谨慎看待。
- 判定为 `promote` 后，由用户决定是否转实盘。转实盘要另写合约执行器，范围包括：实盘下单、保护单、所有权规则、与手动交易隔离，并单独评审。

## 9. 任务拆分与分派

| 子任务 | 内容 | 文件范围 | 执行者（档位） |
|---|---|---|---|
| T1 | 第 1–6 节：引擎、模拟账本、价格与资金费、状态和接口 | `kernel/stockengine/*`、`trader/auto_trader_stock*.go`、`store/stock_paper.go`、`store/strategy.go`、`store/strategy_stock.go`、`market/usstock/*`、`api/handler_usstock.go` 及对应测试 | Codex xhigh（涉及交易和风控，至少按"一般"档） |
| T2 | 第 7 节：`create_pair.py` 和 `report.py` | `scripts/promotion/*` 及测试 | opencode GLM 5.3 flash（中等） |

- T1 和 T2 改动的文件互不重叠，可以并行。
- 主 agent 审查 diff，重跑 `go build ./... && go test ./kernel/stockengine/ ./trader/ ./store/ ./market/usstock/ ./api/` 和 `pytest scripts/promotion`。
- 合并、上线并重启后端后，用 `create_pair.py` 建对，并写入 prereg。

## 10. 验收

**`short_mode=off` 的回归：**

- 现有 us_stock 测试全部通过；
- 提示词快照逐字节不变；
- 模拟多头的手续费和跳空规则改动有单独测试。

**空头路径的测试：**

- 每条校验规则；
- 仓位计算（含 1 倍保证金上限）；
- 账本开平和资金费；
- 止损止盈触发（含跳空）；
- 权益计算；
- 接口返回的 side；
- `short_mode=paper` 且 `paper_trading=false` 被拒绝；
- 实盘执行器拒绝空头动作。

**上线后：**

- 候选模拟盘第一个交易日的提示词里能看到做空规则和可做空标记；
- 对照模拟盘的提示词与实盘 us-stock 结构一致。
