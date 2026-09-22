# NoFx 量化方案评审(2026-09-22)

- **评审基准**:commit `4fdc6e01`(dev 分支),生产 trader mac-nofx / Conservative Strategy(risk_per_trade_pct=3.5)实盘配置一并核对(data/data.db 只读查询)。
- **评审方式**:五大子系统全量代码走读(候选筛选 / 开仓价 / 止盈止损 / 风控 / 参数迭代),所有结论附 file:line 证据;数据库现状用只读 SQL 核实。
- **评审性质**:**仅评审意见,本文档不构成任何代码改动授权。** 所有问题、建议均为讨论底稿,落地须经用户逐项批准。
- **严重度约定**:P0 = 正在造成资金/口径级错误,应最优先处理;P1 = 方法论级缺陷,系统性影响决策质量;P2 = 应修,影响可度量;P3 = 备忘/低风险。

---

## 0. 总体评价

工程闭环质量高:sizing 从止损反推且只向下钳、保护腿下单后强制核验、看门狗补挂、write-once 1R 锚、config drift 自检、RR 口径统一到 methodology stop(09-19 后)——这些是把"prompt 承诺"变成"程序保证"的正确方向,同规模个人交易系统里少见。

量化方法论层面存在三条主线问题,构成本次评审的核心结论:

1. **约束力分层不清**:相当一部分"门"只存在于 prompt 文案(突破追入六条件、bb_ride confidence≥80、市价例外),执行端无程序复核。对 LLM 决策系统,不写在执行器里的规则不是约束,是建议。
2. **反馈回路有采集无消费**:回测、在线调参、影子拦截、entry_quality、R 分布五套数据基建全部建了采集端,没有一个消费端真正改过阈值;唯一在"自动改参数"的空头调参器,其更新机制有统计缺陷,正在把权重推到边界(已见实据),属于负贡献。
3. **出场结构在系统性截断右尾**:1R 减半 + BE+0.2R + TP 50% + 2×ATR trail 四层机制全部在 1R–2R 区间收割,与实测 avg win +0.48R(PF 0.81 的直接成因)自洽。当前负期望不能只靠"再修 bug"解决,需要出场结构的重新设计并用回测验证——而回测基建目前是空转的。

结论:**该系统当前状态是"工程 85 分、方法论 55 分"**。资金曲线的改善路径明确:先修数据口径(D2),停掉有害的自动调参(A3),把 prompt-only 的门补上执行端复核(B1),再用可靠的回测基建重设计出场(C3)。

### 汇总

| 区域 | P0 | P1 | P2 | P3 |
|---|---|---|---|---|
| A 候选筛选 | 0 | 3 | 3 | 1 |
| B 开仓价 | 0 | 2 | 2 | 2 |
| C 止盈止损 | 0 | 3 | 3 | 2 |
| D 风控 | 1 | 3 | 2 | 0 |
| E 参数迭代 | 0 | 2 | 1 | 0 |
| **合计** | **1** | **13** | **11** | **5** |

---

## A. 候选标的筛选方案

### A1【P1】空头宇宙是幸存者偏差宇宙:只扫"涨过的币"
**证据**:`market/breakout/shortscan.go:34`(24h 涨幅 top50)、`gainer_history.go:14-20`(历史涨幅池,自述动机)、`slowtop.go:100-113`(near-high 磨顶池,90 日高点 <5%);`kernel/engine.go:726-729` 代码注释自认:"Without this the mixed pool is long-only (AI500/OI-top/piggy-dash are all up-side selectors) and the model drifts long."
**评析**:空头候选 100% 由"曾经大涨"定义。回落 >5% 掉出 7 天窗口的币、阴跌破位从未上过涨幅榜的币、两次快照之间完成整个 pump-roundtrip 的币,永远不会成为空头候选。系统没有"24h 跌幅榜空头源"、没有"破位宇宙"。后果:①空头机会集系统性残缺;②唯一在扫的"高位币"恰好是多头情绪最强的品种,逆势做空的选择性压力最大;③候选池方向偏斜随行情 regime 摆动,LLM 的方向暴露跟着池子走而非跟着机会走。
**建议**:补破位/弱势宇宙源(24h 跌幅榜 + 跌破 4h 结构位的币),与现有 short_scan 引擎复用;宇宙构成进 prompt 时标注方向配比,让模型知道自己在什么池子里选。

### A2【P1】回测调参与线上打分口径分裂,阈值选择建立在错误分布上
**证据**:`backtest.go:95-113` 信号分只由 `computeTF+combine` 构成,`sh := &shared{}` 空上下文使 OI/Funding/Flow 维全部中性化;`crowdMaxPenalty`(breakout.go:287-304)、`extendedPenalty`(:308-310)、BTC regime 折扣(scheduler.go:315-326)均只在 `Analyze()` 里,不在回测分数里。而 `bestCutoff` 选出的 strong/medium(backtest.go:321-350)线上套在含全部惩罚的 `rep.SelectedScore`(breakout.go:312 gradeOf)上。回测宇宙仅成交额 top15(scheduler.go:209-212)。
**评析**:调参器在一个"没有惩罚、维度中性化、只有流动性头部币"的分数分布上选最优截断,却应用到一个"有惩罚、维度齐全、主体是涨幅榜山寨币"的分布上。分布漂移下 cutoff 系统性错位——这是参数迭代方案(见 E2)的结构性缺陷在筛选层的直接体现。
**建议**:回测必须重放与线上一致的完整打分路径(含惩罚、含 regime 截面),或至少在 `Analyze()` 输出上做离线重放;回测宇宙应覆盖候选池主力(涨幅榜 alts),不是只看 BTC/ETH 级头部。

