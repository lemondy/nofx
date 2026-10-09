package store

import (
	"fmt"
	"math"
	"time"
)

// StockOwnedRows returns the OPEN LONG rows the us_stock program itself opened
// for a symbol (ai_managed). Their summed Quantity is the ONLY quantity the
// program manages — spot balances also hold the user's manual buys
// (us_stock, design 2026-10-09 §5/§9.6).
func (s *PositionStore) StockOwnedRows(traderID, symbol string) ([]*TraderPosition, error) {
	var rows []*TraderPosition
	err := s.db.Where("trader_id = ? AND symbol = ? AND UPPER(side) = ? AND status = ? AND ai_managed = ?",
		traderID, symbol, "LONG", "OPEN", true).Order("entry_time ASC").Find(&rows).Error
	return rows, err
}

// ApplyExternalReduction shrinks a program-owned row to newQty after part of
// the holding left the account outside the program (manual sale, exchange-side
// stop). The removed part is NOT booked as an exit: entry_quantity drops by the
// same amount so the row's cost base stays the program's own, and no PnL is
// attributed. newQty <= 0 closes the row with zero PnL (reason "external").
func (s *PositionStore) ApplyExternalReduction(id int64, newQty, price float64, at time.Time) error {
	var pos TraderPosition
	if err := s.db.First(&pos, id).Error; err != nil {
		return fmt.Errorf("failed to get position: %w", err)
	}
	nowMs := at.UTC().UnixMilli()
	if newQty <= 1e-9 {
		return s.db.Model(&TraderPosition{}).Where("id = ?", id).Updates(map[string]interface{}{
			"quantity":       0,
			"exit_price":     price,
			"exit_time":      nowMs,
			"status":         "CLOSED",
			"close_reason":   "external",
			"updated_at":     nowMs,
			"entry_quantity": math.Max(pos.EntryQuantity-pos.Quantity, 0),
		}).Error
	}
	diff := pos.Quantity - newQty
	if diff <= 0 {
		return nil
	}
	entryQty := pos.EntryQuantity
	if entryQty <= 0 {
		entryQty = pos.Quantity
	}
	return s.db.Model(&TraderPosition{}).Where("id = ?", id).Updates(map[string]interface{}{
		"quantity":       newQty,
		"entry_quantity": math.Max(entryQty-diff, newQty),
		"updated_at":     nowMs,
	}).Error
}
