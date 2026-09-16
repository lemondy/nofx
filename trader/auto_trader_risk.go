package trader

import (
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	notify "nofx/telegram/notify"
	"nofx/trader/binance"
	"nofx/trader/types"
	"strings"
	"time"
)

// gateNotifyState tracks consecutive hard-gate blocks per symbol so repeated
// bars (the AI retrying every cycle) don't spam Telegram — the first push
// goes out, follow-ups within the window are logged with a running counter.
// (Rebuilt 09-13 after the risk.go corruption — window approximate.)
type gateNotifyState struct {
	Streak int
	Last   time.Time
}

// gateNotifyRecord returns the running streak for a gate key and whether a
// Telegram push should go out (first block, or the first block after the
// dedup window).
func (at *AutoTrader) gateNotifyRecord(key string, now time.Time) (int, bool) {
	at.gateNotifyMu.Lock()
	defer at.gateNotifyMu.Unlock()
	if at.gateNotify == nil {
		at.gateNotify = make(map[string]*gateNotifyState)
	}
	const dedupWindow = 30 * time.Minute // pinned by TestGateNotifyDedup
	if st, ok := at.gateNotify[key]; ok && now.Sub(st.Last) < dedupWindow {
		// Follow-up inside the window: silent, streak climbs, Last unchanged —
		// pushes stay at most once per window per key.
		st.Streak++
		return st.Streak, false
	}
	at.gateNotify[key] = &gateNotifyState{Streak: 1, Last: now}
	return 1, true
}

// drawdownProtectThresholds resolves the drawdown-protect pair: profit above
// minProfit% that then retraces maxDD% of the peak triggers the protective
// close. 0/unset = 5/55 defaults.
func (at *AutoTrader) drawdownProtectThresholds() (minProfit, maxDD float64) {
	minProfit, maxDD = 5.0, 55.0
	if at.config.StrategyConfig == nil {
		return minProfit, maxDD
	}
	rc := at.config.StrategyConfig.RiskControl
	if rc.PeakDrawdownMinProfitPct > 0 {
		minProfit = rc.PeakDrawdownMinProfitPct
	}
	if rc.PeakDrawdownMaxDDPct > 0 {
		maxDD = rc.PeakDrawdownMaxDDPct
	}
	return minProfit, maxDD
}

// startDrawdownMonitor starts drawdown monitoring
func (at *AutoTrader) startDrawdownMonitor() {
	at.monitorWg.Add(1)
	go func() {
		defer at.monitorWg.Done()

		ticker := time.NewTicker(1 * time.Minute) // Check every minute
		defer ticker.Stop()

		logger.Info("📊 Started position drawdown monitoring (check every minute)")

		for {
			select {
			case <-ticker.C:
				at.safeCheckPositionDrawdown()
			case <-at.stopMonitorCh:
				logger.Info("⏹ Stopped position drawdown monitoring")
				return
			}
		}
	}()
}

// safeCheckPositionDrawdown runs one drawdown-check pass with a recover
// backstop — the type-asserted fields above are now guarded, but this monitor
// must never die silently on an unexpected panic (e.g. a future edit that
// reintroduces an unguarded assertion): one bad cycle logs and moves on
// instead of taking down the whole goroutine for the rest of the process.
func (at *AutoTrader) safeCheckPositionDrawdown() {
	defer func() {
		if r := recover(); r != nil {
			logger.Infof("❌ Drawdown monitoring: recovered from panic: %v", r)
		}
	}()
	at.checkPositionDrawdown()
}

// checkPositionDrawdown checks position drawdown situation
func (at *AutoTrader) checkPositionDrawdown() {
	// Get current positions
	positions, err := at.trader.GetPositions()
	if err != nil {
		logger.Infof("❌ Drawdown monitoring: failed to get positions: %v", err)
		return
	}

	for _, pos := range positions {
		symbol, ok := pos["symbol"].(string)
		if !ok || symbol == "" {
			logger.Warnf("⚠️ Drawdown monitoring: position missing/malformed symbol, skipping: %+v", pos)
			continue
		}
		side, ok := pos["side"].(string)
		if !ok || side == "" {
			logger.Warnf("⚠️ Drawdown monitoring: %s missing/malformed side, skipping", symbol)
			continue
		}
		entryPrice, ok := pos["entryPrice"].(float64)
		if !ok {
			logger.Warnf("⚠️ Drawdown monitoring: %s %s missing/malformed entryPrice, skipping", symbol, side)
			continue
		}
		markPrice, ok := pos["markPrice"].(float64)
		if !ok {
			logger.Warnf("⚠️ Drawdown monitoring: %s %s missing/malformed markPrice, skipping", symbol, side)
			continue
		}
		quantity, ok := pos["positionAmt"].(float64)
		if !ok {
			logger.Warnf("⚠️ Drawdown monitoring: %s %s missing/malformed positionAmt, skipping", symbol, side)
			continue
		}
		if quantity < 0 {
			quantity = -quantity // Short position quantity is negative, convert to positive
		}

		// Guard: skip if entry price is zero (prevents division by zero panic)
		if entryPrice <= 0 {
			logger.Warnf("⚠️ Drawdown monitoring: %s %s has zero entry price, skipping", symbol, side)
			continue
		}

		// Calculate current P&L percentage
		leverage := 10 // Default value
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}

		var currentPnLPct float64
		if side == "long" {
			currentPnLPct = ((markPrice - entryPrice) / entryPrice) * float64(leverage) * 100
		} else {
			currentPnLPct = ((entryPrice - markPrice) / entryPrice) * float64(leverage) * 100
		}

		// Construct unique position identifier (distinguish long/short)
		posKey := symbol + "_" + side

		// Get historical peak profit for this position
		at.peakPnLCacheMutex.RLock()
		peakPnLPct, exists := at.peakPnLCache[posKey]
		at.peakPnLCacheMutex.RUnlock()

		if !exists {
			// If no historical peak record, use current P&L as initial value
			peakPnLPct = currentPnLPct
			at.UpdatePeakPnL(symbol, side, currentPnLPct)
		} else {
			// Update peak cache
			at.UpdatePeakPnL(symbol, side, currentPnLPct)
		}

		// Calculate drawdown (magnitude of decline from peak)
		var drawdownPct float64
		if peakPnLPct > 0 && currentPnLPct < peakPnLPct {
			drawdownPct = ((peakPnLPct - currentPnLPct) / peakPnLPct) * 100
		}

		// ── TP ladder (user 2026-09-11): ≥ full → close the rest; ≥ trim →
		// market-trim 1/3 once. Leveraged PnL%, same basis as the peak cache.
		at.tpTrimMutex.Lock()
		trimDone := at.tpTrimDone[posKey]
		at.tpTrimMutex.Unlock()
		switch kernel.TpTierAction(currentPnLPct, trimDone, &at.config.StrategyConfig.RiskControl) {
		case "full":
			logger.Infof("🎯 TP ladder FULL close: %s %s | PnL %.2f%% ≥ %.0f%%", symbol, side, currentPnLPct, kernel.TpFullProfitPct(&at.config.StrategyConfig.RiskControl))
			notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🎯 止盈全平 %s (%s)</b>\n浮盈 <code>%.2f%%</code> ≥ %.0f%%,程序全部平仓锁定利润。",
				notify.Escape(symbol), strings.ToUpper(side[:1])+side[1:], currentPnLPct, kernel.TpFullProfitPct(&at.config.StrategyConfig.RiskControl)))
			at.tpTrimMutex.Lock()
			at.r1TrimDone[posKey] = true
			at.tpTrimDone[posKey] = true
			at.tpTrimMutex.Unlock()
			if err := at.emergencyClosePosition(symbol, side); err != nil {
				logger.Infof("❌ TP full close failed (%s %s): %v", symbol, side, err)
			} else {
				logger.Infof("✅ TP full close succeeded: %s %s", symbol, side)
			}
			continue
		case "trim":
			trimQty := quantity / 3
			logger.Infof("🎯 TP ladder TRIM: %s %s | PnL %.2f%% ≥ %.0f%% — trimming 1/3 (%.6g)", symbol, side, currentPnLPct, kernel.TpTrimProfitPct(&at.config.StrategyConfig.RiskControl), trimQty)
			notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🎯 止盈减仓 1/3 %s (%s)</b>\n浮盈 <code>%.2f%%</code> ≥ %.0f%%,程序市价减仓 1/3。",
				notify.Escape(symbol), strings.ToUpper(side[:1])+side[1:], currentPnLPct, kernel.TpTrimProfitPct(&at.config.StrategyConfig.RiskControl)))
			var err error
			if side == "long" {
				_, err = at.trader.CloseLong(symbol, trimQty)
			} else {
				_, err = at.trader.CloseShort(symbol, trimQty)
			}
			at.tpTrimMutex.Lock()
			at.r1TrimDone[posKey] = true
			at.tpTrimDone[posKey] = true
			at.tpTrimMutex.Unlock()
			if err != nil {
				logger.Infof("❌ TP trim failed (%s %s): %v", symbol, side, err)
			} else {
				logger.Infof("✅ TP trim succeeded: %s %s qty=%.6g (protective orders kept)", symbol, side, trimQty)
			}
			continue
		}

		// Check close condition — thresholds from strategy risk_control
		// (peak_drawdown_min_profit_pct / peak_drawdown_max_dd_pct, defaults 5/55).
		minProfit, maxDD := at.drawdownProtectThresholds()
		if currentPnLPct > minProfit && drawdownPct >= maxDD {
			logger.Infof("🚨 Drawdown close position condition triggered: %s %s | Current profit: %.2f%% | Peak profit: %.2f%% | Drawdown: %.2f%% (thresholds: >%.1f%% & ≥%.1f%%)",
				symbol, side, currentPnLPct, peakPnLPct, drawdownPct, minProfit, maxDD)
			notify.Notify("RISK", at.name, fmt.Sprintf("<b>🚨 回撤保护平仓 %s (%s)</b>\n浮盈峰值 <code>%.2f%%</code> → 当前 <code>%.2f%%</code>(回撤 %.1f%% ≥ %.1f%%)\n触发保护性平仓锁定利润。",
				notify.Escape(symbol), strings.ToUpper(side[:1])+side[1:], peakPnLPct, currentPnLPct, drawdownPct, maxDD))

			// Execute close position
			if err := at.emergencyClosePosition(symbol, side); err != nil {
				logger.Infof("❌ Drawdown close position failed (%s %s): %v", symbol, side, err)
				notify.Notify("ALERT", at.name, fmt.Sprintf("<b>❌ 回撤保护平仓失败 %s (%s)</b>\n<code>%s</code>\n请人工检查持仓！",
					notify.Escape(symbol), strings.ToUpper(side[:1])+side[1:], notify.Escape(err.Error())))
			} else {
				logger.Infof("✅ Drawdown close position succeeded: %s %s", symbol, side)
				notify.Notify("ORDER", at.name, fmt.Sprintf("<b>✅ 回撤保护平仓完成 %s (%s)</b>\n浮盈 <code>%+.2f%%</code> 已落袋。", symbol, strings.ToUpper(side[:1])+side[1:], currentPnLPct))
				// Clear cache for this position after closing
				at.ClearPeakPnLCache(symbol, side)
			}
		} else if currentPnLPct > minProfit {
			// Record situations close to close position condition (for debugging)
			logger.Infof("📊 Drawdown monitoring: %s %s | Profit: %.2f%% | Peak: %.2f%% | Drawdown: %.2f%%",
				symbol, side, currentPnLPct, peakPnLPct, drawdownPct)
		}
	}
}