### A3【P1】空头在线调参器:重复乘法更新把权重撞到边界,当前参数已被它污染
**证据**:`shorttuner.go:218-234` 每次运行(30 分钟慢 tick)对 14 天保留期内**全部**已评样本重算 pearson 并做 `w *= exp(0.25·corr)`(:251-265),无衰减、无窗口、无显著性检验;头注释声称 "since the last tune"(:24-25)与实现不符。当前实据(`data/breakout_params.json`,2026-09-22T23:23):`overbought=0.2984, parabolic=0.2984`(顶死上限 0.30),`divergence/rejection/structure/volume_fade=0.0298`(趴死下限 0.03)——**structure 权重被从默认 0.15"调"到 0.03**,9 维里 6 维已贴边界。样本为每小时 top-10 的选中样本(shorttuner.go:129-134),以因变量(分数)为条件选择;退市币 outcome=0 且计"已评"(:206-210)。
**评析**:对同一 cohort 反复乘法更新,只要相关系数符号稳定就几何级数撞边界——现在已撞到。权重不再反映组件预测力,而是更新机制本身的伪迹;它直接改变线上排序与 top-N 截断,即当前空头候选的排序部分由一个统计缺陷决定。30 样本门槛只数数量,pearson 按 i.i.d. 计算,横截面选中样本强相关,有效样本远小于名义。
**建议**:**先停用**(config 开关),保数据采集;修复方向:增量窗口(只评新样本)+ 相关系数显著性检验(t 检验,不显著不动)+ 防边界(下限提到 ~0.05 或加熵正则)。修好之前,`structure=0.03` 应人工复位。

### A4【P2】进程级全局旋钮多 trader 互踩
**证据**:`gainer_history.go:97-116` `SetShortScanHistoryConfig` process-global,每 trader 每周期覆写(engine.go:958),last-writer-wins;`data/breakout_params.json` 全进程共享,一个策略的回测调参影响所有策略的 piggy 分级线。
**建议**:配置随 per-trader 扫描请求显式传参;params 至少 per-strategy 隔离或加"唯一写者"约束。

### A5【P2】mixed 联合池无总量上限 + short_scan 重复调用
**证据**:`engine.go:729/774/804` `getShortScanCoins` 被调三处,同一币 Sources 追加两个 "short_scan";逐源 ≤10 但联合无上限,`hyper_main` 默认 20(engine.go:1180-1183)不在 `ClampLimits` 内;仅靠 token 预估整体拒绝兜底(engine_analysis.go:63-80)。
**建议**:合并去重一次取;联合池加总量 cap,超限按分数截断而非整轮拒绝。

### A6【P2】门槛 fail 方向不一致
**证据**:OI 地板 fail-open(OI=0"无法判定→保留",engine.go:982-985;数据层失败跳过过滤,engine_analysis.go:293-300);prompt 侧 funding 条件字段缺失=UNKNOWN=按不满足(fail-closed)。同一候选可在 OI 完全未知的情况下以"通过流动性门槛"的地位进 prompt。
**建议**:统一原则并写进代码注释:流动性/可交易性类门槛缺数据 → 保守保留但**打标**;结论类条件(funding 拥挤判定)缺数据 → 按不满足。OI 未知至少要在候选行标注 `oi_unknown`,不让模型误读。

### A7【P3】横截面可比性与陈旧性备忘
`Percentile` 是扫描集内相对排名(shortscan.go:699-703),扫描集分钟级变化,分数跨周期不可比;hint 最长 5min+2min TTL 陈旧;多头六维分与空头九维分是两套不可互比的量纲。均不致命,但做截面统计(如影子校准)时要把这些当作噪声源对待。

---

## B. 开仓价计算

### B1【P1】市价例外通道是 prompt-only,执行端零复核
**证据**:`executeOpenLongWithRecord`/`executeOpenShortWithRecord`(`trader/auto_trader_orders.go:58-198, 201-340`)的完整闸门清单:点差 → 槽位 → 重复仓 → validateOpenRisk(SL/TP/RR/带宽)→ 名义比例 → clampSizeToRisk → 可负担 → min size → 保证金预算 → 下单。**没有任何 breakout 六条件 / bb_ride / pump 证据的程序复核**。六条件只存在于 prompt(`engine_prompt.go:227`)与 kernel 硬门快照;bb_ride 的 confidence≥80 门槛只在 prompt 文案。配套阈值也偏弱:OI 确认仅 `>0`(`signal_layer.go:2305-2311`,任意正增量即过),量能 1.5×20bar,bb_ride surge 是"连链中任一根"放量(`bb_ride.go:81`)。
**评析**:limit_entry 是默认路径,市价本应是"证据齐全才放行的例外"。现状是例外条件约束不了系统,只约束"听话的模型"——模型只要输出 `open_long`(市价)且通过时机门,就直达交易所。这使"默认限价"这层风控对不守规矩的输出形同虚设,也使突破追入六条件无法从数据上被验证(执行记录里根本不知道模型当时有没有满足六条件)。**已核实:这是当前代码的实际行为,不是文档过时。**
**建议**:市价开仓执行入口加一道程序复核:`marketExceptionEvidence` 为真才放行市价,否则强制降级为挂锚点限价(wait);把六条件的布尔结果落进 actionRecord,让 A 影子数据集可以覆盖市价例外。

