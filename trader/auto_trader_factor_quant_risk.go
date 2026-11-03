package trader

import (
	"fmt"
	"math"
	"time"

	"nofx/logger"
	"nofx/store"
	notify "nofx/telegram/notify"
)

const (
	fqHaltDrawdown = "drawdown"
	fqHaltDaily    = "daily"
)

// fqCopyRiskState keeps the risk snapshot independent of rebalance markers.
// Readers use fq.mu, never the execution mutex, to copy these fields.
func fqCopyRiskState(dst, src *store.FactorQuantState) {
	dst.PeakEquity = src.PeakEquity
	dst.DayStartDate, dst.DayStartEquity = src.DayStartDate, src.DayStartEquity
	dst.HaltKind, dst.HaltDay = src.HaltKind, src.HaltDay
	dst.HaltReason, dst.HaltedAt = src.HaltReason, src.HaltedAt
	dst.WarnActive, dst.KillResetApplied = src.WarnActive, src.KillResetApplied
}

// fqSaveRisk remembers transitions before attempting SQL. A failed write must
// never reopen trading or re-anchor a baseline in this process.
func (at *AutoTrader) fqSaveRisk(state *store.FactorQuantState, save func(*store.FactorQuantStore) error) {
	at.fq.mu.Lock()
	at.fq.riskSet = true
	fqCopyRiskState(&at.fq.riskState, state)
	at.fq.mu.Unlock()
	if at.store != nil {
		if err := save(at.store.FactorQuant()); err != nil {
			logger.Errorf("❌ [%s] factor_quant 无法持久化组合风控状态：%v（本进程继续保持风控状态）", at.name, err)
		}
	}
}

func fqEquityLoss(equity, baseline float64) float64 {
	if baseline <= 0 {
		return 0
	}
	return (baseline - equity) / baseline
}

func fqClearHalt(state *store.FactorQuantState) {
	state.HaltKind, state.HaltDay, state.HaltReason, state.HaltedAt = "", "", "", 0
}

