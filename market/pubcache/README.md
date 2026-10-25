# 公开行情共享缓存（任务 B）

采用 RoundTripper，所有生产 `pubcache.Wrap` 共用进程级条目及 in-flight 请求。
接口是 `Wrap(base)`、`Stats()`；测试可用 `New(Options{Now, MaxEntries, MaxBytes})` 隔离实例。
pubcache 只是原有缓存之下的 HTTP 叠加层。原有解析值缓存、scanner 响应缓存和已收盘 K 线序列缓存全部保留，TTL、失效条件、数据解析与后续判断均与 `origin/dev` 一致。上层命中直接返回，不创建 HTTP 请求，也不触达 pubcache；只有原本会发往网络的请求才进入 pubcache，进一步合并并发或复用仍有效的响应。原缓存的刷新窗口不改成 pubcache 的短 TTL。
没有改动 trader/binance、trader/binance_bstock、trader/paper、bnweight 或 netmeter。

## 调用点清单与接入

下表覆盖任务范围内所有实际 URL 构造点。函数名可直接搜索；共享函数的所有上游调用者自动接入，包括 AI、风控、突破调度、影子评估、历史标签和 MAE 回放。

| 文件 / 函数 | 端点 | 接入路径 | pubcache 之上的原有缓存（全部保留） |
| --- | --- | --- | --- |
| `market/api_client.go` `APIClient.GetKlines` | klines | `NewAPIClient` 包装 transport，保留 hook 的原 client/transport | 本函数无；调用者可能有派生结果缓存 |
| `market/data_klines.go` `getKlinesFromBinanceDirect` | klines | `binanceGetJSON` → `NewAPIClient` | 无独立 K 线缓存；供 `market/data.go` AI 数据入口及回退路径使用 |
| `market/historical.go` `GetKlinesRange` | klines（每个分页 URL） | 历史 client 的 transport，Low 优先级 | 本函数无 |
| `market/data.go` `getOpenInterestData` → `fetchOIData` | openInterestHist | `binanceGetJSON` → `NewAPIClient` | `oiDataCache` 解析结果 **3 分钟**；直接调用 `fetchOIData` 不经过它 |
| `market/data.go` `getFundingRate` | premiumIndex | `NewAPIClient` | `fundingRateMap` **5 分钟**（保留曾因 1h 缓存出现两倍偏差的说明） |
| `market/funding_history.go` `fundingHistory` | fundingRate | `binanceGetJSON` → `NewAPIClient` | `fundingHistoryMap` **5 分钟**；结算间隔另缓存一天 |
| `market/sentiment.go` `fetchBinanceCrowd` | premiumIndex（BTC、ETH） | `sentimentHTTPGet` 的 transport | 正常情绪汇总 **30 分钟**；缺来源 **2 分钟**，保留原来源回退规则 |
| `kernel/engine_data_binance.go` `binanceTopTickers` | ticker/24hr | `binanceGetFn` → `binanceGet` 的 transport | `tickersCache` 解析结果 **60 秒**；排名结果另有 5 分钟缓存 |
| 同文件 `binanceKlineChange`、`binanceRollingChange` | klines | 同上 | helper 本身无；部分调用者有排名/quant 结果缓存 |
| 同文件 `binanceBTC1hCloses`、`binanceBTC4hCloses` | klines | 同上 | 各自已收盘序列 **5 分钟**；4h 原有 in-flight 与失败退避保留 |
| 同文件 `binanceOIDelta` | openInterestHist | 同上 | helper 本身无；部分调用者有排名/quant 结果缓存 |
| `market/breakout/binance.go` `binanceDS.Klines` → `fetchKlines` | klines | `newScannerHTTP` 的 transport，Low 优先级 | 合约及现货的已收盘序列缓存至**下一根收盘 + 2 秒**；保留原逐位一致语义 |
| 同文件 `binanceDS.OIHistory` | openInterestHist | `fetchJSONCached` → `fetchBody` → scanner client | `oiHistCacheTTL` **4 分钟** |
| 同文件 `binanceDS.FundingInfo`（两处 URL） | premiumIndex、fundingRate | 同上 | 分别为 `premiumCacheTTL` **2 分钟**、`fundingCacheTTL` **10 分钟** |
| 同文件 `binanceDS.FundingHistory` | fundingRate | 同上 | `fundingCacheTTL` **10 分钟** |
| 同文件 `fetchBoardTickers` | ticker/24hr | 同上 | `tickerCacheTTL` **2 分钟** |
| `market/breakout/shortscan.go` `gainerTickerSnapshot` | ticker/24hr | 同上 | `tickerCacheTTL` **2 分钟**；扫描结果缓存也保留 |
| `market/breakout/slowtop.go` `allPerpTickers` | ticker/24hr | 同上 | `tickerCacheTTL` **2 分钟** |
| `market/breakout/shorttuner.go` `historicalShortHorizon`（两处 URL） | klines、fundingRate | `fetchJSON` → `fetchBody` → scanner client；完整 startTime/endTime | 无上述 `fetchJSONCached` 或序列缓存；原历史标签持久化保留 |
| `market/breakout/trendshort_shadow.go` `historicalTrendPath` | klines（各片段） | 同上 | 无上述序列缓存；原影子评估结果保留 |
| `provider/coinank/coinank_api/kline.go` `Kline` → `get` | /api/kline/list/open | provider client 的 transport | provider 本身无 |
| `market/data_klines.go` `getKlinesFromCoinAnk` | 上一行的免费 K 线 | latest 游标按 15s 对齐，再调用 provider | 本入口无 |
| `provider/coinank/kline.go` `CoinankClient.Kline` → `Get` | /api/kline/lists | provider client 的 transport；携带 apikey 时直接绕过 | provider 本身无 |

