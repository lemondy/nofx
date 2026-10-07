package market

import (
	"sync/atomic"
	"testing"
	"time"
)

// review 2026-10-07 B2-4 tests: injectable fetchers + fake clock.
func setupSentimentTest(t *testing.T) (clock *time.Time, calls *int32) {
	t.Helper()
	oc, os_, ob, on := sentimentFetchCrypto, sentimentFetchStock, sentimentFetchBinance, sentimentNow
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	var n int32
	sentimentFetchCrypto = func() *CryptoFG { atomic.AddInt32(&n, 1); return &CryptoFG{Value: 80} }
	sentimentFetchStock = func() *StockFG { return &StockFG{Score: 50} }
	sentimentFetchBinance = func() *BinanceCrowd { return &BinanceCrowd{BTCLS: 1} }
	sentimentNow = func() time.Time { return now }
	reset := func() {
		sentimentMu.Lock()
		sentimentCache, sentimentCachedAt, sentimentCacheTTLCur = nil, time.Time{}, sentimentCacheTTL
		lastCrypto, lastStock, lastBinance = nil, nil, nil
		lastCryptoAt, lastStockAt, lastBinanceAt = time.Time{}, time.Time{}, time.Time{}
		sentimentMu.Unlock()
	}
	reset()
	t.Cleanup(func() {
		sentimentFetchCrypto, sentimentFetchStock, sentimentFetchBinance, sentimentNow = oc, os_, ob, on
		reset()
	})
	return &now, &n
}

func TestSentimentAllSuccessCachesFullTTL(t *testing.T) {
	now, calls := setupSentimentTest(t)
	GetMarketSentiment()
	*now = now.Add(29 * time.Minute)
	GetMarketSentiment()
	if *calls != 1 {
		t.Fatalf("fetches = %d, want 1 (cached 30 min)", *calls)
	}
	*now = now.Add(2 * time.Minute)
	GetMarketSentiment()
	if *calls != 2 {
		t.Fatalf("fetches = %d, want 2 after TTL", *calls)
	}
}

func TestSentimentFailureAfterSuccessReusesPrevious(t *testing.T) {
	now, _ := setupSentimentTest(t)
	GetMarketSentiment()
	sentimentFetchCrypto = func() *CryptoFG { return nil }
	*now = now.Add(31 * time.Minute)
	s := GetMarketSentiment()
	if s.Crypto == nil || s.Crypto.Value != 80 {
		t.Fatalf("crypto = %+v, want previous value reused", s.Crypto)
	}
	found := false
	for _, n := range s.Notes {
		if n == "crypto_fg_stale_reused" {
			found = true
		}
		if n == "crypto_fg_fetch_failed" {
			t.Fatal("reused source must not also be noted as failed")
		}
	}
	if !found {
		t.Fatalf("notes = %v, want crypto_fg_stale_reused", s.Notes)
	}
	if s.FetchedAt != "2026-10-07 12:31 UTC" {
		t.Fatalf("FetchedAt = %q, must stay the honest refresh wall clock", s.FetchedAt)
	}
	// a partial composite is cached only briefly
	*now = now.Add(3 * time.Hour)
	s = GetMarketSentiment()
	if s.Crypto != nil {
		t.Fatalf("crypto = %+v, want nil once the last good value is >2h old", s.Crypto)
	}
}

func TestSentimentFailureWithNoPriorUsesShortTTL(t *testing.T) {
	now, calls := setupSentimentTest(t)
	fail := true
	sentimentFetchCrypto = func() *CryptoFG {
		atomic.AddInt32(calls, 1)
		if fail {
			return nil
		}
		return &CryptoFG{Value: 70}
	}
	s := GetMarketSentiment()
	if s.Crypto != nil {
		t.Fatal("crypto should be nil on first failure")
	}
	*now = now.Add(time.Minute)
	GetMarketSentiment()
	if *calls != 1 {
		t.Fatalf("fetches = %d, want 1 within the short TTL", *calls)
	}
	fail = false
	*now = now.Add(2 * time.Minute) // 3 min since failure, far below 30 min
	s = GetMarketSentiment()
	if *calls != 2 || s.Crypto == nil || s.Crypto.Value != 70 {
		t.Fatalf("fetches=%d crypto=%+v, want retry after short TTL to succeed", *calls, s.Crypto)
	}
}

// review 2026-10-07 B2-4: a panicking refresh must release the single-flight
// slot — otherwise every later call blocks or never refreshes again.
func TestSentimentRefreshPanicReleasesInflight(t *testing.T) {
	origC, origS, origB := sentimentFetchCrypto, sentimentFetchStock, sentimentFetchBinance
	defer func() { sentimentFetchCrypto, sentimentFetchStock, sentimentFetchBinance = origC, origS, origB }()
	sentimentMu.Lock()
	sentimentCache, sentimentInflight = nil, nil
	sentimentMu.Unlock()

	ch := make(chan struct{})
	sentimentMu.Lock()
	sentimentInflight = ch
	sentimentMu.Unlock()
	sentimentFetchCrypto = func() *CryptoFG { return nil }
	sentimentFetchStock = func() *StockFG { return nil }
	sentimentFetchBinance = func() *BinanceCrowd { return nil }
	// Make refreshSentiment panic inside the publisher.
	func() {
		defer func() { _ = recover() }()
		origRefresh := sentimentPanicHook
		sentimentPanicHook = func() { panic("boom") }
		defer func() { sentimentPanicHook = origRefresh }()
		_ = publishSentimentRefresh(ch)
	}()
	select {
	case <-ch:
	default:
		t.Fatal("in-flight channel not closed after a panicking refresh")
	}
	sentimentMu.Lock()
	inflight := sentimentInflight
	sentimentMu.Unlock()
	if inflight != nil {
		t.Fatal("sentimentInflight not cleared after a panicking refresh")
	}
}
