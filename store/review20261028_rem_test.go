package store

import (
	"path/filepath"
	"testing"
)

func TestReview20261028RemTPStateMigrationAndScope(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "migration.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row := &TraderPosition{TraderID: "t", Symbol: "X", Side: "LONG", EntryPrice: 123, Quantity: 10, EntryQuantity: 10, Status: "OPEN", Fee: 4, R1TrimDone: true}
	if err := st.Position().CreateOpenPosition(row); err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-change schema with an existing populated position.
	if err := st.gdb.Migrator().DropColumn(&TraderPosition{}, "TPProtectionState"); err != nil {
		t.Fatal(err)
	}
	if err := st.Position().InitTables(); err != nil {
		t.Fatal(err)
	}
	if err := ensureColumns(st.gdb, &TraderPosition{}); err != nil {
		t.Fatal(err)
	}
	p, err := st.Position().GetOpenPositionBySymbol("t", "X", "LONG")
	if err != nil || p == nil || p.EntryPrice != 123 || p.Quantity != 10 || p.Fee != 4 || !p.R1TrimDone || p.TPProtectionState != "" {
		t.Fatalf("legacy row changed: %+v %v", p, err)
	}
	for _, p := range []*TraderPosition{
		{TraderID: "t", Symbol: "X", Side: "SHORT", EntryPrice: 123, Quantity: 10, Status: "OPEN"},
		{TraderID: "other", Symbol: "X", Side: "LONG", EntryPrice: 123, Quantity: 10, Status: "OPEN"},
		{TraderID: "t", Symbol: "X", Side: "LONG", EntryPrice: 123, Quantity: 10, Status: "CLOSED"},
	} {
		if err := st.gdb.Create(p).Error; err != nil {
			t.Fatal(err)
		}
	}
	state := `{"runner_done":true,"pos_qty":5,"tp_qty":5}`
	if err := st.Position().SetTPProtectionState("t", "X", "long", state); err != nil {
		t.Fatal(err)
	}
	var rows []TraderPosition
	if err := st.gdb.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		want := ""
		if p.ID == row.ID {
			want = state
		}
		if p.TPProtectionState != want {
			t.Fatalf("state leaked across side/trader/lifecycle: %+v", p)
		}
	}
}
