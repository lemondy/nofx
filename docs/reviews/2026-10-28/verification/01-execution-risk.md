# 执行 / 风控 / 记账疑似问题核实

核实基准：`aa8c361d51cce65b94c98826868223a9c1106109`，本地 `HEAD = origin/dev`。生产数据为 2026-10-28 晚间只读查询结果，运行中的数据库会继续变化。交易规则仅使用指定的只读缓存，缓存时间 **2026-10-26 08:33:33 +08:00**；以下“交易所拒绝”复现使用 SDK 的内存 HTTP transport / 缓存规则判定，没有向真实账户发送订单。

未修改任何生产代码、缓存、日志或生产数据库，未 commit / stash / checkout。新增文件仅为 `trader/verify20261028_execution_test.go`、`trader/binance/verify20261028_execution_test.go`、`scripts/verification/tests/test_verify20261028_data.py`，均为本次临时验证文件，不提交。

证据附件：[验证测试输出](exec-tests.log)、[相关既有回归输出](exec-controls.log)、[原始基线失败](exec-baseline.log)、[取证 SQL 与结果](exec-sql-evidence.md)、[规则逐品种清单 CSV](exec-rules.csv)、[原始统计 JSON](exec-data-evidence.json)、[git 历史证据](exec-git-evidence.txt)。SQL 均带 LIMIT，未读取 decision_records，也未输出密钥、密文或密钥指纹。

## 1. 入场受理到 pending 落库之间崩溃；孤儿撤单最终成交残量

**结论：CONFIRMED。** 限价入场存在此窗口；重启后的孤儿撤单不查最终累计成交，原 SL / TP / 原始 R 计划不能由此路径恢复。

**证据：** `trader/auto_trader_pending.go:405` 调用 `PlaceLimitOrder`，收到成功结果后才在 `:431` 调用 `setPendingEntry`；`:58–65` 先更新内存，再写 shadow。`:93–111` 写库失败仅记录日志，仍可能向上返回入场成功。UNKNOWN 响应的追踪发生在错误返回路径，不能覆盖进程已经退出的窗口。

`TestVerify20261028_PostAcceptedBeforeShadowCrash` 通过真实 `executeOpenLimitLongWithRecord` 执行：

- `crash_after_post_ack`：fake 交易所保存已受理订单，在响应交接边界 panic，模拟进程退出；新建 AutoTrader 只保留原临时数据库和 fake 交易所状态。
- `shadow_insert_failure`：仅在测试临时库创建拒绝 shadow INSERT 的 trigger；POST 成功返回，上层仍返回成功；随后模拟重启。
- 两种情况都让订单在孤儿撤单竞态中成交 `0.4`。关键输出均为 `shadow_rows=0 orphan_cancels=1 final_status_queries=0 residue=0.4 managed=false SL_calls=0 initial_SL=0`。

`trader/auto_trader_reconcile.go:200–218` 只扫描 open orders、按 lim 标签撤单并通知，没有查询撤单后的 status / executedQty。若订单重启前已经全部成交，它连 open-order 扫描也不会出现。普通 OrderSync 可恢复成交账目，但其输入没有原计划；正常已有归属注册表的重启中，未登记仓位还会受到 hands-off 规则约束（`auto_trader.go:1059–1071`）。

**实际影响：** 单进程不能消除进程退出窗口。可留下没有原计划保护、也未被 AI 接管的真实余仓；原始 R、退出模式无法自动恢复。本次没有证明生产已发生该事故。生产 `mac-nofx` 当前 `limit_entry_enabled=0`（默认市价），因此本项实测的限价分支不是其默认入场方式；显式限价、采用限价的 paper 策略仍适用，不能把限价测试扩大为所有市价路径均已复现。

**建议修复范围：** POST 前持久化带稳定 client ID 的完整入场意图，受理后推进状态；重启按 client ID 查回。孤儿撤单必须查询最终成交并核对真实余仓，在恢复保护或明确处置后才结束恢复。

