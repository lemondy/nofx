package usstock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestYahoo429RetryAndNullBars(t *testing.T) {
	now := et("2026-10-09 16:00")
	times := []time.Time{et("2026-10-08 09:30"), et("2026-10-08 10:30"), et("2026-10-08 11:30")}
	var chart map[string]any
	if err := json.Unmarshal([]byte(chartJSON(times, false)), &chart); err != nil {
		t.Fatal(err)
	}
	q := chart["chart"].(map[string]any)["result"].([]any)[0].(map[string]any)["indicators"].(map[string]any)["quote"].([]any)[0].(map[string]any)
	q["high"].([]any)[1] = nil
	calls := 0
	setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "spot.test" {
			return response(200, `[]`), nil
		}
		calls++
		if calls == 1 {
			return response(429, `{}`), nil
		}
		return response(200, encode(chart)), nil
	})
	series, err := GetSeries(context.Background(), "AAPLBUSDT", TF1h, 2, true)
	if err != nil || series.Source != SourceYahoo || len(series.Bars) != 2 || calls != 2 {
		t.Fatalf("series=%+v err=%v calls=%d", series, err, calls)
	}
	if series.Bars[1].OpenTime != times[2].UTC() {
		t.Fatal("null bar was retained")
	}
	requireAscending(t, series.Bars)
}
func TestYahooFallbackHostAndRetryLimit(t *testing.T) {
	calls := map[string]int{}
	setupHTTP(t, et("2026-10-09 16:00"), func(r *http.Request) (*http.Response, error) {
		calls[r.URL.Host]++
		if r.URL.Host == "query1.test" {
			return response(503, `{}`), nil
		}
		return response(200, chartJSON([]time.Time{et("2026-10-08 09:30")}, false)), nil
	})
	bars, err := yahooBars(context.Background(), "AAPL", TF1h, 1)
	if err != nil || len(bars) != 1 || calls["query1.test"] != 3 || calls["query2.test"] != 1 {
		t.Fatalf("bars=%v err=%v calls=%v", bars, err, calls)
	}
}
func TestYahooInsufficientChosenSource(t *testing.T) {
	setupHTTP(t, et("2026-10-09 16:00"), func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "spot.test" {
			return response(200, klineRows([]time.Time{et("2026-10-08 09:30")})), nil
		}
		return response(200, chartJSON([]time.Time{et("2026-10-07 09:30")}, true)), nil
	})
	series, err := GetSeries(context.Background(), "AAPLBUSDT", TF1d, 10, true)
	if !errors.Is(err, ErrInsufficientBars) || series.Source != SourceYahoo || len(series.Bars) != 1 {
		t.Fatalf("series=%v err=%v", series, err)
	}
}
func TestYahoo4hSessionAggregation(t *testing.T) {
	for _, tc := range []struct {
		date    string
		hours   int
		buckets int
	}{{"2026-10-08", 7, 2}, {"2026-11-27", 4, 1}} {
		t.Run(tc.date, func(t *testing.T) {
			now := et(tc.date + " 16:00")
			times := make([]time.Time, tc.hours)
			for i := range times {
				times[i] = et(tc.date + " 09:30").Add(time.Duration(i) * time.Hour)
			}
			setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "spot.test" {
					return response(200, `[]`), nil
				}
				if r.URL.Query().Get("interval") != "60m" {
					t.Fatal("Yahoo 4h must request 60m")
				}
				return response(200, chartJSON(times, false)), nil
			})
			series, err := GetSeries(context.Background(), "AAPLBUSDT", TF4h, tc.buckets, true)
			if err != nil || series.Source != SourceYahoo || len(series.Bars) != tc.buckets {
				t.Fatalf("series=%v err=%v", series, err)
			}
			first := series.Bars[0]
			if first.Open != 100 || first.High != 113 || first.Low != 90 || first.Close != 108 || first.Volume != 206 || first.OpenTime != times[0].UTC() {
				t.Fatalf("first bucket: %+v", first)
			}
			if tc.buckets == 2 {
				last := series.Bars[1]
				if last.Open != 104 || last.Close != 111 || last.Volume != 165 || last.OpenTime != times[4].UTC() {
					t.Fatalf("last bucket: %+v", last)
				}
			}
			requireAscending(t, series.Bars)
		})
	}
}
func TestYahooFormingBarRules(t *testing.T) {
	for _, tc := range []struct {
		name, tf, now string
		times         []time.Time
		want          int
	}{
		{"daily before close", TF1d, "2026-10-09 15:59", []time.Time{et("2026-10-08 09:30"), et("2026-10-09 09:30")}, 1},
		{"daily at close", TF1d, "2026-10-09 16:00", []time.Time{et("2026-10-08 09:30"), et("2026-10-09 09:30")}, 2},
		{"half day before close", TF1d, "2026-11-27 12:59", []time.Time{et("2026-11-25 09:30"), et("2026-11-27 09:30")}, 1},
		{"half day at close", TF1d, "2026-11-27 13:00", []time.Time{et("2026-11-25 09:30"), et("2026-11-27 09:30")}, 2},
		{"current week settled", TF1w, "2026-10-09 17:00", []time.Time{et("2026-09-28 09:30"), et("2026-10-05 09:30")}, 2},
		{"weekly Monday DST", TF1w, "2026-11-02 04:00", []time.Time{et("2026-10-26 09:30"), et("2026-11-02 09:30")}, 1},
		{"hourly forming", TF1h, "2026-10-09 11:00", []time.Time{et("2026-10-09 09:30"), et("2026-10-09 10:30")}, 1},
		{"hourly closing partial", TF1h, "2026-10-09 16:00", []time.Time{et("2026-10-09 14:30"), et("2026-10-09 15:30")}, 2},
		{"15m forming", TF15m, "2026-10-09 10:00", []time.Time{et("2026-10-09 09:45"), et("2026-10-09 10:00")}, 1},
		{"4h forming", TF4h, "2026-10-09 13:29", []time.Time{et("2026-10-09 09:30"), et("2026-10-09 10:30"), et("2026-10-09 11:30"), et("2026-10-09 12:30")}, 0},
		{"4h first bucket closed", TF4h, "2026-10-09 13:30", []time.Time{et("2026-10-09 09:30"), et("2026-10-09 10:30"), et("2026-10-09 11:30"), et("2026-10-09 12:30"), et("2026-10-09 13:30")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupHTTP(t, et(tc.now), func(r *http.Request) (*http.Response, error) {
				return response(200, chartJSON(tc.times, tc.tf == TF1d || tc.tf == TF1w)), nil
			})
			bars, err := yahooBars(context.Background(), "AAPL", tc.tf, 2)
			if err != nil || len(bars) != tc.want {
				t.Fatalf("bars=%+v err=%v want=%d", bars, err, tc.want)
			}
		})
	}
}
func TestYahooParamsAndCacheTTLs(t *testing.T) {
	for _, tc := range []struct {
		tf, interval string
		maxDays      int
		ttl          time.Duration
	}{
		{TF15m, "15m", 60, 5 * time.Minute}, {TF1h, "60m", 730, 15 * time.Minute}, {TF4h, "60m", 730, 15 * time.Minute}, {TF1d, "1d", 0, 6 * time.Hour}, {TF1w, "1wk", 0, 6 * time.Hour},
	} {
		interval, chartRange, ttl := yahooParams(tc.tf, 100000)
		if interval != tc.interval || ttl != tc.ttl {
			t.Fatalf("%s params=%s %s %v", tc.tf, interval, chartRange, ttl)
		}
		if tc.maxDays > 0 && chartRange != map[int]string{60: "60d", 730: "730d"}[tc.maxDays] {
			t.Fatalf("uncapped range %s", chartRange)
		}
	}
	now := et("2026-10-09 16:00")
	calls := 0
	setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
		calls++
		return response(200, chartJSON([]time.Time{et("2026-10-08 09:30")}, true)), nil
	})
	for i := 0; i < 2; i++ {
		if _, err := yahooBars(context.Background(), "AAPL", TF1d, 1); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("Yahoo cache missed")
	}
	nowFunc = func() time.Time { return now.Add(6 * time.Hour) }
	if _, err := yahooBars(context.Background(), "AAPL", TF1d, 1); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("Yahoo cache did not expire")
	}
}
func TestYahooSerializationAndCanceledQueue(t *testing.T) {
	var active, maxActive atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	setupHTTP(t, et("2026-10-09 16:00"), func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		if n > maxActive.Load() {
			maxActive.Store(n)
		}
		once.Do(func() { close(entered) })
		<-release
		active.Add(-1)
		return response(200, chartJSON([]time.Time{et("2026-10-08 09:30")}, false)), nil
	})
	done := make(chan error, 2)
	go func() { _, err := yahooBars(context.Background(), "AAPL", TF1h, 1); done <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := yahooBars(ctx, "SPY", TF1h, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued canceled request: %v", err)
	}
	go func() { _, err := yahooBars(context.Background(), "SPY", TF1h, 1); done <- err }()
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if maxActive.Load() != 1 {
		t.Fatalf("concurrent Yahoo requests=%d", maxActive.Load())
	}
}
func TestYahooCanceledBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	setupHTTP(t, et("2026-10-09 16:00"), func(r *http.Request) (*http.Response, error) { cancel(); return response(429, `{}`), nil })
	retryBaseDelay = time.Hour
	if _, err := yahooBars(ctx, "AAPL", TF1h, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
}

