package store

import (
	"path/filepath"
	"testing"
	"time"
)

// newTestStore builds a throwaway SQLite store per test.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewWithConfig(DBConfig{
		Type: DBTypeSQLite,
		Path: filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// Pending-entry shadow rows: upsert (replace semantics per trader+symbol+side),
// list per trader, delete. One trader must never see another's rows.
func TestPendingEntryStoreRoundTrip(t *testing.T) {
	st := newTestStore(t)

	rowA := &PendingEntryDB{
		TraderID: "trader-A", Symbol: "CLUSDT", Side: "long",
		Price: 94.74, Quantity: 0.51, StopLoss: 92.69, TakeProfit: 98.84,
		Leverage: 3, OrderID: "4214529964", PlacedAt: time.Now(),
	}
	if err := st.PendingEntry().Upsert(rowA); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	// Replace semantics: same trader+symbol+side, new order supersedes.
	rowA2 := &PendingEntryDB{
		TraderID: "trader-A", Symbol: "CLUSDT", Side: "long",
		Price: 94.93, Quantity: 0.56, StopLoss: 93.05, TakeProfit: 98.7,
		Leverage: 3, OrderID: "4215035513", PlacedAt: time.Now(),
	}
	if err := st.PendingEntry().Upsert(rowA2); err != nil {
		t.Fatalf("upsert A2: %v", err)
	}
	rowAShort := &PendingEntryDB{
		TraderID: "trader-A", Symbol: "CLUSDT", Side: "short",
		Price: 97, Quantity: 0.4, StopLoss: 99, TakeProfit: 93,
		Leverage: 3, OrderID: "short-1", PlacedAt: time.Now(),
	}
	if err := st.PendingEntry().Upsert(rowAShort); err != nil {
		t.Fatalf("upsert opposite side: %v", err)
	}
	// Second trader, isolated.
	rowB := &PendingEntryDB{
		TraderID: "trader-B", Symbol: "WLDUSDT", Side: "short",
		Price: 0.47, Quantity: 10, StopLoss: 0.49, TakeProfit: 0.44,
		OrderID: "999", PlacedAt: time.Now(),
	}
	if err := st.PendingEntry().Upsert(rowB); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	got, err := st.PendingEntry().List("trader-A")
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("both sides must coexist: %+v", got)
	}
	bySide := map[string]*PendingEntryDB{}
	for _, row := range got {
		bySide[row.Side] = row
	}
	if bySide["long"] == nil || bySide["long"].OrderID != "4215035513" || bySide["long"].Price != 94.93 || bySide["short"] == nil || bySide["short"].OrderID != "short-1" {
		t.Fatalf("replace must affect only same side: %+v", got)
	}

	gotB, err := st.PendingEntry().List("trader-B")
	if err != nil || len(gotB) != 1 {
		t.Fatalf("list B: %v %+v", err, gotB)
	}

	if err := st.PendingEntry().Delete("trader-A", "CLUSDT", "long"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ = st.PendingEntry().List("trader-A")
	if len(got) != 1 || got[0].Side != "short" {
		t.Fatalf("delete must preserve opposite side: %+v", got)
	}
	if gotB, _ := st.PendingEntry().List("trader-B"); len(gotB) != 1 {
		t.Fatal("delete of A must not touch B")
	}
}

func TestPendingEntryMigratesLegacyUniqueIndex(t *testing.T) {
	st := newTestStore(t)
	if err := st.gdb.Migrator().DropIndex(&PendingEntryDB{}, "idx_pending_trader_symbol_side"); err != nil {
		t.Fatalf("drop new index: %v", err)
	}
	if err := st.gdb.Exec("CREATE UNIQUE INDEX idx_pending_trader_symbol ON trader_pending_entries(trader_id, symbol)").Error; err != nil {
		t.Fatalf("create legacy index: %v", err)
	}
	if err := st.PendingEntry().Upsert(&PendingEntryDB{
		TraderID: "trader-A", Symbol: "SOLUSDT", Side: "long", OrderID: "old-long",
	}); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := st.PendingEntry().initTables(); err != nil {
		t.Fatalf("migrate pending entries: %v", err)
	}
	if st.gdb.Migrator().HasIndex(&PendingEntryDB{}, "idx_pending_trader_symbol") ||
		!st.gdb.Migrator().HasIndex(&PendingEntryDB{}, "idx_pending_trader_symbol_side") {
		t.Fatal("legacy index must be replaced by per-side unique index")
	}
	if err := st.PendingEntry().Upsert(&PendingEntryDB{
		TraderID: "trader-A", Symbol: "SOLUSDT", Side: "short", OrderID: "new-short",
	}); err != nil {
		t.Fatalf("insert opposite side after migration: %v", err)
	}
	got, err := st.PendingEntry().List("trader-A")
	if err != nil || len(got) != 2 {
		t.Fatalf("migration must preserve old row and allow opposite side: %v %+v", err, got)
	}
}
