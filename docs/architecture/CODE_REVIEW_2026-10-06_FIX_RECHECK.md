# 第五轮审查修复后复核 · 2026-10-06

复核源码：`74a5daae0c5a8c74029e46eeeb3237713a492c16`；比较基线：`e67685b0`（业务基线仍为 `abba5978`）。本轮独立核对实际 diff、修复后的调用路径和模拟测试，没有将原报告中的执行指令作为本轮任务。

**结论：原 P0-1 的负数量空头误报，以及原 P2-11 的父协程状态卡死、两类 worker panic 杀进程，已修复。其他成立的问题未在本提交修复，不能将此次通过解释为原报告全量问题清零。另有一处低优先级回调顺序回归，当前生产调用均传 nil，不影响这两个关键修复的通过。**

## 1. 已修复的关键项

| 对照项 | 源码与独立验证 | 裁决 |
| --- | --- | --- |
| 原 P0-1：负数量空头 | [watchdog](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:1612) 在数量判断前执行 `math.Abs(qty)`，后续覆盖量、SL/TP 修复使用正数量。足额保护的 qty=-1 不再产生故障；SL 0/1 与 0.4/1 分别补 1、0.6，经真实覆盖函数回读判足额，旧故障清除，第二轮不重复补挂。 | 已修复 |
| 原 P0-1：账户消费点 | 注册同账户 peer 后，健康负数量空头不再阻断 `entryExecutionBlocked`、`pendingAccountHaltReason`、`applyHardRiskGates`。 | 已修复 |
| 原 P0-1：越过空头止损 | qty=-1、mark=110、记录 SL=105，提交 CloseShort；平仓回读后下一轮清除等待协调的故障。 | 已修复 |
| 原 P2-11：父协程 panic | [统一 defer](/Users/zhangyun/workspace/nofx/market/breakout/scheduler.go:289) 恢复 panic 后持锁清除 scanning，回调一次。独立 transport 注入 panic，随后 RefreshNow 再次到达上游，调用数从 1 增至 2。 | 已修复 |
| 原 P2-11：AnalyzeMany worker | [worker 自身 recover](/Users/zhangyun/workspace/nofx/market/breakout/breakout.go:356) 覆盖 Analyze 等执行路径；panic 展开时释放 semaphore、执行 wg.Done，失败项不发布。子进程中三符号连续 panic、并发度 1，全部记录符号及堆栈，进程正常返回。 | 已修复 |
| 原 P2-11：ScanShorts worker | [worker 自身 recover](/Users/zhangyun/workspace/nofx/market/breakout/shortscan.go:795) 同样覆盖独立 kernel 扫描调用；三 worker panic 的子进程正常返回，结果为空并报告无可分析符号，未伪造成功结果。 | 已修复 |

F04 的符号回归不再阻断 F05/F08 的负数量空头路径。上述动态验证是模拟交易所响应；无记录 SL 的全裸仓 ATR 重算路径未在本轮新增动态验证。

父协程和 worker 均新增 `debug.Stack()` 日志，便于定位失败符号及调用栈。此提交没有新增 Telegram 推送；按上一轮独立裁决，它仍属于运维增强，不能算作尚存的第三个正确性缺陷。

## 2. 仍存在的原问题

