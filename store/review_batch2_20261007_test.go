package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// review 2026-10-07 B2-A/B2-B: use a private SQLite database without external services.
func batch2OrderStore(t *testing.T) *OrderStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "accounting.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.AutoMigrate(&TraderOrder{}, &TraderFill{}, &TraderPosition{}, &AIManagedPosition{}, &AIEntryOrder{}); err != nil {
		t.Fatal(err)
	}
	return NewOrderStore(db)
}

func batch2Trade(id, action string, qty, price, pnl, fee float64) (*TraderOrder, *TraderFill) {
	ms := time.Now().UTC().UnixMilli()
	return &TraderOrder{
			TraderID: "b2", ExchangeID: "account", ExchangeType: "binance", ExchangeOrderID: id,
			Symbol: "XUSDT", Side: "BUY", PositionSide: "LONG", Type: "MARKET", Status: "FILLED",
			OrderAction: action, Quantity: qty, Price: price, CreatedAt: ms,
		}, &TraderFill{
			TraderID: "b2", ExchangeID: "account", ExchangeType: "binance", ExchangeOrderID: "order-" + id,
			ExchangeTradeID: id, Symbol: "XUSDT", Side: "BUY", Quantity: qty, Price: price,
			QuoteQuantity: qty * price, RealizedPnL: pnl, Commission: fee, CommissionAsset: "USDT", CreatedAt: ms,
		}
}

// review 2026-10-07 B2-A: a conflict succeeds but must report zero inserted rows.
func TestB2FillInsertReportsConflict(t *testing.T) {
	s := batch2OrderStore(t)
	_, fill := batch2Trade("1", "open_long", 1, 100, 0, 0.1)
	if inserted, err := s.CreateFillIfAbsent(fill); err != nil || !inserted {
		t.Fatalf("first insert=%t err=%v", inserted, err)
	}
	_, duplicate := batch2Trade("1", "open_long", 1, 100, 0, 0.1)
	if inserted, err := s.CreateFillIfAbsent(duplicate); err != nil || inserted {
		t.Fatalf("duplicate insert=%t err=%v", inserted, err)
	}
}

// review 2026-10-07 B2-B: exercise every position write path, including no-OPEN backfill.
func TestB2TradeTransactionRollsBackPositionWrite(t *testing.T) {
	for _, path := range []string{"open", "partial", "full", "backfill"} {
		t.Run(path, func(t *testing.T) {
			s := batch2OrderStore(t)
			positions := NewPositionStore(s.db)
			qty, pnl := 1.0, 5.0
			action, operation := "close_long", "UPDATE"
			if path == "open" {
				action, operation, pnl = "open_long", "INSERT", 0
			} else {
				row := &TraderPosition{TraderID: "b2", ExchangeID: "account", Symbol: "XUSDT", Side: "LONG", Quantity: 2, EntryQuantity: 2, EntryPrice: 100, Status: "OPEN", Fee: 0.2}
				if path == "backfill" {
					row.Status, row.ExitTime, row.CloseReason = "CLOSED", time.Now().UnixMilli(), "netting_reconcile"
				}
				if err := s.db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
				if path == "full" {
					qty = 2
				}
			}
			if err := s.db.Exec("CREATE TRIGGER b2_fail BEFORE " + operation + " ON trader_positions BEGIN SELECT RAISE(ABORT, 'injected position failure'); END").Error; err != nil {
				t.Fatal(err)
			}
			order, fill := batch2Trade("2", action, qty, 105, pnl, 0.1)
			if applied, err := s.ApplyTrade(order, fill); err == nil || applied {
				t.Fatalf("failed position write committed: applied=%t err=%v", applied, err)
			}
			for _, model := range []interface{}{&TraderOrder{}, &TraderFill{}} {
				var count int64
				if err := s.db.Model(model).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("partial accounting receipt: count=%d err=%v", count, err)
				}
			}
			if err := s.db.Exec("DROP TRIGGER b2_fail").Error; err != nil {
				t.Fatal(err)
			}
			order, fill = batch2Trade("2", action, qty, 105, pnl, 0.1)
			if applied, err := s.ApplyTrade(order, fill); err != nil || !applied {
				t.Fatalf("retry did not commit: applied=%t err=%v", applied, err)
			}
			order, fill = batch2Trade("2", action, qty, 105, pnl, 0.1)
			if applied, err := s.ApplyTrade(order, fill); err != nil || applied {
				t.Fatalf("replay reapplied: applied=%t err=%v", applied, err)
			}
			var row TraderPosition
			if err := positions.db.First(&row).Error; err != nil {
				t.Fatal(err)
			}
			wantQty, wantFee := 2.0, 0.3
			if path == "open" {
				wantQty, wantFee = 1, 0.1
			} else if path == "partial" {
				wantQty = 1
			}
			if row.Quantity != wantQty || row.RealizedPnL != pnl || math.Abs(row.Fee-wantFee) > 1e-9 {
				t.Fatalf("retry accounting wrong: %+v", row)
			}
		})
	}
}

// review 2026-10-07 B2-B: an order left by the old split-write sync is not a receipt.
func TestB2OrderWithoutFillCanBeRepaired(t *testing.T) {
	s := batch2OrderStore(t)
	order, _ := batch2Trade("3", "open_long", 1, 100, 0, 0.1)
	if err := s.CreateOrder(order); err != nil {
		t.Fatal(err)
	}
	order, fill := batch2Trade("3", "open_long", 1, 100, 0, 0.1)
	if applied, err := s.ApplyTrade(order, fill); err != nil || !applied {
		t.Fatalf("orphan order skipped: applied=%t err=%v", applied, err)
	}
	var count int64
	if err := s.db.Model(&TraderOrder{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("order count=%d err=%v", count, err)
	}
}