// Review 2026-10-20 #4: a daily response cached while today's bar was still
// forming must not be served after the close — yahooBars would then accept the
// intraday bar as the final daily bar for up to the 6h TTL.
func TestYahooDailyCacheRefreshedAfterSessionClose(t *testing.T) {
	times := []time.Time{et("2026-10-08 09:30"), et("2026-10-09 09:30")}
	calls := 0
	setupHTTP(t, et("2026-10-09 15:00"), func(r *http.Request) (*http.Response, error) {
		calls++
		return response(200, chartJSON(times, true)), nil
	})
	bars, err := yahooBars(context.Background(), "AAPL", TF1d, 2)
	if err != nil || len(bars) != 1 {
		t.Fatalf("15:00 ET: today's forming bar must be excluded: bars=%d err=%v", len(bars), err)
	}
	// Still before the close: same cache entry is fine.
	nowFunc = func() time.Time { return et("2026-10-09 15:30") }
	if _, err := yahooBars(context.Background(), "AAPL", TF1d, 2); err != nil || calls != 1 {
		t.Fatalf("pre-close hit should be cached: calls=%d err=%v", calls, err)
	}
	// After the close the entry (loaded at 15:00) must be refetched, not reused.
	nowFunc = func() time.Time { return et("2026-10-09 16:30") }
	bars, err = yahooBars(context.Background(), "AAPL", TF1d, 2)
	if err != nil || calls != 2 || len(bars) != 2 {
		t.Fatalf("post-close must refetch final bar: calls=%d bars=%d err=%v", calls, len(bars), err)
	}
	// The entry loaded at 16:30 is final and stays cached.
	nowFunc = func() time.Time { return et("2026-10-09 17:00") }
	if _, err := yahooBars(context.Background(), "AAPL", TF1d, 2); err != nil || calls != 2 {
		t.Fatalf("post-close entry should stay cached: calls=%d err=%v", calls, err)
	}
}

