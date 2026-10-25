package usstock

import (
	"context"
	"math"
	"net/http"
	"testing"
	"time"
)

func quoteJSON(refTime time.Time, refPrice float64, barTimes []time.Time, closes []any) string {
	stamps := make([]int64, len(barTimes))
	for i, t := range barTimes {
		stamps[i] = t.Unix()
	}
	return encode(map[string]any{"chart": map[string]any{"result": []any{map[string]any{
		"meta":      map[string]any{"regularMarketPrice": refPrice, "regularMarketTime": refTime.Unix()},
		"timestamp": stamps, "indicators": map[string]any{"quote": []any{map[string]any{"close": closes}}},
	}}}})
}
func TestQuoteReferencesFreshnessAndDivergence(t *testing.T) {
	for _, tc := range []struct {
		name, now, refTime string
		session            Session
		fresh, prePost     bool
		refPrice           float64
	}{
		{"regular fresh", "2026-10-09 12:00", "2026-10-09 11:59", SessionRegular, true, false, 100},
		{"regular stale", "2026-10-09 12:00", "2026-10-09 11:44", SessionRegular, false, false, 100},
		{"regular previous session", "2026-10-09 09:31", "2026-10-09 09:29", SessionRegular, false, false, 100},
		{"pre uses latest 1m", "2026-10-09 08:00", "2026-10-09 07:59", SessionPre, true, true, 102},
		{"after uses latest 1m", "2026-10-09 17:00", "2026-10-09 16:59", SessionAfter, true, true, 102},
		{"half day after", "2026-11-27 13:05", "2026-11-27 13:04", SessionAfter, true, true, 102},
		{"pre previous day", "2026-10-09 04:05", "2026-10-08 19:59", SessionPre, false, true, 102},
		{"closed", "2026-10-10 12:00", "2026-10-09 15:59", SessionClosed, false, false, 100},
		{"future timestamp", "2026-10-09 12:00", "2026-10-09 12:01", SessionRegular, false, false, 100},
		{"unknown reference", "2026-10-09 12:00", "2026-10-09 11:59", SessionRegular, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refTime := et(tc.refTime)
			setupHTTP(t, et(tc.now), func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "spot.test" {
					if r.URL.Path != "/api/v3/ticker/price" || r.URL.Query().Get("symbol") != "AAPLBUSDT" {
						t.Fatal("ticker request")
					}
					return response(200, `{"price":"103"}`), nil
				}
				want := "false"
				if tc.prePost {
					want = "true"
				}
				if r.URL.Query().Get("includePrePost") != want || r.URL.Query().Get("interval") != "1m" || r.URL.Query().Get("range") != "1d" {
					t.Fatalf("quote chart request %v", r.URL)
				}
				return response(200, quoteJSON(refTime, tc.refPrice, []time.Time{refTime.Add(-time.Minute), refTime, refTime.Add(time.Second)}, []any{101, tc.refPrice, nil})), nil
			})
			quote, err := GetQuote(context.Background(), "AAPLBUSDT")
			if err != nil {
				t.Fatal(err)
			}
			if quote.Session != tc.session || quote.RefFresh != tc.fresh || quote.RefPrice != tc.refPrice || !quote.RefTime.Equal(refTime) {
				t.Fatalf("quote=%+v", quote)
			}
			divergence := 0.0
			if tc.refPrice > 0 {
				divergence = (103 - tc.refPrice) / tc.refPrice * 100
			}
			if math.Abs(quote.DivergencePct-divergence) > 1e-10 {
				t.Fatalf("divergence %g want %g", quote.DivergencePct, divergence)
			}
		})
	}
}
func TestQuoteCacheRecomputesFreshnessAndSession(t *testing.T) {
	now := et("2026-10-09 15:59").Add(50 * time.Second)
	calls := 0
	setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host == "spot.test" {
			return response(200, `{"price":"100"}`), nil
		}
		return response(200, quoteJSON(now, 100, []time.Time{now}, []any{100})), nil
	})
	quote, err := GetQuote(context.Background(), "AAPLBUSDT")
	if err != nil || !quote.RefFresh || calls != 2 {
		t.Fatalf("quote=%v err=%v calls=%d", quote, err, calls)
	}
	quote.RefPrice = -1
	quote, err = GetQuote(context.Background(), "AAPLBUSDT")
	if err != nil || quote.RefPrice != 100 || calls != 2 {
		t.Fatalf("cache alias: quote=%v err=%v calls=%d", quote, err, calls)
	}
	nowFunc = func() time.Time { return now.Add(10 * time.Second) }
	quote, err = GetQuote(context.Background(), "AAPLBUSDT")
	if err != nil || quote.Session != SessionAfter || quote.RefFresh || calls != 4 {
		t.Fatalf("session transition: quote=%v err=%v calls=%d", quote, err, calls)
	}
}
func TestQuoteCacheExpiryAndFreshnessThreshold(t *testing.T) {
	now := et("2026-10-09 12:00")
	calls := 0
	setupHTTP(t, now, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host == "spot.test" {
			return response(200, `{"price":"100"}`), nil
		}
		return response(200, quoteJSON(now.Add(-15*time.Minute), 100, nil, nil)), nil
	})
	quote, err := GetQuote(context.Background(), "AAPLBUSDT")
	if err != nil || !quote.RefFresh {
		t.Fatalf("15m age: quote=%v err=%v", quote, err)
	}
	nowFunc = func() time.Time { return now.Add(time.Second) }
	quote, err = GetQuote(context.Background(), "AAPLBUSDT")
	if err != nil || quote.RefFresh || calls != 2 {
		t.Fatalf("freshness crossing: quote=%v err=%v calls=%d", quote, err, calls)
	}
	nowFunc = func() time.Time { return now.Add(30 * time.Second) }
	if _, err := GetQuote(context.Background(), "AAPLBUSDT"); err != nil || calls != 4 {
		t.Fatalf("cache expiry err=%v calls=%d", err, calls)
	}
}

func TestQuoteRequestCrossesSessionBoundary(t *testing.T) {
	start := et("2026-10-09 15:59").Add(59 * time.Second)
	finish := start.Add(2 * time.Second)
	setupHTTP(t, start, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "spot.test" {
			return response(200, `{"price":"100"}`), nil
		}
		nowFunc = func() time.Time { return finish }
		return response(200, quoteJSON(start, 100, []time.Time{start}, []any{100})), nil
	})
	quote, err := GetQuote(context.Background(), "AAPLBUSDT")
	if err != nil || quote.Session != SessionAfter || quote.RefFresh {
		t.Fatalf("delayed quote=%v err=%v", quote, err)
	}
}
