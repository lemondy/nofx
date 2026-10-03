# NOFX 全功能深度复审 · 2026-10-03

审查基线：`e68ed1fd9110`。这是对 2026-10-01 审查之后代码及修复的再次复审。结论以这个版本为准。

**确认 20 项需要处理的问题：12 项 P1、8 项 P2。14 个 Go 隔离回归场景和 2 个真实 React 页面组件场景复现了其中 14 项发现。现有常规测试通过，但成交保护、网格停机及恢复状态仍存在明显缺口，不能据此认为自动交易链路已经可靠。**

P1 表示可能造成新增风险越限、成交后保护缺失、停机失效或恢复任务丢失；P2 表示统计、编辑器、验证、升级或安全防护正确性问题。交易所、网格、多交易器等条件触发问题会注明适用范围。未将任何问题标为无条件的全项目 P0。

本次只新增审查报告和隔离证据。未修改业务代码、真实账户配置或策略，未发送交易订单、重启服务或执行线上数据库迁移。本文发现能够说明风险路径，不能单凭代码证明用户先前 177 笔 LONG 的 -8.39 盈亏分别由哪些缺陷造成；这需要逐笔订单、成交、费用、保护单及日志归因。

## 审查覆盖与验证边界

建立了 API、认证、加密、kernel、manager、market、MCP、security、store、Telegram、交易器/适配器、wallet、cmd、web/src 的文件清单，共 492 个文件，其中 146 个测试文件。采用全模块结构扫描、关键调用链深入检查及风险场景复现；不表示已经逐行形式化验证全部文件。

| 功能范围 | 核查重点与结果 |
|---|---|
| 登录、注册、密码、注销、钱包 | JWT 吊销、用户作用域、首次用户/注册控制、请求限制、钱包私钥禁入；现有安全回归测试通过 |
| 交易器、模型、交易所、策略 API | 权限归属、配置脱敏、乐观锁、异步重载、开始/停止；重载期间停止意图仍有缺口 R12 |
| 候选池与行情 | 缓存时效、交易场所一致性、OI 豁免、池外标的与方向禁开码；上次核心修复未发现原路径回归 |
| 信号、Prompt、AI 请求 | 多空信号、硬门、健康度口径、预算压缩、流式超时与安全 HTTP；R06、R19；方向健康度另见后文 |
| 市价/限价与保护订单 | 风险金额、部分成交、去重、改单、SL/TP 核验、撤单、离线恢复；R01–R06、R11 |
| 网格策略 | 原生适配器要求、成交账本、停机/暂停、紧急退出、日损失与仓位限额；R07–R10 |
| 仓位同步、统计、复盘 | 净盈亏、AI/手动归属、连亏熔断、PostgreSQL 增量 schema；R13、R14、R17 |
| 回测与调参 | 时间切分、标签 purge、成本、参数验证与应用；原失败场景已修，但“新参数已验证”的结论仍不足，R18 |
| 策略编辑器、市场、看板、设置 | 草稿/选中状态、语言切换、保存版本、数据恢复轮询、手机风险字段；R15、R16 |
| Telegram 与外部数据 | 绑定用户、写操作确认、路由白名单、请求限制；现有确认/授权测试通过；安全 HTTP 复用见 R19 |
| 存储、部署、运维工具 | SQLite WAL/连接池、持久化恢复、退出顺序、镜像上下文、构建与检查；R06、R17、R20 |

不连接真实交易所验证订单最终性；不执行真实账户多交易器并发下单；无 PostgreSQL 实例升级实验；不运行远程攻击或生产部署。静态确认项和可复现项在各发现中分别注明。

## 验证结果

