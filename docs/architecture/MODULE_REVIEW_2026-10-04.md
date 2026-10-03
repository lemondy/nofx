# NOFX 全模块深度审查 · 2026-10-04

审查基线:dev 分支(含至 `fce262ff` 的全部已提交修复 + 并行会话的 158 文件在途重构,按工作区现状评审)。方法:5 个并行对抗审查 agent(数据选标 / 内核决策 / 执行风控 / 存储状态 / API 前端基础设施),每个模块先产出方案介绍、再对抗式找 bug;发现当场修复与待办清单分列。修复落地为 4 个 commit(`d7b5c200` / `0a6dc638` / `16351d79` / `3cc92cc4`)加上当日更早的 `357eb802`(P0 批)。

---

## 一、各模块方案介绍

### 1.1 行情数据与候选池(market/ + kernel 组装)

**行情数据入口(market/data.go)** 分执行侧与分析侧双轨:`GetWithExchangeAndPrice` 产出执行侧三周期快照(3m/4h 必取、1h/1d 尽力而为、15m 由 3m 聚合——09-19 修复:缺 15m 键使执行 ATR 落到 4h 尺度);策略侧三变体:通用 `GetWithTimeframes`(默认 Binance)、`GetWithTimeframesForExchange`(执行数据,要求同所报价,非 Binance fail-closed)、`GetWithTimeframesForVenue`(10-01 F10:分析 K 线必须来自执行场所)。venue 正确性贯穿:交易所原生 live 价回填 forming K 线并量化厂商冻结偏离(VendorStalenessPct);xyz 代币化资产按 `IsXyzDexAsset`(含 Binance 原生上市反查)路由 Hyperliquid;陈旧防护检测连续 5 根价格冻结。

**K 线取数三级回退(market/data_klines.go)**:CoinAnk(按所视图)→ 币安 fapi 直连(仅 Binance,09-24 全量 403 事故教训:单一厂商硬依赖才是缺陷)→ OpenBB sidecar(仅主流币)。执行侧序列永不跨所替换。

**衍生品层**:OI 取 30×1h 真实历史均值(失败置 `OpenInterestOK=false`,缺数≠真零,R4-6);资金费三口径(前瞻 5min 缓存 / 实测结算间隔中位数年化 / 已结算历史 + rollover 唯一谓词);情绪综合三源独立缓存 + 程序判定枚举(sentiment.go)。

**突破调度器(market/breakout/)**:5 分钟扫描猪猪板块(成交量 top + 三榜第二宇宙),BTC regime 横截面调分;short scan 六维做空评分 + 磨顶宇宙(30min,$30M/日预筛,碰撞盖 NearHighAlso 章);调参走持久化 marker(7 天 + starved 检测)。新鲜度三闸:piggy 12min TTL、slowtop 2×TTL 丢弃、RefreshNow 等待者落地即返(10-03 修:等待者不再重复扫描)。

**候选组装(kernel/engine.go → engine_analysis.go)**:按 source_type 扇入(mixed 折叠全部启用源,short/piggy 元数据穿线)→ filterExcludedCoins 单一出口(XYZ chokepoint + 排除集)→ fetch 通道(执行所取数、near_high/NearHighAlso OI 豁免、OI 未知跳过、低 OI 同步移出保三方一致)。

### 1.2 内核决策层(kernel/)

**信号管线(ComputeSymbolSignals)**:逐 TF 归一化特征(趋势四象限/ATR 百分位/量比/RSI/MACD/EMA/swing 结构位 live 分类)→ 衍生品块 → 方向证据计票(±100 directional_score + 冲突分类)→ 15m 微趋势执行过滤 → MarketRegime → DataQuality(按策略实际 fetch 列表要求 ≥60 根,10-03 修:静态 15m 基条移除)→ 限价锚(ATR 缩放 offset + 供给区抑制)→ **突破状态机 + 回踩锚**(10-03 P0 修复后:confirmed/retest_hold 时多头锚=被破前阻力位,止损/RR 从该锚重算;fake_break 锚消失,无追价回退)→ 硬门。

