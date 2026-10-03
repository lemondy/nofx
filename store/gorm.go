package store

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// GormDB is the global GORM database connection
var gormDB *gorm.DB

// DB returns the GORM database connection
func DB() *gorm.DB {
	return gormDB
}

// InitGorm initializes GORM with SQLite
//
// 2026-10-01 (user report "策略保存经常失败、进策略页转圈很久"): the pool was
// MaxOpenConns(1) with journal_mode=DELETE + synchronous=FULL — every API
// read, the order-sync writer (30s), the protection monitor (30s), equity
// snapshots and ~150KB decision rows serialized through ONE connection with
// whole-file exclusive locks and double fsync per write. Saves hit
// SQLITE_BUSY after the 5s busy_timeout ("保存失败") and page reads queued
// behind write bursts (spinner). WAL makes readers and the writer mutually
// non-blocking; NORMAL sync in WAL is crash-safe for app restarts; a small
// pool lets reads run in parallel (SQLite still serializes writers, and
// busy_timeout covers the contention window).
func InitGorm(dbPath string) (*gorm.DB, error) {
	// Per-connection pragmas ride the DSN so EVERY pooled connection gets
	// them (pool >1 makes the old one-shot db.Exec pragmas insufficient —
	// foreign_keys and journal mode are per-connection in SQLite).
	if strings.TrimSpace(dbPath) == "" {
		// 2026-10-03 review: an empty path makes SQLite open a throwaway
		// in-memory DB — total silent data "loss". Fail fast instead.
		return nil, fmt.Errorf("sqlite path is empty")
	}
	if strings.Contains(dbPath, "?") && !strings.HasPrefix(dbPath, "file:") {
		// mattn splits a PLAIN path at the first "?" and swallows the rest as
		// params — the app would silently open the wrong file.
		return nil, fmt.Errorf("sqlite path %q contains '?' — use a file: URI or rename the file", dbPath)
	}
	dsn := dbPath
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn = dsn + sep + "_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
		// Use UTC for all auto-generated timestamps (autoCreateTime, autoUpdateTime)
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}

	// Connection pool for SQLite: a few connections so API reads don't queue
	// behind background writers. Writers still serialize inside SQLite.
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(4)

	gormDB = db
	return db, nil
}

// InitGormPostgres initializes GORM with PostgreSQL
func InitGormPostgres(host string, port int, user, password, dbname, sslmode string) (*gorm.DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		host, port, user, password, dbname, sslmode,
	)

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
		// Use UTC for all auto-generated timestamps (autoCreateTime, autoUpdateTime)
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open PostgreSQL database: %w", err)
	}

	// Set connection pool for PostgreSQL
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(5)

	gormDB = db
	return db, nil
}

// InitGormWithConfig initializes GORM with provided configuration
// Uses DBConfig from driver.go
func InitGormWithConfig(cfg DBConfig) (*gorm.DB, error) {
	switch cfg.Type {
	case DBTypeSQLite:
		return InitGorm(cfg.Path)

	case DBTypePostgres:
		return InitGormPostgres(
			cfg.Host,
			cfg.Port,
			cfg.User,
			cfg.Password,
			cfg.DBName,
			cfg.SSLMode,
		)

	default:
		return nil, fmt.Errorf("unsupported DB_TYPE: %s (use 'sqlite' or 'postgres')", cfg.Type)
	}
}

// ============================================================================
// Query Scopes - Reusable query helpers
// ============================================================================

// ForUser returns a scope that filters by user_id
func ForUser(userID string) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		return db.Where("user_id = ?", userID)
	}
}

// ForTrader returns a scope that filters by trader_id
func ForTrader(traderID string) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		return db.Where("trader_id = ?", traderID)
	}
}

// OpenPositions returns a scope for open positions
func OpenPositions() func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		return db.Where("status = ?", "OPEN")
	}
}

// ClosedPositions returns a scope for closed positions
func ClosedPositions() func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		return db.Where("status = ?", "CLOSED")
	}
}

// OrderByCreatedDesc returns a scope that orders by created_at DESC
func OrderByCreatedDesc() func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		return db.Order("created_at DESC")
	}
}

// OrderByUpdatedDesc returns a scope that orders by updated_at DESC
func OrderByUpdatedDesc() func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		return db.Order("updated_at DESC")
	}
}

// Paginate returns a scope for pagination
func Paginate(limit, offset int) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		return db.Limit(limit).Offset(offset)
	}
}
