package market

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nofx/market/bnweight"
)

// fakeKlineServer emulates Binance /fapi/v1/klines for 1m bars (open times on
// minute boundaries), honouring startTime/endTime/limit, and records requests.
type fakeKlineServer struct {
	mu     sync.Mutex
	limits []int
}

func (f *fakeKlineServer) RoundTrip(req *http.Request) (*http.Response, error) {
	q := req.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	startMs, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
	endMs, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
	f.mu.Lock()
	f.limits = append(f.limits, limit)
	f.mu.Unlock()

	const barMs = int64(60_000)
	open := (startMs + barMs - 1) / barMs * barMs // first bar opening >= startTime
	rows := make([][]interface{}, 0)
	for ; open <= endMs && len(rows) < limit; open += barMs {
		rows = append(rows, []interface{}{open, "1", "2", "0.5", "1.5", "10", open + barMs - 1})
	}
	body, _ := json.Marshal(rows)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func installFakeKlines(t *testing.T) *fakeKlineServer {
	t.Helper()
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	srv := &fakeKlineServer{}
	http.DefaultTransport = srv
	return srv
}

func TestGetKlinesRangeShortRangeUsesSmallLimit(t *testing.T) {
	srv := installFakeKlines(t)
	start := time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)
	bars, err := GetKlinesRange("PAGELIMITSHORTUSDT", "1m", start, start.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 6 { // closed interval: minutes 0..5
		t.Fatalf("got %d bars, want 6", len(bars))
	}
	if len(srv.limits) != 1 || srv.limits[0] > 6 || srv.limits[0] < 6 {
		t.Fatalf("limits=%v, want one request with limit 6 (weight 1, not 1500)", srv.limits)
	}
}

func TestGetKlinesRangeLongRangeStillPagesAtMaxLimit(t *testing.T) {
	srv := installFakeKlines(t)
	start := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	const n = 3200 // minutes after start -> n+1 bars
	bars, err := GetKlinesRange("PAGELIMITLONGUSDT", "1m", start, start.Add(n*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != n+1 {
		t.Fatalf("got %d bars, want %d", len(bars), n+1)
	}
	for i, b := range bars {
		if want := start.Add(time.Duration(i) * time.Minute).UnixMilli(); b.OpenTime != want {
			t.Fatalf("bar %d open=%d want %d (gap or duplicate)", i, b.OpenTime, want)
		}
	}
	if len(srv.limits) < 3 || srv.limits[0] != binanceMaxKlineLimit || srv.limits[1] != binanceMaxKlineLimit {
		t.Fatalf("limits=%v, want full 1500-bar pages first", srv.limits)
	}
	for _, l := range srv.limits {
		if l > binanceMaxKlineLimit {
			t.Fatalf("limit %d exceeds Binance max", l)
		}
	}
}

func TestGetKlinesRangePriorityIsPropagated(t *testing.T) {
	installFakeKlines(t)
	before := bnweight.Snapshot().Totals
	start := time.Date(2026, 10, 29, 4, 0, 0, 0, time.UTC)
	if _, err := GetKlinesRangePriority("PAGELIMITPRIOUSDT", "1m", start, start.Add(3*time.Minute), bnweight.Normal); err != nil {
		t.Fatal(err)
	}
	if _, err := GetKlinesRange("PAGELIMITPRIO2USDT", "1m", start, start.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	after := bnweight.Snapshot().Totals
	if d := after[bnweight.Normal].Requests - before[bnweight.Normal].Requests; d != 1 {
		t.Fatalf("Normal requests delta=%d, want 1", d)
	}
	if d := after[bnweight.Low].Requests - before[bnweight.Low].Requests; d != 1 {
		t.Fatalf("Low requests delta=%d (GetKlinesRange must stay Low), want 1", d)
	}
}

func TestKlinePageLimit(t *testing.T) {
	const m = int64(60_000)
	for _, tc := range []struct {
		span, bar int64
		want      int
	}{
		{5 * m, m, 6},
		{0, m, 1},
		{m / 2, m, 2},
		{10_000 * m, m, binanceMaxKlineLimit},
		{5 * m, 0, binanceMaxKlineLimit},
	} {
		if got := klinePageLimit(1000, 1000+tc.span, tc.bar); got != tc.want {
			t.Errorf("span=%d bar=%d: got %d want %d", tc.span, tc.bar, got, tc.want)
		}
	}
}
