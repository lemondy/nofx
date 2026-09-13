package binance_stocks

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ── API data types ──

// EquitySymbol is one tradable US-equity from exchangeInfo.
type EquitySymbol struct {
	Symbol          string `json:"symbol"`
	Tradability     string `json:"tradability"` // BUY_SELL / BUY / SELL / NONE
	Overnight       bool   `json:"overnightSupported"`
	Fractional      bool   `json:"fractionable"`
	ExtendedSession bool   `json:"extendedSession"`
	StepSize        string `json:"stepSize"`
	MinQty          string `json:"minQty"`
	MaxQty          string `json:"maxQty"`
	MinNotional     string `json:"minNotional"`
}

// EquityOrder is an order as returned by the equity order endpoints.
type EquityOrder struct {
	Symbol          string `json:"symbol"`
	OrderID         int64  `json:"orderId"`
	ClientOrderID   string `json:"clientOrderId"`
	Price           string `json:"price"`
	OrigQty         string `json:"origQty"`
	ExecutedQty     string `json:"executedQty"`
	CumulativeQuote string `json:"cumulativeQuoteQuantity"`
	Status          string `json:"status"` // NEW / FILLED / CANCELED / ...
	Side            string `json:"side"`   // BUY / SELL
	Type            string `json:"type"`   // MARKET / LIMIT
	TimeInForce     string `json:"timeInForce"`
	TradingSession  string `json:"tradingSession"`
	Commission      string `json:"commission"`
	UpdateTime      int64  `json:"updateTime"`
}

// EquityTrade is one fill from trade history.
type EquityTrade struct {
	ID              int64  `json:"id"`
	Symbol          string `json:"symbol"`
	OrderID         int64  `json:"orderId"`
	Price           string `json:"price"`
	Quantity        string `json:"quantity"`
	QuoteQty        string `json:"quoteQty"`
	Commission      string `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`
	Side            string `json:"side"` // BUY / SELL
	Time            int64  `json:"time"`
}

// Quote is the latest quote for a ticker.
type Quote struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
	Time   int64  `json:"time"`
}

// ── Account ──

// SignDisclaimer signs the US equity trading disclaimer (required once per account).
func (c *Client) SignDisclaimer(ctx context.Context) error {
	return c.signedPost(ctx, "/sapi/v1/equity/account/disclaimer", url.Values{}, nil)
}

// SpotBalances returns spot-wallet balances (the equity product settles against
// the spot wallet; used as the trader's account equity).
type SpotBalance struct {
	Asset  string `json:"asset"`
	Free   string `json:"free"`
	Locked string `json:"locked"`
}

// GetSpotBalances returns all non-zero spot balances (signed /sapi/v3/account).
func (c *Client) GetSpotBalances(ctx context.Context) ([]SpotBalance, error) {
	var out struct {
		Balances []SpotBalance `json:"balances"`
	}
	if err := c.signedGet(ctx, "/sapi/v3/account", url.Values{}, &out); err != nil {
		// Older deployments may not expose v3 — fall back to v1.
		if retryErr := c.signedGet(ctx, "/sapi/v1/account", url.Values{}, &out); retryErr != nil {
			return nil, err
		}
	}
	return out.Balances, nil
}

// ── Market data ──

// GetExchangeInfo returns the tradable US-equity symbols (optionally filtered).
func (c *Client) GetExchangeInfo(ctx context.Context, symbol string) ([]EquitySymbol, error) {
	params := url.Values{}
	if symbol != "" {
		params.Set("symbol", strings.ToUpper(symbol))
	}
	var out struct {
		Symbols []EquitySymbol `json:"symbols"`
	}
	if err := c.publicGet(ctx, "/sapi/v1/equity/market/exchangeInfo", params, &out); err != nil {
		return nil, err
	}
	return out.Symbols, nil
}

