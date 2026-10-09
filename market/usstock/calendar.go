package usstock

import (
	"log"
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
var calendarWarning sync.Once
var holidays = dateSet(
	"2026-01-01", "2026-01-19", "2026-02-16", "2026-04-03", "2026-05-25", "2026-06-19", "2026-07-03", "2026-09-07", "2026-11-26", "2026-12-25",
	"2027-01-01", "2027-01-18", "2027-02-15", "2027-03-26", "2027-05-31", "2027-06-18", "2027-07-05", "2027-09-06", "2027-11-25", "2027-12-24",
	"2028-01-17", "2028-02-21", "2028-04-14", "2028-05-29", "2028-06-19", "2028-07-04", "2028-09-04", "2028-11-23", "2028-12-25",
)
var halfDays = dateSet("2026-11-27", "2026-12-24", "2027-11-26", "2028-07-03", "2028-11-24")

func dateSet(dates ...string) map[string]bool {
	result := make(map[string]bool, len(dates))
	for _, date := range dates {
		result[date] = true
	}
	return result
}

// IsTradingDay uses the ET date and the built-in 2026–2028 NYSE calendar.
// Outside those years weekdays are assumed trading days; later years warn once.
func IsTradingDay(t time.Time) bool {
	t = t.In(newYork)
	if t.Year() > 2028 {
		calendarWarning.Do(func() {
			log.Printf("usstock: NYSE calendar ends in 2028; assuming weekdays are trading days; update holiday table")
		})
	}
	return t.Weekday() != time.Saturday && t.Weekday() != time.Sunday && !holidays[t.Format("2006-01-02")]
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
	if halfDays[t.In(newYork).Format("2006-01-02")] {
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
