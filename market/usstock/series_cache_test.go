package usstock

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// dailyServer serves UTC-aligned daily klines up to (and including) the bar
// open at the UTC midnight at/before the server clock, like Binance does.
func dailyServer(t *testing.T, now time.Time, calls *int) {
	setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
		*calls++
		cur := nowFunc().UTC().Truncate(24 * time.Hour)
		times := []time.Time{cur.AddDate(0, 0, -3), cur.AddDate(0, 0, -2), cur.AddDate(0, 0, -1), cur}
		return response(200, klineRows(times)), nil
	})
}

func TestBStockDailyCacheRefreshesAcrossUTCBarClose(t *testing.T) {
	loaded := time.Date(2026, 10, 14, 23, 50, 0, 0, time.UTC) // Wed, forming daily bar
	calls := 0
	dailyServer(t, loaded, &calls)
	series, err := GetSeries(context.Background(), "AAPLBUSDT", TF1d, 3, false)
	last := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	if err != nil || calls != 1 || !series.Bars[2].OpenTime.Equal(last) {
		t.Fatalf("first load: %+v calls=%d err=%v", series, calls, err)
	}
	nowFunc = func() time.Time { return loaded.Add(5 * time.Minute) } // still inside the bar
	if _, err = GetSeries(context.Background(), "AAPLBUSDT", TF1d, 3, false); err != nil || calls != 1 {
		t.Fatalf("same bar must hit cache: calls=%d err=%v", calls, err)
	}
	nowFunc = func() time.Time { return loaded.Add(15 * time.Minute) } // 00:05 UTC, bar closed, TTL 30m not over
	series, err = GetSeries(context.Background(), "AAPLBUSDT", TF1d, 3, false)
	if err != nil || calls != 2 || !series.Bars[2].OpenTime.Equal(last.AddDate(0, 0, 1)) {
		t.Fatalf("expected refetch with the just-closed bar: %+v calls=%d err=%v", series, calls, err)
	}
}

func TestBStockDailyCacheRefreshesAcrossUSCloseButNotOnHoliday(t *testing.T) {
	for _, tc := range []struct {
		name      string
		loaded    time.Time
		wantCalls int
	}{
		{"regular day crosses 16:00 ET", et("2026-11-25 15:50"), 2},
		{"Thanksgiving has no close", et("2026-11-26 15:50"), 1},
		{"half day crosses 13:00 ET", et("2026-11-27 12:50"), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			dailyServer(t, tc.loaded, &calls)
			if _, err := GetSeries(context.Background(), "AAPLBUSDT", TF1d, 3, false); err != nil || calls != 1 {
				t.Fatalf("load calls=%d err=%v", calls, err)
			}
			nowFunc = func() time.Time { return tc.loaded.Add(15 * time.Minute) }
			if _, err := GetSeries(context.Background(), "AAPLBUSDT", TF1d, 3, false); err != nil || calls != tc.wantCalls {
				t.Fatalf("calls=%d want %d err=%v", calls, tc.wantCalls, err)
			}
		})
	}
}

func TestBStockBarEndAlignment(t *testing.T) {
	for _, tc := range []struct {
		tf   string
		at   time.Time
		want time.Time
	}{
		{TF15m, time.Date(2026, 10, 14, 10, 7, 0, 0, time.UTC), time.Date(2026, 10, 14, 10, 15, 0, 0, time.UTC)},
		{TF4h, time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC), time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)},
		{TF1d, time.Date(2026, 10, 14, 23, 59, 0, 0, time.UTC), time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)},
		{TF1d, time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)},
		// Wed 2026-10-14 -> weekly bar ends Mon 2026-10-19 00:00 UTC.
		{TF1w, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)},
		{TF1w, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)},
	} {
		if got := bstockBarEnd(tc.tf, durations[tc.tf], tc.at); !got.Equal(tc.want) {
			t.Errorf("%s %v: got %v want %v", tc.tf, tc.at, got, tc.want)
		}
	}
}