**硬门(computeHardEntryGate)** 全程序预计算,`Allowed = len(Failed)==0`:BSTOCK_DATA / STOP_PLAN_NO_STRUCTURE / STOP_PLAN_OUT_OF_BAND / **WIDE_STOP**(>10%,bstock 豁免,双方向)/ MICRO_TREND_* / EXTENDED_PUMP_UNCONFIRMED / LIMIT_ANCHOR_SUPPRESSED / **EMA20_STRETCH**(>4h EMA20 12%,回踩路径豁免)/ **BTC_4H_DOWNTREND / BTC_WEAK_LONG**(真 4h BTC 判定,10-04 接线修复)/ RR_MAX / DATA_INSUFFICIENT / MIN_SIZE_DEAD_ZONE / LOSS_STREAK_BANNED / STOCK_WEEKEND / POOR_HISTORY / CONSENSUS_OPPOSED / NEG_EDGE_*(PF<0.9 四条)/ VENDOR_DIVERGENCE_*。**贪婪降权**:FNG≥70 时多头方向分在三处读取点 −10。

**stop_plan + rr_scan + tp_options**:止损沿对侧结构近→远取第一个带内位(缓冲方向性,带 [1.5×ATR, max(2×ATR(4h),8%)],bstock 全日线标尺);TP 是近/中/远合格结构位菜单(touch_count 中性证据),规则指定 first_rr_ge_target。

**prompt 组装与校验**:记账先于渲染(LimitAnchors/RRCeilings/GateStates 含回踩豁免穿线);双向硬封压成单表;regime-skip 程序合成 wait 省整次 LLM 调用;决策三重吸附(锚 0.05%/止损/TP 逐字)+ stage 全程序派生;复盘规则引擎 hard/soft 可执行化。

### 1.3 执行与风控(trader/)

**生命周期**:beginRun(RunVersion 乐观意图)→ run() 对齐整点 5 分钟边界 → 周期(先平后开再等待)→ defer recover + 监控收尾 + StopIfVersion。**账户级执行互斥**串行化周期/保护监控(30s)/回撤监控(1min)/Stop 扫尾/对账;AI 调用让锁(withoutExecutionLock,panic 安全),返回后锁内刷新权益消除 AI 延迟窗口。**保护监控**(10-01 F06)在周期间隙秒级定稿成交与修复保护。

**市价开仓=例外路径**:marketExceptionGate 要求 bb_ride/突破量能+OI+score≥80,否则降级锚点限价;开仓链=点差→槽位→风险验证(SL/TP/双锚 RR/杠杆-强平距离/止损带)→价值比→风险反推→可负担性→最小仓位→保证金预算;成交确认轮询 + 滑点熔断(WIDE 之外 10-02 加 max_entry_slippage_bps)+ 保护按成交价重锚。

**限价生命周期**:锚点重跑全部风险门;状态机 NEW/PARTIALLY_FILLED/FILLED/CANCELED;部分成交立即按实际均价保护(水位幂等,双腿齐才推进);失效/到期撤单确认终态后才删计划(撤后复检成交)。

**保护与出场**:双腿挂单 + 覆盖制补差(closePosition=∞,qty 求和,0.1% 带)+ 复核;看门狗做状态治愈/记录价修复/裸仓计算保护(SL=1.5×ATR,TP 1:2,TP 带 split 分数);孤儿清扫撤无仓位之腿;出场多层(0.5R 保本/1.2R 减 1/3/2×ATR 跟踪/TP-runner/时间止损/回撤保护);close_reason 归因(价格证据>意图>TTL,stop_loss_slipped 识别滑点止损)。

**对账**:启动影子行重认领 + 离线成交按实际价补保护 + lim- 标签孤儿扫单(所有权读取失败整体中止);每周期幽灵行对账(30min 最小年龄,无价 72h 按开仓价收口 netting_reconcile_stale)。**硬门栈**:置信度→账户熔断→股票周末→内核方向门(限价任何码即拦/市价拦绝对码/回踩豁免与 BTC 过滤为绝对码)→日亏熔断(F8 双层持久锚)→敞口/净方向/单币帽→连亏→时点门+regime-line(限价按锚位评)→min-hold/early-close。

