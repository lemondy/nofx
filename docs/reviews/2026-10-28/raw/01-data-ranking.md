## A. 确认的 Bug

本次确认 **10 项 P1、1 项 P2**。未确认可直接定为 P0 的问题。以下数值反例均通过离线 Go 测试复现；测试代码通过 `/tmp` 中的 overlay 加载，没有写入仓库。

**A1. 小时 OI 被当成分钟级新观测，15m 激增风控会漏检｜P1**

**位置：**[market/data.go:663](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/market/data.go:663)、`market/data.go:650–651`、`market/oi_risk.go:95–119`；使用方为 `kernel/volatile_gate.go:184–193`。

**触发场景：**小时历史接口最后一个观测为 OI=100；接下来半小时真实 OI 已升至 150，但接口仍返回该小时观测。程序每隔约三分钟将这个旧值重新记录为当前观测，最终返回 `15m变化=0，ok=true`，激增门槛不会触发。小时更新时，又可能把整小时变化误记为最近 15 分钟变化。

**证据：**

```go
// 取的是小时历史，且响应结构未保留 timestamp
binanceGetJSON("/futures/data/openInterestHist?symbol="+symbol+"&period=1h&limit=30", &hist)

// 使用本地抓取时间重新记录旧观测
recordOISampleLocked(symbol, oiNow(), cp.Latest)
```

```go
v := (last.value/ref.value - 1) * 100
```

**修复建议：**15m 风控使用实时 OI 或 5m 历史，保留并校验交易所观测时间；同一源时间戳不得重复采样。观测不足或过期应返回未知。

---

**A2. 执行侧辅助函数忽略采样时间，把旧 forming bar 当成已收盘｜P1**

**位置：**[kernel/regime_line.go:33](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/kernel/regime_line.go:33)、`kernel/regime_line.go:64`；调用方 `trader/auto_trader_risk.go:2864、2890`。

**触发场景：**11:59:30 获取快照，其中 11:00–12:00 K 线尚未收盘；模型返回后，12:00:05 执行检查。`ComputeSymbolSignals` 会尊重 `SampledAt`，但 `RegimeLine`、`ExecutionMicroTrend` 使用执行时钟，误把快照中未完成的 K 线纳入计算。复现中，同一快照的 EMA50 从采样时应使用的 100 变成 101。

**证据：**

```go
j := len(ClosedKlines(td, now, market.TimeframeDuration(tf))) - 1
```

```go
tfs[tf], _ = computeTFSignal(tf, td, now, data.CurrentPrice)
```

**修复建议：**两个入口统一先调用 `snapshotNow(data, now)`。需要最新已收盘结果时重新获取数据，不能仅推进旧快照的判定时钟。

---

**A3. 显示窗口截断了指标暖机历史，趋势分类随显示条数改变｜P1**

**位置：**[market/data_klines.go:184](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/market/data_klines.go:184)、`market/data.go:355–399`、`kernel/signal_layer.go:2523、2580–2586`。

**触发场景：**获取 200 根已收盘 K 线，只保留最近 60 根供展示。前 140 根收盘为 100，后 60 根为 200：

- 全历史暖机 EMA20/EMA50：`199.753363 / 190.931108`，属于上升趋势。
- kernel 在截断后的 60 根上重新计算：`200 / 200`，分类为 `range`。

配置中的显示条数因此能改变方向门槛；已有预计算 EMA 又与 kernel 重算值不一致。

**证据：**

```go
start := len(klines) - count
for i := start; i < len(klines); i++ {
    data.Klines = append(data.Klines, KlineBar{ /* ... */ })
}
```

```go
kb := barsToKlines(klines)
fast := market.ExportCalculateEMA(kb, fastP)
slow := market.ExportCalculateEMA(kb, slowP)
```

**修复建议：**内部指标历史与展示窗口分离，或直接消费完整历史计算、按收盘时间对齐的指标序列。增加“改变展示 count 不改变同一时点信号”的一致性测试。

---

**A4. ATR 存在两个不同种子，同组数据产生不同风控尺度｜P1**

**位置：**[kernel/signal_layer.go:2891](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/kernel/signal_layer.go:2891)、`kernel/signal_layer.go:2923–2926`、`market/data_indicators.go:109–112`。

**触发场景：**15 根 K 线，第一根 `High−Low=20`，后面 14 根 TR 均为 2、收盘均为 100。结果为：

| 实现 | ATR14 |
|---|---:|
| `market.calculateATR` | 2.000000 |
| `kernel.wilderATRSeries` 最后值 | 2.000000 |
| `kernel.wilderATR` | 3.193878 |

