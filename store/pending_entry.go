package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
// ownership from them. One resting entry per trader+symbol+side.
type PendingEntryDB struct {
	ExecutedQty  float64   `gorm:"column:executed_qty;default:0" json:"executed_qty"`
	ProtectedQty float64   `gorm:"column:protected_qty;default:0" json:"protected_qty"`
	ID           uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID     string    `gorm:"column:trader_id;index:idx_pending_trader_symbol_side,unique" json:"trader_id"`
	Symbol       string    `gorm:"column:symbol;index:idx_pending_trader_symbol_side,unique" json:"symbol"`
	Side         string    `gorm:"column:side;size:8;index:idx_pending_trader_symbol_side,unique" json:"side"` // long / short
	Price        float64   `gorm:"column:price" json:"price"`
	Quantity     float64   `gorm:"column:quantity" json:"quantity"`
	StopLoss     float64   `gorm:"column:stop_loss" json:"stop_loss"`
	TakeProfit   float64   `gorm:"column:take_profit" json:"take_profit"`
	Leverage     int       `gorm:"column:leverage" json:"leverage"`
	OrderID      string    `gorm:"column:order_id;size:64" json:"order_id"`
	PlacedAt     time.Time `gorm:"column:placed_at" json:"placed_at"`
	// ExitMode rides the pending entry so the fill stamps the position's
	// exit template (trend|range|quick) even when the fill lands offline.
	ExitMode string `gorm:"column:exit_mode" json:"exit_mode,omitempty"`
}

func (PendingEntryDB) TableName() string { return "trader_pending_entries" }

type PendingEntryStore struct {
	db *gorm.DB
}

func NewPendingEntryStore(db *gorm.DB) *PendingEntryStore {
	return &PendingEntryStore{db: db}
}

func (s *PendingEntryStore) initTables() error {
	// The old unique index allowed only one side per symbol. Remove it before
	// AutoMigrate creates the per-side index, preserving existing rows.
	if s.db.Migrator().HasIndex(&PendingEntryDB{}, "idx_pending_trader_symbol") {
		if err := s.db.Migrator().DropIndex(&PendingEntryDB{}, "idx_pending_trader_symbol"); err != nil {
			return err
		}
	}
	return s.db.AutoMigrate(&PendingEntryDB{}, &GridCheckpoint{})
}

// Upsert writes/refreshes the shadow row for one trader+symbol+side.
// Upsert (2026-10-03 review): a true ON CONFLICT upsert — the old GET-then-
// CREATE/SAVE raced the fill-sync path on the (trader,symbol,side) unique
// key, the loser's Create failed, persistPendingEntry only logged, and the
// missing shadow row orphaned the resting order at restart until the tag
// scan rescued it.
func (s *PendingEntryStore) Upsert(rec *PendingEntryDB) error {
	err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "trader_id"}, {Name: "symbol"}, {Name: "side"}},
		UpdateAll: true,
	}).Create(rec).Error
	if err == nil {
		return nil
	}
	// Legacy/odd index shapes (pre-migration tables) reject the conflict
	// target — fall back to read-then-write. Single-writer SQLite plus the
	// per-trader execution mutex keep the race window acceptable.
	var existing PendingEntryDB
	err2 := s.db.Where("trader_id = ? AND symbol = ? AND side = ?", rec.TraderID, rec.Symbol, rec.Side).First(&existing).Error
	if err2 == nil {
		rec.ID = existing.ID
		return s.db.Save(rec).Error
	}
	if errors.Is(err2, gorm.ErrRecordNotFound) {
		return s.db.Create(rec).Error
	}
	return fmt.Errorf("pending entry lookup failed: %w", err2)
}

// Delete removes the shadow row (order dropped/filled/cancelled).
func (s *PendingEntryStore) Delete(traderID, symbol, side string) error {
	return s.db.Where("trader_id = ? AND symbol = ? AND side = ?", traderID, symbol, side).Delete(&PendingEntryDB{}).Error
}

// List returns all shadow rows of one trader (startup reconciliation input).
func (s *PendingEntryStore) List(traderID string) ([]*PendingEntryDB, error) {
	var out []*PendingEntryDB
	err := s.db.Where("trader_id = ?", traderID).Find(&out).Error
	return out, err
}

func (s *PendingEntryStore) ListForAccount(exchangeID string) ([]*PendingEntryDB, error) {
	var rows []*PendingEntryDB
	err := s.db.Table("trader_pending_entries AS p").Select("p.*").Joins("JOIN traders t ON t.id = p.trader_id").Where("t.exchange_id = ?", exchangeID).Find(&rows).Error
	return rows, err
}

// The grid ledger contains owned entry/exit IDs and cumulative fill watermarks.
// Keep it beyond Stop so failed cancellation and partial exits can be reconciled.
type GridCheckpoint struct {
	TraderID   string `gorm:"primaryKey"`
	ExchangeID string
	Symbol     string
	StateJSON  string
	UpdatedAt  time.Time
}

func (GridCheckpoint) TableName() string { return "trader_grid_checkpoints" }
func (s *PendingEntryStore) SaveGrid(row *GridCheckpoint) error {
	return s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(row).Error
}
func (s *PendingEntryStore) LoadGrid(traderID string) (*GridCheckpoint, error) {
	var row GridCheckpoint
	err := s.db.Where("trader_id = ?", traderID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &row, err
}

func (s *PendingEntryStore) ListGridForAccount(accountKey string) ([]GridCheckpoint, error) {
	var rows []GridCheckpoint
	err := s.db.Where("exchange_id = ?", accountKey).Find(&rows).Error
	return rows, err
}
