package netmeter

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"empty", "", "/"},
		{"root", "/", "/"},
		{"query only", "?secret=hidden#fragment", "/"},
		{"numbers", "/users/123/orders/00042", "/users/:n/orders/:n"},
		{"long number", "/123456789012345678901234567890", "/:n"},
		{"uuid", "/users/550e8400-e29b-41d4-a716-446655440000", "/users/:id"},
		{"uppercase uuid", "/users/550E8400-E29B-41D4-A716-446655440000", "/users/:id"},
		{"hex boundary", "/abcdef0123456789", "/:id"},
		{"long hex", "/ABCDEF0123456789abcdef0123456789", "/:id"},
		{"short hex", "/abcdef012345678", "/abcdef012345678"},
		{"mixed token boundary", "/ghijklmnopqrstu1", "/:token"},
		{"short mixed", "/ghijklmnopqrst1", "/ghijklmnopqrst1"},
		{"long letters stay readable (static camelCase endpoint)", "/futures/data/globalLongShortAccountRatio", "/futures/data/globalLongShortAccountRatio"},
		{"very long opaque letters", "/abcdefghijklmnopqrstuvwxabcdefghijklmnopqrstuvwx", "/:token"},
		{"long mixed digits", "/abcdefghijklmnopqrstuvw1", "/:token"},
		{"short letters", "/abcdefghijklmnopqrstuvw", "/abcdefghijklmnopqrstuvw"},
		{"telegram", "/bot123456:ABC-DEF_very_secret_TOKEN123456/sendMessage", "/bot:token/sendMessage"},
		{"short telegram", "/bot1:abc/getMe", "/bot:token/getMe"},
		{"escaped telegram", "/%62ot123456%3AABC-DEF_secret/getMe", "/bot:token/getMe"},
		{"telegram file", "/file/bot123456:ABC-DEF_secret/getFile", "/file/bot:token/getFile"},
		{"yahoo", "/v8/finance/chart/AAPL", "/v8/finance/chart/:sym"},
		{"class ticker", "/v8/finance/chart/BRK-B", "/v8/finance/chart/:sym"},
		{"index ticker", "/v8/finance/chart/^GSPC", "/v8/finance/chart/:sym"},
		{"escaped index ticker", "/v8/finance/chart/%5EGSPC", "/v8/finance/chart/:sym"},
		{"pair ticker", "/v8/finance/chart/BTC-USD", "/v8/finance/chart/:sym"},
		{"ticker punctuation", "/BRK.B/EURUSD=X/A1", "/:sym/:sym/:sym"},
		{"ticker length boundary", "/ABCDEFGHIJKLMNO/ABCDEFGHIJKLMNOP", "/:sym/ABCDEFGHIJKLMNOP"},
		{"binance", "/fapi/v1/klines", "/fapi/v1/klines"},
		{"binance 24hr", "/api/v3/ticker/24hr", "/api/v3/ticker/24hr"},
		{"query fragment", "/fapi/v1/klines?symbol=BTCUSDT#private", "/fapi/v1/klines"},
		{"fragment", "/path#private?secret=hidden", "/path"},
		{"escaped number", "/users/%31%32%33", "/users/:n"},
		{"escaped slash", "/api/a%2Fb", "/api/a%2Fb"},
		{"slashes", "/api//v1/", "/api//v1/"},
		{"truncate", strings.Repeat("/static", 30), strings.Repeat("/static", 30)[:120]},
		{"truncate after folding", "/123" + strings.Repeat("/static", 30), ("/:n" + strings.Repeat("/static", 30))[:120]},
		{"telegram at cutoff", strings.Repeat("/static", 16) + "/bot123456:ABC-DEF_secret/sendMessage", (strings.Repeat("/static", 16) + "/bot:token/sendMessage")[:120]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizePath(tt.input); got != tt.want {
				t.Fatalf("normalizePath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
	for _, prefix := range []string{"", strings.Repeat("/static", 16), strings.Repeat("/static", 17)} {
		got := normalizePath(prefix + "/bot123456:ABC-DEF_secret/sendMessage")
		for _, secret := range []string{"123456", "ABC", "DEF", "secret"} {
			if strings.Contains(got, secret) {
				t.Fatalf("Telegram credential fragment %q leaked in %q", secret, got)
			}
		}
	}
}

type pathRoundTripFunc func(*http.Request) (*http.Response, error)

func (f pathRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRoundTripPathAndHostAccounting(t *testing.T) {
	for _, mode := range []string{"known", "streamed", "error"} {
		t.Run(mode, func(t *testing.T) {
			const payload = "request payload"
			const response = "response payload"
			const path = "/fapi/v1/klines"
			host := "accounting-" + mode + ".test"
			_, beforeHosts := Snapshot()
			beforePaths := SnapshotPaths(1000)
			beforeReqs, beforeUp, beforeDown := totals()
			failure := errors.New("transport failure")
			rt := Wrap(pathRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if mode == "error" {
					return nil, failure
				}
				if _, err := io.Copy(io.Discard, req.Body); err != nil {
					t.Fatal(err)
				}
				length := int64(len(response))
				if mode == "streamed" {
					length = -1
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), ContentLength: length}, nil
			}))
			var wantUp, wantDown int64
			for _, query := range []string{"?symbol=BTCUSDT&secret=first", "?symbol=ETHUSDT&secret=second"} {
				req, err := http.NewRequest(http.MethodPost, "https://"+strings.ToUpper(host)+":8443"+path+query+"#fragment", strings.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				if mode == "streamed" {
					req.ContentLength = 0 // streamed bodies can have length 0 as well as -1
				}
				wantUp += int64(len(req.URL.RequestURI()) + len(payload))
				resp, err := rt.RoundTrip(req)
				if mode == "error" {
					if !errors.Is(err, failure) {
						t.Fatal(err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, resp.Body); err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				wantDown += int64(len(response))
			}
			reqs, up, down := totals()
			if reqs-beforeReqs != 2 || up-beforeUp != wantUp || down-beforeDown != wantDown {
				t.Fatalf("totals delta = %d/%d/%d, want 2/%d/%d", reqs-beforeReqs, up-beforeUp, down-beforeDown, wantUp, wantDown)
			}
			_, afterHosts := Snapshot()
			afterPaths := SnapshotPaths(1000)
			hostDelta := func(list []HostUsage) HostUsage {
				for _, h := range list {
					if h.Host == host {
						return h
					}
				}
				return HostUsage{}
			}
			pathDelta := func(list []PathUsage) PathUsage {
				for _, p := range list {
					if p.Host == host && p.Path == path {
						return p
					}
				}
				return PathUsage{}
			}
			h0, h1 := hostDelta(beforeHosts), hostDelta(afterHosts)
			p0, p1 := pathDelta(beforePaths), pathDelta(afterPaths)
			if h1.Requests-h0.Requests != 2 || h1.Up-h0.Up != wantUp || h1.Down-h0.Down != wantDown ||
				p1.Requests-p0.Requests != 2 || p1.Up-p0.Up != wantUp || p1.Down-p0.Down != wantDown {
				t.Fatalf("host/path counts diverged: hosts %+v -> %+v, paths %+v -> %+v", h0, h1, p0, p1)
			}
			for _, p := range afterPaths {
				if strings.ContainsAny(p.Path, "?#") || strings.Contains(p.Path, "secret") {
					t.Fatalf("query or fragment entered path key: %+v", p)
				}
			}
		})
	}
}

func TestPathMeterLimitAndOverflow(t *testing.T) {
	var m pathMeter
	first := m.forPath("a.test", "/first")
	for i := 1; i < maxPaths; i++ {
		m.forPath("a.test", "/endpoint"+strconv.Itoa(i))
	}
	if m.forPath("a.test", "/first") != first {
		t.Fatal("existing key changed at capacity")
	}
	for _, host := range []string{"a.test", "b.test"} {
		overflow := m.forPath(host, "/new")
		if overflow != m.forPath(host, "/another") || overflow != m.forPath(host, otherPath) {
			t.Fatal("overflow must be shared per host")
		}
	}
	if count, entries := m.count.Load(), len(m.snapshot(1000)); count != maxPaths || entries != maxPaths+2 {
		t.Fatalf("count=%d entries=%d, want %d/%d", count, entries, maxPaths, maxPaths+2)
	}
}

func TestPathMeterConcurrentLimit(t *testing.T) {
	const workers, perWorker, hostCount = 64, 128, 8
	var m pathMeter
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			host := "host" + strconv.Itoa(worker%hostCount) + ".test"
			for i := 0; i < perWorker; i++ {
				path := "/shared"
				if i%2 == 0 {
					path = "/endpoint" + strconv.Itoa(worker*perWorker+i)
				}
				p := m.forPath(host, path)
				p.requests.Add(1)
				p.up.Add(3)
				p.down.Add(7)
				if i%64 == 0 {
					m.snapshot(maxPaths + hostCount) // snapshot concurrently with insertion/counting
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	list := m.snapshot(1000)
	var requests, up, down int64
	regular, overflow := 0, 0
	for _, p := range list {
		requests += p.Requests
		up += p.Up
		down += p.Down
		if p.Path == otherPath {
			overflow++
		} else {
			regular++
		}
	}
	if len(list) > maxPaths+hostCount || regular != maxPaths || overflow != hostCount || m.count.Load() != int64(regular) {
		t.Fatalf("unbounded or miscounted keys: total=%d regular=%d overflow=%d reserved=%d", len(list), regular, overflow, m.count.Load())
	}
	if requests != workers*perWorker || up != requests*3 || down != requests*7 {
		t.Fatalf("lost concurrent counts: %d/%d/%d", requests, up, down)
	}
}

func TestSnapshotPathsSortAndLimit(t *testing.T) {
	var m pathMeter
	want := []PathUsage{
		{Host: "z.test", Path: "/largest", Requests: 1, Up: 11, Down: 10},
		{Host: "a.test", Path: "/alpha", Requests: 2, Up: 5, Down: 5},
		{Host: "a.test", Path: "/beta", Requests: 3, Up: 8, Down: 2},
		{Host: "b.test", Path: "/alpha", Requests: 4, Up: 2, Down: 8},
	}
	for _, p := range want {
		stats := m.forPath(p.Host, p.Path)
		stats.requests.Store(p.Requests)
		stats.up.Store(p.Up)
		stats.down.Store(p.Down)
	}
	for _, limit := range []int{-1, 0, 1, 3, 4, 30} {
		got := m.snapshot(limit)
		if expected := want[:max(0, min(limit, len(want)))]; !reflect.DeepEqual(got, expected) {
			t.Fatalf("limit %d: got %+v, want %+v", limit, got, expected)
		}
	}
	if got := SnapshotPaths(0); got == nil || len(got) != 0 {
		t.Fatalf("nonpositive limit should return an empty array, got %+v", got)
	}
}

func BenchmarkNormalizePath(b *testing.B) {
	for _, path := range []string{"/fapi/v1/klines", "/api/v3/ticker/24hr", "/v8/finance/chart/AAPL", "/bot123456:ABC-DEF_secret/sendMessage"} {
		b.Run(path, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				normalizePath(path)
			}
		})
	}
}

func BenchmarkPathMeterHit(b *testing.B) {
	var m pathMeter
	m.forPath("fapi.binance.com", "/fapi/v1/klines")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.forPath("fapi.binance.com", "/fapi/v1/klines")
		}
	})
}
