package binance

import (
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
	"nofx/trader/types"
)

const fapiRulesJSON = `{"symbols":[{"symbol":"XUSDT","underlyingSubType":["test"],"orderType":["LIMIT","MARKET"],"timeInForce":["GTC"],"filters":[{"filterType":"LOT_SIZE","stepSize":"0.02500000","minQty":"0.02500000","maxQty":"1000"},{"filterType":"PRICE_FILTER","tickSize":"0.02500000"},{"filterType":"MIN_NOTIONAL","notional":"5.00000000"},{"filterType":"MAX_NUM_ORDERS","limit":200,"extra":{"values":[1,"0.00100000",true]}}]}]}`

func TestFuturesExchangeInfoCacheConcurrentAndTTL(t *testing.T) {
	var calls atomic.Int32
	tr := fapiTestTrader(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/exchangeInfo" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request %s", r.URL)
		}
		calls.Add(1)
		_, _ = io.WriteString(w, fapiRulesJSON)
	})
	clock := newFapiTestClock()
	tr.nowFn = clock.now
	other := &FuturesTrader{client: tr.client, nowFn: clock.now}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			reader := tr
			if i%2 == 0 {
				reader = other
			}
			rules, err := reader.getSymbolOrderRules("XUSDT")
			if err != nil || rules.StepSizeText != "0.02500000" || rules.MinNotional != 5 {
				t.Errorf("rules=%+v err=%v", rules, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent requests=%d", calls.Load())
	}
	if p, err := tr.GetSymbolPricePrecision("XUSDT"); err != nil || p != 3 {
		t.Fatalf("price precision=%d err=%v", p, err)
	}
	if tick, err := tr.symbolTickSize("XUSDT"); err != nil || tick != "0.02500000" {
		t.Fatalf("tick=%s err=%v", tick, err)
	}
	clock.advance(24*time.Hour - time.Nanosecond)
	_, _ = tr.getSymbolOrderRules("XUSDT")
	if calls.Load() != 1 {
		t.Fatal("refreshed before TTL")
	}
	clock.advance(time.Nanosecond)
	_, _ = tr.getSymbolOrderRules("XUSDT")
	if calls.Load() != 2 {
		t.Fatal("did not refresh at 24h")
	}
	// BaseURL separates testnet/mainnet even when transport and credentials match.
	second := fapiTestTrader(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = io.WriteString(w, fapiRulesJSON) })
	_, _ = second.getSymbolOrderRules("XUSDT")
	if calls.Load() != 3 {
		t.Fatal("different endpoint shared the table")
	}
}

func TestFuturesExchangeInfoCacheFailureBackoffAndStale(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	tr := fapiTestTrader(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, `{"code":-1000,"msg":"offline"}`, 500)
			return
		}
		_, _ = io.WriteString(w, fapiRulesJSON)
	})
	clock := newFapiTestClock()
	tr.nowFn = clock.now
	fail.Store(true)
	for i := 0; i < 10; i++ {
		if _, err := tr.getSymbolOrderRules("XUSDT"); err == nil {
			t.Fatal("initial error suppressed")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("initial failure retried %d times", calls.Load())
	}
	clock.advance(5 * time.Minute)
	fail.Store(false)
	if _, err := tr.getSymbolOrderRules("XUSDT"); err != nil {
		t.Fatal(err)
	}
	clock.advance(24 * time.Hour)
	fail.Store(true)
	for i := 0; i < 10; i++ {
		if _, err := tr.getSymbolOrderRules("XUSDT"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("stale backoff requests=%d", calls.Load())
	}
	clock.advance(5*time.Minute - time.Nanosecond)
	_, _ = tr.getSymbolOrderRules("XUSDT")
	if calls.Load() != 3 {
		t.Fatal("backoff expired too early")
	}
	clock.advance(time.Nanosecond)
	_, _ = tr.getSymbolOrderRules("XUSDT")
	if calls.Load() != 4 {
		t.Fatal("no retry at backoff boundary")
	}
	clock.advance(7 * 24 * time.Hour)
	if _, err := tr.getSymbolOrderRules("XUSDT"); err == nil {
		t.Fatal("served rules beyond seven-day stale grace")
	}
}

func TestFuturesExchangeInfoCacheMissUsesThrottledFullRefresh(t *testing.T) {
	var calls atomic.Int32
	tr := fapiTestTrader(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("symbol") != "" {
			t.Error("SDK has no single-symbol option")
		}
		_, _ = io.WriteString(w, fapiRulesJSON)
	})
	clock := newFapiTestClock()
	tr.nowFn = clock.now
	_, _ = tr.getSymbolOrderRules("XUSDT")
	for i := 0; i < 10; i++ {
		if _, err := tr.getSymbolOrderRules(fmt.Sprintf("MISSING%d", i)); err == nil {
			t.Fatal("missing symbol accepted")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("symbol miss bypassed refresh throttle")
	}
	clock.advance(5 * time.Minute)
	_, _ = tr.getSymbolOrderRules("MISSING")
	_, _ = tr.getSymbolOrderRules("OTHER")
	if calls.Load() != 2 {
		t.Fatalf("full-refresh fallback requests=%d", calls.Load())
	}
}

func TestFuturesExchangeInfoCacheDiskRoundTripAndDeepCopy(t *testing.T) {
	previous := FuturesRulesCachePath
	FuturesRulesCachePath = filepath.Join(t.TempDir(), "cache", "rules.json")
	t.Cleanup(func() { FuturesRulesCachePath = previous })
	var calls atomic.Int32
	tr := fapiTestTrader(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = io.WriteString(w, fapiRulesJSON) })
	tr.nowFn = newFapiTestClock().now
	before, err := tr.cachedSymbolRules("XUSDT")
	if err != nil {
		t.Fatal(err)
	}
	sharedFuturesRules.Lock()
	delete(sharedFuturesRules.entries, tr.client.BaseURL)
	sharedFuturesRules.Unlock()
	after, err := tr.cachedSymbolRules("XUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("disk restart made %d requests", calls.Load()-1)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(before.Filters, after.Filters) {
		t.Fatalf("lossy disk roundtrip:\n%#v\n%#v", before, after)
	}
	after.Filters[0]["stepSize"] = "999"
	after.Filters[3]["extra"].(map[string]interface{})["values"].([]interface{})[0] = "mutated"
	after.UnderlyingSubType[0] = "mutated"
	fresh, _ := tr.cachedSymbolRules("XUSDT")
	if !reflect.DeepEqual(before, fresh) {
		t.Fatal("caller mutated shared rule")
	}
	// Concurrent callers must own independent snapshots too.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, e := tr.cachedSymbolRules("XUSDT")
			if e != nil {
				t.Error(e)
				return
			}
			s.Filters[0]["stepSize"] = "123"
		}()
	}
	wg.Wait()
}

