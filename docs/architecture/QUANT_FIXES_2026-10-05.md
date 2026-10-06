# 量化交易审查修复记录 · 2026-10-05

本次修复对应 [原始审查报告](/Users/zhangyun/workspace/nofx/docs/architecture/QUANT_REVIEW_2026-10-05.md) 中 F01–F16。原报告和复现证据保留，用于区分修复前行为与当前行为。修改尚未提交、部署或进行实盘下单。

重点是消除已复现的执行、保护、数据和参数发布缺陷。策略收益、ATR/布林参数优劣以及组合风险模型的有效性，不能由这些代码测试证明。

## 1. 逐项修复

| 编号 | 当前行为 | 主要实现 |
| --- | --- | --- |
| F01 | 多头高价成交、空头低价成交计为不利滑点；有利成交不触发不利滑点告警。超限提交退出，并保留账户恢复阻断，直到新持仓快照确认已平仓。 | [成交执行](/Users/zhangyun/workspace/nofx/trader/auto_trader_orders.go:373)、[成交风险](/Users/zhangyun/workspace/nofx/trader/auto_trader_fill_risk.go:13) |
| F02 | BTC 4h 门控取真正的已闭合 4h 数据，不再用 1h 收盘数组代替。在线门控与历史回放调用同一 BTC regime 分类器。 | [信号参数](/Users/zhangyun/workspace/nofx/kernel/engine_prompt.go:1596)、[BTC 数据](/Users/zhangyun/workspace/nofx/kernel/engine_data_binance.go) |
| F03 | `BTC_4H_STRONGBULL`、`SHORT_TOP_CONFIRM_MISSING` 等绝对禁开原因覆盖市价执行；已启用的日线逆势禁空同时覆盖市价与限价。 | [硬风控](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go)、[执行门控](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending_risk.go) |
| F04 | 保护核验检查有效状态、平仓方向、触发价和实际覆盖数量；单向模式需要 reduce-only/close-position 语义。同一订单 ID 不重复计量。不足量补挂后重新回读。 | [保护契约](/Users/zhangyun/workspace/nofx/trader/auto_trader_protection.go)、[watchdog](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:1574) |
| F05 | 价格越过记录止损时立即提交程序退出。止损补挂连续三次失败进入退出；保护未知期间账户禁止新增风险。退出回执本身不等于持仓已消失。 | [watchdog](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:1574) |
| F06 | 账户熔断、安全模式和保护故障会撤销同账户增加风险的存量挂单，涵盖活跃策略、持久化停用策略记录和网格预留。撤单后查询最终成交并保护残量；未确认终态保持阻断。挂单继续等待前重新检查行情方向、BTC、日线及相关硬门。 | [挂单风险](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending_risk.go:51)、[挂单生命周期](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go) |
| F07 | Hyperliquid 使用包含触发信息的订单接口；Gate 修正触发规则及有数量平仓请求；KuCoin 解析包装响应、剩余合约数量、触发方向及官方成交均价字段。Aster、Bybit、OKX 回读补齐平仓标志。无法完整读取的分页/接口结果按未知处理。 | [Hyperliquid](/Users/zhangyun/workspace/nofx/trader/hyperliquid/trader_account.go:550)、[Gate](/Users/zhangyun/workspace/nofx/trader/gate/trader_orders.go:320)、[KuCoin](/Users/zhangyun/workspace/nofx/trader/kucoin/trader_orders.go:562) |
| F08 | 裸仓 ATR 恢复读取真实持仓数量；SL 成功后立即记录止损和初始 R，不等待 TP 成功。TP 失败保留补挂机会，重试不重复下已核验的 SL。 | [恢复保护](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:2607) |
| F09 | 市价和限价成交均保留原始绝对结构 SL/TP，不随成交差价平移。用真实成交价重算方向、净 RR、强平距离近似和单笔预算；不合格成交撤剩余单并退出。限价退出原因持久化到 `RecoveryReason`。 | [成交风险](/Users/zhangyun/workspace/nofx/trader/auto_trader_fill_risk.go:13)、[持久化挂单](/Users/zhangyun/workspace/nofx/store/pending_entry.go) |
| F10 | 突破和做空扫描器丢弃仍在形成的 K 线，确认信号只使用已闭合柱。 | [K 线解析](/Users/zhangyun/workspace/nofx/market/breakout/binance.go:265) |
| F11 | 15m 合成 1h 按 UTC 整点分组，要求四根连续柱，丢弃残缺桶。信号时间是闭合时间，结构随可用闭合数据重建；固定收益期限检查实际时间，缺柱不延长期限。BTC 回放在足够历史之前保持 neutral，不使用未来 regime。 | [历史回放](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:307) |
| F12 | 参数先写临时文件、同步并原子替换，成功后才更新内存。参数读取返回深拷贝，避免调用者越过持久化修改权重。摘要/标签持久化失败不报告成功，也不推进基于未保存数据的研究。 | [参数保存](/Users/zhangyun/workspace/nofx/market/breakout/params.go)、[回放摘要](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go) |
| F13 | 关闭“24h 信号代理收益直接发布实盘参数”的路径。保留 purge 的时间留出，并要求候选在留出段胜过现行参数；结果保存候选及研究范围，`applied=false`，不调用实盘参数写入。 | [候选研究](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:427) |
| F14 | 做空标签取信号后固定 24h 时点之前最后一根已闭合 1m 柱，精度误差小于一分钟；包含该期间历史资金费和固定成本估计。停机后补跑不再用当前 ticker。旧标签升级至 v2 重建，缺数据记录原因并退避重试。 | [历史标签](/Users/zhangyun/workspace/nofx/market/breakout/shorttuner.go:462) |
| F15 | 同一 UTC 日样本先聚合，隔日取块，减少重叠 24h 标签与重复币种伪重复。每因子至少 30 个块，Fisher z > 3 才产生权重调整；投影后权重仍满足 0.03–0.30、和为 1。即使研究开关打开，也只保存提案，不写实盘权重或推进上线游标。 | [做空调权研究](/Users/zhangyun/workspace/nofx/market/breakout/shorttuner.go:304) |
| F16 | 规则解析按字段、值类型、有限数值和运算符白名单校验；不支持的字段拒绝保存。已有无效 blocking 硬规则明确阻断，而不是静默失效。AI 批量保存返回保存数量和逐项拒绝原因。 | [规则引擎](/Users/zhangyun/workspace/nofx/kernel/rule_engine.go:45)、[规则 API](/Users/zhangyun/workspace/nofx/api/handler_review.go) |

