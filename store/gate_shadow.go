package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// ============================================================================
// Gate shadow blocks (影子拦截记录, 2026-09-21 user directive): every time a
// hard-entry gate blocks a direction that HAD a complete would-be trade
// (entry / stop_plan / structural TP), the counterfactual is recorded here
// and evaluated against the price path once the horizon matures — turning
// the numeric thresholds (min_rr, consensus 50, vendor 1%) from theory into
// calibratable data: would the blocked trades have won?
// ============================================================================

type GateShadowBlock struct {
	ID           uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	TraderID     string    `gorm:"column:trader_id;index:idx_gshadow_key" json:"trader_id"`
	Symbol       string    `gorm:"column:symbol;index:idx_gshadow_key" json:"symbol"`
	Direction    string    `gorm:"column:direction;size:8;index:idx_gshadow_key" json:"direction"` // long / short
	CycleNumber  int       `gorm:"column:cycle_number" json:"cycle_number"`
	BlockedCodes string    `gorm:"column:blocked_codes;size:256" json:"blocked_codes"` // comma-joined machine codes
	EntryPrice   float64   `gorm:"column:entry_price" json:"entry_price"`
	StopPrice    float64   `gorm:"column:stop_price" json:"stop_price"`
	TakeProfit   float64   `gorm:"column:take_profit" json:"take_profit"`
	PlanRR       float64   `gorm:"column:plan_rr" json:"plan_rr"` // |tp-entry|/|entry-sl|
	HorizonHours int       `gorm:"column:horizon_hours" json:"horizon_hours"`
	CreatedAt    time.Time `gorm:"column:created_at;index" json:"created_at"`
	Outcome      string    `gorm:"column:outcome;size:16;index" json:"outcome"` // "" | tp_first | sl_first | timeout | no_data
	ExitPrice    float64   `gorm:"column:exit_price" json:"exit_price"`
	EvaluatedAt  time.Time `gorm:"column:evaluated_at" json:"evaluated_at"`
	// Second horizon (E1, QUANT_REVIEW 09-22): 8h resolves mostly `timeout`
	// for structural TPs (live data: 39/56 timeout, 2 tp_first) — the 48h
	// pass lets the same counterfactual play out far enough for the
	// tp_first/sl_first split to mean something. Same verdict vocabulary.
	Outcome48     string    `gorm:"column:outcome_48h;size:16" json:"outcome_48h"` // "" | tp_first | sl_first | timeout | no_data
	ExitPrice48   float64   `gorm:"column:exit_price_48h" json:"exit_price_48h"`
	EvaluatedAt48 time.Time `gorm:"column:evaluated_at_48h" json:"evaluated_at_48h"`
}

func (GateShadowBlock) TableName() string { return "gate_shadow_blocks" }

type GateShadowStore struct {
	db *gorm.DB
}

func NewGateShadowStore(db *gorm.DB) *GateShadowStore {
	return &GateShadowStore{db: db}
}

func (s *GateShadowStore) initTables() error {
	return s.db.AutoMigrate(&GateShadowBlock{})
}

// CreateIfIdle writes the counterfactual unless an UNEVALUATED row already
// exists for the same trader+symbol+direction — one open shadow per key, so
// a symbol blocked for days doesn't stack hundreds of identical rows.
func (s *GateShadowStore) CreateIfIdle(rec *GateShadowBlock) (bool, error) {
	var existing GateShadowBlock
	err := s.db.Where("trader_id = ? AND symbol = ? AND direction = ? AND outcome = ''",
		rec.TraderID, rec.Symbol, rec.Direction).First(&existing).Error
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	return true, s.db.Create(rec).Error
}

// ListMatured returns unevaluated rows whose horizon has passed.
func (s *GateShadowStore) ListMatured(traderID string, now time.Time) ([]*GateShadowBlock, error) {
	var rows []*GateShadowBlock
	err := s.db.Where("trader_id = ? AND outcome = '' AND created_at < ?",
		traderID, now).Order("created_at ASC").Limit(50).Find(&rows).Error
	return rows, err
}

func (s *GateShadowStore) MarkEvaluated(id uint, outcome string, exitPrice float64, at time.Time) error {
	return s.db.Model(&GateShadowBlock{}).Where("id = ?", id).Updates(map[string]interface{}{
		"outcome": outcome, "exit_price": exitPrice, "evaluated_at": at,
	}).Error
}

// ListMatured48 returns rows whose 8h verdict exists but whose 48h pass is
// still pending and due.
func (s *GateShadowStore) ListMatured48(traderID string, now time.Time) ([]*GateShadowBlock, error) {
	var rows []*GateShadowBlock
	err := s.db.Where("trader_id = ? AND outcome != '' AND outcome_48h = '' AND created_at < ?",
		traderID, now).Order("created_at ASC").Limit(50).Find(&rows).Error
	return rows, err
}

func (s *GateShadowStore) MarkEvaluated48(id uint, outcome string, exitPrice float64, at time.Time) error {
	return s.db.Model(&GateShadowBlock{}).Where("id = ?", id).Updates(map[string]interface{}{
		"outcome_48h": outcome, "exit_price_48h": exitPrice, "evaluated_at_48h": at,
	}).Error
}

// ListEvaluated returns evaluated rows for offline aggregation.
func (s *GateShadowStore) ListEvaluated(traderID string, limit int) ([]*GateShadowBlock, error) {
	var rows []*GateShadowBlock
	err := s.db.Where("trader_id = ? AND outcome != ''", traderID).
		Order("created_at DESC").Limit(limit).Find(&rows).Error
	return rows, err
}
