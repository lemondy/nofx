# 数据层 / LLM 层疑似问题核实

核实日期：2026-10-28；代码目录：`/Users/zhangyun/workspace/nofx-wt-ver-data`；HEAD：`aa8c361d51cce65b94c98826868223a9c1106109`。结论针对本地当前实现，未 fetch 或切换分支。已读取两份原始分报告的 B 节。

仅新增 5 个 `verify20261028_*_test.go` 临时验证文件，没有修改业务代码，没有 commit / stash / checkout。生产库均通过 `sqlite3 'file:/Users/zhangyun/workspace/nofx/data/data.db?mode=ro'` 打开；查询未涉及密钥值。日志只做定向 `rg` / `head` / `tail`。实盘在持续写入，不同 SQL 的结果不是同一事务快照。

**判定口径：CONFIRMED 指明确指出的代码行为或风险条件有复现/证据，不代表已经造成实盘损失。** 本次 10 项核心行为得到确认，1 项缓存共享竞态被当前实现和定向 race 测试否定；若原疑点包含更强的推断，下文单独收窄或否定。

验证日志及跨包测试夹具保存在 `/private/tmp/verify20261028-data/`。最终定向 Go 测试 15 个通过，其中排名测试另以 `-race` 通过；Python 写入端日期测试 1 个通过。未运行已知基线失败 `TestB1BreakevenTrailingSameCycle`，也未进行真实下单或付费模型请求。测试命令及清理记录见文末。

## 1. 多周期串行抓取跨收盘边界

**结论：CONFIRMED。** #17/#23 使用采样时钟的修复有效，但采样时钟本身仍可能晚于较早周期的实际取数时间，不能解决这一窗口。

**证据：**

- `market/data.go:370` 串行获取周期，`market/data.go:419` 在循环结束后才设置 `sampledAt := time.Now()`。每个 `TimeframeSeriesData` 没有独立采样时间。
- `TestVerify20261028FetchCrossBoundary` 对 `GetWithTimeframesForExchange` 注入 fake K 线响应，在第二个周期请求中延迟到真实分钟收盘后。第一个 1m 响应返回于 `15:26:15 UTC`，最后一根为 `15:26–15:27`、冻结收盘价 103；第二个 4h 请求延迟约 44.85 秒，最终 `SampledAt` 为 `15:27:00 UTC` 收盘后约 30 毫秒。同一条采样时钟赋值同时用于普通分析和执行行情分支；此测试使用 1m 以缩短等待，其他周期采用同一结束时间比较。
- `TestVerify20261028CrossBoundaryDownstream` 将该真实返回数据及显式保存的 `SampledAt` 送入当前 kernel 实现，输出：

```text
closed bars 59 -> 60
snapshotNow=2026-10-28T23:27:00+08:00
computeTFSignal BarsUsed=60 UnclosedDropped=false LastClose=103
```

`kernel/signal_layer.go:3098` 的 `snapshotNow` 确实尊重 `SampledAt`，但 `ClosedKlines`（3176）和 `computeTFSignal`（2473）只比较 K 线结束时刻与该时间，故仍纳入了取数时尚未完成的尾线。pubcache 后续过期无法更新已经返回的切片。

**实际影响：** 当前策略取数走 `fetchMarketDataWithStrategy` → `GetWithTimeframesForVenue`，仍使用同一多周期循环。`mac-nofx` 开启多周期、主周期为 15m，机制同样适用；可能影响趋势、ATR、结构位与硬门输入。已证明可触发条件，未统计线上跨边界发生频率，也未证明具体订单受此影响。旧 `GetWithExchangeAndPrice` 分支在第一组短周期返回后即记录时间（131），不要把这两条路径混为一谈。

**建议修复范围：** 保存每个周期的观测时钟或显式尾线闭合状态，并让所有下游消费者使用该周期的时间。不能通过统一推进整个快照时钟来把旧尾线升级为闭合。

## 2. 8h→1h 资金费结算周期切换后的年化失真

**结论：CONFIRMED（合成切换样本复现；未证明当前交易标的正处于切换）。**

**证据：** `market/data.go:49` 使用最近 6 次结算的间隔中位数，并缓存 24h（39）；`market/breakout/binance.go:240` 使用最近 4 次结算的有效间隔均值（253），其资金费历史响应缓存为 10 分钟（`market/breakout/cache.go:40` 附近）。两处都没有读取当前结算周期元数据。

固定每期费率 `0.01%`，切换后的真实周期为 1h，按项目简单年化公式，正确年化为 `0.0001 × 24 × 365 × 100 = 87.6%`。相对误差为“估计年化 / 87.6% − 1”。

