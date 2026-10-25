package binance_bstock

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	binance "github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/common"
)

// Start with a valid persisted table so a test can reject every full request,
// including the first rule() after invalidation.
func seedFilterRules(t *testing.T, tr *BStockTrader, at time.Time, symbols ...binance.Symbol) {
	t.Helper()
	rules := make(map[string]binance.Symbol)
	for _, s := range symbols {
		rules[s.Symbol] = s
	}
	if err := writeRulesFile(RulesCachePath, spotRulesDisk{spotRulesVersion, tr.client.BaseURL, at, rules}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.symbols(); err != nil {
		t.Fatal(err)
	}
}

func otherFilterRule() binance.Symbol {
	s := stockRule()
	s.Symbol, s.BaseAsset = "TSLABUSDT", "TSLAB"
	return s
}

func assertFilterRuleCached(t *testing.T, tr *BStockTrader, symbol string, want bool) {
	t.Helper()
	e := sharedRulesFor(tr.client.BaseURL)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.rules[symbol]; ok != want {
		t.Fatalf("%s cached=%v, want %v", symbol, ok, want)
	}
}

func TestFilterFailureInvalidatesMemoryAndDisk(t *testing.T) {
	setupRulesCache(t)
	logs := captureRulesWarnings(t)
	now := time.Unix(1700000000, 0)
	apiErr := &common.APIError{Code: -1013, Message: "Filter failure: LOT_SIZE"}
	full, single, orders := 0, 0, 0
	updated := stockRule()
	updated.Filters[0]["stepSize"] = "0.01"
	updated.Filters[1]["tickSize"] = "0.1"
	updated.Filters[3]["avgPriceMins"] = float64(0) // JSON numbers round-trip as float64.
	tr := rulesTrader(func() time.Time { return now }, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v3/order":
			orders++
			return nil, apiErr // Preserve the typed error through the SDK/transport.
		case "/api/v3/exchangeInfo":
			if r.URL.Query().Get("symbol") != updated.Symbol {
				full++
				return nil, errors.New("unexpected full exchangeInfo request")
			}
			single++
			return rulesResponse(t, updated), nil
		default:
			return nil, fmt.Errorf("unexpected request: %s", r.URL)
		}
	})
	seedFilterRules(t, tr, now, stockRule(), otherFilterRule())
	e := sharedRulesFor(tr.client.BaseURL)
	e.mu.Lock()
	e.negative[updated.Symbol] = negativeRule{at: now, err: errors.New("old miss")}
	e.mu.Unlock()
	_, err := tr.BuyLimit(updated.Symbol, 1, 100)
	requireError(t, err, "place spot order: spot filter failure")
	if !errors.Is(err, apiErr) || orders != 1 || single != 0 || full != 0 {
		t.Fatalf("err=%v orders=%d single=%d full=%d", err, orders, single, full)
	}
	assertFilterRuleCached(t, tr, updated.Symbol, false)
	assertFilterRuleCached(t, tr, otherFilterRule().Symbol, true)
	e.mu.Lock()
	_, negative := e.negative[updated.Symbol]
	ageUnchanged := e.fetchedAt.Equal(now)
	e.mu.Unlock()
	disk := readRulesDiskTest(t)
	if _, ok := disk.Rules[updated.Symbol]; ok || len(disk.Rules) != 1 || negative || !ageUnchanged || !disk.FetchedAt.Equal(now) {
		t.Fatalf("invalidation changed table age or retained rule/negative: disk=%+v negative=%v", disk, negative)
	}
	if !strings.Contains(logs.String(), updated.Symbol) || !strings.Contains(logs.String(), apiErr.Error()) {
		t.Fatalf("missing invalidation log: %s", logs.String())
	}
	// The on-disk deletion must also survive a process restart.
	resetRulesMemory()
	s, err := tr.rule(updated.Symbol)
	if err != nil || !reflect.DeepEqual(s, updated) || single != 1 || full != 0 || orders != 1 {
		t.Fatalf("refill: rule=%+v err=%v single=%d full=%d orders=%d", s, err, single, full, orders)
	}
	disk = readRulesDiskTest(t)
	if !disk.FetchedAt.Equal(now) || disk.Rules[updated.Symbol].Filters[0]["stepSize"] != "0.01" {
		t.Fatalf("refill extended table age or lost new filter: %+v", disk)
	}
}

func TestFilterFailureIgnoresOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"rejected", &common.APIError{Code: -2010, Message: "Order rejected"}},
		{"other-code-with-filter-text", &common.APIError{Code: -2010, Message: "Filter failure: LOT_SIZE"}},
		{"wrapped-other-code", fmt.Errorf("request: %w", &common.APIError{Code: -2010, Message: "filter"})},
		{"network", errors.New("connection reset")},
		{"plain-filter-text", errors.New("filter failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupRulesCache(t)
			now := time.Unix(1700000000, 0)
			tr := rulesTrader(func() time.Time { return now }, func(r *http.Request) (*http.Response, error) {
				t.Errorf("unexpected network request: %s", r.URL)
				return nil, errors.New("unexpected request")
			})
			seedFilterRules(t, tr, now, stockRule())
			before := readRulesDiskTest(t)
			tr.noteFilterFailure("AAPLBUSDT", tc.err)
			assertFilterRuleCached(t, tr, "AAPLBUSDT", true)
			if !reflect.DeepEqual(before, readRulesDiskTest(t)) {
				t.Fatal("non -1013 error changed persisted rules")
			}
			// An ignored error must not consume the throttle window.
			tr.noteFilterFailure("AAPLBUSDT", &common.APIError{Code: -1013})
			assertFilterRuleCached(t, tr, "AAPLBUSDT", false)
		})
	}
}

func TestFilterFailureThrottleSharedPerSymbol(t *testing.T) {
	setupRulesCache(t)
	start := time.Unix(1700000000, 0)
	now := start
	calls := make(map[string]int)
	handler := rulesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		symbol := r.URL.Query().Get("symbol")
		calls[symbol]++
		if symbol == "AAPLBUSDT" {
			return rulesResponse(t, stockRule()), nil
		}
		if symbol == "TSLABUSDT" {
			return rulesResponse(t, otherFilterRule()), nil
		}
		return nil, errors.New("unexpected full request")
	})
	tr := rulesTrader(func() time.Time { return now }, handler)
	other := rulesTrader(tr.now, handler)
	seedFilterRules(t, tr, start, stockRule(), otherFilterRule())
	err := fmt.Errorf("wrapped request: %w", &common.APIError{Code: -1013})
	tr.noteFilterFailure("AAPLBUSDT", err)
	assertFilterRuleCached(t, tr, "AAPLBUSDT", false)
	if _, err := tr.rule("AAPLBUSDT"); err != nil {
		t.Fatal(err)
	}
	other.noteFilterFailure("AAPLBUSDT", err)
	assertFilterRuleCached(t, tr, "AAPLBUSDT", true)
	now = start.Add(10*time.Minute - time.Nanosecond)
	other.noteFilterFailure("AAPLBUSDT", err)
	assertFilterRuleCached(t, tr, "AAPLBUSDT", true)
	other.noteFilterFailure("TSLABUSDT", err)
	assertFilterRuleCached(t, tr, "TSLABUSDT", false)
	if _, err := tr.rule("TSLABUSDT"); err != nil {
		t.Fatal(err)
	}
	now = start.Add(10 * time.Minute)
	other.noteFilterFailure("AAPLBUSDT", err)
	assertFilterRuleCached(t, tr, "AAPLBUSDT", false)
	assertFilterRuleCached(t, tr, "TSLABUSDT", true)
	if _, err := tr.rule("AAPLBUSDT"); err != nil || calls["AAPLBUSDT"] != 2 || calls["TSLABUSDT"] != 1 || calls[""] != 0 {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
}

