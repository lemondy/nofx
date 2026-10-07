package store

import (
	"fmt"
	"math"
	"strings"

	"gorm.io/gorm"
)

// One-off data repair for the 2026-10-07 review (see
// docs/architecture/FULLSTACK_REVIEW_2026-10-07.md):
//
//   - N1: ClosePositionFully double-added the running PnL/fee total
//     (357eb802, 10-03) — closed rows carry 2× entry fee and 2× earlier
//     partial-close PnL. The fills table is the source of truth; the repair
//     recomputes each closed row from its own fills.
//   - N2: syncs recorded the FILL id as entry_order_id, so the order-id keyed
//     AI ownership never matched. The repair re-keys a row by its real
//     exchange order id and stamps ai_managed when that order is in
//     ai_entry_orders.
//
// Both repairs are idempotent: a row already consistent with its fills or
// already keyed by an order id is left untouched.

// CloseAccumulationFix is one row whose stored PnL/fee disagree with fills.
type CloseAccumulationFix struct {
	PositionID  int64
	Symbol      string
	Side        string
	StoredPnL   float64
	StoredFee   float64
	FillsPnL    float64
	FillsFee    float64
	FillCount   int
	JournalRows int64
}

// RepairCloseAccumulation recomputes realized_pnl/fee of CLOSED rows exited at
// or after sinceMs from their fills (same symbol, same position side, inside
// [entry_time, exit_time]). apply=false only reports. The matching
// trade_journal row (pnl, fee, pnl_pct) is corrected alongside.
func (s *Store) RepairCloseAccumulation(traderID string, sinceMs int64, apply bool) ([]CloseAccumulationFix, error) {
	var rows []TraderPosition
	if err := s.gdb.Where("trader_id = ? AND status = ? AND exit_time >= ?", traderID, "CLOSED", sinceMs).
		Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	var fixes []CloseAccumulationFix
	for _, p := range rows {
		var agg struct {
			PnL float64 `gorm:"column:pnl"`
			Fee float64 `gorm:"column:fee"`
			N   int     `gorm:"column:n"`
		}
		err := s.gdb.Raw(`
			SELECT COALESCE(SUM(f.realized_pnl),0) AS pnl, COALESCE(SUM(f.commission),0) AS fee, COUNT(*) AS n
			FROM trader_fills f JOIN trader_orders o ON o.id = f.order_id
			WHERE f.trader_id = ? AND f.symbol = ? AND UPPER(o.position_side) = ?
			  AND f.created_at >= ? AND f.created_at <= ?`,
			traderID, p.Symbol, strings.ToUpper(p.Side), p.EntryTime, p.ExitTime).Scan(&agg).Error
		if err != nil {
			return fixes, err
		}
		if agg.N == 0 {
			continue // no fill evidence (reconcile-only rows) — leave alone
		}
		if math.Abs(agg.PnL-p.RealizedPnL) < 1e-6 && math.Abs(agg.Fee-p.Fee) < 1e-6 {
			continue
		}
		fix := CloseAccumulationFix{
			PositionID: p.ID, Symbol: p.Symbol, Side: p.Side,
			StoredPnL: p.RealizedPnL, StoredFee: p.Fee,
			FillsPnL: agg.PnL, FillsFee: agg.Fee, FillCount: agg.N,
		}
		if apply {
			err := s.gdb.Transaction(func(tx *gorm.DB) error {
				if err := tx.Model(&TraderPosition{}).Where("id = ?", p.ID).
					Updates(map[string]interface{}{"realized_pnl": agg.PnL, "fee": agg.Fee}).Error; err != nil {
					return err
				}
				fixed := p
				fixed.RealizedPnL, fixed.Fee = agg.PnL, agg.Fee
				res := tx.Model(&TradeJournalDB{}).Where("trader_id = ? AND position_id = ?", traderID, p.ID).
					Updates(map[string]interface{}{
						"realized_pnl": agg.PnL,
						"fee":          agg.Fee,
						"pnl_pct":      calculateJournalPnLPct(fixed),
					})
				if res.Error != nil {
					return res.Error
				}
				fix.JournalRows = res.RowsAffected
				return nil
			})
			if err != nil {
				return fixes, fmt.Errorf("position %d: %w", p.ID, err)
			}
		}
		fixes = append(fixes, fix)
	}
	return fixes, nil
}

// PositionsKeyedByFillSince lists rows opened at or after sinceMs (any
// status) — the candidates for the N2 re-keying.
func (s *Store) PositionsKeyedByFillSince(traderID string, sinceMs int64) ([]TraderPosition, error) {
	var rows []TraderPosition
	err := s.gdb.Where("trader_id = ? AND entry_time >= ? AND entry_order_id != ''", traderID, sinceMs).
		Order("id").Find(&rows).Error
	return rows, err
}

// ReattributeEntryOrder re-keys one row by its real exchange order id and
// stamps ai_managed when that order is a recorded AI entry. Returns whether
// the row is (now) AI-owned. apply=false only reports.
func (s *Store) ReattributeEntryOrder(traderID string, positionID int64, symbol, side, orderID string, apply bool) (bool, error) {
	if orderID == "" {
		return false, nil
	}
	owned := s.AIManaged().OwnsEntry(traderID, symbol, side, orderID)
	if !apply {
		return owned, nil
	}
	updates := map[string]interface{}{"entry_order_id": orderID}
	if owned {
		updates["ai_managed"] = true
	}
	return owned, s.gdb.Model(&TraderPosition{}).Where("id = ?", positionID).Updates(updates).Error
}

// BackupTo writes a consistent copy of the SQLite database to path.
func (s *Store) BackupTo(path string) error {
	if strings.ContainsAny(path, "'") {
		return fmt.Errorf("backup path must not contain quotes")
	}
	return s.gdb.Exec(fmt.Sprintf("VACUUM INTO '%s'", path)).Error
}
