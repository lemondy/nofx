package breakout

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewC1EmptyMainUniverseKeepsSlowTop(t *testing.T) {
	oldHTTP, oldPath := binanceHTTP, gainerHistPath
	gainerHistPath = filepath.Join(t.TempDir(), "gainer_history.json")
	binanceHTTP = &http.Client{Transport: cacheRT(func(*http.Request) (*http.Response, error) { return cacheResp(`[]`), nil })}
	shortScanCacheMu.Lock()
	oldCache := shortScanCache
	shortScanCache = map[string]*shortScanCacheEntry{}
	shortScanCacheMu.Unlock()
	slowTopMu.Lock()
	oldSlow, oldAt := slowTopCache, slowTopAt
	slowTopCache, slowTopAt = []ShortSignal{{Symbol: "TOPUSDT", Score: 70, Universe: "near_high"}}, time.Now()
	slowTopMu.Unlock()
	t.Cleanup(func() {
		binanceHTTP, gainerHistPath = oldHTTP, oldPath
		shortScanCacheMu.Lock()
		shortScanCache = oldCache
		shortScanCacheMu.Unlock()
		slowTopMu.Lock()
		slowTopCache, slowTopAt = oldSlow, oldAt
		slowTopMu.Unlock()
	})
	got, _, err := ScanShorts(1, -1, 1)
	if err != nil || len(got) != 1 || got[0].Symbol != "TOPUSDT" {
		t.Fatalf("healthy slow-top board discarded: %v, %v", got, err)
	}
	shortScanCacheMu.Lock()
	shortScanCache = map[string]*shortScanCacheEntry{}
	shortScanCacheMu.Unlock()
	slowTopMu.Lock()
	slowTopAt = time.Now().Add(-2*slowTopCacheTTL - time.Second)
	slowTopMu.Unlock()
	if got, _, err := ScanShorts(1, -1, 1); err == nil || len(got) != 0 {
		t.Fatalf("expired slow-top reused: %v, %v", got, err)
	}
}
