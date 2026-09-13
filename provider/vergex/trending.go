// Package vergex fetches trending market data from vergex.trade — the same
// public endpoints that power the Vergex /trending page (and NOFX's data page,
// which proxies them via /api/trending/*). Keeping the strategy coin sources
// on these endpoints guarantees the trader candidates match what the user
// sees on the data page.
package vergex

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://vergex.trade"
	httpTimeout    = 15 * time.Second
	maxLimit       = 100
)

// Client fetches trending data from vergex.trade. It is stateless — every
// call performs a fresh upstream request so strategy cycles always see the
// latest rankings.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient creates a vergex trending data client.
func NewClient() *Client {
	return &Client{
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: httpTimeout},
	}
}

// fetchJSON GETs pathWithQuery and decodes the JSON response. Fresh
// browser-relayed payloads (see relay.go) are served without a network
// request, since direct backend requests are challenged by Cloudflare.
func (c *Client) fetchJSON(pathWithQuery string, out any) error {
	served, err := relayLookupJSON(pathWithQuery, out)
	if served || err != nil {
		return err
	}

	resp, err := c.http.Get(c.baseURL + pathWithQuery)
	if err != nil {
		return fmt.Errorf("vergex request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vergex returned status %d for %s", resp.StatusCode, pathWithQuery)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("vergex read body failed: %w", err)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("vergex JSON decode failed: %w", err)
	}
	return nil
}

// AI500Asset is one featured AI500 entry from /trending-category.
type AI500Asset struct {
	Symbol string  `json:"symbol"`
	Pair   string  `json:"pair"`
	Score  float64 `json:"score"`
	Signal string  `json:"signal"`
}

type trendingCategoryResponse struct {
	Category struct {
		Assets []AI500Asset `json:"assets"`
	} `json:"category"`
	TableRows []struct {
		Symbol string  `json:"symbol"`
		Pair   string  `json:"pair"`
		Score  float64 `json:"score"`
	} `json:"tableRows"`
}

// GetAI500Symbols returns up to limit AI500 picks as XXXUSDT symbols, best
// score first. This is the same featured list shown on the data page's
// "AI500 Picks" card.
func (c *Client) GetAI500Symbols(limit int) ([]string, error) {
	q := url.Values{}
	q.Set("lang", "en")
	q.Set("key", "ai500")

	var resp trendingCategoryResponse
	if err := c.fetchJSON("/trending-category?"+q.Encode(), &resp); err != nil {
		return nil, err
	}

	type scored struct {
		symbol string
		score  float64
	}
	var entries []scored
	for _, a := range resp.Category.Assets {
		entries = append(entries, scored{symbol: a.Pair, score: a.Score})
	}
	// The category response can also carry a full ranking table when the
	// market session provides one — include it, keeping assets (highest
	// featured scores) first.
	for _, r := range resp.TableRows {
		entries = append(entries, scored{symbol: r.Pair, score: r.Score})
	}

	var symbols []string
	seen := make(map[string]bool)
	for _, e := range entries {
		s := NormalizeSymbol(e.symbol)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		symbols = append(symbols, s)
		if len(symbols) >= limit {
			break
		}
	}
	return symbols, nil
}

// oiRow is one row of /trending-crypto?tab=oi (both top and low lists).
type oiRow struct {
	Rank           int     `json:"rank"`
	Symbol         string  `json:"symbol"`
	OIDeltaPercent float64 `json:"oi_delta_percent"`
}

type trendingCryptoOIResponse struct {
	Top []oiRow `json:"top"`
	Low []oiRow `json:"low"`
}

// GetOITopSymbols returns up to limit symbols with the largest OI increase
// (the data page's "Open Interest" top list, 1h window).
func (c *Client) GetOITopSymbols(limit int) ([]string, error) {
	return c.fetchOISymbols("top", limit)
}

// GetOILowSymbols returns up to limit symbols with the largest OI decrease
// (the data page's "Open Interest" low list, 1h window).
func (c *Client) GetOILowSymbols(limit int) ([]string, error) {
	return c.fetchOISymbols("low", limit)
}

func (c *Client) fetchOISymbols(side string, limit int) ([]string, error) {
	if limit > maxLimit {
		limit = maxLimit
	}
	q := url.Values{}
	q.Set("tab", "oi")
	q.Set("duration", "1h")
	q.Set("limit", fmt.Sprintf("%d", limit))

	var resp trendingCryptoOIResponse
	if err := c.fetchJSON("/trending-crypto?"+q.Encode(), &resp); err != nil {
		return nil, err
	}

	rows := resp.Top
	if side == "low" {
		rows = resp.Low
	}

	var symbols []string
	for _, r := range rows {
		// Skip upstream placeholder rows and non-tradable symbols
		// (e.g. localized display names like "龙虾USDT").
		s := tradableSymbol(r.Symbol)
		if s == "" {
			continue
		}
		symbols = append(symbols, s)
		if len(symbols) >= limit {
			break
		}
	}
	return symbols, nil
}

// tradableSymbol normalizes an upstream symbol to XXXUSDT, rejecting entries
// that cannot be traded (empty, or containing non-ASCII characters).
func tradableSymbol(symbol string) string {
	s := strings.TrimSpace(strings.ToUpper(symbol))
	if s == "" {
		return ""
	}
	for _, r := range s {
		if r > 127 {
			return ""
		}
	}
	if !strings.HasSuffix(s, "USDT") {
		s += "USDT"
	}
	return s
}

// NormalizeSymbol normalizes a coin symbol to XXXUSDT format.
func NormalizeSymbol(symbol string) string {
	s := strings.TrimSpace(strings.ToUpper(symbol))
	if s == "" {
		return ""
	}
	if !strings.HasSuffix(s, "USDT") {
		s += "USDT"
	}
	return s
}
