package store

import (
	"path/filepath"
	"testing"
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