### B2【P1】ATR 尺度与数据源三分裂,同一参数在不同路径含义不同
**证据**:①偏移锚 = ATR(1h)(`anchor_offset.go:100-111`),呼吸阈值 = 执行周期 ATR EXACT TF(`anchor_offset.go:124-132`,缺 TF→0→固定回退);但 prompt 侧呼吸阈值仍走 `primaryTFSignal`,主周期缺失时**回退最长可用周期**(`signal_layer.go:970-973` + `:2003-2011`)——同设置两侧可按不同阈值判定(09-19 同类 bug 只修了执行侧)。②执行端止损带 `oneHourATRPct` 有 11 档静默回退链 `1h→2h→4h→…→3m`(`auto_trader_risk.go:622-632`),1h 缺失时噪声下限静默换成 4h/8h 尺度。③偏移与呼吸共用同一 `ATRMult` 与同一 clamp [0.15, 1.2](`anchor_offset.go:87-93` vs `:158-164`),而 ATR(15m)≈ATR(1h)/3,同一 clamp 对两个尺度的约束力完全不同。
**评析**:ATR 是全系统最重要的标尺,现在它有三个数据源、两套回退语义、一套共享 clamp。每次"为什么这个币没开成/开进了"的诊断成本都在为这个分裂买单,且 prompt 与执行可能对同一币做出不同判断(一致性靠数据到达时点的运气)。
**建议**:统一为一个 `VolYardstick` 结构(明确:锚/呼吸/floor/cap 各用哪个 TF、缺数据时统一回退表),prompt 与执行共享同一份计算结果快照而不是各自重算。

### B2 附注【P2→并入 B2 处理】锚点穿越市价回退无滑点保护
**证据**:限价带校验 [0.1%, 5%] 失败且 crossed 且未破 SL 时转市价(`auto_trader_pending.go:224-252`),无滑点预算、无保护价;成交后 `reanchorProtectivePrices` 平移 SL/TP(`auto_trader_risk.go:568-582`),RR 失真无复核。
**评析**:锚点被穿越本身是动量信号,"回踩到了"常是趋势延续的开始——市价回退等于在最不利时刻追入。方向上这个功能是对的(否则锚点永久错过),但至少要有:回退只允许在 crossing 后 N 分钟/または 价格距锚 < X%ATR 窗口内、市价单带 SL 保护价(binance 支持上限价的部分场景)或成交后立即校验 realized RR≥min_rr×(1-容差),不满足即离场。

### B3【P2】挂单成交不重验硬门
**证据**:放置时的 RR/pump/timing 结论沿用到成交,挂单寿命 `max(30min, N×interval)`(`auto_trader_pending.go:482-494`);FILLED 路径(:427-431)只处理保护单,不重验任何门;SL 被穿越失效撤销(:440-448)是唯一动态失效条件。
**评析**:30 分钟足够让一次 setup 失效(结构破了、pump 结束了)。限价成交意味着价格回到了锚点——这既可能是"回踩到位"也可能是"趋势反转先到",两解读下旧结论的有效性完全不同。
**建议**:成交时以成交价重跑 checkRR + 时点门(轻量,数据已在手),失败按"即时平仓/保留观察"可配置。

### B4【P3】price=0 抄锚点
模型输出 price=0 时 `correctLimitAnchors` 直接把锚点抄给模型(`engine_position.go:54-62`)——合规逻辑变成"替模型下单"。既然锚点本来就是程序预算的,这未必错,但应在决策记录里标注 `price_source=anchor_copy`,避免复盘时误以为是模型判断。

### B5【P3】pump guard 市价路径无重查
`computePumpGuard`(4h 涨幅 ≥20% 且未确认→拦多)已接 kernel 硬门(`signal_layer.go:1273-1275`),执行侧重算只在限价路径 supply-zone 重算时顺带传入(`auto_trader_pending.go:269`);市价路径无 pump 重查。FIL/SAGA 教训(-5.2%/3.5min)在市价例外通道下仍可复现。并入 B1 一起修。

---

## C. 止盈止损点位计算

### C1【P1】TP 结构位供给薄弱且混入浮动水平,"逐项检查全部结构位"名不副实
**证据**:S/R 每 TF 每侧截断最近 2 个(`signal_layer.go:1689-1691, 1699-1728`),不足时补 BOLL 上下轨(`:1693-1704, 1718-1726`,代码里还残留 `if true {}`);15m/1h/4h 三 TF 满打满算每侧 ≈6-8 个候选位,其中含**随时间衰减的布林带值**。`scanRR` 的 `first_rr_ge_target`(:1332-1418)从这些位里取第一个达标者——TP 触发价可能是持有期内会漂移的 BOLL 而非固定结构。
**评析**:止盈规则 09-11 教训是"漏看结构位",修复方式是"强制逐项检查"——但被检查的清单本身只有 2×TF+BOLL 个元素且含动态水平。min_rr=1.5(现配)下,first_ge_target 对 0.05% 去重容差和 BOLL 补位敏感;把 BOLL 当 TP 锚等于把出场交给一个周期为 20 根 K 线的移动目标。
**建议**:①TP 候选位只允许固定结构位(pivot),BOLL 只做"延伸目标"标注不做 first_ge_target;②每 TF 保留数提到 3-4 或不截断(pivot 数量本来就有限);③`first_target_beyond_structure=true` 的目标目前仅标注不阻断——它意味着 TP 在全局极值之外(价格从未到过的地方),至少应要求模型显式确认。

