package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
	"golang.org/x/sync/singleflight"
	"nofx/logger"
	"nofx/market"
)

const (
	futuresRulesTTL                = 24 * time.Hour
	futuresRulesStaleGrace         = 7 * 24 * time.Hour
	futuresRulesRefreshBackoff     = 5 * time.Minute
	futuresRulesInvalidateInterval = 10 * time.Minute
	futuresRulesVersion            = 1
)

// FuturesRulesCachePath is configured before starting traders. Empty disables
// persistence. The file contains public rules only, never account information.
var FuturesRulesCachePath = "data/cache/futures_exchange_info.json"

var sharedFuturesRules = struct {
	sync.Mutex
	entries map[string]*futuresRulesEntry
}{entries: make(map[string]*futuresRulesEntry)}

type futuresRulesEntry struct {
	baseURL       string
	mu            sync.Mutex
	flight        singleflight.Group
	rules         map[string]futures.Symbol
	fetchedAt     time.Time
	diskLoaded    bool
	lastAttempt   time.Time
	lastErr       error
	warned        sync.Map // endpoint/symbol/reason: warn once, including across trader instances
	invalidatedAt map[string]time.Time
}

type futuresRulesDisk struct {
	Version   int                       `json:"version"`
	BaseURL   string                    `json:"base_url"`
	FetchedAt time.Time                 `json:"fetched_at"`
	Rules     map[string]futures.Symbol `json:"rules"`
}

func futuresRulesFor(baseURL string) *futuresRulesEntry {
	sharedFuturesRules.Lock()
	defer sharedFuturesRules.Unlock()
	e := sharedFuturesRules.entries[baseURL]
	if e == nil {
		e = &futuresRulesEntry{baseURL: baseURL, invalidatedAt: make(map[string]time.Time)}
		sharedFuturesRules.entries[baseURL] = e
	}
	return e
}

// SDK v2.8.9 futures ExchangeInfoService has no Symbol option. A missing or
// invalidated symbol therefore uses a full refresh, at most once per five
// minutes per endpoint (including failed/initial requests).
func (e *futuresRulesEntry) refresh(t *FuturesTrader, miss bool) error {
	_, err, _ := e.flight.Do("all", func() (interface{}, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		now := t.now()
		if !e.diskLoaded {
			e.diskLoaded = true
			e.readDisk(now)
		}
		age := now.Sub(e.fetchedAt)
		usable := e.rules != nil && age >= 0 && age < futuresRulesTTL+futuresRulesStaleGrace
		if !miss && e.rules != nil && age >= 0 && age < futuresRulesTTL {
			return nil, nil
		}
		if !e.lastAttempt.IsZero() && now.Sub(e.lastAttempt) < futuresRulesRefreshBackoff {
			if usable {
				return nil, nil
			}
			if e.lastErr != nil {
				return nil, e.lastErr
			}
			return nil, fmt.Errorf("futures exchangeInfo refresh throttled")
		}
		e.lastAttempt = now
		info, fetchErr := t.client.NewExchangeInfoService().Do(context.Background())
		if fetchErr == nil && (info == nil || len(info.Symbols) == 0) {
			fetchErr = fmt.Errorf("empty futures exchangeInfo symbols")
		}
		if fetchErr == nil {
			rules := make(map[string]futures.Symbol, len(info.Symbols))
			for _, s := range info.Symbols {
				if s.Symbol == "" {
					fetchErr = fmt.Errorf("empty symbol in futures exchangeInfo")
					break
				}
				if _, exists := rules[s.Symbol]; exists {
					fetchErr = fmt.Errorf("duplicate futures exchangeInfo symbol %s", s.Symbol)
					break
				}
				rules[s.Symbol] = s
			}
			if fetchErr == nil {
				e.rules, e.fetchedAt, e.lastErr = rules, now, nil
				e.writeDisk()
				return nil, nil
			}
		}
		e.lastErr = fmt.Errorf("futures exchangeInfo: %w", fetchErr)
		if usable {
			logger.Warnf("futures exchangeInfo refresh failed; using rules fetched at %s: %v", e.fetchedAt.Format(time.RFC3339), fetchErr)
			return nil, nil
		}
		return nil, e.lastErr
	})
	return err
}

