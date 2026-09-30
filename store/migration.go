package store

import "gorm.io/gorm"

// Preserve legacy PostgreSQL column types while adding new model fields.
// Unlike the former table-exists shortcut, this upgrades existing databases.
func ensureColumns(db *gorm.DB, models ...interface{}) error {
	for _, model := range models {
		if !db.Migrator().HasTable(model) {
			if err := db.AutoMigrate(model); err != nil {
				return err
			}
			continue
		}
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(model); err != nil {
			return err
		}
		for _, field := range stmt.Schema.Fields {
			if field.DBName != "" && !db.Migrator().HasColumn(model, field.DBName) {
				if err := db.Migrator().AddColumn(model, field.Name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
