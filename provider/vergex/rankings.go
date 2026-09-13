package vergex

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"nofx/provider/nofxos"
)

// This file maps vergex trending endpoints onto the nofxos ranking structures
// used by the strategy engine's AI prompt builder. The upstream field shapes
// are identical (both describe the same crypto rankings), so the data fills
// the nofxos structs directly and the existing prompt formatters keep working.

// durationOI maps strategy durations to what /trending-crypto supports.
var durationOI = map[string]bool{
	"5m": true, "15m": true, "30m": true, "1h": true,
	"4h": true, "8h": true, "12h": true, "24h": true,
}

// durationPrice is what tab=price supports (no 5m).
var durationPrice = map[string]bool{
	"15m": true, "30m": true, "1h": true, "4h": true,
	"8h": true, "12h": true, "24h": true,
}

// durationFlow is what /flow/markets supports (no 30m).
var durationFlow = map[string]bool{
	"5m": true, "15m": true, "1h": true, "4h": true,
	"8h": true, "12h": true, "24h": true,
}

// flowMaxLimit is the largest limit /flow/markets accepts.
const flowMaxLimit = 25

// normalizeDuration returns fallback when the requested duration is
// unsupported by the target endpoint.
func normalizeDuration(duration string, supported map[string]bool, fallback string) string {
	if supported[duration] {
		return duration
	}
	return fallback
}

// trendingQuery builds a /trending-crypto query.
func trendingQuery(tab, duration string, limit int) url.Values {
	q := url.Values{}
	q.Set("tab", tab)
	if duration != "" {
		q.Set("duration", duration)
	}
	q.Set("limit", fmt.Sprintf("%d", limit))
	return q
}