func TestCacheStillFinal(t *testing.T) {
	cases := []struct {
		name        string
		interval    string
		loaded, now string
		want        bool
	}{
		{"1d same day before close", "1d", "2026-10-09 15:00", "2026-10-09 15:59", true},
		{"1d crosses close", "1d", "2026-10-09 15:00", "2026-10-09 16:00", false},
		{"1d crosses to next day", "1d", "2026-10-08 11:00", "2026-10-09 09:00", false},
		{"1d loaded after close", "1d", "2026-10-09 16:30", "2026-10-09 22:00", true},
		{"1d loaded on weekend", "1d", "2026-10-10 12:00", "2026-10-12 10:00", true},
		{"1wk mid-week to next week", "1wk", "2026-10-07 12:00", "2026-10-13 12:00", false},
		{"1wk mid-week same week", "1wk", "2026-10-07 12:00", "2026-10-08 12:00", true},
		{"1wk loaded after friday close", "1wk", "2026-10-09 17:00", "2026-10-13 12:00", true},
		{"60m same bar", "60m", "2026-10-09 10:00", "2026-10-09 10:25", true},
		{"60m next bar", "60m", "2026-10-09 11:29", "2026-10-09 11:35", false},
		{"15m crosses bar", "15m", "2026-10-09 10:14", "2026-10-09 10:16", false},
		{"15m premarket load", "15m", "2026-10-09 08:00", "2026-10-09 09:50", true},
	}
	for _, c := range cases {
		if got := cacheStillFinal(c.interval, et(c.loaded), et(c.now)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
