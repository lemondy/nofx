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
	fqMaxRetryAttempts      = 6
	fqRecentTickLimit       = 12
)

// factorQuantRuntime is the loop's in-memory state, guarded by mu.
type factorQuantRuntime struct {
	// Shared by loop and API brokers; do not hold mu during funding I/O.
	fundingMu      sync.Mutex
	fundingAt      map[int64]time.Time
	mu             sync.Mutex
	memAsOf        string // as_of of the last attempt in this process (covers a failing state store)
	memRule        string
	retrySet       bool // in-process retry state supersedes a failed store write
	retryAsOf      string
	retryAttempts  int
	lastLog        string // last tick log line (suppresses 5-minute repeats)
	skipKey        string // last skip/failure reason already written to a decision record
	skipKeyLoad    bool
	riskSet        bool // risk snapshot supersedes failed durable writes
	riskState      store.FactorQuantState
	currentTick    FactorQuantTick
	recentTicks    []FactorQuantTick // newest first, capped at fqRecentTickLimit
	targetsAsOf    string
	targetsStatus  string
	targetsRule    string
	targetsHorizon int
	regime         *fqRegimeInfo
}

// IsFactorQuantStrategy reports whether this trader runs the factor_quant strategy.
func (at *AutoTrader) IsFactorQuantStrategy() bool {
	sc := at.config.StrategyConfig
	return sc != nil && sc.StrategyType == store.StrategyTypeFactorQuant && sc.FactorQuantConfig != nil
}

func (at *AutoTrader) isFactorQuantUS() bool {
	return at.IsFactorQuantStrategy() && at.config.StrategyConfig.FactorQuantConfig.IsUS()
}

// IsFactorQuantUSStrategy identifies the US paper-ledger reader for API callers.
func (at *AutoTrader) IsFactorQuantUSStrategy() bool { return at.isFactorQuantUS() }

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
		return &fqStockBroker{at: at, feeBps: fq.EffectiveUSFeeBps(), shortFeeBps: fq.EffectiveUSShortFeeBps(), slipBps: fq.EffectiveUSSlippageBps()}, nil
	}
	pt, ok := at.trader.(*paper.PaperFuturesTrader)
	if !ok {
		return nil, fmt.Errorf("因子量化加密策略需要模拟合约执行器，实际为 %T", at.trader)
	}
	return newFQFuturesBroker(pt, fq.EffectiveLeverage(), fq.EffectiveLongStopPct(), fq.EffectiveShortStopPct()), nil
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

	// Migrate existing crypto positions before either order sync or the first
	// rebalance check can act. Protection failures only warn and leave the loop running.
	at.factorQuantInitStops(broker)

	// Paper fills (including stops on both sides) resolve on the
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

// factorQuantInitStops also runs when the rebalance gate is closed or targets
// cannot be loaded, so a restart/config reload applies the configured stops.
func (at *AutoTrader) factorQuantInitStops(broker fqBroker) {
	if !at.config.StrategyConfig.FactorQuantConfig.IsCrypto() {
		return
	}
	mu := at.executionMutex()
	mu.Lock()
	defer mu.Unlock()
	placed, exited, failed := 0, 0, 0
	for _, res := range at.factorQuantEnsureStops(broker) {
		switch {
		case res.Err != "":
			failed++
			logger.Warnf("⚠️ [%s] factor_quant 启动止损（%s）%s：%s", at.name, fqStopSideLabel(res.Side), res.Symbol, res.Err)
		case res.Exited:
			exited++
			logger.Infof("📐 [%s] factor_quant 止损（%s）%s：%s", at.name, fqStopSideLabel(res.Side), res.Symbol, res.Note)
		case res.Price > 0:
			placed++
		}
	}
	logger.Infof("📐 [%s] factor_quant 启动止损检查：%d 个已放置，%d 个已平仓，%d 个失败", at.name, placed, exited, failed)
}

func (at *AutoTrader) factorQuantEnsureStops(broker fqBroker) []fqStopResult {
	if at.config.StrategyConfig.FactorQuantConfig.IsCrypto() {
		if protector, ok := broker.(interface{ ensureStops() []fqStopResult }); ok {
			return protector.ensureStops()
		}
	}
	return nil
}