// factorQuantRiskCheck runs under executionMutex before any target/retry gate.
// ready=false skips the whole tick; halted=true allows only equity snapshots.
func (at *AutoTrader) factorQuantRiskCheck(broker fqBroker, now time.Time) (ready, halted bool) {
	acct, err := broker.Account()
	if err != nil {
		at.fqNote("risk_skip", fmt.Sprintf("组合风控读取账户失败，本轮跳过：%v", err))
		at.fqLogOnce(fmt.Sprintf("组合风控读取账户失败，本轮跳过：%v", err))
		return false, false
	}
	if acct == nil || math.IsNaN(acct.Equity) || math.IsInf(acct.Equity, 0) {
		at.fqNote("risk_skip", "组合风控账户权益无效，本轮跳过")
		at.fqLogOnce("组合风控账户权益无效，本轮跳过")
		return false, false
	}
	at.fq.mu.Lock()
	at.fq.currentTick.Equity = acct.Equity
	at.fq.mu.Unlock()
	state, err := at.fqState()
	if err != nil {
		at.fqNote("risk_skip", fmt.Sprintf("组合风控读取状态失败，本轮跳过：%v", err))
		at.fqLogOnce(fmt.Sprintf("组合风控读取状态失败，本轮跳过：%v", err))
		return false, false
	}
	fq := at.config.StrategyConfig.FactorQuantConfig
	today := now.UTC().Format(factorDateFmt)
	if id := fq.KillResetID; id != "" && id != state.KillResetApplied {
		clearDrawdown := state.HaltKind == fqHaltDrawdown
		if clearDrawdown {
			fqClearHalt(state)
		}
		state.PeakEquity, state.WarnActive, state.KillResetApplied = acct.Equity, false, id
		at.fqSaveRisk(state, func(s *store.FactorQuantStore) error {
			return s.ApplyKillReset(at.id, id, acct.Equity, clearDrawdown)
		})
		at.fqRiskRecord(acct, "熔断已手动复位", now)
	}

	// A non-positive equity read or a position without a usable mark makes
	// equity unreliable: never let it ratchet the peak, re-anchor the day or
	// fire a liquidation. The rest of the tick (stale flatten) still runs.
	unreliable := ""
	if acct.Equity <= 0 {
		unreliable = fmt.Sprintf("权益 %.2f 不大于 0", acct.Equity)
	}
	for _, p := range acct.Positions {
		if unreliable == "" && !(p.Mark > 0) {
			unreliable = p.Symbol + " 无有效标记价格"
		}
	}
	if unreliable != "" {
		at.fqNote("risk_skip", "组合风控："+unreliable+"，本轮不评估熔断")
		at.fqLogOnce("组合风控：" + unreliable + "，本轮不评估熔断")
		if state.HaltKind != "" {
			at.factorQuantHaltFlatten(broker, acct, state, now)
			return true, true
		}
		return true, false
	}

	peak := state.PeakEquity
	if peak == 0 {
		peak = math.Max(at.initialBalance, acct.Equity)
		if at.store != nil {
			if historical, qerr := at.store.Equity().MaxEquity(at.id); qerr == nil {
				peak = math.Max(peak, historical)
			} else {
				logger.Warnf("⚠️ [%s] factor_quant 读取历史权益峰值失败，使用初始及当前权益：%v", at.name, qerr)
			}
		}
	}
	peak = math.Max(peak, acct.Equity)
	if peak != state.PeakEquity {
		state.PeakEquity = peak
		at.fqSaveRisk(state, func(s *store.FactorQuantStore) error { return s.SetPeakEquity(at.id, peak) })
	}
	if state.DayStartDate != today {
		state.DayStartDate, state.DayStartEquity = today, acct.Equity
		at.fqSaveRisk(state, func(s *store.FactorQuantStore) error { return s.SetDayStart(at.id, today, acct.Equity) })
	}
	if state.HaltKind == fqHaltDaily && state.HaltDay < today {
		fqClearHalt(state)
		at.fqSaveRisk(state, func(s *store.FactorQuantStore) error { return s.SetHalt(at.id, "", "", "", 0) })
	}

	dd, dayLoss := fqEquityLoss(acct.Equity, peak), fqEquityLoss(acct.Equity, state.DayStartEquity)
	newHalt := false
	if cap := fq.EffectiveMaxDrawdownPct(); cap > 0 && dd >= cap && state.HaltKind != fqHaltDrawdown {
		state.HaltKind, state.HaltDay = fqHaltDrawdown, ""
		state.HaltReason = fmt.Sprintf("回撤熔断清仓：权益 %.2f，峰值 %.2f，回撤 %.2f%% ≥ 阈值 %.2f%%", acct.Equity, peak, dd*100, cap*100)
		newHalt = true
	} else if cap := fq.EffectiveDailyLossPct(); cap > 0 && dayLoss >= cap && state.HaltKind == "" {
		state.HaltKind, state.HaltDay = fqHaltDaily, today
		state.HaltReason = fmt.Sprintf("单日亏损熔断清仓：权益 %.2f，UTC 日初 %.2f，亏损 %.2f%% ≥ 阈值 %.2f%%", acct.Equity, state.DayStartEquity, dayLoss*100, cap*100)
		newHalt = true
	}
	if newHalt {
		state.HaltedAt = now.UTC().UnixMilli()
		at.fqSaveRisk(state, func(s *store.FactorQuantStore) error {
			return s.SetHalt(at.id, state.HaltKind, state.HaltDay, state.HaltReason, state.HaltedAt)
		})
		at.fqSetRetry("", 0)
		logger.Errorf("🚨 [%s] factor_quant %s", at.name, state.HaltReason)
		notify.Notify("RISK", at.name, "<b>🚨 因子量化组合熔断</b>\n"+notify.Escape(state.HaltReason))
	}
	if warn := fq.EffectiveDrawdownWarnPct(); warn > 0 {
		if dd >= warn && !state.WarnActive {
			state.WarnActive = true
			at.fqSaveRisk(state, func(s *store.FactorQuantStore) error { return s.SetWarnActive(at.id, true) })
			reason := fmt.Sprintf("组合回撤预警：权益 %.2f，峰值 %.2f，回撤 %.2f%% ≥ 阈值 %.2f%%", acct.Equity, peak, dd*100, warn*100)
			at.fqRiskRecord(acct, reason, now)
			logger.Warnf("⚠️ [%s] factor_quant %s", at.name, reason)
			notify.Notify("RISK", at.name, "<b>⚠️ 因子量化回撤预警</b>\n"+notify.Escape(reason))
		} else if dd < warn/2 && state.WarnActive {
			state.WarnActive = false
			at.fqSaveRisk(state, func(s *store.FactorQuantStore) error { return s.SetWarnActive(at.id, false) })
		}
	}
	if state.HaltKind != "" {
		// Also clear a retry restored from a partial durable write or a restart.
		if state.RetryAsOf != "" && !newHalt {
			at.fqSetRetry("", 0)
		}
		at.factorQuantHaltFlatten(broker, acct, state, now)
		return true, true
	}
	return true, false
}

