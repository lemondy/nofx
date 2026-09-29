package market

import (
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
// Zero fields = unknown (fetch failed) — callers fail-open.
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
	lotFilterMu     sync.Mutex
	lotFilterCacheV map[string]lotFilterCache
)

// lotFilterTTL: exchangeInfo changes ~never intraday; 6h keeps it fresh
// across listing updates without hammering the endpoint.
const lotFilterTTL = 6 * time.Hour

// DefaultMinNotional is the house fallback when the filter is absent —
// Binance USDT-M has carried 5U (VIP-adjustable) for years; the executor
// defaults its own check to 10.
const DefaultMinNotional = 5.0

// GetSymbolLotFilter returns the LOT_SIZE/MIN_NOTIONAL floor for symbol.
// Cached 6h per symbol; fetch failure returns a zero filter (unknown) and
// is retried next call after the TTL — never cached as authoritative.
func GetSymbolLotFilter(symbol string) SymbolLotFilter {
	lotFilterMu.Lock()
	defer lotFilterMu.Unlock()
	if lotFilterCacheV == nil {
		lotFilterCacheV = map[string]lotFilterCache{}
	}
	if c, ok := lotFilterCacheV[symbol]; ok && time.Since(c.fetchedAt) < lotFilterTTL {
		return c.filter
	}
	var payload struct {
		Symbols []struct {
			Symbol  string                   `json:"symbol"`
			Status  string                   `json:"status"`
			Filters []map[string]interface{} `json:"filters"`
		} `json:"symbols"`
	}
	if err := binanceGetJSON("/fapi/v1/exchangeInfo", &payload); err != nil {
		return SymbolLotFilter{} // unknown — fail-open
	}
	out := SymbolLotFilter{MinNotional: DefaultMinNotional}
	for _, s := range payload.Symbols {
		if s.Symbol != symbol {
			continue
		}
		for _, f := range s.Filters {
			ft, _ := f["filterType"].(string)
			switch ft {
			case "LOT_SIZE":
				out.StepSize = parseF(f["stepSize"])
				out.MinQty = parseF(f["minQty"])
			case "MIN_NOTIONAL", "NOTIONAL":
				if v := parseF(f["notional"]); v > 0 {
					out.MinNotional = v
				}
			}
		}
		break
	}
	lotFilterCacheV[symbol] = lotFilterCache{filter: out, fetchedAt: time.Now()}
	return out
}

// MinTradableNotional is the smallest notional this symbol can actually
// trade: max(MIN_NOTIONAL, minQty×price rounded UP to the step grid).
// For QNTUSDT (step 0.1, minQty 0.1, price 260) that is 26U — the floor the
// strategy-level check was missing.
func (f SymbolLotFilter) MinTradableNotional(price float64) float64 {
	if price <= 0 {
		return f.MinNotional
	}
	minNotional := f.MinNotional
	if f.MinQty > 0 {
		minNotional = math.Max(minNotional, f.MinQty*price)
	}
	// Align up to the step grid so the quoted floor is actually placeable.
	if f.StepSize > 0 && f.MinQty > 0 {
		steps := math.Ceil(f.MinQty/f.StepSize - 1e-9)
		minNotional = math.Max(minNotional, steps*f.StepSize*price)
	}
	return minNotional
}

func parseF(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err == nil {
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
}