全仓扫描还找到以下范围外调用，按任务的目录限制保留原样：

- `api/handler_trending_flow.go`：`binanceTopVolumeSymbols`（ticker/24hr）、`oiValueDelta`（openInterestHist）、`symbolPriceChange`（klines），经其独立的 `binanceGetCached`。这些尚未接入本包。
- `docs/_review-attachment/` 中同名 URL 是审查附件，不属于构建包。
- trader 的 SDK 请求不在本任务修改范围。

## 集中 TTL 与安全边界

HTTP 叠加层的集中响应策略表在 `cache.go` 的 `endpointTTLs`，不按交易员区分。

| 精确路径 | 最大 TTL |
| --- | --- |
| /fapi/v1/klines | 15 秒 |
| /futures/data/openInterestHist | 60 秒 |
| /fapi/v1/ticker/24hr | 30 秒 |
| /fapi/v1/fundingRate | 10 分钟 |
| /fapi/v1/premiumIndex | 10 秒 |
| /api/kline/list/open | 15 秒 |
| /api/kline/lists（匿名） | 15 秒 |

TTL 从首次上游请求开始计时，包含 guard 等待及网络耗时；命中、完成、等待者都不会续期，不提供 stale fallback。
K 线若包含正在形成的 bar，过期时间进一步收紧到该 bar 收盘时刻，避免旧的部分 OHLC 在下一次采样被当作已收盘最终值。
Binance 用响应 closeTime；CoinAnk 的 forming end=0 时由 openTime + interval 推导（UTC，月/年使用日历运算）。

仅未携带凭据、无 body 的 GET 可参与。签名、timestamp/recvWindow、listenKey、X-MBX-APIKEY、Authorization、Cookie、CoinAnk apikey 等全部绕过，连 singleflight 都不参与。
键为方法 + 完整 URL；query 参数名排序，保留重复值顺序以免改变上游含义。symbol、interval、limit、历史时间范围、host 等均隔离。
只保存 HTTP 200、完整且合法的 JSON；读取/关闭错误、超时、非 200、Binance 错误对象、CoinAnk success=false 不保存。
同时等待者共享当次响应或错误，结束后立即忘记失败，下一次可重试；等待者可单独取消，首请求的失败会传播给该次所有仍等待的调用者。
每个返回体、Header、Trailer 独立复制，保留原始行情时间戳。

