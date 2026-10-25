基于 `HEAD d51e5fbc`，确认 **12 项问题：3 HIGH、9 MEDIUM；未确认 CRITICAL**。其中两项为历史 D 报告中仍未修复的问题。

未修改源码、配置或报告，未提交 Git，未调用实盘下单、撤单接口，未写入主仓库 `data`。构建和测试使用了指定 `.gocache`，审查前后 Git 状态一致。以下结论依据代码路径确认；未新增复现文件，也不把既有测试通过当作这些边界已经覆盖。

**1. [HIGH] 空候选会同时跳过模型持仓管理**

位置：[trader/auto_trader_loop.go:125](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_loop.go:125)，同文件 `802`、`159`。

- **触发场景**：已有可由 AI 管理、允许退出的持仓，但唯一候选源失败、没有符合条件的信号，或全部被排除。
- **证据**：

  ```go
  // 来源失败被转换为空列表
  logger.Infof("... Failed to get candidate coins: %v (will use empty list)", err)

  if len(ctx.CandidateCoins) == 0 {
      ...
      return nil
  }
  ```

  返回发生在 `GetFullDecisionWithStrategy` 之前，没有检查 `ctx.Positions`。因此持仓的模型平仓、调整保护等决策也被跳过。前置 watchdog 仍会运行，不能把影响扩大成所有保护失效。
- **确认方式**：追踪候选错误处理、空池返回和模型调用顺序；并核对 kernel 对持仓单独采集、渲染的路径。
- **建议修复**：只有“没有入场候选，且没有需要模型处理的持仓”时才跳过；来源失败应进入只管理现仓的周期。

**2. [HIGH] 排除名单没有约束存量入场挂单**

位置：[trader/auto_trader_pending_risk.go:137](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_pending_risk.go:137)、[trader/auto_trader_pending.go:637](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_pending.go:637)。

- **触发场景**：某币已有未成交 GTC 入场单；将其加入 `excluded_coins` 后重启，恢复该 pending。行情和方向条件仍然合格。
- **证据**：新决策在 `auto_trader_risk.go:2566` 检查 `EXCLUDED_COIN`；但 `pendingDirectionBlocked` 完整函数只检查方向、行情、BTC、确认信号等，**不读取排除名单**。正常返回 `""` 后，挂单继续保留并可成交。
- **确认方式**：对照新开仓门与 pending 重查全部分支；现有 `TestBatch2GatesExcludedCoinOverridesPoolAndOwnership` 仅覆盖新决策。
- **建议修复**：pending 生命周期入口也检查规范化后的排除名单；取消剩余入场量并核对终态，部分成交量继续保留保护。

**3. [HIGH] CoinAnk 短行解析会越过 fallback，直接终止 trader**

位置：[provider/coinank/coinank_api/kline.go:41](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/provider/coinank/coinank_api/kline.go:41)、[trader/auto_trader.go:626](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader.go:626)。

- **触发场景**：供应商返回合法 JSON，例如 `{"success":true,"data":[[]]}`，或任一行不足九列。
- **证据**：

  ```go
  for i, k := range result.Data {
      klines[i].StartTime = int64(k[0] + 0.001)
      ...
      klines[i].Count = k[8]
  }
  ```

  没有长度检查。panic 无法转换成 `getKlinesWithFallback` 所需的 error；最终由 `AutoTrader.run` 恢复，并执行停止交易循环、实时保护和同步器的清理。
- **确认方式**：复核历史 D 报告遗留，进一步追踪到实际 panic 恢复位置；未向供应商发送异常请求。
- **建议修复**：解析层检查行长、数值及时间顺序，返回可识别错误，使 Binance fallback 正常执行。

**4. [MEDIUM] Hyperliquid 的大小写规范化破坏 `kPEPE` 等合法名称**

位置：[kernel/engine.go:1566](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/engine.go:1566)，同文件 `1591`；[trader/hyperliquid/trader.go:90](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/hyperliquid/trader.go:90)。