// emergencyClosePosition emergency close position function
func (at *AutoTrader) emergencyClosePosition(symbol, side string) error {
	switch side {
	case "long":
		order, err := at.trader.CloseLong(symbol, 0) // 0 = close all
		if err != nil {
			return err
		}
		logger.Infof("✅ Emergency close long position succeeded, order ID: %v", order["orderId"])
	case "short":
		order, err := at.trader.CloseShort(symbol, 0) // 0 = close all
		if err != nil {
			return err
		}
		logger.Infof("✅ Emergency close short position succeeded, order ID: %v", order["orderId"])
	default:
		return fmt.Errorf("unknown position direction: %s", side)
	}

	at.ClearRecordedStopLoss(symbol, side)
	return nil
}

// GetPeakPnLCache gets peak profit cache
func (at *AutoTrader) GetPeakPnLCache() map[string]float64 {
	at.peakPnLCacheMutex.RLock()
	defer at.peakPnLCacheMutex.RUnlock()

	// Return a copy of the cache
	cache := make(map[string]float64)
	for k, v := range at.peakPnLCache {
		cache[k] = v
	}
	return cache
}

// UpdatePeakPnL updates peak profit cache
func (at *AutoTrader) UpdatePeakPnL(symbol, side string, currentPnLPct float64) {
	at.peakPnLCacheMutex.Lock()
	defer at.peakPnLCacheMutex.Unlock()

	posKey := symbol + "_" + side
	if peak, exists := at.peakPnLCache[posKey]; exists {
		// Update peak (if long, take larger value; if short, currentPnLPct is negative, also compare)
		if currentPnLPct > peak {
			at.peakPnLCache[posKey] = currentPnLPct
		}
	} else {
		// First time recording
		at.peakPnLCache[posKey] = currentPnLPct
	}
}

// ClearPeakPnLCache clears peak cache for specified position
func (at *AutoTrader) ClearPeakPnLCache(symbol, side string) {
	at.peakPnLCacheMutex.Lock()
	defer at.peakPnLCacheMutex.Unlock()

	posKey := symbol + "_" + side
	delete(at.peakPnLCache, posKey)

	// Trim/ladder state shares the peak-PnL lifecycle: a fresh position
	// starts with its trims unspent.
	at.tpTrimMutex.Lock()
	delete(at.tpTrimDone, posKey)
	delete(at.r1TrimDone, posKey)
	delete(at.partialTrimmed, posKey)
	at.tpTrimMutex.Unlock()
}

// ============================================================================
// Risk Control Helpers
// ============================================================================

// isBTCETH checks if a symbol is BTC or ETH
func isBTCETH(symbol string) bool {
	symbol = strings.ToUpper(symbol)
	return strings.HasPrefix(symbol, "BTC") || strings.HasPrefix(symbol, "ETH")
}

// enforcePositionValueRatio checks and enforces position value ratio limits (CODE ENFORCED)
// Returns the adjusted position size (capped if necessary) and whether the position was capped
// positionSizeUSD: the original position size in USD
// equity: the account equity
// symbol: the trading symbol
func (at *AutoTrader) enforcePositionValueRatio(positionSizeUSD float64, equity float64, symbol string) (float64, bool) {
	if at.config.StrategyConfig == nil {
		return positionSizeUSD, false
	}

	riskControl := at.config.StrategyConfig.RiskControl

	// Get the appropriate position value ratio limit
	var maxPositionValueRatio float64
	if isBTCETH(symbol) {
		maxPositionValueRatio = riskControl.BTCETHMaxPositionValueRatio
		if maxPositionValueRatio <= 0 {
			maxPositionValueRatio = 5.0 // Default: 5x for BTC/ETH
		}
	} else {
		maxPositionValueRatio = riskControl.AltcoinMaxPositionValueRatio
		if maxPositionValueRatio <= 0 {
			maxPositionValueRatio = 1.0 // Default: 1x for altcoins
		}
	}

	// Calculate max allowed position value = equity × ratio
	maxPositionValue := equity * maxPositionValueRatio

	// Check if position size exceeds limit
	if positionSizeUSD > maxPositionValue {
		logger.Infof("  ⚠️ [RISK CONTROL] Position %.2f USDT exceeds limit (equity %.2f × %.1fx = %.2f USDT max for %s), capping",
			positionSizeUSD, equity, maxPositionValueRatio, maxPositionValue, symbol)
		return maxPositionValue, true
	}

	return positionSizeUSD, false
}

// enforceMinPositionSize checks minimum position size (CODE ENFORCED)
func (at *AutoTrader) enforceMinPositionSize(positionSizeUSD float64) error {
	if at.config.StrategyConfig == nil {
		return nil
	}

	minSize := at.config.StrategyConfig.RiskControl.MinPositionSize
	if minSize <= 0 {
		minSize = 12 // Default: 12 USDT
	}

	if positionSizeUSD < minSize {
		return fmt.Errorf("❌ [RISK CONTROL] Position %.2f USDT below minimum (%.2f USDT)", positionSizeUSD, minSize)
	}
	return nil
}

