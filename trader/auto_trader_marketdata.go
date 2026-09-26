package trader

import (
	"fmt"

	"nofx/market"
)

// getMarketData obtains an execution quote from the same adapter that sends
// orders, then combines it with exchange-specific candles. If the quote is
// unavailable, the caller must skip the trading operation rather than use a
// stale candle close or another venue's price.
func (at *AutoTrader) getMarketData(symbol string) (*market.Data, error) {
	price, err := at.trader.GetMarketPrice(symbol)
	if err != nil {
		return nil, fmt.Errorf("get %s execution price from %s: %w", symbol, at.exchange, err)
	}
	if price <= 0 {
		return nil, fmt.Errorf("invalid %s execution price from %s: %g", symbol, at.exchange, price)
	}
	return market.GetWithExchangeAndPrice(symbol, at.exchange, price)
}

func (at *AutoTrader) getMarketTimeframes(symbol string, timeframes []string, primaryTimeframe string, count int) (*market.Data, error) {
	price, err := at.trader.GetMarketPrice(symbol)
	if err != nil {
		return nil, fmt.Errorf("get %s execution price from %s: %w", symbol, at.exchange, err)
	}
	if price <= 0 {
		return nil, fmt.Errorf("invalid %s execution price from %s: %g", symbol, at.exchange, price)
	}
	return market.GetWithTimeframesForExchange(symbol, timeframes, primaryTimeframe, count, at.exchange, price)
}