### C2【P1】止损带 floor(1h)/cap(4h) 尺度不对称缺乏统计依据;8% cap 与 min-size 死区双向挤出高波币
**证据**:floor = `sl_min_atr_mult × ATR(1h)`(1.5×现配,`signal_layer.go:1081-1089`),cap = `max(2×ATR(4h), 8%)`(`:1168-1176`,注释自认"Not a typo"但"why"只有一句话);执行端镜像(`auto_trader_risk.go:487-536`)。高波币 4h ATR>4% 时 cap=2×ATR(4h) 可达 15-20%+,1.5% 风险 ÷ 15% 止损 → 名义仅 10% 权益,易触发 `MIN_SIZE_DEAD_ZONE`(`signal_layer.go:1021-1042`)弃单。
**评析**:floor 用细尺度、cap 用粗尺度的直觉(下限要灵敏、上限要稳定)讲得通,但带宽 floor/cap 之比随币种波动率期限结构变化,同一 mult 在不同币上的"结构可容纳度"不可比——没有任何统计归一化,也没有任何回测证据支撑 1.5/2/8% 三个数的组合。更实际的后果是:**高波币被 cap 挤出、低波币被 floor 撑宽,候选池在无人决策的情况下系统性偏向中低波币**。这是隐性的 selection,不影响正确性但影响收益分布的构成,至少应该被看见。
**建议**:用影子数据集(见 E1)按 ATR 分桶统计被拦方向的 would-be 结局,验证 8% cap 与 dead zone 是否在系统性丢弃正期望方向;cap 或可改为 `min(max(2×ATR(4h), 8%), 结构实际需要)` 的显式弃单理由分流(OUT_OF_BAND 已有,保留并统计)。

### C3【P1】出场阶梯四层机制全部在 1R-2R 区间收割,右尾被结构性截断
**证据**:①1R 锁(锚=write-once `initial_stop_loss`):到达 1R → 市价减 50% + SL 移 BE+0.2R(`auto_trader_vol.go:184-256`);②TP 只平 50%(`tp_close_fraction` 默认 0.5,`anchor_offset.go:310-330`);③runner 由 2×ATR(1h) trail 接管,arm 在 1.5×初始止损距离,improve 阈 0.1%(`vol.go:23-31`,三参数硬编码);④ROE 25% full 档**不受 lock 让位保护**(`TpTierAction` full 优先,`anchor_offset.go:203-206`)——5x 杠杆下 5% 价格冲高即触发全平。实测基线:86 笔,胜率 47.7%,avg win +0.48R,avg loss -0.70R,期望 -0.14R,仅 1 笔 ≥+2R。
**评析**:四层机制同向叠加:任何一笔交易想在右尾留 2R 以上,需要先躲过 1R 减半、再躲过 50% TP、再被 trail 只回吐 2×ATR——概率极低。avg win +0.48R 不是模型不行,是**出场设计不允许大赢存在**。与此同时亏损侧被三门(early-close/breakout-hold/min-hold,见 C6)延长持有,左尾靠 SL 兜底。±R 不对称 + 胜率不足 50% = 负期望的机械成因。09-21 实验方向(TP 半仓+runner)是对的,但 runner 的 trail 参数(arm 1.5R/trail 2×ATR/improve 0.1%)与 1R 减半的组合仍然保守,且**这些参数全部无回测支撑**。
**建议**:明确这组实验的度量指标(2 周后重测 R 分布右尾)之外,把"runner 段的出场"单独参数化并用 C4/E2 的回测基建扫参(arm∈{1.5R,2R,3R},trail∈{1.5,2,3}×ATR);ROE 25% full 档在 lock 激活时应与 trim 档一同让位(现在是 5x 杠杆下一个 5% 冲高就把趋势单全平了,与"趋势跑单"设计直接矛盾——**这条建议优先级高,但它改行为,等批准**)。

### C4【P2】1R 锚 write-once 的污染窗口
**证据**:重启后首见时 `initialStopAnchor` 解析顺序含"live recorded stop 兜底并冻结"(`vol.go:302-330`);看门狗状态自愈同样从交易所 stop 回种锚(`auto_trader_risk.go:1188-1196`)。若交易所 stop 已被 AI tighten 或手动移动过,锚即被污染(ONDO 教训在重启窗口重现)。裸仓保护路径的锚 = 1.5×ATR 计算止损(`:1323-1326`),与结构 stop_plan 的 1R 分母量级不同,两者触发的"1R 减仓"不可比。
**建议**:回种时只在 DB 无锚且记录价与交易所价一致(±容差)时冻结;裸仓路径的锚单独打标(`anchor_source=computed_fallback`),统计时分层。

### C5【P2】RR 门是全额仓位单 TP 点估计,实现是分批出场
**证据**:`checkRR`(`auto_trader_risk.go:552-566`)按全额、单 TP、单 SL 计算;实际出场 50%@TP + 50%@trail(或 1R 先减半)。realized RR 分布 ≠ gate 的点估计。
**建议**:不必改门,但影子数据集裁决时应按分批模型重算 would-be R(50%@TP + 50%@horizon mark),否则校准出的 min_rr 系统性偏离实现。