// enforceMaxPositions checks maximum positions count (CODE ENFORCED)
func (at *AutoTrader) enforceMaxPositions(currentPositionCount int) error {
	if at.config.StrategyConfig == nil {
		return nil
	}

	maxPositions := at.config.StrategyConfig.RiskControl.MaxPositions
	if maxPositions <= 0 {
		maxPositions = 3 // Default: 3 positions
	}

	if currentPositionCount >= maxPositions {
		return fmt.Errorf("❌ [RISK CONTROL] Already at max positions (%d/%d)", currentPositionCount, maxPositions)
	}
	return nil
}

// getSideFromAction converts order action to side (BUY/SELL)
func getSideFromAction(action string) string {
	switch action {
	case "open_long", "close_short":
		return "BUY"
	case "open_short", "close_long":
		return "SELL"
	default:
		return "BUY"
	}
}

// ============================================================================
// Open-risk validation (stop window rides dual yardsticks)
// ============================================================================

// validateOpenRisk enforces entry risk gates on open decisions before any
// order is sent: mandatory stop-loss, SL/TP side sanity, the strategy's
// minimum risk-reward ratio, and an outlier wide-stop cap. Returns an error
// to reject the open. The stop window rides two yardsticks (user directive
// 2026-09-10): the noise FLOOR scales on ATR(1h) — the rhythm the trade
// actually needs to survive — while the wide-stop CAP stays on ATR(4h)
// (max(2×ATR(4h), 8%)): the 1h cap bound at the 8% floor on violent movers
// and pushed structural stops out of the band.
func (at *AutoTrader) validateOpenRisk(decision *kernel.Decision, entryPrice, floorATRPct, capATRPct float64) error {
	if at.config.StrategyConfig == nil || entryPrice <= 0 {
		return nil
	}
	minRR := at.config.StrategyConfig.RiskControl.MinRiskRewardRatio
	isLong := strings.HasPrefix(decision.Action, "open_long")

	// 1. Stop loss is mandatory — it is placed on the exchange right after
	// the open, so an entry without one would run unprotected.
	if decision.StopLoss <= 0 {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: missing stop_loss (entry would be unprotected)", decision.Action, decision.Symbol)
	}

	// 1b. Take profit is mandatory too (audit 09-13 #3): the min-RR gate only
	// runs when a TP exists, so an open that simply omits take_profit used to
	// bypass the entire reward-side validation AND place no TP protection on
	// the exchange. The prompt requires TP on every open — enforce it.
	if decision.TakeProfit <= 0 {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: missing take_profit (min-RR gate requires a target; omitting TP does not skip validation)", decision.Action, decision.Symbol)
	}

	// 2. SL/TP must sit on the correct side of the entry price, otherwise the
	// exchange rejects the conditional orders.
	if isLong && decision.StopLoss >= entryPrice {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: stop_loss %.6g must be below entry %.6g", decision.Action, decision.Symbol, decision.StopLoss, entryPrice)
	}
	if !isLong && decision.StopLoss <= entryPrice {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: stop_loss %.6g must be above entry %.6g", decision.Action, decision.Symbol, decision.StopLoss, entryPrice)
	}
	if decision.TakeProfit > 0 {
		if isLong && decision.TakeProfit <= entryPrice {
			return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: take_profit %.6g must be above entry %.6g", decision.Action, decision.Symbol, decision.TakeProfit, entryPrice)
		}
		if !isLong && decision.TakeProfit >= entryPrice {
			return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: take_profit %.6g must be below entry %.6g", decision.Action, decision.Symbol, decision.TakeProfit, entryPrice)
		}
	}

	// 3. Minimum risk-reward ratio, anchored at BOTH the AI's decision price
	// and the live execution price. Validating only the live ticker let a
	// market-order fill away from the checked price smuggle in a below-floor
	// plan (SOL: decision RR 2.08 vs required 3.0, passed on a lower ticker).
	if minRR > 0 && decision.TakeProfit > 0 {
		if decision.Price > 0 {
			if err := checkRR(decision, decision.Price, minRR); err != nil {
				return err
			}
		}
		if err := checkRR(decision, entryPrice, minRR); err != nil {
			return err
		}
	}

	// 4. Outlier wide-stop cap: SL distance ≤ max(2×ATR(4h), 8%). Volatile
	// coins get proportionally more room (a stop inside 2×ATR is noise
	// fodder), but never unbounded — an 8.9% stop at 3x leverage is -27%
	// margin on one trade. The CAP yardstick stays 4h (user directive
	// 2026-09-10): on the 1h scale it bound at the 8% floor and rejected
	// structural stops on exactly the violent movers the pool surfaces.
	slDistPct := math.Abs(decision.StopLoss-entryPrice) / entryPrice * 100
	maxSLDist := 8.0
	if capATRPct*2 > maxSLDist {
		maxSLDist = capATRPct * 2
	}
	if slDistPct > maxSLDist {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: stop distance %.2f%% exceeds cap %.2f%% (max(2×ATR(4h) %.2f%%, 8%%))",
			decision.Action, decision.Symbol, slDistPct, maxSLDist, capATRPct)
	}

	// 5. Noise floor: stop closer than SLMinATRMult × ATR(1h) gets swept by
	// normal fluctuation before the trend can develop (the FLOOR yardstick is
	// 1h — the rhythm the trade must survive). Together with the cap above
	// this pins the stop to [floor, cap].
	if floorMult := at.config.StrategyConfig.RiskControl.SLMinATRMult; floorMult > 0 && floorATRPct > 0 {
		floor := floorMult * floorATRPct
		if slDistPct < floor {
			return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: stop distance %.2f%% below noise floor %.2f%% (%.1f×ATR(1h) %.2f%%) — widen to the next structure level",
				decision.Action, decision.Symbol, slDistPct, floor, floorMult, floorATRPct)
		}
	}
	return nil
}

// checkRR validates the risk-reward ratio at one price anchor.
func checkRR(decision *kernel.Decision, entryPrice, minRR float64) error {
	isLong := strings.HasPrefix(decision.Action, "open_long")
	risk := entryPrice - decision.StopLoss
	reward := decision.TakeProfit - entryPrice
	if !isLong {
		risk = decision.StopLoss - entryPrice
		reward = entryPrice - decision.TakeProfit
	}
	if risk <= 0 {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: invalid stop distance", decision.Action, decision.Symbol)
	}
	rr := reward / risk
	if rr < minRR {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: risk-reward 1:%.2f below required 1:%.1f (anchor %.6g SL %.6g TP %.6g)",
			decision.Action, decision.Symbol, rr, minRR, entryPrice, decision.StopLoss, decision.TakeProfit)
	}
	return nil
}

// reanchorProtectivePrices shifts the AI's SL/TP by the difference between
// the actual fill and the price the RR gate validated at, so the PLANNED
// stop/target distances survive market-order slippage.
func reanchorProtectivePrices(decision *kernel.Decision, refPrice, fillPrice float64) {
	if refPrice <= 0 || fillPrice <= 0 || decision.StopLoss <= 0 {
		return
	}
	delta := fillPrice - refPrice
	if decision.StopLoss > 0 {
		decision.StopLoss += delta
	}
	if decision.TakeProfit > 0 {
		decision.TakeProfit += delta
	}
}

// ============================================================================
// ATR yardsticks
// ============================================================================

// atrPctFromTimeframes returns ATR(14) as a percent of price from the first
// timeframe in the priority list with enough bars — deterministic; 0 when
// none qualify.
func atrPctFromTimeframes(data *market.Data, names ...string) float64 {
	if data == nil {
		return 0
	}
	for _, name := range names {
		if tf, ok := data.TimeframeData[name]; ok && len(tf.Klines) >= 15 {
			return atrPercentFromSeries(tf)
		}
	}
	return 0
}

// oneHourATRPct returns ATR(14) as a percent of price on the 1h timeframe.
// The stop-loss noise FLOOR's yardstick (user directive 2026-09-10: buffer
// and floor ride the 1h rhythm the trade must survive) and the vol-target
// rescale / trailing stop yardstick. Falls back toward other TFs
// deterministically; 0 when unavailable.
func oneHourATRPct(data *market.Data) float64 {
	return atrPctFromTimeframes(data, "1h", "2h", "4h", "6h", "8h", "12h", "1d", "30m", "15m", "5m", "3m")
}

// fourHourATRPct returns ATR(14) as a percent of price on the 4h timeframe —
// the wide-stop CAP yardstick (user directive 2026-09-10: on the 1h scale the
// cap bound at the 8% floor and pushed structural stops out of the band on
// exactly the violent movers the candidate pool surfaces).
func fourHourATRPct(data *market.Data) float64 {
	return atrPctFromTimeframes(data, "4h", "6h", "8h", "12h", "1d", "2h", "1h", "30m", "15m", "5m", "3m")
}