| 路径 / 最近有效间隔（小时，旧→新） | 估计周期 | 输出年化 | 相对误差 |
|---|---:|---:|---:|
| market 中位数 `[8,8,8,8,1]` | 8h | 10.95% | −87.50% |
| market 中位数 `[8,8,8,1,1]` | 8h | 10.95% | −87.50% |
| market 中位数 `[8,8,1,1,1]` | 1h | 87.60% | 0% |
| breakout 均值 `[8,8,1]` | 5.6667h | 15.4588% | −82.35% |
| breakout 均值 `[8,1,1]` | 3.3333h | 26.28% | −70.00% |
| breakout 均值 `[1,1,1]` | 1h | 87.60% | 0% |

`TestVerify20261028FundingTransitionAndCache` 还将已缓存的 8h 估计与最新全 1h 历史并置，实际 `fundingIntervalHours` 仍返回 8h：

```text
fresh history all 1h, cached interval remains 8h (TTL=24h0m0s)
APR=10.95% vs 87.6% (-87.5%)
```

`TestVerify20261028FundingTransitionMean` 用 fake HTTP 调用真正的 `binanceDS.FundingInfo()`，复现表中均值结果。中位数自身的观测滞后和 24h 缓存滞后是两层问题；缓存若在切换前刚建立，可在剩余 TTL 内继续使用旧周期。

**实际影响：** `kernel/signal_layer.go:941` 用该周期年化，结果参与拥挤/费率解释和市价例外判断；`shortscan.go:308` 使用 scanner 周期，影响年化、评分及提示。生产 `mac-nofx` 已开启 `use_short_scan=true`，因此路径启用；当前是否有合约切换周期及其真实影响仍需交易所历史/元数据，不能由这些合成样本推定。

**建议修复范围：** 两条路径统一当前结算周期来源，优先采用交易所元数据；历史推断仅作有年龄标记的回退，并在观测到新间隔时使旧缓存失效。

## 3. 价格排名缓存命中与 OI 补全共享对象竞态

**结论：REFUTED（针对所述缓存别名竞态，当前 #21 修复已覆盖）。**

**证据：** `kernel/engine_data_binance.go:568` 的缓存命中分支先在锁内 `copyPriceRanking`，再解锁并对副本执行 `fillPriceRankingOIFromRanking`。`copyPriceRanking`（668）分别复制 map、duration 对象和 Top/Low 切片；OI 缓存也通过 `copyOIRanking`（533）返回独立切片。缓存未发布前的初次 OI 补全不与命中读者共享可写结果。

`TestVerify20261028RankingCacheConcurrentFill` 预置未填 OI 的价格缓存与有效 OI 缓存，启动 32 个 goroutine，每个执行 100 次缓存命中、清零调用者副本后再次强制 OI 补全、JSON 渲染及副本修改。输出：

```text
3200 concurrent cache hits + forced OI fills + JSON renders;
stored cache remains unmodified
PASS (go test -race)
```

Top/Low 两侧都执行写入，缓存原始 OI 始终为 0，symbol 始终为 BTCUSDT。race 链接阶段有 macOS `LC_DYSYMTAB` 警告，但测试退出码为 0，未出现 `WARNING: DATA RACE`。

**实际影响：** 单进程多交易员并发命中也未复现原指针共享问题。此结论只否定本条所述的缓存/OI 补全竞态，不等于证明整个排名子系统不存在其他竞态。

**建议修复范围：** 本条不需要新增业务修复；保留返回独立副本的契约，并将类似定向 race 覆盖纳入回归即可。

## 4. 清算覆盖窗口没有扣除断连

**结论：CONFIRMED。**

**证据：** `market/liquidation_feed.go:90` 在启动时记录 `liqStarted`，早于首次成功连接；`LiquidationStats`（278）用 `min(now-liqStarted,24h)` 填 `WindowHours`。重连循环（94）没有记录在线时段、扣除断连或回补缺失事件，返回字段的注释却称其为实际覆盖窗口。

日志命令：

```sh
rg 'liquidation feed (connected|disconnected)' \
  /Users/zhangyun/workspace/nofx/nofx-console.log
```

固定统计窗口为 **北京时间 2026-10-25 23:10:00 至 2026-10-28 23:10:00（连续 72h）**。日志本身从 10-25 19:11 开始，覆盖统计起点；提取结果保存在 `liquidation-events.log`。

| 日期 | disconnected 警告数 | connected 日志数 |
|---|---:|---:|
| 10-25（23:10 后） | 0 | 1 |
| 10-26 | 6 | 5 |
| 10-27 | 0 | 0 |
| 10-28（23:10 前） | 0 | 4 |