| 检查 | 结果 | 证据 |
|---|---|---|
| `go test ./...` | 通过；部分包复用 Go 测试缓存 | [后端日志](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/evidence/go-test.txt) |
| `go vet ./...` | 通过 | [静态检查日志](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/evidence/go-vet.txt) |
| `go test -race ./trader ./store ./api ./kernel` | 通过；有 macOS 链接器警告，无已检测到的竞态 | [竞态日志](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/evidence/go-race.txt) |
| 前端现有测试 | 6 个文件、110 项通过 | [前端测试](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/evidence/web-test.txt) |
| 前端生产构建 | 通过；主 JS 2,188.82 kB，gzip 628.68 kB，存在大 chunk 警告 | [构建日志](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/evidence/web-build.txt) |
| 前端 lint | 未通过：26,741 errors，其中 26,728 被标为可自动修复；多数为格式规则，不能解读为 26,741 个业务缺陷 | [lint 日志摘要](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/evidence/web-lint.txt) |
| 新增 Go 安全契约测试 | 14 个场景全部失败，直接复现缺口；不是常规测试套件失败 | [复现日志](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/evidence/regression-results.txt) |
| 新增策略编辑器组件测试 | 2 个场景全部失败，直接复现覆盖行为 | [页面复现日志](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-03/evidence/web-regression-results.txt) |

重跑证据：从仓库根目录运行 `python3 docs/architecture/review-2026-10-03/evidence/reproduce.py` 和 `python3 docs/architecture/review-2026-10-03/evidence/reproduce_web.py`。前者通过 Go overlay 注入测试，后者暂时放入组件测试并在 finally 删除；均不调用真实交易接口。当前版本预期非零退出，修复后应转为通过。Go 构建缓存及标准测试的本地模拟端口可能需要环境权限。

常规竞态测试通过不能否定 R06 的逻辑互斥缺口：现有测试没有强制覆盖“监控读到 false 后，主循环立即启动”的交错。

## P1：先处理执行保护、停机与风险额度

### R01 · 止损替换成功后，撤单操作会把新止损一并撤掉

位置：[moveStopExchange](/Users/zhangyun/workspace/nofx/trader/auto_trader_vol.go:89)、[Binance 按方向枚举撤止损](/Users/zhangyun/workspace/nofx/trader/binance/futures_orders.go:320)。上次 F02 的修复引入了新的成功路径问题。

函数先 `SetStopLoss`，然后按 symbol/side 调用“撤销所有止损”。没有保存旧订单 ID，也不排除刚创建的订单。当新单在撤单查询中可见时，新旧止损全部取消，函数仍返回 nil。保本、移动止损、AI 调整会共用此路径，本地随后可能记录新 SL，但交易所已经没有这个保护。

Binance 的 `-4130` 自恢复会先撤旧单并重新挂新单，再被外层撤掉；若重挂失败，其内部也没有恢复旧单：[重挂失败路径](/Users/zhangyun/workspace/nofx/trader/binance/futures_orders.go:887)。所以“下单失败时旧止损保持不变”的注释也不涵盖真实适配器行为。

**证据**：`TestReview03StopUpdateMustRetainNewProtection`：返回成功，旧止损和新止损均不存在。测试验证通用调用链；Binance 枚举撤单及自恢复为静态确认。

**修复要求**：返回并记录订单 ID，使用原生修改或只撤原订单 ID；新保护核验后才提交本地状态。不支持新旧并存的适配器需有恢复原保护的补偿步骤及再次核验。

### R02 · 保护单核验失败只告警，仍推进“已保护”水位

位置：[返回值与核验](/Users/zhangyun/workspace/nofx/trader/auto_trader_orders.go:722)、[核验仅告警](/Users/zhangyun/workspace/nofx/trader/auto_trader_orders.go:781)、[水位推进](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:701)。上次 F01 只修复了下单接口报错的情况。

`placeProtectiveOrders` 的两个 error 仅来自 placement；`verifyProtectiveLegs` 没有返回核验结果。交易所下单返回成功但查询不到保护单，甚至核验查询持续失败时，函数仍返回两个 nil，`ProtectedQty` 更新为全部成交量，FILLED 分支随后允许删除恢复计划。

**证据**：`TestReview03VerificationFailureMustKeepWatermark`：模拟成功回执但交易所挂单为空，核验打印失败告警，水位仍为 1。

**修复要求**：将核验成功、缺失、未知纳入返回结果及持久化状态；缺失/未知不得推进确认水位。确认内容应包括订单 ID、方向、类型、实际触发价和覆盖量。

### R03 · 保护去重只看价格，多次部分成交后覆盖数量不足

位置：[价格去重](/Users/zhangyun/workspace/nofx/trader/auto_trader_orders.go:733)、[按增量挂保护](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:693)。适用于按数量保护的适配器及分批 TP。