// atrPercentFromSeries computes Wilder ATR(14) as a percent of the last
// close from a timeframe's kline series.
func atrPercentFromSeries(tf *market.TimeframeSeriesData) float64 {
	kb := make([]market.Kline, len(tf.Klines))
	for i, b := range tf.Klines {
		kb[i] = market.Kline{OpenTime: b.Time, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume}
	}
	// Wilder ATR(14), percent of the last close.
	n := len(kb)
	if n < 15 {
		return 0
	}
	tr := make([]float64, n)
	tr[0] = kb[0].High - kb[0].Low
	for i := 1; i < n; i++ {
		pc := kb[i-1].Close
		tr[i] = math.Max(kb[i].High-kb[i].Low, math.Max(math.Abs(kb[i].High-pc), math.Abs(kb[i].Low-pc)))
	}
	a := 0.0
	for i := 1; i <= 14; i++ {
		a += tr[i]
	}
	a /= 14
	for i := 15; i < n; i++ {
		a = (a*13 + tr[i]) / 14
	}
	lastClose := kb[n-1].Close
	if lastClose <= 0 {
		return 0
	}
	return a / lastClose * 100
}

// clampSizeToRisk caps the position value at equity × RiskPerTradePct% ÷
// stop-distance% — sizing derives from the stop, never the other way round
// (framework: 单笔风险金额固定,止损越远仓位越小). Returns the capped size.
func (at *AutoTrader) clampSizeToRisk(decision *kernel.Decision, sizeUSD, equity, livePrice float64) float64 {
	riskPct := at.config.StrategyConfig.RiskControl.RiskPerTradePct
	if riskPct <= 0 {
		riskPct = 1.5 // default single-trade risk budget
	}
	if equity <= 0 || livePrice <= 0 || decision.StopLoss <= 0 || sizeUSD <= 0 {
		return sizeUSD
	}
	distPct := math.Abs(livePrice-decision.StopLoss) / livePrice * 100
	if distPct <= 0 {
		return sizeUSD
	}
	maxNotional := equity * riskPct / distPct
	if sizeUSD <= maxNotional {
		return sizeUSD
	}
	logger.Infof("  ⚠️ [RISK CONTROL] Position %.2f USDT exceeds risk budget (equity %.2f × %.1f%% ÷ stop %.2f%% = %.2f USDT), clamping",
		sizeUSD, equity, riskPct, distPct, maxNotional)
	return maxNotional
}

// ============================================================================
// Spread gate
// ============================================================================

// topOfBookSpreadPct returns the order-book spread as a percent of mid:
// (bestAsk − bestBid) / ((bestAsk+bestBid)/2) × 100. 0 on malformed input —
// the spread gate fails open, never blocks on unusable data.
func topOfBookSpreadPct(bids, asks [][]float64) float64 {
	if len(bids) == 0 || len(asks) == 0 {
		return 0
	}
	bid, ask := bids[0][0], asks[0][0]
	if bid <= 0 || ask <= 0 || ask < bid {
		return 0 // crossed/garbage book — treat as unusable, fail-open
	}
	mid := (ask + bid) / 2
	return (ask - bid) / mid * 100
}

// spreadBlocksOpen rejects opens on symbols whose live order-book spread
// exceeds the threshold (user directive 2026-09-11, the liquidity gap from
// the original spec): a wide spread eats the limit-order edge and directly
// taxes market fills on thin candidates. Threshold from
// risk_control.max_spread_pct (0 = 0.5% default, negative = disabled).
// Fail-open: no book interface or unusable book never blocks.
func (at *AutoTrader) spreadBlocksOpen(symbol string) (bool, string) {
	if at.config.StrategyConfig == nil {
		return false, ""
	}
	threshold := kernel.MaxSpreadPct(&at.config.StrategyConfig.RiskControl)
	if threshold <= 0 {
		return false, "" // disabled
	}
	book, ok := at.trader.(interface {
		GetOrderBook(symbol string, depth int) (bids, asks [][]float64, err error)
	})
	if !ok {
		return false, ""
	}
	bids, asks, err := book.GetOrderBook(symbol, 5)
	if err != nil {
		// Fail-open, but never silently: an unmeasurable book means the gate
		// is blind for this symbol this cycle (audit 09-13 #5).
		logger.Infof("⚠️ [RISK CONTROL] spread gate: order book unavailable for %s (%v) — gate skipped for this cycle", symbol, err)
		return false, ""
	}
	spread := topOfBookSpreadPct(bids, asks)
	if spread <= 0 {
		return false, "" // fail-open
	}
	if spread <= threshold {
		return false, ""
	}
	return true, fmt.Sprintf(
		"spread gate: order-book spread %.3f%% > threshold %.2f%% of mid — thin book, the anchor edge would be eaten by the spread",
		spread, threshold)
}

// ============================================================================
// Margin budget gate
// ============================================================================

// usedMarginOf sums per-position margin (notional ÷ leverage) from open
// positions. A position with a nonzero size but unusable mark/leverage is
// SKIPPED (fail-open) — that silently undercounts used margin, so it logs a
// warning instead of disappearing (audit 09-13 #5).
func usedMarginOf(positions []map[string]interface{}) float64 {
	total := 0.0
	for _, pos := range positions {
		qty, _ := pos["positionAmt"].(float64)
		if qty == 0 {
			continue
		}
		symbol, _ := pos["symbol"].(string)
		mark, _ := pos["markPrice"].(float64)
		lev, _ := pos["leverage"].(float64)
		if mark <= 0 || lev <= 0 {
			logger.Warnf("⚠️ [RISK CONTROL] margin budget: %s position excluded from used-margin (mark=%.6g lev=%.1f) — used margin is UNDERCOUNTED, check exchange data",
				symbol, mark, lev)
			continue
		}
		if qty < 0 {
			qty = -qty
		}
		total += qty * mark / lev
	}
	return total
}

// marginExceedsBudget: (used + new) margin vs budget fraction × equity.
func marginExceedsBudget(usedMargin, newMargin, equity, budgetFrac float64) bool {
	if equity <= 0 || budgetFrac <= 0 {
		return false
	}
	return usedMargin+newMargin > budgetFrac*equity
}

// pendingMarginReserved sums the margin that resting AI limit-entry orders
// on OTHER symbols would consume if they all filled (notional ÷ leverage).
// Each placement passed the margin gate against the used margin AT PLACEMENT
// TIME — none of them sees the others — so without this reservation N
// pending limits checked individually could all fill and jointly blow the
// budget. excludeSymbol is the symbol currently being opened (its own pending
// entry was already dropped/replaced on that path).
func (at *AutoTrader) pendingMarginReserved(excludeSymbol string) float64 {
	at.pendingEntriesMu.RLock()
	defer at.pendingEntriesMu.RUnlock()
	total := 0.0
	for sym, pe := range at.pendingEntries {
		if sym == excludeSymbol || pe == nil || pe.Price <= 0 || pe.Quantity <= 0 {
			continue
		}
		lev := float64(pe.Leverage)
		if lev <= 0 {
			lev = 1 // assume worst case: no leverage → full notional as margin
		}
		total += pe.Price * pe.Quantity / lev
	}
	return total
}

// marginBudgetBlocksOpen rejects an open when the resulting total margin
// usage would exceed risk_control.max_margin_usage (0 = 90% default). The
// prompt states this budget but until now nothing enforced it — a max-size
// BTC/ETH position alone could cross the 90% line (audit 09-13 #2).
// Resting limit entries count as reserved margin (audit 09-13 #4: N pending
// limits each checked in isolation can jointly exceed the budget once they
// all fill). Fail-open on data errors.
func (at *AutoTrader) marginBudgetBlocksOpen(symbol string, newSizeUSD, newLeverage, equity float64) (bool, string) {
	if at.config.StrategyConfig == nil || newSizeUSD <= 0 || newLeverage <= 0 || equity <= 0 {
		return false, ""
	}
	budget := at.config.StrategyConfig.RiskControl.MaxMarginUsage
	if budget <= 0 {
		budget = 0.9
	}
	positions, err := at.trader.GetPositions()
	if err != nil {
		return false, ""
	}
	newMargin := newSizeUSD / newLeverage
	used := usedMarginOf(positions) + at.pendingMarginReserved(symbol)
	if !marginExceedsBudget(used, newMargin, equity, budget) {
		return false, ""
	}
	return true, fmt.Sprintf(
		"margin budget: used %.2f (incl. %.2f reserved by resting limit entries) + new %.2f USDT (size %.2f @ %.0fx) would exceed %.0f%% × equity %.2f (%.2f USDT) — reduce size/leverage or close a position first",
		used, at.pendingMarginReserved(symbol), newMargin, newSizeUSD, newLeverage, budget*100, equity, budget*equity)
}

