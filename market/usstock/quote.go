package usstock

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

type quoteCacheEntry struct {
	quote  Quote
	loaded time.Time
}

var quoteCache = struct {
	sync.Mutex
	entries map[string]quoteCacheEntry
}{entries: make(map[string]quoteCacheEntry)}

// GetQuote compares the current bStock price to the Yahoo session reference.
// References are fresh only within the same session and at most 15 minutes old.
func GetQuote(ctx context.Context, symbol string) (*Quote, error) {
	info, ok := LookupSymbol(ctx, symbol)
	if !ok {
		return nil, fmt.Errorf("unknown or unavailable bStock symbol %q", symbol)
	}
	quoteCache.Lock()
	defer quoteCache.Unlock()
	now := nowFunc()
	session := SessionAt(now)
	if entry, ok := quoteCache.entries[symbol]; ok && now.Sub(entry.loaded) < 30*time.Second && entry.quote.Session == session {
		quote := entry.quote
		setFresh(&quote, now)
		return &quote, nil
	}
	var ticker struct {
		Price string `json:"price"`
	}
	if err := fetchJSON(ctx, spotBaseURL, "/api/v3/ticker/price", url.Values{"symbol": {symbol}}, false, &ticker); err != nil {
		return nil, err
	}
	price, err := strconv.ParseFloat(ticker.Price, 64)
	if err != nil || price <= 0 {
		return nil, fmt.Errorf("invalid bStock ticker price %q", ticker.Price)
	}
	prePost := session == SessionPre || session == SessionAfter
	// A 30s chart TTL ensures GetQuote never uses the longer series cache.
	result, err := yahooChart(ctx, info.Underlying, "1m", "1d", prePost, 30*time.Second)
	if err != nil {
		return nil, err
	}
	// Retries can cross a session boundary. Evaluate freshness at response time,
	// so a delayed regular-session quote cannot remain fresh after the close.
	now = nowFunc()
	session = SessionAt(now)
	prePost = session == SessionPre || session == SessionAfter
	quote := Quote{Symbol: symbol, BStockPrice: price, Session: session, RefPrice: result.Meta.RegularMarketPrice}
	if result.Meta.RegularMarketTime > 0 {
		quote.RefTime = time.Unix(result.Meta.RegularMarketTime, 0).UTC()
	}
	if prePost {
		// Only close and timestamp are needed for a reference; incomplete OHLCV
		// arrays must not discard an otherwise valid pre/post quote.
		quote.RefPrice, quote.RefTime = 0, time.Time{}
		if len(result.Indicators.Quote) > 0 {
			closes := result.Indicators.Quote[0].Close
			for i, stamp := range result.Timestamp {
				t := time.Unix(stamp, 0).UTC()
				if i < len(closes) && closes[i] != nil && *closes[i] > 0 && !t.After(now) && (quote.RefTime.IsZero() || t.After(quote.RefTime)) {
					quote.RefPrice, quote.RefTime = *closes[i], t
				}
			}
		}
	}
	if quote.RefPrice > 0 {
		quote.DivergencePct = (price - quote.RefPrice) / quote.RefPrice * 100
	}
	setFresh(&quote, now)
	quoteCache.entries[symbol] = quoteCacheEntry{quote, now}
	return &quote, nil
}
func setFresh(quote *Quote, now time.Time) {
	age := now.Sub(quote.RefTime)
	quote.RefFresh = quote.Session != SessionClosed && quote.RefPrice > 0 && !quote.RefTime.IsZero() && age >= 0 && age <= 15*time.Minute && !quote.RefTime.Before(sessionStart(now, quote.Session))
}
