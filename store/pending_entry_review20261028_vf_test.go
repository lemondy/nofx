package store

import (
	"testing"
	"time"
)

// Existing SQLite rows predate the state / client_order_id / original_r
// columns. The additive AutoMigrate must keep them readable (NULLs → zero
// values) and writable, and new intent rows must round-trip their fields.
func TestPendingEntryLegacyRowsSurviveAdditiveMigration(t *testing.T) {
	st := newTestStore(t)
	db := st.GormDB()
	if err := db.Exec(`DROP TABLE trader_pending_entries`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE trader_pending_entries (
		id integer PRIMARY KEY AUTOINCREMENT, trader_id text, symbol text, side text(8),
		price real, quantity real, stop_loss real, take_profit real, leverage integer,
		order_id text(64), placed_at datetime, exit_mode text,
		recovery_reason text, executed_qty real DEFAULT 0, protected_qty real DEFAULT 0)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE UNIQUE INDEX idx_pending_trader_symbol_side ON trader_pending_entries(trader_id, symbol, side)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO trader_pending_entries (trader_id, symbol, side, price, quantity, stop_loss, take_profit, leverage, order_id, placed_at)
		VALUES ('t', 'XUSDT', 'long', 100, 1, 95, 115, 3, '12345', ?)`, time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.PendingEntry().initTables(); err != nil {
		t.Fatalf("additive migration failed: %v", err)
	}
	rows, err := st.PendingEntry().List("t")
	if err != nil || len(rows) != 1 {
		t.Fatalf("legacy row unreadable after migration: rows=%d err=%v", len(rows), err)
	}
	if r := rows[0]; r.OrderID != "12345" || r.State != "" || r.ClientOrderID != "" || r.OriginalR != 0 || r.StopLoss != 95 {
		t.Fatalf("legacy row changed by migration: %+v", r)
	}
	// New intent upgrades the same slot in place.
	if err := st.PendingEntry().Upsert(&PendingEntryDB{TraderID: "t", Symbol: "XUSDT", Side: "long", State: "submitting",
		ClientOrderID: "x-KzrpZaP9lim-abcd-1", OriginalR: 5, OrderID: "client:x-KzrpZaP9lim-abcd-1", Price: 100, Quantity: 1, PlacedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.PendingEntry().List("t")
	if len(rows) != 1 || rows[0].State != "submitting" || rows[0].ClientOrderID != "x-KzrpZaP9lim-abcd-1" || rows[0].OriginalR != 5 {
		t.Fatalf("intent row did not round-trip: %+v", rows)
	}
}
