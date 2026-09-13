package trader

import (
	"fmt"
	"nofx/kernel"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	notify "nofx/telegram/notify"
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

	// [CODE ENFORCED] Check max positions limit
	if err := at.enforceMaxPositions(len(positions)); err != nil {
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

	// [CODE ENFORCED] Check max positions limit
	if err := at.enforceMaxPositions(len(positions)); err != nil {
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
		if err := at.trader.SetTakeProfit(decision.Symbol, positionSide, quantity, decision.TakeProfit); err != nil {
			logger.Infof("  ⚠ Failed to set take profit for %s: %v", decision.Symbol, err)
		}
		side := strings.ToLower(positionSide)
		at.recordOpenTakeProfit(decision.Symbol, side, decision.TakeProfit)
	}
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
	at.ClearPeakPnLCache(decision.Symbol, "short")
	logger.Infof("  ✓ Position closed successfully")
	return nil
}