从第一条 disconnected 到随后第一条 connected 合并为以下 4 个可闭合区间；区间内重复失败不重复累计时长：

| 北京时间断连 → 重连 | 秒 |
|---|---:|
| 10-26 04:31:42 → 04:32:01 | 19 |
| 10-26 21:55:51 → 21:56:06 | 15 |
| 10-26 21:56:26 → 21:56:46 | 20 |
| 10-26 21:58:33 → 21:59:14 | 41 |
| 合计 | **95** |

6 条警告包括区间内再次失败的连接尝试；不能称为 6 个独立停机区间。95 秒是日志可观测缺口的下界，不含连接真正失效至被检测到的时间，也不能从两条无相邻断连的 connected 日志之间推算停机时长。重启会清空内存窗口，不应把跨重启的整个日志间隔算成断连。

**实际影响：** 当前实盘确实使用该 Binance 公共流，近三天有数据缺口证据，故相应进程中的 `WindowHours` 会高估在线覆盖时间。最后一次记录连接为 10-28 22:55:04，至本次统计截止未见新断连；不能把 10-26 的缺口直接算进当前新进程窗口。覆盖窗口是 prompt 的清算背景信息，尚无证据证明因此错误下单。

**建议修复范围：** 分开表示观察跨度、实际在线时长和缺口，首次连通后再启动覆盖计时；断连窗口明确标为不完整，避免把进程年龄称作连续覆盖。

## 5. 因子 live 文件接受未来 as_of

**结论：CONFIRMED（读取端）；当前写入文件超前的推断被证据否定。**

**证据：** `kernel/factor_ensemble.go:149` 只解析日期；`computeFactorEnsemble`（193–197）只检查是否早于 UTC 今日减 3 天，没有上界。

`TestVerify20261028FactorFutureAccepted` 用合法 fixture 写入 `as_of=2099-01-01`，调用真实读取和计算路径：

```text
now=2026-10-28T12:00:00Z accepted AsOf=2099-01-01
```

当前工作树下未发现 live 文件；生产目录仅发现：

```text
/Users/zhangyun/workspace/nofx/data/factor_mining/crypto/live_scores.json
as_of       = 2026-10-26
generated_at= 2026-10-27T18:53:05.864141+00:00
文件 mtime  = 2026-10-28 02:56:54.880225（本地时间）
```

写入端 `scripts/factor_mining/live_scores.py:33` 将 `now` 转成 UTC；38 行只保留 panel 日期 `< generated.normalize()`；41 行取其中最大日期，44 行格式化为 `YYYY-MM-DD`。所以 as_of 指标的是**已结束的 UTC 日线的日期**，不是生成日，也不是预测标签的未来到期日。`generated_at` 与 mtime 不相同本身不构成日期超前。

已有写入端测试 `LiveScoresTests.test_as_of_excludes_today_and_future_using_utc` 在生产项目现有 `.venv/bin/python` 下通过，覆盖 UTC 和 UTC+8 的跨日情况。系统 Python 首次运行因缺 numpy 失败，切换现有虚拟环境后成功；未安装依赖、未改 Python 代码。

**实际影响：** 当前文件日期正常，尚无实盘前视证据。主交易员配置未开启该字段；paper“因子 ensemble 参考字段-候选”开启，其他核查 A/B 对照关闭。读取端确实缺防线，未来文件或历史时钟重放时可能读到超前因子；是否存在实际历史重放调用链未在本任务中证实。该字段本身是参考证据，不能直接改变程序硬门。

**建议修复范围：** 读取端按调用方 UTC 采样日增加日期上界，历史回放按当时可用的版本读取；保留写入端现有的闭合日线过滤。

## 6. 各 provider 实际温度与 400 兼容性

**结论：CONFIRMED（存在按模型而定的兼容性风险）；“所有兼容 provider 固定 0.5”和“所有 GPT-5.x 必报 400”均不成立，当前生产未见温度 400 证据。**

**证据：** `TestVerify20261028ProviderTemperature` 实例化各实际 provider 构造器，读取最终 hook 产生的请求体；结果如下。生产构造调用没有传 `WithTemperature`，表中是当前默认决策调用实际采用的值，并非服务端有效采样温度。

| provider | 决策请求 temperature |
|---|---|
| openai | 0.5 |
| deepseek | 0.5 |
| qwen | 0.5 |
| glm | 0.5 |
| **kimi** | **1.0**（`mcp/provider/kimi.go:37` 已覆盖） |
| minimax | 0.5 |
| grok | 0.5 |
| gemini | 0.5 |
| custom | 0.5 |
| claude | **不发送该字段** |

