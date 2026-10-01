package trader

import (
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/logger"
	"strings"
	"time"
)

// ============================================================================
// Grid Order Placement and Management
// ============================================================================

// checkTotalPositionLimit checks if adding a new position would exceed total limits
// Returns: (allowed bool, currentPositionValue float64, maxAllowed float64)
func (at *AutoTrader) checkTotalPositionLimit(symbol string, additionalValue float64) (bool, float64, float64) {
	gridConfig := at.config.StrategyConfig.GridConfig

	// Calculate max allowed total position value
	// Total position should not exceed: TotalInvestment * Leverage
	maxTotalPositionValue := gridConfig.TotalInvestment * float64(gridConfig.Leverage)

	// Get current position value from exchange
	currentPositionValue := 0.0
	positions, err := at.trader.GetPositions()
	if err == nil {
		for _, pos := range positions {
			if sym, ok := pos["symbol"].(string); ok && sym == symbol {
				if size, ok := pos["positionAmt"].(float64); ok {
					if price, ok := pos["markPrice"].(float64); ok {
						currentPositionValue = math.Abs(size) * price
					} else if entryPrice, ok := pos["entryPrice"].(float64); ok {
						currentPositionValue = math.Abs(size) * entryPrice
					}
				}
			}
		}
	}

	// Also count pending orders as potential position
	at.gridState.mu.RLock()
	pendingValue := 0.0
	for _, level := range at.gridState.Levels {
		if level.State == "pending" {
			pendingValue += level.OrderQuantity * level.Price
		}
	}
	at.gridState.mu.RUnlock()

	totalAfterOrder := currentPositionValue + pendingValue + additionalValue
	allowed := totalAfterOrder <= maxTotalPositionValue

	return allowed, currentPositionValue + pendingValue, maxTotalPositionValue
}

// gridOrderFilled queries ONE order's terminal status (F13, 2026-10-01
// review). known=false → the adapter could not tell — the level stays
// pending for the next sync instead of being guessed.
func (at *AutoTrader) gridOrderFilled(orderID string) (filled, known bool) {
	gridConfig := at.config.StrategyConfig.GridConfig
	if gridConfig == nil || orderID == "" {
		return false, false
	}
	status, err := at.trader.GetOrderStatus(gridConfig.Symbol, orderID)
	if err != nil {
		return false, false
	}
	st, _ := status["status"].(string)
	switch strings.ToUpper(st) {
	case "FILLED":
		return true, true
	case "CANCELED", "EXPIRED", "REJECTED":
		return false, true
	}
	return false, false
}

