package market

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

// ============================================================================
// Per-symbol exchange lot constraints (user report 09-29: QNTUSDT limit entry
// died at placement — "MIN_QTY: quantity 0.05345287 rounds down to 0.0
// (stepSize=0.1, minQty=0.1)"). Risk-based sizing produced a 13.9U notional
// while QNT's lot structure forces ≥0.1 base units (≈26U) — the kernel's
// min-size dead-zone check only knew the STRATEGY's min_position_size (5U),
// so the model was shown an "allowed" setup that can never execute. These
// filters feed the kernel's MinSizeCheck so the gate kills the setup BEFORE
// the model plans it.
// ============================================================================

// SymbolLotFilter is the exchange's tradability floor for one symbol.
// Zero fields = unknown (absent or fetch failed) — callers fail-open.
type SymbolLotFilter struct {
	StepSize    float64 // quantity granularity
	MinQty      float64 // minimum order quantity
	MinNotional float64 // minimum order value in USDT
}

type lotFilterCache struct {
	filter    SymbolLotFilter
	fetchedAt time.Time
}

var (
	lotFilterMu        sync.Mutex
	lotFilterCacheV    map[string]lotFilterCache
	lotFilterFetchedAt time.Time
	lotFilterFailedAt  time.Time
	lotFilterInflight  chan struct{}
	// lotFilterFetch is the offline test seam for a complete exchange snapshot.
	lotFilterFetch = func() (map[string]SymbolLotFilter, error) {
		var payload struct {
			Symbols []struct {
				Symbol  string                   `json:"symbol"`
				Status  string                   `json:"status"`
				Filters []map[string]interface{} `json:"filters"`
			} `json:"symbols"`
		}
		if err := binanceGetJSON("/fapi/v1/exchangeInfo", &payload); err != nil {
			return nil, err
		}
		// Binance error bodies can decode successfully but contain no symbols.
		if len(payload.Symbols) == 0 {
			return nil, fmt.Errorf("empty futures exchangeInfo symbols")
		}
		filters := make(map[string]SymbolLotFilter, len(payload.Symbols))
		for _, s := range payload.Symbols {
			if s.Status == "TRADING" && s.Symbol != "" {
				filters[s.Symbol] = ParseSymbolLotFilter(s.Filters)
			}
		}
		return filters, nil
	}
)

// lotFilterTTL: exchangeInfo changes ~never intraday; 6h keeps it fresh
// across listing updates without hammering the endpoint.
const lotFilterTTL = 6 * time.Hour

const lotFilterFailureBackoff = time.Minute

// FallbackMinNotional is shared with the Binance executor when a known
// symbol has no valid MIN_NOTIONAL/NOTIONAL filter.
const FallbackMinNotional = 10.0

// ParseSymbolLotFilter parses the tradability floor without fetching data.
// Kernel planning does not yet know whether entry will use MARKET or LIMIT.
// Take the larger minQty and coarser step from MARKET_LOT_SIZE and LOT_SIZE
// so the floor shown to the model covers both execution paths.
func ParseSymbolLotFilter(filters []map[string]interface{}) SymbolLotFilter {
	out := SymbolLotFilter{MinNotional: FallbackMinNotional}
	notionalFound := false
	for _, f := range filters {
		switch f["filterType"] {
		case "LOT_SIZE", "MARKET_LOT_SIZE":
			out.StepSize = math.Max(out.StepSize, parseF(f["stepSize"]))
			out.MinQty = math.Max(out.MinQty, parseF(f["minQty"]))
		case "MIN_NOTIONAL", "NOTIONAL":
			v := f["notional"]
			if v == nil || v == "" {
				v = f["minNotional"]
			}
			if n := parseF(v); !notionalFound && n > 0 {
				out.MinNotional = n
				notionalFound = true
			}
		}
	}
	return out
}