第一次成交 0.4 后挂 0.4 的 SL；累计成交变成 0.8，第二次要为增量挂单时，旧 SL 价格相同就被判为“已存在”，完全不核对数量。SL/TP 均跳过后，水位却变为 0.8。watchdog 的“有此类型订单”检查也无法发现数量差额。

Binance 的 closePosition SL 覆盖整仓，不能用此测试宣称其 SL 必然少覆盖；但数量化分批 TP 及 Bybit/OKX 等数量型保护存在此问题。

**证据**：`TestReview03AdditionalPartialFillMustIncreaseCoverage`：实际 SL 覆盖 0.4，水位 0.8。

**修复要求**：以累计成交及当前剩余仓位为依据，对各腿做覆盖量对账。区分 closePosition 与 qty-sized；幂等键不能只有价格，需明确该订单覆盖哪些成交量。

### R04 · 撤单/过期后的部分成交，保护失败或终态未知仍删除恢复任务

位置：[终态分支](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:488)、[主动撤单后的删除](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:607)、[离线残量处理](/Users/zhangyun/workspace/nofx/trader/auto_trader_reconcile.go:140)。上次 F06/F07 未完整覆盖。

CANCELED/EXPIRED/REJECTED 分支调用保护函数后无条件 drop。主动撤单成功后的最终成交查询如果失败，也直接 drop；如果查询成功但 SL 拒绝，结果相同。离线恢复在 `protectOfflineResidue` 后也不看保护成功与否就删除持久化记录。通知仍可能写“未成交”。

**证据**：`TestReview03CanceledPartialFailureMustKeepPlan`、`TestReview03CancelUnknownFinalFillMustKeepPlan`。一个已成交 0.4 且 SL 失败的计划、一个最终成交量未知的计划均被删除；离线路径为静态确认。

**修复要求**：入场订单终态与残余保护任务分开存储。只有确定成交量、价格及对应保护成功，或确认没有任何成交后才能清理任务。未知终态保留重试，不推断为未成交。

### R05 · Hyperliquid 把查询失败/订单消失猜成 FILLED，零成交回执仍被清理

位置：[适配器](/Users/zhangyun/workspace/nofx/trader/hyperliquid/trader_account.go:365)、[零数量绕过完整性检查](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:478)。适用于 Hyperliquid。

`GetOrderStatus` 只查 open orders。接口失败或订单不在列表时，返回 `FILLED`、`avgPrice=0`、`executedQty=0`，且 error 为 nil。订单不在列表不能区分成交、撤销、查询范围不全。现项目支持限价 GTC，代码中的 IOC 假设并不覆盖这些订单。

保护函数遇零数量/零均价直接退出；FILLED 检查却只有 `executed>0` 才验证水位。计划被删除并通知“保护单已挂”，没有实际创建 SL/TP。网格账本还会把此结果当全部成交。

**证据**：`TestReview03FilledWithoutReceiptMustKeepPlan` 使用适配器相同的零回执，保护调用为 0 次但计划被删除；真实适配器返回值静态确认。

**修复要求**：查询真实订单历史状态及成交回执，错误保留 unknown/error；正确覆盖产品/账户查询范围。FILLED 必须有有效成交凭证，或者通过持仓/成交对账确认结果，不接受猜测式终态。

### R06 · 慢 AI 期间保护监控停摆；停止时先退出监控，再等 AI 才撤挂单

位置：[监控 gate](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:91)、[整个 cycle 标为 active](/Users/zhangyun/workspace/nofx/trader/auto_trader.go:601)、[Stop 顺序](/Users/zhangyun/workspace/nofx/trader/auto_trader.go:647)、[进程退出超时](/Users/zhangyun/workspace/nofx/main.go:193)。上次 F06 的 30 秒监控并未真正与 AI 解耦。

主循环在整个 runCycle/RunGridCycle，包括外部 AI 请求期间都保持 `cycleActive=true`。保护 ticker 因此跳过，限价单在等待 AI 的几分钟内成交，仍可能没有及时保护。

`Load()` 检查也不是互斥锁：监控刚读到 false，主循环就可 `Store(true)` 并开始运行，两者同时处理共享 pending 指针/保护状态。现有 race 测试未复现此交错；这项是静态并发正确性发现，不是宣称已检测到数据竞态。