// placeGridLimitOrder places a limit order for grid trading
func (at *AutoTrader) placeGridLimitOrder(d *kernel.Decision, side string) error {
	// F12 (2026-10-01 review): REFUSE instead of falling back to the
	// GridTraderAdapter — that adapter fabricated "entries" out of stop/TP
	// protection orders with synthetic exchange order IDs. Grid requires a
	// native GridTrader (binance/bybit/okx/hyperliquid/aster/bitget);
	// kucoin/gate/indodax must not run grid strategies.
	gridTrader, ok := at.trader.(GridTrader)
	if !ok {
		return fmt.Errorf("grid limit orders unsupported on %s: no native GridTrader implementation — grid strategy cannot run on this exchange", at.exchange)
	}

	gridConfig := at.config.StrategyConfig.GridConfig

	// CRITICAL: Validate and cap quantity to prevent excessive position sizes
	// This protects against AI miscalculations or leverage misconfigurations
	quantity := d.Quantity
	if d.Price > 0 && gridConfig.TotalInvestment > 0 {
		// Calculate max allowed position value per grid level
		// Each level gets proportional share of total investment
		maxMarginPerLevel := gridConfig.TotalInvestment / float64(gridConfig.GridCount)
		maxPositionValuePerLevel := maxMarginPerLevel * float64(gridConfig.Leverage)
		maxQuantityPerLevel := maxPositionValuePerLevel / d.Price

		// Also get the level's allocated USD for additional validation
		at.gridState.mu.RLock()
		var levelAllocatedUSD float64
		if d.LevelIndex >= 0 && d.LevelIndex < len(at.gridState.Levels) {
			levelAllocatedUSD = at.gridState.Levels[d.LevelIndex].AllocatedUSD
		}
		at.gridState.mu.RUnlock()

		// Use level-specific allocation if available
		if levelAllocatedUSD > 0 {
			levelMaxPositionValue := levelAllocatedUSD * float64(gridConfig.Leverage)
			levelMaxQuantity := levelMaxPositionValue / d.Price
			if levelMaxQuantity < maxQuantityPerLevel {
				maxQuantityPerLevel = levelMaxQuantity
			}
		}

		// Cap quantity if it exceeds the maximum allowed
		if quantity > maxQuantityPerLevel {
			logger.Warnf("[Grid] Quantity %.4f exceeds max allowed %.4f (position_value $%.2f > max $%.2f), capping",
				quantity, maxQuantityPerLevel, quantity*d.Price, maxPositionValuePerLevel)
			quantity = maxQuantityPerLevel
		}

		// Safety check: ensure position value is reasonable (within 2x of intended max as absolute limit)
		positionValue := quantity * d.Price
		absoluteMaxValue := gridConfig.TotalInvestment * float64(gridConfig.Leverage) * 2 // 2x safety margin
		if positionValue > absoluteMaxValue {
			logger.Errorf("[Grid] CRITICAL: Position value $%.2f exceeds absolute max $%.2f! Rejecting order.",
				positionValue, absoluteMaxValue)
			return fmt.Errorf("position value $%.2f exceeds safety limit $%.2f", positionValue, absoluteMaxValue)
		}
	}

	// CRITICAL: Check total position limit before placing order
	orderValue := quantity * d.Price
	allowed, currentValue, maxValue := at.checkTotalPositionLimit(d.Symbol, orderValue)
	if !allowed {
		logger.Errorf("[Grid] TOTAL POSITION LIMIT EXCEEDED: current=$%.2f + order=$%.2f > max=$%.2f. Rejecting order.",
			currentValue, orderValue, maxValue)
		return fmt.Errorf("total position value $%.2f would exceed limit $%.2f", currentValue+orderValue, maxValue)
	}

	req := &LimitOrderRequest{
		Symbol:     d.Symbol,
		Side:       side,
		Price:      d.Price,
		Quantity:   quantity, // Use validated/capped quantity
		Leverage:   gridConfig.Leverage,
		PostOnly:   gridConfig.UseMakerOnly,
		ReduceOnly: false,
		ClientID:   fmt.Sprintf("grid-%d-%d", d.LevelIndex, time.Now().UnixNano()%1000000),
	}

	result, err := gridTrader.PlaceLimitOrder(req)
	if err != nil {
		return fmt.Errorf("failed to place limit order: %w", err)
	}

	// Update grid level state
	at.gridState.mu.Lock()
	if d.LevelIndex >= 0 && d.LevelIndex < len(at.gridState.Levels) {
		at.gridState.Levels[d.LevelIndex].State = "pending"
		at.gridState.Levels[d.LevelIndex].OrderID = result.OrderID
		// F13 (2026-10-01 review): record the VALIDATED/capped quantity the
		// exchange actually received — the raw AI quantity diverged whenever
		// the cap fired and corrupted the fill inference baseline.
		at.gridState.Levels[d.LevelIndex].OrderQuantity = quantity
		at.gridState.OrderBook[result.OrderID] = d.LevelIndex
	}
	at.gridState.mu.Unlock()

	logger.Infof("[Grid] Placed %s limit order at $%.2f, qty=%.4f, level=%d, orderID=%s",
		side, d.Price, quantity, d.LevelIndex, result.OrderID)

	return nil
}

// cancelGridOrder cancels a specific grid order
func (at *AutoTrader) cancelGridOrder(d *kernel.Decision) error {
	gridTrader, ok := at.trader.(GridTrader)
	if !ok {
		gridTrader = NewGridTraderAdapter(at.trader)
	}

	if err := gridTrader.CancelOrder(d.Symbol, d.OrderID); err != nil {
		return fmt.Errorf("failed to cancel order: %w", err)
	}

	// Update state
	at.gridState.mu.Lock()
	if levelIdx, ok := at.gridState.OrderBook[d.OrderID]; ok {
		if levelIdx >= 0 && levelIdx < len(at.gridState.Levels) {
			at.gridState.Levels[levelIdx].State = "empty"
			at.gridState.Levels[levelIdx].OrderID = ""
			at.gridState.Levels[levelIdx].OrderQuantity = 0
		}
		delete(at.gridState.OrderBook, d.OrderID)
	}
	at.gridState.mu.Unlock()

	logger.Infof("[Grid] Cancelled order: %s", d.OrderID)
	return nil
}

