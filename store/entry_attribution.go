package store

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ============================================================================
// Entry-source attribution: every AI-opened position gets one row recording
// WHICH candidate-pool source(s) it came from (ai500 / oi_top / short_scan /
// piggy_dash / ...), so per-source win rates can be computed later. Written
// at fill time right after the AI-managed ownership mark; best-effort (a
// failed record must never block the open). When the symbol was not in the
// cycle's candidate pool, Sources falls back to "unknown". Side is lowercase
// long/short, matching ai_managed_positions.
// ============================================================================

type EntryAttribution struct {
	ID               uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID         string    `gorm:"column:trader_id;not null;index:idx_entry_attr_key,priority:1;uniqueIndex:idx_entry_attr_order,priority:1" json:"trader_id"`
	Symbol           string    `gorm:"column:symbol;not null;index:idx_entry_attr_key,priority:2" json:"symbol"`
	Side             string    `gorm:"column:side;not null;index:idx_entry_attr_key,priority:3" json:"side"` // long / short
	OrderID          string    `gorm:"column:order_id;not null;default:'';uniqueIndex:idx_entry_attr_order,priority:2" json:"order_id"`
	Sources          string    `gorm:"column:sources;not null;default:''" json:"sources"` // comma-separated, e.g. "piggy_dash,short_scan"
	ScannerDirection string    `gorm:"column:scanner_direction;not null;default:''" json:"scanner_direction"`
	ShortScore       float64   `gorm:"column:short_score;not null;default:0" json:"short_score"`
	ShortGrade       string    `gorm:"column:short_grade;not null;default:''" json:"short_grade"`
	ShortUniverse    string    `gorm:"column:short_universe;not null;default:''" json:"short_universe"`
	ShortConfirmed   bool      `gorm:"column:short_confirmed;not null;default:false" json:"short_confirmed"`
	CreatedAt        time.Time `gorm:"column:created_at;index:idx_entry_attr_key,priority:4" json:"created_at"`
}

func (EntryAttribution) TableName() string { return "entry_attributions" }

type EntryAttributionStore struct {
	db *gorm.DB
}

func NewEntryAttributionStore(db *gorm.DB) *EntryAttributionStore {
	return &EntryAttributionStore{db: db}
}

func (s *EntryAttributionStore) initTables() error {
	return s.db.AutoMigrate(&EntryAttribution{})
}

// Record persists one attribution row per (trader, entry order). The same
// entry is marked from several paths (market open, pending fill, offline
// reconcile) — the first write wins and later ones are no-ops, so the row
// keeps the attribution captured closest to the decision (review 2026-10-07).
func (s *EntryAttributionStore) Record(a *EntryAttribution) error {
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(a).Error
}

// ListByTrader returns attribution rows for a trader created at or after the
// given instant, oldest first.
func (s *EntryAttributionStore) ListByTrader(traderID string, since time.Time) ([]EntryAttribution, error) {
	var out []EntryAttribution
	err := s.db.Where("trader_id = ? AND created_at >= ?", traderID, since).
		Order("created_at ASC").Find(&out).Error
	return out, err
}
