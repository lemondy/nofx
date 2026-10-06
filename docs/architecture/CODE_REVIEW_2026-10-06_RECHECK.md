# 第五轮代码审查报告独立复核 · 2026-10-06

复核对象为 `113bbb39` 提交的 `CODE_REVIEW_2026-10-06.md`，业务代码基线为 `abba5978`。原报告保留为历史记录；本文给出独立裁决及可重跑证据。本次为单线复核，未沿用上一轮 agent 的结论作为证据。

**结论：空头数量符号回归、扫描器父 goroutine 恢复后卡死、两类 worker panic 越过外层 recover，均已复现。报告整体不能裁定为“全部验证无误”：存在错误的影响范围、错误的 BTC 误拦/误放方向、漏读的既有 API 防护，以及错误的“并发合流”验证结论。原 P2 ×11 混入了部署事项、工作区事项和测试建议，不宜继续作为 11 个已证实代码缺陷计数。**

## 1. 部署阻断与扫描器缺陷

### 1.1 原 P0-1：缺陷成立，影响必须写准

[watchdog](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:1607) 在 `isAIManaged` 通过后，直接以 `qty <= 0` 判定数量不可用。[Binance 持仓接口](/Users/zhangyun/workspace/nofx/trader/binance/futures_positions.go:43) 保留负数量空头；该仓位因此进入 `continue`，跳过止损读取、补挂、越过止损时的紧急退出和连续失败退出预算。

独立镜像用例给出以下结果（每个场景执行两轮 watchdog；保护挂单均已足量有效）：

| 输入 | 账户故障 | 读取并记录 SL | 结果 |
| --- | --- | --- | --- |
| long，qty=+1 | 无 | 95 | 正常 |
| short，qty=-1 | quantity unavailable，逐轮复立 | 0 | 缺陷复现 |
| short，qty=+1 | 无 | 105 | 正常 |

同账户注册 peer 的 `entryExecutionBlocked`、`pendingAccountHaltReason`、`applyHardRiskGates` 均被负数量空头故障阻断，已独立测试。

“部署前必须修”成立。需要改写原报告的两处概括：

- 触发条件是 **AI 管理的负数量非零空头仓位**，典型为 Binance；不能扩大为所有适配器的任何空头。只有做空策略配置、尚无此类持仓时，不会“启动即自锁”。
- 故障阻断新增风险并可触发存量入场挂单撤销，不等于所有账户操作停摆。已有交易所保护单不会因此失效；平仓/减仓入口也没有被该故障统一封锁。但这个 watchdog 对受影响空头的保护恢复和紧急退出会被跳过，风险真实存在。

原 P0 编号在本文仅用于对照；保留其项目内“部署阻断”优先级。输入处 `math.Abs(qty)` 是正确修复方向，必须补负数量空头的足额保护、缺保护补挂、越过止损退出及账户 peer 场景测试。F05、F08 的完整空头行为同样依赖此修复，不能在当前基线上无条件宣称合格。

### 1.2 原 P2-11：两项正确性缺陷成立；告警属于运维增强

[runOnce 的 recover](/Users/zhangyun/workspace/nofx/market/breakout/scheduler.go:273) 只记录日志。用内存 HTTP transport 在父 goroutine 注入 panic 后，观测到 `scanning=true`、`onDone=false`；随后的 `RefreshNow` 等到超时，未再发起扫描。

[AnalyzeMany worker](/Users/zhangyun/workspace/nofx/market/breakout/breakout.go:343) 和 [ScanShorts worker](/Users/zhangyun/workspace/nofx/market/breakout/shortscan.go:786) 都没有自己的 recover。分别在测试子进程内注入 worker panic，两个子进程均以 **exit status 2** 终止；外层 recover 未执行。AnalyzeMany 场景经过实际的 `Scheduler.runOnce` 调用路径。

原报告影响面过大：[piggy_dash](/Users/zhangyun/workspace/nofx/kernel/engine.go:1051) 依赖 Scheduler 快照，并在快照超过 12 分钟且刷新失败后停用该来源；[交易候选 short_scan](/Users/zhangyun/workspace/nofx/kernel/engine.go:1140) 则直接调用 `ScanShorts`，使用独立缓存和 inflight 状态，未检查 Scheduler.scanning。因此父 goroutine 被 recover 后，调度器双榜会停止继续更新，**不能据此推出所有扫描来源候选都冻结**。若 panic 出现在 worker 内，则是整个进程终止，影响更大。

