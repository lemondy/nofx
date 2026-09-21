package trader

import (
	"testing"
	"time"

	"nofx/store"
)

// One-way margin mode netting (BTWUSDT 09-21) leaves DB rows OPEN for
// positions the exchange no longer holds. The matcher must flag exactly
// those rows — and never race a fresh open or a case-mismatch.
func TestOrphanedRows(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour).UnixMilli()
	fresh := now.Add(-5 * time.Minute).UnixMilli()

	live := map[string]bool{
		"BTCUSDT_long": true,
		"ETHUSDT_long": true, // opposite side of the ETH short row below
	}
	rows := []*store.TraderPosition{
		{ID: 1, Symbol: "BTCUSDT", Side: "LONG", EntryTime: old},    // exchange holds it → keep
		{ID: 2, Symbol: "BTWUSDT", Side: "SHORT", EntryTime: old},   // symbol gone → orphan (the live case)
		{ID: 3, Symbol: "ETHUSDT", Side: "SHORT", EntryTime: old},   // netted: only opposite side remains → orphan
		{ID: 4, Symbol: "SOLUSDT", Side: "LONG", EntryTime: fresh},  // too fresh → keep (cache/open race guard)
		{ID: 5, Symbol: "XRPUSDT", Side: "LONG", EntryTime: 0, UpdatedAt: old}, // zero entry time → UpdatedAt fallback → orphan
		{ID: 6, Symbol: "btcusdt", Side: "long", EntryTime: old},    // normalized symbol+side matches row 1's key? No —
		// row 6 normalizes to the SAME key as row 1 → keep (case-insensitivity)
	}

	orphans := orphanedRows(rows, live, now, orphanPositionMinAge)
	got := map[int64]bool{}
	for _, r := range orphans {
		got[r.ID] = true
	}
	if len(orphans) != 3 || !got[2] || !got[3] || !got[5] {
		t.Fatalf("want orphans {2,3,5}, got %v", got)
	}
}
