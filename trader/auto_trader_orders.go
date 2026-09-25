package trader

import (
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	notify "nofx/telegram/notify"
	"nofx/trader/types"
	"strings"
	"time"
)

// executeDecisionWithRecord executes AI decision and records detailed information
func (at *AutoTrader) executeDecisionWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	switch decision.Action {
	case "open_long":
		decision, err := at.marketExceptionGate(decision, actionRecord, true)
		if err != nil {
			return err
		}
		if decision.Action == "open_long_limit" {
			// Degraded: no program evidence for a market open but a live
			// anchor exists — take the default limit path instead.
			return at.executeOpenLimitLongWithRecord(decision, actionRecord)
		}
		at.dropPendingEntry(decision.Symbol) // market entry supersedes any pending limit
		return at.executeOpenLongWithRecord(decision, actionRecord)
	case "open_short":
		decision, err := at.marketExceptionGate(decision, actionRecord, false)
		if err != nil {
			return err
		}
		if decision.Action == "open_short_limit" {
			return at.executeOpenLimitShortWithRecord(decision, actionRecord)
		}
		at.dropPendingEntry(decision.Symbol)
		return at.executeOpenShortWithRecord(decision, actionRecord)
	case "open_long_limit":
		return at.executeOpenLimitLongWithRecord(decision, actionRecord)
	case "open_short_limit":
		return at.executeOpenLimitShortWithRecord(decision, actionRecord)
	case "close_long":
		return at.executeCloseLongWithRecord(decision, actionRecord)
	case "close_short":
		return at.executeCloseShortWithRecord(decision, actionRecord)
	case "adjust_stop_loss":
		return at.executeAdjustStopLossWithRecord(decision, actionRecord)
	case "partial_close_long":
		return at.executePartialCloseWithRecord(decision, actionRecord, "long")
	case "partial_close_short":
		return at.executePartialCloseWithRecord(decision, actionRecord, "short")
	case "hold", "wait":
		// No execution needed, just record
		return nil
	default:
		return fmt.Errorf("unknown action: %s", decision.Action)
	}
}

// stampEntryPath tags the entry context (timing-TF trend) for path
// attribution stats — rally-window shorts vs down-trend shorts vs future
// bb_ride market entries must be separately measurable (user 09-13 #2).
func (at *AutoTrader) stampEntryPath(record *store.DecisionAction, data *market.Data) {
	tf, trend := finestSubHourTrend(data)
	if tf == "" {
		return
	}
	record.EntryPath = tf + ":" + trend
}