func (at *AutoTrader) factorQuantProtectReport(broker fqBroker, rep *fqRebalanceReport) {
	rep.Stops = append(rep.Stops, at.factorQuantEnsureStops(broker)...)
	// A breached stop can close a position after the order execution snapshot.
	if len(rep.Stops) > 0 && rep.Err == nil {
		if after, err := broker.Account(); err == nil {
			rep.After = after
		}
	}
}

func (at *AutoTrader) factorQuantTickSafe(broker fqBroker) {
	defer func() {
		if p := recover(); p != nil {
			at.fqNote("panic", fmt.Sprint(p))
			at.fqFinishTick()
			logger.Errorf("🚨 [%s] factor_quant tick panic: %v", at.name, p)
		}
	}()
	at.factorQuantTick(broker, at.fqClock())
}

// factorQuantTick evaluates portfolio risk before the gate, then saves equity.
func (at *AutoTrader) factorQuantTick(broker fqBroker, now time.Time) {
	mu := at.executionMutex()
	mu.Lock()
	defer mu.Unlock()
	at.fq.mu.Lock()
	at.fq.currentTick = FactorQuantTick{At: now.UTC()}
	at.fq.mu.Unlock()
	defer func() {
		// Let the safe wrapper record panics once, with the recovered value.
		if p := recover(); p != nil {
			panic(p)
		}
		at.fqFinishTick()
	}()
	ready, halted := at.factorQuantRiskCheck(broker, now)
	if !ready {
		return
	}
	if !halted {
		at.factorQuantRebalanceCheck(broker, now)
	}
	at.factorQuantSaveEquity(broker, now)
}

// fqNote observes an outcome without changing logs or decision deduplication.
func (at *AutoTrader) fqNote(code, msg string) {
	at.fq.mu.Lock()
	at.fq.currentTick.Code, at.fq.currentTick.Message = code, msg
	at.fq.mu.Unlock()
}