默认最多 2000 条、64 MiB 响应体；单响应最大 8 MiB，超过即报错且不存储。
按访问/写入/Stats 惰性清理过期项，满容量时逐出最早到期项。
`Stats()` 提供总计和每端点 Hits/Misses/Coalesced、Entries、Bytes；Misses 是实际 fetch 次数，Coalesced 是并发等待者次数。
默认每 10 分钟一条 Info 累计汇总，逐端点列出命中率及节省比例 `(Hits+Coalesced)/总数`，不输出 query 或凭据。

depth、bookTicker、ticker/price、exchangeInfo、账户/订单等均未列入白名单。
实际额外发现 premiumIndex，按要求纳入 10s；未发现范围内直接调用瞬时 openInterest，保持不缓存。
spot K 线和 long/short ratio 未加入白名单，仍使用原先 scanner 局部缓存；合约 K 线、OI、ticker、premiumIndex、fundingRate 也保留表中原缓存。派生排名、扫描结果、情绪汇总、资金结算间隔等业务缓存保持原语义。

## 包装链及新鲜度

Binance：`http.Client → pubcache → bnweight → netmeter → 原 transport → 网络`。
`SafeHTTPClient` 本来返回 `netmeter.Wrap(transport)`；bnweight 在 base=nil 时也保留 netmeter 默认层。
CoinAnk：`http.Client → pubcache → http.DefaultTransport（现有 netmeter）→ 网络`。
上层缓存命中时整个 HTTP 链均不执行；pubcache 命中时两种 meter 均不执行。pubcache 包内集成测试同时断言底层调用、网络请求/字节、权重统计均无变化。

原有缓存决定调用方何时需要刷新；pubcache 决定该次刷新是否必须访问网络。两层独立计时，pubcache 不提前淘汰上层缓存，也不修改其时间戳或 TTL。原有数据解析、形成中 K 线过滤、freshness/quality 判断和后续策略逻辑保持原样。
例如 OI：同一符号首次获取后两分钟内仍由原 3 分钟解析缓存返回，pubcache 计数不变。解析缓存过期时，若其他调用者刚刷新过相同 URL 的原始响应且仍在 60 秒窗口内，本次刷新由 pubcache 供给；否则才访问网络。不能把“上层 TTL 不变”理解为原始响应年龄只受上层 TTL 约束，两层各自的有效期都会参与复用。

CoinAnk latest 原来每次使用不同毫秒 ts，完整 URL 天然不同。仅 market 的 latest 入口将 ts 向下对齐到 15s 窗口；窗口内共享同一 URL，下一窗口换键，游标最多落后 15s。显式历史 provider 请求完全不修改。

`market.SeriesStaleReason` 用 newest bar 的结束时间相对 SampledAt 判断，超过 3 个周期才判旧；gap 检查阈值是 6 个周期。
`kernel/signal_layer.go` 的 DataFreshness 沿用这些 bar 时间戳，DataQuality 判断实际返回的 bar 数量/完整性；缓存不会刷新行情时间戳或豁免旧源。
对于健康的最短 1m 数据，15s 远小于 3 周期阈值；跨收盘缓存提前失效，避免改变形成中/闭合的判断。
已增加 kernel 回归：缓存窗口内 freshness/quality 不误判，真正旧的源仍在双向 hard gate 被阻断。

## 此前没有独立缓存、现在新增覆盖的路径

