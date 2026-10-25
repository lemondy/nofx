package usstock

import (
	"sync"
	"time"
	_ "time/tzdata"
)

var newYork = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return loc
}()

// NYSE holiday and early-close rules, computed per year (no per-year table):
//
//	New Year's Day        Jan 1            (Saturday: NOT observed on Friday)
//	Martin Luther King Jr 3rd Monday Jan
//	Washington's Birthday 3rd Monday Feb
//	Good Friday           Friday before Easter Sunday
//	Memorial Day          last Monday May
//	Juneteenth            Jun 19           (from 2022)
//	Independence Day      Jul 4
//	Labor Day             1st Monday Sep
//	Thanksgiving          4th Thursday Nov
//	Christmas             Dec 25
//
// Fixed-date holidays falling on Saturday are observed the Friday before
// (except New Year's Day), on Sunday the Monday after. Early closes (13:00 ET):
// the day after Thanksgiving, Dec 24, and Jul 3 when Jul 4 is a Tue–Fri
// weekday — each only if that day is otherwise a normal trading day.

// easterSunday uses the anonymous Gregorian algorithm.
func easterSunday(year int) (month time.Month, day int) {
	a := year % 19
	b, c := year/100, year%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month = time.Month((h + l - 7*m + 114) / 31)
	day = (h+l-7*m+114)%31 + 1
	return
}

// nthWeekday returns the day of month of the n-th (1-based) given weekday; n<0
// means the last one in the month.
func nthWeekday(year int, month time.Month, wd time.Weekday, n int) int {
	if n > 0 {
		first := time.Date(year, month, 1, 12, 0, 0, 0, time.UTC)
		return 1 + (int(wd)-int(first.Weekday())+7)%7 + (n-1)*7
	}
	last := time.Date(year, month+1, 0, 12, 0, 0, 0, time.UTC)
	return last.Day() - (int(last.Weekday())-int(wd)+7)%7
}

// observed shifts a fixed-date holiday to the weekday it is observed on.
func observed(year int, month time.Month, day int, saturdayToFriday bool) (time.Time, bool) {
	d := time.Date(year, month, day, 12, 0, 0, 0, time.UTC)
	switch d.Weekday() {
	case time.Saturday:
		if !saturdayToFriday {
			return time.Time{}, false
		}
		return d.AddDate(0, 0, -1), true
	case time.Sunday:
		return d.AddDate(0, 0, 1), true
	}
	return d, true
}

// holidayDates returns the NYSE full-closure dates ("2006-01-02") of a year.
func holidayDates(year int) map[string]bool {
	out := map[string]bool{}
	add := func(t time.Time, ok bool) {
		if ok && t.Year() == year {
			out[t.Format("2006-01-02")] = true
		}
	}
	date := func(m time.Month, d int) time.Time { return time.Date(year, m, d, 12, 0, 0, 0, time.UTC) }
	add(observed(year, time.January, 1, false))
	add(date(time.January, nthWeekday(year, time.January, time.Monday, 3)), true)
	add(date(time.February, nthWeekday(year, time.February, time.Monday, 3)), true)
	em, ed := easterSunday(year)
	add(date(em, ed).AddDate(0, 0, -2), true)
	add(date(time.May, nthWeekday(year, time.May, time.Monday, -1)), true)
	if year >= 2022 {
		add(observed(year, time.June, 19, true))
	}
	add(observed(year, time.July, 4, true))
	add(date(time.September, nthWeekday(year, time.September, time.Monday, 1)), true)
	add(date(time.November, nthWeekday(year, time.November, time.Thursday, 4)), true)
	add(observed(year, time.December, 25, true))
	return out
}

