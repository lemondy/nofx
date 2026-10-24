package usstock

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestReview1024StaleDailyRefresh(t *testing.T) {
	calls := 0
	setupHTTP(t, et("2026-10-22 15:30"), func(r *http.Request) (*http.Response, error) {
		calls++
		return response(200, chartJSON([]time.Time{et("2026-10-20 09:30")}, true)), nil
	})
	s, err := GetUnderlyingSeries(context.Background(), "AAPLBUSDT", TF1d, 1)
	if err == nil || s == nil || calls != 2 {
		t.Fatalf("stale data accepted or not refreshed once: calls=%d series=%+v err=%v", calls, s, err)
	}
}

func TestReview1024YahooRequestCrossesClose(t *testing.T) {
	calls := 0
	setupHTTP(t, et("2026-10-22 15:59"), func(r *http.Request) (*http.Response, error) {
		calls++
		nowFunc = func() time.Time { return et("2026-10-22 16:01") }
		return response(200, chartJSON([]time.Time{et("2026-10-21 09:30"), et("2026-10-22 09:30")}, true)), nil
	})
	bars, err := yahooBars(context.Background(), "AAPL", TF1d, 2)
	if err != nil || len(bars) != 2 || calls != 2 {
		t.Fatalf("cross-close response not re-fetched: calls=%d bars=%d err=%v", calls, len(bars), err)
	}
}

func TestReview1024RequiredNYSEDate(t *testing.T) {
	for _, tc := range []struct{ tf, now, want string }{
		{TF1d, "2026-10-22 15:30", "2026-10-21"},
		{TF1d, "2026-10-22 17:29", "2026-10-21"},
		{TF1d, "2026-10-22 17:30", "2026-10-22"},
		{TF1d, "2026-10-24 10:00", "2026-10-23"},
		{TF1d, "2026-11-26 12:00", "2026-11-25"},
		{TF1d, "2026-11-27 14:29", "2026-11-25"},
		{TF1d, "2026-11-27 14:30", "2026-11-27"},
		{TF1d, "2026-03-09 10:00", "2026-03-06"},
		{TF1w, "2026-10-23 17:29", "2026-10-16"},
		{TF1w, "2026-10-23 17:30", "2026-10-23"},
		{TF1w, "2026-04-02 17:29", "2026-03-27"}, // Good Friday week ends Thursday
		{TF1w, "2026-04-02 17:30", "2026-04-02"},
		{TF1w, "2026-11-27 14:30", "2026-11-27"},
	} {
		t.Run(tc.tf+tc.now, func(t *testing.T) {
			if got := RequiredDataDate(tc.tf, et(tc.now)); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestReview1024StaleSourcesForceRefresh(t *testing.T) {
	for _, source := range []string{SourceYahoo, SourceBStock} {
		for _, tf := range []string{TF1d, TF1w} {
			for _, recover := range []bool{false, true} {
				t.Run(source+tf+fmt.Sprint(recover), func(t *testing.T) {
					calls := 0
					old, last := et("2026-10-20 09:30"), et("2026-10-23 09:30")
					if tf == TF1w {
						old, last = et("2026-10-12 09:30"), et("2026-10-19 09:30")
					}
					if source == SourceBStock {
						old = time.Date(old.Year(), old.Month(), old.Day(), 0, 0, 0, 0, time.UTC)
						last = time.Date(last.Year(), last.Month(), last.Day(), 0, 0, 0, 0, time.UTC)
					}
					setupHTTP(t, et("2026-10-26 10:00"), func(r *http.Request) (*http.Response, error) {
						calls++
						stamp := old
						if recover && calls > 1 {
							stamp = last
						}
						if source == SourceBStock {
							return response(200, klineRows([]time.Time{stamp})), nil
						}
						return response(200, chartJSON([]time.Time{stamp}, true)), nil
					})
					var s *Series
					var err error
					if source == SourceYahoo {
						s, err = GetUnderlyingSeries(context.Background(), "AAPLBUSDT", tf, 1)
					} else {
						s, err = GetSeries(context.Background(), "AAPLBUSDT", tf, 1, false)
					}
					if calls != 2 || s == nil || (err == nil) != recover || (!recover && !errors.Is(err, ErrStaleBars)) {
						t.Fatalf("calls=%d series=%+v err=%v", calls, s, err)
					}
					cutoff := DataDate(s)
					if recover && cutoff != "2026-10-23" {
						t.Fatalf("cutoff=%s", cutoff)
					}
				})
			}
		}
	}
}

func TestReview1024ForceRefreshBypassesYahooCache(t *testing.T) {
	calls := 0
	setupHTTP(t, et("2026-10-22 15:30"), func(r *http.Request) (*http.Response, error) {
		calls++
		day := et("2026-10-20 09:30")
		if calls > 1 {
			day = et("2026-10-21 09:30")
		}
		return response(200, chartJSON([]time.Time{day}, true)), nil
	})
	if _, err := yahooBars(context.Background(), "AAPL", TF1d, 1); err != nil {
		t.Fatal(err)
	}
	s, err := GetUnderlyingSeries(context.Background(), "AAPLBUSDT", TF1d, 1)
	if err != nil || calls != 2 || DataDate(s) != "2026-10-21" {
		t.Fatalf("cache not bypassed: calls=%d series=%+v err=%v", calls, s, err)
	}
}

func TestReview1024YahooWeeklyRequestCrossesHolidayClose(t *testing.T) {
	calls := 0
	setupHTTP(t, et("2026-04-02 15:59"), func(r *http.Request) (*http.Response, error) {
		calls++
		nowFunc = func() time.Time { return et("2026-04-02 16:01") }
		return response(200, chartJSON([]time.Time{et("2026-03-23 09:30"), et("2026-03-30 09:30")}, true)), nil
	})
	bars, err := yahooBars(context.Background(), "AAPL", TF1w, 2)
	if err != nil || calls != 2 || len(bars) != 2 {
		t.Fatalf("holiday weekly close not refreshed: calls=%d bars=%d err=%v", calls, len(bars), err)
	}
	for _, entry := range yahooCache {
		if !entry.loaded.Equal(et("2026-04-02 16:01")) {
			t.Fatalf("wrong request time: %v", entry.loaded)
		}
	}
}

func TestReview1024ExecutionQuoteBypassesBothCaches(t *testing.T) {
	spot, yahoo := 0, 0
	setupHTTP(t, et("2026-10-22 15:30"), func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "spot.test" {
			spot++
			return response(200, fmt.Sprintf(`{"price":"%d"}`, 100+spot)), nil
		}
		yahoo++
		return response(200, fmt.Sprintf(`{"chart":{"result":[{"meta":{"regularMarketPrice":100,"regularMarketTime":%d}}]}}`, et("2026-10-22 15:30").Unix())), nil
	})
	first, err := GetQuote(context.Background(), "AAPLBUSDT")
	if err != nil {
		t.Fatal(err)
	}
	_, err = GetQuote(context.Background(), "AAPLBUSDT")
	if err != nil || spot != 1 || yahoo != 1 {
		t.Fatal("normal cache broken")
	}
	final, err := GetQuote(WithForceRefresh(context.Background()), "AAPLBUSDT")
	if err != nil || spot != 2 || yahoo != 2 || final.BStockPrice == first.BStockPrice || final.BStockTime.IsZero() {
		t.Fatalf("execution reused cache: quote=%+v spot=%d yahoo=%d err=%v", final, spot, yahoo, err)
	}
}
