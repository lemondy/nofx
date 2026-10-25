package binance_bstock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	binance "github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/common"
	"golang.org/x/sync/singleflight"
	"nofx/logger"
)

const (
	spotRulesTTL                = 24 * time.Hour
	spotRulesStaleGrace         = 7 * 24 * time.Hour
	spotRulesNegativeTTL        = 10 * time.Minute
	spotRulesInvalidateInterval = 10 * time.Minute
	// After a failed full refresh keep serving the stale table without touching
	// the network again for this long: the refresh holds the entry lock for up to
	// the HTTP timeout, so retrying on every rule() call would stall order paths.
	spotRulesRefreshBackoff = 5 * time.Minute
	spotRulesVersion        = 1
)

// RulesCachePath can be configured before starting traders. Empty disables disk
// caching. Only the filtered, public exchange rules are persisted here.
var RulesCachePath = "data/cache/bstock_spot_rules.json"

var sharedSpotRules = struct {
	sync.Mutex
	entries map[string]*spotRulesEntry
}{entries: make(map[string]*spotRulesEntry)}

type negativeRule struct {
	at  time.Time
	err error
}

type spotRulesEntry struct {
	baseURL    string
	mu         sync.Mutex
	flight     singleflight.Group
	rules      map[string]binance.Symbol
	fetchedAt  time.Time
	diskLoaded bool
	negative   map[string]negativeRule
	// lastRefreshFail is the time of the last failed full refresh served from
	// stale rules; zero after any successful refresh.
	lastRefreshFail time.Time
	// Separate from mu so the throttle check and invalidate can be serialized
	// together while invalidate takes the rules/disk snapshot lock.
	filterMu      sync.Mutex
	invalidatedAt map[string]time.Time
}

type spotRulesDisk struct {
	Version   int                       `json:"version"`
	BaseURL   string                    `json:"base_url"`
	FetchedAt time.Time                 `json:"fetched_at"`
	Rules     map[string]binance.Symbol `json:"rules"`
}

func sharedRulesFor(baseURL string) *spotRulesEntry {
	sharedSpotRules.Lock()
	defer sharedSpotRules.Unlock()
	entry := sharedSpotRules.entries[baseURL]
	if entry == nil {
		entry = &spotRulesEntry{baseURL: baseURL, negative: make(map[string]negativeRule), invalidatedAt: make(map[string]time.Time)}
		sharedSpotRules.entries[baseURL] = entry
	}
	return entry
}

// invalidate leaves the table age intact: the next miss uses lookup's
// single-symbol request, without expiring the full table.
func (e *spotRulesEntry) invalidate(symbol string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.rules, symbol)
	delete(e.negative, symbol)
	e.writeDisk(e.baseURL)
}

func (t *BStockTrader) noteFilterFailure(symbol string, err error) {
	var api *common.APIError
	if !errors.As(err, &api) || api.Code != -1013 {
		return
	}
	e := sharedRulesFor(t.client.BaseURL)
	e.filterMu.Lock()
	defer e.filterMu.Unlock()
	now := t.now()
	if at, ok := e.invalidatedAt[symbol]; ok && now.Sub(at) < spotRulesInvalidateInterval {
		return
	}
	e.invalidatedAt[symbol] = now
	e.invalidate(symbol)
	logger.Warnf("bStock rules cache invalidated for %s after filter failure: %v", symbol, err)
}

func isBStockRule(s binance.Symbol) bool {
	return s.Status == "TRADING" && s.QuoteAsset == "USDT" && strings.HasSuffix(s.BaseAsset, "B") && s.Symbol == s.BaseAsset+"USDT"
}