// halfDayDates returns the 13:00 ET early-close dates of a year.
func halfDayDates(year int) map[string]bool {
	out := map[string]bool{}
	hol := holidayDates(year)
	add := func(t time.Time) {
		key := t.Format("2006-01-02")
		if wd := t.Weekday(); wd != time.Saturday && wd != time.Sunday && !hol[key] {
			out[key] = true
		}
	}
	thanksgiving := time.Date(year, time.November, nthWeekday(year, time.November, time.Thursday, 4), 12, 0, 0, 0, time.UTC)
	add(thanksgiving.AddDate(0, 0, 1))
	add(time.Date(year, time.December, 24, 12, 0, 0, 0, time.UTC))
	jul4 := time.Date(year, time.July, 4, 12, 0, 0, 0, time.UTC)
	if wd := jul4.Weekday(); wd >= time.Tuesday && wd <= time.Friday {
		add(jul4.AddDate(0, 0, -1))
	}
	return out
}

var calendarCache = struct {
	sync.Mutex
	holidays, halfDays map[int]map[string]bool
}{holidays: map[int]map[string]bool{}, halfDays: map[int]map[string]bool{}}

func yearSet(cache map[int]map[string]bool, year int, compute func(int) map[string]bool) map[string]bool {
	calendarCache.Lock()
	defer calendarCache.Unlock()
	if set, ok := cache[year]; ok {
		return set
	}
	set := compute(year)
	cache[year] = set
	return set
}

func isHoliday(t time.Time) bool {
	t = t.In(newYork)
	return yearSet(calendarCache.holidays, t.Year(), holidayDates)[t.Format("2006-01-02")]
}

func isHalfDay(t time.Time) bool {
	t = t.In(newYork)
	return yearSet(calendarCache.halfDays, t.Year(), halfDayDates)[t.Format("2006-01-02")]
}

// IsTradingDay uses the ET date and the rule-computed NYSE calendar (any year).
func IsTradingDay(t time.Time) bool {
	t = t.In(newYork)
	return t.Weekday() != time.Saturday && t.Weekday() != time.Sunday && !isHoliday(t)
}

func etTime(t time.Time, hour, minute int) time.Time {
	t = t.In(newYork)
	return time.Date(t.Year(), t.Month(), t.Day(), hour, minute, 0, 0, newYork)
}

// SessionBounds returns regular-session bounds for t's ET date.
func SessionBounds(t time.Time) (open, close time.Time, ok bool) {
	if !IsTradingDay(t) {
		return time.Time{}, time.Time{}, false
	}
	hour := 16
	if isHalfDay(t) {
		hour = 13
	}
	return etTime(t, 9, 30), etTime(t, hour, 0), true
}

// SessionAt returns the session using inclusive starts and exclusive ends.
func SessionAt(t time.Time) Session {
	open, close, ok := SessionBounds(t)
	if !ok || t.Before(etTime(t, 4, 0)) || !t.Before(etTime(t, 20, 0)) {
		return SessionClosed
	}
	if t.Before(open) {
		return SessionPre
	}
	if t.Before(close) {
		return SessionRegular
	}
	return SessionAfter
}

func sessionStart(t time.Time, session Session) time.Time {
	open, close, _ := SessionBounds(t)
	switch session {
	case SessionPre:
		return etTime(t, 4, 0)
	case SessionRegular:
		return open
	case SessionAfter:
		return close
	default:
		return time.Time{}
	}
}

// NextClose returns the first regular-session close strictly after t,
// skipping weekends and holidays (half days close at 13:00 ET).
func NextClose(t time.Time) time.Time {
	day := t.In(newYork)
	for i := 0; i < 15; i++ { // the longest closure run is far below 15 days
		if _, close, ok := SessionBounds(day.AddDate(0, 0, i)); ok && close.After(t) {
			return close
		}
	}
	return time.Time{}
}

// NextTradingDay returns noon ET of the first trading day strictly after t's ET date.
func NextTradingDay(t time.Time) time.Time {
	day := etTime(t, 12, 0)
	for i := 1; i <= 15; i++ {
		if d := day.AddDate(0, 0, i); IsTradingDay(d) {
			return d
		}
	}
	return time.Time{}
}

// CrossedClose reports whether a regular-session close lies in (from, to].
func CrossedClose(from, to time.Time) bool {
	if !to.After(from) {
		return false
	}
	next := NextClose(from)
	return !next.IsZero() && !next.After(to)
}
