package usstock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/logger"
)

func readSymbolsDiskTest(t *testing.T) symbolsDisk {
	t.Helper()
	data, err := os.ReadFile(SymbolsCachePath)
	if err != nil {
		t.Fatal(err)
	}
	var disk symbolsDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	return disk
}

func countRegistryRequests(spot, futures *atomic.Int32) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v3/exchangeInfo" {
			spot.Add(1)
			return response(200, spotFixture), nil
		}
		futures.Add(1)
		return response(200, futuresFixture), nil
	}
}

func TestRegistryDiskRestart(t *testing.T) {
	now := et("2026-10-09 12:00")
	setupHTTP(t, now, nil)
	var spot, futures atomic.Int32
	httpClient.Transport = countRegistryRequests(&spot, &futures)
	before, err := ListSymbols(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resetCaches() // Simulate a restart; retain only the persisted file.
	after, err := ListSymbols(context.Background())
	if err != nil || spot.Load() != 1 || futures.Load() != 1 || !reflect.DeepEqual(before, after) {
		t.Fatalf("restart: spot=%d futures=%d before=%v after=%v err=%v", spot.Load(), futures.Load(), before, after, err)
	}
	if info, ok := LookupSymbol(context.Background(), "AAPLBUSDT"); !ok || info.Underlying != "AAPL" {
		t.Fatalf("lookup from disk: %+v %v", info, ok)
	}
	after[0].Underlying = "bad"
	again, _ := ListSymbols(context.Background())
	if !reflect.DeepEqual(before, again) {
		t.Fatal("disk-loaded registry aliases callers")
	}
	// Reading disk must not renew the original fetch time.
	nowFunc = func() time.Time { return now.Add(24*time.Hour - time.Nanosecond) }
	if _, err := ListSymbols(context.Background()); err != nil || spot.Load() != 1 {
		t.Fatalf("unexpired registry: spot=%d err=%v", spot.Load(), err)
	}
	nowFunc = func() time.Time { return now.Add(24 * time.Hour) }
	if _, err := ListSymbols(context.Background()); err != nil || spot.Load() != 2 || futures.Load() != 2 {
		t.Fatalf("expired registry: spot=%d futures=%d err=%v", spot.Load(), futures.Load(), err)
	}
}

func TestRegistryCacheConcurrentSymbols(t *testing.T) {
	setupHTTP(t, et("2026-10-09 12:00"), nil)
	var spot, futures atomic.Int32
	httpClient.Transport = countRegistryRequests(&spot, &futures)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			symbols, err := ListSymbols(context.Background())
			if err != nil || len(symbols) != 2 {
				t.Errorf("symbols=%v err=%v", symbols, err)
				return
			}
			symbols[0].Underlying = "bad"
		}()
	}
	wg.Wait()
	if spot.Load() != 1 || futures.Load() != 1 {
		t.Fatalf("concurrent requests: spot=%d futures=%d", spot.Load(), futures.Load())
	}
}

func TestRegistryDiskInvalidation(t *testing.T) {
	for _, kind := range []string{"expired", "spot-url", "futures-url", "corrupt", "version", "future-time", "empty"} {
		t.Run(kind, func(t *testing.T) {
			start := et("2026-10-09 12:00")
			setupHTTP(t, start, nil)
			var spot, futures atomic.Int32
			httpClient.Transport = countRegistryRequests(&spot, &futures)
			seedRegistry(t)
			disk := readSymbolsDiskTest(t)
			now := start
			switch kind {
			case "expired":
				now = start.Add(24 * time.Hour)
			case "spot-url":
				disk.SpotBaseURL = "https://other-spot.test"
			case "futures-url":
				disk.FuturesBaseURL = "https://other-futures.test"
			case "version":
				disk.Version++
			case "future-time":
				disk.FetchedAt = start.Add(time.Hour)
			case "empty":
				disk.Symbols = nil
			}
			if err := writeSymbolsFile(SymbolsCachePath, disk); err != nil {
				t.Fatal(err)
			}
			if kind == "corrupt" {
				if err := os.WriteFile(SymbolsCachePath, []byte(`{"version":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			resetCaches()
			nowFunc = func() time.Time { return now }
			symbols, err := ListSymbols(context.Background())
			if err != nil || len(symbols) != 2 || spot.Load() != 2 || futures.Load() != 2 {
				t.Fatalf("invalid cache: spot=%d futures=%d symbols=%v err=%v", spot.Load(), futures.Load(), symbols, err)
			}
			disk = readSymbolsDiskTest(t)
			if disk.Version != symbolsDiskVersion || disk.SpotBaseURL != spotBaseURL || disk.FuturesBaseURL != futuresBaseURL || !disk.FetchedAt.Equal(now) || !reflect.DeepEqual(disk.Symbols, symbols) {
				t.Fatalf("cache not replaced: %+v", disk)
			}
		})
	}
}

func TestRegistryDiskStaleFallback(t *testing.T) {
	for _, source := range []string{"spot", "futures"} {
		t.Run(source, func(t *testing.T) {
			start := et("2026-10-09 12:00")
			setupHTTP(t, start, nil)
			before, err := ListSymbols(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			resetCaches()
			// Registry retains its existing unlimited failed-refresh fallback.
			nowFunc = func() time.Time { return start.Add(30 * 24 * time.Hour) }
			calls := 0
			httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if source == "futures" && r.URL.Path == "/api/v3/exchangeInfo" {
					return response(200, spotFixture), nil
				}
				return nil, errors.New("offline")
			})
			after, err := ListSymbols(context.Background())
			wantCalls := 1
			if source == "futures" {
				wantCalls = 2
			}
			if err != nil || !reflect.DeepEqual(before, after) || calls != wantCalls {
				t.Fatalf("disk fallback: calls=%d symbols=%v err=%v", calls, after, err)
			}
			if disk := readSymbolsDiskTest(t); !disk.FetchedAt.Equal(start) {
				t.Fatalf("failure renewed disk time: %v", disk.FetchedAt)
			}
		})
	}
}

func TestRegistryDiskWriteFailureOnlyWarns(t *testing.T) {
	setupHTTP(t, et("2026-10-09 12:00"), nil)
	var logs bytes.Buffer
	old := logger.Log.Out
	logger.Log.SetOutput(&logs)
	t.Cleanup(func() { logger.Log.SetOutput(old) })
	SymbolsCachePath = t.TempDir() // Atomic rename onto a directory must fail.
	if symbols, err := ListSymbols(context.Background()); err != nil || len(symbols) != 2 {
		t.Fatalf("disk failure affected registry: %v %v", symbols, err)
	}
	if !strings.Contains(logs.String(), "WARN") || !strings.Contains(logs.String(), "cache write failed") {
		t.Fatalf("missing write warning: %s", logs.String())
	}
	files, err := os.ReadDir(filepath.Dir(SymbolsCachePath))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".usstock-symbols-") {
			t.Fatalf("temporary file leaked: %s", f.Name())
		}
	}
}
