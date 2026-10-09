package usstock

import (
	"testing"
	"time"
)

func TestCalendarDST(t *testing.T) {
	for _, tc := range []struct{ date, open, close string }{
		{"2026-03-09 12:00", "2026-03-09T13:30:00Z", "2026-03-09T20:00:00Z"},
		{"2026-11-02 12:00", "2026-11-02T14:30:00Z", "2026-11-02T21:00:00Z"},
	} {
		t.Run(tc.date, func(t *testing.T) {
			open, close, ok := SessionBounds(et(tc.date))
			if !ok || open.UTC().Format(time.RFC3339) != tc.open || close.UTC().Format(time.RFC3339) != tc.close {
				t.Fatalf("bounds %v %v %v", open, close, ok)
			}
		})
	}
}
func TestCalendarSessionEdges(t *testing.T) {
	for _, tc := range []struct {
		date    string
		session Session
	}{
		{"2026-10-09 03:59", SessionClosed}, {"2026-10-09 04:00", SessionPre},
		{"2026-10-09 09:29", SessionPre}, {"2026-10-09 09:30", SessionRegular},
		{"2026-10-09 15:59", SessionRegular}, {"2026-10-09 16:00", SessionAfter},
		{"2026-10-09 19:59", SessionAfter}, {"2026-10-09 20:00", SessionClosed},
		{"2026-11-26 12:00", SessionClosed}, {"2026-11-26 18:00", SessionClosed},
		{"2026-11-27 12:59", SessionRegular}, {"2026-11-27 13:00", SessionAfter},
		{"2026-10-10 12:00", SessionClosed}, {"2026-10-11 04:00", SessionClosed},
	} {
		t.Run(tc.date, func(t *testing.T) {
			if got := SessionAt(et(tc.date)); got != tc.session {
				t.Fatalf("got %s want %s", got, tc.session)
			}
		})
	}
	for _, date := range []string{"2026-11-26 12:00", "2026-10-10 12:00"} {
		if _, _, ok := SessionBounds(et(date)); ok || IsTradingDay(et(date)) {
			t.Fatalf("nontrading date: %s", date)
		}
	}
}
func TestCalendarHolidayAndHalfDayTables(t *testing.T) {
	for date := range holidays {
		day := et(date + " 12:00")
		if IsTradingDay(day) || SessionAt(day) != SessionClosed {
			t.Errorf("holiday %s open", date)
		}
	}
	for date := range halfDays {
		_, close, ok := SessionBounds(et(date + " 12:00"))
		if !ok || close.Hour() != 13 || SessionAt(close) != SessionAfter {
			t.Errorf("half day %s: %v %v", date, close, ok)
		}
	}
	if !IsTradingDay(et("2029-01-01 12:00")) {
		t.Fatal("future weekdays should be treated as trading days")
	}
	// ET date, not UTC date, determines trading status.
	if IsTradingDay(time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)) {
		t.Fatal("still Sunday in ET")
	}
}
