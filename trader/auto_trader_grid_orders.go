package trader

import (
	"errors"
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
	if cache, ok := at.trader.(interface{ InvalidateAccountCache() }); ok {
		cache.InvalidateAccountCache()
	}
	currentPositionValue := 0.0
	positions, err := at.trader.GetPositions()
	if err != nil {
		return false, 0, maxTotalPositionValue
	}
	for _, pos := range positions {
		sym, ok := pos["symbol"].(string)
		if !ok {
			return false, math.Inf(1), maxTotalPositionValue
		}
		if sym != symbol {
			continue
		}
		size, ok := pos["positionAmt"].(float64)
		if !ok || math.IsNaN(size) || math.IsInf(size, 0) {
			return false, math.Inf(1), maxTotalPositionValue
		}
		if size == 0 {
			continue
		}
		price, ok := pos["markPrice"].(float64)
		if !ok || price <= 0 {
			price, ok = pos["entryPrice"].(float64)
		}
		if !ok || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return false, math.Inf(1), maxTotalPositionValue
		}
		currentPositionValue += math.Abs(size) * price
	}

	// Include every trader's resting entry on the same account and symbol.
	pendingValue := 0.0
	for key, pe := range at.accountPendingEntries() {
		if key == "unknown" {
			return false, math.Inf(1), maxTotalPositionValue
		}
		if pe != nil && pe.Symbol == symbol {
			pendingValue += math.Max(0, pe.Quantity-pe.ProtectedQty) * pe.Price
		}
	}

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
	if d.Symbol != gridConfig.Symbol || d.Price <= 0 || d.Quantity <= 0 || d.LevelIndex < 0 || d.LevelIndex >= len(at.gridState.Levels) {
		return fmt.Errorf("invalid grid entry")
	}
	at.gridState.mu.RLock()
	level := at.gridState.Levels[d.LevelIndex]
	paused := at.gridState.IsPaused
	at.gridState.mu.RUnlock()
	if paused || level.State == "pending" || level.PositionSize > 0 || level.ExitOrderID != "" {
		return fmt.Errorf("grid level unavailable for new entry")
	}
	if err := at.persistGridLedger(); err != nil {
		return err
	}

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

	balance, err := at.trader.GetBalance()
	if err != nil || gridAccountEquity(balance) <= 0 {
		return fmt.Errorf("grid account equity unknown: %v", err)
	}
	positionSide := "long"
	if strings.EqualFold(side, "SELL") {
		positionSide = "short"
	}
	if blocked, reason := at.marginBudgetBlocksOpen(d.Symbol, positionSide, orderValue, float64(gridConfig.Leverage), gridAccountEquity(balance)); blocked {
		return fmt.Errorf("grid %s", reason)
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
		at.gridState.Levels[d.LevelIndex].Side = strings.ToLower(side)
		at.gridState.Levels[d.LevelIndex].OrderID = result.OrderID
		// F13 (2026-10-01 review): record the VALIDATED/capped quantity the
		// exchange actually received — the raw AI quantity diverged whenever
		// the cap fired and corrupted the fill inference baseline.
		at.gridState.Levels[d.LevelIndex].OrderQuantity = result.Quantity
		if result.Quantity <= 0 {
			at.gridState.Levels[d.LevelIndex].OrderQuantity = quantity
		}
		at.gridState.Levels[d.LevelIndex].ExecutedQuantity = 0
		if result.Price > 0 {
			at.gridState.Levels[d.LevelIndex].Price = result.Price
		}
		at.gridState.OrderBook[result.OrderID] = d.LevelIndex
	}
	at.gridState.mu.Unlock()

	if err := at.persistGridLedger(); err != nil {
		return fmt.Errorf("grid entry %s placed but recovery ledger failed: %w", result.OrderID, err)
	}
	logger.Infof("[Grid] Placed %s limit order at $%.2f, qty=%.4f, level=%d, orderID=%s",
		side, d.Price, quantity, d.LevelIndex, result.OrderID)

	return nil
}

