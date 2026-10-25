package breakout

import (
	"net/http"
	"testing"
	"time"

	"nofx/market/pubcache"
)

func TestFuturesPubCacheOverlayPreservesClosedSeriesCache(t *testing.T) {
	oldHTTP, oldEnabled, oldNow := binanceHTTP, cacheEnabled, cacheNow
	cacheEnabled = true
	resetResponseCaches()
	t.Cleanup(func() {
		binanceHTTP, cacheEnabled, cacheNow = oldHTTP, oldEnabled, oldNow
		resetResponseCaches()
	})
	open := time.Now().Truncate(15 * time.Minute)
	now := open.Add(time.Second)
	cacheNow = func() time.Time { return now }
	c := pubcache.New(pubcache.Options{Now: func() time.Time { return now }})
	calls := 0
	binanceHTTP = &http.Client{Transport: c.Wrap(cacheRT(func(*http.Request) (*http.Response, error) {
		calls++
		return cacheResp("[" + klineRow(open.Add(-15*time.Minute).UnixMilli(), open.UnixMilli()-1) + "," +
			klineRow(open.UnixMilli(), open.Add(15*time.Minute).UnixMilli()-1) + "]"), nil
	}))}
	fetch := func() {
		bars, err := NewBinanceDS("PUBCACHEUSDT").Klines("15m", 2)
		if err != nil || len(bars) != 1 || bars[0].Close != 1.5 {
			t.Fatalf("parsed/copy/forming-bar behavior changed: %v %v", bars, err)
		}
		bars[0].Close = -1
	}
	fetch()
	fetch()
	if calls != 1 {
		t.Fatalf("duplicate request not shared: %d", calls)
	}
	before := c.Stats().Counts
	now = now.Add(pubcache.KlineTTL)
	fetch()
	if calls != 1 || c.Stats().Counts != before {
		t.Fatalf("closed-series cache must bypass expired pubcache: calls=%d stats=%+v", calls, c.Stats())
	}
	now = open.Add(15*time.Minute + klineCacheSlack)
	fetch()
	if calls != 2 || c.Stats().Misses != 2 {
		t.Fatalf("both expired caches must refetch: calls=%d stats=%+v", calls, c.Stats())
	}
}
