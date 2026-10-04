# 做多/做空标的筛选 · 代码核对包(2026-10-04)

> ⚠️ **本包是修复前快照**(2026-10-04 审查底稿)。同日两个修复批次已取代其中
> 行为,核对时以下列为准(11ee538c + 6817bf8d):
> 批次1: ①computeTF age 改「首次穿越」——held/Confirmed 不再恒真,confirmPenalty 复活,
>    retested 不再计入交叉棒;②DirDown room 修复(不再恒 3.0);③scheduler regime 改用
>    shortscan 同款 4h-EMA 分类器,调分后 Grade 重算、Percentile 按终序;④候选池质量下限
>    (piggy 滤 noise+approach,short_scan 滤 noise);⑤分级统一 finalizeShortGrade,
>    BTC 折扣不丢未确认帽,slow-top 先合并后吃折扣;⑥FakeBreakout 要求现价仍在位下;
>    ⑦Crowding 三源独立+重归一,fundPart 年化,clamp100 挡 NaN;⑧kernel computeBreakoutState
>    固定前高——fake_break/retest_hold/extended 可达;OI 阈值 >0.3%;NEG_EDGE 带方向;
>    mixed `:=` 遮蔽修复。
> 批次2: ⑨BTC4hRegime 共享分类器(scheduler/shortscan/kernel 同一定义,60 根收敛门槛);
>    ⑩SHORT_TOP_CONFIRM_MISSING(默认开,short_scan 候选需顶部确认,无证据候选不拦);
>    ⑪BTC_4H_STRONGBULL(默认关,btc_filter_short 显式开);⑫费率拥挤二选一**有意不门化**
>    (提示词定位是支撑证据非必要条件);⑬piggy 缺数维度剔除+重归一,α 缺数取 1.0;
>    ⑭computeBreakoutState/suppressAnchors 改 ClosedKlines(now) 时间判定式。
> 实证:磨顶门槛 45 次 skipped(分 30-33) vs 0 次入选——磨顶宇宙从未进池,待拍板。

直接可读的源文件副本(与工作区同版本),按核对动线排列。核心函数在
signal_layer.go 内的行号(便于跳转):

## 做多标的筛选(谁入选 + 打分)
- market/breakout/scheduler.go    — piggy 突破扫描(5min,61 币,BTC regime 调分)
- market/breakout/breakout.go     — 六维打分(combine: 1h×0.65+15m×0.35+共振;
                                    computeTF: Level confluence/Volume/Flow/OI/Funding/Momentum)
- market/breakout/breakout.go     — gradeOf 阈值(strong 80/medium 60/weak 40, params 可调)

## 做空标的筛选
- market/breakout/shortscan.go    — 九维打分(权重 shortWeights(): 0.10×7+structure/crowding 0.15,
                                    归一化,调参器可覆盖)+ 顶部确认四信号 + strong 未确认硬帽 medium
- market/breakout/slowtop.go      — 磨顶宇宙(90d 高点 5%+4h 顶背离,$30M 预筛,30min)

## 方向门(硬性准入,全部程序预计算)
- kernel/signal_layer.go          — computeHardEntryGate(:1703) 全部禁开码;
                                    buildLongPullbackPlan(:1554) 回踩锚;
                                    methodStopPlan(:1351) 止损计划;
                                    scanRRForSymbol(:1905) TP 菜单;
                                    computeBreakoutState(:3014) 突破状态机
- kernel/engine.go                — 候选组装(GetCandidateCoins/filterExcludedCoins/
                                    getPiggyDashCoins 含 12min TTL)
- kernel/engine_analysis.go       — fetch 通道(near_high OI 豁免、快照遍历、数据质量)

## 阈值速查(与 DATA_COMPUTATION_2026-10-04.md §5/§6/§9 对照)
- piggy: strong 80 / medium 60(params.json 可调,实盘 80/60);合成 1h×0.65+15m×0.35+共振奖 5(双 60 门槛)
- short_scan: strong 70 / medium 55 / weak 40(硬编码);未确认 strong 硬帽 medium;
  未确认+量能<40+24h>20% ×0.8 排名折扣
- 多头门: EMA20 距离帽 12%、WIDE_STOP 10%(bstock 豁免)、BTC 真 4h 双码、贪婪降权 FNG≥70 → −10 分
- 空头门: 费率拥挤年化 >32.9%(8h 口径)二选一 / rollover.detected、MICRO_TREND_NOT_SHORT、
  CONSENSUS_OPPOSED(±50)、NEG_EDGE_*(PF<0.9)