### C6【P2】亏损侧出场摩擦不对称
**证据**:early-close(持仓 <4h 需 ≥2 根 1h 逆势收盘 K,`auto_trader_risk.go:848-912`)+ breakout-hold(仅拦**浮亏** close,`closegate.go:107-146`)+ min-hold 10min 三门都只作用于平仓(亏损方向),浮盈单不受限。
**评析**:损失实现被延后、盈利实现被阶梯加速。设计意图(防 AI 恐慌卖飞)成立,但组合效果是左尾时长拉长(资金占用+情绪成本+隔夜风险),幅度上 SL 兜底不变。配合 C3,系统在双向都"不利于大 R"。这不必立即改,但应纳入 C3 重设计一起权衡。

### C7【P3】注释漂移备忘
`stopFloorPct` 注释写死 "1.5×ATR(1h)" 而实际乘数来自配置(`signal_layer.go:1081`);`strategy.go:347` MinRiskRewardRatio 注释 "CODE ENFORCED" vs `:604` "(AI guided)" 自相矛盾。按项目教训(锚点注释过时事故),注释里的数字一律删掉或 Sprintf。

---

## D. 风控方案

### D1【P0】equity 口径混用:Binance 路径的 sizing/保证金预算分母实际是钱包余额(不含浮动盈亏)
**证据**:`binance/futures_account.go:32-35` GetBalance 只产出 `totalWalletBalance/availableBalance/totalUnrealizedProfit`,**从不产出 `totalEquity`**;而执行端取 equity 的顺序是 `totalEquity → totalWalletBalance → availableBalance`(`auto_trader_orders.go:107-115`,pending.go:292-299)——第一档永远落空,实取钱包余额。另一边,账户熔断与 prompt 用 `wallet+unrealized`(`auto_trader_loop.go:486-492`)。余额还有 15s 缓存(`binance/futures.go:103`)。
**评析**:持仓浮亏时 wallet > equity → 保证金预算门与 sizing 分母偏大,实际风险被低估;浮盈时反向过度收紧。同一个周期内两个闸门用两个口径,账户熔断的触发点也随之漂移。这不是理论风险:`clampSizeToRisk` 的风险上限、`marginBudgetBlocksOpen` 的预算上限都建立在这个分母上。**一处数据源补一行 `totalEquity = wallet + unrealized` 即可修复。**
**建议**:P0 修复:GetBalance 填 totalEquity(或执行端统一改为 wallet+unrealized 并注释口径);同时明确预算门用哪个口径(建议 equity 含 uPnL,更保守)。

### D2【P1】组合风险敞口:3.5%×5 仓×相关标的,账户级唯一兜底在 -20%
**证据**(实盘现配):`risk_per_trade_pct=3.5, max_positions=5, altcoin_max_position_value_ratio=5`(Conservative,data.db);连亏熔断按 symbol 独立计数(`lossstreak.go:132-138` filtered per-symbol),不同币各 3 连亏可同周期并发;账户级熔断 `account_max_drawdown_pct=20`(`auto_trader_risk.go:1504-1526`)是唯一账户级闸;`dailyPnL` 每日重置但无任何消费方(loop.go:82-87)——**日亏停机设计实际不存在**。
**评析**:满载单周期止损总风险 = 5×3.5% = 17.5% equity,且候选高度同质(A1:同一批涨幅榜币,相关性≈1,常同向同亏)。从 17.5% 满载风险到 20% 熔断之间只剩一次满载连亏的余量。连亏熔断的语义也有损耗:盈利 1 笔清零重启计数,ban 期内亏损不刷新,真实连亏策略每 symbol 每 24h 已实现 N×3.5%≈10.5% 才 ban。
**建议**:①加账户级风险敞口闸(Σ未平仓风险 ≤ 账户风险预算,如 8-10%),比"仓位数"更本质——它天然处理相关性;②恢复日亏停机(dailyPnL 已算好,缺一个消费闸);③3.5% 单笔风险是用户拍板的配置,评审不反对,但它使上述两条从"建议"升级为"必要"。

### D3【P1】trader_positions.leverage 全量=1,复盘/反馈回路吃脏数据;强平距离零校验
**证据**:`store/position_builder.go:71` 硬编码 `Leverage: 1`;binance 走 OrderSync 不走本地带杠杆落库(`auto_trader_decision.go:283-287`);实库 185 行全部 leverage=1(只读查询核实)。后果:`journal.go:182-183` 保证金口径按 1x 虚增,journal ROI/复盘失真——而 POOR_HISTORY(≥5 笔 <35% 胜率)、preTradeRuleCheck、entry_quality 对齐全部消费 journal。强平距离无任何闸门比较"SL 距离 vs 强平距离"(仅 grid 有);drawdown 监控在 leverage 字段缺失时默认 10x 计算 PnL%(`auto_trader_risk.go:147-150`),TP 阶梯与 drawdown-protect 建立在这个杠杆化 PnL% 上。
**评析**:这条的隐蔽性在于:风控执行不受影响(usedMarginOf 读交易所实时杠杆),受影响的是**所有基于 journal 的统计与反馈**——R 分布、POOR_HISTORY、entry_quality 对齐,全是参数迭代的输入。垃圾进垃圾出,而且现在的 expectancy 悲观偏差(-1R 假设,见 E1)叠加杠杆口径失真,模型每周期看到的"自身战绩"是双重失真的。
**建议**:OrderSync 回填真实杠杆(binance position risk 接口有 leverage 字段);journal 的保证金/ROI 重算;加一条 SL-vs-强平距离预检(1.5R 止损距强平 <20% 时拒单或降杠杆)。