// ============================================================================
// Early-close lock
// ============================================================================

// oneHAgainstCandles counts the trailing consecutive CLOSED 1h candles that
// run AGAINST the position direction (long → bearish candles, short →
// bullish candles). The forming bar is excluded. This is the trend-change
// evidence the early-close gate wants: the 1h rhythm itself turning.
func oneHAgainstCandles(data *market.Data, side string) int {
	if data == nil {
		return 0
	}
	tf, ok := data.TimeframeData["1h"]
	if !ok || tf == nil || len(tf.Klines) < 2 {
		return 0
	}
	bars := tf.Klines[:len(tf.Klines)-1] // drop the forming bar — closed candles only
	n := 0
	for i := len(bars) - 1; i >= 0; i-- {
		b := bars[i]
		if b.Close == b.Open {
			break // doji: no direction, breaks the streak
		}
		against := (side == "long" && b.Close < b.Open) || (side == "short" && b.Close > b.Open)
		if !against {
			break
		}
		n++
	}
	return n
}

// earlyCloseBlocksClose gates AI-initiated closes that happen BEFORE the
// position has ridden its intended swing (user directive 2026-09-11): a close
// earlier than EarlyCloseMinHours (default 4h) is only allowed when the 1h
// timeframe shows ≥2 closed candles against the position direction — real
// trend-change evidence, not a wobble. Exempt by construction: exchange
// SL/TP triggers and the drawdown-protect close are program paths, not AI
// decisions; a mark at/beyond the recorded stop also passes (the position is
// stopping out regardless). Fail-open on missing data — a gate must never
// trap a position it cannot see.
func (at *AutoTrader) earlyCloseBlocksClose(symbol, side string, markPrice float64, data *market.Data) (bool, string) {
	if at.config.StrategyConfig == nil {
		return false, ""
	}
	hours := kernel.EarlyCloseHours(&at.config.StrategyConfig.RiskControl)
	if hours <= 0 {
		return false, ""
	}
	posKey := symbol + "_" + side
	openMs, exists := at.positionFirstSeenTime[posKey]
	if !exists || openMs <= 0 {
		return false, "" // age unknown — don't block
	}
	held := time.Since(time.UnixMilli(openMs))
	if held >= time.Duration(hours)*time.Hour {
		return false, ""
	}
	// Hard-exit bypass: the mark is already at/beyond the recorded stop.
	if sl := at.GetRecordedStopLoss(symbol, side); sl > 0 && markPrice > 0 {
		if (side == "long" && markPrice <= sl) || (side == "short" && markPrice >= sl) {
			return false, ""
		}
	}
	evidence := oneHAgainstCandles(data, side)
	if evidence >= 2 {
		return false, "" // 1h trend-change candles present — exit allowed
	}
	return true, fmt.Sprintf(
		"early-close lock: held %.1fh < %dh and 1h shows only %d against-direction closed candle(s) (need ≥2 trend-change candles); SL/TP and drawdown-protect paths unaffected",
		held.Hours(), hours, evidence)
}

// minHoldBlocksClose reports whether an AI-initiated close must be blocked by
// the minimum holding period lock. Hard exits are never blocked: the price
// already crossed the recorded stop-loss (exchange stop handles the exit).
func (at *AutoTrader) minHoldBlocksClose(symbol, side string, markPrice float64) (bool, string) {
	if at.config.StrategyConfig == nil {
		return false, ""
	}
	minHoldMinutes := at.config.StrategyConfig.RiskControl.MinHoldMinutes
	if minHoldMinutes <= 0 {
		return false, ""
	}

	posKey := symbol + "_" + side
	openMs, exists := at.positionFirstSeenTime[posKey]
	if !exists || openMs <= 0 {
		return false, "" // age unknown — don't block
	}
	held := time.Since(time.UnixMilli(openMs))
	if held >= time.Duration(minHoldMinutes)*time.Minute {
		return false, ""
	}

	// Hard-exit bypass: stop-loss already hit.
	if sl := at.GetRecordedStopLoss(symbol, side); sl > 0 && markPrice > 0 {
		if (side == "long" && markPrice <= sl) || (side == "short" && markPrice >= sl) {
			return false, ""
		}
	}
	return true, fmt.Sprintf("min-hold lock: held %.1fmin < %dmin and stop-loss not hit", held.Minutes(), minHoldMinutes)
}

// ============================================================================
// Position-management executors (adjust SL / partial close)
// ============================================================================

// stopMoveTightens validates an AI stop-loss move: tighten-only, never widen,
// never cross the mark (audit-proof guard for the adjust_stop_loss action).
func stopMoveTightens(side string, currentSL, newSL, markPrice float64) bool {
	if currentSL <= 0 || newSL <= 0 || markPrice <= 0 {
		return false
	}
	if side == "long" {
		return newSL > currentSL && newSL < markPrice
	}
	return newSL < currentSL && newSL > markPrice
}

// stopMoveLocksProfit enforces the breakeven-or-better rule (user directive
// 09-16, NEARUSDT case): an AI tighten may only fire once it locks in
// breakeven or better — long newSL >= entry, short newSL <= entry. A
// "tighten" that still locks a loss just squeezes the escape room into
// short-timeframe noise: the NEAR stop parked under a 5m support cluster was
// swept by a single 5m wick (2.438, wick 2.432) fifteen minutes before price
// broke the very structure high the tighten claimed to keep room for, and the
// pre-tighten stop (2.4) was never touched. Cutting risk on a LOSING position
// is the close path (early-close gate), not a loss-locking stop.
func stopMoveLocksProfit(side string, entryPrice, newSL float64) bool {
	if entryPrice <= 0 || newSL <= 0 {
		return false
	}
	if side == "long" {
		return newSL >= entryPrice
	}
	return newSL <= entryPrice
}

// stopPriceForSide returns the stop trigger of the position-side's stop
// order from the open-order list (0 when none).
func stopPriceForSide(orders []types.OpenOrder, side string) float64 {
	want := strings.ToUpper(side)
	for _, o := range orders {
		if strings.Contains(strings.ToUpper(o.Type), "STOP") &&
			strings.ToUpper(o.PositionSide) == want && o.StopPrice > 0 {
			return o.StopPrice
		}
	}
	return 0
}

// exchangeStopPrice reads the position's current stop trigger from the
// exchange — the durable fallback for the in-memory recorded stop, which a
// restart wipes (pre-restart positions lose SetRecordedStopLoss state).
func (at *AutoTrader) exchangeStopPrice(symbol, side string) float64 {
	orders, err := at.trader.GetOpenOrders(symbol)
	if err != nil {
		return 0
	}
	return stopPriceForSide(orders, side)
}

