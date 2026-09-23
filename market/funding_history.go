package market

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Funding settlement HISTORY (distinct from the forward estimate).
//
// premiumIndex.lastFundingRate (see data.go getFundingRate) is the CONTINUOUS
// forward estimate for the NEXT settlement. The rollover condition of the
// short-crowding rule ("费率刚从高位回落") is about the sequence of SETTLED
// rates vs that live forward value — so the signal layer needs the real
// settlement history, not just the drifting estimate (user review 2026-09-15
// point 6: the prompt demanded funding_rollover but no structured-signal
// field could verify it, leaving only the scanner's snapshot pattern text —
// which the prompt itself demotes to "辅助证据").
// ============================================================================

// fundingHistoryCache is one symbol's settled funding rates, oldest→newest.
type fundingHistoryCache struct {
	Rates     []float64
	Times     []int64 // fundingTime ms, parallel to Rates
	UpdatedAt time.Time
}

var (
	fundingHistoryMap sync.Map // map[string]*fundingHistoryCache
	frHistoryTTL      = 5 * time.Minute
)

// fundingHistory returns the symbol's recent SETTLED funding rates (oldest →
// newest). Cached for 5 minutes — same freshness contract as the live-rate
// cache, so the rollover verdict and the displayed forward rate are never
// more than a cache-TTL apart. ok=false on fetch failure: the caller must
// treat the rollover condition as UNKNOWN, never as "not rolled over".
func fundingHistory(symbol string) (rates []float64, times []int64, ok bool) {
	if cached, hit := fundingHistoryMap.Load(symbol); hit {
		c := cached.(*fundingHistoryCache)
		if time.Since(c.UpdatedAt) < frHistoryTTL {
			return c.Rates, c.Times, true
		}
	}
	var raw []struct {
		FundingRate string `json:"fundingRate"`
		FundingTime int64  `json:"fundingTime"`
	}
	if err := binanceGetJSON("/fapi/v1/fundingRate?symbol="+symbol+"&limit=6", &raw); err != nil || len(raw) == 0 {
		return nil, nil, false
	}
	rates = make([]float64, 0, len(raw))
	times = make([]int64, 0, len(raw))
	for _, p := range raw {
		v, err := strconv.ParseFloat(p.FundingRate, 64)
		if err != nil {
			continue
		}
		rates = append(rates, v)
		times = append(times, p.FundingTime)
	}
	if len(rates) == 0 {
		return nil, nil, false
	}
	fundingHistoryMap.Store(symbol, &fundingHistoryCache{Rates: rates, Times: times, UpdatedAt: time.Now()})
	return rates, times, true
}

// FundingRolloverDetected is the single program definition of "funding rate
// has rolled over from a high" (user review 2026-09-15 point 6): the rate
// SETTLED 3 intervals ago was above the crowding-relevant threshold and the
// LIVE forward rate is now below it — crowded longs starting to unwind.
// prev = the historical settled reference, cur = live forward estimate,
// periodThreshold = raw decimal per settlement interval (already scaled to
// the symbol's real interval by the caller). The short scanner and the
// structured signal must both route through this function so the scanner's
// "资金费率回落" pattern and derivatives.funding_rollover can never disagree
// by definition.
func FundingRolloverDetected(prev, cur, periodThreshold float64) bool {
	return prev > periodThreshold && cur < prev
}

// IsUSMarketWeekend reports whether `now` falls on a Saturday or Sunday in US
// Eastern time — the non-trading days of the tokenized-stock underlyings.
// (The trader's gate delegates here so prompt-side and execution-side share
// one calendar definition.)
func IsUSMarketWeekend(now time.Time) bool {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		// IANA data unavailable: fall back to a fixed UTC-5 estimate (no DST
		// handling — weekend boundaries are day-granular, a DST-hour is
		// irrelevant).
		loc = time.FixedZone("EST", -5*3600)
	}
	et := now.In(loc)
	return et.Weekday() == time.Saturday || et.Weekday() == time.Sunday
}

// ============================================================================
// Binance tokenized-stock ("bstock") classification for PROMPT-side use.
// Authoritative source is exchangeInfo underlyingSubType containing "Stocks"
// — the same rule the trader/binance executor gate applies. Cached 24h; a
// fetch failure reports false (fail-open: the executor still rejects the
// order, the prompt just doesn't pre-block the direction).
// ============================================================================

var (
	bstockSymbols    map[string]bool
	usEquitySymbols  map[string]bool // EQUITY/PREMARKET underlyingType only — US-session weighting set
	binanceListed    map[string]bool // EVERY symbol Binance futures lists — the xyz-routing check
	bstockMu         sync.RWMutex
	bstockLoaded     time.Time
	bstockLoadError  time.Time
)