// marketExceptionGate enforces the market-order exception with program teeth
// (B1, QUANT_REVIEW 2026-09-22). In limit-entry mode a market open is the
// EXCEPTION path, and until now the exception existed only as prompt prose —
// executeOpenLong/Short had no evidence check, so any market decision that
// survived the timing gates went straight to the exchange.
//
// The kernel prices the exception per cycle (DirectionGate.MarketException —
// bb_ride for longs / short_ride for shorts, or a confirmed breakout with
// volume+OI and |directional_score| ≥ MarketExceptionMinScore) and ships it
// in GateState. Enforcement here:
//   - evidence present AND decision confidence ≥ MarketExceptionMinScore
//     (the prompt's bb_ride clause, now code) → market open as decided;
//   - no evidence, live anchor → decision is REWRITTEN to the anchor limit
//     order — the default path the strategy contract always promised;
//   - no evidence, anchor suppressed → rejected fail-closed (mirrors
//     LIMIT_ANCHOR_SUPPRESSED: the direction is unexecutable).
//
// Deliberately NOT gated here: the crossed-anchor market fallback
// (auto_trader_pending.go) — it converts an ALREADY-PLACED limit entry whose
// anchor the price ran through, a documented market path that never passes
// through this dispatch. Market-default strategies (limit_entry_enabled=
// false) are exempt: market IS their default path.
func (at *AutoTrader) marketExceptionGate(d *kernel.Decision, record *store.DecisionAction, isLong bool) (*kernel.Decision, error) {
	if at.config.StrategyConfig == nil {
		return d, nil
	}
	rc := at.config.StrategyConfig.RiskControl
	if !rc.LimitEntryEnabled {
		return d, nil
	}
	gs := at.cycleGateStates[market.Normalize(d.Symbol)]
	if gs == nil {
		return d, fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: no hard-gate state this cycle — market open unverifiable (fail-closed)", d.Action, d.Symbol)
	}
	exception := gs.LongMarketException
	anchorLive := gs.LongLimitAllowed
	anchor := gs.LongEntryPrice
	if !isLong {
		exception = gs.ShortMarketException
		anchorLive = gs.ShortLimitAllowed
		anchor = gs.ShortEntryPrice
	}
	if exception && d.Confidence >= kernel.MarketExceptionMinScore {
		return d, nil
	}
	if anchorLive && anchor > 0 {
		limitAction := "open_long_limit"
		if !isLong {
			limitAction = "open_short_limit"
		}
		streak, push := at.gateNotifyRecord("mktgate:"+d.Symbol, time.Now())
		logger.Warnf("🛡️ [%s] MARKET→LIMIT %s %s: no market-exception evidence (or confidence %d < %d) — degraded to anchor limit %.6g (streak %d)",
			at.name, d.Action, d.Symbol, d.Confidence, kernel.MarketExceptionMinScore, anchor, streak)
		if push {
			notify.Notify("ALERT", at.name, fmt.Sprintf(
				"<b>🛡️ 市价降级限价 %s</b>\n%s 无市价例外证据(或 confidence %d < %d),已按锚点 <code>%.6g</code> 改挂限价\n\n<i>%s</i>",
				notify.Escape(d.Symbol), d.Action, d.Confidence, kernel.MarketExceptionMinScore, anchor, notify.Escape(d.Reasoning)))
		}
		d.Action = limitAction
		d.Price = anchor
		d.MarketDegraded = true
		return d, nil
	}
	return d, fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: no market-exception evidence and no live anchor — direction unexecutable this cycle", d.Action, d.Symbol)
}