// cancelAllGridOrders cancels all grid orders
func (at *AutoTrader) cancelAllGridOrders() error {
	gridConfig := at.config.StrategyConfig.GridConfig

	if err := at.trader.CancelAllOrders(gridConfig.Symbol); err != nil {
		return fmt.Errorf("failed to cancel all orders: %w", err)
	}

	// Reset all pending levels
	at.gridState.mu.Lock()
	for i := range at.gridState.Levels {
		if at.gridState.Levels[i].State == "pending" {
			at.gridState.Levels[i].State = "empty"
			at.gridState.Levels[i].OrderID = ""
			at.gridState.Levels[i].OrderQuantity = 0
		}
	}
	at.gridState.OrderBook = make(map[string]int)
	at.gridState.mu.Unlock()

	logger.Infof("[Grid] Cancelled all orders")
	return nil
}

// pauseGrid pauses grid trading
func (at *AutoTrader) pauseGrid(reason string) error {
	at.cancelAllGridOrders()

	at.gridState.mu.Lock()
	at.gridState.IsPaused = true
	at.gridState.mu.Unlock()

	logger.Infof("[Grid] Paused: %s", reason)
	return nil
}

// resumeGrid resumes grid trading
func (at *AutoTrader) resumeGrid() error {
	at.gridState.mu.Lock()
	at.gridState.IsPaused = false
	at.gridState.mu.Unlock()

	logger.Infof("[Grid] Resumed")
	return nil
}

// adjustGrid adjusts grid parameters
func (at *AutoTrader) adjustGrid(d *kernel.Decision) error {
	// Cancel existing orders first
	at.cancelAllGridOrders()

	gridConfig := at.config.StrategyConfig.GridConfig

	// Get current price
	price, err := at.trader.GetMarketPrice(gridConfig.Symbol)
	if err != nil {
		return fmt.Errorf("failed to get market price: %w", err)
	}

	// Reinitialize grid levels
	at.initializeGridLevels(price, gridConfig)

	logger.Infof("[Grid] Adjusted grid bounds around price $%.2f", price)
	return nil
}

// syncGridState syncs grid state with exchange
func (at *AutoTrader) syncGridState() {
	gridConfig := at.config.StrategyConfig.GridConfig

	// Get open orders from exchange
	openOrders, err := at.trader.GetOpenOrders(gridConfig.Symbol)
	if err != nil {
		logger.Warnf("[Grid] Failed to get open orders: %v", err)
		return
	}

	// Build set of active order IDs
	activeOrderIDs := make(map[string]bool)
	for _, order := range openOrders {
		activeOrderIDs[order.OrderID] = true
	}

	// Get current positions to verify fills. F13 (2026-10-01 review): a
	// FAILED position query no longer feeds 0 into the fill inference —
	// unknown must stay unknown (levels keep their pending state for the
	// next round) instead of marking vanished orders cancelled while they
	// may have filled.
	positions, err := at.trader.GetPositions()
	currentPositionSize := 0.0
	positionsKnown := true
	if err != nil {
		positionsKnown = false
		logger.Warnf("[Grid] Failed to get positions for state sync: %v — fill inference SKIPPED this round (levels stay pending, unknown ≠ cancelled)", err)
	} else {
		for _, pos := range positions {
			if sym, ok := pos["symbol"].(string); ok && sym == gridConfig.Symbol {
				if size, ok := pos["positionAmt"].(float64); ok {
					currentPositionSize = size
				}
			}
		}
	}

	// Update levels based on order status
	at.gridState.mu.Lock()
	expectedPositionSize := 0.0
	for _, level := range at.gridState.Levels {
		if level.State == "filled" {
			expectedPositionSize += level.PositionSize
		}
	}

	for i := range at.gridState.Levels {
		level := &at.gridState.Levels[i]
		if level.State == "pending" && level.OrderID != "" {
			if activeOrderIDs[level.OrderID] {
				continue
			}
			// Order no longer exists — classify via its OWN terminal status
			// first (F13: the shared position-size heuristic marked a
			// cancelled + a filled order in the SAME round both filled,
			// double-counting the ledger).
			filled, known := at.gridOrderFilled(level.OrderID)
			switch {
			case known && filled:
				level.State = "filled"
				level.PositionEntry = level.Price
				level.PositionSize = level.OrderQuantity
				at.gridState.TotalTrades++
				expectedPositionSize += level.PositionSize
				logger.Infof("[Grid] Level %d order filled at $%.2f", i, level.Price)
			case known:
				level.State = "empty"
				level.OrderID = ""
				level.OrderQuantity = 0
				logger.Infof("[Grid] Level %d order cancelled/expired", i)
			case positionsKnown && math.Abs(currentPositionSize) > math.Abs(expectedPositionSize):
				// Terminal status unavailable (adapter without per-order
				// query) — fall back to the position delta, updated per
				// order so multiple resolutions in one round stay ordered.
				level.State = "filled"
				level.PositionEntry = level.Price
				level.PositionSize = level.OrderQuantity
				at.gridState.TotalTrades++
				expectedPositionSize += level.PositionSize
				logger.Infof("[Grid] Level %d order filled at $%.2f (position-delta inference)", i, level.Price)
			case positionsKnown:
				level.State = "empty"
				level.OrderID = ""
				level.OrderQuantity = 0
				logger.Infof("[Grid] Level %d order cancelled/expired", i)
			default:
				// Nothing verifiable this round: keep the level pending —
				// unknown is retried, never guessed.
				logger.Infof("[Grid] Level %d order %s vanished but state unknown (position query failed) — kept pending for next sync", i, level.OrderID)
				continue
			}
			delete(at.gridState.OrderBook, level.OrderID)
		}
	}
	at.gridState.mu.Unlock()

	logger.Debugf("[Grid] Synced state: position=%.4f, orders=%d", currentPositionSize, len(openOrders))

	// Check stop loss
	at.checkAndExecuteStopLoss()

	// Check grid skew
	at.autoAdjustGrid()
}