// GetLatestQuote returns the latest quote for a ticker.
func (c *Client) GetLatestQuote(ctx context.Context, symbol string) (*Quote, error) {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	var out Quote
	if err := c.publicGet(ctx, "/sapi/v1/equity/market/quote", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Trading ──

// OrderRequest describes an equity order per the field matrix:
//
//	BUY  LIMIT  → price + quantity + tradingSession
//	BUY  MARKET → notional
//	SELL LIMIT  → price + quantity + tradingSession
//	SELL MARKET → quantity
type OrderRequest struct {
	Symbol         string
	Side           string // BUY / SELL
	OrderType      string // MARKET / LIMIT
	Quantity       float64
	Notional       float64
	Price          float64
	TradingSession string // RTH / EXTENDED / 24H (LIMIT only)
	TimeInForce    string // DAY (default) / GTC
	NewClientID    string
}

// PlaceOrder places an equity order.
func (c *Client) PlaceOrder(ctx context.Context, req OrderRequest) (*EquityOrder, error) {
	symbol := strings.ToUpper(strings.TrimSpace(req.Symbol))
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	side := strings.ToUpper(req.Side)
	orderType := strings.ToUpper(req.OrderType)
	if side != "BUY" && side != "SELL" {
		return nil, fmt.Errorf("side must be BUY or SELL")
	}
	if orderType != "MARKET" && orderType != "LIMIT" {
		return nil, fmt.Errorf("orderType must be MARKET or LIMIT")
	}

	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", side)
	params.Set("orderType", orderType)

	switch {
	case orderType == "LIMIT":
		if req.Price <= 0 || req.Quantity <= 0 {
			return nil, fmt.Errorf("LIMIT requires price and quantity")
		}
		if req.TradingSession == "" {
			req.TradingSession = "RTH"
		}
		params.Set("price", fToStr(req.Price))
		params.Set("quantity", fToStr(req.Quantity))
		params.Set("tradingSession", req.TradingSession)
		if req.TimeInForce != "" {
			params.Set("timeInForce", strings.ToUpper(req.TimeInForce))
		}
	case side == "BUY": // MARKET BUY → notional
		if req.Notional <= 0 {
			return nil, fmt.Errorf("MARKET BUY requires notional (USD amount)")
		}
		params.Set("notional", fToStr(req.Notional))
	default: // MARKET SELL → quantity
		if req.Quantity <= 0 {
			return nil, fmt.Errorf("MARKET SELL requires quantity")
		}
		params.Set("quantity", fToStr(req.Quantity))
	}

	if req.NewClientID != "" {
		params.Set("newClientOrderId", req.NewClientID)
	}

	var out EquityOrder
	if err := c.signedPost(ctx, "/sapi/v1/equity/order/place", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelOrder cancels one equity order.
func (c *Client) CancelOrder(ctx context.Context, symbol string, orderID int64) error {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	params.Set("orderId", strconv.FormatInt(orderID, 10))
	return c.signedPost(ctx, "/sapi/v1/equity/order/cancel", params, nil)
}

// CancelAllOrders cancels all open orders for a symbol.
func (c *Client) CancelAllOrders(ctx context.Context, symbol string) error {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	return c.signedPost(ctx, "/sapi/v1/equity/order/cancel-all", params, nil)
}

// GetOpenOrders returns currently open equity orders.
func (c *Client) GetOpenOrders(ctx context.Context, symbol string) ([]EquityOrder, error) {
	params := url.Values{}
	if symbol != "" {
		params.Set("symbol", strings.ToUpper(symbol))
	}
	var out []EquityOrder
	if err := c.signedGet(ctx, "/sapi/v1/equity/order/open-orders", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetOrderHistory returns equity order history for a symbol.
func (c *Client) GetOrderHistory(ctx context.Context, symbol string, limit int) ([]EquityOrder, error) {
	if limit <= 0 {
		limit = 100
	}
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	params.Set("limit", strconv.Itoa(limit))
	var out []EquityOrder
	if err := c.signedGet(ctx, "/sapi/v1/equity/order/history", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetOrderDetail returns one order by ID.
func (c *Client) GetOrderDetail(ctx context.Context, symbol string, orderID int64) (*EquityOrder, error) {
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	params.Set("orderId", strconv.FormatInt(orderID, 10))
	var out EquityOrder
	if err := c.signedGet(ctx, "/sapi/v1/equity/order/detail", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTradeHistory returns equity fills for a symbol since startTime.
func (c *Client) GetTradeHistory(ctx context.Context, symbol string, startTime time.Time, limit int) ([]EquityTrade, error) {
	if limit <= 0 {
		limit = 500
	}
	params := url.Values{}
	params.Set("symbol", strings.ToUpper(symbol))
	if !startTime.IsZero() {
		params.Set("startTime", strconv.FormatInt(startTime.UnixMilli(), 10))
	}
	params.Set("limit", strconv.Itoa(limit))
	var out []EquityTrade
	if err := c.signedGet(ctx, "/sapi/v1/equity/trade/history", params, &out); err != nil {
		return nil, err
	}
	return out, nil
}