这会直接改变 `ATRPct`、止损缓冲、止损带及 R:R 判断，薄历史时尤其明显。

**证据：**

```go
// kernel.wilderATR：把第 0 根无前收盘价的范围纳入种子
tr[0] = bars[0].High - bars[0].Low
a := tr[0]
```

```go
// market 与 kernel 的 ATRSeries：从 TR[1] 开始
for i := 1; i <= period; i++ {
    a += tr[i]
}
a /= float64(period)
```

**修复建议：**统一成一个 ATR 实现及有效起始索引；明确历史不足时返回未知。优先让标量值等于同一序列实现的最后一个有效值。

---

**A5. 最小可交易金额没有对齐 MIN_NOTIONAL 所需数量｜P1**

**位置：**[market/symbol_lot_filter.go:97](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/market/symbol_lot_filter.go:97)；使用方 `kernel/signal_layer.go:1361`。

**触发场景：**`stepSize=0.1、minQty=0.1、MIN_NOTIONAL=5、price=26`。函数返回最低 5 USDT，但按 5 USDT 计算的数量向下取整后只有 0.1，名义金额为 2.6 USDT，无法下单。实际最低数量是 0.2，金额为 5.2 USDT。

**证据：**

```go
minNotional = math.Max(minNotional, f.MinQty*price)
steps := math.Ceil(f.MinQty/f.StepSize - 1e-9)
minNotional = math.Max(minNotional, steps*f.StepSize*price)
```

这里只对 `MinQty` 取整，没有对 `MinNotional/price` 取整。

**修复建议：**先计算 `max(minQty, minNotional/price)`，再按 stepSize 向上对齐，最后转换为金额；数量对齐使用十进制定点运算。

---

**A6. R:R 去重会丢弃真实结构位，错误判定无可用目标｜P1**

**位置：**[kernel/signal_layer.go:2301](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/kernel/signal_layer.go:2301)、`kernel/signal_layer.go:2364`。

**触发场景：**做多入场 100、止损 98、最低 R:R=2.5。真实摆动高点为 105，布林带补充价位为 104.99。二者落在 0.05% 去重容差内，排序后先保留 104.99，再丢弃 105。由于 104.99 是布林带来源，不能成为正式目标，最终返回 `Usable=false`，尽管 105 正好满足 R:R=2.5。

**证据：**

```go
for _, t := range targets {
    if !inList(uniq, t, entry) {
        uniq = append(uniq, t)
    }
}
```

```go
if minRR > 0 && !out.Usable && v >= minRR-1e-9 && !bollT[t] {
```

**修复建议：**聚类去重时保留来源元数据，真实结构位优先；避免用动态布林带代表并替换邻近结构位。去重后仍需保证合格目标没有因阈值边界被删除。

---

**A7. 排名缓存缺少请求参数，结果由先调用的策略决定｜P1**

**位置：**[kernel/engine_data_binance.go:405](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/kernel/engine_data_binance.go:405)、`kernel/engine_data_binance.go:492–499`。

**触发场景：**

- 策略 A 先请求价格排名 `1h/top5`，策略 B 随后请求 `4h/top20`，却收到 A 的缓存，缺少 4h 数据。
- OI 排名先请求 `1h/top5`，随后请求 `1h/top20`，仍只返回 5 条。
- `fillPriceRankingOIFromRanking` 请求深度 150，也会命中较浅缓存，使价格榜的 OI 补全不完整。

**证据：**

```go
// OI 只检查 duration，没有 limit
oiRankCacheKey == duration
```

```go
// 价格缓存没有检查 durations 或 limit
if priceRankCache != nil && time.Since(priceRankFetched) < rankingCacheTTL {
    return data, nil
}
```

**修复建议：**缓存完整排名，按请求返回独立裁剪副本；或者将规范化后的周期集合、limit、宇宙版本全部纳入缓存键。

---

**A8. 同名涨跌幅使用不同时间窗口，价格/OI 联合解释失真｜P1**

**位置：**[kernel/signal_layer.go:3130](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/kernel/signal_layer.go:3130)、`kernel/engine_data_binance.go:132–164、424、515`。

**触发场景一：**12:14，11:00 收盘为 100，11:15 收盘及当前价均为 110。信号层删除 forming bar 后回看四根，得到 **+10%**；排名层在包含 forming bar 的五根数组中取首根，得到 **0%**。二者声称使用同一 rolling 60m 定义，实际相差一根 15m K 线。

