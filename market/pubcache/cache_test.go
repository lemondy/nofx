package pubcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/market/bnweight"
	"nofx/netmeter"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const klineURL = "https://fapi.binance.com/fapi/v1/klines?symbol=BTCUSDT&interval=5m&limit=100"

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"X-Test": {"original"}},
		Trailer: http.Header{"X-Trailer": {"original"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func request(t *testing.T, method, rawURL string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func read(t *testing.T, rt http.RoundTripper, req *http.Request) (*http.Response, []byte, error) {
	t.Helper()
	r, err := rt.RoundTrip(req)
	if err != nil {
		return nil, nil, err
	}
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	return r, b, err
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for concurrent callers")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPubCacheTTLPolicyAndExpiry(t *testing.T) {
	for path, ttl := range map[string]time.Duration{
		"/fapi/v1/klines": 15 * time.Second, "/futures/data/openInterestHist": time.Minute,
		"/fapi/v1/ticker/24hr": 30 * time.Second, "/fapi/v1/fundingRate": 10 * time.Minute,
		"/fapi/v1/premiumIndex": 10 * time.Second, "/api/kline/list/open": 15 * time.Second, "/api/kline/lists": 15 * time.Second,
	} {
		t.Run(path, func(t *testing.T) {
			if TTL(path) != ttl {
				t.Fatalf("TTL=%s want %s", TTL(path), ttl)
			}
			now := time.Unix(1000, 0)
			c := New(Options{Now: func() time.Time { return now }})
			calls := 0
			rt := c.Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				now = now.Add(time.Second) // network latency consumes TTL
				body := `[]`
				if path == "/fapi/v1/premiumIndex" {
					body = `{"symbol":"BTCUSDT","lastFundingRate":"0.001"}`
				}
				if strings.HasPrefix(path, "/api/") {
					body = `{"success":true,"data":[]}`
				}
				return response(200, body), nil
			}))
			url := "https://example.test" + path
			_, body, err := read(t, rt, request(t, "GET", url))
			if err != nil {
				t.Fatal(err)
			}
			body[0] = 'x'
			now = time.Unix(1000, 0).Add(ttl - time.Nanosecond)
			r, body, err := read(t, rt, request(t, "GET", url))
			if err != nil || body[0] == 'x' || calls != 1 {
				t.Fatalf("hit/copy calls=%d body=%s err=%v", calls, body, err)
			}
			r.Header.Set("X-Test", "mutated")
			r.Trailer.Set("X-Trailer", "mutated")
			r, _, _ = read(t, rt, request(t, "GET", url))
			if r.Header.Get("X-Test") != "original" || r.Trailer.Get("X-Trailer") != "original" {
				t.Fatal("response metadata aliased")
			}
			now = time.Unix(1000, 0).Add(ttl)
			read(t, rt, request(t, "GET", url))
			if calls != 2 || c.Stats().Hits != 2 || c.Stats().Misses != 2 {
				t.Fatalf("expiry must use fetch start: calls=%d stats=%+v", calls, c.Stats())
			}
		})
	}
}

func TestPubCacheCanonicalQueryIsolation(t *testing.T) {
	c := New(Options{})
	calls := 0
	rt := c.Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return response(200, `[]`), nil }))
	urls := []string{klineURL, "https://fapi.binance.com/fapi/v1/klines?limit=100&interval=5m&symbol=BTCUSDT",
		strings.Replace(klineURL, "BTC", "ETH", 1), strings.Replace(klineURL, "5m", "15m", 1), strings.Replace(klineURL, "100", "200", 1), strings.Replace(klineURL, "fapi.binance.com", "mirror.test", 1)}
	for _, url := range urls {
		if _, _, err := read(t, rt, request(t, "GET", url)); err != nil {
			t.Fatal(err)
		}
	}
	if calls != len(urls)-1 {
		t.Fatalf("query keys: calls=%d", calls)
	}
	a, _ := requestKey(request(t, "GET", klineURL+"&extra=b&extra=a"))
	b, _ := requestKey(request(t, "GET", klineURL+"&extra=a&extra=b"))
	if a == b {
		t.Fatal("repeated query values with different order may have different upstream semantics")
	}
}

