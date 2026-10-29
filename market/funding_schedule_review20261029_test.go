package market

import (
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func review20261029Schedule(t *testing.T, fetch func() (map[string]float64, error)) {
	t.Helper()
	fundingScheduleCache.Lock()
	oldHours, oldFetchedAt, oldFailedAt := fundingScheduleCache.hours, fundingScheduleCache.fetchedAt, fundingScheduleCache.failedAt
	oldFetch := fundingScheduleFetch
	fundingScheduleCache.hours = nil
	fundingScheduleCache.fetchedAt, fundingScheduleCache.failedAt = time.Time{}, time.Time{}
	fundingScheduleFetch = fetch
	fundingScheduleCache.Unlock()
	t.Cleanup(func() {
		fundingScheduleCache.Lock()
		fundingScheduleCache.hours = oldHours
		fundingScheduleCache.fetchedAt, fundingScheduleCache.failedAt = oldFetchedAt, oldFailedAt
		fundingScheduleFetch = oldFetch
		fundingScheduleCache.Unlock()
	})
}

func review20261029FundingTimes(gaps ...int64) []int64 {
	times := []int64{0}
	for _, gap := range gaps {
		times = append(times, times[len(times)-1]+gap*time.Hour.Milliseconds())
	}
	return times
}

func TestReview20261029FundingIntervalSources(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scheduled  float64
		gaps       []int64
		cached     float64
		cacheAge   time.Duration
		want       float64
		recomputed bool
	}{
		{"schedule overrides old history and cache", 1, []int64{8, 8, 8, 8, 1}, 8, 20 * time.Minute, 1, false},
		{"unlisted symbol uses median", 0, []int64{8, 1, 1}, 0, 0, 1, true},
		{"latest gap invalidates fresh cache", 0, []int64{8, 1, 1}, 8, 20 * time.Minute, 1, true},
		{"recompute even while median still lags", 0, []int64{8, 8, 8, 8, 1}, 8, 20 * time.Minute, 8, true},
		{"matching latest gap keeps fresh cache", 0, []int64{8, 8, 4}, 4, 20 * time.Minute, 4, false},
		{"history cache expires after one hour", 0, []int64{8, 8, 4}, 4, 61 * time.Minute, 8, true},
		{"no history remains unknown", 0, nil, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const symbol = "SCHEDULEREVIEWUSDT"
			review20261029Schedule(t, func() (map[string]float64, error) {
				table := map[string]float64{"OTHERUSDT": 4}
				if tc.scheduled > 0 {
					table[symbol] = tc.scheduled
				}
				return table, nil
			})
			fundingHistoryMap.Store(symbol, &fundingHistoryCache{Times: review20261029FundingTimes(tc.gaps...), UpdatedAt: time.Now()})
			cachedAt := time.Now().Add(-tc.cacheAge)
			if tc.cached > 0 {
				fundingIntervalMap.Store(symbol, &FundingIntervalCache{Hours: tc.cached, UpdatedAt: cachedAt})
			}
			t.Cleanup(func() {
				fundingHistoryMap.Delete(symbol)
				fundingIntervalMap.Delete(symbol)
			})
			if got := fundingIntervalHours(symbol); got != tc.want {
				t.Fatalf("funding interval = %v, want %v", got, tc.want)
			}
			cached, exists := fundingIntervalMap.Load(symbol)
			if tc.recomputed {
				if !exists || !cached.(*FundingIntervalCache).UpdatedAt.After(cachedAt) {
					t.Fatal("historical estimate was not recalculated")
				}
			} else if tc.cached > 0 && (!exists || !cached.(*FundingIntervalCache).UpdatedAt.Equal(cachedAt)) {
				t.Fatal("cache should have remained unchanged")
			}
		})
	}
}

func TestReview20261029FundingScheduleWholeTableTTL(t *testing.T) {
	calls := 0
	review20261029Schedule(t, func() (map[string]float64, error) {
		calls++
		if calls == 1 {
			return map[string]float64{"BTCUSDT": 1, "ETHUSDT": 4}, nil
		}
		return map[string]float64{"BTCUSDT": 8}, nil
	})
	for _, symbol := range []string{" btc_usdt ", "BTC", "BTC-USDT-SWAP"} {
		if h, ok := FundingScheduleHours(symbol); !ok || h != 1 {
			t.Fatalf("normalized lookup %q = %v, %v", symbol, h, ok)
		}
	}
	if h, ok := FundingScheduleHours("ETHUSDT"); !ok || h != 4 {
		t.Fatalf("second symbol = %v, %v", h, ok)
	}
	if h, ok := FundingScheduleHours("MISSINGUSDT"); ok || h != 0 {
		t.Fatalf("missing symbol must be unknown: %v, %v", h, ok)
	}
	fundingScheduleCache.fetchedAt = time.Now().Add(-59 * time.Minute)
	FundingScheduleHours("BTCUSDT")
	if calls != 1 {
		t.Fatalf("whole table must be shared for one hour: calls=%d", calls)
	}
	fundingScheduleCache.fetchedAt = time.Now().Add(-61 * time.Minute)
	if h, ok := FundingScheduleHours("BTCUSDT"); !ok || h != 8 || calls != 2 {
		t.Fatalf("expired schedule was not refreshed: %v, %v, calls=%d", h, ok, calls)
	}
	if _, ok := FundingScheduleHours("ETHUSDT"); ok {
		t.Fatal("successful refresh must replace the whole table, including removals")
	}
}