```go
closes := closedCloses(data.TimeframeData[bestTf], now, bestDur)
idx := len(closes) - 1 - barsBack
```

**触发场景二：**12:01，当前 4h K 线开盘价 110、现价 111，而 08:01 价格为 100。4h 排名显示 **+0.91%**，实际滚动四小时为 **+11%**；每逢新 4h 周期开盘，该指标接近重置。

```go
// 请求当前一根 K 线
"/fapi/v1/klines?symbol="+symbol+"&interval="+interval+"&limit=1"
return (close_ - open) / open * 100, close_, nil
```

OI 排名也把两个历史 OI 样本的变化，与当前 forming candle 的开盘至今涨跌配对，二者并非“同一窗口”。

**修复建议：**统一按目标时间戳计算收益；OI 联合分析使用与两个 OI 观测时间匹配的价格。若保留“本根 K 线开盘至今”指标，应单独命名。

---

**A9. data/funding 独立限频桶仅统计，不参与准入｜P1**

**位置：**[market/bnweight/weights.go:45](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/market/bnweight/weights.go:45)、`market/bnweight/meter.go:257–263、318–335、365–366`。

**触发场景：**OI、账户多空比及资金费率请求集中发生，而 IP 权重仍较低。离线模拟中，同一时刻 **1001 个 data 请求及 501 个 funding 请求全部无等待通过**，已超过仓库自行声明的五分钟预算。

**证据：**

```go
return endpoint, 0, BucketData
return endpoint, 0, BucketFundingRate
```

```go
used := m.effectiveUsedLocked(now) // 准入只检查 IP 权重
```

```go
if bucket == BucketIP1M {
    m.ownWeight += weight
}
```

**修复建议：**准入时原子检查并预留对应桶的预算，按桶独立等待或拒绝低优先级请求。服务器当前实际配额未联网核验，但客户端未执行自身声明预算这一点已确认。

---

**A10. 做空研究样本把旧小时收盘价当成当前入场价｜P1，影响研究结果**

**位置：**[market/breakout/shortscan.go:193](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/market/breakout/shortscan.go:193)、`market/breakout/shorttuner.go:150–154、294`。

**触发场景：**12:45 采样，最近完整小时收盘价为 100，实时价格已为 110；24 小时后仍为 110。样本把时间记为 12:45、入场价记为 100，计算净做空收益 **−10.2%**；若按采样时的 110 入场，净收益应为 **−0.2%**。

**证据：**

```go
Price: c1h[len(c1h)-1], // 已收盘小时价格
```

```go
TS:    now.UnixMilli(),
Price: sig.Price,
```

```go
s.Outcome = (s.Price-res.price)/s.Price*100 + res.funding - btCostRoundTrip
```

`trendshort.go:178` 与 `trendshort_shadow.go:127–130` 也存在同类价格、采样时间配对。

**修复建议：**指标继续使用闭合 K 线；收益标签的入场价使用采样时可执行报价，并记录其观测时间。旧标签需版本化重算。当前研究提案不会自动发布实盘参数，因此不将其描述为直接错误下单。

---

**A11. 扫描器把完全横盘判为 RSI=100｜P2**

**位置：**[market/breakout/indicators.go:82](/Users/zhangyun/workspace/nofx/.claude/worktrees/quant-system-review-optimization-1428c0/market/breakout/indicators.go:82)、`market/breakout/shortscan.go:565–568`。

**触发场景：**60 根收盘价均为 100，但存在正常成交量及盘中高低波动。扫描器返回 RSI=100；行情层返回中性 50。同一标的因此在不同模块呈现不同动量状态。

**证据：**

```go
if al == 0 {
    return 100
}
```

没有先处理 `ag == 0 && al == 0`。

**修复建议：**明确平盘退化值并统一标量、序列实现。项目若选择中性 50，应统一测试；若要求完全兼容 TA-Lib，应按其约定处理，不能只修一处。

## B. 疑似问题（需进一步验证）

