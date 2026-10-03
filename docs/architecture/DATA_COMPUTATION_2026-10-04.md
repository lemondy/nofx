# NOFX 数据源与数据标的计算方案 · 2026-10-04

口径基线:dev 分支工作区(含 10-01~10-04 全部修复)。本文回答两个问题:**每个数据源提供什么、怎么算、失败时语义是什么**;**每类标的走哪条数据路由、哪些参数不同**。所有数字与代码一致(文件:行号可查)。

---

## 一、数据源清单

| 数据源 | 提供内容 | 缓存/节流 | 失败语义 |
|---|---|---|---|
| **Binance fapi**(主源,`kernel/engine_data_binance.go` + `trader/binance/`) | K线、OI(30×1h 真实历史)、资金费(前瞻+已结算历史)、多空账户比、top trader 持仓比、taker 买卖比、强平流、突破板块榜、BTC 相关性/β 基准 | OI/量化层 60s~5min 多级缓存;BTC 收盘 1h/4h 各 5min 独立缓存(10-03 分家) | OI 失败 → 非 nil 零值 + `OpenInterestOK=false`(缺数≠真零,R4-6);资金费失败不缓存(响应回显 symbol 防 -1121 假 0) |
| **CoinAnk**(`market/data_klines.go`) | 按交易所视图的 K 线序列(非 Binance 场所的执行数据源) | — | 失败→Binance 直连(仅 Binance 场景)→OpenBB(仅主流币);执行侧(`getExecutionKlines`)永不跨所替换,fail-closed |
| **Hyperliquid**(`provider/hyperliquid/`) | xyz 代币化资产 K 线、持仓、订单簿 | — | xyz 数据路由唯一源;`binanceListsNative` 反查防止 Binance 原生上市股误入 |
| **OpenBB/yfinance sidecar**(`openbb/`) | 三级回退末级(主流币,4h 用 1h 顶) | sidecar 常驻 | 仅作兜底,altcoin 覆盖差 |
| **Alternative.me**(`market/sentiment.go`) | 加密恐惧贪婪指数(日频) | 三源独立 TTL | `Crypto` 指针可 nil(消费方必须 nil-guard,10-03 崩溃修复) |
| **FearGreedChart** | 美股情绪代理(非 CNN 官方):JUNK/MOMENTUM/PUT-CALL/SAFE HAVEN/VOL 五分量加权 | 独立 TTL | 缺数按剩余源判定 |
| **Binance 多空比/资金费市场口径** | BTC/ETH 多空账户比、资金费市场年化(情绪块用) | 30min 组合缓存 | — |
| **CFTC** | 比特币期货持仓周报(投机/套保净多) | 周频 | 慢变量背景,禁止参与精算 |
| **已废弃**:NofxOS key、vergex 浏览器中继 → 由 Binance 原生量化层 + rod headless relay 替代(10-01 确认) |

**K 线回退链**(crypto):CoinAnk → Binance fapi 直连 → OpenBB;执行侧序列不跨所替换。K 线拉取 200~1500 根/周期(data.go:350);**ATR 只用已收盘 K 线**(`closedKlines`)。

**新鲜度与真实性防护**:`isStaleData` 连续 5 根价格冻结+零量 → 拒绝;`VendorStalenessPct` 量化 forming K 线冻结价 vs 交易所 live 价(±80% 外拒绝回补);`VendorDivergence` 硬门(缺数=UNKNOWN 拦)。

---

## 二、标的分类与路由

| 标的类 | 识别 | K线源 | 结构/止损标尺 | 特殊门 |
|---|---|---|---|---|
| **普通加密永续**(BTC/ETH/山寨) | 默认 | CoinAnk(Binance 视图)→直连 | 15m–4h 结构,缓冲/带用 **ATR(1h)** | 全部门 |
| **bstock / EQUITY 代币化股票**(AAPL/TSLA/MU/SNDK…) | `market.IsBStockSymbol`(Binance 股票分类缓存) | CoinAnk + 强制追加 1d | 4h–1d 结构,缓冲/带用 **ATR(1d)**;TP 触及统计也 1d | 止损带 `[1.5×ATR(1d), max(2×ATR(1d),8%)]`;**WIDE_STOP 10% 帽豁免**(自身带治理);`BSTOCK_DAILY_DATA_UNAVAILABLE`(ATR(1d) 缺失不降级);周末禁开(美东六日) |
| **xyz 代币化资产**(Hyperliquid 上市:xyz:SILVER 等) | `IsXyzDexAsset`(含 Binance 原生反查) | Hyperliquid API | 同加密(1h 标尺) | OI 数据源缺失→OI 检查跳过;冒号 chokepoint 防入决策层;路由必经 `GetWithTimeframesForVenue` |
| **BTC** | 硬编码基准 | fapi 直连(1h 相关性 + **真 4h** 趋势过滤,两缓存分家) | 同加密 | 突破扫描强制在列;BTC regime 调分(逆势 ×0.85/震荡 ×0.95);多头市场过滤的基准 |
| **持仓中的币** | trader_positions | 执行侧三周期快照(3m/4h 必取,15m 由 3m 聚合,1h/1d 尽力) | 同上 | 不受候选池 OI 地板约束 |

