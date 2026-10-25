package market

import (
	"strings"
	"testing"
	"time"
)

// barsEnding builds n consecutive bars of `tf` whose LAST bar opens at lastOpen.
func barsEnding(tf string, lastOpen time.Time, n int) []KlineBar {
	dur := TimeframeDuration(tf)
	out := make([]KlineBar, n)
	for i := 0; i < n; i++ {
		out[i] = KlineBar{Time: lastOpen.Add(time.Duration(i-(n-1)) * dur).UnixMilli(), Close: 100, Volume: 10}
	}
	return out
}

// Review 2026-10-20 #8: a series whose last bars wiggle with real volume but
// stopped 30 minutes ago used to pass isStaleData (price-freeze only).
func TestSeriesStaleReasonByBarTime(t *testing.T) {
	sampled := time.Date(2026, 10, 20, 12, 0, 30, 0, time.UTC)
	cases := []struct {
		name     string
		tf       string
		lastOpen time.Time
		stale    bool
	}{
		{"forming bar", "3m", time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), false},
		{"just-closed bar (vendor lag)", "3m", time.Date(2026, 10, 20, 11, 57, 0, 0, time.UTC), false},
		{"2 periods behind", "3m", time.Date(2026, 10, 20, 11, 51, 0, 0, time.UTC), false},
		{"30 minutes behind", "3m", time.Date(2026, 10, 20, 11, 30, 0, 0, time.UTC), true},
		{"4h bar still the forming one", "4h", time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC), false},
		{"4h feed stopped a day ago", "4h", time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC), true},
	}
	for _, c := range cases {
		reason := SeriesStaleReason(c.tf, barsEnding(c.tf, c.lastOpen, 60), sampled, true)
		if (reason != "") != c.stale {
			t.Errorf("%s: reason=%q, want stale=%v", c.name, reason, c.stale)
		}
	}
	// Judged at the SAMPLING clock: the same 11:57 bar is fine at 12:00:30 but a
	// later processing time must not matter (it is not an input).
	if SeriesStaleReason("3m", nil, sampled, true) != "" || SeriesStaleReason("3m", barsEnding("3m", sampled, 5), time.Time{}, true) != "" {
		t.Error("empty series / zero sampling time must not be judged")
	}
}

func TestSeriesStaleReasonDetectsGap(t *testing.T) {
	sampled := time.Date(2026, 10, 20, 12, 0, 30, 0, time.UTC)
	bars := barsEnding("15m", time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), 40)
	// Punch a 3h hole (12 bars) into the recent tail by shifting the older half back.
	for i := 0; i < 20; i++ {
		bars[i].Time -= (3 * time.Hour).Milliseconds()
	}
	r := SeriesStaleReason("15m", bars, sampled, true)
	if !strings.Contains(r, "gap") {
		t.Fatalf("expected gap reason, got %q", r)
	}
	if SeriesStaleReason("15m", bars, sampled, false) != "" {
		t.Error("gap check must be skippable (session markets)")
	}
	// A hole older than the scanned tail is ignored.
	old := barsEnding("15m", time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), 80)
	for i := 0; i < 20; i++ {
		old[i].Time -= (3 * time.Hour).Milliseconds()
	}
	if r := SeriesStaleReason("15m", old, sampled, true); r != "" {
		t.Errorf("old hole outside the tail window flagged: %q", r)
	}
}

func TestStaleTimeframesModeSessions(t *testing.T) {
	// Wed 2026-10-14 11:00 ET (EDT = UTC-4) -> 15:00 UTC: US session open.
	open := time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)
	mk := func(sampled time.Time, lastAgo time.Duration) map[string]*TimeframeSeriesData {
		return map[string]*TimeframeSeriesData{
			"3m": {Klines: barsEnding("3m", sampled.Add(-lastAgo), 60)},
			"4h": {Klines: barsEnding("4h", sampled.Add(-lastAgo-24*time.Hour), 60)},
		}
	}
	// 24/7: both stale.
	if got := staleTimeframesMode(mk(open, 2*time.Hour), open, freshnessAlways); len(got) != 2 {
		t.Fatalf("24/7 market: want 3m+4h stale, got %v", got)
	}
	// US session open for the whole window: 3m is judged and flagged; 4h cannot
	// be (window spans the overnight break) so it is skipped.
	got := staleTimeframesMode(mk(open, 2*time.Hour), open, freshnessUSSession)
	if _, ok := got["3m"]; !ok || len(got) != 1 {
		t.Fatalf("US session open: want only 3m stale, got %v", got)
	}
	// Just after the open the look-back window still covers the closed market.
	justOpen := time.Date(2026, 10, 14, 13, 35, 0, 0, time.UTC) // 09:35 ET
	if got := staleTimeframesMode(mk(justOpen, 3*time.Hour), justOpen, freshnessUSSession); len(got) != 0 {
		t.Errorf("market only just opened: must not be stale, got %v", got)
	}
	// Weekend / closed market: never stale, however old the last bar.
	sat := time.Date(2026, 10, 17, 15, 0, 0, 0, time.UTC)
	if got := staleTimeframesMode(mk(sat, 20*time.Hour), sat, freshnessUSSession); len(got) != 0 {
		t.Errorf("closed market flagged stale: %v", got)
	}
	// Unknown calendar: never judged.
	if got := staleTimeframesMode(mk(open, 48*time.Hour), open, freshnessNever); got != nil {
		t.Errorf("freshnessNever flagged: %v", got)
	}
	// Plain crypto symbol resolves to the 24/7 mode through the public entry.
	if got := StaleTimeframes("BTCUSDT", mk(open, 2*time.Hour), open); len(got) != 2 {
		t.Errorf("BTCUSDT should be judged 24/7, got %v", got)
	}
	if StaleTimeframes("BTCUSDT", mk(open, 2*time.Hour), time.Time{}) != nil {
		t.Error("zero sampling time must disable the check")
	}
}