停止时关闭监控通道，再阻塞等待 `runDone`，最后才撤 pending。等待中的 GTC 可继续成交。进程只等 10 秒就退出，慢 AI 场景下撤单步骤可能尚未执行。持久化计划能帮助下次启动恢复，但不保证停机期间的保护。

**修复要求**：订单与保护服务独立于 AI 调用；短状态事务用真正的 mutex/CAS 所有权控制，外部 AI 等待不占保护执行权。停止应立即阻断新增入场、尽快撤单并确认最终成交，保护/对账服务留到任务完成后退出；AI 请求应可取消。

### R07 · 网格停止和日亏损暂停会留下入场挂单，紧急退出还可能假报成功

位置：[Stop 只处理普通 pending](/Users/zhangyun/workspace/nofx/trader/auto_trader.go:668)、[日亏损直接 pause](/Users/zhangyun/workspace/nofx/trader/auto_trader_grid.go:440)、[pause 忽略撤单错误](/Users/zhangyun/workspace/nofx/trader/auto_trader_grid_orders.go:234)、[emergencyExit](/Users/zhangyun/workspace/nofx/trader/auto_trader_grid.go:285)。适用于网格。

网格订单保存在 GridState.OrderBook/Levels，普通 Stop 的 pending 清理不访问它们。日亏损触发仅设 IsPaused，没有撤销已有入场单；暂停后循环在同步成交账本前返回，存量订单仍可增加仓位。手动 pause 忽略撤单错误，emergencyExit 忽略或仅记录撤单/平仓错误，最终返回 nil。

**证据**：`TestReview03StopGridMustCancelRestingEntries`：有 resting grid entry 的交易器 Stop 后没有撤单调用。`TestReview03PauseGridMustReportCancelFailure`：撤单拒绝仍返回成功。日亏损与紧急退出分支静态确认。

**修复要求**：统一网格停止/风控暂停协议，撤销入场并核验；部分成交保留账本与退出/保护任务。失败返回具体剩余订单和仓位，不能把“已设置本地暂停”当作交易所已清空。

### R08 · 网格账本丢弃“部分成交后撤单”的实际仓位，还保留猜测成交路径

位置：[二值订单分类](/Users/zhangyun/workspace/nofx/trader/auto_trader_grid_orders.go:61)、[同步状态](/Users/zhangyun/workspace/nofx/trader/auto_trader_grid_orders.go:324)。上次 F13 修复了全成交/全撤销同轮推断，但未覆盖部分成交。

仍在 open orders 中的部分成交订单直接 continue，不进入格点仓位；消失后若状态 CANCELED，就设 empty/清零，不读取 `executedQty`。FILLED 则直接用计划 quantity/price。当订单状态未知、持仓接口可读时，仍通过账户总仓位差值猜测某订单是否全成交，无法区分多个订单、方向及其他交易。

**证据**：`TestReview03GridPartialCancelMustRetainExposure`：CANCELED、实际成交 0.4，格点变 empty、PositionSize=0。

**修复要求**：用逐订单累计成交量、真实均价、成交增量和最终余量更新格点；订单取消不清除已成交仓位。无法取得订单凭证时保持 unknown 并对账，不能继续用总仓位给具体订单补造全成交。

### R09 · 网格日亏损基线和峰值仍只在内存，同一天重载会解除限制

位置：[网格日初资产](/Users/zhangyun/workspace/nofx/trader/auto_trader_grid.go:247)、[状态字段](/Users/zhangyun/workspace/nofx/trader/auto_trader_grid.go:40)。上次普通策略 F08 已修，网格未复用持久化基线。

余额口径已改为正确的 totalEquity，但 DayStartDay/DayStartEquity/PeakEquity 仍只在 GridState 内。新建/重载网格后，以当前亏损后的资产重新作为日初资产，损失立即归零；峰值回撤基准也重建。

**证据**：`TestReview03GridDailyBaselineMustSurviveReload`：1000→900 得到 10%；同日创建新 GridState 后得到 0%。

**修复要求**：以实际账户、策略和交易日持久化基线及熔断状态；重载不重置。统一普通策略与网格的资金流修正、日期边界和人工重置语义。

### R10 · 网格仓位查询失败被当成零仓位，仍允许新增风险