func TestFuturesExchangeInfoCacheRejectsInvalidDisk(t *testing.T) {
	for _, kind := range []string{"corrupt", "version", "endpoint", "future", "expired", "wrong-symbol"} {
		t.Run(kind, func(t *testing.T) {
			previous := FuturesRulesCachePath
			FuturesRulesCachePath = filepath.Join(t.TempDir(), "rules.json")
			t.Cleanup(func() { FuturesRulesCachePath = previous })
			var calls atomic.Int32
			tr := fapiTestTrader(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = io.WriteString(w, fapiRulesJSON) })
			clock := newFapiTestClock()
			tr.nowFn = clock.now
			var info futures.ExchangeInfo
			if err := json.Unmarshal([]byte(fapiRulesJSON), &info); err != nil {
				t.Fatal(err)
			}
			disk := futuresRulesDisk{futuresRulesVersion, tr.client.BaseURL, clock.now(), map[string]futures.Symbol{"XUSDT": info.Symbols[0]}}
			switch kind {
			case "version":
				disk.Version++
			case "endpoint":
				disk.BaseURL = "other"
			case "future":
				disk.FetchedAt = clock.now().Add(time.Hour)
			case "expired":
				disk.FetchedAt = clock.now().Add(-8 * 24 * time.Hour)
			case "wrong-symbol":
				disk.Rules["WRONG"] = disk.Rules["XUSDT"]
			}
			if err := writeFuturesRulesFile(FuturesRulesCachePath, disk); err != nil {
				t.Fatal(err)
			}
			if kind == "corrupt" {
				if err := os.WriteFile(FuturesRulesCachePath, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tr.getSymbolOrderRules("XUSDT"); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("invalid disk was reused")
			}
		})
	}
}

func TestFuturesExchangeInfoCacheStaleDiskAndFilterInvalidation(t *testing.T) {
	previous := FuturesRulesCachePath
	FuturesRulesCachePath = filepath.Join(t.TempDir(), "rules.json")
	t.Cleanup(func() { FuturesRulesCachePath = previous })
	var calls atomic.Int32
	var fail atomic.Bool
	tr := fapiTestTrader(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, `{"code":-1000,"msg":"offline"}`, 500)
			return
		}
		_, _ = io.WriteString(w, fapiRulesJSON)
	})
	clock := newFapiTestClock()
	tr.nowFn = clock.now
	if _, err := tr.getSymbolOrderRules("XUSDT"); err != nil {
		t.Fatal(err)
	}
	forget := func() {
		sharedFuturesRules.Lock()
		delete(sharedFuturesRules.entries, tr.client.BaseURL)
		sharedFuturesRules.Unlock()
	}
	forget()
	clock.advance(24 * time.Hour)
	fail.Store(true)
	if _, err := tr.getSymbolOrderRules("XUSDT"); err != nil {
		t.Fatalf("stale disk fallback failed: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatal("stale disk was used without attempting refresh")
	}
	// A filter error arriving before this process loads disk must still remove
	// the persisted symbol, without resetting the table's fetched_at timestamp.
	forget()
	tr.noteFilterFailure("XUSDT", &common.APIError{Code: -1013})
	data, err := os.ReadFile(FuturesRulesCachePath)
	if err != nil {
		t.Fatal(err)
	}
	var disk futuresRulesDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if _, ok := disk.Rules["XUSDT"]; ok {
		t.Fatal("invalidated symbol survived in disk")
	}
	if disk.FetchedAt.Equal(clock.now()) {
		t.Fatal("invalidation extended the table TTL")
	}
}