// cancelGridOrder cancels a specific grid order
func (at *AutoTrader) cancelGridOrder(d *kernel.Decision) error {
	if at.gridState == nil {
		return fmt.Errorf("grid state missing")
	}
	at.gridState.mu.RLock()
	owned := false
	for _, level := range at.gridState.Levels {
		if level.OrderID == d.OrderID && level.State == "pending" && d.Symbol == at.config.StrategyConfig.GridConfig.Symbol {
			owned = true
		}
	}
	at.gridState.mu.RUnlock()
	if !owned {
		return fmt.Errorf("grid order %s is not owned by this trader", d.OrderID)
	}
	canceler, ok := at.trader.(interface{ CancelOrder(string, string) error })
	if !ok {
		return fmt.Errorf("native grid cancellation unsupported")
	}
	cancelErr := canceler.CancelOrder(d.Symbol, d.OrderID)
	at.syncGridState()
	at.gridState.mu.RLock()
	defer at.gridState.mu.RUnlock()
	for _, level := range at.gridState.Levels {
		if level.OrderID == d.OrderID && level.State == "pending" {
			return errors.Join(cancelErr, fmt.Errorf("grid order %s cancellation/final fill unconfirmed", d.OrderID))
		}
	}
	// A final exchange receipt is authoritative even if a duplicate cancel was rejected.
	return nil
}