### D4【P1】fail-open 点位清单(汇总)
单点都有日志,但系统性看,"查询失败=放行"的闸门偏多:

| 闸门 | 位置 | fail 行为 |
|---|---|---|
| 点差门 | `auto_trader_risk.go:731-739` | book 不可用/查询错 → 放行 |
| 保证金预算 | `:827-829` | GetPositions 错误 → 放行 |
| 已用保证金 | `:766-771` | mark/lev 异常仓位跳过(undercount) |
| 连亏熔断 | `lossstreak.go:128-131` | store 错误 → 不拦 |
| 看门狗 | `auto_trader_risk.go:1181-1182` | GetOpenOrders 失败 → 跳过该仓 |
| OI 地板 | `engine.go:982-985` | OI=0 → 保留 |
| ATR 全缺 | `auto_trader_risk.go:518` | floor 失效,仅剩 cap=8% |

**建议**:不必全改 fail-closed(停摆成本也真实),但应分级:资金安全类(保证金预算、熔断)失败时应拒绝新开仓直到恢复;信息类(OI)保留但打标。至少把"本周期 fail-open 命中次数"做成可查指标,避免静默退化。

### D5【P2】口径与时点杂项
①账户熔断用周期初快照 equity,执行端保证金门用执行时 GetBalance——同周期两口径两时点;②`initialBalance` 静态 DB 值,出入金不重置基线,熔断阈值会随出入金漂移;③同周期先执行的开仓占用保证金,同批后面的开仓被拒——保守方向可接受,但 AI 看到的与执行结果可能不符,可在决策记录标注 intra-batch margin consumption。

---

## E. 参数迭代方案

### E1【P1】五套校准数据全部"有采集、零消费";唯一闭环(空头调参器)有害(A3)
**证据与现状**:
- **gate_shadow_blocks**:54 未评、15 sl_first、39 timeout、**仅 2 tp_first**(只读查询核实)。8h 视界对结构位 TP 太短(70% timeout),`win_rate` 分母仅 17——目前即使接上闭环也不足以校准 RR_MAX/CONSENSUS/VENDOR。代码自述"Purely observational: nothing here feeds back"(auto_trader_shadow.go:19);RR_MAX/CONSENSUS±50(`signal_layer.go:1310`)/vendor 1%(`anchor_offset.go:356-367`)三个阈值自设立以来从未被数据修订。**积极信号**:已裁决的 MICRO_TREND/CONSENSUS 拦截方向 12 sl_first vs 2 tp_first——闸门当前可能在正确拦损,但样本太小不足为凭。
- **entry_quality**:12,313 条(67% <60 分,80+ 仅 80),BucketStats 的结局匹配是"同 symbol+side 之后任意下一笔 journal 交易"(entry_assessment.go:130-145),未验证该笔确实出自该决策;唯一消费方是展示 API。且 prompt 强制 `confidence == entry_quality` 同值(engine_prompt.go:237)——单变量自报,区分度为零。
- **R 分布**:86 笔基线是人工一次性分析,无自动化重算;`trade_journal` 有 PlannedStopLoss/PlannedTakeProfit,逐笔 R 可推导但无代码做。
- **expectancyR** 假设输家恒 -1R(engine_prompt.go:1982-1992),与实测 -0.70R 矛盾——prompt 每周期向模型播报系统性悲观的自身战绩,且 PF<0.9 → NEGATIVE_EDGE 标签建在其上。
**建议**:①影子视界改双档(8h + 48h),timeout 桶按 mark-to-market 单列,先积累到每阻断码 ≥30 裁决样本再做第一次阈值校准(校准对象首选 min_rr 与 CONSENSUS±50,两者都有明确的数据语义);②R 分布做成周期性任务(journal 已有 Planned SL/TP,纯计算),其结果替换 expectancyR 的 -1R 假设;③entry_quality 要么让模型给独立第二变量,要么承认它就是 confidence 并停止重复统计。

### E2【P1】回测基建无效:饥饿空转 + 无成本模型 + in-sample argmax + 无样本外验证
**证据**:最近一次回测(2026-09-17)signals=4,四档 count 全 0,changes=null(`data/breakout_backtest.json` 实查);`btMinSample=40` 但信号 stride 3、前瞻 96 根 15m 重叠,有效 N 虚高;`bestCutoff` 同窗口 in-sample argmax(backtest.go:321-350),无 train/test 切分、无 walk-forward(全仓 grep 无命中);forwardReturn 纯收盘价,**无手续费/滑点/资金费成本**(backtest.go:138-151,grep fee/slippage 零命中)——15m 级信号 0.1-0.3% 的 in-sample edge 大概率是成本前幻觉;config.db 的 `backtest_runs/metrics/decisions/trades/equity/checkpoints` 全套表 **0 行且无任何 Go 代码引用**(死 schema);weekly tune 一次 pass 同时动 4 参(阈值改变重划"赢家"→再用新标签算中心,自举无归因)。
**评析**:这是全系统最大的基建缺口:出场重设计(C3)、阈值校准(E1)、筛选宇宙评估(A1)全部需要"能忠实重放打分+含成本+样本外"的回放器,而现在它不存在(存在的是上面这个饥饿的近似物)。相较之下,K 线数据、打分函数、结构位计算都是现成的,补齐的真实工作量可控。
**建议**:按优先级建一个最小可靠回放器:①历史 1h/15m K 线重放 `Analyze()` 完整路径(含惩罚/regime);②双边 taker 成本 + 0.05% 滑点 + funding;③walk-forward(如滚动 30 天训练/7 天验证);④一次只动一个参数或做简单的网格+验证集选择。现 weekly tune 在此之前应视为无效通道(它本来也因饥饿没改过任何参数)。