// closeAllPositions closes all open positions for the grid symbol
func (at *AutoTrader) closeAllPositions() error {
	gridConfig := at.config.StrategyConfig.GridConfig
	if gridConfig == nil {
		return nil
	}

	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("failed to get positions: %w", err)
	}

	for _, pos := range positions {
		symbol, _ := pos["symbol"].(string)
		if symbol != gridConfig.Symbol {
			continue
		}

		size, _ := pos["positionAmt"].(float64)
		if size == 0 {
			continue
		}

		if size > 0 {
			_, err = at.trader.CloseLong(symbol, size)
		} else {
			_, err = at.trader.CloseShort(symbol, -size)
		}
		if err != nil {
			logger.Infof("Failed to close position: %v", err)
		}
	}

	return nil
}

// checkAndExecuteStopLoss checks if any filled level has exceeded stop loss and closes it
func (at *AutoTrader) checkAndExecuteStopLoss() {
	gridConfig := at.config.StrategyConfig.GridConfig
	if gridConfig.StopLossPct <= 0 {
		return // Stop loss not configured
	}

	currentPrice, err := at.trader.GetMarketPrice(gridConfig.Symbol)
	if err != nil {
		logger.Warnf("[Grid] Failed to get market price for stop loss check: %v", err)
		return
	}

	at.gridState.mu.Lock()
	defer at.gridState.mu.Unlock()

	for i := range at.gridState.Levels {
		level := &at.gridState.Levels[i]
		if level.State != "filled" || level.PositionEntry <= 0 {
			continue
		}

		// Calculate loss percentage
		var lossPct float64
		if level.Side == "buy" {
			// Long position: loss when price drops
			lossPct = (level.PositionEntry - currentPrice) / level.PositionEntry * 100
		} else {
			// Short position: loss when price rises
			lossPct = (currentPrice - level.PositionEntry) / level.PositionEntry * 100
		}

		// Check if stop loss triggered
		if lossPct >= gridConfig.StopLossPct {
			logger.Warnf("[Grid] STOP LOSS TRIGGERED: Level %d, entry=$%.2f, current=$%.2f, loss=%.2f%%",
				i, level.PositionEntry, currentPrice, lossPct)

			// Close the position
			var closeErr error
			if level.Side == "buy" {
				_, closeErr = at.trader.CloseLong(gridConfig.Symbol, level.PositionSize)
			} else {
				_, closeErr = at.trader.CloseShort(gridConfig.Symbol, level.PositionSize)
			}

			if closeErr != nil {
				logger.Errorf("[Grid] Failed to execute stop loss for level %d: %v", i, closeErr)
			} else {
				level.State = "stopped"
				realizedLoss := -lossPct * level.AllocatedUSD / 100
				level.UnrealizedPnL = realizedLoss
				at.gridState.TotalTrades++
				// Update daily PnL tracking (lock already held, update directly)
				at.gridState.DailyPnL += realizedLoss
				at.gridState.TotalProfit += realizedLoss
				logger.Infof("[Grid] Stop loss executed: Level %d closed at $%.2f (loss %.2f%%)",
					i, currentPrice, lossPct)
			}
		}
	}
}
