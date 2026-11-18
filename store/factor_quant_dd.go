package store

import "gorm.io/gorm/clause"

// SetDrawdownTrim persists absolute quantity ceilings before any trim executes.
func (s *FactorQuantStore) SetDrawdownTrim(id string, applied, pending float64, targets string) error {
	row := &FactorQuantState{TraderID: id, AppliedDDScale: applied, PendingDDScale: pending, DDTrimTargets: targets}
	return s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "trader_id"}}, DoUpdates: clause.AssignmentColumns([]string{"applied_dd_scale", "pending_dd_scale", "dd_trim_targets"})}).Create(row).Error
}