func (at *AutoTrader) fqFinishTick() {
	at.fq.mu.Lock()
	defer at.fq.mu.Unlock()
	tick := at.fq.currentTick
	if tick.Code == "" {
		tick.Code, tick.Message = "ok", "检查完成"
	}
	if len(at.fq.recentTicks) < fqRecentTickLimit {
		at.fq.recentTicks = append(at.fq.recentTicks, FactorQuantTick{})
	}
	copy(at.fq.recentTicks[1:], at.fq.recentTicks[:len(at.fq.recentTicks)-1])
	at.fq.recentTicks[0] = tick
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

// fqState merges durable state with attempts made in this process, so a failed
// store write does not cause the next tick to repeat a completed operation.
func (at *AutoTrader) fqState() (*store.FactorQuantState, error) {
	state := &store.FactorQuantState{}
	if at.store != nil {
		st, err := at.store.FactorQuant().Get(at.id)
		if err != nil {
			return nil, err
		}
		if st != nil {
			state = st
		}
	}
	at.fq.mu.Lock()
	defer at.fq.mu.Unlock()
	if at.fq.memAsOf > state.LastAsOf {
		state.LastAsOf = at.fq.memAsOf
		state.LastRule = at.fq.memRule
	}
	if at.fq.retrySet {
		state.RetryAsOf = at.fq.retryAsOf
		state.RetryAttempts = at.fq.retryAttempts
	}
	if at.fq.riskSet {
		fqCopyRiskState(state, &at.fq.riskState)
	}
	return state, nil
}

func (at *AutoTrader) fqSetRetry(asOf string, attempts int) {
	if asOf == "" {
		attempts = 0
	}
	at.fq.mu.Lock()
	at.fq.retrySet, at.fq.retryAsOf, at.fq.retryAttempts = true, asOf, attempts
	at.fq.mu.Unlock()
	if at.store != nil {
		if err := at.store.FactorQuant().SetRetry(at.id, asOf, attempts); err != nil {
			logger.Warnf("⚠️ [%s] factor_quant 无法持久化重试状态：%v", at.name, err)
		}
	}
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
	targets, loadErr := loadQuantTargets(path, fq.Market)
	effectiveTargets, regimeInfo := fqResolveRegimeTargets(targets, fq, path)
	targets = effectiveTargets
	at.fq.mu.Lock()
	if targets != nil {
		at.fq.targetsAsOf, at.fq.targetsStatus = targets.AsOf, targets.Status
		at.fq.targetsRule, at.fq.targetsHorizon = targets.Rule.Name, targets.Horizon
	}
	regimeCopy := regimeInfo
	at.fq.regime = &regimeCopy
	at.fq.mu.Unlock()
	state, err := at.fqState()
	if err != nil {
		// Fail safe: without the durable marker a restart could double-rebalance.
		at.fqNote("state_error", fmt.Sprintf("读取调仓状态失败，本轮跳过：%v", err))
		logger.Warnf("⚠️ [%s] factor_quant 读取调仓状态失败，本轮跳过：%v", at.name, err)
		return
	}
	if targets != nil && state.RetryAsOf != "" && targets.AsOf != state.RetryAsOf {
		logger.Infof("📐 [%s] factor_quant 目标 as_of 已从 %s 变为 %s，清除旧目标重试", at.name, state.RetryAsOf, targets.AsOf)
		at.fqSetRetry("", 0)
		state.RetryAsOf, state.RetryAttempts = "", 0
	}
	if loadErr != nil {
		at.fqNote("targets_error", fmt.Sprintf("本轮跳过：%v", loadErr))
		at.fqLogOnce(fmt.Sprintf("本轮跳过：%v", loadErr))
		at.factorQuantFlattenCheck(broker, targets, state, now)
		return
	}
	lastAsOf := state.LastAsOf
	hasPositions := false
	if lastAsOf == "" {
		acct, aerr := broker.Account()
		if aerr != nil {
			at.fqNote("account_error", fmt.Sprintf("本轮跳过：%v", aerr))
			at.fqLogOnce(fmt.Sprintf("本轮跳过：%v", aerr))
			return
		}
		hasPositions = len(acct.Positions) > 0
	}
	dec := fqShouldRebalance(targets, fq, lastAsOf, hasPositions, now)
	if !dec.Rebalance {
		if dec.Code == fqCodeTooSoon && state.RetryAsOf != "" && state.RetryAsOf == lastAsOf && targets.AsOf == state.RetryAsOf {
			at.factorQuantRetry(broker, targets, state, dec, now, regimeInfo)
			return
		}
		if dec.Code != fqCodeShortDisabled && at.factorQuantFlattenCheck(broker, targets, state, now) {
			return
		}
		msg := fmt.Sprintf("不调仓 [%s] as_of=%s：%s", dec.Code, targets.AsOf, dec.Reason)
		at.fqNote(dec.Code, msg)
		at.fqLogOnce(msg)
		if dec.noisy() {
			key := fmt.Sprintf("%s|%s|%s", dec.Code, targets.AsOf, targets.Status)
			if !at.fqSkipSeen(key) {
				at.saveFactorQuantRecord(fqBuildSkipRecord(targets, dec, fq, now))
			}
		}
		return
	}

	logger.Infof("📐 [%s] factor_quant 开始调仓 as_of=%s rule=%s %s：%s", at.name, targets.AsOf, targets.Rule.Name, regimeInfo.summaryText(), dec.Reason)
	rep := fqRunRebalance(broker, targets, fqResolvedPlanConfig(fq))
	rep.Regime = regimeInfo
	if rep.Err != nil {
		if at.factorQuantFlattenCheck(broker, targets, state, now) {
			return
		}
		// Nothing was traded: keep the gate open, but write one record per as_of.
		at.fqNote("rebalance_error", fmt.Sprintf("调仓中止：%v", rep.Err))
		at.fqLogOnce(fmt.Sprintf("调仓中止：%v", rep.Err))
		if !at.fqSkipSeen("error|" + targets.AsOf) {
			at.saveFactorQuantRecord(fqBuildRecord(rep, dec, fq, at.initialBalance, now))
		}
		return
	}
	at.factorQuantProtectReport(broker, rep)
	at.fq.mu.Lock()
	at.fq.memAsOf = targets.AsOf
	at.fq.memRule = targets.Rule.Name
	at.fq.mu.Unlock()
	if at.store != nil {
		if err := at.store.FactorQuant().SetRebalanced(at.id, targets.AsOf, targets.RunID, targets.Rule.Name, now); err != nil {
			logger.Errorf("❌ [%s] factor_quant 无法持久化调仓 as_of=%s：%v（本进程内不会重复调仓，重启后可能重复）", at.name, targets.AsOf, err)
		}
	}
	if rep.failedCount() > 0 {
		at.fqSetRetry(targets.AsOf, 0)
	} else {
		at.fqSetRetry("", 0)
	}
	rec := fqBuildRecord(rep, dec, fq, at.initialBalance, now)
	at.saveFactorQuantRecord(rec)
	at.fqNote("rebalanced", rep.withWarnings(fmt.Sprintf("调仓完成 as_of=%s：%d 笔下单，%d 笔失败，%d 个跳过", targets.AsOf, len(rep.Results), rep.failedCount(), len(rep.Plan.Skips))))
	logger.Infof("📐 [%s] factor_quant 调仓完成 as_of=%s：%d 笔下单，%d 笔失败，%d 个跳过", at.name, targets.AsOf, len(rep.Results), rep.failedCount(), len(rep.Plan.Skips))
	at.fqSkipSeen("done|" + targets.AsOf) // a finished rebalance supersedes older skip/failure keys
}

func fqResolvedPlanConfig(fq *store.FactorQuantConfig) fqPlanConfig {
	return fqPlanConfig{
		CapitalFraction: fq.EffectiveCapitalFraction(), Band: fq.EffectiveBand(),
		MinOrder: fq.EffectiveMinOrderUSDT(), LongOnly: fq.IsUS() && fq.EffectiveUSShortMode() != "paper", CostRate: fqUSCostRate(fq),
	}
}

// factorQuantRetry is reached only through too_soon: the gate has already
// checked status, freshness and horizon, and the caller matched both as_ofs.
func (at *AutoTrader) factorQuantRetry(broker fqBroker, targets *quantTargets, state *store.FactorQuantState, dec fqDecision, now time.Time, regimeInfo fqRegimeInfo) {
	fq := at.config.StrategyConfig.FactorQuantConfig
	if state.RetryAttempts >= fqMaxRetryAttempts {
		// Recover a crash after the final attempt was persisted but before clear.
		at.fqSetRetry("", 0)
		dec.Code = fqCodeRetryExhausted
		dec.Reason = fmt.Sprintf("失败重试次数已耗尽（%d 次），等待下一次正常调仓", fqMaxRetryAttempts)
		at.fqNote("retry_exhausted", fmt.Sprintf("as_of=%s：%s", targets.AsOf, dec.Reason))
		logger.Warnf("⚠️ [%s] factor_quant as_of=%s：%s", at.name, targets.AsOf, dec.Reason)
		if !at.fqSkipSeen("retry_exhausted|" + targets.AsOf) {
			rec := fqBuildSkipRecord(targets, dec, fq, now)
			rec.Success = false
			at.saveFactorQuantRecord(rec)
		}
		return
	}
	attempt := state.RetryAttempts + 1
	dec.Reason = fmt.Sprintf("重试第 %d 次（as_of=%s，上次调仓有订单或止损失败）", attempt, targets.AsOf)
	planCfg := fqResolvedPlanConfig(fq)
	planCfg.Retry = true
	rep := fqRunRebalance(broker, targets, planCfg)
	rep.Regime = regimeInfo
	at.factorQuantProtectReport(broker, rep)
	at.fqSetRetry(targets.AsOf, attempt)
	failed := rep.Err != nil || rep.failedCount() > 0
	if !failed {
		at.fqSetRetry("", 0)
	} else if attempt == fqMaxRetryAttempts {
		at.fqSetRetry("", 0)
		dec.Code = fqCodeRetryExhausted
		dec.Reason += fmt.Sprintf("；失败重试次数已耗尽（%d 次），等待下一次正常调仓", fqMaxRetryAttempts)
		logger.Warnf("⚠️ [%s] factor_quant as_of=%s：%s", at.name, targets.AsOf, dec.Reason)
		at.fqSkipSeen("retry_exhausted|" + targets.AsOf)
	}
	if len(rep.Results) > 0 || len(rep.Stops) > 0 || failed {
		at.saveFactorQuantRecord(fqBuildRecord(rep, dec, fq, at.initialBalance, now))
	}
	code := "retry"
	if dec.Code == fqCodeRetryExhausted {
		code = "retry_exhausted"
	}
	at.fqNote(code, rep.withWarnings(fmt.Sprintf("%s：%d 笔下单，%d 笔失败，中止=%v", dec.Reason, len(rep.Results), rep.failedCount(), rep.Err)))
	logger.Infof("📐 [%s] factor_quant %s：%d 笔下单，%d 笔失败，中止=%v", at.name, dec.Reason, len(rep.Results), rep.failedCount(), rep.Err)
}

// factorQuantFlattenCheck handles ticks that cannot rebalance. Only positions
// owned by a previous rebalance may be flattened; last_as_of stays unchanged.
// Returning true suppresses ordinary skip records throughout failed attempts,
// preserving the durable flatten dedupe key across ticks and restarts.
func (at *AutoTrader) factorQuantFlattenCheck(broker fqBroker, targets *quantTargets, state *store.FactorQuantState, now time.Time) bool {
	fq := at.config.StrategyConfig.FactorQuantConfig
	fileHorizon := 0
	if targets != nil {
		fileHorizon = targets.Horizon
	}
	threshold := fq.EffectiveFlattenAfterDays(fileHorizon)
	if state.LastAsOf == "" || threshold < 0 {
		return false
	}
	last, err := time.Parse(factorDateFmt, state.LastAsOf)
	if err != nil || fqDaysBetween(last, now) <= threshold {
		return false
	}
	acct, err := broker.Account()
	if err != nil {
		at.fqNote("account_error", fmt.Sprintf("过期清仓读取账户失败：%v", err))
		logger.Warnf("⚠️ [%s] factor_quant 过期清仓读取账户失败：%v", at.name, err)
		return true
	}
	if len(acct.Positions) == 0 {
		return false
	}
	at.fqSetRetry("", 0)
	reason := fmt.Sprintf("目标已 %d 天未更新（上次调仓 as_of=%s，阈值 %d 天），自动清仓", fqDaysBetween(last, now), state.LastAsOf, threshold)
	first := !at.fqSkipSeen("flatten|" + state.LastAsOf)
	horizon := fq.EffectiveHorizon()
	if horizon == 0 {
		horizon = fileHorizon
	}
	synthetic := &quantTargets{Version: quantTargetsVersion, Market: fq.Market, Status: quantStatusOK,
		AsOf: state.LastAsOf, asOfDate: last, Horizon: horizon, Rule: quantRule{Name: state.LastRule}, Weights: map[string]float64{}}
	planCfg := fqResolvedPlanConfig(fq)
	planCfg.Flatten = true
	rep := fqRunRebalance(broker, synthetic, planCfg)
	complete := rep.Err == nil && rep.failedCount() == 0
	if first || complete {
		at.saveFactorQuantRecord(fqBuildRecord(rep, fqDecision{Code: "flatten", Horizon: horizon, Reason: reason}, fq, at.initialBalance, now))
	}
	if !complete {
		at.fqNote("flatten", rep.withWarnings(fmt.Sprintf("%s：%d 笔失败，中止=%v；下个周期继续清仓", reason, rep.failedCount(), rep.Err)))
		logger.Warnf("⚠️ [%s] factor_quant %s：%d 笔失败，中止=%v；下个周期继续清仓", at.name, reason, rep.failedCount(), rep.Err)
	} else {
		at.fqNote("flatten", rep.withWarnings(reason+"：清仓完成"))
		logger.Infof("📐 [%s] factor_quant %s：清仓完成", at.name, reason)
	}
	return true
}

// fqUSCostRate is the planner's cost buffer: fee + slippage of the US ledger.
func fqUSCostRate(fq *store.FactorQuantConfig) float64 {
	if !fq.IsUS() {
		return 0
	}
	return (fq.EffectiveUSFeeBps() + fq.EffectiveUSSlippageBps()) / 10000
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
	if rep.Regime.Benchmark != "" {
		line := rep.Regime.summaryText()
		sb.WriteString(line + "\n")
		rec.ExecutionLog = append(rec.ExecutionLog, "[factor_quant] "+line)
	}
	for _, warning := range rep.Warnings {
		sb.WriteString("警告：" + warning + "\n")
		rec.ExecutionLog = append(rec.ExecutionLog, "[factor_quant] 警告："+warning)
	}

	for _, stop := range rep.Stops {
		label := "止损（" + fqStopSideLabel(stop.Side) + "）"
		line := fmt.Sprintf("补放%s %s：%.8g", label, stop.Symbol, stop.Price)
		if stop.Err != "" {
			line = fmt.Sprintf("补放%s %s 失败：%s", label, stop.Symbol, stop.Err)
		} else if stop.Exited {
			line = fmt.Sprintf("%s %s：%s（止损价 %.8g）", label, stop.Symbol, stop.Note, stop.Price)
		}
		sb.WriteString(line + "\n")
		rec.ExecutionLog = append(rec.ExecutionLog, "[factor_quant] "+line)
	}

	if rep.Err != nil {
		next := "将在下个检查周期重试。"
		if dec.Code == fqCodeRetryExhausted {
			next = "重试次数已耗尽，等待下一次正常调仓。"
		}
		fmt.Fprintf(&sb, "调仓中止：%v\n本轮未下任何市价订单，%s", rep.Err, next)
		rec.CoTTrace = sb.String()
		rec.Success = false
		rec.ErrorMessage = rep.Err.Error()
		rec.ExecutionLog = append(rec.ExecutionLog, "[factor_quant] 调仓中止："+rep.Err.Error())
		return rec
	}

	if cr := fqUSCostRate(fq); cr > 0 {
		fmt.Fprintf(&sb, "权益 %.2f ÷ (1+成本 %.4f) × 资金比例 %.2f = 预算 %.2f USDT\n", rep.Before.Equity, cr, fq.EffectiveCapitalFraction(), rep.Plan.Budget)
	} else {
		fmt.Fprintf(&sb, "权益 %.2f × 资金比例 %.2f = 预算 %.2f USDT\n", rep.Before.Equity, fq.EffectiveCapitalFraction(), rep.Plan.Budget)
	}
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
				line += fmt.Sprintf("，止损（%s）%.6g", fqStopSideLabel(o.Side), r.StopPrice)
				if r.StopExited {
					line += "，" + r.Note
				}
			}
			if r.Fee > 0 {
				line += fmt.Sprintf("，手续费 %.4f USDT", r.Fee)
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
	broker, err := at.newFactorQuantBroker()
	if err != nil {
		return nil, err
	}
	st, err := broker.Account()
	if err != nil {
		return nil, fmt.Errorf("failed to get paper ledger: %w", err)
	}
	unrealized := st.Unrealized
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
		"available_balance": st.Balance,
		"total_pnl":         totalPnL,
		"total_pnl_pct":     pnlPct,
		"initial_balance":   at.initialBalance,
		"daily_pnl":         at.dailyPnL,
		"position_count":    len(st.Positions),
		"margin_used":       st.Exposure,
		"margin_used_pct":   usedPct,
		"paper":             true,
	}, nil
}