func TestFuturesExchangeInfoCacheFilterFailureThrottle(t *testing.T) {
	tr := fapiTestTrader(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, fapiRulesJSON) })
	clock := newFapiTestClock()
	tr.nowFn = clock.now
	_, _ = tr.getSymbolOrderRules("XUSDT")
	e := futuresRulesFor(tr.client.BaseURL)
	for _, err := range []error{errors.New("code=-1013"), &common.APIError{Code: -1111}, nil} {
		tr.noteFilterFailure("XUSDT", err)
	}
	e.mu.Lock()
	n := len(e.rules)
	e.mu.Unlock()
	if n != 1 {
		t.Fatal("non-filter error invalidated rules")
	}
	filterErr := fmt.Errorf("wrapped: %w", &common.APIError{Code: -1013, Message: "filter"})
	tr.noteFilterFailure("XUSDT", filterErr)
	e.mu.Lock()
	n = len(e.rules)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("-1013 did not evict symbol")
	}
	clock.advance(5 * time.Minute)
	_, _ = tr.getSymbolOrderRules("XUSDT")
	tr.noteFilterFailure("XUSDT", filterErr)
	e.mu.Lock()
	n = len(e.rules)
	e.mu.Unlock()
	if n != 1 {
		t.Fatal("10-minute invalidation throttle bypassed")
	}
	clock.advance(5 * time.Minute)
	tr.noteFilterFailure("XUSDT", filterErr)
	e.mu.Lock()
	n = len(e.rules)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("not invalidated at 10-minute boundary")
	}
}

func TestFuturesExchangeInfoFilterFailureOrderHooksNeverResubmit(t *testing.T) {
	for _, kind := range []string{"market-long", "market-short", "close-long", "close-short", "limit", "reduce-limit", "stop", "take-profit"} {
		t.Run(kind, func(t *testing.T) {
			var posts atomic.Int32
			tr := fapiTestTrader(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/fapi/v1/exchangeInfo":
					_, _ = io.WriteString(w, fapiRulesJSON)
				case strings.Contains(r.URL.Path, "positionRisk"):
					_, _ = io.WriteString(w, batch1PositionsJSON)
				case r.URL.Path == "/fapi/v1/symbolConfig":
					_, _ = io.WriteString(w, `[{"symbol":"XUSDT","leverage":2,"marginType":"CROSSED"}]`)
				case strings.HasSuffix(r.URL.Path, "/ticker/price"):
					_, _ = io.WriteString(w, `[{"symbol":"XUSDT","price":"100"}]`)
				case strings.HasSuffix(r.URL.Path, "/openOrders"), strings.HasSuffix(r.URL.Path, "/openAlgoOrders"):
					_, _ = io.WriteString(w, `[]`)
				case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/order") || strings.HasSuffix(r.URL.Path, "/algoOrder")):
					posts.Add(1)
					http.Error(w, `{"code":-1013,"msg":"Filter failure"}`, 400)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", 404)
				}
			})
			_, _ = tr.getSymbolOrderRules("XUSDT")
			var err error
			switch kind {
			case "market-long":
				_, err = tr.OpenLong("XUSDT", 1, 2)
			case "market-short":
				_, err = tr.OpenShort("XUSDT", 1, 2)
			case "close-long":
				_, err = tr.CloseLong("XUSDT", 1)
			case "close-short":
				_, err = tr.CloseShort("XUSDT", 1)
			case "limit", "reduce-limit":
				_, err = tr.PlaceLimitOrder(&types.LimitOrderRequest{Symbol: "XUSDT", Side: "BUY", Quantity: 1, Price: 100, Leverage: 2, ReduceOnly: kind == "reduce-limit"})
			case "stop":
				err = tr.SetStopLoss("XUSDT", "LONG", 0, 90)
			case "take-profit":
				err = tr.SetTakeProfit("XUSDT", "LONG", 0, 120)
			}
			if err == nil || posts.Load() != 1 {
				t.Fatalf("err=%v posts=%d", err, posts.Load())
			}
			e := futuresRulesFor(tr.client.BaseURL)
			e.mu.Lock()
			_, exists := e.rules["XUSDT"]
			e.mu.Unlock()
			if exists {
				t.Fatal("order error branch did not invalidate symbol")
			}
		})
	}
}
