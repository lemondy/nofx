审查基准：`d51e5fbc`，与 `origin/dev` 一致。确认 **7 项问题：HIGH 3 项、MEDIUM 4 项，未确认 CRITICAL**。

未修改源码或报告、未提交 commit、未调用真实交易接口。数据目录仅作只读访问；SQLite 使用 `mode=ro&immutable=1`。工作区差异仍只有开始时已有的 `web/node_modules`。

**关于 10 月 16 日起“前五名大多暴涨后下跌”的现象**

抽样记录可以直接由当前动量定义解释，**没有发现动量下标偏移**。以最后一根为 t：

```text
代码：close[n-22] / close[n-253] - 1
等价：close[t-21] / close[t-252] - 1
```

我读取本地复权日线，在内存中复算了 10 月 16 日决策采用的 10 月 15 日收盘数据：

| 标的 | 12-1 动量复算值 | 决策日志 | 最近 21 根涨跌 | 收盘价 / SMA200 |
|---|---:|---:|---:|---:|
| SNDK | +1094.1001% | +1094.1% | −8.23% | 1394.83 / 1205.42 |
| LITE | +486.4269% | +486.4% | −5.42% | 869.54 / 775.21 |
| MU | +395.8069% | +395.8% | +0.52% | 931.24 / 714.69 |

对应 `data.db` 的 `decision_records.id=9133/9134`。复算来源为 `data/factor_mining/us/daily/{SNDK,LITE,MU}.json`，并非重新请求行情。

`momentum` 模式只要求“站上 SMA200，再按跳过最近一个月的动量排序”，不要求 Kell 阶段向上。因此 SNDK/LITE 最近下跌，仍能排名很高；现有测试甚至明确要求 `downtrend` 标的可以进入动量候选。

但也发现了实际数据滞后：**10 月 22 日 15:30 ET 的快照仍使用截至 10 月 20 日的日线，报告年龄却只有约 12 分钟**。因此不能把所有异常都归为行情，下面第 1 项需要优先处理。

**确认的问题**

**1. [HIGH] 只检查根数，不检查行情最后日期；旧日线可被包装成新扫描报告**

位置：[yahoo.go:199](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/market/usstock/yahoo.go:199)、[series.go:212](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/market/usstock/series.go:212)、[scan.go:352](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/stockengine/scan.go:352)、[snapshot.go:26](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/stockengine/snapshot.go:26)。

- **触发场景：** Yahoo 返回 HTTP 成功、历史根数充足，但最新交易日缺失。停牌或供应商数据滞后也会走这条路径。
- **证据：** `yahooBars` 排除形成中的 K 线后直接 `return closed, nil`；`GetUnderlyingSeries`、`fetchScanBars` 和 `BuildSnapshot` 主要检查数量，没有核验最后一根是否达到最近应有的收盘时间。扫描的 `GeneratedAt` 使用本次扫描时钟，与底层数据日期无关。
- **实际记录：** `decision_records.id=11364`，2026-10-22 15:31 ET，SNDK/LITE/MU 的 Yahoo 日线均止于 **10-20**，扫描报告年龄 **12m6s**。动量值分别为 +1160.4%、+479.3%、+416.3%，与本地日线以 10-20 为 t 的复算一致。按仓库日历，此时应至少具备 10-21 日线。
- **影响：** 过期的 SMA200、动量、Kell 阶段继续决定候选；报告年龄看起来正常。
- **建议修复：** 按 NYSE 日历核验各周期最新应有的已收盘时间，记录每个结果的实际数据截止时间；不达标时刷新或切换来源，仍不达标则禁止用它新增扫描候选。
- **确认方式：** 静态追踪上述调用链，并只读核对决策记录、独立复算本地日线。相关包现有测试通过，但没有这条时间新鲜度约束。

**2. [HIGH] 刷新失败后，过期扫描报告仍可无限期决定候选**

位置：[scan.go:202](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/stockengine/scan.go:202)、[auto_trader_stock_cycle.go:196](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_stock_cycle.go:196)、[auto_trader_stock_cycle.go:237](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_stock_cycle.go:237)。

- **触发场景：** 已有成功报告，随后扫描持续失败。某股票已跌破 SMA200，但旧报告仍为 `AboveSMA200=true`。
- **证据：** 缓存返回“旧报告 + error”；`stockCycleSymbols` 记录日志后，只要 `report != nil` 就继续调用 `TopMomentum/TopLong`，没有年龄上限。45 分钟检查位于后面的快照附加信息及 `freshScanResults`，并不撤销已选出的候选。
- **影响：** 几小时前甚至前几天的排名仍可授权候选；入场校验也没有重新核验 SMA200 或扫描资格。正常缓存同样只按年龄失效，没有按收盘边界失效。
- **建议修复：** 候选选择同时校验报告年龄和数据截止日期；过期报告只用于诊断，扫描不可用时保留持仓及 hybrid 手动标的，暂停新增扫描候选。
- **确认方式：** 阅读缓存发布、候选选择及入场校验完整路径；`TestGetScanReportFailedRefreshKeepsOldReport` 已通过，并确认其返回两小时前的报告。旧设计确实允许沿用报告，但这不能保证“当前仍站上 SMA200”的筛选条件。

