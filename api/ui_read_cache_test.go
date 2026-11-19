package api

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/trader"
)

func TestUIReadCacheFreshAndSingleflight(t *testing.T) {
	var c uiReadCache[int]
	var calls int32
	gate := make(chan struct{})
	fetch := func() (int, error) {
		n := int(atomic.AddInt32(&calls, 1))
		<-gate
		return n * 10, nil
	}

	const waiters = 6
	vals := make([]int, waiters)
	stales := make([]bool, waiters)
	errs := make([]error, waiters)
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			vals[idx], stales[idx], _, errs[idx] = c.get(context.Background(), "t1", time.Second, time.Minute, nil, fetch)
		}(i)
	}
	time.Sleep(25 * time.Millisecond)
	close(gate)
	wg.Wait()

	if calls != 1 {
		t.Fatalf("concurrent cold calls should coalesce to 1 fetch, got %d", calls)
	}
	for i := 0; i < waiters; i++ {
		if errs[i] != nil || stales[i] || vals[i] != 10 {
			t.Fatalf("waiter %d = (%d, stale=%v, err=%v), want (10, false, nil)", i, vals[i], stales[i], errs[i])
		}
	}

	// Next healthy request always fetches fresh live data (freshTTL = 0).
	v, stale, _, err := c.get(context.Background(), "t1", time.Second, time.Minute, nil, func() (int, error) {
		return 20, nil
	})
	if err != nil || stale || v != 20 {
		t.Fatalf("subsequent healthy fetch = (%d, stale=%v, err=%v), want (20, false, nil)", v, stale, err)
	}
}

