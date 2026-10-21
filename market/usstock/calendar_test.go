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

// Known NYSE schedules (published), used to pin the rule-based calendar.
var knownHolidays = map[int][]string{
	2024: {"2024-01-01", "2024-01-15", "2024-02-19", "2024-03-29", "2024-05-27", "2024-06-19", "2024-07-04", "2024-09-02", "2024-11-28", "2024-12-25"},
	2025: {"2025-01-01", "2025-01-20", "2025-02-17", "2025-04-18", "2025-05-26", "2025-06-19", "2025-07-04", "2025-09-01", "2025-11-27", "2025-12-25"},
	2026: {"2026-01-01", "2026-01-19", "2026-02-16", "2026-04-03", "2026-05-25", "2026-06-19", "2026-07-03", "2026-09-07", "2026-11-26", "2026-12-25"},
	2027: {"2027-01-01", "2027-01-18", "2027-02-15", "2027-03-26", "2027-05-31", "2027-06-18", "2027-07-05", "2027-09-06", "2027-11-25", "2027-12-24"},
	// 2028-01-01 is a Saturday: not observed on Friday 2027-12-31.
	2028: {"2028-01-17", "2028-02-21", "2028-04-14", "2028-05-29", "2028-06-19", "2028-07-04", "2028-09-04", "2028-11-23", "2028-12-25"},
	2029: {"2029-01-01", "2029-01-15", "2029-02-19", "2029-03-30", "2029-05-28", "2029-06-19", "2029-07-04", "2029-09-03", "2029-11-22", "2029-12-25"},
	2030: {"2030-01-01", "2030-01-21", "2030-02-18", "2030-04-19", "2030-05-27", "2030-06-19", "2030-07-04", "2030-09-02", "2030-11-28", "2030-12-25"},
}
var knownHalfDays = map[int][]string{
	2024: {"2024-07-03", "2024-11-29", "2024-12-24"},
	2025: {"2025-07-03", "2025-11-28", "2025-12-24"},
	2026: {"2026-11-27", "2026-12-24"},
	2027: {"2027-11-26"},
	2028: {"2028-07-03", "2028-11-24"},
}

func TestCalendarRulesMatchPublishedSchedules(t *testing.T) {
	for year, want := range knownHolidays {
		got := holidayDates(year)
		if len(got) != len(want) {
			t.Errorf("%d holidays: got %v want %v", year, got, want)
		}
		for _, date := range want {
			day := et(date + " 12:00")
			if !got[date] || IsTradingDay(day) || SessionAt(day) != SessionClosed {
				t.Errorf("holiday %s not closed", date)
			}
			if _, _, ok := SessionBounds(day); ok {
				t.Errorf("holiday %s has session bounds", date)
			}
		}
	}
	for year, want := range knownHalfDays {
		got := halfDayDates(year)
		if len(got) != len(want) {
			t.Errorf("%d half days: got %v want %v", year, got, want)
		}
		for _, date := range want {
			_, close, ok := SessionBounds(et(date + " 12:00"))
			if !ok || close.Hour() != 13 || SessionAt(close) != SessionAfter {
				t.Errorf("half day %s: %v %v", date, close, ok)
			}
		}
	}
}

func TestCalendarNamedDays(t *testing.T) {
	for _, tc := range []struct {
		date    string
		trading bool
	}{
		{"2026-11-26", false}, // Thanksgiving
		{"2026-12-25", false}, // Christmas
		{"2027-01-18", false}, // MLK
		{"2026-11-27", true},  // day after Thanksgiving (half day)
		{"2027-12-31", true},  // 2028-01-01 Saturday is not observed
		{"2027-12-24", false}, // Christmas Saturday -> Friday
		{"2027-07-05", false}, // July 4 Sunday -> Monday
		{"2026-07-03", false}, // July 4 Saturday -> Friday
		{"2026-10-12", true},  // Columbus Day: NYSE open
		{"2026-11-11", true},  // Veterans Day: NYSE open
		{"2026-10-10", false}, // Saturday
	} {
		if got := IsTradingDay(et(tc.date + " 12:00")); got != tc.trading {
			t.Errorf("%s trading=%v want %v", tc.date, got, tc.trading)
		}
	}
	if !IsTradingDay(et("2026-12-24 12:00")) {
		t.Fatal("Christmas Eve 2026 trades (13:00 close)")
	}
	if _, c, _ := SessionBounds(et("2026-12-24 09:30")); c.Hour() != 13 {
		t.Fatalf("Christmas Eve close %v", c)
	}
	// ET date, not UTC date, determines trading status.
	if IsTradingDay(time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)) {
		t.Fatal("still Sunday in ET")
	}
}

func TestCalendarNextCloseAndCrossed(t *testing.T) {
	for _, tc := range []struct{ from, want string }{
		{"2026-11-25 10:00", "2026-11-25 16:00"},
		{"2026-11-25 16:00", "2026-11-27 13:00"}, // Thanksgiving skipped; half day
		{"2026-11-26 09:00", "2026-11-27 13:00"},
		{"2026-12-24 12:59", "2026-12-24 13:00"},
		{"2026-12-24 13:00", "2026-12-28 16:00"}, // Christmas + weekend
		{"2027-01-15 17:00", "2027-01-19 16:00"}, // MLK weekend
		{"2026-10-09 17:00", "2026-10-12 16:00"},
	} {
		if got := NextClose(et(tc.from)); !got.Equal(et(tc.want)) {
			t.Errorf("NextClose(%s)=%v want %s", tc.from, got, tc.want)
		}
	}
	if !CrossedClose(et("2026-11-25 15:00"), et("2026-11-25 16:00")) || CrossedClose(et("2026-11-25 15:00"), et("2026-11-25 15:59")) {
		t.Fatal("same-day close boundary is inclusive of the close instant")
	}
	// A holiday has no close to cross.
	if CrossedClose(et("2026-11-26 09:00"), et("2026-11-26 23:00")) {
		t.Fatal("Thanksgiving has no close")
	}
	if got := NextTradingDay(et("2026-11-25 20:00")); got.Format("2006-01-02") != "2026-11-27" {
		t.Fatalf("NextTradingDay %v", got)
	}
}