## 2. MARK 计算追踪止损，CONTRACT_PRICE 条件单拒绝

**结论：CONFIRMED。** 给定 MARK=100.2、CONTRACT=99.9、新 SL=100.1，MARK 侧可生成该候选，SELL STOP_MARKET 的 CONTRACT 触发条件已经满足，可能拒单。直接拒单保留旧保护；替换分支会先撤旧保护，再尝试恢复，不能概括为始终原地保留。

**证据：** `auto_trader_stream.go:385、414` 将 MARK 传给追踪；`auto_trader_vol.go:231–261` 以该价格生成候选。`binance/futures_orders.go:1269–1288` 的 `stopWorkingType` 对流动性充足或 OI 未知品种使用 CONTRACT_PRICE，薄 OI 使用 MARK_PRICE；`SetStopLoss` 把此类型显式发送给 SDK。

`TestVerify20261028_MarkTrailingRetries` 以 entry=99、initial/旧 SL=98.5 和相应 ATR 精确生成 100.1，连续两轮执行：`attempts=2 recorded=98.5 old_stop_cancels=0`，说明失败后没有推进 recorded SL，下一轮重复尝试。

`TestVerify20261028_StopReferenceAndRestore` 通过 SDK 的内存 HTTP transport 注入 Binance 错误响应：

| 分支 | 两轮/一轮结果 | 旧保护与重试 |
|---|---|---|
| CONTRACT，直接返回 `-2021` | 两轮 POST=2、cancel=0 | 旧 SL=98.5 保留，新 SL 未生效；下一轮继续请求 |
| CONTRACT，先 `-4130`、替换时 `-2021` | 两轮 POST=6、cancel=2、restore=2 | 每轮撤旧单→新单失败→旧单恢复并查询确认；两次操作间存在空窗 |
| MARK_PRICE | 一轮 POST=1，100.1 接受 | MARK=100.2 尚未跨越 SELL stop；该价格分歧不会造成上述立即触发拒绝 |