func TestFilterFailureLookupFailureUsesNegativeCache(t *testing.T) {
	for _, kind := range []string{"network", "api-error", "missing"} {
		t.Run(kind, func(t *testing.T) {
			setupRulesCache(t)
			now := time.Unix(1700000000, 0)
			calls := 0
			tr := rulesTrader(func() time.Time { return now }, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/api/v3/exchangeInfo" || r.URL.Query().Get("symbol") != "AAPLBUSDT" {
					t.Errorf("unexpected request: %s", r.URL)
				}
				switch kind {
				case "network":
					return nil, errors.New("offline")
				case "api-error":
					return nil, &common.APIError{Code: -1121, Message: "Invalid symbol"}
				default:
					return rulesResponse(t), nil
				}
			})
			seedFilterRules(t, tr, now, stockRule(), otherFilterRule())
			tr.noteFilterFailure("AAPLBUSDT", &common.APIError{Code: -1013})
			_, first := tr.rule("AAPLBUSDT")
			_, second := tr.rule("AAPLBUSDT")
			if first == nil || first != second || calls != 1 {
				t.Fatalf("first=%v second=%v calls=%d", first, second, calls)
			}
			e := sharedRulesFor(tr.client.BaseURL)
			e.mu.Lock()
			miss, ok := e.negative["AAPLBUSDT"]
			e.mu.Unlock()
			if !ok || miss.err != first || !miss.at.Equal(now) {
				t.Fatalf("negative cache missing: %+v", miss)
			}
			if s, err := tr.rule("TSLABUSDT"); err != nil || s.Symbol != "TSLABUSDT" || calls != 1 {
				t.Fatalf("other symbol affected: rule=%+v err=%v calls=%d", s, err, calls)
			}
		})
	}
}

func TestFilterFailureOrderPaths(t *testing.T) {
	apiErr := &common.APIError{Code: -1013, Message: "Filter failure: LOT_SIZE"}
	for _, tc := range []struct {
		name, path, prefix, op string
		call                   func(*BStockTrader) error
	}{
		{"market-buy", "/api/v3/order", "", "place spot order", func(tr *BStockTrader) error { _, err := tr.BuyMarketNotional("AAPLBUSDT", 20); return err }},
		{"market-buy-quantity", "/api/v3/order", "", "place spot order", func(tr *BStockTrader) error { _, err := tr.OpenLong("AAPLBUSDT", 1, 1); return err }},
		{"limit-buy", "/api/v3/order", "", "place spot order", func(tr *BStockTrader) error { _, err := tr.BuyLimit("AAPLBUSDT", 1, 100); return err }},
		{"sell", "/api/v3/order", "", "place spot order", func(tr *BStockTrader) error { _, err := tr.SellMarket("AAPLBUSDT", 1); return err }},
		{"stop-limit", "/api/v3/order", "protection canceled; replacement stop failed: ", "place spot order", func(tr *BStockTrader) error { _, err := tr.SetProtection("AAPLBUSDT", 1, 90, 89, 0); return err }},
		{"OCO", "/api/v3/orderList/oco", "protection canceled; replacement OCO failed: ", "/api/v3/orderList/oco", func(tr *BStockTrader) error { _, err := tr.SetProtection("AAPLBUSDT", 1, 90, 89, 120); return err }},
		{"take-profit-replacement", "/api/v3/order", "protection canceled; replacement take-profit failed: ", "place take-profit", func(tr *BStockTrader) error { return tr.SetTakeProfit("AAPLBUSDT", "LONG", 1, 120) }},
		{"preflight", "/api/v3/order/test", "", "Preflight", func(tr *BStockTrader) error { return tr.Preflight("AAPLBUSDT") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, f, _ := fixture(t)
			f.symbols = append(f.symbols, otherFilterRule())
			if _, err := tr.symbols(); err != nil {
				t.Fatal(err)
			}
			if tc.name == "take-profit-replacement" {
				f.orders = []*binance.Order{tpOrder(-1)}
			}
			f.handle = func(r requestRecord) (interface{}, int, bool) {
				if r.path == tc.path && r.method == http.MethodPost {
					return map[string]interface{}{"code": apiErr.Code, "msg": apiErr.Message}, http.StatusBadRequest, true
				}
				return nil, 0, false
			}
			err := tc.call(tr)
			want := tc.prefix + wrap(tc.op, apiErr).Error()
			if err == nil || err.Error() != want {
				t.Fatalf("error=%v, want %s", err, want)
			}
			var api *common.APIError
			if !errors.As(err, &api) || api.Code != apiErr.Code || api.Message != apiErr.Message {
				t.Fatalf("error lost APIError: %v", err)
			}
			assertFilterRuleCached(t, tr, "AAPLBUSDT", false)
			assertFilterRuleCached(t, tr, "TSLABUSDT", true)
			if disk := readRulesDiskTest(t); len(disk.Rules) != 1 || disk.Rules["TSLABUSDT"].Symbol != "TSLABUSDT" {
				t.Fatalf("invalidation not persisted: %+v", disk)
			}
			posts := 0
			for _, r := range f.requests {
				if r.method == http.MethodPost {
					posts++
					if r.path != tc.path || r.params.Get("symbol") != "AAPLBUSDT" {
						t.Fatalf("unexpected order: %+v", r)
					}
				}
			}
			if posts != 1 || len(requestsTo(f, "/api/v3/exchangeInfo", "")) != 1 {
				t.Fatalf("rejected order retried or rules refreshed: %+v", f.requests)
			}
		})
	}
}