---

## 三、K 线指标计算方案(每 TF,`computeTFSignal`)

- **结算纪律**:只算已收盘 K 线(forming bar 一律剔除);live 价仅用于"距离/位置"类字段。
- **趋势四象限**(`classifyTrend`):EMA20 vs EMA50 定向(斜率),价对快线定状态 → `up / pullback / down / rally`(对称设计:下跌反弹=rally 是空头入场窗);EMA 间隙 <0.25×ATR 判 range(双向封锁)。
- **OSC 套件**:RSI14(平盘=50 修正)、StochRSI(读未置零头修复)、MACD(`macd_hist` 实为 MACD line/price,字典注明)、ATR14(Wilder,已收盘)、BOLL(20,2,来源值打 BOLLSourced 标签)、量比(288 根基线)。
- **结构位**:swing pivot(≥15m,0.05% 容差去重),对 **live 价**分类支撑/阻力、各封顶 3 个;完整 pivot 集留 `StructuralSupport/Resistance`(非序列化)供硬门;附 `support_dist_pct/`resistance_dist_pct`(锚点/位×100)。
- **趋势窗口收益**:`trend_window_return_pct`(看 return_window_hours)、`price_change_60m/24h_live_pct`(live 口径)、`prev_hour_close_change_pct`(最近完整 1h 收盘)。

---

## 四、衍生品与情绪计算

- **资金费**:`funding_rate` 原始小数;`funding_annualized_pct = rate ×(24/实测结算间隔)×365×100`——结算间隔取**近 N 期结算时间戳中位数**(抗新币 8h→4h 阶跃污染);funding_rollover = 前 3 期费率高于阈值 + 当前前瞻费率回落(`detected=true` 为唯一依据,hint 禁用)。
- **OI**:`oi_change_1h_pct` 真实 1h 变化;`oi_vs_avg_pct` 对 30×1h 均值;USD delta = 基础量变化 × 最新隐含价(防符号翻转)。
- **Long squeeze**(做多对称证据,程序预计算):年化费率 ≤ −5%(空头付费)+ 多空账户比 < 1 + 机构期货净流入 > 0,三者同时成立。
- **情绪综合**(`classifySentimentRegime`):①三源同向极端(加密>80+美股>75+多空比>2+费率年化>50%)=拥挤区;②背离以结构+硬门为准;③`sentiment_trade_effect=CONTEXT_ONLY` 永不覆盖硬门;**贪婪降权**(10-01):FNG≥70(可配)时多头方向分在三处门读取点 −`sentiment_long_deweight_pts`(默认 10)。
- **CFTC**:周报背景,不构成触发。

---

## 五、结构几何与交易计划计算

### 5.1 限价锚点
- 常规:offset = ATRMult×**ATR(1h)** clamp 到 [Min,Max](可配固定模式);buy=价×(1−offset),sell 对称。
- **突破回踩锚**(10-01):1h 突破 `confirmed/retest_hold` 且位仍在价下 → buy 锚=**被破阻力位**(旧阻转支撑),`chase_dist_pct` 记录延伸度;收盘跌回位下 → 状态转 fake_break,锚消失(无追价回退)。
- **供给区抑制**:锚贴对面结构(<呼吸阈值≈0.5×ATR(执行 TF))清零 → LIMIT_ANCHOR_SUPPRESSED(fail-closed,无例外则封锁);回踩锚豁免(内核+执行端双处)。

### 5.2 止损计划(`methodStopPlan`,唯一权威)
- 对侧结构**近→远**步出,取第一个"结构 ± 方向缓冲(**多 0.4×ATR(1h) / 空 0.5×**;bstock 全按 1d)** 落在带内"的真实位;带 = [floor=1.5×ATR(1h 或 1d), cap=max(2×ATR(4h 或 1d), **8%**)]——绝不夹逼到无人区(不 clamp)。
- **WIDE_STOP**(10-02):计划距离 > `max_stop_distance_pct`(默认 10%,负=关)→ 双方向双路径拦;bstock 豁免(自身日线带治理)。
- 失败码:STOP_PLAN_NO_STRUCTURE / STOP_PLAN_OUT_OF_BAND(不分过紧过宽)。

### 5.3 RR 扫描与 TP 菜单(`scanRRForSymbol`)
- 以**同一止损**对全部 TP 侧结构位穷举:`first_rr_ge_target`(规则指定 TP)、`best_rr`(上界,<min_rr ⇒ RR_MAX 拦)、`tp_options` 近/中/远三档合格菜单(非 BOLL 来源、RR≥min_rr),各附 `touch_count`(窗口内触及该位的已收盘 1h/bstock 1d 根数,中性证据)与 `beyond_structure`(历史无参考)。
- 执行端吸附:SL/TP 0.05% 容差逐字校验,tp_option 只能在菜单内选。

### 5.4 突破状态机(`computeBreakoutState`,1h 结构)
`below → approach → broken_unconfirmed → confirmed → (retest_hold | fake_break | extended)`;量能确认=突破 K 量 ≥1.5×;OI 确认=1h OI 顺向扩张。市价例外(追突破)= confirmed+量+OI+|score|≥80+无方向冲突+非连亏+费率不拥挤(年化≤50%)。

### 5.5 Pump 守卫
4h 最近 5 根**已闭合**K 窗口涨幅 ≥20%(默认,可配)且回踩未确认 → `EXTENDED_PUMP_UNCONFIRMED` 拦多;确认=15m 收回 EMA20 上方 + 更高 15m swing low。市价例外另加执行端证据复核+confidence≥80。

### 5.6 bb_ride / short_ride
15m 贴上/下轨骑行(量能 ≥1.5×,连续 K 条件),程序预计算 `ride=true`;是市价例外的第二/三条路径。

---

## 六、候选池与打分(各宇宙)

| 宇宙 | 进入逻辑 | 打分/加权 | 专属门 |
|---|---|---|---|
| **AI500** | 流动性排名 | 通用分 | min-OI 15M USD(可配) |
| **piggy_dash 突破** | 成交量 top + 热门/涨幅/跌幅三榜并集,5min 扫描 | 六维(Price .25/Volume .20/Flow .20/OI .15/Funding .10/Momentum .10;1h×0.65+15m×0.35 合成+共振奖 5)+BTC regime 调分+extended ×0.65(单次截面) | 板块 12min TTL(超龄同步刷,仍超龄源停用) |
| **short_scan 涨幅榜做空** | 24h 涨幅榜 | 九维加权(stretch/overbought/rejection/volume_fade/extension/crowding/parabolic/divergence/structure,默认 0.10×7+structure/crowding 0.15,归一化,调参器可动;**美股盘中 +20%** | 费率拥挤二选一(near_high 豁免)+ 15m 微趋势转向(默认姿态) |
| **slowtop 磨顶 near_high** | 90 日高点 5% 内 + 4h 顶背离,30min 刷新 | — | $30M/日量能预筛替代 OI 地板;缓存 2×TTL 丢弃;碰撞盖 NearHighAlso |
| **hist_gainer 历史涨幅池** | 近 7 天日涨幅 Top20 合并去重 | — | 回落是入选原因非做空结论 |
| **breakdown 破位** | 24h 跌幅榜 + 4h 趋势向下 | — | 顺势反弹做空,仍看结构确认 |
| **hyper_main / hyper_all** | Hyperliquid 主流/全量 | — | 冒号过滤 |
| **static / mixed** | 手工清单 / 多源折叠 | — | 空白条目跳过;折叠保 NearHighAlso+ShortScanAtMs |

**候选统一后处理**:去重折叠 → filterExcludedCoins(XYZ chokepoint + 排除集)→ fetch(场所正确、OI 地板、near_high/NearHighAlso 豁免、数据质量)→ 每币硬门 → 渲染(完整块 or 双向硬封压缩表)→ regime-skip(全拦+持仓锁死=合成 wait,零 LLM)。

**多头专属计算**(10-01/02):EMA20_STRETCH(4h EMA20 距离>12% 封追涨,回踩豁免)、BTC_4H_DOWNTREND / BTC_WEAK_LONG(币 24h 弱于 BTC)、回踩锚、WIDE_STOP(见上)。

---

## 七、风控数字(计算口径)

- **仓位公式**(唯一):notional = equity × risk%(2.0)÷ stop_distance%,下限 min(策略+交易所 lot 双下限),上限价值帽;confidence 只决定开不开,不加仓。
- **敞口**:Σ(数量×|开仓−止损|;未保护仓按 **8% 最坏估计**)+ 本单 + 本周期预留 ≤ 10%(可配);净方向 |多−空| ≤ 6%;保证金预算 (已用+新)≤90%×equity;单币帽(仅 Binance)。
- **日亏熔断**:(账户,UTC 日)首锚为准(注册表+risk_baselines 双层),回撤 ≥10% 停开至次日;账户级 ≥20% 只减仓。
- **滑点**:市价不利滑点告警 100bps;`max_entry_slippage_bps` 可配熔断(超限立即平+记失败)。
- **连亏**:3 连亏/24h → 禁开 24h(程序真值镜像,禁止模型自判)。
- **净口径**:所有统计(聚合/方向/品种/时长/分桶/journal/复盘 AI 行)统一 `realized_pnl − fee`;落库值为交易所毛值,消费方单次减费。

---

## 八、口径与质量约定(全库通用)

1. **缺数语义**:字段缺失 = UNKNOWN = "按不满足处理"(funding_rollover/VENDOR/NEG_EDGE_RR 均此约定);OI 缺数例外地"跳过检查"(缺数≠真零)。
2. **时间戳**:全部 UTC;数据携带其**描述的时间**(SourceAt/数据日),禁止用拉取时钟冒充。
3. **程序字段唯一权威**:模型禁止从原始 K 线重算 RR/止损/结构;hint/榜单是扫描时刻快照,只作辅助证据。
4. **回踩/貼线语义**:限价单的时点/锚位检查按**锚价**评估(非实时价);市价按现价。
5. **证据中立**:scanner 输出/Touch count/历史池标签均为证据非结论,冲突须显式解决(SCANNER_VS_STRUCTURE/SCANNER_VS_SCANNER 等)。

---

## 九、做多 / 做空标的与入场方案(方向详解)

### 9.1 做多(标的来源 → 入场路径 → 门 → 出场)

**多头标的宇宙**:AI500、piggy 突破榜(方向=breakout)、mixed 多源、static 清单;BTC 过滤器(`btc_filter_long`,默认开)要求 BTC 4h 非下跌趋势且币 24h 涨幅 ≥ BTC。

**三条入场路径**(按优先级,程序预计算 `hard_entry_gate.long`):

| 路径 | 触发条件 | 入场价 | 止损 | 说明 |
|---|---|---|---|---|
| ① 突破回踩限价(设计首选) | 1h 突破 `confirmed/retest_hold`,旧阻仍在现价下方 | **被破阻力位**(旧阻转支撑) | 止损计划按位下结构生成,带 [1.5×ATR(1h), max(2×ATR(4h),8%)] | `long_pullback` 块携带证据;延伸 pivots 不算供给;收盘跌回位下→fake_break 锚消失;**EMA20_STRETCH 豁免**(等回踩=设计出口) |
| ② 常规回撤限价(非突破币) | 锚未被供给区抑制(≥0.5×ATR(执行TF) 呼吸空间) | 现价 − ATRMult×ATR(1h)(clamp 到配置带) | 同上 | 常规路径;限价在 15m regime 线正确一侧才放行 |
| ③ 市价例外(两条,均需 `marketExceptionEvidence`) | a) 15m 布林**上轨骑行** `bb_ride.ride=true`;b) 突破 confirmed+量≥1.5×+OI 顺向+score≥80 | 现价(滑点熔断 100bps 告警 / `max_entry_slippage_bps` 可配熔断) | 同① | 费率年化 ≤50% 才允许(多头拥挤侧);EXTENDED_PUMP_UNCONFIRMED 时另需 pump 确认+confidence≥80 |

**多头专属门**(任一失败即拦,码进 failed 可引用):

| 码 | 条件 |
|---|---|
| EXTENDED_PUMP_UNCONFIRMED | 4h 五根闭合 K 窗口涨幅 ≥20% 且回踩未确认(确认=15m 收回 EMA20 上+更高 swing low) |
| EMA20_STRETCH_x_GT_y | 现价高于 4h EMA20 超过 12%(默认),**仅追涨路径**(回踩锚豁免) |
| BTC_4H_DOWNTREND / BTC_WEAK_LONG_x_VS_y | BTC 4h EMA20<EMA50 且价在 EMA20 下 / 币 24h 弱于 BTC |
| WIDE_STOP_x_GT_y | 止损计划距离 >10%(bstock 豁免) |
| MICRO_TREND_NOT_LONG | 最细子小时趋势非 up/pullback(时点门开时) |
| RR_MAX / DATA_INSUFFICIENT / MIN_SIZE_DEAD_ZONE / VENDOR_DIVERGENCE / POOR_HISTORY / LOSS_STREAK_BANNED / CONSENSUS_OPPOSED / NEG_EDGE_* / STOCK_WEEKEND | 通用(见 §5/§8) |

**多头证据徽章**(加分不拦):`long_squeeze.detected`(费率 ≤−5% 年化+多空比<1+机构净流入>0)、`funding_not_overheated`(年化≤50%)、`volume_confirmation`+`oi_confirmation`(突破量能/OI)、trend regime ADX 确认。

**时点**:最细子小时 TF 须 up/pullback;pullback 时 15m regime 慢线必须守住——**限价按锚位评估**(回踩锚在线上方即合法,实时 tick 短暂下穿不拒)。

### 9.2 做空(标的来源 → 姿态 → 入场路径 → 门)

**空头标的宇宙**(全部来自做空扫描,自带九维加权分):

| 宇宙 | 进入逻辑 | 专属豁免/门 |
|---|---|---|
| short_scan(24h 涨幅榜) | 六维+费率+OI 评分,分级 strong≥70 / medium≥55 / weak≥40(**未确认的 strong 降级 medium**——顶部确认是做空的生命线) | 费率拥挤二选一(见下);**near_high 豁免费率条件**(磨顶币费率已正常化,确认信号=4h 顶背离) |
| slowtop 磨顶 near_high | 距 90 日高点 <5% + 4h 顶背离,30min 刷新,$30M/日预筛 | 豁免 OI 地板(量能预筛替代);缓存 2×TTL 丢弃 |
| breakdown 破位 | 24h 跌幅榜 + 4h 趋势向下 | 顺势反弹做空;榜内是入选原因非结论 |
| hist_gainer 历史涨幅池 | 近 7 天日涨幅 Top20 快照合并 | 冲高回落期标的,仍看各维确认 |
| 美股时段加权 | 美东周一至五 09:30–16:00,EQUITY 代币 short_scan 得分 ×1.2 | 加权改排序不改门 |

**默认姿态(稳定规则,勿逐次重判)**:short_scan 按涨幅入选,1h/4h 结构天然偏多——scanner 说可空、结构说多头是**常态而非冲突**。规则:顶部确认信号(顶背离/假突破/破 EMA20/费率 rollover)+ `execution_filter.short_allowed=true`(15m 微趋势已转 down/rally)**两条同时成立**才允许做空;仅确认而 15m 仍 up → `wait + wait_bias=short`,触发条件写"15m 转 down + RECHECK_ALL_HARD_GATES"。

**入场路径**:

| 路径 | 触发条件 | 入场价 | 说明 |
|---|---|---|---|
| ① 限价锚(默认) | 锚通过供给区检查 | 现价 + ATRMult×ATR(1h)(clamp) | 时点:down/rally;rally 时 regime 慢线按锚位守 |
| ② 市价例外 a:15m **下轨骑行** `short_ride.ride=true` | + 费率不拥挤(空头侧年化 ≥−50%) | 现价 | 与 bb_ride 对称 |
| ③ 市价例外 b:confirmed **breakdown** 突破+量≥1.5×+OI 顺向+score≤−80 | | 现价 | breakdown 方向匹配才可借用 |

**空头专属门**:

| 码 | 条件 |
|---|---|
| MICRO_TREND_NOT_SHORT | 最细子小时趋势非 down/rally |
| 费率拥挤二选一(策略规定,支撑证据非必要) | ① 衍生品费率年化 >32.9%(8h 口径每期 >0.0300%,按真实结算间隔换算);② `funding_rollover.detected=true`(前 3 期高+当前回落;hint 里的"费率回落"字样禁用)——两者皆不满足时不要仅因费率理由做空 |
| CONSENSUS_OPPOSED_x | 方向分 ≥+50(多头共识 ≥50 对抗做空) |
| block_short_1d_uptrend | bstock 1d 趋势 up(可配) |
| STOCK_WEEKEND | bstock 美东周末 |
| 其余通用码 | 同 §5/§8 |

**空头证据九维**(short_scan 权重,调参器可动):structure 0.15、crowding 0.15、overbought/parabolic/rejection/volume_fade/stretch/divergence/extension 各 0.10;`NearHighAlso` 碰撞标记穿越全链路。

### 9.3 方向对称性对照(多头 ↔ 空头镜像关系)

| 空头纪律 | 多头对应 | 状态 |
|---|---|---|
| 费率拥挤/rollover | 费率不过热(年化≤50%)+ long_squeeze 加分 | ✅ 双向落地 |
| 4h 顶背离/假突破 | 突破+OI 增+放量(OI增+价涨=偏多推断) | ✅ 双向落地 |
| 磨顶豁免(near_high) | 不追已偏离 EMA20 的币(EMA20_STRETCH 帽) | ✅ 10-01 落地 |
| 暴涨延伸门 | EXTENDED_PUMP_UNCONFIRMED(同门共用) | ✅ 原有 |
| 15m 微趋势时点 | 同门(方向条件镜像) | ✅ 原有 |
| BTC regime | BTC_4H_DOWNTREND/BTC_WEAK_LONG(仅拦多,不拦空) | ✅ 10-01 落地 |
| 突破回踩入场 | 空头侧对称路径(breakdown retest)**未实现**——当前空头只有追破位市价例外,回踩锚仅多头 | ⏳ 待排(用户拍板) |

### 9.4 出场体系(方向共用,程序阶梯独立于 AI 规划)

**仓位公式**(唯一):notional = equity × risk%(2.0)÷ stop_distance%,夹 [min 策略+交易所双下限, 价值帽];confidence 只决定开不开。

**exit_mode 模板**(开仓时选,持仓中不可改):
- `trend`(默认):结构位止盈只平**当时剩余的 50%**,剩余由 2×ATR 跟踪跑单;
- `range`:到目标位全平,不跑单;
- `quick`:到目标全平 + 开仓超 4h 仍浮亏程序时间止损。

**程序阶梯(CODE ENFORCED,独立于 AI,不占 75% partial 上限)**:
1. **0.5R**(breakeven_arm_r,实盘 0.5):浮盈达 0.5R → 止损移至开仓价 **+0.20R**(不减仓,锁微利防噪声);
2. **1.2R**(profit_lock 锁利档,与杠杆无关):市价减仓 **1/3**(一次);执行前必须先把止损收紧到保本或更好;
3. **1R**:无新增动作(止损已在 0.5R 档移至 +0.20R);
4. **ROE 档**(legacy,tp_trim_profit_pct 实盘 15%):R 档未配时回退;
5. **2×ATR 跟踪**:arm 于 1.5× 初始止损距离,只紧不松(≥0.1% 改进才动);
6. **TP-runner 转换**:强趋势冲破 TP 位后固定 TP 转趋势跑单;
7. **回撤保护**:峰值回吐阈值平仓(交易所 SL/TP 与回撤保护不走 AI close 门)。

**AI 平仓门**(串联 AND,任一拦即 hold):min-hold 15 分钟;不足 4h 需 ≥2 根逆势 1h 收盘;浮亏时最近反向结构位 <1.0% 且 15m 未破=不给平("给突破留空间");手动仓一律 hands-off。


---

*整理基线:10-04 工作区;常量逐条对码核验(methodStopBuffer 0.4/0.5、UnprotectedWorstCase 8%、MarketException score 80/funding 50%、PumpGuard 20%、EMA20 帽 12%、WIDE_STOP 10%、降权 10 分/FNG 70、piggy TTL 12min、slowtop $30M/30min/90d/5%)。配套文档:`MODULE_REVIEW_2026-10-04.md`(审查与修复台账)。*
