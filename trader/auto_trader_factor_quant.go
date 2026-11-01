package trader

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"nofx/logger"
	"nofx/store"
	"nofx/trader/paper"
)

// factor_quant (design 2026-11-01): the programmatic factor strategy. Own run
// loop, no LLM call, no AI prompt, no futures AI monitors. Python produces
// quant_targets.json after the nightly factor mining; this loop reads it every
// 5 minutes and, when the rebalance gate opens, trades a PAPER account towards
// the target weights.

const (
	factorQuantTickInterval = 5 * time.Minute
	factorQuantRuleFallback = "未知规则"
)

// factorQuantRuntime is the loop's in-memory state (only touched under the
// execution mutex or by the single loop goroutine).
type factorQuantRuntime struct {
	mu          sync.Mutex
	memAsOf     string // as_of of the last attempt in this process (covers a failing state store)
	lastLog     string // last tick log line (suppresses 5-minute repeats)
	skipKey     string // last skip/failure reason already written to a decision record
	skipKeyLoad bool
}

// IsFactorQuantStrategy reports whether this trader runs the factor_quant strategy.
func (at *AutoTrader) IsFactorQuantStrategy() bool {
	sc := at.config.StrategyConfig
	return sc != nil && sc.StrategyType == store.StrategyTypeFactorQuant && sc.FactorQuantConfig != nil
}

func (at *AutoTrader) isFactorQuantUS() bool {
	return at.IsFactorQuantStrategy() && at.config.StrategyConfig.FactorQuantConfig.IsUS()
}

func (at *AutoTrader) fqClock() time.Time {
	if at.fqNow != nil {
		return at.fqNow()
	}
	return time.Now()
}

// factorQuantGuard is the paper-only rule, enforced at construction: a
// factor_quant trader may only run on a paper account. Crypto trades the paper
// futures simulator; US books the bStock paper ledger and never builds a live
// spot executor. Any other exchange type is refused.
func factorQuantGuard(config *AutoTraderConfig) error {
	sc := config.StrategyConfig
	if sc == nil || sc.StrategyType != store.StrategyTypeFactorQuant {
		return nil
	}
	if sc.FactorQuantConfig == nil {
		return fmt.Errorf("因子量化策略缺少 factor_quant_config 配置")
	}
	if err := sc.FactorQuantConfig.Validate(); err != nil {
		return err
	}
	if config.Exchange != "paper" {
		return fmt.Errorf("因子量化策略只允许运行在模拟盘账户上（交易所类型必须是 paper，当前为 %q）：为保护实盘资金，已拒绝启动", config.Exchange)
	}
	return nil
}

// factorQuantInitLedger prepares the US bStock paper ledger at construction
// and adopts its starting cash as the P&L baseline (a restored ledger keeps
// its own baseline, like us_stock paper).
func factorQuantInitLedger(config *AutoTraderConfig, st *store.Store) error {
	sc := config.StrategyConfig
	if sc == nil || sc.StrategyType != store.StrategyTypeFactorQuant || !sc.FactorQuantConfig.IsUS() || st == nil {
		return nil
	}
	acct, err := st.StockPaper().EnsureAccount(config.ID, config.InitialBalance)
	if err != nil {
		return fmt.Errorf("初始化美股模拟账本失败：%w", err)
	}
	config.InitialBalance = acct.InitialCash
	return nil
}

// newFactorQuantBroker builds the paper venue. It refuses anything but the
// paper simulator (crypto) or the bStock paper ledger (us).
func (at *AutoTrader) newFactorQuantBroker() (fqBroker, error) {
	if !at.IsFactorQuantStrategy() {
		return nil, fmt.Errorf("不是因子量化策略")
	}
	fq := at.config.StrategyConfig.FactorQuantConfig
	if at.exchange != "paper" {
		return nil, fmt.Errorf("因子量化策略只允许模拟盘账户（当前交易所类型 %q）", at.exchange)
	}
	if fq.IsUS() {
		return &fqStockBroker{at: at}, nil
	}
	pt, ok := at.trader.(*paper.PaperFuturesTrader)
	if !ok {
		return nil, fmt.Errorf("因子量化加密策略需要模拟合约执行器，实际为 %T", at.trader)
	}
	return newFQFuturesBroker(pt, fq.EffectiveLeverage(), fq.EffectiveShortStopPct()), nil
}

