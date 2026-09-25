# 执行链复审与修订设计（2026-09-26）

审查基线：`5e0792e5`；对比基线：`d987d766`。本次检查实现与调用链，不将提交说明视为修复证据。未下实盘订单，未调用 ZCode。生产代码未修改。

## 结论

当前修复不能整体验收。3d/1w 时长补齐、常规充足历史下布林带闭合索引、Binance 按方向撤止损和保护单方向匹配，局部实现方向正确；但成交、委托、保护单、托管所有权和风险预算尚未形成一致的生命周期。

上一轮将双向保护问题归为 P0 过重：它依赖双向持仓及特定操作，本文归为 P1（高优先级），不称为无条件触发的问题。此前“立即保护部分成交”的方案也需补充独立成交监听：只在 AI 决策周期中查询订单仍有分钟级保护延迟。

## 可定位的问题

### R1 / P1：保护数量修复读取了错误字段

`trader/auto_trader_vol.go:455` 的 positionQty 只读取 `quantity`；Binance、Bybit、OKX 的 GetPositions 返回 `positionAmt`，short 可能为负。正常持仓也返回 0。moveStopExchange 在读取数量前就撤止损，因此 Bybit/OKX 仍会以零数量重挂；持仓查询失败也走同一条路径。

修订：将仓位映射转为强类型快照，使用绝对基础资产数量；查询返回错误而不是静默 0。撤单前完成数量、目标方向和新止损合法性校验。未知数量不撤旧单。

### R2 / P1：全成交重复创建保护单，失败也推进水位

`trader/auto_trader_pending.go:436` 调用 protectExecutedSlice 后，448 行再次按原始计划、全部数量调用 placeProtectiveOrders。一次性成交 Q 且 TP 比例为 0.5 时会尝试挂两张 0.5Q TP，使总目标从半仓变为全仓；保护价格还混合实际均价重锚值和原计划值。重复 SL 可能被交易所拒绝。

`protectExecutedSlice:604` 中 placeProtectiveOrders 无返回值，失败后仍把 ProtectedQty 更新为 executed；同数量下一次直接跳过。split TP 在挂单成功前标记 runner，使失败的 TP 后续被 watchdog 跳过。ProtectedQty 未持久化，重启会重复处理已保护成交量。Binance SL 为全仓 closePosition，不能将追加成交简单转换成追加一张 SL。

修订：以订单成交累计量驱动单一保护 reconcile；删除 FILLED 旧挂单分支。SL、TP 分别跟踪期望状态、已确认订单 ID、数量、触发价和失败状态。仅交易所确认后推进对应状态；重启时核对真实挂单，不能仅依赖内存水位。分批 TP 累计目标基于已成交量与已止盈量计算，不重复增加。

### R3 / P1：市价成交价仍未进入重锚链路

`trader/auto_trader_orders.go:270` 读取 avgPrice，但 Binance OpenLong/OpenShort 的返回 map 仅含 orderId、symbol、status。recordAndConfirmOrder 对 Binance 在 `trader/auto_trader_decision.go:284` 直接返回，不更新该 map。fillPrice 因此为 0，reanchorProtectivePrices 静默跳过。把重锚移动到记录前只修复顺序，未修复实际数据来源。

修订：统一 OrderExecution 结构，区分 accepted、partially_filled、filled；均价、累计数量来自确认过的成交事件/订单查询。未知成交价不伪造“最终成交价”，保留待确认状态并持续补全。保护监听与 AI 请求解耦，首次成交即进入保护流程。

### R4 / P1：按方向隔离只覆盖移动止损，开平仓仍全交易对撤单

`trader/binance/futures_orders.go:16,71,170,228` 的市价开仓、全量平仓仍调用 CancelAllOrders(symbol)。AI 开 long 可以撤掉已有 short 的 SL/TP，包括手动 short 的保护单；即使后面的下单失败，撤单已经发生。手动仓 watchdog 被 hands-off 跳过后，不会自动恢复。

修订：所有清理都必须定位订单 ID、方向和归属；开新仓不得预先清除同交易对全部委托。其他适配器缺少按方向能力时不得回退宽范围撤单。平仓清理只清本轮托管仓位关联订单。

### R5 / P1：撤单结果未确认就遗忘订单

