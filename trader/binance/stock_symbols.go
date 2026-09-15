package binance

import (
	"context"
	"nofx/market"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Binance tokenized-stock ("bstock") symbol detection (user directive
// 2026-09-11: stocks on Binance trade on weekends but follow the US market —
// weekend volatility and profit probability are poor, so stock-class symbols
// must not take new positions on weekends until 24h stock trading exists).
//
// The authoritative classification is exchangeInfo underlyingSubType
// containing "Stocks" (DELLUSDT/SKHYUSDT/SKHYNIXUSDT/SPCXUSDT/…), fetched
// once and cached for a day — no hand-maintained ticker list.
// ============================================================================

var (
	stockSymbolsMu     sync.RWMutex
	stockSymbols       map[string]bool
	stockSymbolsLoaded time.Time
)

const stockSymbolsTTL = 24 * time.Hour

// loadStockSymbols refreshes the cached stock-symbol set from exchangeInfo.
// Errors are returned to the caller (the gate fails open — never block on a
// classification outage).
func loadStockSymbols(t *FuturesTrader) (map[string]bool, error) {
	stockSymbolsMu.RLock()
	if stockSymbols != nil && time.Since(stockSymbolsLoaded) < stockSymbolsTTL {
		cached := stockSymbols
		stockSymbolsMu.RUnlock()
		return cached, nil
	}
	stockSymbolsMu.RUnlock()

	info, err := t.client.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, s := range info.Symbols {
		for _, st := range s.UnderlyingSubType {
			if strings.EqualFold(st, "Stocks") {
				set[s.Symbol] = true
				break
			}
		}
	}
	stockSymbolsMu.Lock()
	stockSymbols = set
	stockSymbolsLoaded = time.Now()
	stockSymbolsMu.Unlock()
	return set, nil
}

// IsStockSymbol reports whether the symbol is a Binance tokenized stock
// (underlyingSubType "Stocks"). Unknown/unfetchable → false (fail-open).
func (t *FuturesTrader) IsStockSymbol(symbol string) bool {
	set, err := loadStockSymbols(t)
	if err != nil {
		return false
	}
	return set[strings.ToUpper(symbol)]
}

// IsUSMarketWeekend reports whether `now` falls on a Saturday or Sunday in
// US Eastern time — the underlying stock market's non-trading days. Delegates
// to market.IsUSMarketWeekend so the executor gate and the prompt-side
// STOCK_WEEKEND hard block share one calendar definition.
func IsUSMarketWeekend(now time.Time) bool {
	return market.IsUSMarketWeekend(now)
}
