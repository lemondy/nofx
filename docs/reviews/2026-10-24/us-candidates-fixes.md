# 美股候选审查修复记录（2026-10-24）

工作目录：`/Users/zhangyun/workspace/nofx/.claude/worktrees/fix-cand-u`。
对应报告：`us-candidates.md`，原报告未修改。7 项均先核对实现，确认成立后修复。未提交或推送，未调用真实交易接口，未向 `/Users/zhangyun/workspace/nofx/data` 写入文件。

## 逐项结果

| 项目 | 结论与复核依据 | 最小修复及回归测试 |
| --- | --- | --- |
| 1. 日线/周线新鲜度 | **已修复**。原取数、扫描与快照只有根数判断，没有 NYSE 截止日期判断。 | 用仓库 NYSE 日历计算已结算交易日/周末交易日；常量 `CloseDataGrace=90m` 允许刚收盘的一根暂缺，覆盖节假日、半日市与 DST。Yahoo/bStock 日周线过旧且已有数据时绕过缓存刷新一次，仍旧则返回 `ErrStaleBars`；刷新失败保留旧数据作诊断。扫描逐标的记录 `data_through`（包括跳过项）并拒绝旧数据，快照将过旧周期列入 `MissingTF`，不阻塞持仓退出。测试：`StaleDailyRefresh`、`RequiredNYSEDate`、`StaleSourcesForceRefresh`、`ForceRefreshBypassesYahooCache`、`ScanRejectsStaleDaily`、`SnapshotStaleBarsBlockOnlyEntries`。 |
| 2. 过期扫描报告 | **已修复**。原刷新失败返回旧报告，选股只判断非 nil。 | 候选授权报告年龄严格小于 `min(2×20m,90m)=40m`，并逐行校验数据截止日期；扫描缓存也按最新必需交易日失效。旧报告保留作诊断，不授权新增扫描候选；持仓与 hybrid 手动标的保留。测试：`ExpiredScanRetainsOnlyHeldAndManual`、`FreshReportWithOldDataCannotAuthorize`、`ReportChecksAgeAndIndividualDates`、`ScanCacheExpiresAfterSettlementGrace`。 |
| 3. manual 模式持仓授权 | **已修复**。原 manual 提前返回，周期配置也不补持仓，校验器首先拒绝不在配置中的标的。 | 各模式均补入程序持仓，周期配置统一使用管理列表；另用 `EntrySymbols` 明确授权新增。未获候选授权的持仓允许 hold/reduce/close/adjust_stop，open/add 返回 `EXIT_ONLY_SYMBOL`。测试：`ManualKeepsRemovedHolding`、`RemovedHoldingCannotAdd`、`RemovedHoldingExitAuthorization`（6 个动作）、`ExplicitEntryAuthorization`。 |
| 4. bStock 自然日日线参与排名 | **已修复**。原回退后直接按切片位置计算 12-1 动量与 SMA200。 | bStock 回退日线必须是连续 NYSE 常规交易日、实际常规开盘时间的序列；UTC 午夜、夜盘、非交易日或缺交易日的序列记录“momentum unavailable”，整条扫描结果跳过。持仓快照继续保留管理数据，但不生成该序列的有效动量读数。测试：`BStockCalendarDaysNotMomentum`；旧扫描夹具改为真实交易日时间戳，原排序、单飞、基本面与取消断言保留。 |
| 5. 一个 lot step 的余额被隐藏 | **已修复**。原 `qty<=step` 直接省略；下游使用展示列表判断库存并缩减归属。 | `GetPositions` 返回全部正余额并添加 `dust` 标记。新增不经过持仓/估值缓存的 `RawStockBalance`（free+locked）；缩减前先核对原始余额，保护变更后的余额复核也使用该读数；不将正余额向下取整成零。首次原始余额读取失败则隔离当轮操作，保留归属和既有保护。测试：`OneStepBalanceVisible`（小于/等于/大于 step）、`RawBalanceBypassesPositionCache`、`ShrinkVerifiesRawInventory`（展示遗漏及原始余额读取失败）。 |
| 6. 入场执行前复核 | **已修复**。原执行使用 LLM 前的快照价格，未再次检查时段、报价与背离。 | 完成同账户管理者检查后，绕过 bStock/Yahoo 报价缓存取最终行情；用执行时钟重查时段、正股参考报价（同 session、15m 内）、bStock 请求采样时间（30s 内）和背离，并在 tick 舍入后重新校验止损距离、TP/限价及风险数量。失败记录原因并放弃此次入场；退出/保护不经过此新增门禁。测试：`EntryRechecksClock`、`EntryRechecksQuote`（5 种异常）、`FinalPriceResizesRisk`、`ExecutionQuoteBypassesBothCaches`。 |
| 7. Yahoo 请求进行中跨收盘 | **已修复**。原缓存时间与闭合判断使用响应完成时间。 | 缓存及结果记录请求开始时间，K 线最终性按请求开始时间判定；请求跨收盘/周期边界时独立重取一次，重复跨界则报错且不发布该响应。周线采用实际周末交易日收盘，包含 Good Friday 缩短交易周。测试：`YahooRequestCrossesClose`、`YahooWeeklyRequestCrossesHolidayClose`。 |