func (e *spotRulesEntry) symbols(t *BStockTrader) (map[string]binance.Symbol, error) {
	_, err, _ := e.flight.Do("all", func() (interface{}, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if !e.diskLoaded {
			e.diskLoaded = true
			e.readDisk(t.client.BaseURL, t.now())
		}
		if e.rules != nil && t.now().Sub(e.fetchedAt) < spotRulesTTL {
			return nil, nil
		}
		if e.rules != nil && !e.lastRefreshFail.IsZero() && t.now().Sub(e.lastRefreshFail) < spotRulesRefreshBackoff && t.now().Sub(e.fetchedAt) < spotRulesTTL+spotRulesStaleGrace {
			return nil, nil // recent failed refresh: keep serving stale rules, retry after the backoff
		}
		info, err := t.client.NewExchangeInfoService().Do(context.Background())
		if err != nil {
			if e.rules != nil && t.now().Sub(e.fetchedAt) < spotRulesTTL+spotRulesStaleGrace {
				e.lastRefreshFail = t.now()
				logger.Warnf("bStock exchangeInfo refresh failed; using rules fetched at %s: %v", e.fetchedAt.Format(time.RFC3339), err)
				return nil, nil
			}
			return nil, wrap("exchangeInfo", err)
		}
		e.lastRefreshFail = time.Time{}
		rules := make(map[string]binance.Symbol)
		for _, s := range info.Symbols {
			if isBStockRule(s) {
				rules[s.Symbol] = s
			}
		}
		e.rules, e.fetchedAt = rules, t.now()
		e.writeDisk(t.client.BaseURL)
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rules := make(map[string]binance.Symbol, len(e.rules))
	for symbol, s := range e.rules {
		rules[symbol] = cloneRule(s)
	}
	return rules, nil
}

// A miss never triggers another full exchangeInfo request. Both successful
// additions and negative lookups are shared by all traders on this endpoint.
func (e *spotRulesEntry) lookup(t *BStockTrader, symbol string) (binance.Symbol, error) {
	if symbol == "" { // The SDK omits the symbol parameter for an empty string.
		return binance.Symbol{}, fmt.Errorf("symbol not tradable on spot bStock: %s", symbol)
	}
	result, err, _ := e.flight.Do("symbol:"+symbol, func() (interface{}, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if s, ok := e.rules[symbol]; ok {
			return cloneRule(s), nil
		}
		if miss, ok := e.negative[symbol]; ok && t.now().Sub(miss.at) < spotRulesNegativeTTL {
			return nil, miss.err
		}
		info, err := t.client.NewExchangeInfoService().Symbol(symbol).Do(context.Background())
		if err == nil {
			for _, s := range info.Symbols {
				if s.Symbol == symbol && isBStockRule(s) {
					if e.rules == nil {
						e.rules = make(map[string]binance.Symbol)
					}
					e.rules[symbol] = s
					delete(e.negative, symbol)
					// Do not extend the age of the full table on a single-symbol fill.
					e.writeDisk(t.client.BaseURL)
					return cloneRule(s), nil
				}
			}
			err = fmt.Errorf("symbol not tradable on spot bStock: %s", symbol)
		} else {
			err = wrap("exchangeInfo "+symbol, err)
		}
		e.negative[symbol] = negativeRule{at: t.now(), err: err}
		return nil, err
	})
	if err != nil {
		return binance.Symbol{}, err
	}
	// singleflight shares its result with waiters; give each caller a separate
	// copy of that immutable snapshot, even if a full refresh has since run.
	return cloneRule(result.(binance.Symbol)), nil
}

// readDisk is called once per endpoint, with mu held. Expired rules may only be
// used after a failed refresh and within the same seven-day grace as memory.
func (e *spotRulesEntry) readDisk(baseURL string, now time.Time) {
	if RulesCachePath == "" {
		return
	}
	data, err := os.ReadFile(RulesCachePath)
	if err != nil {
		return
	}
	var disk spotRulesDisk
	if json.Unmarshal(data, &disk) != nil || disk.Version != spotRulesVersion || disk.BaseURL != baseURL || disk.FetchedAt.IsZero() || disk.FetchedAt.After(now) || disk.Rules == nil {
		return
	}
	for symbol, s := range disk.Rules {
		if symbol != s.Symbol || !isBStockRule(s) {
			return
		}
	}
	e.rules, e.fetchedAt = disk.Rules, disk.FetchedAt
}

// Caller holds mu, serializing disk snapshots with refreshes and additions.
func (e *spotRulesEntry) writeDisk(baseURL string) {
	if RulesCachePath == "" {
		return
	}
	if err := writeRulesFile(RulesCachePath, spotRulesDisk{spotRulesVersion, baseURL, e.fetchedAt, e.rules}); err != nil {
		logger.Warnf("bStock rules cache write failed (%s): %v", RulesCachePath, err)
	}
}

func writeRulesFile(path string, disk spotRulesDisk) error {
	data, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".bstock-rules-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func cloneRule(s binance.Symbol) binance.Symbol {
	s.OrderTypes = slices.Clone(s.OrderTypes)
	s.Permissions = slices.Clone(s.Permissions)
	if s.PermissionSets != nil {
		sets := make([][]string, len(s.PermissionSets))
		for i, set := range s.PermissionSets {
			sets[i] = slices.Clone(set)
		}
		s.PermissionSets = sets
	}
	if s.Filters != nil {
		filters := make([]map[string]interface{}, len(s.Filters))
		for i, f := range s.Filters {
			filters[i], _ = cloneRuleJSON(f).(map[string]interface{})
		}
		s.Filters = filters
	}
	return s
}

func cloneRuleJSON(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		if v == nil {
			return v
		}
		out := make(map[string]interface{}, len(v))
		for key, value := range v {
			out[key] = cloneRuleJSON(value)
		}
		return out
	case []interface{}:
		if v == nil {
			return v
		}
		out := make([]interface{}, len(v))
		for i, value := range v {
			out[i] = cloneRuleJSON(value)
		}
		return out
	default:
		return value
	}
}
