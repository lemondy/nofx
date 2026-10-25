package usstock

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestRegistryMatchingFiltersAndCache(t *testing.T) {
	now := et("2026-10-09 12:00")
	setupHTTP(t, now, nil)
	symbols, err := ListSymbols(context.Background())
	if err != nil || len(symbols) != 2 {
		t.Fatalf("symbols=%+v err=%v", symbols, err)
	}
	aapl, ok := LookupSymbol(context.Background(), "AAPLBUSDT")
	if !ok || aapl.Underlying != "AAPL" || aapl.BaseAsset != "AAPLB" || aapl.Status != "TRADING" || aapl.TickSize != .01 || aapl.StepSize != .001 || aapl.MinQty != .002 || aapl.MinNotional != 5 {
		t.Fatalf("AAPL: %+v", aapl)
	}
	spy, ok := LookupSymbol(context.Background(), "SPYBUSDT")
	if !ok || spy.MinNotional != 10 {
		t.Fatalf("MIN_NOTIONAL fallback: %+v", spy)
	}
	for _, s := range []string{"BNBUSDT", "ARBUSDT", "SHIBUSDT", "QNTBUSDT", "TSLABUSDT", "NVDABUSDC", "HKTESTBUSDT", "PREBUSDT"} {
		if _, ok := LookupSymbol(context.Background(), s); ok {
			t.Errorf("incorrect match: %s", s)
		}
	}
	// Callers cannot mutate the cache's registry.
	symbols[0].Underlying = "bad"
	aapl, _ = LookupSymbol(context.Background(), "AAPLBUSDT")
	if aapl.Underlying != "AAPL" {
		t.Fatal("cache was aliased")
	}
	calls := 0
	httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, errors.New("offline") })
	if _, err := ListSymbols(context.Background()); err != nil || calls != 0 {
		t.Fatalf("unexpired cache fetched: calls=%d err=%v", calls, err)
	}
	nowFunc = func() time.Time { return now.Add(24 * time.Hour) }
	symbols, err = ListSymbols(context.Background())
	if err != nil || len(symbols) != 2 || calls != 1 {
		t.Fatalf("last good set: symbols=%v err=%v calls=%d", symbols, err, calls)
	}
}
func TestRegistryInitialFailureRefusesLookup(t *testing.T) {
	setupHTTP(t, et("2026-10-09 12:00"), nil)
	httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { return response(503, `{}`), nil })
	if _, err := ListSymbols(context.Background()); err == nil {
		t.Fatal("expected initial load error")
	}
	if _, ok := LookupSymbol(context.Background(), "AAPLBUSDT"); ok {
		t.Fatal("failed registry allowed lookup")
	}
}
func TestRegistryFuturesFailureRetainsLastSet(t *testing.T) {
	now := et("2026-10-09 12:00")
	setupHTTP(t, now, nil)
	seedRegistry(t)
	nowFunc = func() time.Time { return now.Add(25 * time.Hour) }
	httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "spot.test" {
			return response(200, spotFixture), nil
		}
		return response(503, `{}`), nil
	})
	symbols, err := ListSymbols(context.Background())
	if err != nil || len(symbols) != 2 {
		t.Fatalf("symbols=%v err=%v", symbols, err)
	}
}
