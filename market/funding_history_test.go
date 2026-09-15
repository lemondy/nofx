package market

import (
	"testing"
	"time"
)

func dateUTC(y int, mon time.Month, d, h int) time.Time {
	return time.Date(y, mon, d, h, 0, 0, 0, time.UTC)
}

func TestFundingRolloverDetected(t *testing.T) {
	// Raw decimals: 0.0001 = 0.01% per settlement. Threshold here is already
	// scaled to the symbol's settlement interval by the caller.
	cases := []struct {
		name          string
		prev, cur, th float64
		want          bool
	}{
		{"high then falling", 0.0004, 0.00005, 0.00015, true},
		{"high but still rising", 0.0004, 0.0005, 0.00015, false},
		{"never high", 0.0001, 0.00005, 0.00015, false},
		{"boundary prev equals threshold", 0.00015, 0.00005, 0.00015, false},
		{"cur equals prev", 0.0004, 0.0004, 0.00015, false},
		{"negative funding cannot roll over from high", -0.001, -0.002, 0.00015, false},
	}
	for _, c := range cases {
		if got := FundingRolloverDetected(c.prev, c.cur, c.th); got != c.want {
			t.Errorf("%s: FundingRolloverDetected(%g,%g,%g) = %v, want %v", c.name, c.prev, c.cur, c.th, got, c.want)
		}
	}
}

func TestIsUSMarketWeekend(t *testing.T) {
	// 2026-09-12/13 UTC are Sat/Sun; 2026-09-15 (the review cycle, Tuesday)
	// must be a trading day.
	if !IsUSMarketWeekend(dateUTC(2026, 9, 12, 20)) {
		t.Error("Saturday 2026-09-12 20:00 UTC should be weekend (Sat in ET)")
	}
	if IsUSMarketWeekend(dateUTC(2026, 9, 15, 14)) {
		t.Error("Tuesday 2026-09-15 must not be weekend")
	}
}
