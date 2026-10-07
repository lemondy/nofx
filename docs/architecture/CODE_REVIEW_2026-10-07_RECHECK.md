# 最新修复再次复核 · 2026-10-07

源码基线：`77f3838af41a19c2495da0a897256be87bf4f375`。本次核对 `74a5daae`、`8583e59a`、`6cc2d726`、`77f3838a` 的最终组合行为。审查开始时工作树干净；本轮仅新增报告和隔离验证快照，没有修改业务源码、提交或部署。

**结论：不能裁定“当前核查的问题全部修复”。挂单秒撤的结构性数据长度问题已修复，原空头数量/Panic 问题也已关闭；仍有两项 P2 和两项 P3 可独立复现。另有回放评分的既有研究限制，仅补注释，不应算成已实现历史数据口径一致。**

## 1. 可复现的剩余问题

### R1 · P2：新增 BTC_REGIME_UNKNOWN 未进入执行端绝对禁开码

[kernel 新禁开码](/Users/zhangyun/workspace/nofx/kernel/signal_layer.go:1876) 在 `BTCFilterShort=true` 且 BTC 数据不足时令 short gate 不允许开仓；但 [absoluteBanCode](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:2252) 没有登记该码。

结果是 [applyHardRiskGates 市价分支](/Users/zhangyun/workspace/nofx/trader/auto_trader_risk.go:2374) 和 [entryExecutionBlocked](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending_risk.go:220) 均不阻断它。`LimitEntryEnabled=false` 时，市价 dispatch 直接通过；限价模式开启、且有独立市价例外证据时，也通过。kernel 的 `MarketException` 是先独立按 ride/breakout 证据计算，再添加 BTC 禁开码，故“例外证据成立 + BTC gate 禁开”是可共存的状态。

独立用例：`TestAudit77ResidualBTCUnknownMarketBypass`。构造唯一失败码为该码的 GateState，确认决策过滤保留 `open_short`、执行预检查返回 nil、市价 dispatch 在上述两种配置下均不拒绝。测试停在授权链路，没有向实际交易所下单。普通限价决策仍被“不允许该方向”分支阻断，影响不能扩大成所有入场方式都绕过。

建议将 `BTC_REGIME_UNKNOWN` 纳入绝对禁开码，补决策过滤、执行预检查和市价例外链路的回归。原 P2-2 的短侧统一只完成了 kernel 层。

### R2 · P2：挂单 BTC unknown 检查仍与多/空政策冲突

[pendingDirectionBlocked](/Users/zhangyun/workspace/nofx/trader/auto_trader_pending_risk.go:170) 仍在“任一 BTC 开关启用”时，对两侧挂单共同执行 `len(btc)<60 → 撤单`，没有按方向或新 kernel 政策区分。

默认 `BTCFilterLong=nil` 是启用、`BTCFilterShort=nil` 是关闭。BTC unknown 时，kernel 多头按其明确注释放行 BTC 过滤，空头默认不启用 BTC 过滤；挂单层却把两侧都撤销。`6cc2d726` 声称保留多头 fail-open，因此不能用这段旧的统一 fail-closed 作为“已对齐”的依据。

独立用例：`TestAudit77BTCUnknownDirectionPolicy` 真正开启 long 过滤，证明 kernel 政策；`TestAudit77ResidualBTCUnknownPendingPolicy` 在相同默认开关下模拟 BTC 缺数据，long/short 都返回 `pending BTC regime data unavailable`，实际 `processPendingEntries` 都撤销 NEW 订单并清除 pending 状态。

这不是原“约 20 根行情必撤”的复发；它需要 BTC 数据缺失。发生时，仍可能造成已获 kernel 许可的挂单反复被监控撤销。建议由方向对应的 kernel gate 决定 BTC 禁开，移除与之冲突的两侧统一拒绝。

### R3 · P3：历史池没有保留所有调用者的最大窗口

[recordGainerHistory](/Users/zhangyun/workspace/nofx/market/breakout/gainer_history.go:151) 的 `keep=max(7,historyDays)` 仅计算本次调用的窗口，没有记录其他策略要求的最大窗口；所有调用者共享同一文件。[后台调度器](/Users/zhangyun/workspace/nofx/market/breakout/scheduler.go:369) 定期调用 `ScanShorts(...,0,0)`，解析为默认 7 天。

独立用例：`TestAudit77ResidualMixedHistoryWindows` 在同一文件中写入 20 天前历史，30 天配置记录后该日仍存在；紧接默认 7 天调用后，该日被删除。长窗口策略随后再扫描也无法从文件恢复该历史。原 P3-4 在多调用者场景仍存在。

建议使用跨调用者一致的存储保留政策，例如登记所有有效窗口后按最大值裁剪，或为共享存储设定足以覆盖支持窗口的固定保留上限；消费窗口继续按每个策略独立处理。

### R4 · P3：调参器三阶段合并丢失旧标签失效状态

[第一阶段](/Users/zhangyun/workspace/nofx/market/breakout/shorttuner.go:205) 将 `Evaluated=true && LabelVersion!=2` 的旧标签在内存中改成未评估，然后锁外重新请求历史价格和资金费。但 [第三阶段](/Users/zhangyun/workspace/nofx/market/breakout/shorttuner.go:249) 重读文件后，旧标签仍为 `Evaluated=true`，随即被第 253 行跳过。重新请求得到的正确结果被丢弃，`legacyInvalidated` 只促使相同旧状态再次落盘。

