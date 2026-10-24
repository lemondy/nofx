# 加密货币候选审查 C1 修复记录

范围：`crypto-candidates.md` 第 4、5、6、7、8、9、10、12 项。逐项对照实现后均确认成立，均已修复。第 1、2、3、11 项未处理，审查报告原文未修改。

## 逐项结果

| 项目 | 结果与修复方式 | 主要复现测试（均以 TestReviewC1 开头） |
| --- | --- | --- |
| 4 Hyperliquid 名称 | 已修复。按原始 metadata universe 建立内部符号到场所原名、资产编号、倍率、退市状态的映射；不删除倍率前缀，不对价格或数量做隐式换算。`KPEPEUSDT` 恢复为 `kPEPE`，`PEPE`、`1000PEPE` 不向其别名映射。交易适配器在调用 SDK 前拒绝未知名称，并核对 SDK 资产编号；BTC 的合法编号 0 仍可使用。退市项保留在名称映射中，供已知现仓查询、退出和保护使用。报价、K 线、精度、挂单查询及平仓符号匹配使用对应名称。 | FrontendOrdersPreserveKPrefix、ContractMappingKeepsNamesIDsAndUnits、CandlesRestoreRawNameAndRejectUnknown、TraderResolvesMetadataIncludingDelisted、SDKAssetIDsValidatedWithoutTrading、KnownCandleMappingSurvivesMetadataOutage |
| 5 Hyperliquid 退市 | 已修复。解析 `isDelisted`，在成交量排序、Top N 截断前排除退市条目，原 universe 下标不重排。候选缓存从 24 小时收紧为 5 分钟，限制退市状态缓存时间。 | DelistedFilteredBeforeRanking |
| 6 AI500 两层缓存 | 已修复。provider 返回完整快照及原始 `FetchedAt`、`ExpiresAt`；relay 命中保留原始时间。kernel fallback 截止为 `min(FetchedAt + 15m, ExpiresAt)`，不再以读取时间续期。relay 原有 30 分钟上限保持，因此两层合计不会变成 45 分钟。 | AI500RelayHitDoesNotRenewAge、AI500FallbackHonorsEarlierSourceDeadline、AI500SnapshotRetainsSourceLifetimeAndFullList |
| 7 AI500 limit | 已修复。provider 总是请求上游最大范围（100），kernel 缓存完整解析榜单，每个消费者分别截断。显式小于 100 的 AI500 relay 载荷不能写入共享完整榜单缓存，避免另一条小请求路径覆盖榜单。 | AI500SmallLimitDoesNotShrinkFallback、AI500SnapshotRetainsSourceLifetimeAndFullList、DirectAI500AlwaysFetchesFullRanking |
| 8 Vergex relay 错误 | 已修复。浏览器携带 HTTP 状态返回，非 2xx 为失败；AI500、OI、资金流等响应按端点校验对象、榜单数组及已知字段类型。缺失结构、错误对象、错误字段类型不会覆盖或延长最后成功缓存。结构完整的空数组仍是合法空榜。`fetchAll` 把这些错误交给既有健康状态逻辑。直接 HTTP 路径也执行结构校验。 | RelayErrorMustNotReplaceSuccess、RelayHTTPStatusAndHealth、DirectHTTPErrorsAreNotEmptyRankings |
| 9 Piggy 三倍截断 | 已修复。scheduler 支持不截断地读取完整方向快照，kernel 在形态、等级过滤后取前 N；不再假设三倍预取足够。原陈旧门和刷新逻辑保留。 | PiggyFiltersWholeBoard |
| 10 磨顶池空宇宙 | 已修复。先合并 `slowTopSnapshot()` 返回的有效磨顶宇宙，再判断总结果是否为空，之后仍执行 BTC 门、排序和截断。过期磨顶缓存仍被拒绝。 | EmptyMainUniverseKeepsSlowTop |
| 12 首尾空白 | 已修复运行时路径。`market.Normalize` 先 `TrimSpace`；来源候选、排除名单、静态去重及 Vergex 符号使用同一规范化入口，短扫描和 Piggy 在构造候选时规范化。 | NormalizeWhitespace、CandidateWhitespaceExclusionAndDedupe、AI500SnapshotRetainsSourceLifetimeAndFullList |

## 改动文件

生产代码：

- `kernel/engine.go`
- `market/data.go`
- `market/breakout/scheduler.go`
- `market/breakout/shortscan.go`
- `provider/hyperliquid/coins.go`
- `provider/hyperliquid/kline.go`
- `provider/hyperliquid/symbols.go`（新增）
- `provider/vergex/relay.go`
- `provider/vergex/headless.go`
- `provider/vergex/trending.go`
- `trader/hyperliquid/trader.go`
- `trader/hyperliquid/trader_account.go`
- `trader/hyperliquid/trader_orders.go`
- `trader/hyperliquid/trader_positions.go`