func (at *AutoTrader) factorQuantUSPositions() ([]map[string]interface{}, error) {
	if at.store == nil {
		return nil, fmt.Errorf("no store")
	}
	broker, err := at.newFactorQuantBroker()
	if err != nil {
		return nil, err
	}
	st, err := broker.Account()
	if err != nil {
		return nil, fmt.Errorf("failed to get paper positions: %w", err)
	}
	out := []map[string]interface{}{}
	sort.Slice(st.Positions, func(i, j int) bool { return st.Positions[i].Symbol < st.Positions[j].Symbol })
	for _, h := range st.Positions {
		value := h.Qty * h.Mark
		pnl := (h.Mark - h.Entry) * h.Qty
		if h.Side == fqSideShort {
			value, pnl = -value, -pnl
		}
		pct := 0.0
		if h.Entry > 0 && h.Qty > 0 {
			pct = pnl / (h.Entry * h.Qty) * 100
		}
		out = append(out, map[string]interface{}{
			"symbol": h.Symbol, "side": h.Side, "entry_price": h.Entry, "mark_price": h.Mark, "quantity": h.Qty,
			"leverage": 1, "unrealized_pnl": pnl, "unrealized_pnl_pct": pct, "liquidation_price": 0.0,
			"margin_used": h.Qty * h.Mark, "market_value": value, "paper": true,
		})
	}
	return out, nil
}