位置：[checkTotalPositionLimit](/Users/zhangyun/workspace/nofx/trader/auto_trader_grid_orders.go:27)。适用于网格。

`GetPositions` 失败时 currentPositionValue 保持 0，函数继续只用本地 pending 和候选金额判断允许。某次下单前的仓位查询失败即可低估实际风险。成功查询时同 symbol 的多条持仓还采用覆盖赋值，未累加双向 gross exposure。

**证据**：`TestReview03GridUnknownPositionMustBlockNewRisk`：仓位 timeout，新增 10 的风险检查仍通过。双向覆盖赋值为静态确认。

**修复要求**：风险快照未知时阻断增加风险，返回可辨认错误；成功时按所有相关持仓累计 gross notional，并对本地账本/挂单避免重复计数。

### R11 · 同交易账户多个交易器的挂单风险不共享，账户上限可被突破

位置：[单实例 pending 风险](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:2013)、[单实例周期额度](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:2047)、[保证金预留](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:1042)。上次 F11 仍存在；条件为多个交易器引用同一实际账户。

风险和保证金预留都在 AutoTrader 私有内存。另一交易器的 resting entry 未成交前不在持仓快照，也不在当前实例 pending 集合中。两个交易器各自通过检查后可合计超过账户限额；HTTP 配置锁不能串行化运行中的下单。

**证据**：`TestReview03AccountCapMustIncludeOtherTraderPendingRisk`：同账户权益 1000、账户 cap 30，实例 A pending 风险 20；实例 B 的风险 20 仍被准入，合计 40。

**修复要求**：以真实账户身份建立共享且原子化的风险/保证金预留，覆盖全部交易器、订单和未确认成交。明确同 symbol/side 合并持仓的所有权及保护订单管理规则。

### R12 · 异步重载期间停止接口找不到交易器，恢复意图又来自旧快照

位置：[先从 manager 删除再等待 Stop](/Users/zhangyun/workspace/nofx/manager/trader_manager.go:435)、[Stop 依赖内存实例](/Users/zhangyun/workspace/nofx/api/handler_trader.go:899)、[旧 resume 写回](/Users/zhangyun/workspace/nofx/api/trader_reload.go:12)、[后台重载](/Users/zhangyun/workspace/nofx/api/strategy.go:404)。新异步保存流程的静态发现。

保存成功后后台把交易器从 manager 删除，再等待旧 AI cycle 退出。这个窗口可持续数分钟，用户点击停止时，API 因内存实例不存在返回 404，未写入“期望停止”。重载结束后仍按开始时读取的 resume=true 启动替换实例。用户在配置保存之后无法可靠地阻止自动恢复。

此外，`loadMu` 保护 remove/load，不保护提前读取的 resume 和所有 API 状态写入；旧回调覆盖较新的停止状态存在交错可能。未做生产并发实验，不宣称每次停止都会被覆盖。

**修复要求**：持久化 desiredRunning 与单调版本号，停止请求即使实例正在重建也能落库。重载结束按最新意图/CAS 决定是否启动；保留 reloading/stopping 状态及可取消任务，不在等待期间把控制对象表示为不存在。

## P2：统计、前端、升级、调参与安全防护

### R13 · 连亏熔断仍用毛盈亏，扣费后连续亏损会被当成盈利

位置：[lossStreakState](/Users/zhangyun/workspace/nofx/trader/auto_trader_lossstreak.go:62)。统计界面已改净盈亏，这个风控消费端没有统一。

判断条件仍是 RealizedPnL<0，不扣 Fee。比如每笔毛盈亏 +0.01、手续费 0.1，三笔实际净亏损却在第一笔就重置 streak，允许继续开仓。

**证据**：`TestReview03NetLossesMustTriggerLossStreak`。

**修复要求**：统一净盈亏函数与费用符号，用一致口径判断赢/亏/保本、显示、策略健康度及熔断；明确尚未入账资金费用的处理方式。

### R14 · AI 归属在成交同步与清理登记之间丢失，连亏记录被排除

位置：[开仓归属 stamp](/Users/zhangyun/workspace/nofx/store/position_builder.go:65)、[成交后才 mark](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending.go:678)、[close-time recheck](/Users/zhangyun/workspace/nofx/store/position_builder.go:189)、[平仓清理](/Users/zhangyun/workspace/nofx/trader/auto_trader_orders.go:900)。