// executeAdjustStopLossWithRecord moves one open position's exchange stop to
// the AI's new price — TIGHTEN-ONLY (guard enforced): structure-based profit
// protection the mechanical 1R/ATR ladders cannot express.
func (at *AutoTrader) executeAdjustStopLossWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	positions, err := at.trader.GetPositions()
	if err != nil {
		return err
	}
	var side string
	var markPrice, currentSL, entryPrice float64
	for _, pos := range positions {
		if pos["symbol"] != decision.Symbol {
			continue
		}
		pside, _ := pos["side"].(string)
		mp, _ := pos["markPrice"].(float64)
		ep, _ := pos["entryPrice"].(float64)
		sl := at.GetRecordedStopLoss(decision.Symbol, pside)
		if sl <= 0 {
			// Recorded stop lost (restart, pre-upgrade position): the
			// exchange's own stop order is the durable truth — seed it back.
			if sp := at.exchangeStopPrice(decision.Symbol, pside); sp > 0 {
				at.SetRecordedStopLoss(decision.Symbol, pside, sp)
				sl = sp
				logger.Infof("🔧 [%s] Seeded recorded SL for %s %s from exchange stop %.6g", at.name, decision.Symbol, pside, sp)
			}
		}
		if stopMoveTightens(pside, sl, decision.StopLoss, mp) {
			side, markPrice, currentSL, entryPrice = pside, mp, sl, ep
			break
		}
		// Remember the first position for an accurate error message.
		if side == "" {
			side, markPrice, currentSL, entryPrice = pside, mp, sl, ep
		}
	}
	if side == "" {
		// No position on the exchange — the normal AI-latency race, not an
		// error: the decision was made on the cycle-start context snapshot,
		// and during the 2-10min AI call the position legitimately closed
		// (SL/TP trigger, manual close). There is nothing left to protect,
		// so skip silently instead of firing the failed-execution alert
		// (user 09-14: 没有仓位就停止 stop loss 操作).
		logger.Infof("ℹ️ [%s] adjust_stop_loss skipped: %s has no open position (closed during the AI-call window — context snapshot was stale)", at.name, decision.Symbol)
		return nil
	}
	if !stopMoveTightens(side, currentSL, decision.StopLoss, markPrice) {
		return fmt.Errorf("❌ [RISK CONTROL] adjust_stop_loss %s rejected: new SL %.6g must TIGHTEN (current %.6g, mark %.6g) — never widen, never cross the mark",
			decision.Symbol, decision.StopLoss, currentSL, markPrice)
	}
	// Breakeven-or-better (user directive 09-16, NEARUSDT case): a tighten
	// that still locks a loss is rejected — with no profit there is nothing
	// to "protect", and parking the stop inside short-timeframe noise (below
	// a 5m support cluster) converts intact higher-TF theses into realized
	// losses on routine wicks. Risk reduction on a losing position belongs to
	// the close path (early-close gate), not a loss-locking stop.
	if entryPrice > 0 && !stopMoveLocksProfit(side, entryPrice, decision.StopLoss) {
		return fmt.Errorf("❌ [RISK CONTROL] adjust_stop_loss %s rejected: new SL %.6g still locks a LOSS (entry %.6g, current SL %.6g) — tighten is only allowed to breakeven or better; to cut risk on a losing position use close (early-close gate), never a short-timeframe noise stop",
			decision.Symbol, decision.StopLoss, entryPrice, currentSL)
	}
	if entryPrice <= 0 {
		logger.Infof("⚠️ [%s] adjust_stop_loss %s: entry price unavailable from exchange data — breakeven gate skipped (tighten-only still enforced)", at.name, decision.Symbol)
	}
	if err := at.moveStopExchange(decision.Symbol, side, decision.StopLoss); err != nil {
		return err
	}
	at.SetRecordedStopLoss(decision.Symbol, side, decision.StopLoss)
	logger.Infof("🔧 [%s] AI stop-loss adjusted: %s %s SL → %.6g (was %.6g, mark %.6g)", at.name, decision.Symbol, side, decision.StopLoss, currentSL, markPrice)
	notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🔧 止损调整 %s</b>\nSL %.6g → <code>%.6g</code>(AI 结构性收紧)", notify.Escape(decision.Symbol), currentSL, decision.StopLoss))
	return nil
}

// executePartialCloseWithRecord market-closes close_fraction of a position
// (user 2026-09-12: 分批止盈/减仓). Guards: cumulative partial ≤75% per
// position (the rest must run or exit via close_*); close gates (min-hold /
// early-close / breakout-hold) apply in applyHardRiskGates before this runs.
func (at *AutoTrader) executePartialCloseWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction, side string) error {
	if at.partialTrimmed == nil {
		at.partialTrimmed = make(map[string]float64)
	}
	positions, err := at.trader.GetPositions()
	if err != nil {
		return err
	}
	var qty float64
	for _, pos := range positions {
		if pos["symbol"] == decision.Symbol && pos["side"] == side {
			qty, _ = pos["positionAmt"].(float64)
			break
		}
	}
	if qty <= 0 {
		return fmt.Errorf("❌ %s has no open %s position", decision.Symbol, side)
	}
	posKey := decision.Symbol + "_" + side
	already := at.partialTrimmed[posKey]
	if already+decision.CloseFraction > 0.75 {
		return fmt.Errorf("❌ [RISK CONTROL] partial_close %s rejected: cumulative partial %.0f%% + %.0f%% would exceed 75%% — use close_* for a full exit",
			decision.Symbol, already*100, decision.CloseFraction*100)
	}
	trimQty := qty * decision.CloseFraction
	if _, err := at.reducePosition(decision.Symbol, side, trimQty); err != nil {
		return err
	}
	at.partialTrimmed[posKey] = already + decision.CloseFraction
	actionRecord.Quantity = trimQty
	logger.Infof("🎯 [%s] AI partial close: %s %s %.0f%% (%.6g) — cumulative %.0f%%", at.name, decision.Symbol, side, decision.CloseFraction*100, trimQty, (already+decision.CloseFraction)*100)
	notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🎯 部分平仓 %s</b>\n<i>%s 平 %.0f%%(累计 %.0f%%),剩余仓位继续持有</i>", notify.Escape(decision.Symbol), side, decision.CloseFraction*100, (already+decision.CloseFraction)*100))
	return nil
}

// ============================================================================
// Protection watchdog
// ============================================================================

// missingProtection inspects a position's open orders and reports which
// protective legs are absent. STOP* matches stop-loss algos, TAKE_PROFIT*
// matches TP algos; a resting LIMIT entry on the same symbol is noise here.
func missingProtection(orders []types.OpenOrder) (needSL, needTP bool) {
	needSL, needTP = true, true
	for _, o := range orders {
		t := strings.ToUpper(o.Type)
		if strings.Contains(t, "STOP") {
			needSL = false
		}
		if strings.Contains(t, "TAKE_PROFIT") {
			needTP = false
		}
	}
	return needSL, needTP
}

// exchangeStopPriceSeen — kept for symmetry with stopPriceForSide above.

// processProtectionWatchdog runs once per decision cycle over all open
// positions: any position whose SL/TP order is missing on the exchange gets
// it re-placed at the RECORDED plan price (user request 2026-09-12). Missing
// legs happen via manual cancels, exchange hiccups, or pre-upgrade positions.
// Guards: the TP is not re-placed once the TP-runner converted it into the
// trend-run (the trail owns the exit); wrong-side plan prices are logged
// instead of placed; every check fails open. The AI has no place-SL/TP
// action — without this watchdog a naked position stays naked.
func (at *AutoTrader) processProtectionWatchdog() {
	if at.config.StrategyConfig == nil {
		return
	}
	positions, err := at.trader.GetPositions()
	if err != nil {
		return
	}
	for _, pos := range positions {
		symbol, _ := pos["symbol"].(string)
		side, _ := pos["side"].(string)
		markPrice, _ := pos["markPrice"].(float64)
		if symbol == "" || side == "" || markPrice <= 0 {
			continue
		}
		orders, err := at.trader.GetOpenOrders(symbol)
		if err != nil {
			continue // fail-open
		}
		needSL, needTP := missingProtection(orders)
		// State healing: the orders exist but the in-memory records were
		// wiped by a restart — seed them from the exchange so trailing /
		// breakeven / min-hold bypass keep working for pre-restart positions.
		if !needSL && at.GetRecordedStopLoss(symbol, side) <= 0 {
			if sp := at.exchangeStopPrice(symbol, side); sp > 0 {
				at.SetRecordedStopLoss(symbol, side, sp)
				logger.Infof("🔧 [%s] Protection watchdog: seeded recorded SL %s %s = %.6g from exchange", at.name, symbol, side, sp)
			}
		}
		if !needTP && at.getOpenTakeProfit(symbol, side) <= 0 {
			for _, o := range orders {
				if strings.Contains(strings.ToUpper(o.Type), "TAKE_PROFIT") && o.StopPrice > 0 {
					at.recordOpenTakeProfit(symbol, side, o.StopPrice)
					logger.Infof("🔧 [%s] Protection watchdog: seeded recorded TP %s %s = %.6g from exchange", at.name, symbol, side, o.StopPrice)
					break
				}
			}
		}
		if !needSL && !needTP {
			continue
		}
		positionSide := strings.ToUpper(side) // LONG / SHORT
		posKey := symbol + "_" + side
		var repaired []string
		if needSL {
			if sl := at.GetRecordedStopLoss(symbol, side); sl > 0 {
				valid := (side == "long" && sl < markPrice) || (side == "short" && sl > markPrice)
				if valid {
					if err := at.trader.SetStopLoss(symbol, positionSide, 0, sl); err == nil {
						repaired = append(repaired, fmt.Sprintf("SL %.6g", sl))
					} else {
						logger.Infof("⚠️ [%s] Protection watchdog: SL re-place failed for %s: %v", at.name, symbol, err)
						at.alertUnprotectedPosition(symbol, side, fmt.Sprintf("SL re-place failed: %v", err))
					}
				} else {
					logger.Infof("⚠️ [%s] Protection watchdog: %s recorded SL %.6g is on the wrong side of mark %.6g — not placed, manual check", at.name, symbol, sl, markPrice)
					at.alertUnprotectedPosition(symbol, side, fmt.Sprintf("recorded SL %.6g is on the wrong side of mark %.6g", sl, markPrice))
				}
			} else {
				logger.Infof("⚠️ [%s] Protection watchdog: %s has no SL order and no recorded stop — cannot auto-repair", at.name, symbol)
				at.alertUnprotectedPosition(symbol, side, "no SL order on the exchange and no recorded stop to repair from")
			}
		}
		if needTP && !at.tpRunnerDone(posKey) {
			// After the TP-runner converts the fixed TP into the trend-run,
			// the trail owns the exit — re-placing a TP here would undo it.
			if tp := at.getOpenTakeProfit(symbol, side); tp > 0 {
				valid := (side == "long" && tp > markPrice) || (side == "short" && tp < markPrice)
				if valid {
					if err := at.trader.SetTakeProfit(symbol, positionSide, 0, tp); err == nil {
						repaired = append(repaired, fmt.Sprintf("TP %.6g", tp))
					} else {
						logger.Infof("⚠️ [%s] Protection watchdog: TP re-place failed for %s: %v", at.name, symbol, err)
					}
				} else {
					logger.Infof("⚠️ [%s] Protection watchdog: %s recorded TP %.6g is on the wrong side of mark %.6g — not placed, manual check", at.name, symbol, tp, markPrice)
				}
			} else {
				logger.Infof("⚠️ [%s] Protection watchdog: %s has no TP order and no recorded TP — cannot auto-repair", at.name, symbol)
			}
		}
		if len(repaired) > 0 {
			logger.Infof("🛡️ [%s] Protection watchdog repaired %s: %s (plan prices)", at.name, symbol, strings.Join(repaired, " + "))
			notify.Notify("ORDER", at.name, fmt.Sprintf("<b>🛡️ 保护单补挂 %s</b>\n<i>%s(按开仓计划价自动补挂)</i>", notify.Escape(symbol), strings.Join(repaired, " + ")))
		}
	}
}

