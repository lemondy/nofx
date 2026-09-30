package store

import (
	"fmt"
	"gorm.io/gorm"
)

// A legacy model accidentally made exchange_order_id globally unique. Repair
// only that exact named index; preserve all data and unrelated indexes.
func repairOrderIndexes(db *gorm.DB) error {
	for _, item := range []struct {
		model interface{}
		name  string
	}{
		{&TraderOrder{}, "idx_orders_exchange_unique"},
		{&TraderFill{}, "idx_fills_exchange_unique"},
	} {
		if !db.Migrator().HasTable(item.model) {
			continue
		}
		indexes, err := db.Migrator().GetIndexes(item.model)
		if err != nil {
			return fmt.Errorf("inspect order indexes: %w", err)
		}
		for _, index := range indexes {
			if index.Name() == item.name && len(index.Columns()) != 2 {
				if err := db.Transaction(func(tx *gorm.DB) error {
					if err := tx.Migrator().DropIndex(item.model, item.name); err != nil {
						return err
					}
					return tx.Migrator().CreateIndex(item.model, item.name)
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
