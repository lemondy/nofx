package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// StockWeekHalt persists the us_stock weekly drawdown circuit so a restart or
// config reload cannot lift it before the next ET week. One row per
// (trader, mode); Mode is "paper" or "live" so a mode switch never inherits the
// other mode's circuit. WeekKey is the ET Monday (YYYY-MM-DD) the circuit
// tripped in: it is in force only while the current ET week has the same key.
type StockWeekHalt struct {
	TraderID    string  `gorm:"column:trader_id;primaryKey" json:"trader_id"`
	Mode        string  `gorm:"column:mode;primaryKey" json:"mode"`
	WeekKey     string  `gorm:"column:week_key;not null" json:"week_key"`
	StartEquity float64 `gorm:"column:start_equity" json:"start_equity"` // the week's baseline the drawdown was measured against
	HaltedAt    int64   `gorm:"column:halted_at" json:"halted_at"`       // Unix ms UTC
}

func (StockWeekHalt) TableName() string { return "stock_week_halts" }

// SetWeekHalt records (or replaces) the tripped circuit of one trader+mode.
func (s *StockPaperStore) SetWeekHalt(traderID, mode, weekKey string, startEquity float64, at time.Time) error {
	row := &StockWeekHalt{TraderID: traderID, Mode: mode, WeekKey: weekKey, StartEquity: startEquity, HaltedAt: at.UTC().UnixMilli()}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "trader_id"}, {Name: "mode"}},
		DoUpdates: clause.AssignmentColumns([]string{"week_key", "start_equity", "halted_at"}),
	}).Create(row).Error
}

// GetWeekHalt returns the stored circuit of a trader+mode, or (nil, nil) when none.
func (s *StockPaperStore) GetWeekHalt(traderID, mode string) (*StockWeekHalt, error) {
	var row StockWeekHalt
	err := s.db.Where("trader_id = ? AND mode = ?", traderID, mode).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// ClearWeekHalt forgets the circuit of a trader+mode (no error when absent).
func (s *StockPaperStore) ClearWeekHalt(traderID, mode string) error {
	return s.db.Where("trader_id = ? AND mode = ?", traderID, mode).Delete(&StockWeekHalt{}).Error
}