// alertUnprotectedPosition pushes a Telegram alert when the watchdog finds a
// position with NO usable stop-loss protection (missing order, missing
// recorded price, wrong-side price, or a failed re-place) — the SL leg is the
// one leg that must never silently stay naked. Deduped like the other gate
// alerts (gateNotifyRecord) so a stuck position doesn't spam every cycle.
func (at *AutoTrader) alertUnprotectedPosition(symbol, side, reason string) {
	streak, push := at.gateNotifyRecord("unprotected-sl:"+symbol+":"+side, time.Now())
	if !push {
		return
	}
	notify.Notify("ALERT", at.name, fmt.Sprintf(
		"<b>🚨 仓位无止损保护 %s (%s)</b>\n%s\n\n<i>看门狗无法自动补挂,请人工核查该仓位并手动设置止损(streak %d)</i>",
		notify.Escape(symbol), strings.ToUpper(side[:1])+side[1:], notify.Escape(reason), streak))
}

// ============================================================================
// Recorded stop-loss state (drives the min-hold/early-close hard-exit bypass)
// ============================================================================

// SetRecordedStopLoss records the stop-loss price of a freshly opened position
// (used by the min-hold gate to allow hard-exit closes).
func (at *AutoTrader) SetRecordedStopLoss(symbol, side string, price float64) {
	at.positionStopLossMutex.Lock()
	defer at.positionStopLossMutex.Unlock()
	at.positionStopLoss[symbol+"_"+side] = price
}

// GetRecordedStopLoss returns the recorded stop-loss for a position.
func (at *AutoTrader) GetRecordedStopLoss(symbol, side string) float64 {
	at.positionStopLossMutex.RLock()
	defer at.positionStopLossMutex.RUnlock()
	return at.positionStopLoss[symbol+"_"+side]
}

// ClearRecordedStopLoss drops the recorded stop-loss after the position is closed.
func (at *AutoTrader) ClearRecordedStopLoss(symbol, side string) {
	at.positionStopLossMutex.Lock()
	defer at.positionStopLossMutex.Unlock()
	delete(at.positionStopLoss, symbol+"_"+side)
}

// regimeLineOf returns the SLOW EMA (regime line) of the timing timeframe —
// the structural line a dip/bounce entry must respect: longs on pullback
// need price ABOVE it (a dip that broke it is a candidate reversal), shorts
// on rally need price BELOW it (a bounce that broke it is a candidate
// reversal). Mirrors computeTFSignal's adaptive slow period; 0 when
// insufficient.
func regimeLineOf(data *market.Data, tf string) float64 {
	tfData, ok := data.TimeframeData[tf]
	if !ok || tfData == nil {
		return 0
	}
	dur := market.TimeframeDuration(tf)
	if dur <= 0 {
		return 0
	}
	kl := kernel.ClosedKlines(tfData, time.Now(), dur)
	kb := make([]market.Kline, len(kl))
	for i, b := range kl {
		kb[i] = market.Kline{OpenTime: b.Time, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume}
	}
	n := len(kb)
	if n < 12 {
		return 0
	}
	slowP := 50
	if n < 50 {
		slowP = 20
	}
	if n < 25 {
		slowP = 12
	}
	return market.ExportCalculateEMA(kb, slowP)
}

// finestSubHourTrend returns the finest sub-hour timeframe present in the
// data (15m preferred, else 30m) and its deterministic trend.
func finestSubHourTrend(data *market.Data) (string, string) {
	bestTF := ""
	bestDur := time.Duration(0)
	if data != nil {
		for tf := range data.TimeframeData {
			d := market.TimeframeDuration(tf)
			if d <= 0 || d > time.Hour {
				continue
			}
			if bestDur == 0 || d < bestDur {
				bestTF, bestDur = tf, d
			}
		}
	}
	if bestTF == "" {
		return "", ""
	}
	return bestTF, kernel.TimeframeTrend(data, bestTF)
}


// ============================================================================
// Hard Risk Gates (CODE ENFORCED, strategy risk_control driven)
// ============================================================================