PositionBuilder 的注释假定下单前登记 AI 来源，但实际路径是成交回执后才 mark。交易所同步线程先创建 OPEN 行时 AIManaged=false；若交易器随后平仓/检测消失并 unmark，关闭成交同步时也查不到 mark，这笔 AI 亏损永久保留为手动。连亏熔断明确过滤 AIManaged=false，复盘又按决策时间窗口归属，两个消费端可能不一致。

**证据**：`TestReview03OwnershipMustSurviveCloseBeforeSync` 在临时 SQLite 按“开仓同步→mark→unmark→关闭成交同步”执行，最终 CLOSED 行仍为 AIManaged=false。

**修复要求**：持久化 AI 入场订单的来源，以具体 order/fill ID stamp 仓位；登记清除前固化归属。不要简单把未成交挂单的 symbol/side 标为 AI，从而误认同币手动成交。

### R15 · 切换界面语言仍覆盖已保存的自定义 Prompt

位置：[语言 effect](/Users/zhangyun/workspace/nofx/web/src/pages/StrategyStudioPage.tsx:200)、[替换 prompt_sections](/Users/zhangyun/workspace/nofx/web/src/pages/StrategyStudioPage.tsx:223)。上次 F16 只保护“当前有未保存修改”的草稿。

刚加载的自定义策略是 clean 状态，因此切换界面语言仍从默认模板覆盖所有 prompt_sections 并设 dirty。用户下一次保存就把自定义交易规则换成默认规则。请求发出后如果用户开始编辑，返回结果也未再次校验 dirty/selected ID。

**证据**：真实 StrategyStudioPage 的 jsdom 组件测试中，已保存的 `USER_SAVED_CUSTOM` 变成 `ENGLISH_DEFAULT`。没有真实后端策略写入。

**修复要求**：UI language 与策略 Prompt language 分离；只有显式替换模板才覆盖自定义内容。异步回填必须校验策略 ID、请求版本和最新草稿状态。

### R16 · 保存 A 后的延迟刷新会把已选中的 B 切回 A

位置：[单策略刷新回填](/Users/zhangyun/workspace/nofx/web/src/pages/StrategyStudioPage.tsx:148)、[保存后发起后台刷新](/Users/zhangyun/workspace/nofx/web/src/pages/StrategyStudioPage.tsx:524)。

后台刷新只检查 dirty，不检查当前 selectedStrategy.id 是否仍为请求的 strategyId。保存 A 后迅速选择 B（B 的 dirty=false），A 的刷新回执会把 B 的对象与 fresh A 合并，并把编辑配置改回 A。这个结果与注释宣称“不改变选择”相反。

如果用户保存后立即继续编辑，刷新又会因为 dirty 跳过 updated_at 更新，下一次保存可能带旧版本而发生 409；当前冲突分支强制回填会丢草稿。这项为同函数的静态补充，不把它当作页面测试已验证的场景。

**证据**：第二个真实页面组件测试：保存 Changed A→选择 Strategy B→释放 A 的延迟列表回执，B 不再是选中编辑对象。

**修复要求**：草稿按 ID 管理，回填只更新对应策略；当前选择必须匹配请求 ID。成功保存直接接收完整服务端对象及新版本号，把草稿更新和版本更新分开处理；冲突保留本地草稿供比较。

### R17 · 已存在 PostgreSQL journal 表时跳过增量字段迁移

位置：[journal 初始化](/Users/zhangyun/workspace/nofx/store/journal.go:75)。适用于从旧 PostgreSQL schema 升级。

只要 information_schema 发现 trade_journal 已存在，就直接 return nil，既不 AutoMigrate 也不调用 ensureColumns。较旧表没有 ai_managed 等新字段时，当前 GORM 插入/按新字段处理会失败。SQLite 与全新 PostgreSQL 建表测试不能覆盖此路径。

**证据类型**：静态检查；未运行真实 PostgreSQL 升级。

**修复要求**：所有支持的数据库都执行增量迁移；显式版本化迁移及字段/索引核验。加入“旧表→当前 schema→同步 journal/回填归属”的升级测试。