// executeOpenLongWithRecord executes open long position and records detailed information
func (at *AutoTrader) executeOpenLongWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  📈 Open long: %s", decision.Symbol)

	if marketData, err := market.GetWithExchange(decision.Symbol, at.exchange); err == nil {
		at.stampEntryPath(actionRecord, marketData)
	}

	// Spread gate: thin books eat the entry edge (market fills pay the spread).
	if blocked, reason := at.spreadBlocksOpen(decision.Symbol); blocked {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: %s", decision.Action, decision.Symbol, reason)
	}

	// ⚠️ Get current positions for multiple checks
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}

	// [CODE ENFORCED] Check max positions limit. Resting limit entries on
	// OTHER symbols occupy slots too (same accounting as the limit path) —
	// counting only open positions let a market open push open+pending past
	// the cap when the limit path had already been refused.
	if err := at.enforceMaxPositions(nextSlotCount(len(positions), at.snapshotPendingSymbols(), decision.Symbol)); err != nil {
		return err
	}

	// Check if there's already a position in the same symbol and direction
	for _, pos := range positions {
		if pos["symbol"] == decision.Symbol && pos["side"] == "long" {
			return fmt.Errorf("❌ %s already has long position, close it first", decision.Symbol)
		}
	}

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return err
	}

	// Get balance (needed for multiple checks)
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("failed to get account balance: %w", err)
	}
	availableBalance := 0.0
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Get equity for position value ratio check
	equity := 0.0
	if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
		equity = eq
	} else if eq, ok := balance["totalWalletBalance"].(float64); ok && eq > 0 {
		equity = eq
	} else {
		equity = availableBalance // Fallback to available balance
	}

	// [CODE ENFORCED] Entry risk gates: mandatory SL, SL/TP side sanity,
	// min RR (dual-anchor), stop-distance window [ATR floor, wide cap].
	floorATR, capATR := at.stopBandATRs(decision.Symbol, marketData)
	if err := at.validateOpenRisk(decision, marketData.CurrentPrice, floorATR, capATR); err != nil {
		return err
	}

	// [CODE ENFORCED] Position Value Ratio Check: position_value <= equity × ratio
	adjustedPositionSize, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
	if wasCapped {
		decision.PositionSizeUSD = adjustedPositionSize
	}

	// [CODE ENFORCED] Risk-based sizing: position value derives from the stop
	// distance (equity × risk% ÷ dist%), clamped down when the AI oversizes.
	decision.PositionSizeUSD = at.clampSizeToRisk(decision, decision.PositionSizeUSD, equity, marketData.CurrentPrice)

	// ⚠️ Auto-adjust position size if insufficient margin
	// Formula: totalRequired = positionSize/leverage + positionSize*0.001 + positionSize/leverage*0.01
	//        = positionSize * (1.01/leverage + 0.001)
	marginFactor := 1.01/float64(decision.Leverage) + 0.001
	maxAffordablePositionSize := availableBalance / marginFactor

	actualPositionSize := decision.PositionSizeUSD
	if actualPositionSize > maxAffordablePositionSize {
		// Use 98% of max to leave buffer for price fluctuation
		adjustedSize := maxAffordablePositionSize * 0.98
		logger.Infof("  ⚠️ Position size %.2f exceeds max affordable %.2f, auto-reducing to %.2f",
			actualPositionSize, maxAffordablePositionSize, adjustedSize)
		actualPositionSize = adjustedSize
		decision.PositionSizeUSD = actualPositionSize
	}

	// [CODE ENFORCED] Minimum position size check
	if err := at.enforceMinPositionSize(decision.PositionSizeUSD); err != nil {
		return err
	}

	// Margin-budget gate: (used + new) margin ≤ max_margin_usage × equity —
	// the prompt states the budget, this enforces it (audit 09-13 #2).
	if blocked, reason := at.marginBudgetBlocksOpen(decision.Symbol, decision.PositionSizeUSD, float64(decision.Leverage), equity); blocked {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: %s", decision.Action, decision.Symbol, reason)
	}
	// Calculate quantity with adjusted position size
	quantity := actualPositionSize / marketData.CurrentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = marketData.CurrentPrice

	// Set margin mode
	if err := at.trader.SetMarginMode(decision.Symbol, at.config.IsCrossMargin); err != nil {
		logger.Infof("  ⚠️ Failed to set margin mode: %v", err)
		// Continue execution, doesn't affect trading
	}

	// Open position
	order, err := at.trader.OpenLong(decision.Symbol, quantity, decision.Leverage)
	if err != nil {
		return err
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	logger.Infof("  ✓ Position opened successfully, order ID: %v, quantity: %.4f", order["orderId"], quantity)

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "open_long", quantity, marketData.CurrentPrice, decision.Leverage, 0)

	fillPrice := orderFloat(order, "avgPrice")
	// Reanchor BEFORE recording (2026-09-25 P2): the recorded stop and the
	// write-once 1R anchor must describe the REAL opening risk at the actual
	// fill, not the pre-slippage plan.
	reanchorProtectivePrices(decision, marketData.CurrentPrice, fillPrice)

	// Record position opening time and stop-loss (drives the min-hold gate)
	posKey := decision.Symbol + "_long"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()
	at.SetRecordedStopLoss(decision.Symbol, "long", decision.StopLoss)
	at.SetInitialStopLoss(decision.Symbol, "long", decision.StopLoss) // 1R anchor — write-once, immune to later tighten
	// Peak PnL is per-position state — a re-opened symbol must not inherit
	// the previous trade's peak (stale peaks poison the drawdown monitors).
	at.ClearPeakPnLCache(decision.Symbol, "long")

	at.markAIManaged(decision.Symbol, "long")
	at.placeProtectiveOrders(decision, "LONG", quantity, marketData.CurrentPrice, fillPrice)
	at.reportFillSlippageRR(decision, marketData.CurrentPrice, fillPrice)
	return nil
}