| 疑似问题 | 位置与具体风险 | 还需验证 |
|---|---|---|
| 多周期串行抓取跨收盘边界 | `market/data.go:363–409` 在全部周期获取后才记录 `SampledAt`。较早获取的 forming bar 可能在后续请求完成时被视为闭合；pubcache 到期机制不能修正已经返回的响应。 | 注入跨边界延迟，验证完整抓取链；应记录每个周期的观测时钟。 |
| 资金费率周期切换后年化失真 | `market/data.go:49–61` 缓存周期 24h，使用最近间隔中位数；`market/breakout/binance.go:240–253` 则使用均值。若刚从 8h 改为 1h，两者都可能继续使用旧周期，且结果不同。 | 用真实周期切换样本和交易所周期元数据核验；中位数不能保证代表当前周期。 |
| 价格排名缓存共享对象存在竞态 | `engine_data_binance.go:494–498` 解锁后返回缓存指针，并调用 `fillPriceRankingOIFromRanking`；后者在 `595、601` 原地写切片。 | 对并发缓存命中、OI 补全和渲染运行定向 `-race`；本次未运行竞态检测。 |
| 清算数据覆盖率可能被高估 | `liquidation_feed.go:90、278–280` 用进程运行时间表示覆盖窗口，没有扣除断连时间；代码注释还假定流覆盖每笔清算。 | 核对流的采样/丢失语义，以及 `z` 累计成交量是否可能重复计入；结合断连日志检查实际缺口。 |
| 因子文件可能接受未来日期 | `kernel/factor_ensemble.go:193–197` 只拒绝过旧日期，没有拒绝 `AsOf > now`。历史重放若读取较新的 live 文件，可能引入未来因子。 | 审查 Python 写入端的日期定义、训练截止日及历史运行的数据文件选择；当前读取端不足以证明不存在前视。 |

## C. 可复用的成熟方案（替代自写轮子）

| 仓库具体模块 | 建议 | 收益、成本与风险 |
|---|---|---|
| `market/data_indicators.go`、`breakout/indicators.go`、kernel 内重复指标 | 评估统一到 `go-talib` 等实现；先以 Python TA-Lib 建立离线黄金数据集。 | 收益高，能消除种子、暖机和退化值分叉。迁移成本中等；必须固定 EMA、RSI、ATR、MACD 的初始化及有效索引，避免指标切换导致阈值整体漂移。 |
| 三套 Binance REST 解析及过滤器处理 | 优先复用仓库已依赖的 `adshao/go-binance/v2` 类型化接口；数量精度可使用已有 `shopspring/decimal`。 | 减少 `interface{}` 强制转换和被忽略的解析错误。成本中等；仍需保留公共缓存、代理及分桶限频包装，SDK 不能替代这些跨调用方策略。 |
| `backtest.go`、`shorttuner.go` 的研究评估 | 因子诊断可用 Alphalens；批量信号筛选可用 vectorbt；收益报表可用 QuantStats/empyrical；统计推断可用 SciPy/statsmodels。 | 适合替换统计与报告层。直接更换整个实盘引擎成本和风险较高；这些工具也不会自动解决错误标签、历史宇宙和成交假设。 |

**这里自写是合理的：**

- `swingPivots`、流动性扫单、结构止损与 R:R 菜单承载本项目交易规则，保留自写合理，重点是统一数据语义与边界测试。
- `pubcache` 的公共接口白名单、forming candle 到期、跨客户端请求合并值得保留。
- `bnweight` 的交易请求优先级合理，应补全独立桶准入。
- 因子与调参结果保持研究用途、禁止直接自动发布实盘参数，这个边界应继续保留。

公式核对中，`market` 的 EMA 使用 SMA 种子，RSI 使用 Wilder 平滑，布林带使用总体标准差；不能把总体标准差误报为应改成样本标准差。`macd_hist` 实际是归一化 MACD 线，但 `kernel/engine_prompt.go:2129` 已明确解释，属于命名债务。

## D. 优化建议（按收益排序）

1. **先统一数据与指标契约，再继续调参。**  
   围绕 A1–A4、A8，为数据保留 `source_time、received_at、interval、closed`，内部计算历史与展示条数分离。建立跨模块一致性检查：同一时点、同一数据应得到相同 EMA/ATR；推进处理时钟不能改变旧快照的收盘状态。

2. **建立可重放的历史候选宇宙，消除研究样本的选择偏差。**  
   `scheduler.go:240` 用当前成交额前 30 名回测过去；`shorttuner.go:143–148` 只记录当时高分标的。`gainer_history` 已缓解“跌出今日涨幅榜就消失”，但不能补回历史未记录、退市或流动性衰退标的。建议每轮保存资格宇宙、失败标的、各来源标签及截断前分数，重放当时的候选形成过程。

3. **分别验证不同候选来源，避免混合分数失去可比性。**  
   `breakdownscan.go` 使用独立组合，且改写 `Components.Structure`；`shortscan.go:877–887` 最终又与顶部反转候选混排，`shorttuner.go:149–165` 未记录 Universe。建议按来源分别评估净收益、命中率及覆盖率，再校准到统一排序尺度。当前 0–100 分量在数值范围上大体一致，但同分并不代表同等预期收益。