- AI 周期及候选币数据的 K 线读取：`market/data.go` → `getExecutionKlines` / `getKlinesWithFallback` → CoinAnk 或 Binance direct，以及 `APIClient.GetKlines`。各交易员相同完整 URL 的读取可在底层共享；不改动 trader 目录。
- CoinAnk 免费 K 线，以及匿名 `CoinankClient.Kline`；带 apikey 的请求绕过。latest 入口对齐 15 秒窗口，显式 ts、startTime、endTime 保留原值并分键。
- `GetKlinesRange` 的相同历史分页 URL；breakout 的 `historicalShortHorizon` K 线/资金费查询及 `historicalTrendPath` K 线片段。
- kernel 的 `binanceKlineChange`、`binanceRollingChange`、`binanceOIDelta` 等 helper 自身没有独立 HTTP/解析缓存；原上游排名/quant 缓存未命中的请求现在也可共享。BTC closes、top tickers 本来已有缓存，不列为此前无缓存。
- OI 要区分路径：`fetchOIData` / `binanceGetJSON` 本身无解析缓存，kernel 的 `binanceOIDelta` 也没有；但 `market/data.go` 的正常 `Get*` → `fetchOIDataOrZero` → `getOpenInterestData` **原本就有 3 分钟缓存**，不能算新增无缓存覆盖。pubcache 只接住其过期刷新、并发未命中及其他直接 HTTP 读取。

预期流量：原缓存命中的调用继续零 HTTP；原本落网的相同 URL 请求由短 TTL 或 singleflight 合并，网络请求数只会减少或保持不变。不同 query、历史范围及凭据请求不混用。

## 验证记录

第 2 轮环境：`GOCACHE=$PWD/.gocache GOPROXY=off`，无网络、禁止监听端口；没有 commit/push。

| 验收命令 | 结果 |
| --- | --- |
| `go build ./...` | 通过 |
| `go vet ./market/... ./kernel ./provider/...` | 通过 |
| `go test ./market/... ./kernel ./provider/... -count=1` | 未全绿：既有端口/外网依赖失败；market 主包、kernel、pubcache、新增测试通过 |
| `go test ./market/... -race -run 'Cache\|Pub' -count=1` | 通过 |
| `git diff --check` | 通过 |

叠加层集成测试（全部使用假的 RoundTripper，不监听端口、不访问上游）：

- `market/pubcache_overlay_test.go`：真实 `getOpenInterestData` → `fetchOIData` → `NewAPIClient` 路径；30s、90s、120s 读取均由原 3 分钟缓存返回，网络请求总数为 1，pubcache Hits/Misses/Coalesced 均不增加。另构造“解析缓存已过期、其他调用刚预热底层”的状态，确认重新解析命中 pubcache，网络计数不增加。
- `kernel/pubcache_freshness_test.go`：ticker 解析缓存 59s 时，即使 30s pubcache 已过期也不调用 HTTP；61s 后刷新。保留原 freshness/quality 回归。
- `market/breakout/pubcache_test.go`：合约已收盘序列跨越 15s pubcache TTL 仍由原缓存返回；至下一根收盘加 slack 才刷新；形成中 bar 过滤和独立拷贝保持不变。
- `provider/coinank/coinank_api/pubcache_test.go`：保留失败重试和独立解析测试；新增两个未对齐的显式历史 ts 分键测试，确认时间戳不被 latest 15 秒对齐改写。
- 原四个测试文件与 `origin/dev` 逐字相同：`market/oi_cache_test.go`、`market/breakout/cache_test.go`、`kernel/hermetic_market_test.go`、`kernel/review_batch3_20261006_test.go`。

本轮日志：`/private/tmp/fapi-pub-round2-{build,vet,test,race,baseline-test,offline-test}.log`。HEAD 基线为 `3b87a93381a5055b499542106b8a77774a2d3cec`（与 `origin/dev` 相同）；以 `git show HEAD:<file>` 生成 9 个原有 Go 文件的临时副本，Go `-overlay` 替换修改文件并隐藏全部 7 个新增 Go 文件，不切换或修改工作树。overlay 路径记录在 `/private/tmp/fapi-pub-round2-baseline-overlay-path`。

完整测试中的环境失败：