### E3【P2】常数无校准通道清单 + prompt 口径杂项
以下关键参数无任何数据反馈路径,属"手拍常数":tp_trim/full 10/25、profit_lock 1R/BE+0.2R、peak_drawdown 15/45(实配)、pump_guard 20%、trail 三参数、min_hold 10min、risk_per_trade 1.5/3.5。短期不必全接校准,但应有一张"参数→反馈数据源→下次校准条件"的表,防止常数永久化。另:英文 prompt 统计标题硬编码 "(30d rolling window"(`engine_prompt.go:537`,中文版已动态化);stats_window_days 改值后英文模型看到错误口径。

---

## 建议处理顺序(待批准后执行)

1. **D1(P0)**:GetBalance 补 totalEquity——一行级修复,立刻让 sizing/预算/熔断三处口径归一。
2. **A3**:停用空头在线调参器 + 人工复位 short_weights(它正在主动污染排序)。
3. **B1**:市价开仓执行入口加例外证据复核(+B5 pump 重查)——把"默认限价"从 prompt 承诺变成程序保证,同时给影子数据集补上市价例外的可观测性。
4. **D2/D3**:账户风险敞口闸 + 日亏停机消费 dailyPnL + leverage 落库修复(数据反馈回路的地基)。
5. **E2**:最小可靠回放器(成本+walk-forward+完整打分路径)——后续一切参数工作的前提。
6. **C3/E1**:在 5 的基础上重设计出场阶梯,并用双视界影子数据做第一次 min_rr/CONSENSUS 校准。
7. 其余 P2/P3 按批次捎带。

## 评审未覆盖范围

LLM prompt 工程(仅审了与执行一致性相关部分)、grid 引擎、OKX/其他交易所适配层、前端、安全(鉴权/密钥)、multicoin 并发调度性能。如需可另立批次。

---
*评审人:ZCode(量化视角全量代码走读)。本文档为评审意见,未修改任何代码;任何一条的落地均以用户批准为前提。*

---

## 附:落地状态(2026-09-23 更新)

经用户批准逐项改进后落地。每批独立 commit + 回归测试,全仓 `go build ./... && go test ./...` 通过。

| 评审项 | 状态 | Commit | 摘要 |
|---|---|---|---|
| D1(P0) equity 口径 | ✅ 已修 | `6e57e9b6` | 所有交易所 GetBalance 补 `totalEquity = wallet + uPnL`(binance/okx/gate/aster/bitget/stocks);bitget 的 snake_case `total_equity` 无人消费,已替换 |
| A3 空头调参器 | ✅ 已停 | `ebf9f812` | 权重更新默认关闭(`short_tuner_enabled`,nil/false=停),采样与评估保留;`data/breakout_params.json` 权重复位为设计默认;修复待做(增量窗口+显著性检验) |
| C1 TP 结构位 | ✅ 已修 | `38cb4540` | 每 TF 每侧截断 2→3;BOLL 值打 `BOLLSourced` 标记,`scanRR` 的 `first_rr_ge_target` 不再取 BOLL(仍进 best_rr);BOLL-only 达标→unusable |
| E3/C7 杂项 | ✅ 已修 | `38cb4540` | 英文 prompt 统计标题动态化;`stopFloorPct` 注释去硬编码;min_rr 注释矛盾修正;`if true {}` 移除 |
| B1(+B5) 市价例外 | ✅ 已修 | `f7917fab` | kernel `marketExceptionEvidence` 方向匹配 + 突破腿要求 directional_score≥80(常量 `MarketExceptionMinScore`);执行端 `marketExceptionGate`:有证据+confidence≥80 放行;无证据+有锚→降级锚点限价(`MarketDegraded` 阻断穿越回退复活市价);无锚→fail-closed 拒单;市价模式策略豁免;prompt 已宣告 CODE ENFORCED |
| A5/A6 候选池 | ✅ 已修 | `824cf5a9` | `getShortScanCoins` 三次调用合一,`appendUniqueSource` 全源去重;OI 未知候选显式标注「OI数据缺失:流动性门槛未能核验」 |
| D2 账户风控 | ✅ 已加 | `84279f0d` | ①`max_account_risk_pct`(默认10,负=关):Σ持仓止损风险(无保护仓位按 8% 上限最坏估计,`UnprotectedStopWorstCasePct` 单一来源)+新单风险 ≤ 权益%;②`daily_max_loss_pct`(默认10,负=关):日内回撤熔断,拦开仓不拦平仓;两者均入 applyHardRiskGates + prompt |
| D3 杠杆真相 | ✅ 已修 | `23f907ba` | 每周期从交易所实况回填 OPEN 行真实杠杆(185 行全 1x 的病灶);`validateOpenRisk` 新增强平距离预检(止损距离 ≥ ~1/杠杆×0.9×80% 拒单) |
| B3/B2附 成交复核 | ✅ 已加 | `fe62d855` | 限价成交报告 realized RR(<min_rr/2 告警);市价成交对比校验价 vs avgPrice 的 RR 劣化告警(含 bps 滑点)。纯可观测性,不改行为 |
| C3 tp_full 让位 | ✅ 开关就绪 | `2d823b8e` | `tp_full_yields_to_lock`(默认关=现状,避免污染 2 周 R 分布重测;用户可开) |
| E1 校准闭环 | ✅ 部分落地 | `b463a56e` | ①expectancy_r 改实测(journal R 分布,≥5 笔;标签注明口径);②影子拦截双视界(8h `outcome` + 48h `outcome_48h` 自动迁移,`/api/gate-shadow-stats` 出 `overall_48h`/`by_code_48h`) |
| A1 空头幸存者偏差 | ⏳ 待专项 | — | 需新增破位/弱势宇宙源,涉及扫描引擎扩展 |
| A2 回测口径分裂 | ⏳ 待专项 | — | 依赖 E2 回放器(回测须重放完整 Analyze 路径) |
| A4 全局旋钮互踩 | ⏳ 待专项 | — | per-strategy 参数隔离,涉及 params 存储结构 |
| B2/B4 ATR 尺度统一 | ⏳ 待专项 | — | VolYardstick 结构统一三处数据源,牵涉面广 |
| E2 回测基建 | ⏳ 待专项 | — | 成本模型+walk-forward+完整打分路径重放,最大的独立基建项 |
| C6 出场摩擦不对称 | ⏳ 待拍板 | — | 与 C3 出场重设计一并权衡 |