### R18 · 调参中心变化仍没有重放新参数，却标为 holdout-verified

位置：[中心漂移](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:516)。上次 F15 的负 holdout 漂移、标签跨边界和低阈值样本截断已修；此项是剩余验证缺口。

当前条件仅检查历史 testOverall>0，然后依据训练集盈利样本中位数改变 price_atr_center/vol_center，保存并记录 holdout-verified。testOverall 是已有信号/旧评分的平均 forward return，没有用 next 参数重算验证集评分、入场集合或收益。旧参数有效并不能验证新中心有效，修改中心后原来验证过的 cutoff 也不能直接继承验证结论。

**证据类型**：静态数据流；未将此描述成旧负 holdout 回归测试再次失败。

**修复要求**：完整参数组在独立保留集重放评分和实际交易行为，比较净收益、回撤、样本量及稳定性后才应用；不能重放的参数变化应清楚标为未验证，先 shadow，不输出已验证标签。

### R19 · 流式 AI 请求丢失安全重定向钩子，DNS 校验也未绑定实际拨号 IP

位置：[流式 client 重建](/Users/zhangyun/workspace/nofx/mcp/client.go:914)、[安全 client 重定向校验](/Users/zhangyun/workspace/nofx/security/url_validator.go:207)、[DNS 校验后再次按 hostname 拨号](/Users/zhangyun/workspace/nofx/security/url_validator.go:188)。安全防护缺口，条件触发。

流式调用复制了 Transport、设置 Timeout，遗漏原 client 的 CheckRedirect。正常直连中的私有 IP 拨号校验还在，不能据此断言所有直连都能访问内网；但配置 HTTP 代理时，Transport 只拨代理并豁免其地址检查，目的地重定向的 URL 校验因此丢失，外部可配置服务的响应能把请求导向代理可达的内部地址。

同时，LookupIP 验证后仍把 hostname 交给 DialContext 再解析，而没有拨已验证的 IP，存在检查与使用分离的 DNS rebinding 窗口。

**证据类型**：静态请求路径和配置分支；未进行远程攻击验证。

**修复要求**：从原 HTTP client 复制完整策略，只调整超时/请求 context；所有重定向继续校验。拨已验证 IP 并保留正确 TLS 主机名；代理目的地需保留 URL 校验及可信的出站访问限制。

### R20 · Docker 构建上下文未排除 .env、密钥和运行数据库

位置：[.dockerignore](/Users/zhangyun/workspace/nofx/.dockerignore:44)、[后端 COPY 全上下文](/Users/zhangyun/workspace/nofx/docker/Dockerfile.backend:49)。适用于源码目录含运行配置/数据、尤其远程构建或共享缓存。

仅排除 config.json、日志及少量构建产物，未排除 .env/.env.*、密钥目录、运行 data/ 与数据库文件。后端 COPY . . 会把它们送入构建层及可能的构建缓存。最终镜像只复制二进制/库，本次不声称最终运行镜像必然包含这些原文件。

**证据类型**：静态 Docker 上下文规则；未上传任何文件至远程 builder，也未查看或公开真实密钥内容。

**修复要求**：排除运行配置、密钥和数据库，并明确保留需要的示例配置；优先显式 COPY 构建需要的源码。对上下文做路径级检查，验证 runtime data 不进入镜像构建层。

## 上次发现的复验状态