`mcp/client.go:261` 非流式、929 流式都取 `Cfg.Temperature`；配置默认值来自 `MCPClientTemperature=0.5`（24），支持构造选项覆盖，并非硬编码到不可修改。通用 Request 路径（659）允许 `req.Temperature` 覆盖；Claude 简单决策路径（`mcp/provider/claude.go:97`）省略，Request 路径则仅在显式指定时发送（157）。不能仅凭省略推断所有 Claude 模型最终都按同一个有效温度采样。

只读 SQL（仅列非敏感字段）：

```sql
SELECT t.name,t.is_running,m.provider,m.custom_model_name,m.enabled
FROM traders t LEFT JOIN ai_models m ON m.id=t.ai_model_id
ORDER BY t.is_running DESC LIMIT 35;
SELECT provider,name,custom_model_name,enabled FROM ai_models LIMIT 30;
```

结果：7 个运行交易员（mac-nofx、美股大师、us-stock、两组 paper A/B 的 4 个交易员）均绑定 **glm / glm-5.3-flash**。模型表另有启用的 `deepseek / deepseek-v4-flash` 和 `openai / gpt-5.6-luna`，本次没有运行交易员绑定二者。其余 provider 未启用。`production-config.txt` 保存结果，没有查询 API key/secret 的值。

兼容性判断结合了 OpenAI Docs 技能及官方文档：

