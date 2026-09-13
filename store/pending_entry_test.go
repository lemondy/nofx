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

// Pending-entry shadow rows: upsert (replace semantics per trader+symbol),
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
	// Replace semantics: same trader+symbol, new order supersedes.
	rowA2 := &PendingEntryDB{
		TraderID: "trader-A", Symbol: "CLUSDT", Side: "long",
		Price: 94.93, Quantity: 0.56, StopLoss: 93.05, TakeProfit: 98.7,
		Leverage: 3, OrderID: "4215035513", PlacedAt: time.Now(),
	}
	if err := st.PendingEntry().Upsert(rowA2); err != nil {
		t.Fatalf("upsert A2: %v", err)
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
	if len(got) != 1 || got[0].OrderID != "4215035513" || got[0].Price != 94.93 {
		t.Fatalf("replace semantics broken: %+v", got)
	}

	gotB, err := st.PendingEntry().List("trader-B")
	if err != nil || len(gotB) != 1 {
		t.Fatalf("list B: %v %+v", err, gotB)
	}

	if err := st.PendingEntry().Delete("trader-A", "CLUSDT"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ = st.PendingEntry().List("trader-A")
	if len(got) != 0 {
		t.Fatalf("delete broken: %+v", got)
	}
	if gotB, _ := st.PendingEntry().List("trader-B"); len(gotB) != 1 {
		t.Fatal("delete of A must not touch B")
	}
}