func TestInvalidateRulesWriteFailureOnlyWarns(t *testing.T) {
	setupRulesCache(t)
	logs := captureRulesWarnings(t)
	now := time.Unix(1700000000, 0)
	tr := rulesTrader(func() time.Time { return now }, func(r *http.Request) (*http.Response, error) {
		return rulesResponse(t, stockRule()), nil
	})
	seedFilterRules(t, tr, now, stockRule())
	RulesCachePath = t.TempDir() // Atomic rename onto a directory must fail.
	tr.noteFilterFailure("AAPLBUSDT", &common.APIError{Code: -1013})
	assertFilterRuleCached(t, tr, "AAPLBUSDT", false)
	if !strings.Contains(logs.String(), "cache write failed") || !strings.Contains(logs.String(), "WARN") {
		t.Fatalf("missing write warning: %s", logs.String())
	}
}

func TestInvalidateConcurrentRules(t *testing.T) {
	for _, throttled := range []bool{true, false} {
		t.Run(fmt.Sprintf("throttled=%v", throttled), func(t *testing.T) {
			setupRulesCache(t)
			now := time.Unix(1700000000, 0)
			var full, single atomic.Int32
			handler := rulesRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("symbol") != "AAPLBUSDT" {
					full.Add(1)
					return nil, errors.New("unexpected full request")
				}
				single.Add(1)
				return rulesResponse(t, stockRule()), nil
			})
			tr := rulesTrader(func() time.Time { return now }, handler)
			other := rulesTrader(tr.now, handler)
			seedFilterRules(t, tr, now, stockRule(), otherFilterRule())
			e := sharedRulesFor(tr.client.BaseURL)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < 24; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					for j := 0; j < 8; j++ {
						if i%3 == 0 {
							if throttled {
								other.noteFilterFailure("AAPLBUSDT", &common.APIError{Code: -1013})
							} else {
								e.invalidate("AAPLBUSDT")
							}
						} else {
							s, err := tr.rule("AAPLBUSDT")
							if err != nil || s.Symbol != "AAPLBUSDT" {
								t.Errorf("rule=%+v err=%v", s, err)
							}
						}
					}
				}(i)
			}
			close(start)
			wg.Wait()
			if _, err := tr.rule("AAPLBUSDT"); err != nil || full.Load() != 0 || single.Load() == 0 || (throttled && single.Load() != 1) {
				t.Fatalf("err=%v full=%d single=%d", err, full.Load(), single.Load())
			}
			assertFilterRuleCached(t, tr, "TSLABUSDT", true)
		})
	}
}