`trader/auto_trader_pending.go:537` 在 CancelOrder 返回错误时仍删除内存和数据库 pending 记录，并通知“已撤销”。部分成交失效分支也忽略撤单错误。未撤掉的委托可能继续成交且不再被跟踪；挂单仓位名额和保证金预留也随之释放。撤单与新成交竞态未进行终态查询。

修订：进入 cancel_requested；撤单超时保留状态和预留。查询确认终态并处理最终累计成交量后，才结束 entry 生命周期。保护尚未确认的残余仓位交由持久保护任务继续处理，不能随 pending 行删除而丢失。

### R6 / P1：托管所有权在迁移、重启和离线成交链路不完整

`trader/auto_trader.go:713` 使用所有 trader 共用的 data/ai_marks_seeded 文件；第一个 trader 写入后，其余 trader 跳过迁移。迁移还把当前所有仓位直接认定为 AI，包括原先手动仓，与 hands-off 目标冲突。

`trader/auto_trader_loop.go:586` 在“当前仍存在、但缺少开仓时间”的首次见到分支删除持久 AI 标记；这不是 position-gone 分支。真正的消失清理分支（659 行起）反而未删除持久标记。

`trader/auto_trader_reconcile.go:171` 离线全成交路径未 markAIManaged，并先删 pending 行再查询持仓。查询失败会丢失恢复依据；已存在迁移旗标时，这类仓位可能一直被新 watchdog 当作手动仓跳过。

修订：所有权按 trader/account、symbol、side、position generation 保存，并关联可验证的 AI entry order IDs。迁移使用数据库逐 trader 的事务记录，依据历史 AI 委托匹配，不直接接管全部仓位。未知归属明确保留 unknown。同一成交处理服务覆盖在线/离线路径；先持久登记成交与所有权，再协调保护。只有确认仓位生命周期结束才清标记。人工与 AI 同方向混仓的策略需显式定义，不能靠 symbol+side 区分数量归属。

### R7 / P1：调整空头止损校验的是多头所有权

`trader/auto_trader_risk.go:1069` 根据 Action 是否包含 short 判断归属；实际 action 是 adjust_stop_loss，因而总检查 long。随后却按止损价格匹配实际仓位。只有 AI short 时会误拒；AI long 与手动 short 同时存在时，可能通过 long 校验后修改手动 short。

修订：决策携带明确 position side/position ID，解析出目标持仓后再检查该持仓归属；有歧义拒绝，不能用价格猜方向。

### R8 / P2（重复计数）及 P1（跨周期漏算）：风险预留生命周期错误

`trader/auto_trader_risk.go:1598` 返回 newRisk = candidateRisk + reserved，1741 行又将其累加到 reserved。连续每笔风险 20 时，预留从 20→60→140，而不是 20→40→60。该预留还在后续硬门检查前记账，被后续拒绝的决策也占预算。

`trader/auto_trader_loop.go:294` 每周期清零，accountRiskExposureBlocks 只计已持仓，不计上一周期未成交 entry。因此不同周期的限价单仍能分别通过上限、随后一起成交。仓位数和保证金上限不能替代 stop-risk 上限。

修订：函数返回独立 candidateRisk；预算账本区分现有持仓、未成交剩余委托、本批已通过计划。仅最后接受的决策预留，失败释放；部分成交将 reservation 转为 position risk，不能双算。预算值按最终数量/价格/止损计算，并在提交订单前复核。

### R9 / P1：OpenBB 回退将不同周期的数据冒充目标周期

`provider/openbb/client.go:268` 将 4h 映射为 60m，未支持的 3m/2h/3d 等也默认 60m。CryptoKlines 原样返回；`market/data_klines.go:503` 的回退直接交给下游按原请求周期计算。导致 ATR、结构、滚动收益窗口和闭合时间同时失真。此问题仅在前两级源失败且 OpenBB 返回数据时触发。

修订：结果携带真实 interval、venue、instrument、quote currency、as-of、closed-through。4h 必须由完整、对齐的 1h OHLCV 聚合而成；不足或缺条不输出完整 4h。不能从 1h 构造 3m，不支持直接返回错误。现货 USD 数据不能无标记替代交易所 USDT 永续的执行依据。

### R10 / P1：跨交易所口径仅增加日志，尚未修复

