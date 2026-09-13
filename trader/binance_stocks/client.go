// Package binance_stocks implements trading for Binance Stocks — U.S.-listed
// equities and ETFs traded on Binance via the /sapi/v1/equity/* REST family
// (https://developers.binance.com/en/docs/products/stocks/introduction).
//
// Key product differences from Binance futures:
//   - Cash market only: no leverage, no shorting, no hedge mode. The Trader
//     interface maps open_long→BUY, close_long→SELL; short actions return an
//     explicit "unsupported" error.
//   - MARKET BUY orders are placed by notional (USD); MARKET SELL by quantity.
//   - LIMIT orders require tradingSession (RTH/EXTENDED/24H).
//   - No exchange-side stop-loss/take-profit order type — those decisions must
//     be executed by the AI in later cycles.
//   - No positions endpoint: positions are derived from equity trade history.
package binance_stocks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.binance.com"
	requestTimeout = 15 * time.Second
)

func baseURL() string {
	if v := os.Getenv("BINANCE_STOCKS_BASE"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultBaseURL
}

// Client is a signed client for the Binance Stocks (equity) API.
type Client struct {
	apiKey    string
	secretKey string
	http      *http.Client
}

// NewClient creates a stocks API client.
func NewClient(apiKey, secretKey string) *Client {
	return &Client{
		apiKey:    apiKey,
		secretKey: secretKey,
		http:      &http.Client{Timeout: requestTimeout},
	}
}

// signedGet performs a GET SIGNED (USER_DATA/TRADE) request.
func (c *Client) signedGet(ctx context.Context, path string, params url.Values, out interface{}) error {
	return c.signedRequest(ctx, http.MethodGet, path, params, out)
}

// signedPost performs a POST SIGNED (TRADE) request.
func (c *Client) signedPost(ctx context.Context, path string, params url.Values, out interface{}) error {
	return c.signedRequest(ctx, http.MethodPost, path, params, out)
}

func (c *Client) signedRequest(ctx context.Context, method, path string, params url.Values, out interface{}) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("timestamp", fmt.Sprintf("%d", time.Now().UnixMilli()))
	params.Set("recvWindow", "10000")

	raw := params.Encode()
	mac := hmac.New(sha256.New, []byte(c.secretKey))
	mac.Write([]byte(raw))
	signature := hex.EncodeToString(mac.Sum(nil))
	raw += "&signature=" + signature

	endpoint := baseURL() + path + "?" + raw
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request failed: %w", err)
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return parseAPIError(resp.StatusCode, body)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode failed: %w", err)
	}
	return nil
}

// publicGet performs an unsigned MARKET_DATA request (API key header, no signature).
func (c *Client) publicGet(ctx context.Context, path string, params url.Values, out interface{}) error {
	endpoint := baseURL() + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request failed: %w", err)
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return parseAPIError(resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode failed: %w", err)
	}
	return nil
}

// apiError mirrors the Binance error envelope {"code":-XXXX,"msg":"..."}.
type apiError struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("<APIError> code=%d, msg=%s", e.Code, e.Msg)
}

func parseAPIError(status int, body []byte) error {
	var ae apiError
	if err := json.Unmarshal(body, &ae); err == nil && ae.Code != 0 {
		return &ae
	}
	return fmt.Errorf("status %d: %s", status, truncate(string(body), 200))
}

// IsAuthError reports whether err is a Binance -2015/-2014 rejection.
func IsAuthError(err error) bool {
	var ae *apiError
	if ok := asAPIError(err, &ae); ok {
		return ae.Code == -2015 || ae.Code == -2014
	}
	return false
}

func asAPIError(err error, target **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func fToStr(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
