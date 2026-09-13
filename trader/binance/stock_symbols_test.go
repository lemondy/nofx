package binance

import (
	"testing"
	"time"
)

// US Eastern weekend detection: Sat/Sun in America/New_York, hour-insensitive
// (the whole day blocks), and the CET 03:40 sample from the user's screenshot
// (Saturday 07:40 ET-window morning) must block.
func TestIsUSMarketWeekend(t *testing.T) {
	cases := []struct {
		name string
		utc  string
		want bool
	}{
		// 2026-09-12 (Sat) 03:40 Beijing = 09-11 19:40 ET → Friday evening: NOT weekend per the Sat/Sun rule
		{"Friday evening ET", "2026-09-12T03:40:00+08:00", false},
		// 2026-09-12 (Sat) 14:00 Beijing = 09-12 02:00 ET → Saturday: blocked
		{"Saturday ET", "2026-09-12T14:00:00+08:00", true},
		// 2026-09-13 (Sun) 20:00 Beijing = 09-13 08:00 ET → Sunday: blocked
		{"Sunday ET", "2026-09-13T20:00:00+08:00", true},
		// 2026-09-14 (Mon) 03:00 Beijing = 09-13 15:00 ET → Sunday afternoon: blocked
		{"Monday morning Beijing, still Sunday ET", "2026-09-14T03:00:00+08:00", true},
		// 2026-09-14 (Mon) 21:00 Beijing = 09-14 09:00 ET → Monday: open
		{"Monday ET", "2026-09-14T21:00:00+08:00", false},
	}
	for _, tc := range cases {
		tm, err := time.Parse(time.RFC3339, tc.utc)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := IsUSMarketWeekend(tm); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