func (t *FuturesTrader) cachedSymbolRules(symbol string) (futures.Symbol, error) {
	if symbol == "" {
		return futures.Symbol{}, fmt.Errorf("empty futures symbol")
	}
	e := futuresRulesFor(t.client.BaseURL)
	if err := e.refresh(t, false); err != nil {
		return futures.Symbol{}, err
	}
	e.mu.Lock()
	s, ok := e.rules[symbol]
	if ok {
		s = cloneFuturesRule(s)
	}
	e.mu.Unlock()
	if ok {
		return s, nil
	}
	if err := e.refresh(t, true); err != nil {
		return futures.Symbol{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.rules[symbol]; ok {
		return cloneFuturesRule(s), nil
	}
	return futures.Symbol{}, fmt.Errorf("%s trading rules not found (full refresh limited to once per 5 minutes)", symbol)
}

func (t *FuturesTrader) noteFilterFailure(symbol string, err error) {
	var api *common.APIError
	if !errors.As(err, &api) || api.Code != -1013 {
		return
	}
	e := futuresRulesFor(t.client.BaseURL)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := t.now()
	if !e.diskLoaded {
		e.diskLoaded = true
		e.readDisk(now)
	}
	if at, ok := e.invalidatedAt[symbol]; ok && now.Sub(at) < futuresRulesInvalidateInterval {
		return
	}
	e.invalidatedAt[symbol] = now
	delete(e.rules, symbol)
	e.writeDisk()
	logger.Warnf("futures rules cache invalidated for %s after filter failure: %v", symbol, err)
}

// Caller holds mu. Stale disk rules are usable only after a failed refresh,
// within the same seven-day grace as the in-memory snapshot.
func (e *futuresRulesEntry) readDisk(now time.Time) {
	if FuturesRulesCachePath == "" {
		return
	}
	data, err := os.ReadFile(FuturesRulesCachePath)
	if err != nil {
		return
	}
	var disk futuresRulesDisk
	if json.Unmarshal(data, &disk) != nil || disk.Version != futuresRulesVersion || disk.BaseURL != e.baseURL || disk.FetchedAt.IsZero() || disk.FetchedAt.After(now) || now.Sub(disk.FetchedAt) >= futuresRulesTTL+futuresRulesStaleGrace || disk.Rules == nil {
		return
	}
	for key, s := range disk.Rules {
		if key == "" || key != s.Symbol {
			return
		}
	}
	e.rules, e.fetchedAt = disk.Rules, disk.FetchedAt
}

func (e *futuresRulesEntry) writeDisk() {
	if FuturesRulesCachePath == "" || e.rules == nil || e.fetchedAt.IsZero() {
		return
	}
	if err := writeFuturesRulesFile(FuturesRulesCachePath, futuresRulesDisk{futuresRulesVersion, e.baseURL, e.fetchedAt, e.rules}); err != nil {
		logger.Warnf("futures rules cache write failed (%s): %v", FuturesRulesCachePath, err)
	}
}

func writeFuturesRulesFile(path string, disk futuresRulesDisk) error {
	data, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".futures-rules-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func cloneFuturesRule(s futures.Symbol) futures.Symbol {
	s.UnderlyingSubType = slices.Clone(s.UnderlyingSubType)
	s.OrderType = slices.Clone(s.OrderType)
	s.TimeInForce = slices.Clone(s.TimeInForce)
	if s.Filters != nil {
		filters := make([]map[string]interface{}, len(s.Filters))
		for i, f := range s.Filters {
			filters[i], _ = cloneFuturesRuleJSON(f).(map[string]interface{})
		}
		s.Filters = filters
	}
	return s
}

func cloneFuturesRuleJSON(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		if v == nil {
			return v
		}
		out := make(map[string]interface{}, len(v))
		for key, value := range v {
			out[key] = cloneFuturesRuleJSON(value)
		}
		return out
	case []interface{}:
		if v == nil {
			return v
		}
		out := make([]interface{}, len(v))
		for i, value := range v {
			out[i] = cloneFuturesRuleJSON(value)
		}
		return out
	default:
		return value
	}
}

const fallbackFuturesMinNotional = market.FallbackMinNotional

func (t *FuturesTrader) warnRulesOnce(symbol, reason, message string, args ...interface{}) {
	e := futuresRulesFor(t.client.BaseURL)
	if _, loaded := e.warned.LoadOrStore(symbol+":"+reason, true); !loaded {
		logger.Warnf("%s trading rules: "+message, append([]interface{}{symbol}, args...)...)
	}
}

func (t *FuturesTrader) fallbackMinNotional(symbol string, err error) float64 {
	t.warnRulesOnce(symbol, "min-notional", "MIN_NOTIONAL unavailable; using legacy fallback 10: %v", err)
	return fallbackFuturesMinNotional
}

func (t *FuturesTrader) symbolMinNotional(s futures.Symbol) float64 {
	for _, f := range s.Filters {
		if f["filterType"] != "MIN_NOTIONAL" && f["filterType"] != "NOTIONAL" {
			continue
		}
		text, _ := f["notional"].(string)
		if text == "" {
			text, _ = f["minNotional"].(string)
		}
		value, err := strconv.ParseFloat(text, 64)
		if err == nil && positiveFinite(value) {
			return value
		}
	}
	return t.fallbackMinNotional(s.Symbol, fmt.Errorf("missing or invalid MIN_NOTIONAL filter"))
}