**3. [HIGH] manual 模式没有补入已有持仓，配置变更后退出动作会被拒绝**

位置：[auto_trader_stock_cycle.go:193](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_stock_cycle.go:193)、[auto_trader_stock_cycle.go:538](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_stock_cycle.go:538)、[validate.go:78](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/stockengine/validate.go:78)。

- **触发场景：** 已持有程序仓位 NVDA，随后从 manual 列表删除 NVDA，或由 scan 切换成只配置 AAPL 的 manual。
- **证据：**
  ```go
  if !sc.IsScanMode() {
      return sc.Symbols, stockCachedScan()
  }
  ```
  持仓补入发生在这个返回之后；周期配置副本也只在 scan/hybrid 下生成。校验器则先要求所有动作的标的存在于 `Config.Symbols`，否则返回 `UNKNOWN_SYMBOL`。
- **影响：** NVDA 仍可能出现在持仓块中，但 AI 对它的平仓、减仓、调整止损全部被拒绝。独立保护 tick 仍运行，不能据此认为正常决策管理完整保留。
- **建议修复：** 所有模式均将程序持仓纳入管理列表，并将“允许新增仓位的候选”与“允许退出的持仓”分别授权。
- **确认方式：** 静态追踪候选、配置副本、持仓提示及校验顺序。现有 `TestStockScanHeldSymbolAlwaysIncluded` 仅覆盖 scan 模式，不能保证此边界。

**4. [MEDIUM] Yahoo 回退到 bStock 后，交易日指标变成自然日指标**

位置：[scan.go:352](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/stockengine/scan.go:352)、[series.go:124](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/market/usstock/series.go:124)、[momentum.go:42](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/stockengine/momentum.go:42)。

- **触发场景：** Yahoo 不可用，扫描采用 bStock 日线；bStock 累积至少 253 根 UTC 日线后也能通过动量有效性检查。
- **证据：** bStock 只按 `OpenTime + 24h` 判断收盘，保留周末、假日和夜盘形成的日线；动量和 Kell 分类随后直接按切片位置计算，没有转换为正股常规交易日序列。
- **影响：** “252 日回看”实际约为 252 个自然日，“跳过 21 日”也变成自然日；SMA200、Kell 阶段和排名均与 Yahoo、回测的交易日口径不同。来源切换本身即可改变候选。
- **建议修复：** 动量排名只接受同一常规交易日口径的数据；无法取得等价数据时标记不可用。bStock 回退若继续用于其他指标，应明确采用另一套定义。
- **确认方式：** 对照取数、分类及 `stocks_data.py:185` 的排名实现；`TestFetchScanBarsPrefersUnderlying` 已通过，确认回退路径会直接接受 bStock。未将此项认定为当前 SNDK 现象的原因，抽样记录使用的是 Yahoo。

**5. [MEDIUM] 恰好一个 lot step 的真实持仓被隐藏，下游可能误清持仓记录**

位置：[account.go:120](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/binance_bstock/account.go:120)、[auto_trader_stock_state.go:252](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_stock_state.go:252)、[auto_trader_stock_state.go:284](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_stock_state.go:284)。

- **触发场景：** 部分成交或减仓后，程序持仓和真实余额都剩一个 step，例如 `0.001`；保护记录可正常核验。
- **证据：**
  ```go
  if qty <= field(filter(s, "LOT_SIZE"), "stepSize") {
      continue
  }
  ```
  `GetPositions` 省略这份余额，下游 `st.Balances[sym]` 因而为零；满足对账条件后进入 `stockShrinkOwned(... balance=0 ...)`。
- **影响：** 真实库存可能被记为外部清仓，保护被撤销、程序持仓记录清零，随后不再作为持仓候选补入。库存暂时不满足下单金额，也不应等同于库存不存在。
- **建议修复：** 持仓查询返回所有正余额，交易可执行性另行判断；缩减归属前直接核验原始资产余额，不能用经过 dust 过滤的展示列表作为库存真相。
- **确认方式：** 静态追踪省略余额到清理持仓的路径；运行 `TestBalancePositionsAndCache` 通过，该测试明确验证了一个 step 的余额会被隐藏。

**6. [MEDIUM] 入场仍使用 LLM 调用前的报价、时段和新鲜度，旧报告问题尚未修复**

位置：[validate.go:111](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/stockengine/validate.go:111)、[auto_trader_stock_cycle.go:836](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_stock_cycle.go:836)、[auto_trader_stock_exec.go:306](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_stock_exec.go:306)。