// reportFillSlippageRR (B2-附, QUANT_REVIEW 09-22, observability only): a
// market open validated RR at the pre-trade ticker, but fills at avgPrice —
// in a fast market the fill can be the far side of the move (the
// crossed-anchor fallback is exactly such a moment). Compute realized RR at
// the actual fill and alert when it lands below min_rr; SL/TP have already
// re-anchored to the fill, so the trade remains correctly protected — this
// makes the slippage cost VISIBLE instead of silently absorbed.
func (at *AutoTrader) reportFillSlippageRR(decision *kernel.Decision, checkedPrice, fillPrice float64) {
	if fillPrice <= 0 || fillPrice == checkedPrice || decision.StopLoss <= 0 || decision.TakeProfit <= 0 {
		return
	}
	minRR := 1.5
	if at.config.StrategyConfig != nil {
		if v := at.config.StrategyConfig.RiskControl.MinRiskRewardRatio; v > 0 {
			minRR = v
		}
	}
	risk := math.Abs(fillPrice - decision.StopLoss)
	reward := math.Abs(decision.TakeProfit - fillPrice)
	if risk <= 0 {
		return
	}
	rr := reward / risk
	if rr < minRR {
		slippageBps := (fillPrice - checkedPrice) / checkedPrice * 10000
		if slippageBps < 0 {
			slippageBps = -slippageBps
		}
		logger.Warnf("⚠️ [%s] %s %s filled @ %.6g (checked @ %.6g, %.0fbps away): realized RR %.2f < min_rr %.2f — setup degraded by the fill; protection re-anchored at fill",
			at.name, decision.Action, decision.Symbol, fillPrice, checkedPrice, slippageBps, rr, minRR)
		notify.Notify("ALERT", at.name, fmt.Sprintf(
			"<b>⚠️ 市价成交 RR 劣化 %s</b>\n校验价 %.6g → 成交价 <code>%.6g</code>(%.0fbps),成交 RR <code>%.2f</code> &lt; min_rr %.2f\n保护单已按成交价重新锚定,请人工复核",
			notify.Escape(decision.Symbol), checkedPrice, fillPrice, slippageBps, rr, minRR))
	}
}