// runFactorQuantLoop is the factor_quant run loop (dispatched from run()).
func (at *AutoTrader) runFactorQuantLoop() error {
	at.isRunningMutex.RLock()
	stop := at.stopMonitorCh
	at.isRunningMutex.RUnlock()

	broker, err := at.newFactorQuantBroker()
	if err != nil {
		return err
	}
	fq := at.config.StrategyConfig.FactorQuantConfig
	logger.Infof("📐 [%s] factor_quant 启动：market=%s horizon=%d 资金比例=%.2f 目标文件=%s（仅模拟盘，不调用大模型）",
		at.name, fq.Market, fq.EffectiveHorizon(), fq.EffectiveCapitalFraction(), factorQuantTargetsPath(fq))

	// Paper fills (including the short catastrophic stop) resolve on the
	// simulator's order-sync tick; run()'s defer stops it.
	if fq.IsCrypto() && at.store != nil {
		if syncer, ok := at.trader.(interface {
			StartOrderSync(string, string, string, *store.Store, time.Duration)
		}); ok {
			if labeler, ok := at.trader.(interface{ SetDisplayName(string) }); ok {
				labeler.SetDisplayName(at.name + " · factor_quant")
			}
			syncer.StartOrderSync(at.id, at.exchangeID, at.exchange, at.store, 30*time.Second)
		}
	}

	at.factorQuantTickSafe(broker)
	ticker := time.NewTicker(factorQuantTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			logger.Infof("[%s] ⏹ factor_quant loop stopped", at.name)
			return nil
		case <-ticker.C:
			at.factorQuantTickSafe(broker)
		}
	}
}

func (at *AutoTrader) factorQuantTickSafe(broker fqBroker) {
	defer func() {
		if p := recover(); p != nil {
			logger.Errorf("🚨 [%s] factor_quant tick panic: %v", at.name, p)
		}
	}()
	at.factorQuantTick(broker, at.fqClock())
}

// factorQuantTick runs one loop iteration: rebalance gate, then equity snapshot.
func (at *AutoTrader) factorQuantTick(broker fqBroker, now time.Time) {
	mu := at.executionMutex()
	mu.Lock()
	defer mu.Unlock()
	at.factorQuantRebalanceCheck(broker, now)
	at.factorQuantSaveEquity(broker, now)
}

// fqLogOnce logs a tick message only when it differs from the previous one.
func (at *AutoTrader) fqLogOnce(line string) {
	at.fq.mu.Lock()
	same := at.fq.lastLog == line
	at.fq.lastLog = line
	at.fq.mu.Unlock()
	if !same {
		logger.Infof("📐 [%s] %s", at.name, line)
	}
}

// fqLastAsOf returns the durable (and in-process) as_of of the last rebalance.
func (at *AutoTrader) fqLastAsOf() (string, error) {
	last := ""
	if at.store != nil {
		st, err := at.store.FactorQuant().Get(at.id)
		if err != nil {
			return "", err
		}
		if st != nil {
			last = st.LastAsOf
		}
	}
	at.fq.mu.Lock()
	if at.fq.memAsOf > last {
		last = at.fq.memAsOf
	}
	at.fq.mu.Unlock()
	return last, nil
}

// fqSkipSeen reports whether key is the last skip reason already recorded, and
// remembers it otherwise (also durably, so a restart does not repeat it).
func (at *AutoTrader) fqSkipSeen(key string) bool {
	at.fq.mu.Lock()
	if !at.fq.skipKeyLoad {
		at.fq.skipKeyLoad = true
		if at.store != nil {
			if st, err := at.store.FactorQuant().Get(at.id); err == nil && st != nil {
				at.fq.skipKey = st.LastSkipKey
			}
		}
	}
	seen := at.fq.skipKey == key
	at.fq.skipKey = key
	at.fq.mu.Unlock()
	if !seen && at.store != nil {
		if err := at.store.FactorQuant().SetSkipKey(at.id, key); err != nil {
			logger.Warnf("⚠️ [%s] factor_quant 无法持久化跳过标记：%v", at.name, err)
		}
	}
	return seen
}