func TestPubCacheBypassesPrivateAndUnlistedRequests(t *testing.T) {
	for _, tc := range []struct{ name, method, url, header string }{
		{"post", "POST", klineURL, ""}, {"delete", "DELETE", klineURL, ""},
		{"signed", "GET", klineURL + "&signature=secret", ""}, {"empty signature", "GET", klineURL + "&Signature=", ""},
		{"timestamp", "GET", klineURL + "&timestamp=1", ""}, {"api key", "GET", klineURL, "X-MBX-APIKEY"},
		{"lowercase key", "GET", klineURL, "x-mbx-apikey"}, {"auth", "GET", klineURL, "Authorization"},
		{"coinank key", "GET", "https://api.coinank.com/api/kline/lists", "apikey"}, {"cookie", "GET", klineURL, "Cookie"},
		{"listen query", "GET", klineURL + "&listenKey=secret", ""}, {"listen path", "GET", "https://fapi.binance.com/fapi/v1/listenKey", ""},
		{"depth", "GET", "https://fapi.binance.com/fapi/v1/depth", ""}, {"book ticker", "GET", "https://fapi.binance.com/fapi/v1/ticker/bookTicker", ""},
		{"price", "GET", "https://fapi.binance.com/fapi/v1/ticker/price", ""}, {"account", "GET", "https://fapi.binance.com/fapi/v2/account", ""},
		{"exchange info", "GET", "https://fapi.binance.com/fapi/v1/exchangeInfo", ""}, {"instant OI", "GET", "https://fapi.binance.com/fapi/v1/openInterest", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Options{})
			calls := 0
			rt := c.Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return response(200, `[]`), nil }))
			for i := 0; i < 2; i++ {
				req := request(t, tc.method, tc.url)
				if tc.header != "" {
					req.Header[tc.header] = []string{""}
				}
				read(t, rt, req)
			}
			if calls != 2 || c.Stats().Counts != (Counts{}) {
				t.Fatalf("must bypass entirely: calls=%d stats=%+v", calls, c.Stats())
			}
		})
	}
}

type brokenBody struct{ err error }

func (b brokenBody) Read([]byte) (int, error) { return 0, b.err }
func (b brokenBody) Close() error             { return nil }

func TestPubCacheSingleflightFailuresAreSharedThenRetried(t *testing.T) {
	boom := errors.New("read/transport failed")
	for _, tc := range []struct {
		name, url string
		fetch     func() (*http.Response, error)
		wantErr   bool
	}{
		{"transport", klineURL, func() (*http.Response, error) { return nil, boom }, true},
		{"timeout", klineURL, func() (*http.Response, error) { return nil, context.DeadlineExceeded }, true},
		{"partial body", klineURL, func() (*http.Response, error) { r := response(200, ""); r.Body = brokenBody{boom}; return r, nil }, true},
		{"short body", klineURL, func() (*http.Response, error) { r := response(200, "[]"); r.ContentLength = 10; return r, nil }, true},
		{"non 200", klineURL, func() (*http.Response, error) { return response(429, `[]`), nil }, false},
		{"invalid JSON", klineURL, func() (*http.Response, error) { return response(200, `[`), nil }, false},
		{"Binance error", klineURL, func() (*http.Response, error) { return response(200, `{"code":-1121,"msg":"Invalid symbol"}`), nil }, false},
		{"Coinank error", "https://api.coinank.com/api/kline/list/open", func() (*http.Response, error) { return response(200, `{"success":false,"data":[]}`), nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(Options{})
			var calls atomic.Int32
			release := make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			rt := c.Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					<-release
					return tc.fetch()
				}
				if strings.Contains(tc.url, "coinank") {
					return response(200, `{"success":true,"data":[]}`), nil
				}
				return response(200, `[]`), nil
			}))
			const n = 20
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _, err := read(t, rt, request(t, "GET", tc.url))
					if (err != nil) != tc.wantErr {
						t.Errorf("error=%v want error=%v", err, tc.wantErr)
					}
				}()
			}
			waitFor(t, func() bool { return c.Stats().Coalesced == n-1 })
			once.Do(func() { close(release) })
			wg.Wait()
			if calls.Load() != 1 || c.Stats().Entries != 0 {
				t.Fatalf("failed response must be shared, never stored: calls=%d stats=%+v", calls.Load(), c.Stats())
			}
			for i := 0; i < 2; i++ {
				if _, _, err := read(t, rt, request(t, "GET", tc.url)); err != nil {
					t.Fatal(err)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("next request must retry, then cache success: calls=%d", calls.Load())
			}
		})
	}
}