4. **补充跨币相关性、多重试验及滚动样本外验证。**  
   `backtest.go:447–465` 已有标签窗口 purge，`569–587` 已去除同币重叠交易，值得保留；但 `tradeTStatistic` 仍把同期不同币交易视作独立样本。建议按日期或市场冲击聚类推断，使用块 bootstrap，并为反复阈值搜索保留固定最终测试集。`shorttuner` 的九分量显著性门槛也应连同反复运行次数一起评估。

5. **把固定成本信号研究推进到成交与组合验证。**  
   `backtest.go:34` 固定扣除 0.20%，本文件已明确不模拟成交、SL/TP 和组合限制。后续收益最大的扩展是接入实际点差、费用、滑点、限价成交率、资金费率结算及并发持仓约束；候选排序还应检查 BTC 暴露和相关币簇的风险集中。保持研究提案与实盘发布分离。

6. **优化重复计算与共享元数据，再增加扫描吞吐。**  
   `calculateTimeframeSeries` 对每个输出点反复重算 EMA/RSI/MACD，长窗口趋于二次复杂度；可改成一次递推。`GetSymbolLotFilter` 每个新币都拉取整份 exchangeInfo，却只缓存一个币，应整表共享。清算流逐事件扫描全局历史，也适合改成时间桶聚合。

7. **让排名输出包含覆盖率，并固定 top-N 的平分规则。**  
   `mapParallel`、`AnalyzeMany` 会丢弃失败标的，结果却仍获得新的抓取时间；应输出请求数、成功数、错误类型和源数据年龄。`shortscan`、`slowtop` 只按四舍五入后的分数排序，部分输入来自 map 或并发完成顺序；增加明确的次级排序键，并在截断前使用未舍入分数，减少候选抖动。

## E. 本次审查覆盖的文件清单与未覆盖部分

**重点阅读的生产代码：**

- `market/`：`data.go`、`data_klines.go`、`data_indicators.go`、`freshness.go`、`funding_history.go`、`oi_risk.go`、`sentiment.go`、`liquidation_feed.go`、`historical.go`、`historical_parse.go`、`timeframe.go`、`types.go`、`symbol_lot_filter.go`、`api_client.go`。
- 公共基础设施：`market/pubcache/cache.go`、`market/bnweight/meter.go`、`market/bnweight/weights.go`。
- `kernel/`：`engine_data_binance.go`；`signal_layer.go` 中指标、结构、止损、R:R、突破状态及采样时钟相关部分；`market_regime.go`、`regime_line.go`、`bb_ride.go`、`liquidity_sweep.go`、`factor_ensemble.go`。
- `market/breakout/`：`binance.go`、`indicators.go`、`breakout.go`、`breakdownscan.go`、`shortscan.go`、`slowtop.go`、`trendshort.go`、`gainer_history.go`、`cache.go`，以及 `params.go`、`scheduler.go`、`backtest.go`、`shorttuner.go`、`trendshort_shadow.go` 的相关路径。
- 外部客户端：CoinAnk 的 K 线 REST 解析、HTTP 及 WebSocket 路径；NofxOS 的 `client.go`、`coin.go`、`oi.go`、`price.go`、`ai500.go`、`netflow.go` 相关段落。
- 顺藤摸瓜检查了 `engine.go`、`engine_prompt.go`、`engine_analysis.go`、`volatile_gate.go`、执行侧部分调用位置及 `provider/openbb/client.go` 的 4h 聚合逻辑。

**验证结果：**

- 12 个 `/tmp` overlay 定向反例测试通过。
- `pubcache` 测试、选定 market/kernel 测试通过。
- `bnweight` 除本地监听测试外通过；`TestWrapAgainstHTTPTestServer` 因环境禁止监听端口而无法运行。
- 上述核心包及 CoinAnk/NofxOS 的 `go vet` 通过。
- 无联网、无依赖安装；本机未安装 TA-Lib，未做 TA-Lib 逐值运行对照。

**未覆盖：**完整下单与保护单生命周期、全仓并发竞态、React、Python 因子生成/训练流程、所有 CoinAnk 非 K 线接口，以及线上 API 现行契约与实际配额。

**工作区记录：**未执行仓库编辑或提交命令，测试均指定 `-mod=readonly`。结束时发现初始不存在的 `go.mod` 变动，仅涉及六项依赖从 indirect 移至 direct；来源无法确认，已保留，未回滚。