// executeOpenShortWithRecord executes open short position and records detailed information
func (at *AutoTrader) executeOpenShortWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  📉 Open short: %s", decision.Symbol)

	if marketData, err := market.GetWithExchange(decision.Symbol, at.exchange); err == nil {
		at.stampEntryPath(actionRecord, marketData)
	}

	// Spread gate: thin books eat the entry edge (market fills pay the spread).
	if blocked, reason := at.spreadBlocksOpen(decision.Symbol); blocked {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: %s", decision.Action, decision.Symbol, reason)
	}

	// ⚠️ Get current positions for multiple checks
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}

	// [CODE ENFORCED] Check max positions limit. Resting limit entries on
	// OTHER symbols occupy slots too (same accounting as the limit path) —
	// counting only open positions let a market open push open+pending past
	// the cap when the limit path had already been refused.
	if err := at.enforceMaxPositions(nextSlotCount(len(positions), at.snapshotPendingSymbols(), decision.Symbol)); err != nil {
		return err
	}

	// Check if there's already a position in the same symbol and direction
	for _, pos := range positions {
		if pos["symbol"] == decision.Symbol && pos["side"] == "short" {
			return fmt.Errorf("❌ %s already has short position, close it first", decision.Symbol)
		}
	}

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return err
	}

	// Get balance (needed for multiple checks)
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("failed to get account balance: %w", err)
	}
	availableBalance := 0.0
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Get equity for position value ratio check
	equity := 0.0
	if eq, ok := balance["totalEquity"].(float64); ok && eq > 0 {
		equity = eq
	} else if eq, ok := balance["totalWalletBalance"].(float64); ok && eq > 0 {
		equity = eq
	} else {
		equity = availableBalance // Fallback to available balance
	}

	// [CODE ENFORCED] Entry risk gates: mandatory SL, SL/TP side sanity,
	// min RR (dual-anchor), stop-distance window [ATR floor, wide cap].
	floorATR, capATR := at.stopBandATRs(decision.Symbol, marketData)
	if err := at.validateOpenRisk(decision, marketData.CurrentPrice, floorATR, capATR); err != nil {
		return err
	}

	// [CODE ENFORCED] Position Value Ratio Check: position_value <= equity × ratio
	adjustedPositionSize, wasCapped := at.enforcePositionValueRatio(decision.PositionSizeUSD, equity, decision.Symbol)
	if wasCapped {
		decision.PositionSizeUSD = adjustedPositionSize
	}

	// [CODE ENFORCED] Risk-based sizing: position value derives from the stop
	// distance (equity × risk% ÷ dist%), clamped down when the AI oversizes.
	decision.PositionSizeUSD = at.clampSizeToRisk(decision, decision.PositionSizeUSD, equity, marketData.CurrentPrice)

	// ⚠️ Auto-adjust position size if insufficient margin
	// Formula: totalRequired = positionSize/leverage + positionSize*0.001 + positionSize/leverage*0.01
	//        = positionSize * (1.01/leverage + 0.001)
	marginFactor := 1.01/float64(decision.Leverage) + 0.001
	maxAffordablePositionSize := availableBalance / marginFactor

	actualPositionSize := decision.PositionSizeUSD
	if actualPositionSize > maxAffordablePositionSize {
		// Use 98% of max to leave buffer for price fluctuation
		adjustedSize := maxAffordablePositionSize * 0.98
		logger.Infof("  ⚠️ Position size %.2f exceeds max affordable %.2f, auto-reducing to %.2f",
			actualPositionSize, maxAffordablePositionSize, adjustedSize)
		actualPositionSize = adjustedSize
		decision.PositionSizeUSD = actualPositionSize
	}

	// [CODE ENFORCED] Minimum position size check
	if err := at.enforceMinPositionSize(decision.PositionSizeUSD); err != nil {
		return err
	}

	// Margin-budget gate: (used + new) margin ≤ max_margin_usage × equity —
	// the prompt states the budget, this enforces it (audit 09-13 #2).
	if blocked, reason := at.marginBudgetBlocksOpen(decision.Symbol, decision.PositionSizeUSD, float64(decision.Leverage), equity); blocked {
		return fmt.Errorf("❌ [RISK CONTROL] %s %s rejected: %s", decision.Action, decision.Symbol, reason)
	}
	// Calculate quantity with adjusted position size
	quantity := actualPositionSize / marketData.CurrentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = marketData.CurrentPrice

	// Set margin mode
	if err := at.trader.SetMarginMode(decision.Symbol, at.config.IsCrossMargin); err != nil {
		logger.Infof("  ⚠️ Failed to set margin mode: %v", err)
		// Continue execution, doesn't affect trading
	}

	// Open position
	order, err := at.trader.OpenShort(decision.Symbol, quantity, decision.Leverage)
	if err != nil {
		return err
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	logger.Infof("  ✓ Position opened successfully, order ID: %v, quantity: %.4f", order["orderId"], quantity)

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "open_short", quantity, marketData.CurrentPrice, decision.Leverage, 0)

	// Record position opening time and stop-loss (drives the min-hold gate)
	posKey := decision.Symbol + "_short"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()
	fillPrice := orderFloat(order, "avgPrice")
	if fillPrice <= 0 {
		if orderID, ok := order["orderId"].(int64); ok {
			if avg, confirmed := at.confirmedFillPrice(decision.Symbol, fmt.Sprint(orderID), marketData.CurrentPrice); confirmed {
				fillPrice = avg
			}
		}
	}
	reanchorProtectivePrices(decision, marketData.CurrentPrice, fillPrice)

	at.SetRecordedStopLoss(decision.Symbol, "short", decision.StopLoss)
	at.SetInitialStopLoss(decision.Symbol, "short", decision.StopLoss) // 1R anchor — write-once, immune to later tighten
	// Peak PnL is per-position state — see the open_long note above.
	at.ClearPeakPnLCache(decision.Symbol, "short")

	at.markAIManaged(decision.Symbol, "short")
	at.placeProtectiveOrders(decision, "SHORT", quantity, marketData.CurrentPrice, fillPrice)
	at.reportFillSlippageRR(decision, marketData.CurrentPrice, fillPrice)
	return nil
}