func TestPubCacheFiveTradersShareBatchAndBypassMetersOnHits(t *testing.T) {
	c := New(Options{})
	var calls atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	base := roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); <-release; return response(200, `[]`), nil })
	// Independent clients/priorities, one cache above both metering layers.
	clients := []http.RoundTripper{}
	for i := 0; i < 5; i++ {
		clients = append(clients, c.Wrap(bnweight.Wrap(netmeter.Wrap(base), bnweight.Priority(i%2))))
	}
	var wg sync.WaitGroup
	for _, rt := range clients {
		for _, symbol := range []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"} {
			for _, path := range []string{"/fapi/v1/klines?interval=5m&limit=100", "/futures/data/openInterestHist?period=5m&limit=30"} {
				wg.Add(1)
				go func(rt http.RoundTripper, url string) {
					defer wg.Done()
					if _, _, err := read(t, rt, request(t, "GET", url)); err != nil {
						t.Error(err)
					}
				}(rt, "https://fapi.binance.com"+path+"&symbol="+symbol)
			}
		}
	}
	waitFor(t, func() bool { return c.Stats().Coalesced == 24 })
	once.Do(func() { close(release) })
	wg.Wait()
	if calls.Load() != 6 || c.Stats().Misses != 6 {
		t.Fatalf("5 traders × 3 symbols × 2 endpoints: calls=%d stats=%+v", calls.Load(), c.Stats())
	}
	trafficBefore, _ := netmeter.Snapshot()
	weightBefore := bnweight.Snapshot()
	for _, rt := range clients {
		read(t, rt, request(t, "GET", klineURL))
	}
	trafficAfter, _ := netmeter.Snapshot()
	weightAfter := bnweight.Snapshot()
	if calls.Load() != 6 || trafficAfter != trafficBefore || fmt.Sprint(weightBefore) != fmt.Sprint(weightAfter) {
		t.Fatalf("hits reached lower layers: calls=%d traffic before=%+v after=%+v weight before=%+v after=%+v", calls.Load(), trafficBefore, trafficAfter, weightBefore, weightAfter)
	}
}

func TestPubCacheCapacityCleanupAndStatsIsolation(t *testing.T) {
	now := time.Unix(1000, 0)
	c := New(Options{MaxEntries: 2, MaxBytes: 6, Now: func() time.Time { return now }})
	rt := c.Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, `[1]`), nil }))
	for i := 0; i < 20; i++ {
		read(t, rt, request(t, "GET", fmt.Sprintf("%s&extra=%d", klineURL, i)))
		if s := c.Stats(); s.Entries > 2 || s.Bytes > 6 {
			t.Fatalf("capacity exceeded: %+v", s)
		}
	}
	s := c.Stats()
	delete(s.Endpoints, "/fapi/v1/klines")
	if c.Stats().Endpoints["/fapi/v1/klines"].Misses != 20 {
		t.Fatal("Stats returned mutable internal map")
	}
	if !strings.Contains(summary(c.Stats()), "/fapi/v1/klines hit=") {
		t.Fatal("missing endpoint hit rate")
	}
	now = now.Add(KlineTTL)
	if s := c.Stats(); s.Entries != 0 || s.Bytes != 0 {
		t.Fatalf("expired entries not cleaned: %+v", s)
	}
	c = New(Options{MaxEntries: 100, MaxBytes: 5})
	rt = c.Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) { return response(200, `[1]`), nil }))
	for i := 0; i < 5; i++ {
		read(t, rt, request(t, "GET", fmt.Sprintf("%s&extra=%d", klineURL, i)))
	}
	if c.Stats().Entries != 1 || c.Stats().Bytes != 3 {
		t.Fatalf("byte bound failed: %+v", c.Stats())
	}
}

func TestPubCacheCancelledWaiterDoesNotCancelLeader(t *testing.T) {
	c := New(Options{})
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	rt := c.Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) { <-release; return response(200, `[]`), nil }))
	finished := make(chan struct{})
	go func() { defer close(finished); read(t, rt, request(t, "GET", klineURL)) }()
	waitFor(t, func() bool { return c.Stats().Misses == 1 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { _, _, err := read(t, rt, request(t, "GET", klineURL).WithContext(ctx)); errCh <- err }()
	waitFor(t, func() bool { return c.Stats().Coalesced == 1 })
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter cancellation: %v", err)
	}
	once.Do(func() { close(release) })
	<-finished
	if c.Stats().Entries != 1 {
		t.Fatal("waiter cancellation affected upstream work")
	}
}