独立用例：`TestAudit77ResidualLegacyLabelMerge` 写入成熟 v1 标签，提供成功的历史价格/资金费响应，连续执行两轮。每轮都做两次 HTTP 请求，但 journal 始终保留 `Evaluated=true / LabelVersion=1 / Outcome=99`。旧标签无法迁移，重复消耗打标预算，并继续被仅接收 v2 的研究样本筛选排除。

这是 `6cc2d726` 锁外重构引入的回归。原跨 HTTP 持锁问题确已修复；独立真实并发追加用例也通过。建议在 fresh-read 合并阶段保留/重新执行旧版本失效判定，且维护失败重试、50 条上限和并发追加语义。研究发布锁仍有效，此问题没有证明 live 权重被错误发布。

## 2. 已验证的修复及边界

| 对照项 | 本轮裁决与证据 |
| --- | --- |
| 原 P0-1 负数量空头 | 已修复。独立复跑 qty=-1 的足额保护、缺保护补挂并回读、越过止损退出和同账户 peer 三个消费点；未复现 quantity unavailable 自锁。 |
| 原 P2-11 父/worker panic | 已修复。父 panic 后可再次扫描；两类 worker 各三次 panic 均在子进程内恢复并记录符号/堆栈，semaphore/waitgroup 正常结束。 |
| 上轮回调顺序 | `8583e59a` 已修复。正常、错误和 panic 均由统一 defer 先清 scanning 再回调；正式回归及 race 通过。 |
| 挂单秒撤事故数据链 | `77f3838a` 已修复结构性长度错误。独立用例不使用 `AutoTrader.recheckDataFn`，经过真实 recheck → getMarketTimeframes → adapter live price → market 策略取数边界。15m、5m 主周期都请求 60 根、四个选定周期和正确 venue；模拟返回每周期恰好 60 根后 NEW 挂单存活。随后 generic 20 根行情只用于 SL 越过检查，没有重新作为方向校验输入。 |
| 原 P2-1 C2 | bbRideWalk 多/空共同路径和 pump-guard pivot 均采用时间判定的 ClosedKlines。正式闭合尾根/滞后 feed 回归通过。 |
| 原 P2-5 BTC 24h 比较 | 正常数据路径已改用同源 live ticker，正式数值回归通过。ticker 失败时仍显式回退 closed BTC 与 live coin 比较；这是保留的降级限制，不能宣传为所有失败场景也同口径。 |
| 原 P2-6 BTC 并发缓存 | 冷启动正式用例通过；新增六调用者暖缓存过期独立用例确认只请求一次，全部拿到 300 根，race 通过。冷缓存失败后的 30 秒节流仍是既有政策，修复不等于数据永不缺失。 |
| 原 P2-7 挂单查询放大 | 同 symbol 两侧仓位去重已修复：watchdog 正式用例和 refreshExecutionAccount 独立用例均为一次查询，保留两侧保护/绝对数量。不同 symbol 仍各查一次；没有新增所有 symbol 合批接口。 |
| 原 P3-1 volSlotMultiple | 已改为读取闭合输入的最后一根 n-1，正式量能突增回归通过。 |
| 原 P3-3 HTTP 持锁 | 已修复。独立用例将历史 HTTP 阻塞，同时调用实际 SampleShortSignals，追加先完成；HTTP 释放后成熟样本打成 v2，追加样本保留。见 R4 的另一处迁移回归。 |
| 原 P3-2 回放评分 | [backtest](/Users/zhangyun/workspace/nofx/market/breakout/backtest.go:173) 仍使用空 shared，缺历史 OI/funding/depth，α/β 与 live 评分口径限制未实现修复。新增注释正确澄清研究用途；发布锁仍阻止这类前向收益代理直接发布 live 参数，应列为已记录的限制。 |

撤单原因日志现在在尝试撤单前输出，确实提高归因可见性；它是撤单意图日志，单独看到该行不等于交易所已成功撤单。正式方向转向测试中，先设置 `EntryTimingGate=true` 后又替换了 StrategyConfig，实际门被关闭；本轮另补真正启用该门的用例，明确命中 `MICRO_TREND_NOT_LONG`。这两点不否定事故数据修复。

## 3. 验证记录

- `go build ./...`：通过。
- `go test ./kernel ./market ./market/breakout ./trader ./store -count=1`：五包完整测试通过。
- `go test -race ./kernel ./market/breakout ./trader -run '^Test(Fix3|Fix7|Fix06)' -count=1`：通过。
- 独立 Go overlay：10 个新顶层用例，加 5 个复用的独立验证用例，全部经普通或针对性 race 记录验证；包括上述四个缺陷复现。独立新用例的 race 检查通过，未报告数据竞争。
- darwin 链接器有 LC_DYSYMTAB warning，没有导致构建/测试失败。

`Residual` 用例刻意断言问题存在，**PASS 表示复现成功，不能作为“问题已修复”的 CI 通过结论**。源码及模拟边界是本轮结论范围，没有做实盘下单、重启或生产部署验证。

可重跑证据：[README](/Users/zhangyun/workspace/nofx/docs/architecture/recheck-2026-10-07-77f3838a/README.md)、[独立输出](/Users/zhangyun/workspace/nofx/docs/architecture/recheck-2026-10-07-77f3838a/independent-output.txt)、[独立 race 输出](/Users/zhangyun/workspace/nofx/docs/architecture/recheck-2026-10-07-77f3838a/independent-race-output.txt)、[最终针对性输出](/Users/zhangyun/workspace/nofx/docs/architecture/recheck-2026-10-07-77f3838a/final-targeted-output.txt)、[正式验证输出](/Users/zhangyun/workspace/nofx/docs/architecture/recheck-2026-10-07-77f3838a/validation-output.txt)。