KuCoin 成交字段核对依据包括官方订单响应中的 `avgDealPrice`、`dealSize`、`isActive`、`cancelExist` 及分页字段：[KuCoin Get Order List](https://www.kucoin.com/docs-new/rest/futures-trading/orders/get-order-list)。适配器回归使用构造响应和模拟 HTTP，未进行交易所实盘订单验证。

## 2. 成本和恢复契约

新增策略字段 `risk_control.entry_round_trip_cost_bps`，未填写或非正值默认 20bps（0.20%）。这是手续费与滑点的配置估计，不是针对所有交易所、费率等级和流动性的实测值；资金费不在普通开仓的这一固定预算中。

设真实入场价为 E、正确方向的止损距离为 D、目标距离为 T、往返成本估计为 C：

```text
C = E × cost_bps / 10000
净 RR = (T − C) / (D + C)
允许数量 ≤ 净值 × 单笔风险比例 / (D + C)
```

目标菜单、最低仓位可行性、开仓净 RR、单笔缩仓及成交后复核采用这一成本口径。未填写单笔比例时，缩仓和成交复核都使用 1.5%，不会因零配置跳过成交预算。普通账户聚合风险仍以已有止损金额模型为基础，尚不等于相关性/跳空压力损失模型。

限价成交不合格时先保存 `RecoveryReason`，撤销剩余委托，退出已成交头寸；只有订单终态和新快照的空仓都确认后才删除恢复记录。新列由既有数据库 AutoMigrate 纳入，未手工修改运行中的数据库。

保护故障与退出待确认故障按来源区分：有效保护不能清除尚未完成的退出；新快照确认空仓可以清除该退出故障。挂单撤销故障只在重新核验完成后解除。人工的非减仓触发入场单不会被当作有效保护或孤儿保护撤掉。

对于停用策略在熔断撤单期间发生的成交，恢复其原始全量 SL，保留所属策略的持久化记录。当前策略的 TP 平仓比例不能代替停用策略的退出政策，因此该处不擅自重设 TP 比例。

## 3. 自动进化的发布边界

突破回放结果增加 `candidate_params`、`applied`、`evaluation_scope`。研究提案未通过完整订单执行验证之前，不更新当前扫描参数。

`short_tuner_enabled=true` 现在允许做空权重研究提案。提案默认写入 `data/shortscan_weight_proposal.json`，记录现行和候选权重，并标记 `applied=false`。使用自定义实时权重还必须显式有 `short_weights_validated=true`；研究程序不会设置该标志。没有这一验证标志的旧自定义权重回退到默认权重。该标志代表显式的人工发布边界，代码尚未自动核验其验证证据。

做空样本记录 `label_version=2`、标签时点、实际评价时点、成本、资金费、失败原因和重试信息。保留 180 天日志，每次最多补算 50 个标签，失败退避最长 16 小时。隔日分块减少期限重叠，但不保证跨币种、跨市场状态完全独立；Fisher 检验仍依赖近似假设，不能等同于可交易的超额收益证明。

下列研究基础设施仍需实现，当前通过阻断自动发布来避免把代理统计直接用于实盘：真实限价成交与未成交、部分成交、资金费/滑点、实际 SL/TP/trailing/时间退出的订单回放；组合并发和风险预算回放；独立样本验证、多重试验控制、候选版本与回滚证据。报告中的信号收益不能称为完整策略净收益。

## 4. ATR + 布林线及其余策略建议

保留现有“结构失效位 + ATR 缓冲”的初始止损和已有布林市场状态证据。本次修复不把未经验证的上下轨止盈止损规则发布为默认交易方案。ATR + 布林复合方案仍按照原报告区分趋势与震荡做对照研究；其参数、成交率、净收益和回撤需要上述真实执行回放验证。

原报告中其余设计建议，例如空头跌破反抽入场、候选的可交易性分层、所有新开仓的统一新时点结构快照、相关性/BTC beta 压力预算、净值高水位和出入金、原生 bracket/成交事件、统一退出状态机，仍属于后续实现范围。已有账户回撤阈值仍是相对初始本金的亏损阈值。既有无效 blocking 规则现在会显式阻断，应按返回错误修正配置。

## 5. 验证

完整回归命令：

```sh
go test ./trader ./kernel ./market/breakout ./trader/... ./api ./store -count=1
```

新增关键用例的竞态检查：

```sh
go test -race ./trader ./kernel ./market/breakout ./trader/gate ./trader/hyperliquid ./trader/kucoin -run 'Test(Audit05|Fix05)' -count=1
```

证据文件：[完整回归输出](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-05/evidence/fixes-suite-output.txt)、[竞态检查输出](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-05/evidence/fixes-race-output.txt)。竞态检查包含 macOS 链接器 `LC_DYSYMTAB` 警告，无数据竞态报告。检查不启用实盘交易测试开关；部分既有测试会读取公开行情。没有对运行中的交易实例重启、发布或下单。

关键回归涵盖不利滑点方向、做空禁开路径、日线限价门控、足额保护、重复订单、已越过止损、连续恢复失败、退出故障解除、部分成交持久化恢复、默认单笔预算、撤单未确认阻断、SL 成功而 TP 失败、闭合柱、整点聚合、固定历史标签、弱相关及伪重复样本、不发布研究参数、参数持久化失败和规则白名单。