SELL STOP_MARKET 按所选 MARK / CONTRACT 价格跌到触发价时触发，见 [Binance New Algo Order 官方说明](https://developers.binance.com/en/docs/catalog/core-trading-derivatives-trading-usd-s-m-futures/api/rest-api/trade)。此处 `-2021` 和 `-4130` 的响应顺序由 fake 注入，未声称实盘必然优先返回其中某一种。恢复失败时生产函数会返回错误（`futures_orders.go:1364–1383`），成功恢复测试不能证明所有故障下均有旧保护。

**实际影响：** `mac-nofx` 已启用 trailing；MARK/CONTRACT 基差可使计划收紧推迟，并产生重复请求。单进程不解决该问题。直接拒单并不等于旧止损丢失；`-4130` 替换则涉及撤挂空窗。本次最近 100,000 行日志抽样中未找到 `-2021`，不能据此排除历史发生。

**建议修复范围：** 候选合法性和最终条件单必须使用同一触发价格语义，并在提交前使用对应新鲜报价检查。对已跨越候选的情况明确处置，并限制无效价格重复替换；保留旧保护直到替代保护可确认。

## 3. 市价 MIN_NOTIONAL 固定 10；忽略 MARKET_LOT_SIZE

**结论：CONFIRMED。** 与缓存真实规则不一致，存在本地通过而按交易所规则应拒绝的数量/名义金额区间；当前差异主要是最小名义金额与市价最大数量，未发现 minQty 或 stepSize 分歧。

**证据：** `binance/futures_positions.go:223–225` 固定返回 10，`:229–245` 的市价检查据此判断；`futures_orders.go:207、268` 的开多/开空使用此检查。`futures_positions.go:276–305` 只解析 LOT_SIZE 的数量规则；限价路径 `futures_orders.go:970–990` 则使用缓存 `rules.MinNotional`。

只读统计脚本：`scripts/verification/tests/test_verify20261028_data.py`。缓存全部 932 个记录中，795 个 status=TRADING；其中 **795 个 MIN_NOTIONAL 均不等于 10，795 个 MARKET_LOT_SIZE 均与 LOT_SIZE 不同**。全部逐品种值见 [CSV](exec-rules.csv)，无需将“有哪些”限缩成几个示例。

| 缓存范围 | 品种数 | MIN_NOTIONAL 分布 | MARKET_LOT_SIZE 差异 |
|---|---:|---|---|
| 所有 TRADING | 795 | 5：785；20：6；50：3；0.001：1 | 795 个均仅 maxQty 不同；minQty、stepSize 均相同 |
| USDT 保证金永续 | 523 | 5：517；20：5；50：1 | 523 个均仅 maxQty 不同 |

USDT 永续中 >10 的完整清单：**BTCUSDT=50；BCHUSDT、ETCUSDT、ETHUSDT、LINKUSDT、LTCUSDT=20**，其余 517 个为 5。全缓存还包括 BTCUSD1/BTCUSDC=50、ETHUSDC=20；ETHBTC=0.001（不同计价范围，不按 USDT 金额解释）。因此多数品种固定 10 反而过严：合法的 5–10 区间会被本地拒绝；不能把所有 MIN_NOTIONAL≠10 都说成交易所拒单。

`TestVerify20261028_CachedMarketRules` 将指定缓存中的原始 symbol JSON 喂给真实规则解析器，测试报价固定为 10,000（用于构造规则边界，非实时行情）：

```text
BTCUSDT qty=0.004 notional=40 local_min=10 actual_min=50 local=PASS exchange_rule=REJECT
ETHUSDT qty=0.001 notional=10 local_min=10 actual_min=20 local=PASS exchange_rule=REJECT
BTCUSDT qty=120.001 LOT.max=1000 MARKET.max=120 local=PASS exchange_market_rule=REJECT
ETHUSDT qty=2000.001 LOT.max=10000 MARKET.max=2000 local=PASS exchange_market_rule=REJECT
```

**实际影响：** 当前 Binance USDT-M 默认市价路径适用。主 trader 历史订单中、仍在 TRADING 缓存里的 147 个品种全部有上述规则差异；其中较高最小名义金额的历史成交品种为 BCHUSDT、BTCUSDT、ETHUSDT、LTCUSDT。主策略 `min_position_size=5`，适配器固定 10 会额外阻止部分小单，也不能保证通过更高阈值。历史记录的这些品种最大单笔数量均未超过缓存 MARKET.maxQty，本次没有实际超量拒单证据。规则缓存距核实约两天，结论针对这份缓存，不冒充实时 exchangeInfo。

**建议修复范围：** 区分 MARKET 与 LIMIT 规则，统一适配器、sizing 和执行前检查的数据源；量化后再校验正确的 min/max/notional。保留规则刷新与拒单归因，避免把过时规则和数量过小都表现成普通提交失败。

## 4. 同一真实账户重复 exchange 记录导致锁/预留分离

**结论：REFUTED（当前生产配置中的重复记录）；代码层面的身份边界风险仍存在。**

**证据：** `account_execution.go:23–33` 的 key 是 `userID:exchangeID`，而不是交易所真实账户身份。只读 SQL：

```sql
SELECT exchange_type,COUNT(*) records,
       SUM(api_key LIKE 'ENC:v1:%') encrypted_keys
FROM exchanges GROUP BY exchange_type LIMIT 20;
-- binance: records=1, encrypted_keys=1；paper: records=4

SELECT COUNT(DISTINCT e.id) records,COUNT(t.id) running_traders
FROM exchanges e JOIN traders t ON t.exchange_id=e.id AND t.is_running=1
WHERE e.exchange_type='binance' LIMIT 1;
-- records=1, running_traders=3
```

重复检查结果：**未重复**。同类型+account_name 的运行记录重复组为 0；记录中 api_key 存储值相等的重复组为 0。后者是密文相等检查，随机加密的情况下不能代替明文 API key 指纹比较；本案不依赖它下结论，因为生产总共只有一条 Binance exchange 记录，已经排除了“同一 Binance 账户被配置为多条记录”这一条件。没有解密或输出任何凭据，也没有把可编辑的 account_name 当作 Binance UID。

运行标志为真的 Binance trader 除 mac-nofx 外，还有美股大师、us-stock，均指向同一记录；这不等于它们全部都是合约执行实例。四条 paper 记录属于独立模拟账户，不作为真实账户重复凭据。

**实际影响：** 按用户给定的单进程部署，本项重复记录导致的锁分裂当前没有数据证据，且单一记录的配置证据足以反驳。未来增加同账户其他 exchange 记录或多进程时，现有 key / 进程内 mutex 仍可能隔离预算。

**建议修复范围：** 若以后支持同账户多配置，按交易所可验证账户/子账户身份归组；多进程再补跨进程串行化或预算事务。当前不需要因本次核实修改生产配置。

## 5. 手工/外部开仓挂单是否进入账户预留

**结论：CONFIRMED（未登记开仓挂单不计入预留）。** 当前实盘是否恰有此类未成交挂单、是否已超预算，现有数据库不能证明。

**证据：** `account_execution.go:80–164` 只合并各 peer 的内存 pending、持久化 pending、网格 checkpoint，没有全量交易所开仓挂单快照。`refreshExecutionAccount` 的 `:192–209` 虽查询 open orders，但只查询已有持仓的 symbol，且用于获取 SL/TP；不把未知开仓单转换成预留。未成交且尚无持仓的 symbol 连这次查询也不会覆盖。

`TestVerify20261028_UnknownExchangeEntryReservation` 给 fake 账户加入手工 BUY LONG LIMIT，notional=800、1x；权益=1000、max_margin_usage=90%，再申请新保证金 200：

```text
manual LIMIT notional=800 local_reserved=0 new_margin=200
cap=900 local_admit=true total_if_filled=1000
```

加入正确预留 800 后，同一预算函数会拒绝新单。`auto_trader_risk.go:1094–1113` 的保证金预留与 `:2270–2290` 的组合止损风险都依赖已登记 pending，交易所实际冻结保证金并不能替代系统自定的 90% 和止损风险上限。

取证注意：用户明确会在同一 Binance 账户手动交易，应把外部挂单视为实际使用场景。但“client_order_id 为空即手工单”的数据库判据在本仓库不成立：`binance/order_sync.go:486–505` 创建成交订单时根本没有填写该字段，也统一写成 Type=MARKET。SQL 结果为 mac-nofx **1789 个订单、1789 个空 client ID、1789 个有成交量**；不能把它们全部当成手工单，也不能用 LIMIT 行为 0 推断用户从未挂过限价。生产 `trader_pending_entries` 此次查询为 0，同样不能证明交易所没有手工挂单。最近日志中有 `manual/external holding` 记录，但这些行属于股票账户持仓，不能用来量化 USDT-M 未成交挂单。

**实际影响：** 单进程、同一 exchange 记录能串行化程序内部操作，不能把用户在交易所 UI 的挂单加入本地预留。手工开仓单与自动单同时成交时，可超过组合风险/保证金策略上限；已成交手工持仓会进入交易所 GetPositions 的账户暴露统计。当前未知挂单数量及已超预算金额为证据缺口，需某一时点交易所全账户只读 openOrders 快照才能量化。

**建议修复范围：** 在账户准入阶段合并交易所真实开仓挂单，按真实订单 ID 去重，区分开仓、减仓和保护单；未知止损计划采用明确的保守风险策略。保留手工订单归属，不自动撤销它们。

## 6. 四位数量精度与 0.0001 清仓容差是否已影响当前 Binance 品种

**结论：REFUTED（当前 Binance 因 stepSize < 0.0001 品种而记错的推断）。固定精度的通用实现缺陷可以合成复现。**

**证据：** 指定缓存 **全部 932 个规则记录中，stepSize < 0.0001 的品种为 0**；在交易的 795 个品种最细步长为 0.0001，USDT 保证金永续的 523 个最细为 **0.001**。因此“这些更细步长品种”集合为空，生产 `trader_positions` 中对应集合的记录数也为 0；不能声称查到当前实盘被该条件截断的记录。脚本的 `fine_step_positions` SQL 使用空集合表达式 `symbol IN (NULL)`，结果 0，应理解为无适用品种，不是对全库所有记账错误的排除。

代码事实：`store/position.go:295–298` 对 Quantity / EntryQuantity 四舍五入到四位，`:320` 对减仓余量同样处理，`:337–338` 用 `<=0.0001` 判关闭；`position_builder.go:139、153` 也有固定清仓容差。均价本身使用 adaptivePriceRound，不能描述成“均价总是固定四位”；错误数量当分母仍能污染均价。

`TestVerify20261028_QuantityRoundingPotential` 在临时 SQLite 库构造非当前 Binance 规则的 step=0.00001：

```text
真实 0.00011 + 0.00001 @100 → 应为 quantity=0.00012、均价=100
实际入库 quantity=0.0001、均价=120
再减 0.00001 → 数据库 CLOSED，而真实应剩 0.00011
```

**实际影响：** 这份缓存支持当前 USDT-M 数量均为 0.001 或更粗的倍数，不能用这一疑点解释当前主 trader 的记账偏差。通用 Store 对将来更细交易规则、其他适配器、以及小于等于容差的合法余仓仍不稳健。本结论不覆盖规则曾在历史变更、人工导入或其他原因造成的账目错误，也不把股票 spot 的碎股数量套用到合约规则缓存。

**建议修复范围：** 数量运算按实际 step 或足够精度的十进制定点处理；清仓按实际成交余量及交易规则确认。保留均价与原始成交明细的可追溯关系，避免用已错误舍入的余量作分母。

## 7. 平仓净盈亏是否包含资金费

**结论：CONFIRMED（当前持仓历史净盈亏未包含资金费）。** 资金费已在独立账户流水/资金页链路记录，不能据此认为每笔 position 的 net 已包含它。

**证据：** `binance/order_sync.go:486–520` 只从 userTrades 取 Fee / RealizedPnL；`store/order.go:244–267` 的 ApplyTrade 传给 PositionBuilder 的也是 `fill.Commission / fill.RealizedPnL`；`store/position_builder.go:139–156、197–210` 累加这些值。该链路没有 FUNDING_FEE 分摊。

另一条独立链：`binance/futures_income.go:20–52` 读取全部 income type，`api/funds_sync.go:100–101` 写 AccountIncome；`fundsledger/income.go:15–16` 把 FUNDING_FEE 分类为 funding，供资金页使用，没有回写 trader_positions。持仓统计仍使用 `realized_pnl-fee`（`store/position_query.go:46`）。

同一窗口 **UTC [2026-10-01 00:00, 2026-10-28 00:00)**，限定 mac-nofx 的 exchange，且 account_kind=live_futures：

| 数据集 | 数量 | PnL / 资金费 | commission / fee | 净额 |
|---|---:|---:|---:|---:|
| account_income FUNDING_FEE | 258 | +2.24689603 | — | — |
| account_income REALIZED_PNL / COMMISSION | 481 / 794 | +44.34111283 | 11.31323579 | — |
| trader_fills（成交时间在窗口内） | 834 | +44.34111283 | 11.31973579 | — |
| trader_positions（窗口内 CLOSED） | 196 | +34.66092683 | 11.19685626 | +23.46407057 |

不同表的行集合、开平跨期及同步覆盖不同，不能把上表总额之差直接当成资金费，也不能直接给 23.46407057 加整个账户资金费。为排除这个问题，另按每个持仓的 entry→exit、symbol、position side 比对 fills：**12 个有资金费的最近平仓样本，12/12 的 position PnL=fill PnL、position fee=fill commission**，资金费单独存在。

四个具体样本如下，另查同账户同 symbol 的持仓时间重叠记录均为 0，可排除这些样本中资金费同时属于另一个本地持仓的歧义：

| position ID | symbol / side | position PnL = fill PnL | position fee = fill fee | 生命周期资金费 |
|---|---|---:|---:|---:|
| 445 | OKLOUSDT SHORT | 0.75279999 | 0.01305472 | +0.04941784 |
| 555 | WLDUSDT SHORT | 2.0678 | 0.0985997 | -0.00308245 |
| 494 | ZECUSDT SHORT | 2.13817 | 0.02396913 | +0.00766562 |
| 531 | GRAMUSDT SHORT | 0.5355 | 0.13371434 | -0.00995208 |

例如 #445 的 income 在同一生命周期内为 REALIZED_PNL=0.75279999、COMMISSION=-0.01305472、FUNDING_FEE=+0.04941784。当前 position 净额 **0.73974527**，含资金费应为 **0.78916311 USDT**，资金费未隐藏在任何一项里。完整带 LIMIT、带 exchange/time 过滤的 SQL 及结果见附件 `income_oct / funding_positions / funding_single_lifecycle / funding_sample_overlap`。

**实际影响：** 已有当前实盘生产数据证据。长持仓逐笔盈亏、胜率及基于 position net 的统计存在偏差；正资金费使当前展示偏低，负资金费使展示偏高。此结论针对持仓历史口径，账户权益及资金页已有不同数据链路，不能直接认定真实账户余额也漏记。

**建议修复范围：** 保留独立 gross / commission / funding 字段，按真实账户、symbol、持仓时间及 hedge 归属制定资金费分配规则；净额口径在页面、统计和回补中统一，避免将同一资金费重复分配。

## 8. TestB1BreakevenTrailingSameCycle 基线失败根因

**结论：REFUTED（该失败证明生产保本/追踪逻辑有 bug 的判断）；根因是测试 HTTP mock 已过时。**

**证据：** 原命令：

```sh
GOCACHE=$PWD/.gocache go test ./trader -run '^TestB1BreakevenTrailingSameCycle$' -count=1 -v
```

五个子例全部在 `review_batch1_20261007_test.go:230` 失败：`cycle did not read the mocked candles: requests=0`。尚未到 stop submissions 的业务断言。

原测试 `:185–187` 在测试运行中替换 http.DefaultTransport，假设 CoinAnk client 的 Transport=nil，发请求时会取替换后的默认 transport。`git log -S 'pubcache.Wrap(nil)' -- provider/coinank/coinank_api/kline.go` 和 `git blame -L 183,190 market/pubcache/cache.go` 一致指向：

```text
68e23b682a2a8c8b84be7ef5b9cda5db8e36a8b2
2026-10-25 21:51:22 +0800
perf(market) 公开行情跨交易员共享短 TTL 缓存 + singleflight（pubcache，叠加层）
```

该提交将包级 CoinAnk client 从 `Transport=nil` 改为 `Transport: pubcache.Wrap(nil)`（`provider/coinank/coinank_api/kline.go:66–69`）。`market/pubcache/cache.go:183–190` 在 Wrap 时就把 nil 解析为当时的 http.DefaultTransport 并保存在 wrapper 内，因此包初始化之后再替换默认 transport 不会换掉已捕获的底层 transport。原测试本身来自 `f7d4091f`（2026-10-07）。

`TestVerify20261028_PubcacheCapturesDefaultTransport` 全程 fake，按“先 Wrap(nil)，再替换默认 transport”复现：`original_transport_calls=1 test_replacement_calls=0`。

`TestVerify20261028_B1WithMarketDataSeam` 复用现有 marketDataFn 测试入口，保留原五组 entry/initial/current/mark/ATR/arm/lock 和全部止损期望，提供等效已收盘 K 线；五组均通过：

```text
early_and_lock:                   stops=[101]    recorded=101 opening_anchor=95
lock_only:                        stops=[101]    recorded=101 opening_anchor=95
early_short:                      stops=[99]     recorded=99  opening_anchor=105
opening_anchor_after_breakeven:    stops=[]       recorded=101 opening_anchor=95
armed_tightening:                 stops=[106.92] recorded=106.92 opening_anchor=95
```

**实际影响：** 该基线红测不能证明当前实盘没有执行保本/追踪；它证明旧的网络注入方法失效。使用当前测试入口后同样业务断言通过。本核实不由此推断整个 trader 包不存在其他缺陷。

**建议修复范围：** 更新这一个测试的行情注入方式，优先使用 marketDataFn 或显式可注入的行情 client；如需验证 HTTP 集成，显式注入缓存 wrapper 的底层 transport 并隔离缓存。无需为这个失败改变生产止损算法。

## 验证与交付状态

新增 8 个 Go 验证测试（含故障注入及子例）均通过；相关既有回归 TestEntryOrderOwnedBy、TestEntryClientIDShape、TestC2ExcludedPendingRestoredFromDB、TestThinStopWorkingType、TestB1MoveStopExchangeTightenOnly 同时通过。原始已知基线失败单独保留，没有改动原测试或让其静默跳过。

```sh
GOCACHE=$PWD/.gocache go test ./trader ./trader/binance \
  -run '^(TestVerify20261028_|TestEntryOrderOwnedBy$|TestEntryClientIDShape$|TestC2ExcludedPendingRestoredFromDB$|TestThinStopWorkingType$|TestB1MoveStopExchangeTightenOnly$)' \
  -count=1 -v
# ok nofx/trader；ok nofx/trader/binance
```

Python 只读取证脚本已成功运行。测试只向临时 SQLite 写入，生产 SQLite 始终经指定 mode=ro URI 访问；输出中无密钥原文。测试 GOCACHE 在交付前删除；临时测试保留供复核，未提交。

## 汇总表

| # | 核实问题 | 结论 | 当前实盘影响 |
|---|---|---|---|
| 1 | POST→计划落库窗口；孤儿撤单残量 | CONFIRMED | 故障注入证实可能丢计划和残仓保护；当前默认市价降低已测限价分支触发，未证明生产事故 |
| 2 | MARK/CONTRACT 止损语义分歧 | CONFIRMED | 新 SL 可拒绝、反复重试；直接拒单旧 SL 保留，-4130 替换需恢复并有空窗 |
| 3 | 市价最小名义金额 / MARKET_LOT_SIZE | CONFIRMED | 当前缓存全部在交易品种存在差异；6 个 USDT 永续阈值>10；无历史超 MARKET.maxQty 订单证据 |
| 4 | 同账户重复 exchange 配置 | REFUTED（当前配置） | 仅一条 Binance 记录，未重复；单进程下没有由此导致的锁分裂 |
| 5 | 未登记手工/外部开仓挂单预留 | CONFIRMED | 手动交易场景相关；可超策略预算，当前未知挂单实际规模缺交易所快照 |
| 6 | 更细数量步长已导致当前记账错误 | REFUTED（当前规则条件） | 无 step<0.0001 品种及相应记录；通用四位舍入缺陷已合成复现 |
| 7 | position net 未计资金费 | CONFIRMED | 生产数据已证实；逐笔收益和统计遗漏资金费，账户资金页为独立口径 |
| 8 | 基线红测意味着保本/追踪生产逻辑错误 | REFUTED | 68e23b68 改变 transport 捕获；原测试注入过时，五组业务预期均通过 |