| 原编号 | 当前证据 | 本轮验证方式 |
| --- | --- | --- |
| P2-1：C2 两处残留 | [bb_ride](/Users/zhangyun/workspace/nofx/kernel/bb_ride.go:61) 与 [pump_guard pivot](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:528) 仍无条件丢末根。 | 源码未变；复跑 bb_ride：相同闭合历史，无形成中尾根时 ride=true/windows=4，附形成中尾根后正确读入闭合 doji，ride=false/windows=0。 |
| P2-2：BTC unknown 双层分歧 | [kernel](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:1662) 缺数据不生成 BTC 禁开码；[pending](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending_risk.go:159) 任一 BTC 开关启用而不足 60 根时拒绝。市价路径未新增对应 unknown 检查。 | 相关源码与上一轮完全相同。 |
| P2-5：BTC 闭合/币实时窗口 | [btcWeakLongCodes](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:1659) 仍比较 coin_live_24h 与 btc_closed_24h。 | 数值用例再次复现：BTC 闭合 1%、实时 5%、币 3% 时误放；闭合 5%、实时 1%、币 3% 时误拦（以同为实时的比较政策为参照）。 |
| P2-6：BTC 缓存并发与静默窗 | [binanceBTC4hCloses](/Users/zhangyun/workspace/nofx/kernel/engine_data_binance.go:731) 第 743 行解锁、第 748 行才请求；未加 inflight/singleflight。冷缓存并发可返回 nil，暖缓存过期可重复请求，30 秒节流仅适用于空缓存。 | 相关源码未变；本轮未对实际网络做并发压力测试。 |
| P2-7：逐仓读取挂单 | [refreshExecutionAccount](/Users/zhangyun/workspace/nofx/trader/account_execution.go:216) 仍在每个非零持仓中调用 GetOpenOrders，同 symbol 多仓也未合并。 | 调用放大事实未变；严重度仍应按接口预算评估。 |
| P3-1：同时间槽量能 | [volSlotMultiple](/Users/zhangyun/workspace/nofx/market/breakout/breakout.go:993) 在闭合输入上仍读 n-2。 | 函数未变。 |
| P3-2：回放评分维度 | [backtest shared](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:168) 仍为空，α/β=1 且缺历史 OI/Funding；研究发布锁仍有效。 | 相关路径未变，属于回放与 live 的口径限制。 |
| P3-3：调参器跨 HTTP 持锁 | [RunShortTuner](/Users/zhangyun/workspace/nofx/market/breakout/shorttuner.go:189) 锁仍跨越 historicalShortLabel 请求。 | 相关路径未变。 |
| P3-4：历史池窗口 | [固定保留期](/Users/zhangyun/workspace/nofx/market/breakout/gainer_history.go:149) 仍为默认 7 天 + 2 天缓冲，消费配置可取更大的正数。 | 相关路径未变。 |

原 P2-3/4/8/9/10 的降级裁决保持有效；此次没有出现可以将它们重新认定为五个已证实 P2 缺陷的新证据。

## 3. 本次发现：回调先于状态清理

`runOnce` 的 [错误路径](/Users/zhangyun/workspace/nofx/market/breakout/scheduler.go:302) 和 [正常结尾](/Users/zhangyun/workspace/nofx/market/breakout/scheduler.go:382) 仍主动调用 fireOnDone；清除 scanning 要等函数随后进入 defer。因此与“清理及回调由 defer 单独负责”的注释不一致，也改变了旧代码“先清 scanning，再回调”的顺序。

独立用例确认：正常和错误路径的回调均看到 scanning=true；错误路径回调内调用 RefreshNow(1ms)，仍等待约 301ms，期间没有上游调用，回调返回后 scanning 才清除。panic 路径则先清除再回调，顺序不一致。

**影响限于非 nil 回调。当前 Start、ticker、RefreshNow 三个生产调用点全部为 runOnce(nil)，所以这不是当前线上扫描器再次永久卡死，也不否定 §1 的修复。** 建议删除正文两处 fireOnDone，只保留 defer 末尾的一处；再补回调内观察 scanning=false 的正式回归测试。本轮只记录此低优先级问题，没有改动业务源码。

## 4. 验证记录

- `go build ./...`：通过。
- `go test ./trader ./market/breakout -count=1`：两包完整测试通过。
- `go test ./trader ./market/breakout -run '^TestFix06' -count=1 -v`：7 个正式修复测试通过。
- `go test -race ./trader ./market/breakout -run '^TestFix06' -count=1`：通过，未报告 data race；trader 链接器有 LC_DYSYMTAB warning，未导致失败。
- Go overlay 独立用例：新增 6 个顶层用例（trader 3 / market 3）通过；旧 kernel 两个缺陷复现用例通过。回调及 kernel 用例刻意断言残留问题存在，PASS 不等于这些残留已修复。

证据与重跑命令：[README](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-06/fix-verification/README.md)、[独立验证输出](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-06/fix-verification/independent-output.txt)、[构建及正式测试输出](/Users/zhangyun/workspace/nofx/docs/architecture/review-2026-10-06/fix-verification/validation-output.txt)。本轮结论针对源码及模拟响应，不涉及生产进程的版本或部署状态。