建议将状态清理集中到统一 defer，持 `s.mu` 复位 `scanning`，确保回调最多调用一次；两个 worker 都需自己的 recover，并保留失败符号和堆栈。额外 Telegram 告警可提高可见性，但现有 logger 已告警，不能把“没有 Telegram”当作第三个已经证明的正确性缺陷。

原摘要“数量归一化 + 镜像测试 + scanning 复位即可部署”遗漏了同一报告已发现的 worker 进程终止风险，不能用作部署放行结论。

## 2. 原 P2 ×11 逐项裁决

| 原编号 | 独立裁决 | 应修正的内容 |
| --- | --- | --- |
| P2-1 C2 残留 | 成立 | bb_ride 与 pump_guard 的 pivot 分支仍直接丢末根。bb_ride 用例已证明：最后一根已闭合 doji 被漏读时 ride=true，读入同一 doji 后 ride=false。公共 bbRideWalk 同时影响多、空两侧。 |
| P2-2 BTC unknown 语义分歧 | 成立 | kernel 对 unknown 不生成 BTC 禁开码，pending 在任一 BTC 过滤开关启用且不足 60 根时直接拒绝。市价路径没有这条独立 unknown 检查。统一政策需要明确多/空侧意图。 |
| P2-3 公开实时扫描 | 事实部分成立，风险论证不成立 | 路由公开属实；“循环请求都触发重量级扫描、需加节流”漏读了现有缓存、扫描锁和频控。建议移至公开 API 策略/容量事项，不据此认定缺少防护的 P2。 |
| P2-4 sizing 成本标注 | 部署事项；“未标注”不准确 | `QUANT_FIXES_2026-10-05.md` §2 已写明默认 20bps、新公式与行为变化。字段 diff 能显示显式配置修改，代码默认语义变更仍需要代码版本记录。 |
| P2-5 闭合/实时窗口 | 成立，但例子的方向反了 | BTC 实时上涨时会误放中间强度币；BTC 实时回落时会误拦。见下方数值复现。 |
| P2-6 BTC 失败静默窗 | 有条件成立，并存在漏审 | 30 秒返回 nil 的前提是 cache 为空，不是所有请求失败场景；首次成功请求仍在进行时也会让并发调用立即返回 nil。暖缓存过期后没有请求合流，详见 §3。 |
| P2-7 逐仓读挂单 | 调用放大成立；优先级应按实测预算评估 | refreshExecutionAccount 每仓调用 GetOpenOrders，Binance 每次有普通单和 Algo 单两次查询；5 仓对应该处约 10 次请求。可先按唯一 symbol 去重；全 symbol 查询需增加适配器能力，不能假设现有接口都支持。 |
| P2-8 72 魔数 | 不支持作为新 P2 | 72 = 100 × 0.9 × 0.8，来源已在 validateOpenRisk 注释和实现中说明。actualFillRisk 沿用相同近似。局部补引用/抽常量和改进强平模型属于建议。 |
| P2-9 stash | 工作区事项 | stash 存在且含模板透传改动，但缺少预期功能和用户意图证据，不能由此认定当前代码存在 P2 缺陷；本次不恢复或丢弃 stash。 |
| P2-10 auth 护栏 | 测试增强建议；附带叙述错误 | 缺统一路由白名单断言可以补；已有认证/越权测试不能被忽略。reset-password 当前直接返回 410，根本没有原文暗示的验证码/令牌恢复流程。 |
| P2-11 panic | 两项正确性缺陷成立 | 缩小父 goroutine 卡死的影响范围，保留 worker 进程终止风险；Telegram 单列运维建议。 |

原 11 条中，1/2/5/6/7/11 六条有可核验的代码问题或调用放大事实；其中 2/6 有共同的 BTC 数据可用性影响，不宜机械相加。3/4/8/9/10 应调整为政策、部署、可维护性、工作区或测试建议。本文不以重新计数代替严重度评估。

