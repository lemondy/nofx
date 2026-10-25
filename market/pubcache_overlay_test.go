package market

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"nofx/market/pubcache"
)

func TestOpenInterestPubCacheOverlay(t *testing.T) {
	oldTransport, oldFetch, oldNow := http.DefaultTransport, oiFetch, oiNow
	oiDataMu.Lock()
	oldCache := oiDataCache
	oiDataCache = map[string]oiDataEntry{}
	oiDataMu.Unlock()
	t.Cleanup(func() {
		http.DefaultTransport, oiFetch, oiNow = oldTransport, oldFetch, oldNow
		oiDataMu.Lock()
		oiDataCache = oldCache
		oiDataMu.Unlock()
	})
	clock := time.Now()
	oiNow = func() time.Time { return clock }
	parsedMisses, networkCalls := 0, 0
	oiFetch = func(symbol string) (*OIData, error) {
		parsedMisses++
		return fetchOIData(symbol) // real parsing and NewAPIClient transport chain
	}
	http.DefaultTransport = historicalRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		networkCalls++
		if r.URL.Path != "/futures/data/openInterestHist" || r.URL.Query().Get("period") != "1h" || r.URL.Query().Get("limit") != "30" {
			t.Fatalf("unexpected OI request: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`[{"sumOpenInterest":"10"},{"sumOpenInterest":"20"}]`))}, nil
	})
	read := func(symbol string) *OIData {
		t.Helper()
		d, err := getOpenInterestData(symbol)
		if err != nil || d.Latest != 20 || d.Average != 15 {
			t.Fatalf("OI result=%+v err=%v", d, err)
		}
		return d
	}

	t.Run("ParsedCacheServesTwoMinutesWithoutTouchingPubCache", func(t *testing.T) {
		symbol := "PUBCACHE_OVERLAY_TWO_MINUTES_USDT"
		before := pubcache.Stats().Counts
		read(symbol).Latest = -1
		afterFetch := pubcache.Stats().Counts
		if afterFetch.Misses != before.Misses+1 || networkCalls != 1 || parsedMisses != 1 {
			t.Fatalf("first fetch: stats=%+v network=%d parsed=%d", afterFetch, networkCalls, parsedMisses)
		}
		for _, elapsed := range []time.Duration{30 * time.Second, 90 * time.Second, 2 * time.Minute} {
			clock = oiDataCache[symbol].at.Add(elapsed)
			read(symbol)
		}
		if got := pubcache.Stats().Counts; got != afterFetch || networkCalls != 1 || parsedMisses != 1 {
			t.Fatalf("upper cache must bypass HTTP entirely: stats=%+v network=%d parsed=%d", got, networkCalls, parsedMisses)
		}
	})

	t.Run("ExpiredParsedCacheUsesRecentlyWarmedPubCache", func(t *testing.T) {
		// Independent callers can warm the HTTP entry shortly before the
		// 3-minute parsed entry expires. Seed that state without wall-clock sleeps.
		symbol := "PUBCACHE_OVERLAY_EXPIRED_USDT"
		clock = time.Now()
		oiDataMu.Lock()
		oiDataCache[symbol] = oiDataEntry{data: &OIData{Latest: -1}, at: clock.Add(-oiDataTTL)}
		oiDataMu.Unlock()
		if _, err := fetchOIData(symbol); err != nil { // another caller, bypassing the parsed cache
			t.Fatal(err)
		}
		before := pubcache.Stats().Counts
		networkBefore, parsedBefore := networkCalls, parsedMisses
		read(symbol)
		after := pubcache.Stats().Counts
		if parsedMisses != parsedBefore+1 || networkCalls != networkBefore || after.Hits != before.Hits+1 || after.Misses != before.Misses {
			t.Fatalf("expired upper entry must use fresh lower entry: before=%+v after=%+v network=%d parsed=%d", before, after, networkCalls, parsedMisses)
		}
		oiDataMu.Lock()
		refreshed := oiDataCache[symbol]
		oiDataMu.Unlock()
		if !refreshed.at.Equal(clock) || refreshed.data.Latest != 20 {
			t.Fatalf("parsed cache was not refreshed: %+v", refreshed)
		}
	})
}