- `market/bnweight.TestWrapAgainstHTTPTestServer`、`market/breakout.TestFix3ShortTunerMergeKeepsConcurrentAppends`：`httptest ... bind: operation not permitted`；首个 panic 会中止各自的包。
- CoinAnk 免费 API 的 `TestBaseCoinSymbolsNoArgs/BTC/BTCUSDT`、`TestKline`、`TestKlineDaily`：代理 `127.0.0.1:7890` 连接被禁止；`TestDepthWs`、`TestKlineWs`：上游 DNS 不可达。
- Hyperliquid 的 `TestGetCandles_BTC/TSLA/StockPerps`、`TestGetAllMids`、`TestGetAllMidsXYZ`、`TestGetMeta`：同样的代理连接权限失败。

HEAD 基线与当前运行的 **15 个失败测试、4 个失败包完全一致**；监听权限错误 2 处、代理连接权限错误 14 处、CoinAnk WebSocket DNS 错误 2 处的计数也一致。比较明细在 `/private/tmp/fapi-pub-round2-baseline-compare.txt`。排除下面列出的既有环境依赖项后，所有包回归通过（kernel 81.022s）。

排除 5 个既有 listener 测试及上述 13 个在线测试的命令（不改测试文件）：

```sh
GOCACHE=$PWD/.gocache GOPROXY=off go test ./market/... ./kernel ./provider/... -count=1 -skip '^(TestWrapAgainstHTTPTestServer|TestFix3ShortTunerMergeKeepsConcurrentAppends|TestFix05HistoricalLabelDoesNotStretchAfterDowntime|TestFix05ScannersIgnoreFormingCandles|TestFix8ShortTunerLegacyLabelMigrates|TestBaseCoinSymbolsNoArgs|TestBaseCoinSymbolsBTC|TestBaseCoinSymbolsBTCUSDT|TestDepthWs|TestKline|TestKlineDaily|TestKlineWs|TestGetCandles_BTC|TestGetCandles_TSLA|TestGetCandles_StockPerps|TestGetAllMids|TestGetAllMidsXYZ|TestGetMeta)$'
```


## 原有文件差异核对（相对 origin/dev）

逐个使用 `git diff origin/dev -- <file>` 核查；未列出的原有文件没有差异。

| 原有文件 | 保留的差异及原因 |
| --- | --- |
| `kernel/engine_data_binance.go` | 仅 pubcache import 和 `binanceGet` 的 Wrap 接线 |
| `market/api_client.go` | 仅 pubcache import 和 `NewAPIClient` 的 Wrap 接线 |
| `market/breakout/binance.go` | 仅 pubcache import 和 `newScannerHTTP` 的 Wrap 接线 |
| `market/breakout/cache.go` | 仅 `bodyCacheGet` / `bodyCachePut` 防御性字节拷贝 |
| `market/data_klines.go` | 仅 latest CoinAnk 游标对齐 15 秒及对应 import/注释，按第 2 轮要求保留 |
| `market/historical.go` | 仅 pubcache import 和分页 client 的 Wrap 接线 |
| `market/sentiment.go` | 仅 pubcache import 和 `sentimentHTTPGet` 的 Wrap 接线 |
| `provider/coinank/coinank_api/kline.go` | 仅 pubcache import 和 provider client 的 Wrap 接线 |
| `provider/coinank/coinank_http.go` | Wrap 接线；apikey 为空时不加头，按第 2 轮要求保留 |

`git diff origin/dev --stat`（Git 不计入尚未跟踪的新增包、README 和测试；没有暂存或提交）：

```text
 kernel/engine_data_binance.go         | 3 ++-
 market/api_client.go                  | 3 ++-
 market/breakout/binance.go            | 3 ++-
 market/breakout/cache.go              | 4 ++--
 market/data_klines.go                 | 6 +++++-
 market/historical.go                  | 3 ++-
 market/sentiment.go                   | 3 ++-
 provider/coinank/coinank_api/kline.go | 4 +++-
 provider/coinank/coinank_http.go      | 8 ++++++--
 9 files changed, 26 insertions(+), 11 deletions(-)
```