| 2026-10-01 发现 | 本次状态 |
|---|---|
| F01 保护失败推进状态 | 下单报错回传、TP runner 后置写入等已修；核验失败与数量覆盖仍有 R02/R03，终态删除见 R04 |
| F02 先撤旧再挂新 | 原 mock 拒绝场景通过；成功路径出现 R01，真实 Binance 内部替换失败亦无补偿 |
| F03/F04/F05 禁开码、池外准入、限价复盘规则 | 已有统一执行门与回归测试，本次未复现原缺口 |
| F06/F07 成交监控、停止及离线恢复 | 有改进，剩余 R04/R05/R06；网格停止另见 R07 |
| F08 普通策略日亏损重载 | 持久化基线测试通过；网格例外 R09 |
| F09 公开配置凭据泄露 | 脱敏回归通过；构建上下文是不同风险 R20 |
| F10 交易场所行情 | 代码按 venue 取数的修复存在，本次未发现原 Binance 硬编码取数路径回归；真实 venue 一致性未实盘验证 |
| F11 同账户风险预留 | 仍存在，新增隔离场景证实 R11 |
| F12 不支持网格的伪入场适配器 | 原生适配器强制要求的回归通过 |
| F13 网格账本 | 全成交/全撤销及持仓查询失败推断有所修复；部分成交和未知终态仍有 R08 |
| F14 网格权益口径 | totalEquity 双计问题回归通过；持久化及查询失败仍有 R09/R10 |
| F15 回测调参 | 负 holdout 中心变化、标签 purge、固定低分采集有修复；新参数组验证不足 R18 |
| F16 编辑器 | 模型切换触发策略重拉、新建策略客户端版本等已修；saved custom 语言覆盖 R15、异步选中回填 R16 仍存在 |
| F17/F18 数据过期与手机风险字段 | 持续退避轮询、stale/unknown、移动端风险信息已加入；无本次故障注入浏览器端长时间运行实验 |
| F19/F20 OI 豁免与候选池时效 | snapshot 遍历、下游 near_high 豁免、过期控制存在；本次未复现原缺口 |
| F21 成交后的 RR 提示 | 已使用真实 avgPrice；它是告警，不等于成交后持续复核完整准入/结构 |

这里的“已修”指本次读取的实现和现有测试支持该结论，不能把它扩大成所有交易所/所有网络故障均已验证。

## 与 LONG 高频亏损有关的剩余设计问题

方向净统计已加入，但 AI 的 StrategyHealthEdge 仍来自整个 trader 的 rolling ProfitFactor：[取统计](/Users/zhangyun/workspace/nofx/trader/auto_trader_loop.go:900)、[健康度](/Users/zhangyun/workspace/nofx/kernel/engine_prompt.go:1087)。其 store 查询按 trader/status 过滤，未按方向或 AIManaged 过滤：[统计范围](/Users/zhangyun/workspace/nofx/store/position_query.go:90)。

因此，SHORT 的利润或手动成交可以掩盖 LONG 的负期望。整体账户统计作为看板数据是合理的，但不能据此认为亏损的 LONG 子策略健康。建议保留账户统计，并另提供按 AI 来源、方向、策略版本划分的健康度及最低样本门槛，明确是否需要方向独立的限制。此处是产品/策略设计缺口，不把“所有 aggregate 统计”判作计算 bug，也不凭 177 笔汇总数据自动调参或停止实盘。

R01–R06 会使执行结果偏离已设计的退出规则；R13/R14 会削弱连亏限制。应先修这些可验证的一致性问题，再用真实订单级净 R、费用、持有时间、市场状态、重复入场间隔等做归因。仅增加指标或收紧 Prompt，无法弥补成交保护和熔断口径的不一致。

## 修复顺序与验收

1. **保护闭环**：R01–R05。改单后旧/新订单及实际覆盖量可核验；多次部分成交、API 成功但订单不可见、取消后残量、终态查询失败均保留可恢复任务。14 个 Go 证据测试中对应项目应转绿。
2. **运行与账户边界**：R06–R12。慢 AI 不阻挡保护；停止/风控暂停不遗留无人管理的入场单；网格成交增量完整；基线持久化；多个实例共享账户预留；重载期间仍能接受停止意图。补充确定的并发交错及断电恢复测试。
3. **归属、统计、编辑器**：R13–R16。AI 成交归属可追溯、连亏使用净盈亏；语言和异步刷新不改写保存的配置/当前选择；两个页面证据测试应转绿，保存返回新版时间戳且冲突保留草稿。
4. **升级、验证、外部边界**：R17–R20。补 PostgreSQL 旧库迁移、完整参数保留集重放、安全客户端重定向及 DNS/代理用例；构建上下文排除 runtime secrets/data。
5. **工程检查**：清理 lint 基线并设置可执行门槛，按路由/功能拆分前端大 chunk。这两项单列为工程债务，不把格式错误包装为交易功能缺陷。

修复过程应复用本报告的具体触发场景；尤其不能只验证“下单接口报错”或“订单一次全成交”。成功回执、真实订单可见性、部分成交增量和最终恢复任务状态都需要验收。