### 2.1 P2-3 的既有防护

- [全局 middleware](/Users/zhangyun/workspace/nofx/api/server.go:41) 为 `/api/breakout*` 启用 [每 IP market 桶 120 次/分钟](/Users/zhangyun/workspace/nofx/api/request_limits.go:32)。
- [scanComputeMu.TryLock](/Users/zhangyun/workspace/nofx/api/handler_breakout.go:126) 将该接口的实际扫描串行化，竞争请求返回 503。
- [scanCacheTTL](/Users/zhangyun/workspace/nofx/api/handler_breakout.go:45) 为两分钟，缓存跨 limit/concurrency 参数共享。`limit` 只截取返回值；真正扫描的是 TopVolumeSymbols(30)，与报告的 `limit×~6` 描述不同。

复现用例得到 120 个匿名缓存响应，第 121 个返回 429；扫描锁已持有时请求返回 503、Retry-After=5。成功扫描后的两分钟内重复请求不会重新计算。上游持续失败时不填缓存、以及公开接口与后台调度器未共享扫描锁，仍可另行评估容量，但不能省略现有防护直接下原结论。

### 2.2 P2-5 的正确误差方向

实际比较为 `coin_live_24h < btc_closed_24h`：

| 场景 | BTC 闭合 24h | BTC 实时 24h | 币实时 24h | 实际 | 若比较实时 BTC |
| --- | --- | --- | --- | --- | --- |
| BTC 刚上涨 | 1% | 5% | 3% | 放行 | 应拦截，币弱于 BTC |
| BTC 刚回落 | 5% | 1% | 3% | 拦截 | 应放行，币强于 BTC |

上述两例已用真实 `btcWeakLongCodes` 复现。测试里的实时 BTC 仅作为预期政策的比较参考；现行函数本身只接收闭合 BTC 序列。

## 3. §4.1 不能整体盖“验证无误”

最明确的反例是原 [§4.1 BTC 数据链](/Users/zhangyun/workspace/nofx/docs/architecture/CODE_REVIEW_2026-10-06.md:107) 所称“并发合流”。[binanceBTC4hCloses](/Users/zhangyun/workspace/nofx/kernel/engine_data_binance.go:731) 的实际顺序是：

1. 锁内判断 cache、更新 lastAttempt；
2. 第 743 行释放锁；
3. 第 748 行才发 HTTP 请求；
4. 请求完成后重新加锁发布 cache。

没有 singleflight、inflight 等待或者跨请求持锁。冷启动时第二个调用可能在第一个成功请求仍在进行时返回 nil；暖缓存过期时，多个调用都可发 HTTP，失败后也不适用“cache 为空”的 30 秒节流。这是纯静态证据，本文未针对真实 BTC 网络执行并发压力测试。

三处净 RR 和 min-size 成本公式的数学口径核对成立；相关既有测试通过。4h/1h 缓存确实分离、分类器确实共享；不能把这些成立的局部事实扩展为整个数据链已经免于后续审查。原 §4.1 的其他条目保留为上一轮覆盖记录，本文没有逐条重新生成动态证明，不重复宣称“全部正确、无需重审”。

## 4. P3 与 F01–F16

四条 P3 的核心代码事实均能核实：闭合输入下 volSlotMultiple 仍读 n-2；空 shared 导致回放 α/β=1 且缺 OI/Funding；RunShortTuner 跨 HTTP 持锁；历史池固定保留 7+2 天而配置可取任意正数。

P3-3 的 HTTP 超时上界可达 50 × 2 × 15s ≈ 25 分钟。若改为锁外拉取，回锁提交时还需合并并发新增样本，避免旧快照覆盖新 journal，不能只机械移动锁。

F01/F02/F03/F06/F07/F09–F16 的所列主要修复路径与当前代码相符，构建及所选回归通过。F04 明确失败；F05/F08 依赖同一 signed-short 修复，完整端到端行为只能给条件裁决。因此“15 项无条件合格，只有 F04 有问题”应改写为“主要修复成立，F04 的符号回归同时阻断 F05/F08 的负数量空头路径”。