### 1.4 存储与状态(store/)

GORM + SQLite(mattn,**WAL + NORMAL + 4 连接池**,pragma 全在 DSN,空路径/"?" 路径 fail-fast);20 个子存储惰性初始化;decision_records 全审计行(+config_hash 取证);trader_positions(open→sync→归因三层:OwnsEntry/MarkedAfter/分类器;write-once 1R 锚;MAE/MFE 回放;PnL/fee SQL 原子累加);equity 快照;gate_shadow_blocks 反事实(8h/48h,唯一索引防重);entry_assessments(质量分桶→真实胜率,净口径);trade_journal(R 实测修正悲观假设);risk_baselines((账户,UTC 日)首锚为准 + __peak__ 单调);策略乐观锁保存(条件写 + 行数校验 + 版本回传)。

### 1.5 API / 前端 / 基础设施

路由公开/鉴权分组 + 四重 token 校验 + SafeError 包装 + IDOR 双级防护(强制 user_id / show_in_competition);交易员生命周期 loadMu 串行化,保存/更新/删除均为"持久化意图→响应→后台 stop+reload 链"(loadMu 保序,RunVersion 防意图回写);前端 SWR 退避心跳不闩死 + STALE 徽标 + 草稿保护(函数式 updateConfig/选择不覆盖);Telegram 单发送 goroutine + 20 条/分钟滑窗 + 去重;MCP 客户端 per-run 请求上下文(Stop 秒断在飞 AI)+ 流式硬顶;SSRF 双层防御(DNS rebinding 窗口已封,代理下 DNS 失败 fail-closed);JWT 版本化失效。

---

## 二、Bug 评估与处置

### 2.1 已修复(本次 review 周期内,commit 号齐全)