func TestUIReadCacheStaleOnErrorAndSoftTimeout(t *testing.T) {
	clock := time.Date(2026, 11, 19, 22, 0, 0, 0, time.UTC)
	c := uiReadCache[string]{now: func() time.Time { return clock }}

	// Cold error propagates.
	if _, _, _, err := c.get(context.Background(), "t1", 50*time.Millisecond, time.Minute, nil, func() (string, error) {
		return "", errors.New("binance timeout")
	}); err == nil {
		t.Fatal("cold error must propagate when no cached snapshot exists")
	}

	// Seed a good snapshot.
	v, stale, _, err := c.get(context.Background(), "t1", 50*time.Millisecond, time.Minute, nil, func() (string, error) {
		return "v1", nil
	})
	if err != nil || stale || v != "v1" {
		t.Fatalf("seed = (%q, stale=%v, err=%v), want (v1, false, nil)", v, stale, err)
	}

	// Error falls back to cached snapshot within staleTTL.
	v, stale, at, err := c.get(context.Background(), "t1", 50*time.Millisecond, time.Minute, nil, func() (string, error) {
		return "", errors.New("binance 30s timeout")
	})
	if err != nil || !stale || v != "v1" || !at.Equal(clock) {
		t.Fatalf("stale-on-error = (%q, stale=%v, at=%v, err=%v), want (v1, true, %v, nil)", v, stale, at, err, clock)
	}

	// Slow fetch beyond softWait returns stale immediately while completing in background.
	release := make(chan struct{})
	bgDone := make(chan struct{})
	start := time.Now()
	v, stale, _, err = c.get(context.Background(), "t1", 40*time.Millisecond, time.Minute, nil, func() (string, error) {
		defer close(bgDone)
		<-release
		return "v2", nil
	})
	elapsed := time.Since(start)
	if err != nil || !stale || v != "v1" {
		t.Fatalf("soft-timeout = (%q, stale=%v, err=%v), want (v1, true, nil)", v, stale, err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("soft-timeout waited %s, want ~40ms", elapsed)
	}
	close(release)
	<-bgDone
	// Give background goroutine a moment to commit the refreshed entry.
	time.Sleep(10 * time.Millisecond)

	// Next failing request sees the background-refreshed v2 snapshot.
	v, stale, _, err = c.get(context.Background(), "t1", 50*time.Millisecond, time.Minute, nil, func() (string, error) {
		return "", errors.New("transient error")
	})
	if err != nil || !stale || v != "v2" {
		t.Fatalf("after background refresh = (%q, stale=%v, err=%v), want (v2, true, nil)", v, stale, err)
	}

	// Expired snapshot past staleTTL must NOT be served on error.
	clock = clock.Add(2 * time.Minute)
	if _, _, _, err := c.get(context.Background(), "t1", 50*time.Millisecond, time.Minute, nil, func() (string, error) {
		return "", errors.New("still down")
	}); err == nil {
		t.Fatal("expired snapshot past staleTTL must not mask error")
	}
}

func TestUIReadCacheShouldCachePredicate(t *testing.T) {
	var c uiReadCache[int]
	shouldCache := func(v int) bool { return v > 0 }

	// Seed good value (100).
	if v, stale, _, err := c.get(context.Background(), "k", 50*time.Millisecond, time.Minute, shouldCache, func() (int, error) {
		return 100, nil
	}); err != nil || stale || v != 100 {
		t.Fatalf("seed failed: %v %v %v", v, stale, err)
	}

	// Fast degraded response (0) is returned live (not cached) and must not overwrite 100.
	if v, stale, _, err := c.get(context.Background(), "k", 50*time.Millisecond, time.Minute, shouldCache, func() (int, error) {
		return 0, nil
	}); err != nil || stale || v != 0 {
		t.Fatalf("degraded fast response = (%d, stale=%v, err=%v), want (0, false, nil)", v, stale, err)
	}

	// Subsequent error still falls back to last good value 100.
	if v, stale, _, err := c.get(context.Background(), "k", 50*time.Millisecond, time.Minute, shouldCache, func() (int, error) {
		return 0, errors.New("boom")
	}); err != nil || !stale || v != 100 {
		t.Fatalf("fallback after uncached degraded value = (%d, stale=%v, err=%v), want (100, true, nil)", v, stale, err)
	}
}

func TestAttachPositionProtectionOrdersConcurrent(t *testing.T) {
	orig := uiFetchOpenOrders
	defer func() { uiFetchOpenOrders = orig }()

	const delay = 50 * time.Millisecond
	var calls int32
	uiFetchOpenOrders = func(_ string, sym string, _ *trader.AutoTrader) ([]trader.OpenOrder, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(delay)
		if sym == "FAILUSDT" {
			return nil, errors.New("exchange error")
		}
		return []trader.OpenOrder{
			{Symbol: sym, PositionSide: "LONG", Type: "STOP_MARKET", StopPrice: 95.5},
			{Symbol: sym, PositionSide: "LONG", Type: "TAKE_PROFIT_MARKET", StopPrice: 120.0},
		}, nil
	}

	positions := []map[string]interface{}{
		{"symbol": "BTCUSDT", "side": "long"},
		{"symbol": "BTCUSDT", "side": "short"}, // duplicate symbol must be fetched once
		{"symbol": "ETHUSDT", "side": "long"},
		{"symbol": "SOLUSDT", "side": "long"},
		{"symbol": "BNBUSDT", "side": "long"},
		{"symbol": "FAILUSDT", "side": "long"},
	}
	start := time.Now()
	attachPositionProtectionOrders("t1", nil, positions)
	elapsed := time.Since(start)

	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Fatalf("distinct symbol fetches = %d, want 5", got)
	}
	if elapsed >= 3*delay {
		t.Fatalf("attachPositionProtectionOrders took %s, want concurrent (~%s)", elapsed, delay)
	}
	protBTC := positions[0]["protection"].(map[string]interface{})
	if protBTC["sl_price"] != 95.5 || protBTC["tp_price"] != 120.0 {
		t.Fatalf("BTC long protection = %+v", protBTC)
	}
	protBTCShort := positions[1]["protection"].(map[string]interface{})
	if len(protBTCShort) != 0 {
		t.Fatalf("BTC short should ignore LONG-only orders, got %+v", protBTCShort)
	}
	protFail := positions[5]["protection"].(map[string]interface{})
	if protFail["protection_error"] != true {
		t.Fatalf("FAILUSDT protection = %+v, want protection_error=true", protFail)
	}
}
