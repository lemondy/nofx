package binance_bstock

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	binance "github.com/adshao/go-binance/v2"
	"nofx/logger"
)

// Only reset with no requests in flight, to simulate a process restart.
func resetRulesMemory() {
	sharedSpotRules.Lock()
	sharedSpotRules.entries = make(map[string]*spotRulesEntry)
	sharedSpotRules.Unlock()
}

func setupRulesCache(t *testing.T) {
	t.Helper()
	oldPath := RulesCachePath
	RulesCachePath = filepath.Join(t.TempDir(), "cache", "rules.json")
	resetRulesMemory()
	t.Cleanup(func() {
		resetRulesMemory()
		RulesCachePath = oldPath
	})
}

func clearRulesCache(t *testing.T) {
	t.Helper()
	resetRulesMemory()
	if err := os.Remove(RulesCachePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

type rulesRoundTripFunc func(*http.Request) (*http.Response, error)

func (f rulesRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func rulesResponse(t *testing.T, symbols ...binance.Symbol) *http.Response {
	t.Helper()
	data, err := json.Marshal(binance.ExchangeInfo{Symbols: symbols})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}
}

func rulesTrader(now func() time.Time, handler rulesRoundTripFunc) *BStockTrader {
	tr := NewBStockTrader("test-key", "test-secret")
	tr.client.BaseURL = "https://bstock.test"
	tr.client.HTTPClient = &http.Client{Transport: handler}
	tr.now = now
	return tr
}

func captureRulesWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	old := logger.Log.Out
	logger.Log.SetOutput(&logs)
	t.Cleanup(func() { logger.Log.SetOutput(old) })
	return &logs
}

func TestRulesCacheSharedConcurrentSymbols(t *testing.T) {
	setupRulesCache(t)
	now := time.Unix(1700000000, 0)
	var requests atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	handler := rulesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v3/exchangeInfo" || r.URL.RawQuery != "" {
			t.Errorf("unexpected full request: %s", r.URL)
		}
		if requests.Add(1) == 1 {
			close(entered)
		}
		<-release
		bad := stockRule()
		bad.Symbol = "BTCUSDT"
		return rulesResponse(t, stockRule(), bad), nil
	})
	traders := []*BStockTrader{rulesTrader(func() time.Time { return now }, handler), rulesTrader(func() time.Time { return now }, handler)}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(tr *BStockTrader) {
			defer wg.Done()
			<-start
			rules, err := tr.symbols()
			if err != nil || len(rules) != 1 || rules["AAPLBUSDT"].BaseAsset != "AAPLB" {
				t.Errorf("rules=%v err=%v", rules, err)
			}
			// Every caller must receive its own nested filter map.
			if err == nil {
				rules["AAPLBUSDT"].Filters[0]["stepSize"] = "bad"
				delete(rules, "AAPLBUSDT")
			}
		}(traders[i%2])
	}
	close(start)
	<-entered
	close(release)
	wg.Wait()
	rules, err := traders[1].symbols()
	if err != nil || requests.Load() != 1 || rules["AAPLBUSDT"].Filters[0]["stepSize"] != "0.001" {
		t.Fatalf("requests=%d rules=%v err=%v", requests.Load(), rules, err)
	}
}

func TestRulesCacheTTLAndStaleFallback(t *testing.T) {
	setupRulesCache(t)
	logs := captureRulesWarnings(t)
	start := time.Unix(1700000000, 0)
	now := start
	calls, fail := 0, false
	tr := rulesTrader(func() time.Time { return now }, func(r *http.Request) (*http.Response, error) {
		calls++
		if fail {
			return nil, errors.New("offline")
		}
		return rulesResponse(t, stockRule()), nil
	})
	if _, err := tr.symbols(); err != nil {
		t.Fatal(err)
	}
	now = start.Add(24*time.Hour - time.Nanosecond)
	if _, err := tr.symbols(); err != nil || calls != 1 {
		t.Fatalf("unexpired: calls=%d err=%v", calls, err)
	}
	now = start.Add(24 * time.Hour)
	if _, err := tr.symbols(); err != nil || calls != 2 {
		t.Fatalf("refresh: calls=%d err=%v", calls, err)
	}
	refreshed := now
	fail = true
	now = refreshed.Add(24 * time.Hour)
	if rules, err := tr.symbols(); err != nil || len(rules) != 1 || calls != 3 {
		t.Fatalf("fallback: calls=%d rules=%v err=%v", calls, rules, err)
	}
	if !strings.Contains(logs.String(), "WARN") || !strings.Contains(logs.String(), "refresh failed") {
		t.Fatalf("missing fallback warning: %s", logs.String())
	}
	// Neither a failed refresh nor disk persistence may extend fetched_at.
	disk := readRulesDiskTest(t)
	if !disk.FetchedAt.Equal(refreshed) {
		t.Fatalf("failed refresh changed fetched_at: %v", disk.FetchedAt)
	}
	now = refreshed.Add(spotRulesTTL + spotRulesStaleGrace - time.Nanosecond)
	if _, err := tr.symbols(); err != nil {
		t.Fatalf("inside grace: %v", err)
	}
	now = refreshed.Add(spotRulesTTL + spotRulesStaleGrace)
	if _, err := tr.symbols(); err == nil {
		t.Fatal("rules used beyond stale grace")
	}
}