// confirmedFillPrice polls the order status for the ACTUAL average fill
// price (R3, 2026-09-26 review): Binance's OpenLong/OpenShort response map
// carries only orderId/status — avgPrice is always 0 there, so the old code
// silently skipped the slippage reanchor (fillPrice==refPrice ⇒ no-op).
// Bounded polling (5×400ms); ok=false → caller falls back to the checked
// price with a warning.
func (at *AutoTrader) confirmedFillPrice(symbol, orderID string, checkedPrice float64) (float64, bool) {
	if orderID == "" {
		return checkedPrice, false
	}
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(400 * time.Millisecond)
		}
		status, err := at.trader.GetOrderStatus(symbol, orderID)
		if err != nil {
			continue
		}
		if st, _ := status["status"].(string); strings.ToUpper(st) == "NEW" {
			continue
		}
		if avg := orderFloat(status, "avgPrice"); avg > 0 {
			return avg, true
		}
	}
	return checkedPrice, false
}

// orderFloat reads a float64 field from an exchange order response map.
func orderFloat(m map[string]interface{}, key string) float64 {
	if m == nil {
		return 0
	}
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

// placeProtectiveOrders places the exchange-side stop-loss/take-profit algo
// orders (closePosition mode on Binance) right after a successful open, so the
// AI's planned SL/TP are enforced by the exchange instead of only living in
// the decision log. The SL/TP are RE-ANCHORED to the actual fill price (the
// RR gate validated distances at a pre-fill ticker — a market order can fill
// away from it, silently compressing the take-profit distance). Zero prices
// are skipped; placement failures raise an alert because the position would
// otherwise run unprotected. quantity is used by exchange implementations that
// place qty-sized trigger orders.
func (at *AutoTrader) placeProtectiveOrders(decision *kernel.Decision, positionSide string, quantity float64, refPrice, fillPrice float64) error {
	// NOTE: callers pass FINAL (fill-reanchored) SL/TP — the reanchor used to
	// live here, but it ran AFTER the recorded stop/1R-anchor were written,
	// leaving memory on the pre-slippage plan while the exchange got the
	// shifted one (2026-09-25 P2). Market paths reanchor explicitly before
	// recording; pending/partial paths pre-anchor in protectExecutedSlice.
	// R2 (2026-09-26 review): returns the SL-leg error so callers can gate
	// their state advancement on ACTUAL success.
	var slErr error
	if decision.StopLoss > 0 {
		if err := at.trader.SetStopLoss(decision.Symbol, positionSide, quantity, decision.StopLoss); err != nil {
			slErr = err
			logger.Infof("  ⚠ Failed to set stop loss for %s: %v", decision.Symbol, err)
			notify.Notify("ALERT", at.name, fmt.Sprintf("<b>⚠️ %s 止损单设置失败</b>\n<code>%s</code>\n该仓位当前没有交易所止损保护，请人工关注！", notify.Escape(decision.Symbol), notify.Escape(err.Error())))
		}
	} else {
		logger.Infof("  ⚠ AI decision for %s has no stop_loss, exchange stop not placed", decision.Symbol)
	}
	if decision.TakeProfit > 0 {
		// Split TP (09-21 user experiment): the algo closes only
		// tp_close_fraction of the position at the structure level; the
		// remainder stays on as a trend-runner under the trailing stop.
		// Collapsed to a full close when trailing is disabled — a runner
		// without a ratchet just gives the move back.
		//
		// tpQty keeps the legacy contract (full quantity — adapters size
		// their TP order by it) whenever the split is off.
		tpQty := quantity
		if frac := at.effectiveTPCloseFraction(); frac < 1.0 {
			tpQty = quantity * frac
			// The remainder is now a runner: the protection watchdog must
			// NOT re-place a full TP over it (the same flag also marks the
			// legacy TP-runner conversion). ClearPeakPnLCache already ran
			// upstream, so this mark survives the open-path reset.
			at.markTPRunner(decision.Symbol + "_" + strings.ToLower(positionSide))
			logger.Infof("  🏃 %s split TP: algo closes %.0f%% (%.6g) at %.6g — remainder trails", decision.Symbol, frac*100, tpQty, decision.TakeProfit)
		}
		if err := at.trader.SetTakeProfit(decision.Symbol, positionSide, tpQty, decision.TakeProfit); err != nil {
			logger.Infof("  ⚠ Failed to set take profit for %s: %v", decision.Symbol, err)
		}
		side := strings.ToLower(positionSide)
		at.recordOpenTakeProfit(decision.Symbol, side, decision.TakeProfit)
	}

	// MANDATORY post-open verification (09-22 lesson): placement APIs can
	// succeed while the leg never rests (the -1106 reduceOnly rejection left
	// every split-TP position SL-only behind a single log line). Re-query the
	// exchange and confirm each intended leg is actually there — escalate to
	// ALERT when it is not, so a missing leg can never again be silent.
	at.verifyProtectiveLegs(decision, positionSide, decision.StopLoss > 0, decision.TakeProfit > 0)
	return slErr
}

// missingLegsReport returns "" when every INTENDED leg (wantSL/wantTP) is
// resting on the exchange; otherwise names what is missing. Pure — the
// retry/notify wrapper around it is verifyProtectiveLegs.
func missingLegsReport(orders []types.OpenOrder, positionSide string, wantSL, wantTP bool) string {
	needSL, needTP := missingProtection(orders, positionSide)
	var missing []string
	if wantSL && needSL {
		missing = append(missing, "SL")
	}
	if wantTP && needTP {
		missing = append(missing, "TP")
	}
	return strings.Join(missing, "+")
}

// verifyProtectiveLegs re-queries the exchange open orders right after the
// placement pass. One retry after a short delay rides out read-after-write
// lag without masking a real rejection; a still-missing leg is an ALERT —
// the position is running unprotected or without its profit leg.
func (at *AutoTrader) verifyProtectiveLegs(decision *kernel.Decision, positionSide string, wantSL, wantTP bool) {
	if !wantSL && !wantTP {
		return // no leg was intended — nothing to verify
	}
	report := ""
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		orders, err := at.trader.GetOpenOrders(decision.Symbol)
		if err != nil {
			report = fmt.Sprintf("open-orders query failed: %v", err)
			continue
		}
		report = missingLegsReport(orders, positionSide, wantSL, wantTP)
		if report == "" {
			logger.Infof("  ✅ [%s] protective legs verified on exchange: %s %s", at.name, decision.Symbol, positionSide)
			return
		}
	}
	logger.Infof("  🚨 [%s] protective leg verification FAILED: %s %s missing %s", at.name, decision.Symbol, positionSide, report)
	notify.Notify("ALERT", at.name, fmt.Sprintf(
		"<b>🚨 %s %s 保护单核验失败</b>\n<i>开仓后交易所挂单核验(已重试): <b>%s</b> 腿缺失——仓位可能在无止损/无止盈状态运行,请立即人工核查!</i>",
		notify.Escape(decision.Symbol), positionSide, report))
}