测试简称均带 `TestReview1024` 或 `TestStockReview1024` 前缀。新增 24 个顶层测试及其表驱动子用例，分布于：

- `market/usstock/review1024_test.go`
- `kernel/stockengine/review1024_test.go`
- `trader/auto_trader_stock_review1024_test.go`
- `trader/binance_bstock/account_review1024_test.go`

## 修复前复现

先加入仅依赖原接口的回归测试，再运行原实现，确认旧日线被接受、请求跨收盘不重取、旧扫描结果进入候选、自然日动量入选、持仓管理动作被 `UNKNOWN_SYMBOL` 拒绝、延迟入场仍买入、一个 step 库存被隐藏。

manual 周期用例补全模拟账户初始化后，额外用 Go `-overlay` 加载 HEAD 的原周期和原校验器，确认实际平仓被 `UNKNOWN_SYMBOL` 拒绝。overlay 文件位于 `/tmp`，没有回写原实现。辅助边界用例用于验证修复后契约；未声称这些依赖新增字段/接口的辅助用例可直接在原实现上编译。

## 改动文件

生产代码仅在授权范围：

- `market/usstock/freshness.go`（新增）、`series.go`、`yahoo.go`、`quote.go`、`types.go`
- `kernel/stockengine/scan.go`、`snapshot.go`、`validate.go`、`types.go`
- `trader/auto_trader_stock.go`、`auto_trader_stock_cycle.go`、`auto_trader_stock_exec.go`、`auto_trader_stock_state.go`
- `trader/binance_bstock/account.go`

对应既有测试夹具/断言调整：

- `market/usstock/series_test.go`、`series_cache_test.go`、`yahoo_test.go`
- `kernel/stockengine/scan_test.go`、`universe_test.go`
- `trader/auto_trader_stock_test.go`（增加 fake 原始余额接口、报价时间、扫描截止日期）
- `trader/binance_bstock/trader_test.go`（确认 dust 仍在持仓列表）

此外仅新增本修复记录，未改审查报告原文或其他任务的文件。

## 验证

所有 Go 命令均在本 worktree 执行，使用 `GOCACHE=$PWD/.gocache`、`GOPROXY=off`，最终验收另设 `GOFLAGS=-mod=readonly`，关闭 live smoke：

```sh
go build ./...
go vet ./market/usstock ./kernel/stockengine ./trader ./trader/binance_bstock
USSTOCK_LIVE=0 go test ./market/usstock ./kernel/stockengine ./trader ./trader/binance_bstock -count=1
```

最终验收：**全部通过**。构建和 vet 无报错；完整包测试结果：

- `nofx/market/usstock`：通过（0.146s）
- `nofx/kernel/stockengine`：通过（0.496s）
- `nofx/trader`：通过（7.064s）
- `nofx/trader/binance_bstock`：通过（0.336s）

另运行 `go test -race ./market/usstock ./kernel/stockengine -run 'TestReview1024|TestYahooSerializationAndCanceledQueue|TestGetScanReportConcurrentSingleFlight|TestWarmScanReportSharesFlightAndDeadline' -count=1`：两包通过。stockengine 的 race 链接出现 macOS `LC_DYSYMTAB` warning，退出码为 0，未检测出数据竞争。

没有出现沙箱监听端口/httptest panic；本次相关包完整测试无需排除任何用例（显式关闭的 live smoke 除外）。`git diff --check` 通过。

修复前复现输出：`/tmp/nofx-us-review1024-before.log`、`/tmp/nofx-us-review1024-manual-before.log`。最终验收输出：`/tmp/nofx-us-review1024-acceptance.log`。定向 race 输出：`/tmp/nofx-us-review1024-race.log`。

## 局限与保守选择

- NYSE 日历沿用仓库的规则日历，没有新增临时休市事件源；上游复权、拆股、分红数据正确性未做线上核验。
- 无法证明常规交易日口径的 bStock 日线不再授权任何扫描新增（包括 Kell），可能减少候选；已有持仓继续在管理列表中。
- bStock `ticker/price` 没有逐笔成交时间，新鲜度用本次请求开始时间衡量；正股参考价仍要求供应商时间戳满足同 session 和 15 分钟上限。入场检查之后到交易所接受订单之间的市场变化不能由本地预检查消除。
- 未调用真实交易接口，也未做真实下单/供应商在线 smoke；测试使用注入时钟、内存 HTTP transport、fake executor 与临时 SQLite。