`market/data.go:172` 仍获取 Binance ticker；getKlinesWithFallback 在其他交易所 CoinAnk 失败时也退回 Binance。warning 不约束实际下单。此项仍待实现，不能标为完成。

修订：执行报价始终来自下单交易所；参考市场可用于背景分析但需标注且不覆盖执行快照。行情失效、跨标的或跨周期不一致时停止新开仓，已持仓保护继续依据可用的交易所状态运行。

### R11 / P2：分类测试注入没有设置被测分类表

`market/funding_history.go:172` 更新 binanceListed/usEquitySymbols，却只把 bstockSymbols 初始化为空。IsBStockSymbol / IsUSEquitySymbol 依赖后者，注入的 AAPL 并未被判定为股票。不能用新增测试名称或提交消息证明股票日线止损已验证。

修订：依赖注入完整分类接口或完整快照，恢复全部先前状态并控制时间/加载逻辑。股票分析侧还需保证所需 1d 数据存在，避免分析侧按 1h 生成计划而执行侧按 1d 拒绝；两侧共用同一个 stop-band 解析器和明确的降级策略。

## 分批实现与验收

| 批次 | 实现边界 | 必须验证的行为 |
| --- | --- | --- |
| A：接口契约 | TypedPosition / OrderExecution；数量与成交价；方向与归属 | Bybit/OKX 负 short 数量正确取绝对值；查询失败不撤旧单；Binance ACK 无成交价进入待确认；AI long 操作不撤手动 short 委托 |
| B：成交与保护状态机 | 在线/离线统一 reconcile；SL/TP 各自确认；撤单恢复 | NEW→PARTIAL→FILLED、直接 FILLED、PARTIAL→CANCELED；重复事件/重启不重复 TP；0.5 TP 比例最终目标不超过半仓（扣除已执行）；挂单超时先查再重试；部分成功只重试缺失腿 |
| C：所有权 | 成交登记、数据库迁移、position generation、unknown | 两个 trader 均独立迁移；旧手动仓不被整体接管；离线成交保留 AI 标记；时间未知不能删标记；外部平仓/重开不继承旧生命周期；调整 short 检查 short 归属 |
| D：风险账本 | 持仓+挂单剩余+本批计划，最终接受才预留 | 连续候选 20/20/20 正确累计 60；后续门拒绝不占预算；跨周期两个各 6% 限价单不能在 10% 上限下都进入可成交状态；部分成交转换不双算；重启重建预留 |
| E：行情与指标 | 真实 source/interval 元数据、受控聚合、股票统一 stop-band | 4×1h 聚合正确；缺条不伪装 4h；3m 请求不得返回 1h；跨交易所报价不会覆盖执行价；3d/1w 未收线剔除；改变形成中 K 线不改变闭合指标；股票日线缺失的两侧行为一致 |

保护监听应独立于分钟级 AI 周期，并设置可测试的成交到保护时延目标。需要撤旧挂新的交易所适配器应有目标侧串行化、立即补偿与恢复任务；不能承诺网络失败下无裸露窗口。保护失败时不标成功、不释放恢复状态，并限制新增风险。是否自动紧急平仓属于交易行为决策，不能作为未明确的默认修复。

开仓价变化不应无条件平移结构性 TP/SL。实施时区分 absolute_structure 与 relative_distance 两种计划；前者保留结构位并按实际成交复算 RR/风险，后者可按已确认均价重锚。tick/lot 取整后的最终值进入记录和验收。需要改变现有策略语义时单独列为可审阅变更，不夹在接口修复中。

## 验证记录

执行 `GOCACHE=/private/tmp/nofx-review-gocache go test ./trader ./market ./kernel ./store ./api -count=1`。

- trader、market、store、api 通过。
- kernel 失败：TestStopBandScaleForStocks（1.50，期望 7.5）；TestApplyUSStockSessionBoost（60，期望 90）。已定位到分类注入实现缺陷，不将这两项简单归因于网络。
- 当前测试未检索到对 protectExecutedSlice、ProtectedQty、cycleRiskReservedUSD、AIManaged、closedBOLLValue 的直接命名覆盖；函数级测试通过不能证明上述跨模块状态流正确。
- 本报告的触发示例来自代码路径和数值推演；没有声称已做真实交易所故障注入或实盘复现。新状态机应通过假交易所/录制响应进行集成验收。