// factorQuantRiskStatus reads the independent portfolio breaker state. SQL and
// account reads stay outside executionMutex; fqState snapshots memory via fq.mu.
func (at *AutoTrader) factorQuantRiskStatus() RiskStatus {
	out := RiskStatus{GeneratedAt: time.Now().UTC(), InitialBalance: at.initialBalance,
		AccountDrawdownBasis: "peak",
		LossStreakBans:       []LossStreakBan{}, Pending: []PendingStatus{}, Positions: []PositionRStatus{}}
	fq := at.config.StrategyConfig.FactorQuantConfig
	out.DailyLossCapPct = fq.EffectiveDailyLossPct() * 100
	out.AccountDrawdownCap = fq.EffectiveMaxDrawdownPct() * 100
	equityOK := false
	if at.isFactorQuantUS() {
		if info, err := at.factorQuantUSAccountInfo(); err == nil {
			out.Equity, _ = info["total_equity"].(float64)
			equityOK = true
		}
	} else if at.trader != nil {
		if bal, err := at.trader.GetBalance(); err == nil {
			out.Equity = gridAccountEquity(bal)
			equityOK = true
		}
	}
	state, err := at.fqState()
	out.FactorQuant = at.fqRunStatus(state)
	if err != nil {
		out.DailyHaltUnknown, out.ReduceOnly = true, true
		out.ProtectionFault = "无法读取因子量化组合风控状态"
		return out
	}
	if equityOK {
		out.AccountDrawdownPct = fqEquityLoss(out.Equity, state.PeakEquity) * 100
	}
	if state.DayStartDate == at.fqClock().UTC().Format(factorDateFmt) {
		out.DayStartEquity = state.DayStartEquity
		if equityOK {
			out.DailyLossPct = fqEquityLoss(out.Equity, state.DayStartEquity) * 100
		}
	}
	out.DailyHalted = state.HaltKind == fqHaltDaily
	out.AccountBreakerActive = state.HaltKind == fqHaltDrawdown
	out.ReduceOnly = state.HaltKind != ""
	return out
}