// effectiveTPCloseFraction resolves the split-TP fraction for this trader:
// trailing disabled → 1.0 (full close, no runner), otherwise the configured
// kernel.TPCloseFraction (0 = default 0.5, negative = legacy 1.0).
func (at *AutoTrader) effectiveTPCloseFraction() float64 {
	if at.config.StrategyConfig == nil || !at.config.StrategyConfig.RiskControl.TrailingStopEnabled {
		return 1.0
	}
	return kernel.TPCloseFraction(&at.config.StrategyConfig.RiskControl)
}

// executeCloseLongWithRecord executes close long position and records detailed information
func (at *AutoTrader) executeCloseLongWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  🔄 Close long: %s", decision.Symbol)

	// Hands-off rule (user directive 2026-09-25): the AI does not manage
	// manually opened positions — closing them is automation acting on them.
	if !at.isAIManaged(decision.Symbol, "long") {{
		return fmt.Errorf("❌ [HANDS-OFF] %s was not opened by the AI — manual positions are never closed/adjusted by the program (close the position manually or take it over via a decision to open it)", decision.Symbol)
	}}

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return err
	}
	actionRecord.Price = marketData.CurrentPrice

	// Normalize symbol for database lookup
	normalizedSymbol := market.Normalize(decision.Symbol)

	// Get entry price and quantity - prioritize local database for accurate quantity
	var entryPrice float64
	var quantity float64

	// First try to get from local database (more accurate for quantity)
	if at.store != nil {
		if openPos, err := at.store.Position().GetOpenPositionBySymbol(at.id, normalizedSymbol, "LONG"); err == nil && openPos != nil {
			quantity = openPos.Quantity
			entryPrice = openPos.EntryPrice
			logger.Infof("  📊 Using local position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
		}
	}

	// Fallback to exchange API if local data not found
	if quantity == 0 {
		positions, err := at.trader.GetPositions()
		if err == nil {
			for _, pos := range positions {
				if pos["symbol"] == decision.Symbol && pos["side"] == "long" {
					if ep, ok := pos["entryPrice"].(float64); ok {
						entryPrice = ep
					}
					if amt, ok := pos["positionAmt"].(float64); ok && amt > 0 {
						quantity = amt
					}
					break
				}
			}
		}
		logger.Infof("  📊 Using exchange position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
	}

	// Close position
	order, err := at.trader.CloseLong(decision.Symbol, 0) // 0 = close all
	if err != nil {
		return err
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "close_long", quantity, marketData.CurrentPrice, 0, entryPrice)

	at.ClearRecordedStopLoss(decision.Symbol, "long")
	at.ClearInitialStopLoss(decision.Symbol, "long")
	at.ClearPeakPnLCache(decision.Symbol, "long")
	at.unmarkAIManaged(decision.Symbol, "long") // lifecycle complete — registry clean
	logger.Infof("  ✓ Position closed successfully")
	return nil
}

// executeCloseShortWithRecord executes close short position and records detailed information
func (at *AutoTrader) executeCloseShortWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  🔄 Close short: %s", decision.Symbol)

	// Hands-off rule (user directive 2026-09-25): the AI does not manage
	// manually opened positions — closing them is automation acting on them.
	if !at.isAIManaged(decision.Symbol, "short") {{
		return fmt.Errorf("❌ [HANDS-OFF] %s was not opened by the AI — manual positions are never closed/adjusted by the program (close the position manually or take it over via a decision to open it)", decision.Symbol)
	}}

	// Get current price
	marketData, err := market.GetWithExchange(decision.Symbol, at.exchange)
	if err != nil {
		return err
	}
	actionRecord.Price = marketData.CurrentPrice

	// Normalize symbol for database lookup
	normalizedSymbol := market.Normalize(decision.Symbol)

	// Get entry price and quantity - prioritize local database for accurate quantity
	var entryPrice float64
	var quantity float64

	// First try to get from local database (more accurate for quantity)
	if at.store != nil {
		if openPos, err := at.store.Position().GetOpenPositionBySymbol(at.id, normalizedSymbol, "SHORT"); err == nil && openPos != nil {
			quantity = openPos.Quantity
			entryPrice = openPos.EntryPrice
			logger.Infof("  📊 Using local position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
		}
	}

	// Fallback to exchange API if local data not found
	if quantity == 0 {
		positions, err := at.trader.GetPositions()
		if err == nil {
			for _, pos := range positions {
				if pos["symbol"] == decision.Symbol && pos["side"] == "short" {
					if ep, ok := pos["entryPrice"].(float64); ok {
						entryPrice = ep
					}
					if amt, ok := pos["positionAmt"].(float64); ok {
						quantity = -amt // positionAmt is negative for short
					}
					break
				}
			}
		}
		logger.Infof("  📊 Using exchange position data: qty=%.8f, entry=%.2f", quantity, entryPrice)
	}

	// Close position
	order, err := at.trader.CloseShort(decision.Symbol, 0) // 0 = close all
	if err != nil {
		return err
	}

	// Record order ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	// Record order to database and poll for confirmation
	at.recordAndConfirmOrder(order, decision.Symbol, "close_short", quantity, marketData.CurrentPrice, 0, entryPrice)

	at.ClearRecordedStopLoss(decision.Symbol, "short")
	at.ClearInitialStopLoss(decision.Symbol, "short")
	at.ClearPeakPnLCache(decision.Symbol, "short")
	at.unmarkAIManaged(decision.Symbol, "short") // lifecycle complete — registry clean
	logger.Infof("  ✓ Position closed successfully")
	return nil
}
