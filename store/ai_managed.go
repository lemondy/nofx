package store

import (
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ============================================================================
// AI-managed position registry (user directive 2026-09-25): positions NOT
// opened by the AI are hands-off for the automation — no watchdog seeding or
// repair, no vol-target/trailing/1R-lock/breakeven-arm, and AI close/adjust
// decisions on them are rejected. Binance-side, AI and manual entries share
// the account and API key, so ownership is decided by THIS registry: every
// AI open path marks its position at fill time, and everything unmarked is
// manual. Durable (survives restarts) — an in-memory set would reclassify
// every manual position as AI on the next deploy and silently resume
// touching it.
// ============================================================================

type AIManagedPosition struct {
	ID           int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID     string    `gorm:"column:trader_id;not null;index:idx_ai_managed_key,unique" json:"trader_id"`
	Symbol       string    `gorm:"column:symbol;not null;index:idx_ai_managed_key,unique" json:"symbol"`
	Side         string    `gorm:"column:side;not null;index:idx_ai_managed_key,unique" json:"side"` // long / short
	EntryOrderID string    `gorm:"column:entry_order_id;not null;default:''" json:"entry_order_id"`
	CreatedAt    time.Time `gorm:"column:created_at" json:"created_at"`
}

func (AIManagedPosition) TableName() string { return "ai_managed_positions" }

type AIManagedStore struct {
	db *gorm.DB
}

func NewAIManagedStore(db *gorm.DB) *AIManagedStore {
	return &AIManagedStore{db: db}
}

func (s *AIManagedStore) initTables() error {
	return s.db.AutoMigrate(&AIManagedPosition{}, &AIEntryOrder{})
}

// Mark records the (trader, symbol, side) triple as AI-opened. Idempotent.
func (s *AIManagedStore) Mark(traderID, symbol, side string) error {
	return s.mark(traderID, symbol, side, "")
}

// Durable order provenance survives clearing the live position registry.
type AIEntryOrder struct {
	TraderID string `gorm:"primaryKey"`
	OrderID  string `gorm:"primaryKey"`
	Symbol   string
	Side     string
}

func (AIEntryOrder) TableName() string { return "ai_entry_orders" }

func (s *AIManagedStore) MarkEntry(traderID, symbol, side, orderID string) error {
	return s.mark(traderID, symbol, side, orderID)
}
func (s *AIManagedStore) mark(traderID, symbol, side, orderID string) error {
	side = strings.ToLower(side)
	return s.db.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		if orderID != "" {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&AIEntryOrder{TraderID: traderID, OrderID: orderID, Symbol: symbol, Side: side}).Error; err != nil {
				return err
			}
		}
		mark := AIManagedPosition{TraderID: traderID, Symbol: symbol, Side: side, CreatedAt: now}
		if err := tx.Where(AIManagedPosition{TraderID: traderID, Symbol: symbol, Side: side}).FirstOrCreate(&mark).Error; err != nil {
			return err
		}
		if orderID != "" {
			if err := tx.Model(&mark).Update("entry_order_id", orderID).Error; err != nil {
				return err
			}
		}
		q := tx.Model(&TraderPosition{}).Where("trader_id = ? AND symbol = ? AND LOWER(side) = ? ", traderID, symbol, side)
		if orderID != "" {
			q = q.Where("entry_order_id = ?", orderID)
		} else {
			q = q.Where("status = ? AND created_at <= ?", "OPEN", mark.CreatedAt.UnixMilli())
		}
		return q.Update("ai_managed", true).Error
	})
}
func (s *AIManagedStore) OwnsEntry(traderID, symbol, side, orderID string) bool {
	if orderID == "" {
		return false
	}
	var count int64
	s.db.Model(&AIEntryOrder{}).Where("trader_id = ? AND order_id = ? AND symbol = ? AND LOWER(side) = ?", traderID, orderID, symbol, strings.ToLower(side)).Count(&count)
	if count > 0 {
		return true
	}
	// Resting AI plans are persisted before fills reach the syncer.
	s.db.Model(&PendingEntryDB{}).Where("trader_id = ? AND order_id = ? AND symbol = ? AND LOWER(side) = ?", traderID, orderID, symbol, strings.ToLower(side)).Count(&count)
	return count > 0
}

// Unmark removes the mark (position fully closed).
func (s *AIManagedStore) Unmark(traderID, symbol, side string) error {
	return s.db.Where("trader_id = ? AND symbol = ? AND side = ?", traderID, symbol, side).
		Delete(&AIManagedPosition{}).Error
}

// IsMarked reports whether this position was opened by the AI.
func (s *AIManagedStore) IsMarked(traderID, symbol, side string) bool {
	var n int64
	s.db.Model(&AIManagedPosition{}).
		Where("trader_id = ? AND symbol = ? AND side = ?", traderID, symbol, side).
		Count(&n)
	return n > 0
}

// IsLegacyMarked supports marks made before entry order provenance was recorded.
// A live mark with a known order must not claim unrelated manual entry orders.
func (s *AIManagedStore) IsLegacyMarked(traderID, symbol, side string) bool {
	var n int64
	s.db.Model(&AIManagedPosition{}).Where("trader_id = ? AND symbol = ? AND side = ? AND entry_order_id = ''", traderID, symbol, strings.ToLower(side)).Count(&n)
	return n > 0
}

// List returns every marked position key for a trader.
func (s *AIManagedStore) List(traderID string) ([]AIManagedPosition, error) {
	var out []AIManagedPosition
	err := s.db.Where("trader_id = ?", traderID).Find(&out).Error
	return out, err
}

// MarkedAfter reports whether a mark exists for this (trader, symbol, side)
// that was created AT OR AFTER the given instant. The position builder's
// close-time re-check uses it to distinguish the one-time seedAIManagedOnce
// migration's pre-existing rows (created before their mark — AI's book) from
// manual rows that merely share the symbol+side with an active AI position
// (created after any mark — created unmarked because the mark did not exist
// yet, 09-28 review P3 regression: a manual win on the same symbol+side was
// being stamped AI by the close-time re-check and reset the loss streak).
func (s *AIManagedStore) MarkedAfter(traderID, symbol, side string, at time.Time) bool {
	var mark AIManagedPosition
	err := s.db.Where("trader_id = ? AND symbol = ? AND side = ? AND entry_order_id = ''", traderID, symbol, strings.ToLower(side)).
		First(&mark).Error
	if err != nil {
		return false
	}
	return !at.After(mark.CreatedAt)
}
