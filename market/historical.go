package market

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"nofx/market/bnweight"
	"nofx/market/pubcache"
	"nofx/security"
	"time"
)

const (
	binanceFuturesKlinesURL = "https://fapi.binance.com/fapi/v1/klines"
	binanceMaxKlineLimit    = 1500
)

// ErrInvalidSymbol identifies Binance's permanent -1121 symbol rejection.
var ErrInvalidSymbol = errors.New("invalid symbol")

// Preserve the historical API error text while exposing a sentinel to callers.
type invalidSymbolError struct {
	message string
}

func (e *invalidSymbolError) Error() string { return e.message }
func (e *invalidSymbolError) Unwrap() error { return ErrInvalidSymbol }

// GetKlinesRange fetches K-line series within specified time range (closed interval), returns data sorted by time in ascending order.
// History paging runs at bnweight.Low priority; callers that need the data for
// live trading decisions should use GetKlinesRangePriority.
func GetKlinesRange(symbol string, timeframe string, start, end time.Time) ([]Kline, error) {
	return GetKlinesRangePriority(symbol, timeframe, start, end, bnweight.Low)
}

// klinePageLimit sizes one page to the bars still needed in [cursorMs, endMs]
// (+1 spare so the last bar is never cut off), capped at Binance's maximum.
// Binance klines weight grows with limit (<100 → 1, ≥1000 → 10), so a short
// range must not pay for a full 1500-bar page.
func klinePageLimit(cursorMs, endMs int64, barMs int64) int {
	if barMs <= 0 {
		return binanceMaxKlineLimit
	}
	span := endMs - cursorMs
	if span < 0 {
		span = 0
	}
	bars := (span+barMs-1)/barMs + 1
	if bars > binanceMaxKlineLimit {
		return binanceMaxKlineLimit
	}
	if bars < 1 {
		return 1
	}
	return int(bars)
}

// GetKlinesRangePriority is GetKlinesRange with an explicit bnweight priority:
// under IP-weight pressure Low requests are held first, so latency-sensitive
// callers (e.g. paper execution simulation) pass bnweight.Normal.
func GetKlinesRangePriority(symbol string, timeframe string, start, end time.Time, prio bnweight.Priority) ([]Kline, error) {
	symbol = Normalize(symbol)
	normTF, err := NormalizeTimeframe(timeframe)
	if err != nil {
		return nil, err
	}
	if !end.After(start) {
		return nil, fmt.Errorf("end time must be after start time")
	}

	startMs := start.UnixMilli()
	endMs := end.UnixMilli()

	var all []Kline
	cursor := startMs

	barMs := TimeframeDuration(normTF).Milliseconds()
	client := &http.Client{Timeout: 15 * time.Second, Transport: pubcache.Wrap(bnweight.Wrap(nil, prio))}

	for cursor < endMs {
		req, err := http.NewRequest("GET", binanceFuturesKlinesURL, nil)
		if err != nil {
			return nil, err
		}

		q := req.URL.Query()
		q.Set("symbol", symbol)
		q.Set("interval", normTF)
		limit := klinePageLimit(cursor, endMs, barMs)
		q.Set("limit", fmt.Sprintf("%d", limit))
		q.Set("startTime", fmt.Sprintf("%d", cursor))
		q.Set("endTime", fmt.Sprintf("%d", endMs))
		req.URL.RawQuery = q.Encode()

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}

		body, err := security.ReadResponseBody(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			apiErr := fmt.Errorf("binance klines api returned status %d: %s", resp.StatusCode, string(body))
			var rejection struct {
				Code int `json:"code"`
			}
			if json.Unmarshal(body, &rejection) == nil && rejection.Code == -1121 {
				return nil, &invalidSymbolError{message: apiErr.Error()}
			}
			return nil, apiErr
		}

		var raw [][]interface{}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			break
		}

		batch := make([]Kline, len(raw))
		for i, item := range raw {
			kline, err := parseHistoricalKline(item)
			if err != nil {
				return nil, fmt.Errorf("invalid kline %d: %w", i, err)
			}
			batch[i] = kline
		}

		if len(all)+len(batch) > 1000000 {
			return nil, fmt.Errorf("historical kline range exceeds safety limit")
		}
		all = append(all, batch...)

		last := batch[len(batch)-1]
		if last.CloseTime+1 <= cursor {
			return nil, fmt.Errorf("upstream kline cursor did not advance")
		}
		cursor = last.CloseTime + 1

		// If returned quantity is less than request limit, reached the end, can exit early.
		if len(batch) < limit {
			break
		}
	}

	return all, nil
}
