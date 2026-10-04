# 做多/做空标的筛选 · 代码核对包(2026-10-04)

> ⚠️ **本包是修复前快照**(2026-10-04 审查底稿)。同日修复批次已取代其中
> 八处行为,核对时以下列为准(详见 10f8e85d 之后的修复提交):
> 1. computeTF age 改「首次穿越」——held/Confirmed 不再恒真,confirmPenalty 复活,
>    retested 不再计入交叉棒;
> 2. DirDown room 修复——做空 RoomATR 不再恒 3.0;
> 3. scheduler regime 改用 shortscan 同款 4h-EMA 分类器(btcRegimePenalty),
>    调分后 Grade 重算、Percentile 按终序;
> 4. 候选池质量下限——piggy 过滤 noise+approach,short_scan 过滤 noise;
> 5. shortscan 分级统一走 finalizeShortGrade——BTC 折扣不再绕过未确认 strong 帽;
>    slow-top 先合并后吃折扣;
> 6. FakeBreakout 要求现价仍在位下;
> 7. Crowding 三源独立+权重重归一,fundPart 按结算间隔年化(中心 50/宽 40),
>    clamp100 挡 NaN;
> 8. kernel computeBreakoutState 改固定前高(排除最近 8 根的 30 根极值)——
>    fake_break/retest_hold/extended 可达,回踩锚成为真实回踩位;
>    OIConfirm 阈值 >0 → >0.3%;NEG_EDGE_SCORE 改带方向比较。
> 混合源 `piggyCoins, piggyErr :=` 遮蔽(engine.go)已修。

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
