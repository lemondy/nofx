package trader

import (
	"testing"
	"time"

	"nofx/kernel"
)

// timeStopDue truth table (user menu directive 09-29): quick-mode positions
// exit only when BOTH the hold-time window elapsed AND price PnL is still
// ≤0. Profitable positions are never time-stopped; stopHours ≤ 0 disables.
func TestTimeStopDue(t *testing.T) {
	fourHours := 4 * time.Hour
	cases := []struct {
		name    string
		held    time.Duration
		pnlPct  float64
		stopHrs float64
		want    bool
	}{
		{"young loser waits", 2 * time.Hour, -0.5, 4, false},
		{"aged loser exits", fourHours, -0.5, 4, true},
		{"aged flat exits", fourHours, 0, 4, true},
		{"aged winner kept", fourHours, 1.2, 4, false},
		{"disabled", 100 * time.Hour, -3, 0, false},
		{"exactly at threshold exits", fourHours, -0.01, 4, true},
	}
	for _, c := range cases {
		if got := timeStopDue(c.held, c.pnlPct, c.stopHrs); got != c.want {
			t.Fatalf("%s: timeStopDue(%v, %.2f, %.0f) = %v, want %v", c.name, c.held, c.pnlPct, c.stopHrs, got, c.want)
		}
	}
}

// tpFractionForMode: only the TREND template runs a runner — range/quick
// collapse the split fraction to a full close; trend keeps the configured
// split; trailing-off traders already run 1.0 everywhere.
func TestTPFractionForMode(t *testing.T) {
	cases := []struct {
		mode string
		base float64
		want float64
	}{
		{kernel.ExitModeTrend, 0.5, 0.5}, // trend keeps the split
		{kernel.ExitModeRange, 0.5, 1.0}, // range: the target IS the exit
		{kernel.ExitModeQuick, 0.5, 1.0}, // quick: full exit at target + time stop
		{kernel.ExitModeRange, 1.0, 1.0}, // trailing off: already full
		{"", 0.5, 0.5},                   // legacy decision without a mode = trend
		{"quick", 0.05, 1.0},             // even the min split collapses
	}
	for _, c := range cases {
		if got := tpFractionForMode(c.mode, c.base); got != c.want {
			t.Fatalf("tpFractionForMode(%q, %.2f) = %.2f, want %.2f", c.mode, c.base, got, c.want)
		}
	}
}
