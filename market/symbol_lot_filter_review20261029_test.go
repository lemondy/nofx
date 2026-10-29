package market

import (
	"encoding/json"
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

func TestReview20261029ParseSymbolLotFilter(t *testing.T) {
	for _, filterType := range []string{"MIN_NOTIONAL", "NOTIONAL"} {
		for _, key := range []string{"notional", "minNotional"} {
			for _, value := range []interface{}{"25.5", 25.5, json.Number("25.5")} {
				f := map[string]interface{}{"filterType": filterType, key: value}
				if got := ParseSymbolLotFilter([]map[string]interface{}{f}); got.MinNotional != 25.5 {
					t.Errorf("%s %s=%#v: got %+v", filterType, key, value, got)
				}
			}
		}
	}
	for _, tc := range []struct {
		name    string
		filters []map[string]interface{}
		want    SymbolLotFilter
	}{
		{"missing", nil, SymbolLotFilter{MinNotional: 10}},
		{"notional takes precedence", []map[string]interface{}{
			{"filterType": "NOTIONAL", "notional": "20", "minNotional": "50"},
		}, SymbolLotFilter{MinNotional: 20}},
		{"empty notional uses minNotional", []map[string]interface{}{
			{"filterType": "MIN_NOTIONAL", "notional": "", "minNotional": 15},
		}, SymbolLotFilter{MinNotional: 15}},
		{"first valid notional", []map[string]interface{}{
			{"filterType": "MIN_NOTIONAL", "notional": "NaN"},
			{"filterType": "NOTIONAL", "minNotional": "12"},
			{"filterType": "MIN_NOTIONAL", "notional": "30"},
		}, SymbolLotFilter{MinNotional: 12}},
		{"market stricter", []map[string]interface{}{
			{"filterType": "LOT_SIZE", "minQty": "0.01", "stepSize": "0.001"},
			{"filterType": "MARKET_LOT_SIZE", "minQty": "0.1", "stepSize": "0.01"},
		}, SymbolLotFilter{MinQty: 0.1, StepSize: 0.01, MinNotional: 10}},
		{"limit stricter", []map[string]interface{}{
			{"filterType": "MARKET_LOT_SIZE", "minQty": "0.01", "stepSize": "0.001"},
			{"filterType": "LOT_SIZE", "minQty": "0.1", "stepSize": "0.01"},
		}, SymbolLotFilter{MinQty: 0.1, StepSize: 0.01, MinNotional: 10}},
		{"independent maxima", []map[string]interface{}{
			{"filterType": "LOT_SIZE", "minQty": 0.2, "stepSize": 0.001},
			{"filterType": "MARKET_LOT_SIZE", "minQty": 0.1, "stepSize": 0.01},
		}, SymbolLotFilter{MinQty: 0.2, StepSize: 0.01, MinNotional: 10}},
		{"market missing", []map[string]interface{}{
			{"filterType": "LOT_SIZE", "minQty": "0.025", "stepSize": "0.025"},
		}, SymbolLotFilter{MinQty: 0.025, StepSize: 0.025, MinNotional: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseSymbolLotFilter(tc.filters); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	for _, invalid := range []interface{}{nil, "", "bad", 0, -1, "0", "-1", "NaN", "Inf", "-Inf", math.NaN(), math.Inf(1), math.Inf(-1)} {
		filters := []map[string]interface{}{
			{"filterType": "LOT_SIZE", "minQty": "0.02", "stepSize": "0.01"},
			{"filterType": "MARKET_LOT_SIZE", "minQty": invalid, "stepSize": invalid},
			{"filterType": "MIN_NOTIONAL", "notional": invalid},
		}
		want := SymbolLotFilter{MinQty: 0.02, StepSize: 0.01, MinNotional: 10}
		if got := ParseSymbolLotFilter(filters); got != want {
			t.Errorf("invalid value %#v: got %+v, want %+v", invalid, got, want)
		}
		filters = filters[1:]
		filters[0]["filterType"] = "LOT_SIZE"
		if got := ParseSymbolLotFilter(filters); got != (SymbolLotFilter{MinNotional: 10}) {
			t.Errorf("invalid LOT_SIZE value %#v: got %+v", invalid, got)
		}
	}
}

func review20261029LotFetch(t *testing.T, fetch func() (map[string]SymbolLotFilter, error)) {
	t.Helper()
	previous := lotFilterFetch
	ResetSymbolLotFilters()
	lotFilterFetch = fetch
	t.Cleanup(func() {
		lotFilterFetch = previous
		ResetSymbolLotFilters()
	})
}

func review20261029ExpireLotTable() {
	lotFilterMu.Lock()
	defer lotFilterMu.Unlock()
	expired := time.Now().Add(-lotFilterTTL)
	lotFilterFetchedAt = expired
	for symbol, c := range lotFilterCacheV {
		c.fetchedAt = expired
		lotFilterCacheV[symbol] = c
	}
}

func TestReview20261029LotFilterWholeTable(t *testing.T) {
	a := SymbolLotFilter{MinQty: 0.1, StepSize: 0.01, MinNotional: 20}
	b := SymbolLotFilter{MinQty: 1, StepSize: 1, MinNotional: 5}
	calls := 0
	review20261029LotFetch(t, func() (map[string]SymbolLotFilter, error) {
		calls++
		return map[string]SymbolLotFilter{"AUSDT": a, "BUSDT": b}, nil
	})
	SetSymbolLotFilterForTesting("PINUSDT", a)
	for _, symbol := range []string{"AUSDT", "BUSDT", "MISSINGUSDT", "OTHERUSDT", "AUSDT"} {
		want := map[string]SymbolLotFilter{"AUSDT": a, "BUSDT": b}[symbol]
		if got := GetSymbolLotFilter(symbol); got != want {
			t.Fatalf("%s: got %+v, want %+v", symbol, got, want)
		}
	}
	if calls != 1 {
		t.Fatalf("two symbols and misses fetched %d tables, want 1", calls)
	}
	if got := GetSymbolLotFilter("PINUSDT"); got != a {
		t.Fatalf("table refresh removed an unexpired test pin: %+v", got)
	}
	review20261029ExpireLotTable()
	GetSymbolLotFilter("BUSDT")
	if calls != 2 {
		t.Fatalf("expired table did not refresh: calls=%d", calls)
	}
}

func TestReview20261029LotFilterFailureBackoff(t *testing.T) {
	calls := 0
	fail := true
	want := SymbolLotFilter{MinNotional: 20}
	review20261029LotFetch(t, func() (map[string]SymbolLotFilter, error) {
		calls++
		if fail {
			return nil, errors.New("offline")
		}
		return map[string]SymbolLotFilter{"AUSDT": want}, nil
	})
	for _, symbol := range []string{"AUSDT", "BUSDT", "AUSDT"} {
		if got := GetSymbolLotFilter(symbol); got != (SymbolLotFilter{}) {
			t.Fatalf("failure must be unknown, got %+v", got)
		}
	}
	if calls != 1 || lotFilterFailedAt.IsZero() || lotFilterFailureBackoff != time.Minute {
		t.Fatalf("failure backoff missing: calls=%d failedAt=%v", calls, lotFilterFailedAt)
	}
	lotFilterMu.Lock()
	lotFilterFailedAt = time.Now().Add(-lotFilterFailureBackoff)
	lotFilterMu.Unlock()
	fail = false
	if got := GetSymbolLotFilter("AUSDT"); got != want || calls != 2 {
		t.Fatalf("backoff expiry did not recover: got %+v calls=%d", got, calls)
	}
	if !lotFilterFailedAt.IsZero() {
		t.Fatal("successful refresh retained failure state")
	}

	// An expired successful table survives subsequent failed refreshes.
	review20261029ExpireLotTable()
	fail = true
	for _, symbol := range []string{"AUSDT", "MISSINGUSDT", "AUSDT"} {
		expected := SymbolLotFilter{}
		if symbol == "AUSDT" {
			expected = want
		}
		if got := GetSymbolLotFilter(symbol); got != expected {
			t.Fatalf("failed refresh %s: got %+v want %+v", symbol, got, expected)
		}
	}
	if calls != 3 {
		t.Fatalf("stale failure retried during backoff: calls=%d", calls)
	}
	ResetSymbolLotFilters()
	if got := GetSymbolLotFilter("AUSDT"); got != (SymbolLotFilter{}) || calls != 4 {
		t.Fatalf("Reset retained table/backoff: got %+v calls=%d", got, calls)
	}
	SetSymbolLotFilterForTesting("PINUSDT", want)
	if got := GetSymbolLotFilter("PINUSDT"); got != want || calls != 4 {
		t.Fatalf("offline pin ignored: got %+v calls=%d", got, calls)
	}
}

func TestReview20261029LotFilterConcurrent(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			var calls atomic.Int32
			want := SymbolLotFilter{MinNotional: 20}
			review20261029LotFetch(t, func() (map[string]SymbolLotFilter, error) {
				calls.Add(1)
				// A fetch must not hold the cache mutex.
				SetSymbolLotFilterForTesting("PINUSDT", want)
				if fail {
					return nil, errors.New("offline")
				}
				return map[string]SymbolLotFilter{"AUSDT": want, "BUSDT": want}, nil
			})
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < 32; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					expected := want
					if fail {
						expected = SymbolLotFilter{}
					}
					symbol := []string{"AUSDT", "BUSDT"}[i%2]
					if got := GetSymbolLotFilter(symbol); got != expected {
						t.Errorf("%s: got %+v, want %+v", symbol, got, expected)
					}
				}(i)
			}
			close(start)
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("concurrent fetches=%d, want 1", calls.Load())
			}
			if got := GetSymbolLotFilter("PINUSDT"); got != want {
				t.Fatalf("refresh overwrote test pin: %+v", got)
			}
		})
	}
}

