package kernel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"nofx/market/pubcache"
)

type pubcacheTickerRT func(*http.Request) (*http.Response, error)

func (f pubcacheTickerRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBinanceTopTickersPubCacheOverlayKeepsSixtySeconds(t *testing.T) {
	oldGet := binanceGetFn
	tickersMu.Lock()
	oldCache, oldFetched := tickersCache, tickersFetched
	tickersCache, tickersFetched = nil, time.Time{}
	tickersMu.Unlock()
	t.Cleanup(func() {
		binanceGetFn = oldGet
		tickersMu.Lock()
		tickersCache, tickersFetched = oldCache, oldFetched
		tickersMu.Unlock()
	})
	clock := time.Now()
	c := pubcache.New(pubcache.Options{Now: func() time.Time { return clock }})
	getCalls, networkCalls := 0, 0
	client := &http.Client{Transport: c.Wrap(pubcacheTickerRT(func(*http.Request) (*http.Response, error) {
		networkCalls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
			`[{"symbol":"PUBCACHEUSDT","lastPrice":"105","quoteVolume":"1e10"}]`))}, nil
	}))}
	binanceGetFn = func(ctx context.Context, path string, out interface{}) error {
		getCalls++
		if path != "/fapi/v1/ticker/24hr" {
			t.Fatalf("unexpected ticker path: %s", path)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fapi.binance.com"+path, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return json.NewDecoder(resp.Body).Decode(out)
	}
	fetch := func() {
		t.Helper()
		tickers, err := binanceTopTickers(context.Background())
		if err != nil || len(tickers) != 1 || tickers[0].Symbol != "PUBCACHEUSDT" {
			t.Fatalf("tickers=%+v err=%v", tickers, err)
		}
	}
	fetch()
	before := c.Stats().Counts
	if networkCalls != 1 || getCalls != 1 || before.Misses != 1 {
		t.Fatalf("first fetch: network=%d get=%d stats=%+v", networkCalls, getCalls, before)
	}
	// Age both layers beyond pubcache's 30s, but inside the original 60s.
	tickersMu.Lock()
	tickersFetched = time.Now().Add(-59 * time.Second)
	tickersMu.Unlock()
	clock = clock.Add(59 * time.Second)
	fetch()
	if getCalls != 1 || networkCalls != 1 || c.Stats().Counts != before {
		t.Fatalf("60s parsed cache must bypass HTTP: get=%d network=%d stats=%+v", getCalls, networkCalls, c.Stats())
	}
	tickersMu.Lock()
	tickersFetched = time.Now().Add(-61 * time.Second)
	tickersMu.Unlock()
	clock = clock.Add(2 * time.Second)
	fetch()
	if getCalls != 2 || networkCalls != 2 || c.Stats().Misses != 2 {
		t.Fatalf("expired parsed cache must refetch: get=%d network=%d stats=%+v", getCalls, networkCalls, c.Stats())
	}
}

func TestPubCacheKlineTTLDoesNotMisclassifyFreshness(t *testing.T) {
	fetched := time.Date(2026, 10, 25, 12, 10, 0, 0, time.UTC)
	// Keep the forming bar inside the same bar across the full cache window.
	barOpen := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	compute := func(lastOpen, sampled time.Time) *SymbolSignal {
		t.Helper()
		sig, err := ComputeSymbolSignals("CAPUSDT", sampledTestData(lastOpen, sampled), SignalOptions{Now: sampled, PrimaryTF: "1h"})
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	a := compute(barOpen, fetched)
	b := compute(barOpen, fetched.Add(pubcache.KlineTTL-time.Nanosecond))
	if len(b.DataFreshness.StaleTimeframes) != 0 || !b.Timeframes["1h"].UnclosedDropped {
		t.Fatalf("cached forming bar judged incorrectly: %+v", b.DataFreshness)
	}
	if a.DataQuality.Sufficient != b.DataQuality.Sufficient || a.DataQuality.Complete != b.DataQuality.Complete {
		t.Fatalf("short TTL changed data quality: before=%+v after=%+v", a.DataQuality, b.DataQuality)
	}
	stale := compute(barOpen.Add(-6*time.Hour), fetched.Add(pubcache.KlineTTL-time.Nanosecond))
	if len(stale.DataFreshness.StaleTimeframes) != 1 || staleGateCode(stale.HardGate.Long) == "" || staleGateCode(stale.HardGate.Short) == "" {
		t.Fatal("cache must not make old bar timestamps look freshly sampled")
	}
}
