package trader

import (
	"path/filepath"
	"testing"

	"nofx/kernel"
	"nofx/store"
)

func profitLockTestTrader(t *testing.T, st *store.Store) *AutoTrader {
	t.Helper()
	return &AutoTrader{
		id:                      "trader-t1",
		name:                    "test",
		store:                   st,
		positionStopLoss:        make(map[string]float64),
		positionInitialStopLoss: make(map[string]float64),
	}
}

func profitLockTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.NewWithConfig(store.DBConfig{
		Type: store.DBTypeSQLite,
		Path: filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// ONDOUSDT 2026-09-17 regression: the 1R anchor must be the OPENING stop —
// AI tightening of the live stop (0.3428 → 0.3503 → 0.3512) must not lower
// the bar, so +0.49% no longer reads as 1.89R and trims half the position.
func TestInitialStopAnchorOpeningRisk(t *testing.T) {
	st := profitLockTestStore(t)
	at := profitLockTestTrader(t, st)

	// Trade sync creates the OPEN row first, as it does for limit fills.
	if err := st.Position().CreateOpenPosition(&store.TraderPosition{
		TraderID: "trader-t1", Symbol: "ONDOUSDT", Side: "LONG",
		Quantity: 144.8, EntryPrice: 0.3503, EntryTime: 1, Source: "sync",
	}); err != nil {
		t.Fatalf("create position: %v", err)
	}

	// Entry plan captured at open; AI then tightens the LIVE stop twice.
	at.SetInitialStopLoss("ONDOUSDT", "long", 0.3428)
	at.SetRecordedStopLoss("ONDOUSDT", "long", 0.3503)
	at.SetRecordedStopLoss("ONDOUSDT", "long", 0.3512)

	anchor := at.initialStopAnchor("ONDOUSDT", "long", 0.3512)
	if !nearly(anchor, 0.3428) {
		t.Fatalf("anchor = %.6g, want opening stop 0.3428", anchor)
	}

	// Mark 0.352 (+0.49%) vs opening risk 2.14% → 0.23R: no trim.
	_, trim := kernel.ProfitLockTargets("long", 0.3503, anchor, 0.3512, 0.352, 1.0)
	if trim {
		t.Fatal("tightened live stop pulled the 1R bar down — trim fired at +0.49%")
	}
	// True 1R ≈ 0.3578 → trim fires.
	_, trim = kernel.ProfitLockTargets("long", 0.3503, anchor, 0.3512, 0.3579, 1.0)
	if !trim {
		t.Fatal("true 1R must fire the trim")
	}

	// Write-once: both the DB row and the in-memory anchor resist rewrites.
	row, err := st.Position().GetOpenPositionBySymbol("trader-t1", "ONDOUSDT", "LONG")
	if err != nil || row == nil {
		t.Fatalf("open row lookup: %v", err)
	}
	if !nearly(row.InitialStopLoss, 0.3428) {
		t.Fatalf("persisted anchor = %.6g, want 0.3428", row.InitialStopLoss)
	}
	at.SetInitialStopLoss("ONDOUSDT", "long", 0.36)
	row, _ = st.Position().GetOpenPositionBySymbol("trader-t1", "ONDOUSDT", "LONG")
	if !nearly(row.InitialStopLoss, 0.3428) {
		t.Fatalf("anchor rewritten after set-once: %.6g", row.InitialStopLoss)
	}
}

// In-memory anchor set at open before trade sync creates the row: the scan's
// re-stamp must heal the row so the anchor survives a restart.
func TestInitialStopAnchorHealsLateRow(t *testing.T) {
	st := profitLockTestStore(t)
	at := profitLockTestTrader(t, st)

	at.SetInitialStopLoss("LITEUSDT", "long", 900.0) // row doesn't exist yet — map only
	if anchor := at.initialStopAnchor("LITEUSDT", "long", 908.0); !nearly(anchor, 900.0) {
		t.Fatalf("anchor = %.6g, want 900", anchor)
	}

	if err := st.Position().CreateOpenPosition(&store.TraderPosition{
		TraderID: "trader-t1", Symbol: "LITEUSDT", Side: "LONG",
		Quantity: 0.03, EntryPrice: 908.0, EntryTime: 2, Source: "sync",
	}); err != nil {
		t.Fatalf("create position: %v", err)
	}
	if anchor := at.initialStopAnchor("LITEUSDT", "long", 908.0); !nearly(anchor, 900.0) {
		t.Fatalf("anchor = %.6g, want 900", anchor)
	}
	row, _ := st.Position().GetOpenPositionBySymbol("trader-t1", "LITEUSDT", "LONG")
	if row == nil || !nearly(row.InitialStopLoss, 900.0) {
		t.Fatalf("late row not healed: %+v", row)
	}
}

// Pre-anchor (legacy) position: fall back to the live stop and freeze it, so
// the anchor is deterministic from first scan onward.
func TestInitialStopAnchorLegacyFallback(t *testing.T) {
	st := profitLockTestStore(t)
	at := profitLockTestTrader(t, st)

	if err := st.Position().CreateOpenPosition(&store.TraderPosition{
		TraderID: "trader-t1", Symbol: "AKEUSDT", Side: "SHORT",
		Quantity: 12.0, EntryPrice: 5.0, EntryTime: 3, Source: "sync",
	}); err != nil {
		t.Fatalf("create position: %v", err)
	}
	at.SetRecordedStopLoss("AKEUSDT", "short", 5.2)

	if anchor := at.initialStopAnchor("AKEUSDT", "short", 5.2); !nearly(anchor, 5.2) {
		t.Fatalf("legacy fallback anchor = %.6g, want 5.2", anchor)
	}
	row, _ := st.Position().GetOpenPositionBySymbol("trader-t1", "AKEUSDT", "SHORT")
	if row == nil || !nearly(row.InitialStopLoss, 5.2) {
		t.Fatalf("legacy fallback not frozen to row: %+v", row)
	}

	// After the freeze, hydration wins over any later live-stop change.
	at.SetRecordedStopLoss("AKEUSDT", "short", 5.1)
	if anchor := at.initialStopAnchor("AKEUSDT", "short", 5.1); !nearly(anchor, 5.2) {
		t.Fatalf("anchor drifted with live stop: %.6g", anchor)
	}
}

// Restart story: a fresh AutoTrader (empty maps) must recover the anchor from
// the persisted row, not from the live stop.
func TestInitialStopAnchorSurvivesRestart(t *testing.T) {
	st := profitLockTestStore(t)
	at := profitLockTestTrader(t, st)
	if err := st.Position().CreateOpenPosition(&store.TraderPosition{
		TraderID: "trader-t1", Symbol: "ONDOUSDT", Side: "LONG",
		Quantity: 144.8, EntryPrice: 0.3503, EntryTime: 4, Source: "sync",
		InitialStopLoss: 0.3428,
	}); err != nil {
		t.Fatalf("create position: %v", err)
	}

	if anchor := at.initialStopAnchor("ONDOUSDT", "long", 0.3512); !nearly(anchor, 0.3428) {
		t.Fatalf("restarted anchor = %.6g, want persisted 0.3428", anchor)
	}
}