适配器核对是代码及模拟响应层面的结论，没有本次实盘订单验证；OKX 包没有测试文件。不能把构造响应通过等同于所有交易所实际保护回读已获得全面认证。

## 5. 生产事实与报告一致性

只读核对结果：PID 67272 于 **2026-10-05 17:07:44（Asia/Shanghai）** 启动，txt 路径为本仓库 `nofx`。磁盘文件时间为 10-05 17:07；`go version -m nofx` 显示 `vcs.revision=4cf16c40...`，同时 **vcs.modified=true**。故应表述为“以 4cf16c40 为版本元数据的脏工作树构建”，不能等同于纯净的 4cf16c40 源码。

二进制包含 processProtectionWatchdog 方法名，未找到本批新增的 protectionFaultReason / actualFillRisk / entryExecutionBlocked 方法名和 `live mark or quantity unavailable` 文本。结合时间与元数据，支持 **F04/F09 等关键修复尚未进入该二进制**；完整构建源码差异不能仅由 Git revision 恢复。本次没有重启生产进程。

按报告提交时点截断现有 `nofx-console.log`：

| 明确统计窗口（2026，Asia/Shanghai） | 磨顶 reserved-slot skipped 日志条数 | reserved-slot 入选日志条数 |
| --- | --- | --- |
| 10-05 17:07:44 → 10-06 19:54:37 | 191 | 0 |
| 10-06 00:00:00 → 19:54:37 | 130 | 0 |
| 10-06 13:25:00 → 19:54:37 | 44 | 0 |

原“45/0”没有给统计窗口，不能用作可复核的频次结论。这些是日志事件计数，可能含同一候选/不同策略反复扫描，不是独立交易机会或完整入选率。

原 §5“13:25 首扫 skipped 66”与现有日志不符：进程启动后的首个质量下限记录是 **10-05 17:08:14，skipped 76，piggy floor 6/24**；10-06 13:25:16 是 **skipped 69，piggy floor 8/24**。行情门码在该窗口没有匹配日志，仅能证明“该日志中未见”，不能直接证明没有符合条件的候选。10-04 网络自愈归因及精确 8/74 口径未由本文补证，不应新增肯定结论。

文档还留有以下定稿问题：

- 附录 A 的 kernel engine/prompt/schema 行仍写 `agent（进行中）`，与“全部完成”冲突。本文不能替上一轮审查者凭空补一个完成状态。
- F02 将失败节流指向 P2-1，应为 P2-6；开头修复裁决指向 §5，应为 §4；开头把 P0 指向 §3，应为 §1。
- §2、§3 引言对原 P1 两项和 P2-3/P2-4 的映射不一致；按清单正文，scan=P2-3，sizing=P2-4。
- §3.1 已有 1–4 有序项，可引用为 P3-1/P3-2，但显式稳定编号更清晰。“进行中”残留意味着无法认可“复核状态全部清零”。
- panic recover 的 Git 引入提交是 `6edb7f454`，时间为 10-04 06:51:19；代码注释写的是“10-03 review”。原“10-03 引入”应区分审查/编辑日期与提交日期。
- “工程质量高于以往任何一轮”没有评价指标，只能保留为主观判断。

## 6. 本次验证与证据

- `go build ./...` 通过。
- 不用测试缓存的回归：trader、market/breakout、kernel、store、api、KuCoin、Gate、Hyperliquid、Bybit、Aster 均通过；OKX 为 `[no test files]`。
- 五包 overlay 隔离复现集通过；其中用例有意断言上述基线缺陷存在，PASS **不代表缺陷已修复**。worker 用例仅终止测试子进程。内存假行情与临时目录不会访问交易账户或生产数据。
- 初次受限环境下，market 的 httptest 因禁止监听端口失败；在允许本地测试服务和 Go 缓存的环境重跑通过，不把环境限制记为产品缺陷。本次未重跑 race，不扩大测试覆盖承诺。

证据：[复现输出](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-06/evidence/reproduction-output.txt)、[回归输出](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-06/evidence/suite-output.txt)、[生产只读证据](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-06/evidence/production-output.txt)、[重跑说明及测试源码](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-06/evidence/README.md)。业务代码和原报告保持原基线，本文与证据尚未提交。
