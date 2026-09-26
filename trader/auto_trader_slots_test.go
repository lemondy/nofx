package trader

import "testing"

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
