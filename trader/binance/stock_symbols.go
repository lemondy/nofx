package binance

import (
	"time"

	"nofx/market"
)

// ============================================================================
// Binance tokenized-stock ("bstock") symbol detection — DELEGATED to the
// market package (2026-09-23). The classification is exchangeInfo
// underlyingType EQUITY/PREMARKET/KR_EQUITY/HK_EQUITY/CN_EQUITY (the legacy
// underlyingSubType "Stocks" matcher went dead when Binance renamed the
// field — verified live 2026-09-23: 163 EQUITY, 0 "Stocks" — silently
// disabling the weekend gate that keys on it). market.loadBStockSymbols
// owns the classification and its 24h cache; the executor reads the same
// source so the prompt-side STOCK_WEEKEND block and this gate can never
// disagree.
// ============================================================================

// IsStockSymbol reports whether the symbol is a Binance tokenized stock.
// Unknown/unfetchable → false (fail-open — never block on a classification
// outage).
func (t *FuturesTrader) IsStockSymbol(symbol string) bool {
	return market.IsBStockSymbol(symbol)
}

// IsUSMarketWeekend reports whether `now` falls on a Saturday or Sunday in
// US Eastern time — the underlying stock market's non-trading days. Delegates
// to market.IsUSMarketWeekend so the executor gate and the prompt-side
// STOCK_WEEKEND hard block share one calendar definition.
func IsUSMarketWeekend(now time.Time) bool {
	return market.IsUSMarketWeekend(now)
}