func (at *AutoTrader) factorQuantRebalanceCheck(broker fqBroker, now time.Time) {
	fq := at.config.StrategyConfig.FactorQuantConfig
	path := factorQuantTargetsPath(fq)
	targets, err := loadQuantTargets(path, fq.Market)
	if err != nil {
		at.fqLogOnce(fmt.Sprintf("本轮跳过：%v", err))
		return
	}
	lastAsOf, err := at.fqLastAsOf()
	if err != nil {
		// Fail safe: without the durable marker a restart could double-rebalance.
		logger.Warnf("⚠️ [%s] factor_quant 读取调仓状态失败，本轮跳过：%v", at.name, err)
		return
	}
	hasPositions := false
	if lastAsOf == "" {
		acct, aerr := broker.Account()
		if aerr != nil {
			at.fqLogOnce(fmt.Sprintf("本轮跳过：%v", aerr))
			return
		}
		hasPositions = len(acct.Positions) > 0
	}
	dec := fqShouldRebalance(targets, fq, lastAsOf, hasPositions, now)
	if !dec.Rebalance {
		at.fqLogOnce(fmt.Sprintf("不调仓 [%s] as_of=%s：%s", dec.Code, targets.AsOf, dec.Reason))
		if dec.noisy() {
			key := fmt.Sprintf("%s|%s|%s", dec.Code, targets.AsOf, targets.Status)
			if !at.fqSkipSeen(key) {
				at.saveFactorQuantRecord(fqBuildSkipRecord(targets, dec, fq, now))
			}
		}
		return
	}

	logger.Infof("📐 [%s] factor_quant 开始调仓 as_of=%s rule=%s：%s", at.name, targets.AsOf, targets.Rule.Name, dec.Reason)
	rep := fqRunRebalance(broker, targets, fqPlanConfig{
		CapitalFraction: fq.EffectiveCapitalFraction(),
		Band:            fq.EffectiveBand(),
		MinOrder:        fq.EffectiveMinOrderUSDT(),
		LongOnly:        fq.IsUS(),
	})
	if rep.Err != nil {
		// Nothing was traded: keep the gate open, but write one record per as_of.
		at.fqLogOnce(fmt.Sprintf("调仓中止：%v", rep.Err))
		if !at.fqSkipSeen("error|" + targets.AsOf) {
			at.saveFactorQuantRecord(fqBuildRecord(rep, dec, fq, at.initialBalance, now))
		}
		return
	}
	at.fq.mu.Lock()
	at.fq.memAsOf = targets.AsOf
	at.fq.mu.Unlock()
	if at.store != nil {
		if err := at.store.FactorQuant().SetRebalanced(at.id, targets.AsOf, targets.RunID, targets.Rule.Name, now); err != nil {
			logger.Errorf("❌ [%s] factor_quant 无法持久化调仓 as_of=%s：%v（本进程内不会重复调仓，重启后可能重复）", at.name, targets.AsOf, err)
		}
	}
	rec := fqBuildRecord(rep, dec, fq, at.initialBalance, now)
	at.saveFactorQuantRecord(rec)
	logger.Infof("📐 [%s] factor_quant 调仓完成 as_of=%s：%d 笔下单，%d 笔失败，%d 个跳过", at.name, targets.AsOf, len(rep.Results), rep.failedCount(), len(rep.Plan.Skips))
	at.fqSkipSeen("done|" + targets.AsOf) // a finished rebalance supersedes older skip/failure keys
}

func (at *AutoTrader) saveFactorQuantRecord(rec *store.DecisionRecord) {
	if err := at.saveDecision(rec); err != nil {
		logger.Infof("⚠ [%s] Failed to save factor_quant decision record: %v", at.name, err)
	}
}

