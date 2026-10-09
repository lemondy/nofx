package usstock

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestBStockEnoughDropsFormingAndCaches(t *testing.T) {
	now := time.Date(2026, 10, 9, 14, 15, 0, 0, time.UTC)
	times := []time.Time{now.Add(-45 * time.Minute), now.Add(-30 * time.Minute), now.Add(-15 * time.Minute), now}
	calls := 0
	setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/v3/klines" || r.URL.Query().Get("interval") != TF15m {
			t.Fatalf("unexpected request %v", r.URL)
		}
		return response(200, klineRows(times)), nil
	})
	series, err := GetSeries(context.Background(), "AAPLBUSDT", TF15m, 3, true)
	if err != nil || series.Source != SourceBStock || len(series.Bars) != 3 || series.Bars[2].OpenTime != now.Add(-15*time.Minute) {
		t.Fatalf("series=%+v err=%v", series, err)
	}
	requireAscending(t, series.Bars)
	series.Bars[0].Close = -1
	series, err = GetSeries(context.Background(), "AAPLBUSDT", TF15m, 3, true)
	if err != nil || calls != 1 || series.Bars[0].Close < 0 {
		t.Fatalf("cache: %+v calls=%d err=%v", series, calls, err)
	}
	nowFunc = func() time.Time { return now.Add(time.Minute) }
	if _, err := GetSeries(context.Background(), "AAPLBUSDT", TF15m, 3, false); err != nil || calls != 2 {
		t.Fatalf("cache expiry calls=%d err=%v", calls, err)
	}
}
func TestBStockShortYahooWholeAdjustedSeries(t *testing.T) {
	now := et("2026-10-09 17:00")
	spotTimes := make([]time.Time, 73)
	for i := range spotTimes {
		spotTimes[i] = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
	}
	yahooTimes := make([]time.Time, 230)
	for i := range yahooTimes {
		yahooTimes[i] = et("2025-01-02 09:30").AddDate(0, 0, i)
	}
	spotCalls, yahooCalls := 0, 0
	setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "spot.test" {
			spotCalls++
			if r.URL.Query().Get("endTime") != "" {
				return response(200, `[]`), nil
			}
			return response(200, klineRows(spotTimes)), nil
		}
		yahooCalls++
		if r.Header.Get("User-Agent") != "Mozilla/5.0" || r.URL.Query().Get("includePrePost") != "false" || r.URL.Query().Get("events") != "div,split" || r.URL.Query().Get("interval") != "1d" {
			t.Fatalf("Yahoo params: %v", r)
		}
		return response(200, chartJSON(yahooTimes, true)), nil
	})
	series, err := GetSeries(context.Background(), "AAPLBUSDT", TF1d, 220, true)
	if err != nil || series.Source != SourceYahoo || len(series.Bars) != 220 || series.Underlying != "AAPL" {
		t.Fatalf("series=%+v err=%v", series, err)
	}
	first := series.Bars[0]
	if first.Open != 55 || first.High != 60 || first.Low != 50 || first.Close != 57.5 || first.Volume != 60 {
		t.Fatalf("adjusted OHLC/raw volume: %+v", first)
	}
	requireAscending(t, series.Bars)
	if spotCalls != 2 || yahooCalls != 1 {
		t.Fatalf("calls spot=%d Yahoo=%d", spotCalls, yahooCalls)
	}
	series, err = GetSeries(context.Background(), "AAPLBUSDT", TF1d, 220, false)
	if !errors.Is(err, ErrInsufficientBars) || series.Source != SourceBStock || len(series.Bars) != 73 {
		t.Fatalf("no fallback: %+v err=%v", series, err)
	}
	if yahooCalls != 1 {
		t.Fatal("unexpected Yahoo call")
	}
	series, err = GetSeries(context.Background(), "AAPLBUSDT", TF1d, 220, true)
	if err != nil || yahooCalls != 1 || spotCalls != 2 {
		t.Fatalf("cached series calls=%d/%d err=%v", spotCalls, yahooCalls, err)
	}
}
func TestKlinePaginationStitchesWithoutDuplicates(t *testing.T) {
	now := et("2026-10-09 17:00")
	start := now.Add(-8 * time.Hour)
	times := make([]time.Time, 6)
	for i := range times {
		times[i] = start.Add(time.Duration(i) * time.Hour)
	}
	calls := 0
	setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
		calls++
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit > 1000 {
			t.Fatal("limit over 1000")
		}
		switch calls {
		case 1:
			return response(200, klineRows(times[3:])), nil
		case 2:
			want := strconv.FormatInt(times[3].UnixMilli()-1, 10)
			if r.URL.Query().Get("endTime") != want {
				t.Fatalf("endTime %s want %s", r.URL.Query().Get("endTime"), want)
			}
			return response(200, klineRows(times[:4])), nil // intentional overlap
		default:
			t.Fatal("unexpected page")
			return nil, nil
		}
	})
	series, err := GetSeries(context.Background(), "AAPLBUSDT", TF1h, 6, false)
	if err != nil || len(series.Bars) != 6 || calls != 2 {
		t.Fatalf("series=%v err=%v calls=%d", series, err, calls)
	}
	requireAscending(t, series.Bars)
}
func TestKlineRequestCapAndNativeIntervals(t *testing.T) {
	for _, tf := range []string{TF15m, TF1h, TF4h, TF1d, TF1w} {
		t.Run(tf, func(t *testing.T) {
			now := et("2026-10-09 17:00")
			calls := 0
			setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Query().Get("interval") != tf {
					t.Fatal("not native interval")
				}
				stamp := now.Add(-time.Duration(calls) * durations[tf])
				return response(200, klineRows([]time.Time{stamp})), nil
			})
			series, err := GetSeries(context.Background(), "AAPLBUSDT", tf, 6000, false)
			if !errors.Is(err, ErrInsufficientBars) || calls != maxKlineRequests || len(series.Bars) != 5 {
				t.Fatalf("cap series=%v err=%v calls=%d", series, err, calls)
			}
		})
	}
}
func TestGetSeriesValidation(t *testing.T) {
	setupHTTP(t, et("2026-10-09 12:00"), nil)
	for _, tc := range []struct {
		symbol, tf string
		need       int
	}{{"AAPLBUSDT", "5m", 1}, {"AAPLBUSDT", TF1d, 0}, {"BNBUSDT", TF1h, 1}} {
		if _, err := GetSeries(context.Background(), tc.symbol, tc.tf, tc.need, true); err == nil {
			t.Errorf("accepted %+v", tc)
		}
	}
}
