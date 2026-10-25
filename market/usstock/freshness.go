package usstock

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CloseDataGrace allows one missing settled session (or week) while vendors
// publish final bars, for 90 minutes after the NYSE close, including half days.
const CloseDataGrace = 90 * time.Minute

var ErrStaleBars = errors.New("stale closed bars")

type forceRefreshKey struct{}

// WithForceRefresh bypasses cache reads for this request; successful fetches
// still update the shared cache.
func WithForceRefresh(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceRefreshKey{}, true)
}
func forceRefresh(ctx context.Context) bool { v, _ := ctx.Value(forceRefreshKey{}).(bool); return v }

// LatestClosedTradingDay returns the most recent NYSE regular-session close.
func LatestClosedTradingDay(now time.Time) time.Time {
	for day, i := etTime(now, 12, 0), 0; i < 20; i, day = i+1, day.AddDate(0, 0, -1) {
		if _, close, ok := SessionBounds(day); ok && !close.After(now) {
			return close
		}
	}
	return time.Time{}
}

func weekClose(day time.Time) time.Time {
	local := day.In(newYork)
	monday := etTime(local, 12, 0).AddDate(0, 0, -((int(local.Weekday()) + 6) % 7))
	for i := 4; i >= 0; i-- {
		if _, close, ok := SessionBounds(monday.AddDate(0, 0, i)); ok {
			return close
		}
	}
	return time.Time{}
}

func latestWeekClose(now time.Time) time.Time {
	close := weekClose(now)
	if close.After(now) {
		close = weekClose(now.AddDate(0, 0, -7))
	}
	return close
}

// RequiredDataDate is the minimum settled NYSE date a daily/weekly series must
// cover now. Subtracting the grace period permits exactly one just-closed bar.
func RequiredDataDate(timeframe string, now time.Time) string {
	if now.IsZero() {
		return ""
	}
	at := now.Add(-CloseDataGrace)
	switch timeframe {
	case TF1d:
		return LatestClosedTradingDay(at).In(newYork).Format("2006-01-02")
	case TF1w:
		return latestWeekClose(at).In(newYork).Format("2006-01-02")
	default:
		return ""
	}
}

// DataDate reports the last bar's session date (weekly: last NYSE session of
// that week). Binance daily/weekly labels are UTC dates, not ET timestamps.
func DataDate(s *Series) string {
	if s == nil || len(s.Bars) == 0 {
		return ""
	}
	day := s.Bars[len(s.Bars)-1].OpenTime.In(newYork)
	if s.Source == SourceBStock {
		u := s.Bars[len(s.Bars)-1].OpenTime.UTC()
		day = time.Date(u.Year(), u.Month(), u.Day(), 12, 0, 0, 0, newYork)
	}
	if s.Timeframe == TF1w {
		day = weekClose(day)
	}
	return day.Format("2006-01-02")
}

func CheckDataDate(timeframe, cutoff string, now time.Time) error {
	if required := RequiredDataDate(timeframe, now); required != "" && (cutoff == "" || cutoff < required) {
		return fmt.Errorf("%w: %s data_through=%s required=%s", ErrStaleBars, timeframe, cutoff, required)
	}
	return nil
}
func CheckSeriesFreshness(s *Series, now time.Time) error {
	if s == nil {
		return fmt.Errorf("%w: series unavailable", ErrStaleBars)
	}
	return CheckDataDate(s.Timeframe, DataDate(s), now)
}

// RegularDailyBars requires actual regular-session opens and consecutive NYSE
// trading dates. UTC midnight / night / weekend bStock candles cannot establish
// equivalence to the underlying's 252-session momentum or SMA200.
func RegularDailyBars(bars []Bar) bool {
	for i, b := range bars {
		open, _, ok := SessionBounds(b.OpenTime)
		if !ok || !b.OpenTime.Equal(open) {
			return false
		}
		if i > 0 && NextTradingDay(bars[i-1].OpenTime).Format("2006-01-02") != b.OpenTime.In(newYork).Format("2006-01-02") {
			return false
		}
	}
	return len(bars) > 0
}

func freshBars(ctx context.Context, symbol, timeframe string, need int, source string) ([]Bar, error) {
	load := bstockBars
	if source == SourceYahoo {
		load = yahooBars
	}
	bars, err := load(ctx, symbol, timeframe, need)
	if timeframe != TF1d && timeframe != TF1w {
		return bars, err
	}
	s := &Series{Timeframe: timeframe, Source: source, Bars: bars}
	if stale := CheckSeriesFreshness(s, nowFunc()); stale != nil {
		if err == nil && len(bars) > 0 {
			refreshed, refreshErr := load(WithForceRefresh(ctx), symbol, timeframe, need)
			err = refreshErr
			if len(refreshed) > 0 || refreshErr == nil {
				bars = refreshed
			}
			s.Bars = bars
			stale = CheckSeriesFreshness(s, nowFunc())
		}
		err = errors.Join(err, stale)
	}
	return bars, err
}
