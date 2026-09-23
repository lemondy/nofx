package market

import (
	"testing"
	"time"
)

// Pins for the 2026-09-23 classification fix: Binance renamed the stock
// classification from underlyingSubType "Stocks" to underlyingType
// EQUITY-family — the old subType-only matcher matched nothing and silently
// disabled every stock gate. The predicate must accept the new types, the
// legacy subType, and nothing else.
func TestIsStockClassification(t *testing.T) {
	cases := []struct {
		ut   string
		subs []string
		want bool
	}{
		{"EQUITY", nil, true},
		{"PREMARKET", nil, true},
		{"KR_EQUITY", nil, true},
		{"HK_EQUITY", nil, true},
		{"CN_EQUITY", nil, true},
		{"COIN", []string{"PoW", "Crypto"}, false},
		{"COMMODITY", []string{"Metals"}, false},
		{"INDEX", nil, false},
		{"", []string{"Stocks"}, true},    // legacy subType still honored
		{"COIN", []string{"stocks"}, true}, // case-insensitive legacy
	}
	for _, c := range cases {
		if got := isStockClassification(c.ut, c.subs); got != c.want {
			t.Errorf("isStockClassification(%q, %v) = %v, want %v", c.ut, c.subs, got, c.want)
		}
	}
}

// US regular hours: Mon–Fri 09:30–16:00 Eastern; weekends never open;
// UTC inputs convert correctly (14:30 UTC = 09:30 EST winter / 10:30 EDT
// summer — assert both sides of the boundary in a fixed-offset frame via
// explicit Eastern wall-clock times).
func TestIsUSMarketOpen(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	cases := []struct {
		wall string // Eastern wall-clock
		day  time.Weekday
		want bool
	}{
		{"09:29", time.Monday, false},
		{"09:30", time.Monday, true},
		{"12:00", time.Wednesday, true},
		{"15:59", time.Friday, true},
		{"16:00", time.Friday, false},
		{"12:00", time.Saturday, false},
		{"12:00", time.Sunday, false},
	}
	for _, c := range cases {
		h, m := 0, 0
		if _, err := time.Parse("15:04", c.wall); err == nil {
			h, m = atoi2(c.wall[:2]), atoi2(c.wall[3:])
		}
		// Find a date with the wanted weekday in a fixed week.
		base := time.Date(2026, 9, 21, h, m, 0, 0, loc) // 2026-09-21 is a Monday
		for base.Weekday() != c.day {
			base = base.AddDate(0, 0, 1)
		}
		if got := IsUSMarketOpen(base); got != c.want {
			t.Errorf("IsUSMarketOpen(%s %s) = %v, want %v", c.day, c.wall, got, c.want)
		}
	}
}

// atoi2 parses a two-digit decimal string.
func atoi2(s string) int {
	return int(s[0]-'0')*10 + int(s[1]-'0')
}

// The US-equity subset follows the loaded classification (globals injected
// directly — package-private test).
func TestIsUSEquitySymbol(t *testing.T) {
	bstockMu.Lock()
	oldAll, oldUS := bstockSymbols, usEquitySymbols
	bstockSymbols = map[string]bool{"AAPLUSDT": true, "SAMSUNGUSDT": true, "BTCUSDT": true}
	usEquitySymbols = map[string]bool{"AAPLUSDT": true} // Samsung is HK/KR/CN-side, not US
	bstockMu.Unlock()
	t.Cleanup(func() {
		bstockMu.Lock()
		bstockSymbols, usEquitySymbols = oldAll, oldUS
		bstockMu.Unlock()
	})

	if !IsUSEquitySymbol("aaplusdt") {
		t.Error("AAPLUSDT must classify as US equity (case-insensitive)")
	}
	if IsUSEquitySymbol("SAMSUNGUSDT") {
		t.Error("non-US equity token must NOT classify as US equity")
	}
	if IsUSEquitySymbol("BTCUSDT") {
		t.Error("crypto must not classify as US equity")
	}
}
