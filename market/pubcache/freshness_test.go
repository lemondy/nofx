package pubcache

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestPubCacheFormingCandleExpiresAtClose(t *testing.T) {
	closeAt := time.Date(2026, 10, 25, 12, 1, 0, 0, time.UTC)
	openAt := closeAt.Add(-time.Minute)
	for _, path := range []string{"/fapi/v1/klines", "/api/kline/list/open"} {
		t.Run(path, func(t *testing.T) {
			now := closeAt.Add(-5 * time.Second)
			c := New(Options{Now: func() time.Time { return now }})
			calls := 0
			rt := c.Wrap(roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				// CoinAnk has end=0 for forming rows. Binance carries the
				// last millisecond of the bar; expire there conservatively.
				body := fmt.Sprintf(`[[%d,"1","2","0.5","1.5","10",%d,"15",5,"4","6"]]`, openAt.UnixMilli(), closeAt.Add(-time.Millisecond).UnixMilli())
				if path == "/api/kline/list/open" {
					body = fmt.Sprintf(`{"success":true,"data":[[%d.0,0.0,1,1.5,2,0.5,10,15,5]]}`, openAt.UnixMilli())
				}
				return response(200, body), nil
			}))
			url := "https://example.test" + path + "?interval=1m"
			read(t, rt, request(t, "GET", url))
			now = now.Add(time.Second)
			read(t, rt, request(t, "GET", url))
			if calls != 1 {
				t.Fatal("within-bar requests must share the response")
			}
			now = closeAt
			read(t, rt, request(t, "GET", url))
			if calls != 2 {
				t.Fatal("old partial OHLC crossed the candle-close boundary")
			}
		})
	}
}

func TestPubCacheSharedDefaultAcrossClients(t *testing.T) {
	before := Stats()
	calls := 0
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(200, `[]`), nil
	})
	// A unique complete URL avoids interference from other default-cache tests.
	url := klineURL + fmt.Sprintf("&test=%d", time.Now().UnixNano())
	read(t, Wrap(base), request(t, "GET", url))
	read(t, Wrap(base), request(t, "GET", url))
	after := Stats()
	if calls != 1 || after.Hits-before.Hits != 1 || after.Misses-before.Misses != 1 {
		t.Fatalf("Wrap/Stats must share process state: calls=%d before=%+v after=%+v", calls, before, after)
	}
}
