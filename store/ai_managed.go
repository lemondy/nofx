package store

import (
	"time"

	"gorm.io/gorm"
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
	ID        int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID  string    `gorm:"column:trader_id;not null;index:idx_ai_managed_key,unique" json:"trader_id"`
	Symbol    string    `gorm:"column:symbol;not null;index:idx_ai_managed_key,unique" json:"symbol"`
	Side      string    `gorm:"column:side;not null;index:idx_ai_managed_key,unique" json:"side"` // long / short
	CreatedAt time.Time `gorm:"column:created_at" json:"created_at"`
}

func (AIManagedPosition) TableName() string { return "ai_managed_positions" }

type AIManagedStore struct {
	db *gorm.DB
}

func NewAIManagedStore(db *gorm.DB) *AIManagedStore {
	return &AIManagedStore{db: db}
}

func (s *AIManagedStore) initTables() error {
	return s.db.AutoMigrate(&AIManagedPosition{})
}

// Mark records the (trader, symbol, side) triple as AI-opened. Idempotent.
func (s *AIManagedStore) Mark(traderID, symbol, side string) error {
	return s.db.Where(AIManagedPosition{TraderID: traderID, Symbol: symbol, Side: side}).
		FirstOrCreate(&AIManagedPosition{TraderID: traderID, Symbol: symbol, Side: side, CreatedAt: time.Now()}).Error
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

// List returns every marked position key for a trader.
func (s *AIManagedStore) List(traderID string) ([]AIManagedPosition, error) {
	var out []AIManagedPosition
	err := s.db.Where("trader_id = ?", traderID).Find(&out).Error
	return out, err
}