- OpenAI 的 [GPT-5.4 参数兼容说明](https://developers.openai.com/api/docs/guides/latest-model?model=gpt-5.4) 明确：旧 `gpt-5/gpt-5-mini/gpt-5-nano` 发送 temperature 会报错；`gpt-5.4`/`gpt-5.2` 在 reasoning 非 `none` 时同样不支持。当前代码会原样发 0.5，选择受限模型时存在请求失败风险。**但当前默认 OpenAI 模型 gpt-5.4 的 reasoning 默认是 none，不能据此认定其默认调用必报 400。** 本项目简单决策请求没有显式设置 reasoning effort。
- 对数据库里的 `gpt-5.6-luna` 没有做真实服务调用，也不能拿 5.4 的条件替代该型号契约，因此其确切接受行为仍未知；当前没有运行交易员使用它。
- 智谱 [GLM-5.3-Flash 官方说明](https://docs.bigmodel.cn/cn/guide/models/vlm/glm-5.3-flash) 推荐温度 1，不等于禁止 0.5。当前 mac-nofx 最近成功记录如 id=17881/17879/17876，`success=1`，均有非空模型响应，不能把这项描述为当前 GLM 调用故障。
- DeepSeek [思考模式官方说明](https://api-docs.deepseek.com/guides/thinking_mode/) 明确不支持的 temperature 为兼容旧客户端会被忽略而非报错；“请求里发了 0.5”不等于服务端一定按 0.5 工作。

**实际影响：** 当前所有运行交易员用 GLM，未证实温度导致 400。风险主要在切换到受限 OpenAI 模型或由上游设置非 none reasoning 时；custom 端点也可能有自己的限制，不能笼统保证。以上是请求体和官方契约核验，没有伪造“真实 API 已复现 400”的结论。

**建议修复范围：** 按模型能力与 reasoning 配置决定是否发送温度，记录实际请求参数；保留 Kimi 的覆盖并避免把 provider 名当成完整的模型兼容性判断。

## 7. BTC 头行指标包含 forming K 线

**结论：CONFIRMED（形成中数据参与指标计算）；“整个 prompt 宣称所有内容都只用已收盘 K 线”的表述需收窄。**

**证据：** `market/data.go:213` 对形成中的 3m 尾线应用实时价，221–222 随即对含尾线的完整序列计算 MACD / RSI7；没有闭合过滤。`TestVerify20261028BTCFormingIndicators` 调用真实 `GetWithExchangeAndPrice`，只改变实时价，所有闭合 K 线不变：

```text
live=104 -> MACD=0.1768438995 RSI7=59.87306230
live=150 -> MACD=3.8463595690 RSI7=92.82646127
```

`kernel/engine_prompt.go:528` 的 BTC 头行直接使用 `Data.CurrentMACD/CurrentRSI7`。多周期路径（`market/data.go:478`）也对完整 primaryKlines 计算 Current 字段，不能默认它们是 closed-only 或一定是 3m：生产 mac-nofx 主周期实际为 15m。

但 `closed candles only` 实际位于 `engine_prompt.go:2174` 的 **Structured Signal 块标题**，相应 TF 指标通过 `computeTFSignal` 丢尾线；它不是整个 user prompt 的总声明。应区分头行 current 指标与块内闭合指标；本条不能证明结构化指标因此被污染（第 1 项跨采样边界是另一问题）。

只读样本查询以最近 500 个 id 为边界、最多返回 3 行：

```sql
SELECT d.id,t.name,d.timestamp,
       substr(d.input_prompt,instr(d.input_prompt,'BTC:'),165) AS btc_header
FROM decision_records d JOIN traders t ON t.id=d.trader_id
WHERE d.id>(SELECT max(id)-500 FROM decision_records)
  AND t.name='mac-nofx' AND instr(d.input_prompt,'BTC:')>0
ORDER BY d.id DESC LIMIT 3;
```

实盘 id=17845、17843 含 BTC 头行，分别显示 `MACD=-136.2712, RSI7=29.17` 与 `MACD=-128.8472, RSI7=30.30`。本次抽取窗口内 mac-nofx 131 条记录有 2 条 BTC 头行；这是条件性出现，不是每周期固定出现。SQL 的数值仅证明已渲染该字段，没有反推对应 K 线是否正形成中。

**实际影响：** BTC 进入 `MarketDataMap` 时，LLM 可看到包含未完成主周期的背景指标；头行未标清周期/形成状态，可能与结构化闭合指标不一致。它不直接替换程序硬门；未证明具体决策因此改变。执行侧旧 3m 路径的 Current 指标同样有该行为，但不要把生产主策略的头行一律称为 3m。

**建议修复范围：** 明确区分实时与闭合指标并标注周期；如期望头行也遵循闭合口径，改为复用结构化闭合指标，避免维持两套含义不明的数值。

## 8. 批内重复与同币开平冲突

**结论：CONFIRMED。排序和已有仓位检查只能部分兜底。**

**证据：** `parseFullDecisionResponse`（`kernel/engine_analysis.go:457`）逐条 `validateDecision`；`engine_position.go:90/99` 没有批次内同币去重、动作互斥或模拟状态更新。

`TestVerify20261028DecisionBatchParsing` 将三组真实 JSON 走 `extractDecisions → parseFullDecisionResponse → validateDecision`，所有两条动作均保持可执行：

```text
duplicate_open : open_long/open_long  （初始空仓）
open_plus_close: open_long/close_long （初始有仓）
duplicate_close: close_long/close_long（初始有仓）
```

该测试把**校验后的**批次保存为夹具，`TestVerify20261028ValidatedBatchSortingAndFakeExecution` 读取并调用真正的 `sortDecisionsByPriority`（`trader/auto_trader_loop.go:1134`），再交给即时更新仓位的 fake 执行账本：

| 批次 | 实际排序 | fake 成功/拒绝 | 最终状态 |
|---|---|---|---|
| 重复开多 | open_long, open_long | 开 1，拒绝 1 | 有多仓 |
| 开多 + 平多 | close_long, open_long | 平 1，开 1 | 再次有多仓 |
| 重复平多 | close_long, close_long | 平 1，拒绝 1 | 空仓 |

`TestVerify20261028ActualSameDirectionGuard` 另外调用真实 `executeOpenLongWithRecord`，fake 交易所返回已有 BTC 多仓，确实得到：

```text
BTCUSDT already has long position, close it first; submits=0
```

实际同向拦截位于 `trader/auto_trader_orders.go:247`、540，执行前重新 `GetPositions`。批次执行是串行的（`auto_trader_loop.go:378`），但“先平后开”本身没有冲突消解语义：平仓成功且新快照确认空仓后，同批开仓可再次通过已有仓位检查；其他风控仍可能拒绝它。

**实际影响：** 模型返回上述批次时当前主交易员及 paper 均可受影响。正常新鲜仓位读可挡住第二次开仓，不能阻止同周期平掉后再开、重复平仓请求或重复的失败日志。本次没有验证最终一致/缓存滞后下是否会双开，也没有将 fake 排序结果冒充完整真实下单链的成功结果。

**建议修复范围：** 在解析后的批次层确定每个 symbol/side 的唯一意图，显式拒绝或合并冲突；执行侧的新鲜仓位检查继续保留作为第二道防线。

## 9. 规则验证无样本外显著性；普通 soft tag 被视为币名

**结论：CONFIRMED；当前生产规则为空，风险休眠。soft tag 缺陷的实际作用域是执行侧 lesson 日志匹配。**

**证据：**

- `kernel/rule_verify.go:24` 取最近 90 天 AI 已平仓总体，`RuleVerifyMinMatched=3`；129–163 只检查匹配数、净亏损和每笔净收益低于总体基准，没有独立 holdout、置信区间、显著性或多重检验。`api/handler_review.go:186` 把全部同一历史窗口重新传入验证，没有把生成规则使用过的样本排除。总体比“只看复盘亏损子集”更完整，但仍不是样本外验证。
- `TestVerify20261028RuleSupportedThreeSamples` 输入 3 个被条件选中的 −0.01 和 1 个未命中的 +0.01，得到：`Status=supported, Matched=3, Population=4, NetPnL=-0.03, BaselineNetPerTrade=-0.005`。因此 supported 不能解释为统计显著或未来有效。
- `TestVerify20261028SoftRuleGenericTags` 在 BTCUSDT / ETHUSDT / SOLUSDT 三币上运行 `MatchSoftRules`：

```text
breakout -> symbolLike=true, matches=0
rsi      -> symbolLike=true, matches=0
reversal -> symbolLike=true, matches=0
fomo     -> symbolLike=false, matches=3
BTCUSDT  -> symbolLike=true, matches=1
```

`kernel/rule_engine.go:355` 将长度 2–12 的字母数字标签视作 symbol-like，只排除少量词汇。`breakout/rsi/reversal` 与任何完整交易币对都不相等，导致通用标签没有匹配对象。

只读 SQL：

```sql
SELECT COUNT(*) total, COALESCE(SUM(enabled),0) enabled
FROM trading_rules LIMIT 1;
SELECT rule_type,enabled,count(*) n
FROM trading_rules GROUP BY rule_type,enabled LIMIT 10;
```

结果：`total=0, enabled=0`；分组查询无行。

**实际影响：** 当前没有启用规则，因此不影响现有实盘的规则执行。未来规则存在时，统计标签可能给人过强的有效性暗示。标签缺陷会漏掉 `trader/auto_trader_rules.go:87` 的 awareness lesson 日志；**不会使该 soft lesson 从 prompt 消失**，因为 `BuildRulesPromptText`（374）仍把所有启用 soft rules 渲染进去，并不调用 `MatchSoftRules`。soft rule 本来也不执行硬拦截。

**建议修复范围：** 将当前 supported 明确限定为历史描述并引入独立时间窗口验证；symbol 作用域与行为标签分字段表达，避免靠小排除词表猜币名。

## 10. 进入 prompt 的外部字符串白名单/长度限制

**结论：CONFIRMED（列出的主要来源均没有统一字符集/长度白名单；不等于已经发生恶意注入）。**

**证据：** `TestVerify20261028PromptExternalStrings` 把带换行、长度超过 3000 字节的标记分别置于 RulesText 来源的 lesson、情绪 Classification、CFTC ReportDate，调用真正的 `BuildUserPrompt`，三份标记完整保留；同时当前 Vergex AI500 规范化器保留 symbol 内部换行。来源及现有过滤如下：

| 来源 | 从输入到 prompt 的路径 | 现有过滤 / 缺口 |
|---|---|---|
| RulesText | 手工规则、导入或 AI 复盘 proposal → store → `BuildRulesPromptText`（`kernel/rule_engine.go:374`）→ `engine_prompt.go:760` | `api/handler_review.go:304` 校验必填、rule_type/on_violation 枚举与 hard condition 语法；未限制 Name/Description/LessonText/SourceStats 的字符集/长度，也未剥离换行和指令文本。hard 文本附带 MUST obey 前缀；不是所有 soft 文本都附同样前缀。生产规则表为空。 |
| 加密情绪标签 | Alternative.me `/fng/` 的 `value_classification` → `market/sentiment.go:167` → `Render`（472） | Value/时间戳转数值，SourceAt 由解析后的时间格式化；Classification 原样字符串，没有枚举/长度限制。当前实盘示例为 Neutral。 |
| 美股情绪的字符串 | FearGreedChart `/api` → `fetchStockFG`（`market/sentiment.go:178`）→ Render（482） | 分量 Val/Wt 为整数，分量 Name 和 Recent.Date 原样进入文本；MarketDate 没有严格日期解析，Components 也无单独数量/字符串长度白名单。Regime/TradeEffect、涨跌方向文字由程序生成，不同于第三方自由标签。 |
| CFTC ReportDate | OpenBB `/cftc/cot?provider=cftc...` → `provider/openbb/client.go:385` → `engine_prompt.go:519` | JSON 类型约束为 string；原样赋值 `r.Date`，没有按 YYYY-MM-DD 解析或长度/换行过滤。数值持仓字段用格式化数值输出。生产 prompt 中确有 CFTC 段落。 |
| **当前 AI500 symbol** | Vergex/browser relay 的 pair → `GetAI500Snapshot`（`provider/vergex/trending.go:126`）→ `NormalizeSymbol`（241）→ `kernel/engine.go:1452` 候选 → prompt 币名 | 去首尾空白、转大写、`market.Normalize` 删除 `_/-/-SWAP` 并补后缀，去空项和重复项、限制候选条数；**没有 symbol 长度或字母数字白名单，内部换行仍保留**。同文件 OI 用的 `tradableSymbol` 仅拒非 ASCII，且 AI500 没走它；ASCII 控制符也不因此被拒。 |
| 旧 NofxOS AI500 | `provider/nofxos/ai500.go:156` | 仅 TrimSpace/ToUpper/补 USDT；当前 engine 明确只用 Vergex，不能把旧 NofxOS 说成当前 AI500 上游。 |

AI500 输出测试例子：`BTC\nVERIFY_AI500_TEXT\nUSDT` 规范化后为 `BTC\nVERIFYAI500TEXT\nUSDT`。下游行情获取失败通常使该候选不渲染（`engine_prompt.go:803` 要求有行情），这是可达性上的限制，不是字符串白名单；本次没有证明任意畸形币名可以通过真实行情接口完整抵达模型。

新闻标题/URL 在 `engine_prompt.go:2175` 的边界明确移除，属于已做的过滤；不能因为新闻已剥离就推定上述其他字段也被处理。JSON 转义与 HTTP 响应总体字节上限也不能替代字段白名单或消除语义指令。

**实际影响：** 情绪、CFTC、AI500 当前在用，mac-nofx 的 `use_ai500=true, ai500_limit=5`；RulesText 当前为空。存在输入边界欠约束，未发现线上恶意内容，也未复现其突破执行硬门。应将“可保留外部指令文本”与“已成功操控交易”分开。

**建议修复范围：** 日期严格解析，symbol 按可交易宇宙/明确字符集和长度验证，情绪标签映射到封闭枚举；自由规则文本设字段长度和控制字符约束，并清晰标为低信任资料。

## 11. VolTarget 加仓邀请与 MarginUsage 文案

**结论：CONFIRMED（存在潜伏加仓文案矛盾和两套阈值表述）；“当前实盘启用加仓邀请”被只读配置否定，70% 与 90% 也不是两个相互冲突的执行硬门。**

**证据：** `kernel/engine_prompt.go:424` 仅在 `VolTargetEnabled=true` 时写入“<80% 时你可……评估加仓”；同向已有仓位却被真实开仓执行函数（`auto_trader_orders.go:247/540`）拒绝，第 8 项的实际执行函数测试也验证了这个拦截。

`TestVerify20261028VolTargetAndMarginPrompt` 生成真正 system prompt，验证：VolTarget 关闭时没有“评估加仓”，开启时出现；策略 `MaxMarginUsage=.9` 时，schema 的 `Safe <30%, Dangerous >70%` / 中文同义文案仍存在。`kernel/schema.go:97` 是固定风险解释；`engine_prompt.go:87` 按策略输出保证金上限，两者来自不同来源。

只读 SQL：

```sql
SELECT t.name,s.name AS strategy,
       json_extract(s.config,'$.risk_control.vol_target_enabled') AS vol_target,
       json_extract(s.config,'$.risk_control.max_margin_usage') AS max_margin
FROM traders t LEFT JOIN strategies s ON s.id=t.strategy_id
WHERE t.is_running=1 OR t.name='mac-nofx' LIMIT 20;
```

| 当前运行交易员 | vol_target_enabled | max_margin_usage |
|---|---:|---:|
| mac-nofx / Conservative Strategy | 0 | 0.9 |
| 美股大师 | 0 | 0.9 |
| us-stock | 0 | 0.9 |
| 因子 ensemble 参考字段-候选 / 对照 | 0 / 0 | 0.9 / 0.9 |
| 提前减仓 0.8R（1/3）-候选 / 对照 | 0 / 0 | 0.9 / 0.9 |

未被当前运行交易员引用的 Balanced Strategy、做空策略确实有 `vol_target_enabled=1`，不要把策略表中存在启用项等同于实盘正在启用。

实盘 decision_records 样本 id=17881（mac-nofx）对 system_prompt 的只读 `instr` 结果为：“评估加仓”位置 0，“危险值>70%”位置 453。说明当前实发 prompt 没有加仓邀请，但含静态风险文案；最近 500 个 id 的 mac-nofx 131 条记录均含该 70% 文案。

**实际影响：** 当前主交易员和运行中的 paper A/B 均关闭 VolTarget，此加仓邀请目前不生效；未来开启后会诱导不可执行建议。70% 是自然语言风险提示，90% 是策略硬上限，二者可以同时成立，不能据此声称程序在 70% 和 90% 两处互相冲突地拒单；当前影响是模型解释风险时可能对阈值含义困惑。另两名运行 Binance 交易员也已列出，核查没有改动任何策略。

**建议修复范围：** 加仓邀请应与实际允许的动作一致；将静态风险解释与策略硬上限明确分名，或统一由策略生成相关说明，不必机械地把“危险”阈值改成硬上限。

## 验证执行与限制

新增文件：

```text
market/verify20261028_data_test.go
market/breakout/verify20261028_funding_test.go
kernel/verify20261028_data_test.go
mcp/provider/verify20261028_temperature_test.go
trader/verify20261028_batch_test.go
```

执行命令（market 先产生跨边界夹具，kernel 再产生已校验批次，trader 最后消费）：

```sh
GOCACHE=$PWD/.gocache go test -mod=readonly -gcflags=all=-l ./market -run '^TestVerify20261028' -v -count=1
GOCACHE=$PWD/.gocache go test -mod=readonly ./kernel -run '^TestVerify20261028' -v -count=1
GOCACHE=$PWD/.gocache go test -mod=readonly ./trader -run '^TestVerify20261028' -v -count=1
GOCACHE=$PWD/.gocache go test -mod=readonly ./market/breakout ./mcp/provider -run '^TestVerify20261028' -v -count=1
GOCACHE=$PWD/.gocache go test -mod=readonly -race ./kernel -run '^TestVerify20261028RankingCacheConcurrentFill$' -v -count=1
/Users/zhangyun/workspace/nofx/.venv/bin/python -m unittest scripts.factor_mining.tests.test_live_scores.LiveScoresTests.test_as_of_excludes_today_and_future_using_utc -v
```

market 使用 gomonkey 注入取数，故关闭内联；跨收盘测试在第二个 fake 请求中等待真实分钟边界，不替换全局时钟，不触碰生产进程。初版曾用全局时钟替换，在本机 ARM64 上干扰后续测试，已移除并改为真实延迟后整体通过。执行侧批次行为用了 fake，并另单测真实同向仓位拒绝函数，没有声称模拟了交易所最终一致性、所有风险门或保护单生命周期。资金费样本不是线上合约切换事实。清算日志统计只包含指定日志可观测到的区间。当前模型事实来自只读配置与有限记录样本，不能外推所有历史模型请求。

测试日志：`market-tests.log`、`kernel-tests.log`、`trader-tests.log`、`provider-breakout-tests.log`、`race-tests.log`、`python-tests-venv.log`；生产配置：`production-config.txt`。全部位于 `/private/tmp/verify20261028-data/`。本报告不包含任何密钥。

结束检查：所有测试进程已结束，已删除本任务 `.gocache`；`git diff --exit-code` 退出 0，`git status --short` 仅有上述 5 个新增临时测试文件，业务源码和 go.mod/go.sum 均无变更。

## 汇总表

| # | 核实项 | 结论 | 当前实盘影响 / 边界 |
|---|---|---|---|
| 1 | 串行多周期跨收盘 | **CONFIRMED** | 当前路径在用；采样时钟修复仍不能弥补源观测时间偏晚，未量化线上频率 |
| 2 | 8h→1h 年化失真 | **CONFIRMED** | 合成切换下 market −87.5%、scanner 最初 −82.35%；当前标的是否切换未知 |
| 3 | 排名缓存/OI 补全共享竞态 | **REFUTED** | 返回副本已覆盖；3,200 次并发定向 race 通过 |
| 4 | 清算覆盖窗口高估 | **CONFIRMED** | 72h 日志有 6 条警告、4 个缺口共 95 秒；不等于当前新进程已含这些旧缺口 |
| 5 | 接受未来因子文件 | **CONFIRMED** | 读取端接受 2099 日期；当前文件不超前，写入端日期测试通过，主要影响开启参考字段的 paper 候选 |
| 6 | 温度与模型兼容 | **CONFIRMED（条件性）** | Kimi 已为 1；部分 OpenAI 模型受限，当前全为 GLM，未证实生产温度 400 |
| 7 | BTC 头行含 forming 指标 | **CONFIRMED（口径需限定）** | 头行条件性出现；closed-only 标题只覆盖结构化块，主策略周期并非固定 3m |
| 8 | 批内重复/开平冲突 | **CONFIRMED** | 新鲜仓位可挡同向重复；排序允许先平后开，无批次意图消解 |
| 9 | 规则统计与 soft tag | **CONFIRMED** | 表为空；tag 问题主要漏 awareness 日志，不会阻止所有 soft lesson 进入 prompt |
| 10 | 外部字符串过滤 | **CONFIRMED** | 多处自由文本无字段白名单/长度界限；没有线上恶意注入或突破硬门证据 |
| 11 | 加仓邀请/保证金文案 | **CONFIRMED（潜伏/语义）** | 所有运行策略 VolTarget=0；静态70%提示与90%硬上限同时存在，但不是双硬门冲突 |
