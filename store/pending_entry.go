package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ============================================================================
// Pending limit-entry persistence (write-through shadow of the trader's
// in-memory pendingEntries map)
// ============================================================================

// PendingEntryDB is the durable shadow of one resting limit-entry order.
// pendingEntries in the trader is memory-only, so a restart would orphan the
// resting GTC order: nobody would manage its expiry, and a fill would go
// unprotected (SL/TP are placed by the pending-entry lifecycle on FILLED).
// Rows are written on every set/drop; startup reconciliation restores
// ownership from them. One resting entry per trader+symbol (replace
// semantics, same as the map).
type PendingEntryDB struct {
	ID         uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID   string    `gorm:"column:trader_id;index:idx_pending_trader_symbol,unique" json:"trader_id"`
	Symbol     string    `gorm:"column:symbol;index:idx_pending_trader_symbol,unique" json:"symbol"`
	Side       string    `gorm:"column:side;size:8" json:"side"` // long / short
	Price      float64   `gorm:"column:price" json:"price"`
	Quantity   float64   `gorm:"column:quantity" json:"quantity"`
	StopLoss   float64   `gorm:"column:stop_loss" json:"stop_loss"`
	TakeProfit float64   `gorm:"column:take_profit" json:"take_profit"`
	Leverage   int       `gorm:"column:leverage" json:"leverage"`
	OrderID    string    `gorm:"column:order_id;size:64" json:"order_id"`
	PlacedAt   time.Time `gorm:"column:placed_at" json:"placed_at"`
}

func (PendingEntryDB) TableName() string { return "trader_pending_entries" }

type PendingEntryStore struct {
	db *gorm.DB
}

func NewPendingEntryStore(db *gorm.DB) *PendingEntryStore {
	return &PendingEntryStore{db: db}
}

func (s *PendingEntryStore) initTables() error {
	return s.db.AutoMigrate(&PendingEntryDB{})
}

// Upsert writes/refreshes the shadow row for one trader+symbol.
func (s *PendingEntryStore) Upsert(rec *PendingEntryDB) error {
	var existing PendingEntryDB
	err := s.db.Where("trader_id = ? AND symbol = ?", rec.TraderID, rec.Symbol).First(&existing).Error
	if err == nil {
		rec.ID = existing.ID
		return s.db.Save(rec).Error
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.db.Create(rec).Error
	}
	return fmt.Errorf("pending entry lookup failed: %w", err)
}

// Delete removes the shadow row (order dropped/filled/cancelled).
func (s *PendingEntryStore) Delete(traderID, symbol string) error {
	return s.db.Where("trader_id = ? AND symbol = ?", traderID, symbol).Delete(&PendingEntryDB{}).Error
}

// List returns all shadow rows of one trader (startup reconciliation input).
func (s *PendingEntryStore) List(traderID string) ([]*PendingEntryDB, error) {
	var out []*PendingEntryDB
	err := s.db.Where("trader_id = ?", traderID).Find(&out).Error
	return out, err
}