// GetSymbolLotFilter uses one six-hour snapshot for all trading symbols.
// Failed refreshes retain the old table and back off for one minute. Missing
// symbols return zero (unknown) without inventing an exchange floor.
func GetSymbolLotFilter(symbol string) SymbolLotFilter {
	for {
		lotFilterMu.Lock()
		now := time.Now()
		c, ok := lotFilterCacheV[symbol]
		if (ok && now.Sub(c.fetchedAt) < lotFilterTTL) ||
			(!lotFilterFetchedAt.IsZero() && now.Sub(lotFilterFetchedAt) < lotFilterTTL) ||
			(!lotFilterFailedAt.IsZero() && now.Sub(lotFilterFailedAt) < lotFilterFailureBackoff) {
			lotFilterMu.Unlock()
			return c.filter
		}
		if done := lotFilterInflight; done != nil {
			lotFilterMu.Unlock()
			<-done
			continue
		}
		done := make(chan struct{})
		lotFilterInflight = done
		fetch := lotFilterFetch
		lotFilterMu.Unlock()

		filters, err := fetch()

		lotFilterMu.Lock()
		// Reset invalidates an outstanding refresh as well as its snapshot.
		if lotFilterInflight == done {
			if err != nil {
				lotFilterFailedAt = time.Now()
			} else {
				fetchedAt := time.Now()
				table := make(map[string]lotFilterCache, len(filters))
				for sym, f := range filters {
					table[sym] = lotFilterCache{filter: f, fetchedAt: fetchedAt}
				}
				// Preserve unexpired test pins, including those installed
				// while the request was in flight.
				for sym, pinned := range lotFilterCacheV {
					if fetchedAt.Sub(pinned.fetchedAt) < lotFilterTTL {
						table[sym] = pinned
					}
				}
				lotFilterCacheV = table
				lotFilterFetchedAt = fetchedAt
				lotFilterFailedAt = time.Time{}
			}
			lotFilterInflight = nil
		}
		out := lotFilterCacheV[symbol].filter
		close(done)
		lotFilterMu.Unlock()
		return out
	}
}

// MinTradableNotional is the smallest notional this symbol can actually
// trade: max(MIN_NOTIONAL, minQty×price) with the implied quantity rounded
// UP to the step grid — including the quantity MIN_NOTIONAL implies
// (review 2026-10-28 A5: step 0.1, notional 5, price 26 quotes qty 0.19,
// which orders round DOWN to 0.1 = 2.6U and get rejected; the placeable
// floor is 0.2 → 5.2U). For QNTUSDT (step 0.1, minQty 0.1, price 260) that
// is 26U — the floor the strategy-level check was missing.
func (f SymbolLotFilter) MinTradableNotional(price float64) float64 {
	if price <= 0 {
		return f.MinNotional
	}
	minNotional := f.MinNotional
	if f.MinQty > 0 {
		minNotional = math.Max(minNotional, f.MinQty*price)
	}
	// Align up to the step grid so the quoted floor is actually placeable.
	if f.StepSize > 0 {
		q := math.Max(f.MinQty, f.MinNotional/price)
		if q > 0 {
			steps := math.Ceil(q/f.StepSize - 1e-9)
			aligned := steps * f.StepSize
			// Float error can leave steps×step×price a hair under the
			// exchange floor (fine-grained 0.001 grids); buy one more
			// step so the quoted floor never dips below MIN_NOTIONAL.
			if aligned*price < f.MinNotional {
				aligned += f.StepSize
			}
			minNotional = math.Max(minNotional, aligned*price)
		}
	}
	return minNotional
}

func parseF(v interface{}) float64 {
	switch v.(type) {
	case string, json.Number, float64, float32,
		int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
		if err == nil && f > 0 && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	}
	return 0
}

// SetSymbolLotFilterForTesting pins a lot filter for a symbol in the cache
// (offline test hook, same pattern as SetEquityClassificationForTesting).
func SetSymbolLotFilterForTesting(symbol string, f SymbolLotFilter) {
	lotFilterMu.Lock()
	defer lotFilterMu.Unlock()
	if lotFilterCacheV == nil {
		lotFilterCacheV = map[string]lotFilterCache{}
	}
	lotFilterCacheV[symbol] = lotFilterCache{filter: f, fetchedAt: time.Now()}
}

// ResetSymbolLotFilters clears the whole lot-filter cache (test isolation).
func ResetSymbolLotFilters() {
	lotFilterMu.Lock()
	defer lotFilterMu.Unlock()
	lotFilterCacheV = map[string]lotFilterCache{}
	lotFilterFetchedAt = time.Time{}
	lotFilterFailedAt = time.Time{}
	lotFilterInflight = nil
}
