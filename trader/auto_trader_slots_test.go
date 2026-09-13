package trader

import "testing"

// Slot accounting: the count AFTER a new entry must include open positions,
// resting entries on OTHER symbols, and the new order itself — the replaced
// symbol's own resting entry does not double-count.
func TestNextSlotCount(t *testing.T) {
	pending := []string{"APTUSDT", "WLDUSDT"}
	if got := nextSlotCount(2, pending, "APTUSDT"); got != 4 {
		t.Fatalf("2 positions + WLDUSDT resting + new APTUSDT = 4, got %d", got)
	}
	if got := nextSlotCount(0, pending, "NEWUSDT"); got != 3 {
		t.Fatalf("0 positions + 2 resting + new = 3, got %d", got)
	}
	if got := nextSlotCount(0, nil, "X"); got != 1 {
		t.Fatalf("clean book: new order alone = 1, got %d", got)
	}
}