测试：

- `kernel/review_20261024_candidates_test.go`（新增）
- `market/review_20261024_normalize_test.go`（新增）
- `market/breakout/review_20261024_shortscan_test.go`（新增）
- `provider/hyperliquid/review_20261024_coins_test.go`（新增）
- `provider/vergex/review_20261024_relay_test.go`（新增）
- `trader/hyperliquid/review_20261024_symbols_test.go`（新增）
- `kernel/review_20261009_ai500_fallback_test.go`（调整现有测试注入接口与缓存清理，原断言保留）

另新增本修复记录。未修改其他范围的源码。

## 验证

全部命令从本 worktree 执行，使用 `GOCACHE="$PWD/.gocache" GOPROXY=off`。

修复前先加入 9 个复现测试，覆盖全部 8 项，执行：

```sh
go test ./kernel ./market ./market/breakout ./provider/vergex ./provider/hyperliquid ./trader/hyperliquid -run '^TestReviewC1' -count=1
```

9 个复现测试均按预期失败，分别暴露名称不匹配、退市漏过滤、缓存续期、limit 覆盖、错误载荷覆盖、三倍截断、磨顶空池提前返回及空白规范化问题。随后补充边界测试，新增共 19 个测试。

最终检查：

```sh
go build ./...
go vet ./kernel ./market ./market/breakout ./provider/vergex ./provider/hyperliquid ./provider/nofxos ./trader/hyperliquid
```

均通过。

相关包的完整测试尝试遇到沙箱禁止监听端口：`httptest: failed to listen on a port ... bind: operation not permitted`。端口相关的 4 个既有测试为：

- `TestFix3ShortTunerMergeKeepsConcurrentAppends`
- `TestFix05HistoricalLabelDoesNotStretchAfterDowntime`
- `TestFix05ScannersIgnoreFormingCandles`
- `TestFix8ShortTunerLegacyLabelMigrates`

`provider/hyperliquid/kline_test.go` 中另有 6 个直接访问外网的行情冒烟测试：`TestGetCandles_BTC`、`TestGetCandles_TSLA`、`TestGetCandles_StockPerps`、`TestGetAllMids`、`TestGetAllMidsXYZ`、`TestGetMeta`。为避免调用外部场所接口，本次未执行这 6 个测试；未修改它们来隐藏限制。

排除上述端口和联网测试后，最终相关 7 个包的测试全部通过：

```sh
go test ./kernel ./market ./market/breakout ./provider/vergex ./provider/hyperliquid ./provider/nofxos ./trader/hyperliquid \
  -skip '^Test(Fix3ShortTunerMergeKeepsConcurrentAppends|Fix05HistoricalLabelDoesNotStretchAfterDowntime|Fix05ScannersIgnoreFormingCandles|Fix8ShortTunerLegacyLabelMigrates|GetCandles_.*|GetAllMids.*|GetMeta)$' -count=1
```

新增复现、AI500 原有 fallback、相关 metadata 并发测试也通过 race 检查：

```sh
go test -race ./kernel ./market/breakout ./provider/vergex ./provider/hyperliquid ./trader/hyperliquid \
  -run '^TestReviewC1|^TestAI500Fallback|^TestMetaConcurrent' -count=1
```

macOS 链接器产生既有 `LC_DYSYMTAB` 警告，命令退出 0，未报告数据竞争。`git diff --check` 通过。

## 局限与边界

- `StrategyConfig.Validate` 位于本任务允许范围之外，未修改保存校验或持久化内容；包含首尾空白的已保存配置在运行时经统一 Normalize 处理。
- 未进行实盘或外网集成验证，未调用下单、撤单、杠杆等交易接口，未运行真实 Chromium relay。状态和响应结构由构造的 HTTP 响应验证。
- Hyperliquid 候选退市状态仍有最多 5 分钟缓存窗口。名称映射按 API URL 隔离主网/测试网；元数据短暂失败时保留已知名称用于 K 线读取，不把旧映射当作新的候选榜单，也不放行未知单位。已知 XYZ 的既有路由保持，未增加现仓保护的元数据网络依赖。
- `api/handler_trending.go` 的页面代理缓存不在允许范围，未修改；策略使用的 provider relay 会拒绝该入口转交的错误载荷。严格响应校验可能拒绝上游将来的结构变更，此时按失败处理，不将其解释为空榜。
- 未 git commit/push，未写入 `/Users/zhangyun/workspace/nofx/data`。新增磨顶复现测试将历史池写入 `t.TempDir()`。
