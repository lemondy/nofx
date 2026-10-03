package store

import (
	"path/filepath"
	"testing"
	"time"
)

// Deterministic fill-sync -> later ownership mark -> exchange closure cleanup
// -> close-fill sync. All storage is in a temporary SQLite database.
func TestReview03OwnershipMustSurviveCloseBeforeSync(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "review.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pb := NewPositionBuilder(st.Position())
	now := time.Now().UnixMilli()
	if err := pb.ProcessTrade("t1", "fake", "binance", "XUSDT", "LONG", "open_long", 1, 100, 0.1, 0, now, "entry"); err != nil {
		t.Fatal(err)
	}
	if err := st.AIManaged().Mark("t1", "XUSDT", "long"); err != nil {
		t.Fatal(err)
	}
	if err := st.AIManaged().Unmark("t1", "XUSDT", "long"); err != nil {
		t.Fatal(err)
	}
	if err := pb.ProcessTrade("t1", "fake", "binance", "XUSDT", "LONG", "close_long", 1, 95, 0.1, -5, now+1000, "exit"); err != nil {
		t.Fatal(err)
	}
	var row TraderPosition
	if err := st.gdb.Where("trader_id = ? AND status = ?", "t1", "CLOSED").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if !row.AIManaged {
		t.Fatal("AI fill synced before ownership mark stayed manual after mark was removed before close-fill sync; loss streak excludes this loss")
	}
}

func TestEntryProvenanceSurvivesCleanupBeforeOpenSyncAndLateMark(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "provenance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pb := NewPositionBuilder(st.Position())
	now := time.Now().UnixMilli()
	if err := st.AIManaged().MarkEntry("t", "XUSDT", "long", "ai-entry"); err != nil {
		t.Fatal(err)
	}
	if err := st.AIManaged().Unmark("t", "XUSDT", "long"); err != nil {
		t.Fatal(err)
	}
	if err := pb.ProcessTrade("t", "fake", "binance", "XUSDT", "LONG", "open_long", 1, 100, 0, 0, now, "ai-entry"); err != nil {
		t.Fatal(err)
	}
	if err := pb.ProcessTrade("t", "fake", "binance", "XUSDT", "LONG", "close_long", 1, 95, 0, -5, now+1, "exit"); err != nil {
		t.Fatal(err)
	}
	if err := pb.ProcessTrade("t", "fake", "binance", "XUSDT", "LONG", "open_long", 1, 100, 0, 0, now+2, "manual-entry"); err != nil {
		t.Fatal(err)
	}
	if err := pb.ProcessTrade("t", "fake", "binance", "XUSDT", "LONG", "close_long", 1, 105, 0, 5, now+3, "manual-exit"); err != nil {
		t.Fatal(err)
	}
	if err := st.AIManaged().MarkEntry("t", "XUSDT", "long", "ai-entry"); err != nil {
		t.Fatal(err)
	}
	var rows []TraderPosition
	if err := st.gdb.Where("trader_id = ?", "t").Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !rows[0].AIManaged || rows[1].AIManaged {
		t.Fatalf("order provenance=%+v", rows)
	}
}

func TestJournalLegacySchemaAddsOwnershipColumnWithoutDataLoss(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.gdb.Exec("DROP TABLE trade_journal").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.gdb.Exec("CREATE TABLE trade_journal (id INTEGER PRIMARY KEY, trader_id TEXT, position_id INTEGER, symbol TEXT, side TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.gdb.Exec("INSERT INTO trade_journal (id,trader_id,position_id,symbol,side) VALUES (1,'t',2,'XUSDT','LONG')").Error; err != nil {
		t.Fatal(err)
	}
	// The same additive migrator is called for existing PostgreSQL journal tables.
	if err := ensureColumns(st.gdb, &TradeJournalDB{}); err != nil {
		t.Fatal(err)
	}
	if !st.gdb.Migrator().HasColumn(&TradeJournalDB{}, "ai_managed") {
		t.Fatal("new ownership column absent")
	}
	var row TradeJournalDB
	if err := st.gdb.First(&row, 1).Error; err != nil {
		t.Fatal(err)
	}
	if row.Symbol != "XUSDT" || row.AIManaged != nil {
		t.Fatalf("legacy row corrupted: %+v", row)
	}
	if err := ensureColumns(st.gdb, &TradeJournalDB{}); err != nil {
		t.Fatal("migration not idempotent", err)
	}
}

func TestStrategySaveReturnsItsExactWrittenVersion(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row := &Strategy{ID: "s", UserID: "u", Config: "{}", Name: "original", UpdatedAt: time.Now().UTC()}
	if err := st.Strategy().Create(row); err != nil {
		t.Fatal(err)
	}
	first := *row
	first.Name = "first"
	if err := st.Strategy().Update(&first, row.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	version := first.UpdatedAt
	second := first
	second.Name = "second"
	if err := st.Strategy().Update(&second, version); err != nil {
		t.Fatal(err)
	}
	if first.UpdatedAt != version || version == row.UpdatedAt || version == second.UpdatedAt {
		t.Fatal("response token drifted from committed write")
	}
	if err := st.Strategy().Update(&first, version); err != ErrStrategyConflict {
		t.Fatalf("stale second save accepted: %v", err)
	}
}

func TestLiveOrderMarkDoesNotClaimManualEntry(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "manual.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AIManaged().MarkEntry("t", "XUSDT", "long", "ai-order"); err != nil {
		t.Fatal(err)
	}
	pb := NewPositionBuilder(st.Position())
	now := time.Now().UnixMilli()
	if err := pb.ProcessTrade("t", "fake", "binance", "XUSDT", "LONG", "open_long", 1, 100, 0, 0, now, "manual-order"); err != nil {
		t.Fatal(err)
	}
	if err := pb.ProcessTrade("t", "fake", "binance", "XUSDT", "LONG", "close_long", 1, 105, 0, 5, now+1, "manual-close"); err != nil {
		t.Fatal(err)
	}
	var row TraderPosition
	if err := st.gdb.Where("entry_order_id = ?", "manual-order").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.AIManaged {
		t.Fatal("live AI mark claimed unrelated manual order")
	}
}