// fqRunStatus snapshots observations even when durable state is unavailable.
func (at *AutoTrader) fqRunStatus(state *store.FactorQuantState) *FactorQuantRunStatus {
	at.fq.mu.Lock()
	out := &FactorQuantRunStatus{
		TickIntervalSec: int(factorQuantTickInterval / time.Second),
		RecentTicks:     append([]FactorQuantTick{}, at.fq.recentTicks...),
		TargetsAsOf:     at.fq.targetsAsOf, TargetsStatus: at.fq.targetsStatus, TargetsRule: at.fq.targetsRule,
		Horizon: at.config.StrategyConfig.FactorQuantConfig.EffectiveHorizon(),
	}
	if out.Horizon == 0 {
		out.Horizon = at.fq.targetsHorizon
	}
	if at.fq.regime != nil {
		out.Regime = at.fq.regime.toStatus()
	}
	if state == nil {
		state = &store.FactorQuantState{LastAsOf: at.fq.memAsOf, RetryAsOf: at.fq.retryAsOf, RetryAttempts: at.fq.retryAttempts}
	}
	at.fq.mu.Unlock()
	if len(out.RecentTicks) > 0 {
		last := out.RecentTicks[0]
		out.LastTick = &last
	}
	out.LastRebalanceAsOf = state.LastAsOf
	if state.RebalanceAt != 0 {
		rebalancedAt := time.UnixMilli(state.RebalanceAt).UTC()
		out.LastRebalanceAt = &rebalancedAt
	}
	if last, err := time.Parse(factorDateFmt, state.LastAsOf); err == nil && out.Horizon > 0 {
		out.NextEligibleAsOf = last.AddDate(0, 0, out.Horizon).Format(factorDateFmt)
	}
	out.RetryAsOf, out.RetryAttempts = state.RetryAsOf, state.RetryAttempts
	return out
}