- **触发场景**：`hyper_all/hyper_main` 返回 `kPEPE` 等含小写前缀的名称。
- **证据**：候选变成 `KPEPEUSDT`，交易适配器进一步变成 `KPEPE`；而 `GetMarketPrice` 在原始 `allMids` 中精确查找 `allMids[coin]`，无法匹配 `kPEPE`。
- **确认方式**：核对候选转换、报价查询及固定版本 SDK 的名称映射；[官方 SDK 响应样本](https://github.com/hyperliquid-dex/hyperliquid-python-sdk/blob/master/tests/cassettes/info_test/test_get_candles_snapshot.yaml)明确使用 `kPEPE`。
- **建议修复**：建立“内部规范名 → 场所原始名称、资产编号、合约倍率”的映射。调用适配器前拒绝未知名称；不能仅统一大写，也不能直接把 `PEPE/1000PEPE/kPEPE` 当作相同计价单位。

**5. [MEDIUM] Hyperliquid 全量候选包含明确退市的资产**

位置：[provider/hyperliquid/coins.go:51](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/provider/hyperliquid/coins.go:51)，同文件 `108`。

- **触发场景**：metadata 的 `universe` 中存在 `isDelisted:true`。
- **证据**：本地结构只解析 `Name`，随后无条件把每个条目加入 `allCoins`；`hyper_all` 因此把退市条目作为候选返回。
- **确认方式**：对照解析和候选出口；[官方 metadata 定义](https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals)明确包含退市条目及 `isDelisted` 字段。
- **建议修复**：解析并过滤退市状态，在排序、截断之前完成；状态刷新与成交量排名缓存分别设置期限。

**6. [MEDIUM] 两层缓存叠加，可把 AI500 旧榜延长到约 45 分钟**

位置：[provider/vergex/relay.go:62](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/provider/vergex/relay.go:62)、[kernel/engine.go:1464](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/engine.go:1464)，同文件 `1484`。

- **触发场景**：上游在 T0 后停止更新，策略在约 T0+29 分钟仍读取到 relay 缓存。
- **证据**：relay 有 30 分钟 TTL；此次缓存命中被 kernel 当作成功，再执行：

  ```go
  ai500LastGood.symbols = cp
  ai500LastGood.at = ai500Now()
  ```

  relay 在第 30 分钟过期后，kernel 又允许从刚重置的时间起复用 15 分钟，绕过 relay 的过期边界。
- **确认方式**：串联两个缓存的计时方式；`TestAI500Fallback*` 通过，但只模拟直接取数，未覆盖 relay 数据年龄。
- **建议修复**：传递原始获取时间及有效截止时间；缓存命中不能重新延长数据寿命。

**7. [MEDIUM] AI500 全局缓存会被小 limit 策略缩短**

位置：[kernel/engine.go:1451](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/engine.go:1451)，同文件 `1480`。

- **触发场景**：策略 A 请求 10 币并缓存成功；策略 B 请求 1 币并覆盖缓存；随后上游失败，A 使用 fallback。
- **证据**：

  ```go
  symbols, err := ai500Fetch(e, limit)
  ...
  ai500LastGood.symbols = cp
  ```

  缓存是进程级单槽，保存的却是按调用者 limit 截断后的结果。A 最终只得到一个币，另外九个候选丢失。
- **确认方式**：核对 provider 截断位置、全局缓存覆盖和 fallback 读取；既有测试只覆盖“大缓存向小请求截断”。
- **建议修复**：缓存完整榜单，消费者分别截断；或按请求范围隔离缓存。

**8. [MEDIUM] Vergex relay 把 HTTP 错误 JSON 当作成功空榜**

位置：[provider/vergex/headless.go:135](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/provider/vergex/headless.go:135)、[provider/vergex/relay.go:53](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/provider/vergex/relay.go:53)。

- **触发场景**：上游返回 HTTP 503，正文是 `{"error":"temporarily unavailable"}` 等合法对象。
- **证据**：浏览器 fetch 不检查 `r.ok`；`FeedRelay` 只检查 JSON 语法。错误体覆盖旧榜后，AI500/OI 结构反序列化成功，结果为空且无 error。AI500 fallback 不启动，relay 健康状态也可能被记为成功。
- **确认方式**：复核历史 D 报告遗留，并串联 `fetchAll → FeedRelay → fetchJSON → GetAI500Symbols/fetchOISymbols`。
- **建议修复**：检查 HTTP 状态和端点响应结构；区分合法空榜与错误载荷，错误不得覆盖最后成功数据。

**9. [MEDIUM] Piggy 在质量过滤之前只取前三倍，可能漏掉真正合格的候选**

位置：[kernel/engine.go:1078](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/engine.go:1078)，同文件 `1118`；[market/breakout/scheduler.go:494](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/market/breakout/scheduler.go:494)。

- **触发场景**：`limit=1`，同方向前三名均为 `approach`，第四名是达到最低等级的真实突破。
- **证据**：先执行 `fetch := limit * 3`，scheduler 返回前三行即停止；随后质量过滤删除这三行，函数报告空池，永远看不到第四行。
- **确认方式**：核对 scheduler 的提前截断与 kernel 的后置过滤；现有 Piggy 测试没有覆盖合格行位于截断之后。
- **建议修复**：遍历完整快照，先过滤方向、形态和等级，再取前 N。

**10. [MEDIUM] 主做空宇宙为空时，提前返回导致健康磨顶池也被丢弃**

位置：[market/breakout/shortscan.go:867](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/market/breakout/shortscan.go:867)，同文件 `880`。

- **触发场景**：实时涨幅均不足 5%，历史池关闭，跌幅榜没有通过 4h 下行门的标的；独立 slow-top 缓存仍有合格信号。
- **证据**：

  ```go
  if len(out) == 0 {
      return nil, time.Time{}, fmt.Errorf(...)
  }
  out = mergeShortScans(out, slowTopSnapshot())
  ```

  空池判断早于磨顶池合并。
- **确认方式**：追踪各宇宙组装、空结果返回和独立 slow-top 快照读取顺序。
- **建议修复**：合并仍有效的所有宇宙后，再判断总结果是否为空。

**11. [MEDIUM] pending 重查撤销了入场时明确豁免顶确认的 breakdown 空单**

位置：[kernel/engine_prompt.go:1736](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/engine_prompt.go:1736)、[trader/auto_trader_pending_risk.go:207](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/trader/auto_trader_pending_risk.go:207)。

- **触发场景**：breakdown 候选分数合格，`Confirmed=false`，其他方向门正常；限价空单尚未成交，默认顶确认门开启。
- **证据**：kernel 对 `ShortUniverse=="breakdown"` 明确不传顶确认限制；pending 却对所有 short 执行：

  ```go
  if market.Normalize(s.Symbol) == market.Normalize(pe.Symbol) && !s.Confirmed {
      return "pending short topping confirmation lost"
  }
  ```

  没有 breakdown 豁免，合法挂单会在周期重查时被撤。
- **确认方式**：对照入场信号富化与 pending 判断；现有 pending 方向测试没有覆盖该宇宙差异。
- **建议修复**：入场和重查共享确认规则，并保留信号家族，避免对延续型空单重新要求顶部确认。

**12. [MEDIUM] 非空符号未去首尾空白，排除名单可失效**

位置：[market/data.go:978](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/market/data.go:978)、[kernel/engine.go:1034](/Users/zhangyun/workspace/nofx/.claude/worktrees/project-review-bug-check-3a1440/kernel/engine.go:1034)。

- **触发场景**：通过策略 JSON/API 保存 `excluded_coins:["BTCUSDT "]`；候选源正常返回 `BTCUSDT`。
- **证据**：`Normalize` 只执行 `strings.ToUpper`，没有 `TrimSpace`。上述排除项变成 `"BTCUSDT USDT"`，与候选不匹配；交易端再次调用同一函数，仍然不能拦截。静态列表中的非空白条目也存在同类问题。
- **确认方式**：核对规范化、两处排除集合和 `StrategyConfig.Validate`；保存校验未拒绝或清理这种输入。
- **建议修复**：统一规范化入口先去首尾空白，再校验名称；所有来源、排除名单和去重使用一致规则。

**验证结果**

`GOCACHE="$PWD/.gocache" GOPROXY=off go build ./...` 通过。以下相关测试通过：

- kernel：静态去重、混合 Piggy 元数据、候选移除与 verdict、AI500 fallback、扫描冲突、时段加权、采样时刻及陈旧门。
- market/breakout：K 线与响应缓存、历史池筛选、磨顶预筛、OI 数量口径、breakdown。
- trader：候选白名单、排除名单、filtered 候选拦截和 pending 方向重查。
- provider/nofxos 单元测试；Vergex relay、Hyperliquid 纯转换及精度测试；美股日历/DST 测试。

**已核验正确的部分**

- **`OI 4.88M < 5.0M` 的换算及比较方向正确**：当前路径取 OI 数量，乘同一交易符号价格换成 USD，再除以百万；严格小于阈值才过滤。
- 常规 `BTC/BTCUSDT`、大小写、Gate/OKX 分隔符可归一化；`1000PEPE` 前缀被保留，没有直接抹掉合约倍率。
- mixed 使用映射去重，并保留做空分数、确认状态、`NearHighAlso`、扫描时间和 Piggy 方向；排序确定，排序函数没有额外截断持仓。
- `snapshotCandidateCoins` 避免边遍历边移除造成跳项；低 OI 会同步记录原因并移出候选。
- 历史报告中的“行情失败候选仍能新开仓”普通路径已修复：交易端会排除 `FilteredCandidates`；新决策的排除名单门也有效。
- 美股时段使用 `America/New_York`，包含夏令时、节假日及提前收盘；加权发生在做空候选最终截断之前。
- 历史涨幅池按 UTC 日期保存滚动 24h 榜的盘中峰值，跨日按最大涨幅排名；扫描结构 K 线丢弃形成中蜡烛，符合当前“榜单快照池”定义。
- NofxOS 旧取数客户端已不在本轮 AI500/OI 候选主链，未把其遗留问题计入当前候选缺陷。

**无法确认/需要线上数据的点**

- 无法仅凭代码确认某次 `OI 4.88M` 日志对应的数据年龄和真实合约数量；需要原始 OI、价格、时间戳及场所信息。
- OI 获取失败时跳过地板是当前明确的放行策略；静态、AI500、Piggy 等来源不能因此被视为已经核验过最低 OI。是否必须改为拒绝，需要确定产品要求。
- Vergex 是否始终按分数排序、是否实际出现重复或变体符号，需要真实响应样本；当前解析信任上游顺序。
- 各问题的线上触发频次、受影响订单及损益未核验。本报告确认实现缺口，不据此断言已经发生实盘事故。