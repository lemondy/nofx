package usstock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

// resetCaches is deliberately test-only and called with no requests in flight.
func resetCaches() {
	symbolCache.Lock()
	symbolCache.symbols = nil
	symbolCache.loaded = time.Time{}
	symbolCache.Unlock()
	bstockCache.Lock()
	bstockCache.entries = make(map[string]barCacheEntry)
	bstockCache.Unlock()
	yahooGate <- struct{}{}
	yahooCache = make(map[string]yahooCacheEntry)
	<-yahooGate
	quoteCache.Lock()
	quoteCache.entries = make(map[string]quoteCacheEntry)
	quoteCache.Unlock()
	calendarWarning = sync.Once{}
}

const spotFixture = `{"symbols":[
 {"symbol":"AAPLBUSDT","baseAsset":"AAPLB","quoteAsset":"USDT","status":"TRADING","filters":[{"filterType":"PRICE_FILTER","tickSize":"0.01"},{"filterType":"LOT_SIZE","stepSize":"0.001","minQty":"0.002"},{"filterType":"MIN_NOTIONAL","minNotional":"3"},{"filterType":"NOTIONAL","minNotional":"5"}]},
 {"symbol":"SPYBUSDT","baseAsset":"SPYB","quoteAsset":"USDT","status":"TRADING","filters":[{"filterType":"MIN_NOTIONAL","minNotional":"10"}]},
 {"symbol":"BNBUSDT","baseAsset":"BNB","quoteAsset":"USDT","status":"TRADING"},
 {"symbol":"ARBUSDT","baseAsset":"ARB","quoteAsset":"USDT","status":"TRADING"},
 {"symbol":"SHIBUSDT","baseAsset":"SHIB","quoteAsset":"USDT","status":"TRADING"},
 {"symbol":"QNTBUSDT","baseAsset":"QNTB","quoteAsset":"USDT","status":"TRADING"},
 {"symbol":"TSLABUSDT","baseAsset":"TSLAB","quoteAsset":"USDT","status":"BREAK"},
 {"symbol":"NVDABUSDC","baseAsset":"NVDAB","quoteAsset":"USDC","status":"TRADING"},
 {"symbol":"HKTESTBUSDT","baseAsset":"HKTESTB","quoteAsset":"USDT","status":"TRADING"},
 {"symbol":"PREBUSDT","baseAsset":"PREB","quoteAsset":"USDT","status":"TRADING"}
]}`
const futuresFixture = `{"symbols":[{"baseAsset":"AAPL","underlyingType":"EQUITY"},{"baseAsset":"SPY","underlyingType":"EQUITY"},{"baseAsset":"TSLA","underlyingType":"EQUITY"},{"baseAsset":"NVDA","underlyingType":"EQUITY"},{"baseAsset":"QNT","underlyingType":"COIN"},{"baseAsset":"HKTEST","underlyingType":"HK_EQUITY"},{"baseAsset":"PRE","underlyingType":"PREMARKET"}]}`

func setupHTTP(t *testing.T, now time.Time, handler roundTripFunc) {
	t.Helper()
	oldClient, oldNow, oldRetry := httpClient, nowFunc, retryBaseDelay
	oldSpot, oldFutures, oldYahoo, oldFallback := spotBaseURL, futuresBaseURL, yahooBaseURL, yahooFallbackBaseURL
	resetCaches()
	nowFunc = func() time.Time { return now }
	retryBaseDelay = 0
	spotBaseURL, futuresBaseURL, yahooBaseURL, yahooFallbackBaseURL = "https://spot.test", "https://futures.test", "https://query1.test", "https://query2.test"
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v3/exchangeInfo" {
			return response(200, spotFixture), nil
		}
		if r.URL.Path == "/fapi/v1/exchangeInfo" {
			return response(200, futuresFixture), nil
		}
		if handler != nil {
			return handler(r)
		}
		t.Errorf("unexpected request: %s", r.URL)
		return response(400, `{}`), nil
	})}
	t.Cleanup(func() {
		resetCaches()
		httpClient, nowFunc, retryBaseDelay = oldClient, oldNow, oldRetry
		spotBaseURL, futuresBaseURL, yahooBaseURL, yahooFallbackBaseURL = oldSpot, oldFutures, oldYahoo, oldFallback
	})
}
func et(date string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", date, newYork)
	if err != nil {
		panic(err)
	}
	return t
}
func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func klineRows(times []time.Time) string {
	rows := make([][]any, 0, len(times))
	for i, t := range times {
		rows = append(rows, []any{t.UnixMilli(), fmt.Sprint(100 + i), fmt.Sprint(110 + i), fmt.Sprint(90 + i), fmt.Sprint(105 + i), "50"})
	}
	return encode(rows)
}
func chartJSON(times []time.Time, adjusted bool) string {
	stamps := make([]int64, 0, len(times))
	fields := map[string][]any{"open": {}, "high": {}, "low": {}, "close": {}, "volume": {}}
	adj := make([]any, 0, len(times))
	for i, t := range times {
		stamps = append(stamps, t.Unix())
		fields["open"] = append(fields["open"], float64(100+i))
		fields["high"] = append(fields["high"], float64(110+i))
		fields["low"] = append(fields["low"], float64(90+i))
		fields["close"] = append(fields["close"], float64(105+i))
		fields["volume"] = append(fields["volume"], float64(50+i))
		adj = append(adj, float64(105+i)/2)
	}
	indicators := map[string]any{"quote": []any{fields}}
	if adjusted {
		indicators["adjclose"] = []any{map[string]any{"adjclose": adj}}
	}
	return encode(map[string]any{"chart": map[string]any{"result": []any{map[string]any{"timestamp": stamps, "indicators": indicators}}, "error": nil}})
}
func seedRegistry(t *testing.T) {
	t.Helper()
	if _, err := ListSymbols(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func requireAscending(t *testing.T, bars []Bar) {
	t.Helper()
	for i, bar := range bars {
		if bar.OpenTime.Location() != time.UTC {
			t.Fatalf("not UTC: %v", bar.OpenTime)
		}
		if i > 0 && !bars[i-1].OpenTime.Before(bar.OpenTime) {
			t.Fatalf("not strictly ascending at %d", i)
		}
	}
}