func (at *AutoTrader) fqRiskRecord(acct *fqAccount, reason string, now time.Time) {
	at.saveFactorQuantRecord(&store.DecisionRecord{Timestamp: now.UTC(), Success: true, CoTTrace: "因子量化 · " + reason,
		ExecutionLog: []string{"[factor_quant] " + reason}, AccountState: fqAccountSnapshot(acct, at.initialBalance), Positions: fqPositionSnapshots(acct)})
}

// factorQuantHaltFlatten reuses stale-flatten execution, including full-close
// stop cancellation. No last_as_of means these holdings are not strategy-owned.
func (at *AutoTrader) factorQuantHaltFlatten(broker fqBroker, acct *fqAccount, state *store.FactorQuantState, now time.Time) {
	at.fqNote("halted", fmt.Sprintf("风控停机 [%s]：%s", state.HaltKind, state.HaltReason))
	fq := at.config.StrategyConfig.FactorQuantConfig
	synthetic := &quantTargets{Version: quantTargetsVersion, Market: fq.Market, Status: quantStatusOK,
		AsOf: state.LastAsOf, Horizon: fq.EffectiveHorizon(), Rule: quantRule{Name: state.LastRule}, Weights: map[string]float64{}}
	rep := &fqRebalanceReport{Targets: synthetic, Before: acct, After: acct}
	if state.LastAsOf != "" && len(acct.Positions) > 0 {
		planCfg := fqResolvedPlanConfig(fq)
		planCfg.Flatten = true
		rep = fqRunRebalance(broker, synthetic, planCfg)
	}
	complete := rep.Err == nil && rep.failedCount() == 0 && rep.After != nil && (state.LastAsOf == "" || len(rep.After.Positions) == 0)
	key := fmt.Sprintf("halt|%s|%d", state.HaltKind, state.HaltedAt)
	reason := state.HaltReason
	if complete {
		key += "|complete"
		reason += "；自有持仓清仓完成，保持熔断"
	} else {
		reason += "；清仓未完成，下个周期继续清仓"
		logger.Errorf("🚨 [%s] factor_quant %s：%d 笔失败，中止=%v", at.name, reason, rep.failedCount(), rep.Err)
	}
	if !at.fqSkipSeen(key) {
		rec := fqBuildRecord(rep, fqDecision{Code: "halt", Horizon: synthetic.Horizon, Reason: reason}, fq, at.initialBalance, now)
		if !complete && rec.Success {
			rec.Success, rec.ErrorMessage = false, "清仓未完成或无法确认清仓结果"
		}
		at.saveFactorQuantRecord(rec)
	}
}
