package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// ============================================================================
// Journal ownership (user request 09-27): every journal row must say whether
// the trade was AI-opened or manual. A trade counts as AI iff a SUCCESSFUL
// open/open_*_limit decision for the same symbol+side exists inside the
// ownership window (40 min before entry → +2 min). Pinned regressions:
//   - limit actions must match (plain open_long alone classified nearly
//     everything manual — the limit-entry default ships *_limit)
//   - rejected proposals (success=false, e.g. HANDS-OFF on a manual
//     position) must NOT attribute the trade to the AI (HYPE 09-27)
//   - BackfillOwnership stamps NULL rows + re-enriches empty planned data,
//     and never touches already-filled rows (idempotent)
// ============================================================================

func insertDecision(t *testing.T, st *Store, traderID, symbol, action string, success bool, at time.Time) {
	t.Helper()
	acts := []DecisionAction{{
		Action: action, Symbol: symbol, Success: success,
		Price: 100, StopLoss: 97, TakeProfit: 110, Confidence: 80,
		Reasoning: "test decision",
	}}
	raw, _ := json.Marshal(acts)
	if err := st.gdb.Create(&DecisionRecordDB{
		TraderID: traderID, CycleNumber: 1, Timestamp: at, Decisions: string(raw),
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func insertClosedPosition(t *testing.T, st *Store, traderID, symbol, side string, entry time.Time) {
	t.Helper()
	if err := st.gdb.Create(&TraderPosition{
		TraderID: traderID, Symbol: symbol, Side: side,
		Quantity: 1, EntryPrice: 100, ExitPrice: 105, RealizedPnL: 5,
		Leverage: 3, Status: "CLOSED", CloseReason: "test",
		EntryTime: entry.UnixMilli(), ExitTime: entry.Add(time.Hour).UnixMilli(),
		CreatedAt: entry.UnixMilli(), UpdatedAt: entry.Add(time.Hour).UnixMilli(),
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestJournalOwnershipLimitOpenCountsAsAI(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "own.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	trader := "t1"
	entry := time.Now().UTC().Add(-time.Hour)
	// AI limit open 10 min before the fill — the standard path.
	insertDecision(t, st, trader, "AAAUSDT", "open_long_limit", true, entry.Add(-10*time.Minute))
	insertClosedPosition(t, st, trader, "AAAUSDT", "LONG", entry)

	if _, err := st.TradeJournal().SyncFromPositions(trader); err != nil {
		t.Fatal(err)
	}
	rows, _, err := st.TradeJournal().List(trader, 10, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 journal row, got %d", len(rows))
	}
	if rows[0].AIManaged == nil || !*rows[0].AIManaged {
		t.Fatalf("limit open must classify as AI, got %+v", rows[0].AIManaged)
	}
	// The same decision also feeds planned SL/TP (the enrich path).
	if rows[0].PlannedStopLoss != 97 || rows[0].PlannedTakeProfit != 110 {
		t.Fatalf("planned SL/TP not enriched: %+v", rows[0])
	}
}

func TestJournalOwnershipRejectedProposalStaysManual(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "own2.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	trader := "t1"
	entry := time.Now().UTC().Add(-time.Hour)
	// AI PROPOSED an open but execution rejected it (HANDS-OFF on a manual
	// position — HYPE 09-27): success=false must not count.
	insertDecision(t, st, trader, "HYPEUSDT", "open_long_limit", false, entry.Add(-10*time.Minute))
	insertClosedPosition(t, st, trader, "HYPEUSDT", "LONG", entry)

	if _, err := st.TradeJournal().SyncFromPositions(trader); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := st.TradeJournal().List(trader, 10, 0, "", "")
	if len(rows) != 1 {
		t.Fatalf("want 1 journal row, got %d", len(rows))
	}
	if rows[0].AIManaged == nil || *rows[0].AIManaged {
		t.Fatalf("rejected proposal must classify as manual, got %+v", rows[0].AIManaged)
	}
}

func TestJournalOwnershipOutsideWindowStaysManual(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "own3.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	trader := "t1"
	entry := time.Now().UTC().Add(-time.Hour)
	// Decision 50 min before entry: beyond the 40-min ownership window (the
	// limit order would have expired — a manual fill, not this decision).
	insertDecision(t, st, trader, "BBBUSDT", "open_short_limit", true, entry.Add(-50*time.Minute))
	insertClosedPosition(t, st, trader, "BBBUSDT", "SHORT", entry)

	if _, err := st.TradeJournal().SyncFromPositions(trader); err != nil {
		t.Fatal(err)
	}
	rows, _, _ := st.TradeJournal().List(trader, 10, 0, "", "")
	if len(rows) != 1 || rows[0].AIManaged == nil || *rows[0].AIManaged {
		t.Fatalf("decision outside the window must classify as manual, got %+v", rows)
	}
}

func TestBackfillOwnershipStampsLegacyRowsAndReEnriches(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "own4.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	trader := "t1"
	entry := time.Now().UTC().Add(-2 * time.Hour)
	insertDecision(t, st, trader, "CCCUSDT", "open_long_limit", true, entry.Add(-5*time.Minute))

	// Legacy row: synced before the ownership column existed (NULL) with the
	// enrichment miss (empty planned data despite a matching decision).
	legacy := &TradeJournalDB{
		TraderID: trader, PositionID: 1, Symbol: "CCCUSDT", Side: "LONG",
		Quantity: 1, EntryPrice: 100, ExitPrice: 105, RealizedPnL: 5, PnLPct: 5,
		EntryTime: entry.UnixMilli(), ExitTime: entry.Add(time.Hour).UnixMilli(),
		CreatedAt: entry.UnixMilli(), UpdatedAt: entry.UnixMilli(),
	}
	if err := st.gdb.Create(legacy).Error; err != nil {
		t.Fatal(err)
	}

	n, err := st.TradeJournal().BackfillOwnership(trader)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("backfill stamped %d rows, want 1", n)
	}

	var got TradeJournalDB
	if err := st.gdb.First(&got, legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.AIManaged == nil || !*got.AIManaged {
		t.Fatalf("legacy row must classify as AI: %+v", got.AIManaged)
	}
	if got.PlannedStopLoss != 97 || got.Confidence != 80 {
		t.Fatalf("legacy row decision basis not re-enriched: planned SL %v conf %d", got.PlannedStopLoss, got.Confidence)
	}

	// Idempotent + never touches already-classified rows: a second run is a
	// no-op, and reviewed payloads survive.
	if n2, err := st.TradeJournal().BackfillOwnership(trader); err != nil || n2 != 0 {
		t.Fatalf("second backfill must be a no-op, got n=%d err=%v", n2, err)
	}
}
