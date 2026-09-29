package store

import (
	"fmt"
	"math"
	"nofx/logger"
	"strings"
	"time"
)

// PositionBuilder handles position creation and updates with support for:
// - Position averaging (merging multiple opens)
// - Partial closes (reducing quantity)
// - FIFO matching
// - Time-ordered processing
type PositionBuilder struct {
	positionStore *PositionStore
}

// NewPositionBuilder creates a new PositionBuilder
func NewPositionBuilder(positionStore *PositionStore) *PositionBuilder {
	return &PositionBuilder{
		positionStore: positionStore,
	}
}

// ProcessTrade processes a single trade and updates position accordingly
// tradeTimeMs is Unix milliseconds UTC
func (pb *PositionBuilder) ProcessTrade(
	traderID, exchangeID, exchangeType, symbol, side, action string,
	quantity, price, fee, realizedPnL float64,
	tradeTimeMs int64,
	orderID string,
) error {
	if strings.HasPrefix(action, "open_") {
		return pb.handleOpen(traderID, exchangeID, exchangeType, symbol, side, quantity, price, fee, tradeTimeMs, orderID)
	} else if strings.HasPrefix(action, "close_") {
		return pb.handleClose(traderID, exchangeID, exchangeType, symbol, side, quantity, price, fee, realizedPnL, tradeTimeMs, orderID)
	}
	return nil
}

// handleOpen handles opening positions (create new or average into existing)
// tradeTimeMs is Unix milliseconds UTC
func (pb *PositionBuilder) handleOpen(
	traderID, exchangeID, exchangeType, symbol, side string,
	quantity, price, fee float64,
	tradeTimeMs int64,
	orderID string,
) error {
	// Get existing OPEN position for (symbol, side)
	existing, err := pb.positionStore.GetOpenPositionBySymbol(traderID, symbol, side)
	if err != nil {
		return fmt.Errorf("failed to get open position: %w", err)
	}

	nowMs := time.Now().UTC().UnixMilli()
	if existing == nil {
		// Ownership stamp at row creation (09-28 review P3): AI open paths
		// mark the ai_managed registry BEFORE placing the order, so a row
		// born while the mark exists belongs to the AI's book — manual fills
		// averaging into it keep the first-open ownership. The loss-streak
		// circuit breaker consumes this flag to keep manual trades (and
		// other traders sharing the account) out of the AI's streak.
		aiOwned := NewAIManagedStore(pb.positionStore.db).IsMarked(traderID, symbol, strings.ToLower(side))
		// Create new position
		position := &TraderPosition{
			TraderID:           traderID,
			ExchangeID:         exchangeID,
			ExchangeType:       exchangeType,
			ExchangePositionID: fmt.Sprintf("sync_%s_%s_%d", symbol, side, tradeTimeMs),
			Symbol:             symbol,
			Side:               side,
			Quantity:           quantity,
			EntryPrice:         price,
			EntryOrderID:       orderID,
			EntryTime:          tradeTimeMs,
			Leverage:           1,
			Status:             "OPEN",
			Source:             "sync",
			Fee:                fee,
			AIManaged:          aiOwned,
			CreatedAt:          nowMs,
			UpdatedAt:          nowMs,
		}
		return pb.positionStore.CreateOpenPosition(position)
	}

	// Merge: Calculate weighted average entry price and update position
	logger.Infof("  📊 Averaging position: %s %s %.6f @ %.2f + %.6f @ %.2f",
		symbol, side, existing.Quantity, existing.EntryPrice, quantity, price)

	// Also update exchange_id and exchange_type if they were empty
	if existing.ExchangeID == "" || existing.ExchangeType == "" {
		if err := pb.positionStore.UpdatePositionExchangeInfo(existing.ID, exchangeID, exchangeType); err != nil {
			logger.Infof("  ⚠️  Failed to update exchange info: %v", err)
		}
	}

	return pb.positionStore.UpdatePositionQuantityAndPrice(existing.ID, quantity, price, fee)
}