// factorQuantSaveEquity writes the periodic equity snapshot that feeds the
// promotion report and the equity charts.
func (at *AutoTrader) factorQuantSaveEquity(broker fqBroker, now time.Time) {
	if at.store == nil {
		return
	}
	acct, err := broker.Account()
	if err != nil {
		logger.Infof("⚠️ [%s] factor_quant 权益快照跳过：%v", at.name, err)
		return
	}
	margin := acct.Exposure
	if fq := at.config.StrategyConfig.FactorQuantConfig; fq.IsCrypto() {
		margin = acct.Exposure / float64(fq.EffectiveLeverage())
	}
	pct := 0.0
	if acct.Equity > 0 {
		pct = margin / acct.Equity * 100
	}
	if err := at.store.Equity().Save(&store.EquitySnapshot{TraderID: at.id, Timestamp: now.UTC(), TotalEquity: acct.Equity,
		Balance: acct.Equity - acct.Unrealized, UnrealizedPnL: acct.Unrealized, PositionCount: len(acct.Positions), MarginUsedPct: pct}); err != nil {
		logger.Infof("⚠️ Failed to save equity snapshot: %v", err)
	}
}

// ------------------------------------------------------------ decision records

func fqModeLabel(fq *store.FactorQuantConfig) string {
	if fq.IsUS() {
		return "美股 bStock 模拟账本"
	}
	return "加密 模拟合约账户"
}

func fqAccountSnapshot(a *fqAccount, initial float64) store.AccountSnapshot {
	if a == nil {
		return store.AccountSnapshot{InitialBalance: initial}
	}
	pct := 0.0
	if a.Equity > 0 {
		pct = a.Exposure / a.Equity * 100
	}
	return store.AccountSnapshot{TotalBalance: a.Equity, AvailableBalance: a.Balance, TotalUnrealizedProfit: a.Unrealized,
		PositionCount: len(a.Positions), MarginUsedPct: pct, InitialBalance: initial}
}

func fqPositionSnapshots(a *fqAccount) []store.PositionSnapshot {
	if a == nil {
		return nil
	}
	out := make([]store.PositionSnapshot, 0, len(a.Positions))
	for _, p := range a.Positions {
		amt := p.Qty
		if p.Side == fqSideShort {
			amt = -p.Qty
		}
		pnl := (p.Mark - p.Entry) * p.Qty
		if p.Side == fqSideShort {
			pnl = -pnl
		}
		out = append(out, store.PositionSnapshot{Symbol: p.Symbol, Side: p.Side, PositionAmt: amt, EntryPrice: p.Entry,
			MarkPrice: p.Mark, UnrealizedProfit: pnl, Leverage: 1})
	}
	return out
}

func fqRuleName(t *quantTargets) string {
	if t != nil && t.Rule.Name != "" {
		return t.Rule.Name
	}
	return factorQuantRuleFallback
}

// fqBuildSkipRecord is the record of a skipped day (status != ok, stale, ...).
func fqBuildSkipRecord(t *quantTargets, dec fqDecision, fq *store.FactorQuantConfig, now time.Time) *store.DecisionRecord {
	summary := fmt.Sprintf("因子量化 · 未调仓（%s）\nas_of=%s 规则=%s horizon=%d 状态=%s\n原因：%s\n账户保持现状，不开新仓。",
		fqModeLabel(fq), t.AsOf, fqRuleName(t), t.Horizon, t.Status, dec.Reason)
	return &store.DecisionRecord{
		Timestamp: now.UTC(),
		CoTTrace:  summary,
		Success:   true,
		ExecutionLog: []string{
			fmt.Sprintf("[factor_quant] 跳过 code=%s as_of=%s status=%s：%s", dec.Code, t.AsOf, t.Status, dec.Reason),
		},
	}
}