const (
	bstockTTL        = 24 * time.Hour
	bstockRetryEvery = 1 * time.Hour
)

func loadBStockSymbols() map[string]bool {
	bstockMu.RLock()
	set := bstockSymbols
	fresh := set != nil && time.Since(bstockLoaded) < bstockTTL
	lastErr := !bstockLoadError.IsZero() && time.Since(bstockLoadError) < bstockRetryEvery
	bstockMu.RUnlock()
	if fresh || lastErr {
		return set
	}
	var info struct {
		Symbols []struct {
			Symbol            string   `json:"symbol"`
			UnderlyingType    string   `json:"underlyingType"`
			UnderlyingSubType []string `json:"underlyingSubType"`
		} `json:"symbols"`
	}
	if err := binanceGetJSON("/fapi/v1/exchangeInfo", &info); err != nil || len(info.Symbols) == 0 {
		bstockMu.Lock()
		bstockLoadError = time.Now()
		bstockMu.Unlock()
		return set // possibly nil — caller fails open
	}
	next := map[string]bool{}
	usNext := map[string]bool{}
	allNext := map[string]bool{}
	for _, s := range info.Symbols {
		if isStockClassification(s.UnderlyingType, s.UnderlyingSubType) {
			next[s.Symbol] = true
		}
		if s.UnderlyingType == "EQUITY" || s.UnderlyingType == "PREMARKET" {
			usNext[s.Symbol] = true
		}
		allNext[s.Symbol] = true
	}
	bstockMu.Lock()
	bstockSymbols = next
	usEquitySymbols = usNext
	binanceListed = allNext
	bstockLoaded = time.Now()
	bstockLoadError = time.Time{}
	bstockMu.Unlock()
	return next
}

// binanceListsNative reports whether BASE (no USDT suffix) trades as a
// native Binance futures perp. Nil cache (classification never loaded /
// fetch failed) → false: callers fall back to their legacy routing.
func binanceListsNative(base string) bool {
	bstockMu.RLock()
	defer bstockMu.RUnlock()
	if binanceListed == nil {
		return false
	}
	return binanceListed[base+"USDT"]
}

// isStockClassification is the single classification predicate (2026-09-23
// fix): Binance RENAMED the classification — underlyingSubType "Stocks" is
// gone, equity tokens now carry underlyingType EQUITY/PREMARKET/KR_EQUITY/
// HK_EQUITY/CN_EQUITY (verified live 2026-09-23: 703 COIN / 163 EQUITY /
// 8 COMMODITY / 0 "Stocks"). The old subType-only matcher matched NOTHING,
// silently disabling every stock gate that keys on it (weekend no-open,
// STOCK_WEEKEND). Accept the new types plus the legacy subType for safety.
func isStockClassification(underlyingType string, subTypes []string) bool {
	switch underlyingType {
	case "EQUITY", "PREMARKET", "KR_EQUITY", "HK_EQUITY", "CN_EQUITY":
		return true
	}
	for _, st := range subTypes {
		if strings.EqualFold(st, "Stocks") || strings.Contains(strings.ToLower(st), "stock") {
			return true
		}
	}
	return false
}

// IsUSEquitySymbol reports whether the symbol is a token of a US-listed
// equity (or its pre-market token) — the set the US-session weighting
// applies to. KR/HK/CN equity tokens follow their home calendars, not the
// US session, and are deliberately excluded here.
func IsUSEquitySymbol(symbol string) bool {
	set := loadBStockSymbols()
	if set == nil {
		return false
	}
	return set[strings.ToUpper(symbol)] && usEquitySymbols[strings.ToUpper(symbol)]
}

// IsUSMarketOpen reports whether `now` falls inside US regular trading
// hours (Eastern, Mon–Fri 09:30–16:00) — the session the equity tokens
// track their underlying most tightly and where the session weighting
// applies.
func IsUSMarketOpen(now time.Time) bool {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.FixedZone("EST", -5*3600)
	}
	et := now.In(loc)
	if et.Weekday() == time.Saturday || et.Weekday() == time.Sunday {
		return false
	}
	mins := et.Hour()*60 + et.Minute()
	return mins >= 9*60+30 && mins < 16*60
}

// IsBStockSymbol reports whether the futures symbol is a Binance tokenized
// stock (DELLUSDT/SKHYUSDT/…). Unknown/unfetchable → false (fail-open, same
// contract as the executor gate).
func IsBStockSymbol(symbol string) bool {
	set := loadBStockSymbols()
	if set == nil {
		return false
	}
	return set[strings.ToUpper(symbol)]
}
