package usstock

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"
)

// PerpQuote is a last-trade price, timestamped before the uncached request.
type PerpQuote struct {
	Symbol string    `json:"symbol"`
	Price  float64   `json:"price"`
	Time   time.Time `json:"time"`
}

// GetPerpQuote reads the mapped perpetual's last price. Callers may inject this
// function at the trader boundary; package tests replace the HTTP transport.
func GetPerpQuote(ctx context.Context, symbol string) (*PerpQuote, error) {
	started := nowFunc()
	var raw struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
	}
	if err := fetchJSON(ctx, futuresBaseURL, "/fapi/v1/ticker/price", url.Values{"symbol": {symbol}}, false, &raw); err != nil {
		return nil, err
	}
	price, err := strconv.ParseFloat(raw.Price, 64)
	if err != nil || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) || raw.Symbol != symbol {
		return nil, fmt.Errorf("invalid perpetual quote for %s", symbol)
	}
	return &PerpQuote{Symbol: symbol, Price: price, Time: started}, nil
}