// fqBuildRecord renders one rebalance attempt as a decision record.
func fqBuildRecord(rep *fqRebalanceReport, dec fqDecision, fq *store.FactorQuantConfig, initial float64, now time.Time) *store.DecisionRecord {
	t := rep.Targets
	rec := &store.DecisionRecord{Timestamp: now.UTC()}
	var sb strings.Builder
	fmt.Fprintf(&sb, "因子量化调仓（%s，仅模拟盘，未调用大模型）\n", fqModeLabel(fq))
	fmt.Fprintf(&sb, "as_of=%s 规则=%s horizon=%d 天 run_id=%s\n", t.AsOf, fqRuleName(t), dec.Horizon, t.RunID)
	fmt.Fprintf(&sb, "触发：%s\n", dec.Reason)

	if rep.Err != nil {
		fmt.Fprintf(&sb, "调仓中止：%v\n本轮未下任何订单，将在下个检查周期重试。", rep.Err)
		rec.CoTTrace = sb.String()
		rec.Success = false
		rec.ErrorMessage = rep.Err.Error()
		rec.ExecutionLog = []string{"[factor_quant] 调仓中止：" + rep.Err.Error()}
		return rec
	}

	fmt.Fprintf(&sb, "权益 %.2f × 资金比例 %.2f = 预算 %.2f USDT\n", rep.Before.Equity, fq.EffectiveCapitalFraction(), rep.Plan.Budget)
	counts := map[string]int{}
	for _, o := range rep.Plan.Orders {
		counts[o.Kind]++
	}
	failed := rep.failedCount()
	fmt.Fprintf(&sb, "计划 %d 笔（平仓 %d / 减仓 %d / 加仓 %d / 建仓 %d），跳过 %d 个，失败 %d 笔\n",
		len(rep.Plan.Orders), counts[fqKindClose], counts[fqKindReduce], counts[fqKindIncrease], counts[fqKindOpen], len(rep.Plan.Skips), failed)

	for i, r := range rep.Results {
		o := r.Order
		status := "成功"
		if !r.OK {
			status = "失败：" + r.Err
		} else if r.StopErr != "" {
			status = "成交但止损失败：" + r.StopErr
		}
		line := fmt.Sprintf("%d. %s %s %s 数量=%.8g 价格≈%.6g 名义≈%.2f [%s] → %s",
			i+1, o.Action(), o.Symbol, strings.ToUpper(o.Side), o.Qty, o.Price, o.Notional, o.Reason, status)
		if r.OK {
			if r.FillPrice > 0 {
				line += fmt.Sprintf("（成交价 %.6g", r.FillPrice)
				if r.OrderID != "" {
					line += " 单号 " + r.OrderID
				}
				line += "）"
			}
			if r.StopPrice > 0 {
				line += fmt.Sprintf("，灾难止损 %.6g", r.StopPrice)
			}
		}
		sb.WriteString(line + "\n")
		rec.ExecutionLog = append(rec.ExecutionLog, "[factor_quant] "+line)
		rec.Decisions = append(rec.Decisions, store.DecisionAction{
			Action: o.Action(), Symbol: o.Symbol, Quantity: o.Qty, Leverage: fqLeverageOf(fq), Price: o.Price,
			Reasoning: o.Reason, EntryOrderID: r.OrderID, Timestamp: now.UTC(), Success: r.OK && r.StopErr == "", Error: strings.TrimSpace(r.Err + " " + r.StopErr),
			StopLoss: r.StopPrice,
		})
		rec.CandidateCoins = append(rec.CandidateCoins, o.Symbol)
	}
	if len(rep.Plan.Skips) > 0 {
		sb.WriteString("跳过：\n")
		for _, s := range rep.Plan.Skips {
			line := fmt.Sprintf("- %s：%s", s.Symbol, s.Reason)
			sb.WriteString(line + "\n")
			rec.ExecutionLog = append(rec.ExecutionLog, "[factor_quant] 跳过 "+s.Symbol+"："+s.Reason)
		}
	}
	if len(rep.PriceErrs) > 0 {
		names := make([]string, 0, len(rep.PriceErrs))
		for s := range rep.PriceErrs {
			names = append(names, s)
		}
		sort.Strings(names)
		sb.WriteString("无价格：\n")
		for _, s := range names {
			sb.WriteString(fmt.Sprintf("- %s：%s\n", s, rep.PriceErrs[s]))
			rec.ExecutionLog = append(rec.ExecutionLog, "[factor_quant] 无价格 "+s+"："+rep.PriceErrs[s])
		}
	}
	if len(t.Excluded) > 0 {
		fmt.Fprintf(&sb, "目标文件已排除 %d 个标的（例如空头排除规则）。\n", len(t.Excluded))
	}
	rec.CoTTrace = sb.String()
	rec.Success = failed == 0
	if failed > 0 {
		rec.ErrorMessage = fmt.Sprintf("%d 笔订单失败", failed)
	}
	rec.AccountState = fqAccountSnapshot(rep.After, initial)
	if rep.After == nil {
		rec.AccountState = fqAccountSnapshot(rep.Before, initial)
	}
	rec.Positions = fqPositionSnapshots(rep.After)
	return rec
}