func TestReview20261029LotFilterDefaultFetchOffline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
		want   SymbolLotFilter
	}{
		{"trading only", 200, `{"symbols":[{"symbol":"AUSDT","status":"TRADING","filters":[{"filterType":"MIN_NOTIONAL","minNotional":"25"}]},{"symbol":"BUSDT","status":"BREAK","filters":[]}]}`, nil, SymbolLotFilter{MinNotional: 25}},
		{"request failure", 0, "", errors.New("offline"), SymbolLotFilter{}},
		{"decode failure", 200, `{`, nil, SymbolLotFilter{}},
		{"empty symbols", 200, `{"symbols":[]}`, nil, SymbolLotFilter{}},
		{"rate limited", 429, `{"code":-1003,"msg":"Too many requests"}`, nil, SymbolLotFilter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review20261029LotFetch(t, lotFilterFetch) // real fetch, fake HTTP transport
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			calls := 0
			http.DefaultTransport = historicalRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "fapi.binance.com" || r.URL.Path != "/fapi/v1/exchangeInfo" {
					t.Fatalf("unexpected request: %s", r.URL)
				}
				if tc.err != nil {
					return nil, tc.err
				}
				// Keep the fake 429 from leaving the shared weight meter in
				// cooldown for later tests; the lot-cache backoff is separate.
				header := http.Header{"Retry-After": []string{"0.000000001"}}
				return &http.Response{StatusCode: tc.status, Header: header, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			if got := GetSymbolLotFilter("AUSDT"); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if got := GetSymbolLotFilter("BUSDT"); got != (SymbolLotFilter{}) {
				t.Fatalf("non-trading or absent symbol acquired a floor: %+v", got)
			}
			if calls != 1 {
				t.Fatalf("requests=%d, want 1", calls)
			}
			if tc.want == (SymbolLotFilter{}) && lotFilterFailedAt.IsZero() {
				t.Fatal("failed/empty response was cached as a successful table")
			}
		})
	}
}
