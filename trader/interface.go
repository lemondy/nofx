package trader

import (
	"fmt"
	"nofx/logger"
	"nofx/trader/types"
)

// Re-export types for backward compatibility
type (
	ClosedPnLRecord   = types.ClosedPnLRecord
	TradeRecord       = types.TradeRecord
	Trader            = types.Trader
	OpenOrder         = types.OpenOrder
	LimitOrderRequest = types.LimitOrderRequest
	LimitOrderResult  = types.LimitOrderResult
	GridTrader        = types.GridTrader
)

// GridTraderAdapter wraps a basic Trader to provide GridTrader interface
// Uses stop orders as a fallback when limit orders aren't directly available
type GridTraderAdapter struct {
	Trader
}

// NewGridTraderAdapter creates an adapter for basic Trader
func NewGridTraderAdapter(t Trader) *GridTraderAdapter {
	return &GridTraderAdapter{Trader: t}
}

// PlaceLimitOrder refuses to FABRICATE a grid entry (F12, 2026-10-01
// review): the old fallback mapped BUY→SHORT-stop / SELL→LONG-TP and echoed
// the client ID back as an exchange order ID with status NEW — a resting
// EXIT-PROTECTION order is not a resting ENTRY limit, and the synthetic ID
// poisoned the grid ledger (canceling it canceled nothing, a trigger opened
// an opposite position the ledger never tracked). Exchanges without a
// native GridTrader implementation must not run grid strategies;
// placeGridLimitOrder rejects the adapter path up front and the decision
// record shows the failure.
func (a *GridTraderAdapter) PlaceLimitOrder(req *LimitOrderRequest) (*LimitOrderResult, error) {
	return nil, fmt.Errorf("grid limit entry not supported on this exchange adapter: no native GridTrader implementation (order %s NOT placed — the protective-order fallback is disabled)", req.ClientID)
}

// CancelOrder cancels a specific order
func (a *GridTraderAdapter) CancelOrder(symbol, orderID string) error {
	// Try to use CancelOrder if trader supports it directly
	if canceler, ok := a.Trader.(interface {
		CancelOrder(symbol, orderID string) error
	}); ok {
		return canceler.CancelOrder(symbol, orderID)
	}

	// For traders that only support CancelAllOrders, log a warning
	// This is a limitation - we cannot cancel individual orders
	logger.Warnf("[Grid] Trader does not support individual order cancellation, "+
		"cannot cancel order %s. Consider using exchange-specific GridTrader implementation.", orderID)

	// Return error instead of canceling all orders
	return fmt.Errorf("individual order cancellation not supported for this exchange")
}

// GetOrderBook returns empty order book (not supported in basic Trader)
func (a *GridTraderAdapter) GetOrderBook(symbol string, depth int) (bids, asks [][]float64, err error) {
	// Not supported, return empty
	return nil, nil, nil
}