func TestRulesCacheInitialFailure(t *testing.T) {
	setupRulesCache(t)
	tr := rulesTrader(time.Now, func(r *http.Request) (*http.Response, error) { return nil, errors.New("offline") })
	if _, err := tr.symbols(); err == nil {
		t.Fatal("expected initial failure")
	}
}

func TestRulesCacheSingleSymbolAndNegativeCache(t *testing.T) {
	setupRulesCache(t)
	start := time.Unix(1700000000, 0)
	now := start
	calls := make(map[string]int)
	tr := rulesTrader(func() time.Time { return now }, func(r *http.Request) (*http.Response, error) {
		symbol := r.URL.Query().Get("symbol")
		calls[symbol]++
		if symbol == "" {
			return rulesResponse(t, stockRule()), nil
		}
		if symbol == "TSLABUSDT" {
			s := stockRule()
			s.Symbol, s.BaseAsset = symbol, "TSLAB"
			return rulesResponse(t, s), nil
		}
		return rulesResponse(t), nil
	})
	if _, err := tr.rule("TSLABUSDT"); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.rule("TSLABUSDT"); err != nil || calls[""] != 1 || calls["TSLABUSDT"] != 1 {
		t.Fatalf("single fill calls=%v err=%v", calls, err)
	}
	other := rulesTrader(tr.now, tr.client.HTTPClient.Transport.(rulesRoundTripFunc))
	if all, err := other.symbols(); err != nil || len(all) != 2 {
		t.Fatalf("addition not shared: %v %v", all, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := other.rule("MISSINGBUSDT"); err == nil {
			t.Fatal("expected missing-symbol error")
		}
	}
	now = start.Add(10*time.Minute - time.Nanosecond)
	_, _ = tr.rule("MISSINGBUSDT")
	if calls["MISSINGBUSDT"] != 1 {
		t.Fatalf("negative cache missed: %v", calls)
	}
	now = start.Add(10 * time.Minute)
	_, _ = tr.rule("MISSINGBUSDT")
	if calls["MISSINGBUSDT"] != 2 || calls[""] != 1 {
		t.Fatalf("negative TTL/full requests: %v", calls)
	}
	_, _ = tr.rule("")
	if calls[""] != 1 {
		t.Fatal("empty symbol caused a full exchangeInfo request")
	}
	// Single-symbol additions survive restart without renewing the whole table.
	disk := readRulesDiskTest(t)
	if len(disk.Rules) != 2 || !disk.FetchedAt.Equal(start) {
		t.Fatalf("single fill disk=%+v", disk)
	}
	resetRulesMemory()
	if _, err := other.rule("TSLABUSDT"); err != nil || calls[""] != 1 || calls["TSLABUSDT"] != 1 {
		t.Fatalf("single fill restart: calls=%v err=%v", calls, err)
	}
}

func TestRulesCacheSingleSymbolRejectsInvalidRules(t *testing.T) {
	for _, kind := range []string{"status", "quote", "base", "symbol", "spot-disabled", "api-error", "network-error"} {
		t.Run(kind, func(t *testing.T) {
			setupRulesCache(t)
			calls := 0
			tr := rulesTrader(time.Now, func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("symbol") == "" {
					return rulesResponse(t, stockRule()), nil
				}
				calls++
				s := stockRule()
				s.Symbol, s.BaseAsset = "TSLABUSDT", "TSLAB"
				switch kind {
				case "status":
					s.Status = "BREAK"
				case "quote":
					s.QuoteAsset = "USDC"
				case "base":
					s.BaseAsset = "TSLA"
				case "symbol":
					s = stockRule() // Do not accept a different returned symbol.
				case "spot-disabled":
					s.IsSpotTradingAllowed = false
				case "api-error":
					return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":-1121,"msg":"Invalid symbol."}`))}, nil
				case "network-error":
					return nil, errors.New("offline")
				}
				return rulesResponse(t, s), nil
			})
			for i := 0; i < 2; i++ {
				if _, err := tr.rule("TSLABUSDT"); err == nil {
					t.Fatal("invalid rule accepted")
				}
			}
			if calls != 1 {
				t.Fatalf("negative lookup requests=%d", calls)
			}
		})
	}
}