| # | 级别 | 发现 | 修复 commit |
|---|---|---|---|
| 1 | **P0** | 回踩锚死代码:buildLongPullbackPlan 在 sig.Breakout 赋值前调用,永远 nil,功能上线即未运行(测试直调构造器未暴露) | 10-03 手术,顺序重排 |
| 2 | **P0** | BtcTrendCloses 接线仍调 binanceBTC1hCloses(300)——BTC_4H_DOWNTREND 按 1h EMA 判定、BTC_WEAK_LONG 拿币 24h 比 BTC 7h | 357eb802→3cc92cc4 前:接线 4h |
| 3 | **P0** | BTC 1h/4h 收盘共享无标签缓存——命中会串时间尺度(1h 行当 4h 用) | 357eb802:缓存按周期分家 |
| 4 | **P0**(HEAD,并行重构修) | moveStopExchange 在币安 self-heal 后调 CancelStopLossOrdersForSide 会撤掉刚挂的新止损——每次移损后裸奔回内存假象 | 并行会话工作树(快照退役),待其提交 |
| 5 | **P0**(HEAD,并行重构修) | cycleActive 周期 panic 后永久 true,保护监控永久饿死 | 并行会话工作树(executionMutex),待其提交 |
| 6 | **P1** | mixed 源折叠丢 NearHighAlso + ShortScanAtMs(09-17 豁免在生产主力配置又死) | 357eb802→10-04 补折叠拷贝 |
| 7 | **P1** | hasScannerConflict 比对 "up" 而 piggy 发 "breakout/breakdown"——SCANNER_VS_SCANNER 上线即死码 | 357eb802→10-04 改生产字面量 |
| 8 | **P1** | ClosePositionFully 读-绝对写,并发平仓腿丢 PnL/fee | 10-04:SQL 原子累加 |
| 9 | **P1** | R8 回吐漏 long/short 侧向预留(净方向帽保守误挡)+ 保护失败误退款(仓位在场却退预算) | d7b5c200 |
| 10 | **P1** | Gate/KuCoin 止损退役静默 no-op(无接口=无告警,armed 旧腿堆积) | d7b5c200:告警已加;**适配器实现待排** |
| 11 | **P1** | 单向 BOTH 行被退役过滤器跳过(Aster 叠加无 reduceOnly 堆积) | d7b5c200:BOTH 放行;**aster reduceOnly 待排** |
| 12 | **P1** | 决策落库失败重试缺失——审计链断 + journal 富化空白 + 误标 manual | 0a6dc638 |
| 13 | **P1** | BucketStats(质量分→胜率校准)毛口径,系统性高估高分桶 | 16351d79 |
| 14 | **P1** | pending 重算路径不带 LongPullbackEntry → 回踩锚在执行端供给区被拒的拒绝循环 | 10-03:GateState 穿线豁免 |
| 15 | **P2** | 数据质量静态 15m 基条——非 15m 配置全池 DATA_INSUFFICIENT 永久 all-wait | 357eb802 |
| 16 | **P2** | RefreshNow 等待者抢 flag 跑重复扫描;方向无匹配每轮全量刷 | 357eb802(前者)/10-04(后者) |
| 17 | **P2** | regime-line 守卫按现价不按锚价——回踩限价系统性被拒(AGT 类) | 357eb802:限价按锚评 |
| 18 | **P2** | 情绪 Greedy 无条件解引用 Crypto 指针(fetch 在途=nil)→ 崩周期 | 357eb802:nil 守卫 |
| 19 | **P2** | UpdateReview 整行 Save 覆盖并发落地的交易事实 | 16351d79:只写复盘列 |
| 20 | **P2** | pending Upsert 丢 exit_mode(改价重挂沿用旧退出模板) | 16351d79 |
| 21 | **P2** | gate_shadow 双周期竞态产重复开放反事实行 | 16351d79:唯一索引+竞态容忍 |
| 22 | **P2** | journal UNIQUE 检测精确字符串匹配(驱动文案脆性) | 16351d79:子串匹配 |
| 23 | **P2** | calcJournalPnLPct 毛 PnL%/净 USDT 并排混排 | 16351d79 |
| 24 | **P2** | GetPositionStats / history RecentPnL / GetHoldingTimeStats 毛口径残留 | 16351d79 / 10-03 |
| 25 | **P2** | delete 持全局 traderOpsMu 同步 join——跨用户队头阻塞 | 3cc92cc4:异步化 |
| 26 | **P2** | updateConfig 闭包双调用互覆(半套策略切换) | 3cc92cc4:函数式 |
| 27 | **P2** | url_validator DNS 失败 fail-open(代理场景目的端不可验) | 3cc92cc4:fail-closed |
| 28 | **P2** | 网格熔断把瞬态 DB 错误当 100% 回撤 | d7b5c200 之前的批:fail-open+日志 |
| 29 | **P2** | anchorDailyBaseline 存储错误分支发布非持久锚到进程注册表 | 10-04:错误跳过注册表 |
| 30 | **P2** | markAIManaged 吞错(误标手动仓同类) | 10-03:失败日志 |
| 31 | **P2** | 看门狗 TP 修复丢 split 分数(trend runner 被升级全平) | d7b5c200 |
| 32 | **P2** | TP -4130 self-heal 只重挂 full-close(丢 split) | **待排**(binance 适配器) |
| 33 | **P2** | 敞口门 Price=0 时零预留穿过 | d7b5c200:回退行情价 |
| 34 | **P2** | 孤儿年龄兜底不一致(minAge 用 UpdatedAt、stale 用 CreatedAt) | d7b5c200:统一 UpdatedAt |
| 35 | **P2** | 1s 决策间睡眠在账户锁内(监控排队) | d7b5c200:删除 |
| 36 | **P2** | block_short_1d 文案复制时点门 | d7b5c200 |
| 37 | **P2** | 锚/门捕获键用 data.Symbol 而读取用决策符号(前缀场景潜在不对称) | 10-04:统一 Normalize(sig.Symbol) |
| 38 | **P2** | mcp waitForRetry 双取 ctx 竞态 | 3cc92cc4 |
| 39 | **P3** | verifyProtectiveLegs 告警打 "TP: <nil>" | d7b5c200 |
| 40 | **P3** | expired loss-streak ban 本周期仍渲染 Banned(保守向,下周期自愈) | 有意保留 |
| 41 | **P3** | 回踩锚覆写后 LimitEntryOffsetPct 不更新(文档漂移) | 有意保留(legend 禁止重算) |
| 42 | **P3** | AutoStartRunningTraders 死代码 | 3cc92cc4 删除 |
| 43 | **P3** | StopAll 无上限(二次 Ctrl+C 是文档化逃生口) | 有意保留 |
| 44 | **P3** | 回踩路径对 BTC 过滤无豁免(市场级过滤不打折,注释确认设计) | 有意保留 |

