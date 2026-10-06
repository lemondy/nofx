package breakout

import (
	"testing"
	"time"
)

// ── review 2026-10-06 batch-3: P3-1 / P3-4 ──

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

// P3-4: retention follows the largest configured window, not the hard-coded
// default — a strategy asking for 30 days must not get 9.
func TestFix3GainerHistoryRetentionFollowsKnob(t *testing.T) {
	setGainerHistoryPath(t.TempDir() + "/gainers.json")
	now := time.Now()
	// Record today with a 30-day knob, then backfill a 20-day-old day.
	recordGainerHistory([]GainerQuote{{Symbol: "AAAUSDT", ChgPct: 10, Price: 1}}, now, 30)
	old := now.AddDate(0, 0, -20)
	board20d := []GainerQuote{{Symbol: "OLDDAYUSDT", ChgPct: 10, Price: 1}}
	// recordGainerHistory keys on its `now` arg — emulate an old day by
	// recording with the backdated timestamp.
	recordGainerHistory(board20d, old, 30)

	hist := loadGainerHistory()
	dayKey := old.UTC().Format("2006-01-02")
	if _, ok := hist.Days[dayKey]; !ok {
		t.Fatalf("20-day-old day pruned despite a 30-day knob: retention still follows the default window")
	}
	// With the DEFAULT knob the same old day must be pruned (7+2 window).
	setGainerHistoryPath(t.TempDir() + "/gainers2.json")
	recordGainerHistory(board20d, old, DefaultShortScanHistoryDays)
	recordGainerHistory([]GainerQuote{{Symbol: "AAAUSDT", ChgPct: 10, Price: 1}}, now, DefaultShortScanHistoryDays)
	hist2 := loadGainerHistory()
	if _, ok := hist2.Days[dayKey]; ok {
		t.Fatalf("20-day-old day survived with the default 7-day window")
	}
}