// GetOIRanking returns market-wide OI increase/decrease rankings. Every call
// performs a fresh upstream request.
func (c *Client) GetOIRanking(duration string, limit int) (*nofxos.OIRankingData, error) {
	if duration == "" {
		duration = "1h"
	}
	duration = normalizeDuration(duration, durationOI, "1h")
	if limit <= 0 {
		limit = 10
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	rows, err := c.fetchOIRows(duration, limit)
	if err != nil {
		return nil, err
	}

	result := &nofxos.OIRankingData{
		Duration:  duration,
		FetchedAt: time.Now(),
	}
	for _, r := range rows.Top {
		result.TopPositions = append(result.TopPositions, r.toNofxos())
	}
	for _, r := range rows.Low {
		result.LowPositions = append(result.LowPositions, r.toNofxos())
	}
	// Relay-fed payloads may be larger than the requested limit (the browser
	// relays limit=100 while callers ask for 10-20) — slice to the request.
	result.TopPositions = truncateSlice(result.TopPositions, limit)
	result.LowPositions = truncateSlice(result.LowPositions, limit)
	return result, nil
}

// truncateSlice caps a decoded list at n entries (no-op when shorter).
func truncateSlice[T any](xs []T, n int) []T {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}

// oiRowAPI is one row of /trending-crypto?tab=oi (top and low lists). The
// JSON fields match nofxos.OIPosition one-to-one.
type oiRowAPI struct {
	Rank              int     `json:"rank"`
	Symbol            string  `json:"symbol"`
	Price             float64 `json:"price"`
	CurrentOI         float64 `json:"current_oi"`
	OIDelta           float64 `json:"oi_delta"`
	OIDeltaPercent    float64 `json:"oi_delta_percent"`
	OIDeltaValue      float64 `json:"oi_delta_value"`
	PriceDeltaPercent float64 `json:"price_delta_percent"`
	NetLong           float64 `json:"net_long"`
	NetShort          float64 `json:"net_short"`
}

func (r oiRowAPI) toNofxos() nofxos.OIPosition {
	return nofxos.OIPosition{
		Symbol:            r.Symbol,
		Rank:              r.Rank,
		Price:             r.Price,
		CurrentOI:         r.CurrentOI,
		OIDelta:           r.OIDelta,
		OIDeltaPercent:    r.OIDeltaPercent,
		OIDeltaValue:      r.OIDeltaValue,
		PriceDeltaPercent: r.PriceDeltaPercent,
		NetLong:           r.NetLong,
		NetShort:          r.NetShort,
	}
}

type oiRowsResponse struct {
	Top []oiRowAPI `json:"top"`
	Low []oiRowAPI `json:"low"`
}

// fetchOIRows fetches the OI top/low lists for a duration, skipping upstream
// placeholder rows.
func (c *Client) fetchOIRows(duration string, limit int) (*oiRowsResponse, error) {
	var resp oiRowsResponse
	if err := c.fetchJSON("/trending-crypto?"+trendingQuery("oi", duration, limit).Encode(), &resp); err != nil {
		return nil, err
	}
	resp.Top = filterOIRows(resp.Top)
	resp.Low = filterOIRows(resp.Low)
	return &resp, nil
}

func filterOIRows(rows []oiRowAPI) []oiRowAPI {
	out := rows[:0]
	for _, r := range rows {
		if tradableSymbol(r.Symbol) != "" {
			out = append(out, r)
		}
	}
	return out
}

// GetPriceRanking returns the price gainers/losers ranking (top/low) for one
// duration — the data page's "Top Movers" tab. price_delta is a decimal
// fraction in both upstream and the nofxos struct.
func (c *Client) GetPriceRanking(duration string, limit int) (*nofxos.PriceRankingData, error) {
	if duration == "" {
		duration = "1h"
	}
	duration = normalizeDuration(duration, durationPrice, "1h")
	if limit <= 0 {
		limit = 10
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	var resp struct {
		Top []nofxos.PriceRankingItem `json:"top"`
		Low []nofxos.PriceRankingItem `json:"low"`
	}
	if err := c.fetchJSON("/trending-crypto?"+trendingQuery("price", duration, limit).Encode(), &resp); err != nil {
		return nil, err
	}

	// Relay-fed payloads may exceed the requested limit — slice to it.
	return &nofxos.PriceRankingData{
		Durations: map[string]*nofxos.PriceRankingDuration{
			duration: {Top: truncateSlice(resp.Top, limit), Low: truncateSlice(resp.Low, limit)},
		},
		FetchedAt: time.Now(),
	}, nil
}

// GetNetFlowRanking returns the market-wide net inflow/outflow ranking from
// vergex /flow/markets (the data page's "Net Flow" card). NofxOS split this
// into institution/personal quadrants; vergex provides one merged per-market
// ranking, which is reported in the institution (large-order) slots while the
// retail slots stay empty (the prompt formatter skips empty lists).
func (c *Client) GetNetFlowRanking(duration string, limit int) (*nofxos.NetFlowRankingData, error) {
	if duration == "" {
		duration = "1h"
	}
	duration = normalizeDuration(duration, durationFlow, "1h")
	if limit <= 0 {
		limit = 10
	}
	if limit > flowMaxLimit {
		limit = flowMaxLimit
	}

	inflow, outflow, err := c.fetchFlowRows(duration, limit)
	if err != nil {
		return nil, err
	}

	return &nofxos.NetFlowRankingData{
		Duration:             duration,
		InstitutionFutureTop: inflow,
		InstitutionFutureLow: outflow,
		FetchedAt:            time.Now(),
	}, nil
}

// flowRow is one row of /flow/markets inflow/outflow.
type flowRow struct {
	Key            string `json:"key"`
	MarketType     string `json:"marketType"`
	Symbol         string `json:"symbol"`
	NetFlow        string `json:"netFlow"`
	LatestPrice    string `json:"latestPrice"`
	PriceChangePct any    `json:"priceChangePct"`
}

// fetchFlowRows fetches the net inflow/outflow lists as nofxos.NetFlowPosition.
func (c *Client) fetchFlowRows(window string, limit int) (inflow, outflow []nofxos.NetFlowPosition, err error) {
	q := url.Values{}
	q.Set("window", window)
	q.Set("limit", fmt.Sprintf("%d", limit))

	var resp struct {
		Data struct {
			Inflow  []flowRow `json:"inflow"`
			Outflow []flowRow `json:"outflow"`
		} `json:"data"`
	}
	if err = c.fetchJSON("/api/v1/data-intelligence/flow/markets?"+q.Encode(), &resp); err != nil {
		return nil, nil, err
	}

	toPositions := func(rows []flowRow) []nofxos.NetFlowPosition {
		out := make([]nofxos.NetFlowPosition, 0, len(rows))
		for _, r := range rows {
			if len(out) >= limit {
				break
			}
			sym := tradableSymbol(r.Symbol)
			if sym == "" {
				continue
			}
			out = append(out, nofxos.NetFlowPosition{
				Rank:   len(out) + 1,
				Symbol: sym,
				Amount: parseNumericString(r.NetFlow),
				Price:  parseNumericString(r.LatestPrice),
			})
		}
		return out
	}

	return toPositions(resp.Data.Inflow), toPositions(resp.Data.Outflow), nil
}

// parseNumericString parses vergex's numeric-string fields, tolerating
// empty/invalid values.
func parseNumericString(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

// GetCoinSnapshot returns a single-coin quantitative snapshot (1h window)
// assembled from the OI and price ranking lists. NofxOS supplied per-exchange
// multi-timeframe data which vergex does not offer; this fills the 1h
// timeframe with aggregated Hyperliquid figures instead.
func (c *Client) GetCoinSnapshot(symbol string) (*nofxos.QuantData, error) {
	target := NormalizeSymbol(symbol)
	if target == "" {
		return nil, fmt.Errorf("empty symbol")
	}

	oiRows, err := c.fetchOIRows("1h", maxLimit)
	if err != nil {
		return nil, err
	}

	snap := &nofxos.QuantData{Symbol: target}

	findRow := func(rows []oiRowAPI) *oiRowAPI {
		for i := range rows {
			if tradableSymbol(rows[i].Symbol) == target {
				return &rows[i]
			}
		}
		return nil
	}

	row := findRow(oiRows.Top)
	if row == nil {
		row = findRow(oiRows.Low)
	}
	if row != nil {
		snap.OI = map[string]*nofxos.OIData{
			"hyperliquid": {
				CurrentOI: row.CurrentOI,
				NetLong:   row.NetLong,
				NetShort:  row.NetShort,
				Delta: map[string]*nofxos.OIDeltaData{
					"1h": {
						OIDelta:        row.OIDelta,
						OIDeltaValue:   row.OIDeltaValue,
						OIDeltaPercent: row.OIDeltaPercent,
					},
				},
			},
		}
	}

	var priceResp struct {
		Top []nofxos.PriceRankingItem `json:"top"`
		Low []nofxos.PriceRankingItem `json:"low"`
	}
	if err := c.fetchJSON("/trending-crypto?"+trendingQuery("price", "1h", maxLimit).Encode(), &priceResp); err != nil {
		if snap.OI == nil {
			return nil, err
		}
		// OI data alone is still useful — fall through with no price info.
		return snap, nil
	}

	matchPrice := func(items []nofxos.PriceRankingItem) bool {
		for _, it := range items {
			if strings.EqualFold(it.Pair, target) || NormalizeSymbol(it.Symbol) == target {
				snap.Price = it.Price
				snap.PriceChange = map[string]float64{"1h": it.PriceDelta}
				return true
			}
		}
		return false
	}
	if !matchPrice(priceResp.Top) {
		matchPrice(priceResp.Low)
	}

	// Fallback for coins outside the movers lists (e.g. BTC): the Hyperliquid
	// universe carries lastPrice and 24h change for every tradable symbol.
	if snap.Price == 0 {
		if lastPrice, changePct, ok := c.lookupHLUniverse(target); ok {
			snap.Price = lastPrice
			// HL change24hPct is already in percent; PriceChange is a fraction.
			snap.PriceChange = map[string]float64{"24h": changePct / 100}
		}
	}

	return snap, nil
}

// lookupHLUniverse finds a symbol's last price and 24h change in the
// Hyperliquid universe list (covers all tradable perps, not just movers).
func (c *Client) lookupHLUniverse(target string) (lastPrice, changePct float64, ok bool) {
	base := strings.TrimSuffix(target, "USDT")
	var resp struct {
		Rows []struct {
			Base         string  `json:"base"`
			LastPrice    float64 `json:"lastPrice"`
			Change24hPct float64 `json:"change24hPct"`
		} `json:"rows"`
	}
	if err := c.fetchJSON("/trending-hl?category=crypto", &resp); err != nil {
		return 0, 0, false
	}
	for _, r := range resp.Rows {
		if strings.EqualFold(r.Base, base) {
			return r.LastPrice, r.Change24hPct, true
		}
	}
	return 0, 0, false
}
