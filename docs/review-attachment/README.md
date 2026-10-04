# 做多/做空标的筛选 · 代码核对包(2026-10-04)

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
