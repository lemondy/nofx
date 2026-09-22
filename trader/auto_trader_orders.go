package trader

import (
	"fmt"
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
		at.dropPendingEntry(decision.Symbol) // market entry supersedes any pending limit
		return at.executeOpenLongWithRecord(decision, actionRecord)
	case "open_short":
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
	if err := at.validateOpenRisk(decision, marketData.CurrentPrice, oneHourATRPct(marketData), fourHourATRPct(marketData)); err != nil {
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

	// Record position opening time and stop-loss (drives the min-hold gate)
	posKey := decision.Symbol + "_long"
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()
	at.SetRecordedStopLoss(decision.Symbol, "long", decision.StopLoss)
	at.SetInitialStopLoss(decision.Symbol, "long", decision.StopLoss) // 1R anchor — write-once, immune to later tighten
	// Peak PnL is per-position state — a re-opened symbol must not inherit
	// the previous trade's peak (stale peaks poison the drawdown monitors).
	at.ClearPeakPnLCache(decision.Symbol, "long")

	fillPrice := orderFloat(order, "avgPrice")
	at.placeProtectiveOrders(decision, "LONG", quantity, marketData.CurrentPrice, fillPrice)
	return nil
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
	if err := at.validateOpenRisk(decision, marketData.CurrentPrice, oneHourATRPct(marketData), fourHourATRPct(marketData)); err != nil {
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
	at.SetRecordedStopLoss(decision.Symbol, "short", decision.StopLoss)
	at.SetInitialStopLoss(decision.Symbol, "short", decision.StopLoss) // 1R anchor — write-once, immune to later tighten
	// Peak PnL is per-position state — see the open_long note above.
	at.ClearPeakPnLCache(decision.Symbol, "short")

	fillPrice := orderFloat(order, "avgPrice")
	at.placeProtectiveOrders(decision, "SHORT", quantity, marketData.CurrentPrice, fillPrice)
	return nil
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
func (at *AutoTrader) placeProtectiveOrders(decision *kernel.Decision, positionSide string, quantity float64, refPrice, fillPrice float64) {
	reanchorProtectivePrices(decision, refPrice, fillPrice)
	if decision.StopLoss > 0 {
		if err := at.trader.SetStopLoss(decision.Symbol, positionSide, quantity, decision.StopLoss); err != nil {
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
}

// missingLegsReport returns "" when every INTENDED leg (wantSL/wantTP) is
// resting on the exchange; otherwise names what is missing. Pure — the
// retry/notify wrapper around it is verifyProtectiveLegs.
func missingLegsReport(orders []types.OpenOrder, wantSL, wantTP bool) string {
	needSL, needTP := missingProtection(orders)
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
		report = missingLegsReport(orders, wantSL, wantTP)
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
	logger.Infof("  ✓ Position closed successfully")
	return nil
}

// executeCloseShortWithRecord executes close short position and records detailed information
func (at *AutoTrader) executeCloseShortWithRecord(decision *kernel.Decision, actionRecord *store.DecisionAction) error {
	logger.Infof("  🔄 Close short: %s", decision.Symbol)

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
	logger.Infof("  ✓ Position closed successfully")
	return nil
}