func TestRulesCacheConcurrentSingleSymbol(t *testing.T) {
	setupRulesCache(t)
	var full, single atomic.Int32
	tr := rulesTrader(time.Now, func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("symbol") == "" {
			full.Add(1)
			return rulesResponse(t, stockRule()), nil
		}
		single.Add(1)
		s := stockRule()
		s.Symbol, s.BaseAsset = "TSLABUSDT", "TSLAB"
		return rulesResponse(t, s), nil
	})
	other := rulesTrader(tr.now, tr.client.HTTPClient.Transport.(rulesRoundTripFunc))
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(tr *BStockTrader) {
			defer wg.Done()
			s, err := tr.rule("TSLABUSDT")
			if err != nil || s.Symbol != "TSLABUSDT" {
				t.Errorf("rule=%v err=%v", s, err)
			}
		}([]*BStockTrader{tr, other}[i%2])
	}
	wg.Wait()
	if full.Load() != 1 || single.Load() != 1 {
		t.Fatalf("full=%d single=%d", full.Load(), single.Load())
	}
}

func readRulesDiskTest(t *testing.T) spotRulesDisk {
	t.Helper()
	data, err := os.ReadFile(RulesCachePath)
	if err != nil {
		t.Fatal(err)
	}
	var disk spotRulesDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	return disk
}

func TestRulesCacheDiskRestartAndFilterRoundTrip(t *testing.T) {
	setupRulesCache(t)
	now := time.Unix(1700000000, 0)
	calls := 0
	rule := stockRule()
	rule.BaseAssetPrecision, rule.QuotePrecision = 8, 8
	rule.OrderTypes = []string{"LIMIT", "MARKET"}
	rule.Permissions = []string{"SPOT"}
	rule.PermissionSets = [][]string{{"SPOT", "TRD_GRP_001"}}
	rule.Filters[3]["avgPriceMins"] = float64(5)
	tr := rulesTrader(func() time.Time { return now }, func(r *http.Request) (*http.Response, error) {
		calls++
		return rulesResponse(t, rule), nil
	})
	before, err := tr.symbols()
	if err != nil {
		t.Fatal(err)
	}
	resetRulesMemory()
	after, err := tr.symbols()
	if err != nil || calls != 1 || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(before[rule.Symbol].Filters, after[rule.Symbol].Filters) {
		t.Fatalf("roundtrip: calls=%d before=%+v after=%+v err=%v", calls, before, after, err)
	}
	// The disk form must retain flags, string decimals and float64 numbers.
	if !reflect.DeepEqual(after[rule.Symbol], rule) {
		t.Fatalf("rule changed: got=%+v want=%+v", after[rule.Symbol], rule)
	}
	mutated := after[rule.Symbol]
	mutated.OrderTypes[0], mutated.Permissions[0], mutated.PermissionSets[0][0] = "bad", "bad", "bad"
	mutated.Filters[3]["avgPriceMins"] = float64(999)
	again, _ := tr.symbols()
	if !reflect.DeepEqual(again, before) {
		t.Fatal("nested slices/filters alias the shared cache")
	}
}