func TestReview20261029FundingScheduleFailureBackoff(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "fetch error", true: "empty table"}[empty], func(t *testing.T) {
			calls := 0
			review20261029Schedule(t, func() (map[string]float64, error) {
				calls++
				if empty {
					return map[string]float64{}, nil
				}
				return nil, errors.New("offline")
			})
			for i := 0; i < 3; i++ {
				if h, ok := FundingScheduleHours("BTCUSDT"); ok || h != 0 {
					t.Fatalf("failed initial load = %v, %v", h, ok)
				}
			}
			fundingScheduleCache.failedAt = time.Now().Add(-59 * time.Second)
			FundingScheduleHours("ETHUSDT")
			if calls != 1 {
				t.Fatalf("retry within one minute: calls=%d", calls)
			}
			fundingScheduleCache.failedAt = time.Now().Add(-61 * time.Second)
			fundingScheduleFetch = func() (map[string]float64, error) {
				calls++
				return map[string]float64{"BTCUSDT": 1}, nil
			}
			if h, ok := FundingScheduleHours("BTCUSDT"); !ok || h != 1 || calls != 2 {
				t.Fatalf("retry after backoff = %v, %v, calls=%d", h, ok, calls)
			}
			if !fundingScheduleCache.failedAt.IsZero() {
				t.Fatal("successful refresh must clear failure backoff")
			}
		})
	}
}

func TestReview20261029FundingScheduleStaleLimit(t *testing.T) {
	for _, age := range []time.Duration{2 * time.Hour, 6*time.Hour - time.Minute, 6 * time.Hour, 7 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			calls := 0
			review20261029Schedule(t, func() (map[string]float64, error) {
				calls++
				return nil, errors.New("refresh failed")
			})
			fundingScheduleCache.hours = map[string]float64{"BTCUSDT": 1}
			fundingScheduleCache.fetchedAt = time.Now().Add(-age)
			for i := 0; i < 2; i++ {
				h, ok := FundingScheduleHours("BTCUSDT")
				if age < 6*time.Hour {
					if !ok || h != 1 {
						t.Fatalf("usable old table lost: %v, %v", h, ok)
					}
				} else if ok || h != 0 {
					t.Fatalf("table at least six hours old must be rejected: %v, %v", h, ok)
				}
			}
			if calls != 1 {
				t.Fatalf("stale-table retry was not backed off: calls=%d", calls)
			}
		})
	}
}

func TestReview20261029FundingScheduleConcurrentRefresh(t *testing.T) {
	var calls atomic.Int32
	review20261029Schedule(t, func() (map[string]float64, error) {
		calls.Add(1)
		return map[string]float64{"BTCUSDT": 1}, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Readers racing the first refresh may see "unknown" (history
			// fallback) but never a wrong interval.
			if h, ok := FundingScheduleHours("BTCUSDT"); ok && h != 1 {
				t.Errorf("concurrent lookup = %v, %v", h, ok)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent callers fetched %d times", calls.Load())
	}
}

func TestReview20261029FundingScheduleHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
		wantOK bool
	}{
		{"valid rows", 200, `[{"symbol":"BTCUSDT","fundingIntervalHours":1},{"symbol":"MAXUSDT","fundingIntervalHours":24},{"symbol":"ZEROUSDT","fundingIntervalHours":0},{"symbol":"NEGUSDT","fundingIntervalHours":-1},{"symbol":"LONGUSDT","fundingIntervalHours":25}]`, nil, true},
		{"non-2xx array", 503, `[{"symbol":"BTCUSDT","fundingIntervalHours":1}]`, nil, false},
		{"object instead of array", 200, `{"code":-1003}`, nil, false},
		{"malformed JSON", 200, `[`, nil, false},
		{"empty array", 200, `[]`, nil, false},
		{"null", 200, `null`, nil, false},
		{"network failure", 0, "", errors.New("offline"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = oldTransport })
			calls := 0
			http.DefaultTransport = historicalRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.String() != binanceFAPIBase+"/fapi/v1/fundingInfo" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			table, err := fetchFundingSchedule()
			if (err == nil) != tc.wantOK || calls != 1 {
				t.Fatalf("fetch: table=%v err=%v calls=%d", table, err, calls)
			}
			if tc.wantOK && (len(table) != 2 || table["BTCUSDT"] != 1 || table["MAXUSDT"] != 24) {
				t.Fatalf("invalid intervals were not filtered: %v", table)
			}
		})
	}
}

func TestReview20261029MedianFundingIntervalExport(t *testing.T) {
	if h := MedianFundingIntervalHours(review20261029FundingTimes(8, 1, 1)); h != 1 {
		t.Fatalf("median = %v, want 1", h)
	}
	if h := MedianFundingIntervalHours([]int64{0, 0, -1, 25 * time.Hour.Milliseconds()}); math.IsNaN(h) || h != 0 {
		t.Fatalf("invalid gaps must yield unknown: %v", h)
	}
}

func TestReview20261029FundingScheduleRefreshDoesNotBlockReaders(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	review20261029Schedule(t, func() (map[string]float64, error) {
		close(started)
		<-release
		return map[string]float64{"AAAUSDT": 4}, nil
	})
	done := make(chan struct{})
	go func() { FundingScheduleHours("AAAUSDT"); close(done) }()
	<-started
	reader := make(chan bool, 1)
	go func() { _, ok := FundingScheduleHours("AAAUSDT"); reader <- ok }()
	select {
	case ok := <-reader:
		if ok {
			t.Fatal("reader saw a table before the first refresh finished")
		}
	case <-time.After(time.Second):
		t.Fatal("reader blocked on the in-flight refresh")
	}
	close(release)
	<-done
	if h, ok := FundingScheduleHours("AAAUSDT"); !ok || h != 4 {
		t.Fatalf("after refresh got %v %v", h, ok)
	}
}