// applyHardRiskGates enforces program-level gates the AI cannot override:
//  1. 1d-uptrend short block (risk_control.block_short_1d_uptrend)
//  2. Minimum holding period lock on AI-initiated closes (min_hold_minutes)
//  3. Early-close lock (early_close_min_hours, 1h reversal evidence)
//  4. Loss-streak circuit breaker + entry timing gate + regime-line guard
//  5. Stock weekend open block + breakout-hold close gate
//
// Blocked decisions are dropped with a logged reason, mirroring preTradeRuleCheck.
func (at *AutoTrader) applyHardRiskGates(decisions []kernel.Decision, ctx *kernel.Context) []kernel.Decision {
	if at.config.StrategyConfig == nil || len(decisions) == 0 {
		return decisions
	}
	rc := at.config.StrategyConfig.RiskControl
	if rc.MinHoldMinutes <= 0 && rc.EarlyCloseMinHours >= 0 && !rc.BlockShort1dUptrend && !rc.EntryTimingGate && rc.CloseRejectBreakoutPct <= 0 && !rc.LossStreakBanEnabled && rc.AccountMaxDrawdownPct <= 0 {
		return decisions
	}
	filtered := make([]kernel.Decision, 0, len(decisions))
	for _, d := range decisions {
		// Account-level circuit breaker (user directive, prompt 09-13): once
		// equity has retraced ≥ account_max_drawdown_pct from the initial
		// balance, ALL new opens are blocked — reduce-only mode. The prompt
		// promises this as CODE ENFORCED; this branch is that enforcement
		// (it was prompt-only until now). Closes/SL/TP are never blocked —
		// the account must be able to de-risk.
		if strings.HasPrefix(d.Action, "open_") && rc.AccountMaxDrawdownPct > 0 && at.initialBalance > 0 {
			equity := ctx.Account.TotalEquity
			if equity > 0 {
				drawdownPct := (at.initialBalance - equity) / at.initialBalance * 100
				if drawdownPct >= rc.AccountMaxDrawdownPct {
					streak, push := at.gateNotifyRecord("acctdd:"+d.Symbol, time.Now())
					logger.Warnf("🛑 [%s] GATE BLOCKED %s %s: account drawdown %.2f%% ≥ %.1f%% (equity %.2f vs initial %.2f) — reduce-only mode (streak %d)",
						at.name, d.Action, d.Symbol, drawdownPct, rc.AccountMaxDrawdownPct, equity, at.initialBalance, streak)
					if push {
						notify.Notify("ALERT", at.name, fmt.Sprintf(
							"<b>🛑 账户级熔断 — 只减仓模式</b>\n净值回撤 <code>%.2f%%</code> ≥ %.1f%%(equity %.2f / initial %.2f)\n一切新开仓被程序拦截,直至回撤修复\n\n<i>%s</i>",
							drawdownPct, rc.AccountMaxDrawdownPct, equity, at.initialBalance, notify.Escape(d.Reasoning)))
					}
					continue
				}
			}
		}
		// Stock weekend block: Binance tokenized stocks (bstock) trade on
		// weekends but the US market doesn't — volatility and edge are poor
		// (user 2026-09-11). nil/true = block; closes/SL/TP unaffected.
		if strings.HasPrefix(d.Action, "open_") {
			if noOpen := rc.StockWeekendNoOpen; noOpen == nil || *noOpen {
				if bt, ok := at.trader.(interface {
					IsStockSymbol(symbol string) bool
				}); ok && bt.IsStockSymbol(d.Symbol) && binance.IsUSMarketWeekend(time.Now()) {
					logger.Warnf("🛡️ [%s] GATE BLOCKED %s %s: bstock weekend — US market closed, no new stock positions", at.name, d.Action, d.Symbol)
					continue
				}
			}
		}
		// Loss-streak circuit breaker (opens only).
		if strings.HasPrefix(d.Action, "open_") && rc.LossStreakBanEnabled {
			maxLosses := rc.LossStreakMaxLosses
			if maxLosses <= 0 {
				maxLosses = lossStreakDefaultN
			}
			if blocked, reason := at.lossStreakBlocks(d.Symbol, maxLosses); blocked {
				streak, push := at.gateNotifyRecord("lossstreak:"+d.Symbol, time.Now())
				logger.Warnf("🛡️ [%s] GATE BLOCKED %s %s (streak %d): %s", at.name, d.Action, d.Symbol, streak, reason)
				if push {
					notify.Notify("ALERT", at.name, fmt.Sprintf(
						"<b>🛡️ 连亏熔断 %s</b>\n%s\n\n<i>%s</i>",
						notify.Escape(d.Symbol), reason, notify.Escape(d.Reasoning)))
				}
				continue
			}
		}
		if rc.EntryTimingGate {
			if tf, trend := finestSubHourTrend(ctx.MarketDataMap[d.Symbol]); tf != "" {
				isLongAction := strings.HasPrefix(d.Action, "open_long")
				isShortAction := strings.HasPrefix(d.Action, "open_short")
				// Symmetric policy (audit 09-13): longs enter on
				// up|pullback, blocked in down|rally|range contexts;
				// shorts enter on down|rally (sell the downtrend bounce),
				// blocked in up|pullback|range.
				bad := (isLongAction && (trend == "down" || trend == "rally" || trend == "range")) ||
					(isShortAction && (trend == "up" || trend == "pullback" || trend == "range"))
				if bad {
					streak, push := at.gateNotifyRecord("timing:"+d.Symbol+":"+d.Action, time.Now())
					logger.Warnf("🛡️ [%s] GATE BLOCKED %s %s: %s trend is %s (entry timing gate, streak %d)",
						at.name, d.Action, d.Symbol, tf, trend, streak)
					if push {
						why := map[bool]string{true: "做多", false: "做空"}[strings.HasPrefix(d.Action, "open_long")]
						notify.Notify("ALERT", at.name, fmt.Sprintf(
							"<b>🛡️ 已拦截 %s %s</b>\n\n原因:%s 周期趋势为 <code>%s</code>,入场时点不佳\n\n<i>做多需 up/pullback,做空需 down/rally(反弹做空);range 无动能不放行</i>",
							why, notify.Escape(d.Symbol), tf, trend))
					}
					continue
				}
				// Regime-line guard for dip/bounce entries (audit 09-13
				// #3): pullback/rally windows are only valid while the
				// slow EMA (regime line) holds — price beyond it means
				// the pullback/bounce may be a REVERSAL, not an entry.
				if trend == "pullback" || trend == "rally" {
					if md := ctx.MarketDataMap[d.Symbol]; md != nil {
						line := regimeLineOf(md, tf)
						px := md.CurrentPrice
						if line > 0 && px > 0 {
							broken := (isLongAction && px < line) || (isShortAction && px > line)
							if broken {
								streak, push := at.gateNotifyRecord("regimeline:"+d.Symbol+":"+d.Action, time.Now())
								logger.Warnf("🛡️ [%s] GATE BLOCKED %s %s: %s regime line %.6g broken by %.6g — possible reversal, not an entry window (streak %d)",
									at.name, d.Action, d.Symbol, tf, line, px, streak)
								if push {
									notify.Notify("ALERT", at.name, fmt.Sprintf(
										"<b>🛡️ 已拦截 %s %s</b>\n%s 回踩/反弹入场,但 %s 慢线(regime line)<code>%.6g</code> 已被 <code>%.6g</code> 跌/升破——可能是趋势反转而非入场窗\n\n<i>%s</i>",
										notify.Escape(d.Symbol), map[bool]string{true: "做多", false: "做空"}[isLongAction], tf, tf, line, px, notify.Escape(d.Reasoning)))
								}
								continue
							}
						}
					}
				}
			}
		}
		switch d.Action {
		case "open_short":
			if rc.BlockShort1dUptrend {
				trend := kernel.TimeframeTrend(ctx.MarketDataMap[d.Symbol], "1d")
				if trend == "up" {
					logger.Warnf("🛡️ [%s] GATE BLOCKED open_short %s: 1d trend is up (counter-trend short protection)", at.name, d.Symbol)
					notify.Notify("ALERT", at.name, fmt.Sprintf("<b>🛡️ 已拦截做空 %s</b>\n1d 趋势向上，禁止逆势做空\n<i>%s</i>", notify.Escape(d.Symbol), notify.Escape(d.Reasoning)))
					continue
				}
			}
		case "close_long", "close_short", "partial_close_long", "partial_close_short":
			side := "long"
			if d.Action == "close_short" || d.Action == "partial_close_short" {
				side = "short"
			}
			var markPrice, entryPrice float64
			for _, p := range ctx.Positions {
				if p.Symbol == d.Symbol && p.Side == side {
					markPrice = p.MarkPrice
					entryPrice = p.EntryPrice
					break
				}
			}
			// Resistance-breakout hold: a losing exit into a nearby
			// opposite-side level (with the 15m structure intact) gets
			// blocked — the breakout/breakdown keeps its room.
			if rc.CloseRejectBreakoutPct > 0 && markPrice > 0 && entryPrice > 0 {
				view := buildCloseGateView(ctx.MarketDataMap[d.Symbol], side, markPrice)
				if blocked, reason := closeRejectBreakoutBlocks(side, entryPrice, view, rc.CloseRejectBreakoutPct); blocked {
					streak, push := at.gateNotifyRecord("reshold:"+d.Symbol+":"+d.Action, time.Now())
					logger.Warnf("🛡️ [%s] GATE BLOCKED %s %s (streak %d): %s", at.name, d.Action, d.Symbol, streak, reason)
					if push {
						notify.Notify("ALERT", at.name, fmt.Sprintf(
							"<b>🛡️ 已拦截平仓 %s</b>\n%s\n\n<i>%s</i>",
							notify.Escape(d.Symbol), reason, notify.Escape(d.Reasoning)))
					}
					continue
				}
			}
			if blocked, reason := at.minHoldBlocksClose(d.Symbol, side, markPrice); blocked {
				logger.Warnf("🛡️ [%s] GATE BLOCKED %s %s: %s", at.name, d.Action, d.Symbol, reason)
				continue
			}
			// Early-close lock: before EarlyCloseMinHours (default 4h) an AI
			// close needs ≥2 closed 1h candles against the position —
			// SL/TP fills and drawdown-protect never pass through here.
			if blocked, reason := at.earlyCloseBlocksClose(d.Symbol, side, markPrice, ctx.MarketDataMap[d.Symbol]); blocked {
				logger.Warnf("🛡️ [%s] GATE BLOCKED %s %s: %s", at.name, d.Action, d.Symbol, reason)
				continue
			}
		}
		filtered = append(filtered, d)
	}
	return filtered
}
