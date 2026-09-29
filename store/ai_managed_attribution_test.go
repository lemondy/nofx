package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// 09-28 review P3: order sync ingests the WHOLE exchange account, so manual
// trades fold into trader_positions and would pollute the AI's loss-streak
// circuit breaker (a manual win resetting a forming 3-loss streak, a manual
// loss extending it). Rows must carry the AI-book attribution from the
// ai_managed_positions registry.
func TestPositionBuilderStampsAIManagedAtOpen(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "attr.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	traderID := "t1"
	pb := NewPositionBuilder(st.Position())

	// AI open path marks the registry BEFORE placing the order — simulate.
	if err := st.AIManaged().Mark(traderID, "AAAUSDT", "long"); err != nil {
		t.Fatal(err)
	}
	if err := pb.ProcessTrade(traderID, "ex", "binance", "AAAUSDT", "LONG", "open_long",
		2, 100, 0.01, 0, 1000, "9001"); err != nil {
		t.Fatal(err)
	}
	// Manual position: never marked.
	if err := pb.ProcessTrade(traderID, "ex", "binance", "BBBUSDT", "LONG", "open_long",
		2, 50, 0.01, 0, 1000, "9002"); err != nil {
		t.Fatal(err)
	}

	aiRow, err := st.Position().GetOpenPositionBySymbol(traderID, "AAAUSDT", "LONG")
	if err != nil || aiRow == nil {
		t.Fatalf("AI row missing: %v", err)
	}
	if !aiRow.AIManaged {
		t.Fatal("row created while the registry mark exists must be stamped AI-managed")
	}
	manualRow, err := st.Position().GetOpenPositionBySymbol(traderID, "BBBUSDT", "LONG")
	if err != nil || manualRow == nil {
		t.Fatalf("manual row missing: %v", err)
	}
	if manualRow.AIManaged {
		t.Fatal("unmarked (manual) position must NOT be stamped AI-managed")
	}

	// Manual fills averaging INTO an AI position keep the first-open
	// ownership (the merge path must not flip the flag).
	if err := pb.ProcessTrade(traderID, "ex", "binance", "AAAUSDT", "LONG", "open_long",
		1, 105, 0.01, 0, 1100, "9003"); err != nil {
		t.Fatal(err)
	}
	merged, err := st.Position().GetOpenPositionBySymbol(traderID, "AAAUSDT", "LONG")
	if err != nil || merged == nil {
		t.Fatalf("merged row missing: %v", err)
	}
	if !merged.AIManaged {
		t.Fatal("averaging into an AI position must keep the AI stamp")
	}
}

// Close-time re-check: the registry mark can still exist when OrderSync
// closes a row the open-time stamp missed (seedAIManagedOnce's pre-existing
// positions) — the full-close path must stamp it then.
func TestPositionBuilderStampsAIManagedAtClose(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "attr2.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	traderID := "t1"
	pb := NewPositionBuilder(st.Position())
	// Row created BEFORE the registry mark existed (seed-era position).
	if err := pb.ProcessTrade(traderID, "ex", "binance", "CCCUSDT", "LONG", "open_long",
		2, 100, 0.01, 0, 1000, "9101"); err != nil {
		t.Fatal(err)
	}
	row, _ := st.Position().GetOpenPositionBySymbol(traderID, "CCCUSDT", "LONG")
	if row == nil || row.AIManaged {
		t.Fatalf("pre-mark row must start unstamped, got %+v", row)
	}
	if err := st.AIManaged().Mark(traderID, "CCCUSDT", "long"); err != nil {
		t.Fatal(err)
	}
	if err := pb.ProcessTrade(traderID, "ex", "binance", "CCCUSDT", "LONG", "close_long",
		2, 110, 0.01, 20, 2000, "9102"); err != nil {
		t.Fatal(err)
	}
	closed, err := st.Position().GetClosedPositions(traderID, 10)
	if err != nil || len(closed) == 0 {
		t.Fatalf("closed row missing: %v", err)
	}
	if !closed[0].AIManaged {
		t.Fatal("close-time registry mark must stamp the closing row as AI-managed")
	}
}

// 09-29 user report (BTWUSDT): the orphan-reconcile pass closed rows with 0
// PnL while their fills were still in flight. The backfill must
// re-attribute late fills to a just-reconciled row — and must NOT touch
// rows closed normally ('sync') or outside the 30-minute window.
func TestBackfillReconciledPnL(t *testing.T) {
	st, err := NewWithConfig(DBConfig{Type: DBTypeSQLite, Path: filepath.Join(t.TempDir(), "bfl.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().UnixMilli()
	if err := st.gdb.Create(&TraderPosition{
		TraderID: "t1", Symbol: "AAAUSDT", Side: "LONG",
		Quantity: 1, EntryPrice: 100, ExitPrice: 110,
		RealizedPnL: 0, Fee: 0.01, Status: "CLOSED", CloseReason: "netting_reconcile",
		EntryTime: now - 3600_000, ExitTime: now - 60_000, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// A normally-synced row must never be backfilled.
	if err := st.gdb.Create(&TraderPosition{
		TraderID: "t1", Symbol: "BBBUSDT", Side: "LONG",
		Quantity: 1, EntryPrice: 50, ExitPrice: 55,
		RealizedPnL: 5, Fee: 0.02, Status: "CLOSED", CloseReason: "sync",
		EntryTime: now - 3600_000, ExitTime: now - 60_000, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if !st.Position().BackfillReconciledPnL("t1", "AAAUSDT", "LONG", 13.66, 0.05, 111.9, now-30_000) {
		t.Fatal("late fill must re-attribute to the reconciled row")
	}
	row, _ := st.Position().GetClosedPositions("t1", 10)
	var got *TraderPosition
	for _, r := range row {
		if r.Symbol == "AAAUSDT" {
			got = r
		}
	}
	if got == nil {
		t.Fatal("row vanished")
	}
	if math.Abs(got.RealizedPnL-13.66) > 1e-9 || math.Abs(got.Fee-0.06) > 1e-9 || math.Abs(got.ExitPrice-111.9) > 1e-9 || got.CloseReason != "sync" {
		t.Fatalf("backfill wrong: pnl %.4f fee %.4f exit %.4f reason %s", got.RealizedPnL, got.Fee, got.ExitPrice, got.CloseReason)
	}

	// A second late fill re-attributes on top (accumulates), same row.
	if !st.Position().BackfillReconciledPnL("t1", "AAAUSDT", "LONG", 1.0, 0.01, 111.9, now-20_000) {
		t.Fatal("second late fill must also re-attribute")
	}
	row2, _ := st.Position().GetClosedPositions("t1", 10)
	for _, r := range row2 {
		if r.Symbol == "AAAUSDT" && math.Abs(r.RealizedPnL-14.66) > 1e-9 {
			t.Fatalf("PnL must accumulate, got %.4f", r.RealizedPnL)
		}
	}

	// A late fill on a normally-synced row (closed <30min ago) also
	// re-attributes — a fill reaching the no-OPEN-row branch was never
	// booked anywhere, so accumulation is safe there too.
	if !st.Position().BackfillReconciledPnL("t1", "BBBUSDT", "LONG", 9, 0, 60, now) {
		t.Fatal("late fill on a recently synced row must re-attribute to it")
	}
}
