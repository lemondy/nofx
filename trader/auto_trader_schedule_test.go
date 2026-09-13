package trader

import (
	"testing"
	"time"
)

// Cycle schedule must align to wall-clock boundaries counted from the top of
// the hour (user request 2026-09-10): 5m fires at :00/:05/:10… regardless of
// when the trader started; boundaries crossing hour and midnight work; a
// non-divisor interval (7m) repeats its pattern every hour.
func TestNextAlignedWait(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	at := func(h, m, s int) time.Time {
		return time.Date(2026, 9, 10, h, m, s, 0, loc)
	}
	cases := []struct {
		name         string
		interval     time.Duration
		now          time.Time
		wantMinSec   string // the next boundary's hh:mm:ss
		wantWait     time.Duration
	}{
		{"5m mid-interval", 5 * time.Minute, at(10, 2, 0), "10:05:00", 3 * time.Minute},
		{"5m exactly on boundary", 5 * time.Minute, at(10, 5, 0), "10:10:00", 5 * time.Minute},
		{"5m one second late", 5 * time.Minute, at(10, 5, 1), "10:10:00", 4*time.Minute + 59*time.Second},
		{"5m crosses hour", 5 * time.Minute, at(10, 57, 30), "11:00:00", 2*time.Minute + 30*time.Second},
		{"5m crosses midnight", 5 * time.Minute, at(23, 59, 30), "00:00:00", 30 * time.Second},
		{"7m non-divisor", 7 * time.Minute, at(10, 3, 0), "10:07:00", 4 * time.Minute},
		{"7m carries past the hour", 7 * time.Minute, at(10, 58, 0), "11:03:00", 5 * time.Minute},
		{"10m from start offset", 10 * time.Minute, at(9, 15, 24), "09:20:00", 4*time.Minute + 36*time.Second},
		{"30m", 30 * time.Minute, at(9, 15, 24), "09:30:00", 14*time.Minute + 36*time.Second},
		{"non-positive interval falls back", 0, at(10, 2, 0), "10:03:00", time.Minute},
	}
	for _, tc := range cases {
		wait := nextAlignedWait(tc.interval, tc.now)
		next := tc.now.Add(wait)
		got := next.In(loc).Format("15:04:05")
		if got != tc.wantMinSec {
			t.Errorf("%s: next boundary %s, want %s", tc.name, got, tc.wantMinSec)
		}
		if wait != tc.wantWait {
			t.Errorf("%s: wait %v, want %v", tc.name, wait, tc.wantWait)
		}
	}
}