### 2.2 有意不修(设计决策,已文档化)

- **NEG_EDGE_RR 在 rr_scan 缺失时 fail-closed**(发射 NEG_EDGE_RR_0.00):宽松版写完又回退——与 funding_rollover/vendor"缺数按不满足处理"的全库约定一致;NEG_EDGE 语境本就是"证据不足即拦"。
- **MCP 流式 4xx 回退的尝试数倍乘(有界 3×3)**:lock-copy 方案因 Client 含 sync.RWMutex 被编译器拒绝;干净方案需要 Client 提供单尝试原语,影响面大收益小。
- **NO_GATE_STATE 对"有持仓但无行情数据"币的限价加仓拒绝**:fail-closed 可辩护,已加注释。
- **回踩路径不豁免 BTC 过滤**:市场级过滤不打折是设计(等回踩≠市场允许),注释确认。

### 2.3 遗留待办(需拍板或适配器工作)

| 项 | 级别 | 说明 |
|---|---|---|
| RefreshNow 同步扫描时长无上界(runOnce 内部超时已限但 AnalyzeMany 总时长可到分钟级) | P2 | 慢币安下周期可能被拖;方向无匹配不再触发刷新已缓解 |
| Gate/KuCoin CancelProtectiveOrder 适配器实现 | P1 余留 | 无能力告警已加;真正按 ID 撤销需适配器工作(仅影响 gate/kucoin 用户) |
| Aster STOP/TP 补 reduceOnly:true | P1 余留 | 无 reduceOnly 的累积风险仍在(单向模式) |
| Hyperliquid GetOpenOrders 恒 LIMIT/StopPrice=0(看门狗反复补挂) | P2 | 适配器能力缺口;建议 watchdog 加能力探测 |
| traderOpsMu 降 per-user | P2 | delete 异步化后队头阻塞已缓解,彻底方案需要 per-user 锁 |
| regime-line 内核镜像(模型可引用的镜像码) | P3 | 锚价评估已解实际伤害;镜像属提示词完善 |
| R8 预留的事务化 | P3 | 当前进程注册表 + 账户互斥已闭环 |
| 前端 26k lint 债 | P3 | 独立治理 |

### 2.4 验证过无问题的关键不变量

锁序单向(traderOpsMu→loadMu→lifecycle→execution)无死锁;运行意图 RunVersion 版本化防回写;回踩锚全链路(时序/抑制豁免/吸附/执行端供给豁免)闭环;情绪降权只打多头;公开面凭证脱敏(SanitizeCredentials)无泄漏路径;净口径无双扣(全部消费方单次减费);Stop 幂等 + sweep-before-join 无在飞下单窗口;F08 日锚双层继承(进程注册表+持久层)正确;SSRF 双层校验(DNS rebinding 已封);pending 混合 upsert 对新旧两种索引形态均正确。

---

## 三、验证

- `go build ./...` 通过;`go vet` 通过
- `go test ./...`:除 `provider/coinank`、`provider/hyperliquid` 两个**真连网络的 provider 测试**因环境网络 EOF 失败外全绿(kernel 463s、trader、store、api、market/breakout 均绿)
- 前端 `tsc --noEmit` 通过;vitest 113/113 通过
- 本次未运行真实资金交易;执行链改动应在下次重启后观察首个周期(告警通道:`stopretirefail` / `MARK FAILED` / `stop_loss_slipped` 等新标签)

---

*审查执行:5 个并行对抗审查 agent + 人工验证;修复 4 commits(10-04)。复现脚本与更早轮次的证据见 `docs/architecture/review-2026-10-01/`。
