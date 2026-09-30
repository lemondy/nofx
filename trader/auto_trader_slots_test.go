package trader

import (
	"testing"

	"nofx/store"
)

// Slot accounting: the count AFTER a new entry must include open positions,
// resting entries on OTHER symbols, and the new order itself — the replaced
// symbol's own resting entry does not double-count.
func TestNextSlotCount(t *testing.T) {
	pending := []string{"APTUSDT|long", "WLDUSDT|short"}
	if got := nextSlotCount(2, pending, "APTUSDT|long"); got != 4 {
		t.Fatalf("2 positions + WLDUSDT resting + new APTUSDT = 4, got %d", got)
	}
	if got := nextSlotCount(0, pending, "NEWUSDT|long"); got != 3 {
		t.Fatalf("0 positions + 2 resting + new = 3, got %d", got)
	}
	if got := nextSlotCount(0, nil, "X|long"); got != 1 {
		t.Fatalf("clean book: new order alone = 1, got %d", got)
	}
	if got := nextSlotCount(0, pending, "APTUSDT|short"); got != 3 {
		t.Fatalf("opposite side occupies its own slot, got %d", got)
	}
}

func TestPendingMarginReservedKeepsOppositeSide(t *testing.T) {
	at := &AutoTrader{}
	at.setPendingEntry(&pendingEntry{Symbol: "SOLUSDT", Side: "long", Price: 100, Quantity: 2, Leverage: 2})
	at.setPendingEntry(&pendingEntry{Symbol: "SOLUSDT", Side: "short", Price: 100, Quantity: 3, ProtectedQty: 1, Leverage: 2})
	if got := at.pendingMarginReserved(pendingEntryKey("SOLUSDT", "long")); got != 100 {
		t.Fatalf("replacing long must reserve the short's remaining 100 USDT margin, got %.2f", got)
	}
	if got := at.pendingMarginReserved(pendingEntryKey("SOLUSDT", "short")); got != 100 {
		t.Fatalf("replacing short must reserve the long's 100 USDT margin, got %.2f", got)
	}
}

// The position cap compares the POST-OPEN count with > : count == max means
// the cap is exactly reached and the open proceeds. The old `>=` (a leftover
// from when callers passed the raw open count) made the effective cap
// max−1 — with max=5 and 4 open positions the 5th open was rejected as
// "Already at max positions (5/5)" (PROMUSDT 2026-09-30).
func TestEnforceMaxPositionsCapBoundary(t *testing.T) {
	at := &AutoTrader{}
	cfg := &store.StrategyConfig{}
	cfg.RiskControl.MaxPositions = 5
	at.config = AutoTraderConfig{StrategyConfig: cfg}

	if err := at.enforceMaxPositions(4); err != nil {
		t.Fatalf("4/5 must pass: %v", err)
	}
	if err := at.enforceMaxPositions(5); err != nil {
		t.Fatalf("5/5 (4 open + this entry) must pass — cap exactly reached: %v", err)
	}
	if err := at.enforceMaxPositions(6); err == nil {
		t.Fatal("6 > 5 must be rejected")
	}

	// MaxPositions unset (0) rides the default 3 — but only when a strategy
	// config exists at all (nil config short-circuits to allow).
	cfg0 := &store.StrategyConfig{}
	at.config = AutoTraderConfig{StrategyConfig: cfg0}
	if err := at.enforceMaxPositions(3); err != nil {
		t.Fatalf("3/3 with default cap must pass: %v", err)
	}
	if err := at.enforceMaxPositions(4); err == nil {
		t.Fatal("4 > 3 default cap must be rejected")
	}
}