func TestRulesCacheDiskInvalidation(t *testing.T) {
	for _, kind := range []string{"expired", "base-url", "corrupt", "version", "future-time", "invalid-rules"} {
		t.Run(kind, func(t *testing.T) {
			setupRulesCache(t)
			start := time.Unix(1700000000, 0)
			now, calls := start, 0
			tr := rulesTrader(func() time.Time { return now }, func(r *http.Request) (*http.Response, error) {
				calls++
				return rulesResponse(t, stockRule()), nil
			})
			if _, err := tr.symbols(); err != nil {
				t.Fatal(err)
			}
			disk := readRulesDiskTest(t)
			switch kind {
			case "expired":
				now = start.Add(24 * time.Hour)
			case "base-url":
				disk.BaseURL = "https://testnet.test"
			case "version":
				disk.Version++
			case "future-time":
				disk.FetchedAt = start.Add(time.Hour)
			case "invalid-rules":
				disk.Rules["BAD"] = stockRule()
			}
			if err := writeRulesFile(RulesCachePath, disk); err != nil {
				t.Fatal(err)
			}
			if kind == "corrupt" {
				if err := os.WriteFile(RulesCachePath, []byte(`{"version":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			resetRulesMemory()
			if _, err := tr.symbols(); err != nil || calls != 2 {
				t.Fatalf("invalid cache not refreshed: calls=%d err=%v", calls, err)
			}
			disk = readRulesDiskTest(t)
			if disk.Version != spotRulesVersion || disk.BaseURL != tr.client.BaseURL || !disk.FetchedAt.Equal(now) || len(disk.Rules) != 1 {
				t.Fatalf("cache not replaced: %+v", disk)
			}
		})
	}
}

func TestRulesCacheDiskStaleFallback(t *testing.T) {
	setupRulesCache(t)
	start := time.Unix(1700000000, 0)
	now := start
	fail := false
	tr := rulesTrader(func() time.Time { return now }, func(r *http.Request) (*http.Response, error) {
		if fail {
			return nil, errors.New("offline")
		}
		return rulesResponse(t, stockRule()), nil
	})
	if _, err := tr.symbols(); err != nil {
		t.Fatal(err)
	}
	fail = true
	now = start.Add(25 * time.Hour)
	resetRulesMemory()
	if rules, err := tr.symbols(); err != nil || len(rules) != 1 {
		t.Fatalf("disk fallback: %v %v", rules, err)
	}
	now = start.Add(spotRulesTTL + spotRulesStaleGrace)
	resetRulesMemory()
	if _, err := tr.symbols(); err == nil {
		t.Fatal("disk fallback exceeded grace")
	}
}

func TestRulesCacheBaseURLIsolation(t *testing.T) {
	setupRulesCache(t)
	calls := 0
	handler := rulesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return rulesResponse(t, stockRule()), nil
	})
	mainnet, testnet := rulesTrader(time.Now, handler), rulesTrader(time.Now, handler)
	testnet.client.BaseURL = "https://testnet.test"
	for _, tr := range []*BStockTrader{mainnet, testnet, mainnet, testnet} {
		if _, err := tr.symbols(); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("different endpoints shared rules: requests=%d", calls)
	}
}

func TestRulesCacheDiskWriteFailureOnlyWarns(t *testing.T) {
	setupRulesCache(t)
	logs := captureRulesWarnings(t)
	// Rename onto a directory fails even when the test runs with elevated rights.
	RulesCachePath = t.TempDir()
	tr := rulesTrader(time.Now, func(r *http.Request) (*http.Response, error) { return rulesResponse(t, stockRule()), nil })
	if rules, err := tr.symbols(); err != nil || len(rules) != 1 {
		t.Fatalf("write failure affected rules: %v %v", rules, err)
	}
	if !strings.Contains(logs.String(), "WARN") || !strings.Contains(logs.String(), "cache write failed") {
		t.Fatalf("missing write warning: %s", logs.String())
	}
	files, err := os.ReadDir(filepath.Dir(RulesCachePath))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".bstock-rules-") {
			t.Fatalf("temporary file leaked: %s", f.Name())
		}
	}
}

// A failed refresh must not be retried on every rule() call: the stale table is
// served and the network is left alone until the backoff expires.
func TestRulesCacheRefreshFailureBackoff(t *testing.T) {
	setupRulesCache(t)
	captureRulesWarnings(t)
	start := time.Unix(1700000000, 0)
	now := start
	calls, fail := 0, false
	tr := rulesTrader(func() time.Time { return now }, func(r *http.Request) (*http.Response, error) {
		calls++
		if fail {
			return nil, errors.New("offline")
		}
		return rulesResponse(t, stockRule()), nil
	})
	if _, err := tr.symbols(); err != nil || calls != 1 {
		t.Fatalf("initial load: calls=%d err=%v", calls, err)
	}
	fail = true
	now = start.Add(spotRulesTTL)
	for i := 0; i < 5; i++ {
		if rules, err := tr.symbols(); err != nil || len(rules) != 1 {
			t.Fatalf("stale fallback %d: rules=%v err=%v", i, rules, err)
		}
	}
	if calls != 2 {
		t.Fatalf("failed refresh retried inside the backoff: calls=%d", calls)
	}
	now = now.Add(spotRulesRefreshBackoff)
	if _, err := tr.symbols(); err != nil || calls != 3 {
		t.Fatalf("no retry after backoff: calls=%d err=%v", calls, err)
	}
	// A success clears the backoff state; the next expiry refreshes immediately.
	fail = false
	now = now.Add(spotRulesRefreshBackoff)
	if _, err := tr.symbols(); err != nil || calls != 4 {
		t.Fatalf("recovery refresh: calls=%d err=%v", calls, err)
	}
}