// handleClose handles closing positions (partial or full)
// tradeTimeMs is Unix milliseconds UTC
func (pb *PositionBuilder) handleClose(
	traderID, exchangeID, exchangeType, symbol, side string,
	quantity, price, fee, realizedPnL float64,
	tradeTimeMs int64,
	orderID string,
) error {
	// Get OPEN position
	position, err := pb.positionStore.GetOpenPositionBySymbol(traderID, symbol, side)
	if err != nil {
		return fmt.Errorf("failed to get open position: %w", err)
	}

	if position == nil {
		// No OPEN row — but if the orphan-reconcile pass just closed one for
		// this (trader, symbol, side) with 0 PnL (the race the pass now
		// pre-flushes against, user report 09-29: BTWUSDT +13.66U vanished),
		// re-attribute this fill's exchange-reported PnL to that row instead
		// of dropping it forever.
		if pb.positionStore.BackfillReconciledPnL(traderID, symbol, side, realizedPnL, fee, price, tradeTimeMs) {
			return nil
		}
		// Otherwise: trades out of order or database cleared — skip.
		logger.Infof("  ⚠️  No matching open position for %s %s (orderID: %s), skipping", symbol, side, orderID)
		return nil
	}

	const QUANTITY_TOLERANCE = 0.0001

	// Calculate realized PnL if not provided (some exchanges like Lighter don't return it)
	if realizedPnL == 0 && position.EntryPrice > 0 {
		if side == "LONG" {
			realizedPnL = (price - position.EntryPrice) * quantity
		} else {
			realizedPnL = (position.EntryPrice - price) * quantity
		}
		// Round to 2 decimal places
		realizedPnL = math.Round(realizedPnL*100) / 100
	}

	if quantity < position.Quantity-QUANTITY_TOLERANCE {
		// Partial close: reduce quantity and update weighted average exit price
		logger.Infof("  📉 Partial close: %s %s %.6f → %.6f (closed %.6f @ %.2f, PnL: %.2f)",
			symbol, side, position.Quantity, position.Quantity-quantity, quantity, price, realizedPnL)
		return pb.positionStore.ReducePositionQuantity(position.ID, quantity, price, fee, realizedPnL)
	} else {
		// Full close (or close with tolerance): mark as CLOSED
		closeQty := quantity
		if quantity > position.Quantity {
			logger.Infof("  ⚠️  Over-close detected: %s %s trying to close %.6f but only %.6f open, closing full position",
				symbol, side, quantity, position.Quantity)
			closeQty = position.Quantity
		}

		// Calculate final weighted average exit price
		// Include previously accumulated partial close prices + this final close
		closedBefore := position.EntryQuantity - position.Quantity
		totalClosed := closedBefore + closeQty
		var finalExitPrice float64
		if totalClosed > 0 {
			finalExitPrice = (position.ExitPrice*closedBefore + price*closeQty) / totalClosed
			// Use adaptive precision based on price magnitude (for meme coins with very small prices)
			finalExitPrice = adaptivePriceRound(finalExitPrice, position.ExitPrice, price, position.EntryPrice)
		} else {
			finalExitPrice = price
		}

		// Calculate total PnL (existing + new)
		totalPnL := position.RealizedPnL + realizedPnL

		// Calculate total fee (existing + new)
		totalFee := position.Fee + fee

		logger.Infof("  ✅ Full close: %s %s %.6f @ %.2f (avg exit: %.2f, entry: %.2f, PnL: %.2f)",
			symbol, side, closeQty, price, finalExitPrice, position.EntryPrice, totalPnL)

		// Close-time ownership re-check (09-28 review P3): the registry mark
		// can still exist when OrderSync closes a row the open-time stamp
		// missed (the one-time seedAIManagedOnce migration's pre-existing
		// positions). ONLY rows created before their mark qualify — a manual
		// row sharing the symbol+side with an active AI position was created
		// unmarked because the mark did not exist yet, and must stay manual
		// (regression caught by TestLossStreakIgnoresManualTrades: a manual
		// win on the same symbol+side was stamped AI and reset the streak).
		if !position.AIManaged && NewAIManagedStore(pb.positionStore.db).MarkedAfter(
			traderID, symbol, strings.ToLower(side), time.UnixMilli(position.CreatedAt)) {
			if err := pb.positionStore.SetAIManaged(position.ID); err != nil {
				logger.Infof("  ⚠️  Failed to stamp ai_managed on closing row %d (%s %s): %v", position.ID, symbol, side, err)
			}
		}

		return pb.positionStore.ClosePositionFully(
			position.ID,
			finalExitPrice,
			orderID,
			tradeTimeMs,
			totalPnL,
			totalFee,
			"sync",
		)
	}
}

// quantitiesMatch checks if two quantities are close enough (within tolerance)
func quantitiesMatch(a, b float64) bool {
	const QUANTITY_TOLERANCE = 0.0001
	return math.Abs(a-b) < QUANTITY_TOLERANCE
}

// BackfillReconciledPnL re-attributes a close fill's exchange-reported PnL
// to the most recent row of the same (trader, symbol, side) that the orphan
// reconcile pass closed with 0 PnL within the last 30 minutes (the
// fill-sync race: the pass stamped a live exit price and 0 realized PnL,
// then the real fills landed). Accumulates PnL and fee, moves the exit to
// the actual fill price/time, and marks the row 'sync' — the reconciliation
// stamp has served its purpose. Returns whether a row was backfilled.
func (s *PositionStore) BackfillReconciledPnL(traderID, symbol, side string, realizedPnL, fee, price float64, tradeTimeMs int64) bool {
	var row TraderPosition
	// No close_reason filter: the FIRST backfill re-stamps the row 'sync',
	// and later fills of the same close (multi-leg exits) must keep landing
	// on it. A fill reaching this path was never booked anywhere (the only
	// caller is the no-OPEN-row branch), so accumulation cannot double-count.
	err := s.db.Where(
		"trader_id = ? AND symbol = ? AND UPPER(side) = ? AND status = ? AND exit_time >= ?",
		traderID, symbol, strings.ToUpper(side), "CLOSED",
		time.Now().UTC().UnixMilli()-30*60*1000,
	).Order("exit_time DESC").First(&row).Error
	if err != nil {
		return false
	}
	updates := map[string]interface{}{
		"realized_pnl": row.RealizedPnL + realizedPnL,
		"fee":          row.Fee + fee,
		"exit_price":   price,
		"exit_time":    tradeTimeMs,
		"close_reason": "sync",
		"updated_at":   time.Now().UTC().UnixMilli(),
	}
	if err := s.db.Model(&TraderPosition{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
		logger.Infof("  ⚠️  BackfillReconciledPnL failed for row #%d (%s %s): %v", row.ID, symbol, side, err)
		return false
	}
	logger.Infof("  🔧 Backfilled reconciled row #%d (%s %s): realized PnL %+.4f at fill %.6g (was stamped 0 by netting_reconcile)",
		row.ID, symbol, side, realizedPnL, price)
	return true
}