func fqLeverageOf(fq *store.FactorQuantConfig) int {
	if fq.IsCrypto() {
		return fq.EffectiveLeverage()
	}
	return 1
}

// ------------------------------------------------------------ API readers (US ledger)

// factorQuantUSAccountInfo is GetAccountInfo for the US paper ledger (the
// shared futures reader would look at the unused paper futures account).
func (at *AutoTrader) factorQuantUSAccountInfo() (map[string]interface{}, error) {
	if at.store == nil {
		return nil, fmt.Errorf("no store")
	}
	st, err := at.stockGatherPaper(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get paper ledger: %w", err)
	}
	unrealized := 0.0
	for _, h := range st.Holdings {
		unrealized += (h.Mark - h.Avg) * h.Owned
	}
	totalPnL := st.Equity - at.initialBalance
	pnlPct, usedPct := 0.0, 0.0
	if at.initialBalance > 0 {
		pnlPct = totalPnL / at.initialBalance * 100
	}
	if st.Equity > 0 {
		usedPct = st.Exposure / st.Equity * 100
	}
	return map[string]interface{}{
		"total_equity":      st.Equity,
		"wallet_balance":    st.Equity - unrealized,
		"unrealized_profit": unrealized,
		"available_balance": st.Available,
		"total_pnl":         totalPnL,
		"total_pnl_pct":     pnlPct,
		"initial_balance":   at.initialBalance,
		"daily_pnl":         at.dailyPnL,
		"position_count":    len(st.Holdings),
		"margin_used":       st.Exposure,
		"margin_used_pct":   usedPct,
		"paper":             true,
	}, nil
}

func (at *AutoTrader) factorQuantUSPositions() ([]map[string]interface{}, error) {
	if at.store == nil {
		return nil, fmt.Errorf("no store")
	}
	st, err := at.stockGatherPaper(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get paper positions: %w", err)
	}
	out := []map[string]interface{}{}
	for _, h := range st.sortedHoldings() {
		pnl := (h.Mark - h.Avg) * h.Owned
		pct := 0.0
		if h.Avg > 0 {
			pct = (h.Mark/h.Avg - 1) * 100
		}
		out = append(out, map[string]interface{}{
			"symbol": h.Symbol, "side": "long", "entry_price": h.Avg, "mark_price": h.Mark, "quantity": h.Owned,
			"leverage": 1, "unrealized_pnl": pnl, "unrealized_pnl_pct": pct, "liquidation_price": 0.0,
			"margin_used": h.Owned * h.Mark, "paper": true,
		})
	}
	return out, nil
}

// factorQuantRiskStatus: a minimal read model — the AI path's breakers
// (daily loss, loss streaks, pending entries) do not exist for this strategy.
func (at *AutoTrader) factorQuantRiskStatus() RiskStatus {
	out := RiskStatus{GeneratedAt: time.Now().UTC(), InitialBalance: at.initialBalance,
		LossStreakBans: []LossStreakBan{}, Pending: []PendingStatus{}, Positions: []PositionRStatus{}}
	if at.isFactorQuantUS() {
		if info, err := at.factorQuantUSAccountInfo(); err == nil {
			out.Equity, _ = info["total_equity"].(float64)
		}
	} else if at.trader != nil {
		if bal, err := at.trader.GetBalance(); err == nil {
			out.Equity = gridAccountEquity(bal)
		}
	}
	if out.InitialBalance > 0 && out.Equity > 0 {
		out.AccountDrawdownPct = (out.InitialBalance - out.Equity) / out.InitialBalance * 100
	}
	return out
}