**注**:全部改动尚未重启进程生效(当前 live 进程仍是旧二进制)。重启前确认:.env 代理变量(HTTPS_PROXY/HTTP_PROXY=127.0.0.1:7890)需显式携带;重启后新闸门(max_account_risk_pct=10、daily_max_loss_pct=10)默认生效,Conservative 3.5%×5仓配置会首次受到敞口约束——若满载 5 仓 3.5% 风险(17.5%)时,第 5 单之后的开仓将被拒,这是设计行为。

---

## 附二:专项任务落地(2026-09-23 第二批)

| 专项 | 状态 | Commit | 摘要 |
|---|---|---|---|
| B2 ATR 尺度统一 | ✅ 已修 | `b353b797` | prompt 侧锚点偏移+呼吸阈值改为与执行端完全一致的精确 TF 查找(旧 fallback 静默换最长周期);1h ATR 11 档回退链降尺度时打日志(仅开仓路径) |
| A4 参数互踩 | ✅ 已修 | `54ea86f1` | 废除 SetShortScanHistoryConfig 进程级旋钮;历史池参数随 `ScanShorts(limit, days, max)` 传参;共享缓存按 (universe\|days\|max) 分键,不同策略各占 2 分钟槽位 |
| A1 空头幸存者偏差 | ✅ 已修 | `13812acf` | 新增 breakdown 破位宇宙(24h 跌幅榜 top30,零额外 API):4h 趋势闸(EMA20<EMA50 且价在其下)+ 透明复合分(结构 .30/下行空间 .25/反弹弱 .15/背离 .15/资金费健康 .15),未确认硬压 medium;prompt 图例已更新 |
| E2+A2 回测基建 | ✅ 已修 | `9e9560e6` | 回放分数补齐可回放的线上惩罚层(extended ×0.65 + BTC regime 时间线回放);远期收益改为净值(0.20% 往返成本=2×5bps taker+2×5bps 滑点);**walk-forward 协议**:阈值在 oldest-70% 选出,须在 newest-30% 验证(桶净边>0 且高于测试段均值,n≥15)否则拒绝;宇宙 15→30;修复 Grade 从未赋值(by_grade 恒空)的 bug |
| C6 出场摩擦不对称 | ⏸ 待拍板 | — | 见下方决策备忘 |

### C6 决策备忘(需用户拍板,未改任何行为)

**问题回顾**:early-close(<4h 需 ≥2 根 1h 逆势收盘 K)、breakout-hold(浮亏且距反向结构 <1% 拦截)、min-hold(10min)三个门都只作用于**浮亏平仓**;浮盈平仓不受限。叠加 C3 的获利阶梯(1R 减半+TP 50%+trail),系统"盈利实现加速、亏损实现延迟"——左尾的资金占用时间和情绪成本被拉长,幅度上 SL 兜底不变。

**方案 A(现状不动)**:设计初衷是防 AI 恐慌性卖飞;SL 已经限定亏损幅度,时间成本是可接受代价。
**方案 B(推荐):深亏逃生通道**——当浮亏 ≥ 1.2×开仓风险(initial_stop_loss 锚,已有)时,early-close 与 min-hold 直接放行(breakout-hold 保留,它拦的是"贴着反向结构卖在最低点"的正确拦截)。理由:亏损已超 1R 说明 setup 已死,继续持有的期望由 SL 兜底变成"白交资金占用+情绪成本";1.2R 而非 1R 是给正常波动留半个身位,避免把 1R 锁的正常回踩误判为深亏。改动小(两个门各加一个旁路条件),可配置(`early_close_escape_r`,默认 0=关,负=关,正=启用)。
**方案 C:对称摩擦**——浮盈平仓也加对称门(如浮盈 <1R 且 <1h 不许全平)。不推荐:与 1R 锁/TP 阶梯功能重叠,闸门叠闸门,且进一步压制右尾(C3 的反面)。

选 B 的话一句话回复即可,我按配置开关实现(默认关闭)。