- **触发场景：** 配置 15:59 ET 决策，采样时常规时段、报价新鲜；模型或 repair 返回时已经 16:01，且策略禁止盘后新增仓位。价格或背离也可能在等待期间改变。
- **证据：** 校验读取旧 `ctx.Session`、旧 `Quote.RefFresh`；执行继续使用 `entry := snap.Price`，随后直接 `BuyMarketNotional`，没有重新取得报价、检查当前时段或重算背离。
- **影响：** 已不满足时段、新鲜度或背离条件的标的仍能成交。
- **建议修复：** 提交订单前按执行时钟重新核验时段、bStock/正股报价和背离，并以最终价格重算止损距离及数量。
- **确认方式：** 复查 10 月 16 日 C 报告对应问题，追踪 `Decide → Validate → execStockEntry → stockOpenLive`；当前仍保留同一缺口，故列为未修复旧项。

**7. [MEDIUM] 跨收盘缓存修复没有覆盖“HTTP 请求进行中跨界”的情况**

位置：[yahoo.go:75](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/market/usstock/yahoo.go:75)、[yahoo.go:115](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/market/usstock/yahoo.go:115)、[yahoo.go:195](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/market/usstock/yahoo.go:195)。

- **触发场景：** Yahoo 在 15:59:59 生成当日形成中日线，网络响应到达程序时已经 16:00:01。
- **证据：** 缓存 `loaded` 使用响应完成后的 `nowFunc()`；`yahooBars` 再用完成后的时间判断日线已收盘。以后 `cacheStillFinal` 检查 `loaded.Before(close)` 为 false，也不会因这次收盘使缓存失效。
- **影响：** 盘中 OHLCV 被当作最终收盘值，日线缓存可继续保留 6 小时。
- **建议修复：** 保存请求开始时间及数据采样/结算状态；请求跨越收盘边界时重新取最终数据，不能仅凭接收完成时间认定收盘。
- **确认方式：** 静态时序核验。已有 `TestYahooDailyCacheRefreshedAfterSessionClose` 通过，确认“先缓存、后跨收盘再调用”的旧问题已修复；该测试未覆盖请求本身跨收盘的窗口。

**验证情况**

以下检查全部通过，Go 命令均设置 `GOCACHE=$PWD/.gocache`、`GOPROXY=off`、`GOFLAGS=-mod=readonly`：

- `go build ./...`
- `kernel/stockengine`、`market/usstock` 全部测试，关闭 live smoke。
- `trader` 中 `TestStock*`、`TestNextStockDecisionTime`。
- bStock 的 `TestBalancePositionsAndCache`。
- 扫描单飞、后台扫描及 Yahoo 串行请求的定向 `-race` 测试。

未新增测试文件。上述异常分支中，未动态复现的部分已明确标为静态确认。

**已核验正确的部分**

- 动量下标、百分比单位与回测 `rank_at` 一致；SMA200 包含最后一根、使用严格 `>`，不足 253 根及计算窗口中的非正数/NaN/Inf 会失效。
- 正常输入下并列排名稳定：实盘依次按动量、RS63、代码排序；回测并列仅按代码，实盘的额外规则已在设计中明确。
- scan/hybrid 先补程序持仓，再补手动标的及扫描 picks；掉出排名、加入 `scan_exclude` 都不会直接移除已有持仓。数据快照缺失也没有直接删除 scan 候选中的持仓。
- scan 忽略手动 `symbols` 与文档一致；`scan_top_n` 是额外扫描名额。格式、重复校验及候选合并去重已检查。
- 正常刷新时，扫描注册表通过合约 `EQUITY` 交叉匹配，测试确认排除 BNB/ARB/SHIB 等后缀碰撞。
- Yahoo 日/周线使用 `adjclose/close` 缩放 OHLC；普通盘中调用会过滤当日未收盘日线。常规跨收盘缓存修复、DST、规则内假日、半日市及小时线尾段收盘测试通过。
- 报价采样时的同 session、15 分钟新鲜度和背离百分比计算正确，阈值使用绝对值；执行阶段缺口见第 6 项。

**无法确认/需要线上数据的点**

- 未取得当时 Yahoo 原始响应及完整 253 根扫描输入，不能独立证明供应商所有拆股、分红复权均正确。本地复权日线与日志动量吻合，支持“公式按定义工作”，不等于证明上游价格无误。
- 10 月 22 日日线停在 10 月 20 日已从记录确认；具体由上游迟到、缓存还是当时部署版本造成，需要请求日志和原始响应定位。
- `DEFAULT_EXCLUDE` 只在 Python 回测默认生效；Go 使用策略显式 `ScanExclude`。当前数据库两份美股策略已配齐该七只排除标的，但名单不等于“所有杠杆 ETF”的类别禁入。回测还另排除 SPY/QQQ，实盘无相同默认；是否要求统一属于策略口径，未擅自判为 bug。
- 未验证线上是否已经发生误清持仓、过期报告开仓或跨时段成交。SQLite 检查使用只读主库快照，未包含可能仍留在 WAL 中的新记录。