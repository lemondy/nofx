package breakout

import (
	"testing"
)

// ── review 2026-10-06 batch-3: P3-1 ──

// P3-1: on an all-closed series the slot comparison must read the LAST
// closed bar (k[n-1]), not one bar stale (k[n-2]).
func TestFix3VolSlotReadsLastClosed(t *testing.T) {
	k := make([]Kline, 0, 32)
	for i := 0; i < 32; i++ {
		vol := 1000.0
		if i == 31 {
			vol = 3000 // ONLY the last bar spikes
		}
		k = append(k, Kline{Volume: vol})
	}
	if got := volSlotMultiple(k, 4); got < 2.9 || got > 3.1 {
		t.Fatalf("volSlotMultiple = %.2f, want ~3.0 (spike on the last closed bar must be seen)", got)
	}
}