// Cancel only owned entry IDs; do not cancel another trader's protection/orders.
func (at *AutoTrader) cancelAllGridOrders() error {
	if at.gridState == nil {
		return nil
	}
	symbol := at.config.StrategyConfig.GridConfig.Symbol
	at.gridState.mu.RLock()
	ids := []string{}
	for _, level := range at.gridState.Levels {
		if level.State == "pending" && level.OrderID != "" {
			ids = append(ids, level.OrderID)
		}
	}
	at.gridState.mu.RUnlock()
	var failures []error
	for _, id := range ids {
		if err := at.cancelGridOrder(&kernel.Decision{Symbol: symbol, OrderID: id}); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// pauseGrid pauses grid trading
func (at *AutoTrader) pauseGrid(reason string) error {
	err := at.cancelAllGridOrders()
	err = errors.Join(err, at.protectGridResidue())
	at.gridState.mu.Lock()
	if !at.gridState.IsPaused {
		at.gridState.PauseReason = reason
	}
	if err != nil {
		at.gridState.PauseReason = "unresolved pause: " + reason
	}
	at.gridState.IsPaused = true
	at.gridState.mu.Unlock()

	logger.Infof("[Grid] Paused: %s", reason)
	return errors.Join(err, at.persistGridLedger())
}

// resumeGrid resumes grid trading
func (at *AutoTrader) resumeGrid() error {
	at.gridState.mu.Lock()
	at.gridState.IsPaused = false
	at.gridState.PauseReason = ""
	at.gridState.mu.Unlock()

	logger.Infof("[Grid] Resumed")
	return at.persistGridLedger()
}

// adjustGrid adjusts grid parameters
func (at *AutoTrader) adjustGrid(d *kernel.Decision) error {
	// Cancel existing orders first
	if err := at.cancelAllGridOrders(); err != nil {
		return err
	}

	gridConfig := at.config.StrategyConfig.GridConfig

	// Get current price
	price, err := at.trader.GetMarketPrice(gridConfig.Symbol)
	if err != nil {
		return fmt.Errorf("failed to get market price: %w", err)
	}

	// Reinitialize only empty slots; preserve every filled lot at its own level.
	at.gridState.mu.Lock()
	old := append([]kernel.GridLevelInfo(nil), at.gridState.Levels...)
	for i, level := range old {
		if i >= gridConfig.GridCount && (level.PositionSize > 0 || level.ExitOrderID != "") {
			at.gridState.mu.Unlock()
			return fmt.Errorf("grid count cannot discard open lots")
		}
	}
	at.initializeGridLevelsLocked(price, gridConfig)
	for i, level := range old {
		if level.PositionSize > 0 {
			at.gridState.Levels[i] = level
		}
	}
	at.gridState.mu.Unlock()

	logger.Infof("[Grid] Adjusted grid bounds around price $%.2f", price)
	return at.persistGridLedger()
}

// syncGridState syncs grid state with exchange
func (at *AutoTrader) syncGridState() {
	if at.gridState == nil {
		return
	}
	defer at.persistGridLedger()
	symbol := at.config.StrategyConfig.GridConfig.Symbol
	at.gridState.mu.RLock()
	levels := append([]kernel.GridLevelInfo(nil), at.gridState.Levels...)
	at.gridState.mu.RUnlock()
	for i, old := range levels {
		if old.ExitOrderID != "" {
			at.reconcileGridExit(i, nil)
			continue
		}
		if old.State != "pending" || old.OrderID == "" {
			continue
		}
		receipt, err := at.trader.GetOrderStatus(symbol, old.OrderID)
		if err != nil {
			continue
		}
		st, _ := receipt["status"].(string)
		st = strings.ToUpper(st)
		terminal := st == "FILLED" || st == "CANCELED" || st == "EXPIRED" || st == "REJECTED"
		if !terminal && st != "PARTIALLY_FILLED" && st != "NEW" {
			continue
		}
		if _, exists := receipt["executedQty"]; !exists {
			continue
		}
		qty, avg, valid := fillReceipt(receipt)
		if !valid {
			continue
		}
		if qty < old.ExecutedQuantity || (qty > 0 && avg <= 0) || (st == "FILLED" && qty <= 0) {
			continue
		}
		at.gridState.mu.Lock()
		if i >= len(at.gridState.Levels) || at.gridState.Levels[i].OrderID != old.OrderID {
			at.gridState.mu.Unlock()
			continue
		}
		level := &at.gridState.Levels[i]
		if qty > 0 {
			if level.ExecutedQuantity == 0 {
				at.gridState.TotalTrades++
			}
			level.PositionSize = qty
			level.PositionEntry = avg
			level.ExecutedQuantity = qty
		}
		if terminal {
			if qty > 0 {
				level.State = "filled"
			} else {
				level.State = "empty"
			}
			delete(at.gridState.OrderBook, level.OrderID)
			level.OrderID = ""
			level.OrderQuantity = 0
		}
		at.gridState.mu.Unlock()
	}
}

// closeAllPositions exits owned grid lots and requires final receipts.
func (at *AutoTrader) closeAllPositions() error {
	if at.gridState == nil || at.config.StrategyConfig.GridConfig == nil {
		return nil
	}
	at.gridState.mu.RLock()
	levels := append([]kernel.GridLevelInfo(nil), at.gridState.Levels...)
	at.gridState.mu.RUnlock()
	var failures []error
	for i, level := range levels {
		if level.PositionSize > 0 || level.ExitOrderID != "" {
			if err := at.closeGridLevel(i); err != nil {
				failures = append(failures, fmt.Errorf("grid level %d: %w", i, err))
			}
		}
	}
	return errors.Join(failures...)
}

// closeGridLevel retains exit tasks until cumulative final fills are confirmed.
func (at *AutoTrader) closeGridLevel(index int) error {
	cfg := at.config.StrategyConfig.GridConfig
	at.gridState.mu.RLock()
	level := at.gridState.Levels[index]
	at.gridState.mu.RUnlock()
	if level.ExitOrderID == "" {
		if level.State == "pending" && level.OrderID != "" {
			if err := at.cancelGridOrder(&kernel.Decision{Symbol: cfg.Symbol, OrderID: level.OrderID}); err != nil {
				return err
			}
			at.gridState.mu.RLock()
			level = at.gridState.Levels[index]
			at.gridState.mu.RUnlock()
		}
		if level.PositionSize <= 0 {
			return nil
		}
		var result map[string]interface{}
		var err error
		if level.Side == "buy" {
			result, err = at.trader.CloseLong(cfg.Symbol, level.PositionSize)
		} else {
			result, err = at.trader.CloseShort(cfg.Symbol, level.PositionSize)
		}
		if err != nil {
			return fmt.Errorf("close failed: %w", err)
		}
		id, ok := orderIDString(result)
		if !ok {
			id = "unknown"
		}
		at.gridState.mu.Lock()
		at.gridState.Levels[index].ExitOrderID = id
		at.gridState.Levels[index].ExitExecutedQuantity = 0
		at.gridState.Levels[index].ExitRealizedPnL = 0
		at.gridState.mu.Unlock()
		if err := at.persistGridLedger(); err != nil {
			return err
		}
		at.reconcileGridExit(index, result)
	} else {
		at.reconcileGridExit(index, nil)
	}
	at.gridState.mu.RLock()
	remaining := at.gridState.Levels[index]
	at.gridState.mu.RUnlock()
	if remaining.ExitOrderID != "" || remaining.PositionSize > 1e-9 {
		return fmt.Errorf("exit unconfirmed: order=%s remaining=%.8g", remaining.ExitOrderID, remaining.PositionSize)
	}
	return nil
}

// checkAndExecuteStopLoss checks if any filled level has exceeded stop loss and closes it
func (at *AutoTrader) checkAndExecuteStopLoss() {
	cfg := at.config.StrategyConfig.GridConfig
	if cfg.StopLossPct <= 0 {
		return
	}
	price, err := at.trader.GetMarketPrice(cfg.Symbol)
	if err != nil {
		logger.Warnf("[Grid] Stop-loss price unknown: %v", err)
		return
	}
	at.gridState.mu.RLock()
	levels := append([]kernel.GridLevelInfo(nil), at.gridState.Levels...)
	at.gridState.mu.RUnlock()
	for i, snapshot := range levels {
		if snapshot.ExitOrderID != "" {
			at.reconcileGridExit(i, nil)
			continue
		}
		if snapshot.PositionSize <= 0 || snapshot.PositionEntry <= 0 {
			continue
		}
		loss := (snapshot.PositionEntry - price) / snapshot.PositionEntry * 100
		if snapshot.Side == "sell" {
			loss = -loss
		}
		if loss < cfg.StopLossPct {
			continue
		}
		if err := at.closeGridLevel(i); err != nil {
			logger.Errorf("[Grid] Stop-loss exit incomplete: %v", err)
		}
	}
}

// Unknown exit receipts remain tied to their order ID and never trigger another close.
func (at *AutoTrader) reconcileGridExit(index int, receipt map[string]interface{}) {
	at.gridState.mu.RLock()
	level := at.gridState.Levels[index]
	at.gridState.mu.RUnlock()
	var err error
	if level.ExitOrderID != "unknown" {
		receipt, err = at.trader.GetOrderStatus(at.config.StrategyConfig.GridConfig.Symbol, level.ExitOrderID)
	}
	if err != nil || receipt == nil {
		logger.Warnf("[Grid] Close %s final fill unknown: %v", level.ExitOrderID, err)
		return
	}
	st, _ := receipt["status"].(string)
	st = strings.ToUpper(st)
	terminal := st == "FILLED" || st == "CANCELED" || st == "EXPIRED" || st == "REJECTED"
	if !terminal && st != "NEW" && st != "PARTIALLY_FILLED" {
		return
	}
	if _, ok := receipt["executedQty"]; !ok {
		return
	}
	qty, avg, valid := fillReceipt(receipt)
	if !valid {
		return
	}
	if qty < level.ExitExecutedQuantity || (qty > 0 && avg <= 0) || (st == "FILLED" && qty <= 0) {
		return
	}
	delta := math.Min(qty-level.ExitExecutedQuantity, level.PositionSize)
	realized := (avg - level.PositionEntry) * qty
	if level.Side == "sell" {
		realized = -realized
	}
	pnlDelta := realized - level.ExitRealizedPnL
	defer at.persistGridLedger()
	at.gridState.mu.Lock()
	defer at.gridState.mu.Unlock()
	current := &at.gridState.Levels[index]
	current.PositionSize = math.Max(0, current.PositionSize-delta)
	current.ExitExecutedQuantity = qty
	current.ExitRealizedPnL = realized
	at.gridState.DailyPnL += pnlDelta
	at.gridState.TotalProfit += pnlDelta
	if terminal {
		current.ExitOrderID = ""
		current.ExitExecutedQuantity = 0
		current.ExitRealizedPnL = 0
		at.gridState.TotalTrades++
		if current.PositionSize <= 1e-9 {
			current.State = "stopped"
			current.PositionSize = 0
			current.UnrealizedPnL = 0
		}
	}
}

// Persist an exchange-side stop before the local protection service exits.
func (at *AutoTrader) protectGridResidue() error {
	cfg := at.config.StrategyConfig.GridConfig
	if cfg.StopLossPct <= 0 {
		return nil
	}
	at.gridState.mu.RLock()
	levels := append([]kernel.GridLevelInfo(nil), at.gridState.Levels...)
	at.gridState.mu.RUnlock()
	type protection struct{ quantity, price float64 }
	legs := map[string]protection{}
	for _, level := range levels {
		if level.PositionSize <= 0 || level.PositionEntry <= 0 {
			continue
		}
		side := "LONG"
		stop := level.PositionEntry * (1 - cfg.StopLossPct/100)
		if level.Side == "sell" {
			side = "SHORT"
			stop = level.PositionEntry * (1 + cfg.StopLossPct/100)
		}
		leg := legs[side]
		leg.quantity += level.PositionSize
		// Tightest configured stop covers all owned lots on this side.
		if leg.price == 0 || (side == "LONG" && stop > leg.price) || (side == "SHORT" && stop < leg.price) {
			leg.price = stop
		}
		legs[side] = leg
	}
	var failures []error
	for side, leg := range legs {
		err := ensureProtectiveCoverage(at.trader, cfg.Symbol, side, "SL", leg.price, leg.quantity)
		if err == nil {
			err, _ = at.verifyProtectiveLegs(&kernel.Decision{Symbol: cfg.Symbol, StopLoss: leg.price}, side, true, false, leg.quantity, 0)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("grid residual %s qty %.8g protection unconfirmed: %w", side, leg.quantity, err))
		}
	}
	return errors.Join(failures...)
}
