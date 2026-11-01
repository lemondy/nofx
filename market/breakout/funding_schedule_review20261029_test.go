package breakout

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestReview20261029FundingInfoSchedule(t *testing.T) {
	for _, tc := range []struct {
		name         string
		customBase   bool
		scheduled    bool
		gaps         []int64
		historyFails bool
		want         float64
	}{
		{"schedule wins over old settlements", false, true, []int64{8, 8, 1}, false, 1},
		{"unlisted symbol uses median of 8 1 1", false, false, []int64{8, 1, 1}, false, 1},
		// Known fallback lag: without exchange metadata, [8,8,1] still has
		// median 8; the old mean of 5.67 mixed two settlement schedules.
		{"unlisted symbol uses median of 8 8 1", false, false, []int64{8, 8, 1}, false, 8},
		{"no settlements defaults to eight", false, false, nil, false, 8},
		{"invalid gaps default to eight", false, false, []int64{0, -1, 25}, false, 8},
		{"history failure defaults to eight", false, false, nil, true, 8},
		{"custom base skips public metadata", true, true, []int64{8, 1, 1}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var premiumCalls, historyCalls atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodGet || r.URL.Query().Get("symbol") != "SCHEDULEREVIEWUSDT" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				switch r.URL.Path {
				case "/fapi/v1/premiumIndex":
					premiumCalls.Add(1)
					_, _ = w.Write([]byte(`{"lastFundingRate":"0.0001"}`))
				case "/fapi/v1/fundingRate":
					historyCalls.Add(1)
					if r.URL.Query().Get("limit") != "4" {
						t.Errorf("funding history limit = %q, want 4", r.URL.Query().Get("limit"))
					}
					if tc.historyFails {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					rows := []map[string]int64{}
					if len(tc.gaps) > 0 {
						at := int64(0)
						rows = append(rows, map[string]int64{"fundingTime": at})
						for _, gap := range tc.gaps {
							at += gap * time.Hour.Milliseconds()
							rows = append(rows, map[string]int64{"fundingTime": at})
						}
					}
					_ = json.NewEncoder(w).Encode(rows)
				default:
					t.Errorf("unexpected endpoint: %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			})

			oldHTTP, oldSchedule, oldEnabled := binanceHTTP, scheduleHours, cacheEnabled
			cacheEnabled = false
			t.Cleanup(func() { binanceHTTP, scheduleHours, cacheEnabled = oldHTTP, oldSchedule, oldEnabled })
			scheduleCalls := 0
			scheduleHours = func(symbol string) (float64, bool) {
				scheduleCalls++
				if tc.customBase {
					t.Fatal("custom base must not consult the public Binance schedule")
				}
				if symbol != "SCHEDULEREVIEWUSDT" {
					t.Fatalf("schedule symbol = %q", symbol)
				}
				return 1, tc.scheduled
			}
			base := defaultFAPIBase
			if tc.customBase {
				base = "https://schedule-review.test"
			}
			t.Setenv("BINANCE_FAPI_BASE", base)
			// Serve fake premiumIndex/fundingRate responses through httptest
			// entirely in memory: this sandbox also forbids localhost listeners.
			binanceHTTP = &http.Client{Transport: cacheRT(func(r *http.Request) (*http.Response, error) {
				if r.URL.Scheme+"://"+r.URL.Host != base {
					t.Errorf("unexpected upstream: %s", r.URL)
				}
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				return recorder.Result(), nil
			})}
			info, err := NewBinanceDS("SCHEDULEREVIEWUSDT").FundingInfo()
			if err != nil || info == nil {
				t.Fatalf("FundingInfo: info=%+v err=%v", info, err)
			}
			if info.IntervalHours != tc.want || info.NextRate != 0.0001 {
				t.Fatalf("FundingInfo = %+v, want interval=%v and rate=0.0001", info, tc.want)
			}
			wantScheduleCalls, wantHistoryCalls := 1, int32(1)
			if tc.customBase {
				wantScheduleCalls = 0
			} else if tc.scheduled {
				wantHistoryCalls = 0
			}
			if premiumCalls.Load() != 1 || historyCalls.Load() != wantHistoryCalls || scheduleCalls != wantScheduleCalls {
				t.Fatalf("calls: premium=%d history=%d schedule=%d", premiumCalls.Load(), historyCalls.Load(), scheduleCalls)
			}
		})
	}
}
