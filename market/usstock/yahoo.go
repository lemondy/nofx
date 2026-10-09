package usstock

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

type yahooResult struct {
	Meta struct {
		RegularMarketPrice float64 `json:"regularMarketPrice"`
		RegularMarketTime  int64   `json:"regularMarketTime"`
	} `json:"meta"`
	Timestamp  []int64 `json:"timestamp"`
	Indicators struct {
		Quote []struct {
			Open   []*float64 `json:"open"`
			High   []*float64 `json:"high"`
			Low    []*float64 `json:"low"`
			Close  []*float64 `json:"close"`
			Volume []*float64 `json:"volume"`
		} `json:"quote"`
		AdjClose []struct {
			Values []*float64 `json:"adjclose"`
		} `json:"adjclose"`
	} `json:"indicators"`
}
type yahooResponse struct {
	Chart struct {
		Result []yahooResult `json:"result"`
		Error  *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}
type yahooCacheEntry struct {
	result yahooResult
	loaded time.Time
}

// The semaphore serializes all Yahoo requests (including retries and quotes),
// while allowing canceled callers to leave the queue promptly.
var yahooGate = make(chan struct{}, 1)
var yahooCache = make(map[string]yahooCacheEntry) // protected by yahooGate
var retryBaseDelay = time.Second                  // test override; retries wait 1s then 2s

func yahooChart(ctx context.Context, ticker, interval, chartRange string, prePost bool, ttl time.Duration) (yahooResult, error) {
	select {
	case yahooGate <- struct{}{}:
	case <-ctx.Done():
		return yahooResult{}, ctx.Err()
	}
	defer func() { <-yahooGate }()
	key := ticker + "/" + interval + "/" + chartRange + "/" + strconv.FormatBool(prePost)
	if entry, ok := yahooCache[key]; ok && nowFunc().Sub(entry.loaded) < ttl {
		return entry.result, nil
	}
	q := url.Values{"interval": {interval}, "range": {chartRange}, "includePrePost": {strconv.FormatBool(prePost)}, "events": {"div,split"}}
	var lastErr error
	for _, host := range []string{yahooBaseURL, yahooFallbackBaseURL} {
		for attempt := 0; attempt < 3; attempt++ {
			var response yahooResponse
			err := fetchJSON(ctx, host, "/v8/finance/chart/"+url.PathEscape(ticker), q, true, &response)
			if err == nil {
				if response.Chart.Error != nil {
					err = fmt.Errorf("Yahoo %s: %s", response.Chart.Error.Code, response.Chart.Error.Description)
				} else if len(response.Chart.Result) != 1 {
					err = fmt.Errorf("Yahoo chart has %d results", len(response.Chart.Result))
				} else {
					result := response.Chart.Result[0]
					yahooCache[key] = yahooCacheEntry{result, nowFunc()}
					return result, nil
				}
			}
			lastErr = err
			if ctx.Err() != nil {
				return yahooResult{}, ctx.Err()
			}
			var status *httpStatusError
			if !errors.As(err, &status) || (status.status != 429 && (status.status < 500 || status.status > 599)) || attempt == 2 {
				break
			}
			timer := time.NewTimer(retryBaseDelay * time.Duration(1<<attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				return yahooResult{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return yahooResult{}, fmt.Errorf("Yahoo chart %s: %w", ticker, lastErr)
}

func yahooParams(timeframe string, need int) (interval, chartRange string, ttl time.Duration) {
	// Range estimates include calendar gaps and a generous margin. Yahoo accepts
	// day ranges for intraday history; keep within its 60d / 730d limits.
	days := 0
	switch timeframe {
	case TF15m:
		interval, ttl = "15m", 5*time.Minute
		days = int(float64(need)/26*1.7) + 7
		if days > 60 {
			days = 60
		}
	case TF1h:
		interval, ttl = "60m", 15*time.Minute
		days = int(float64(need)/7*1.7) + 14
		if days > 730 {
			days = 730
		}
	case TF4h:
		interval, ttl = "60m", 15*time.Minute
		days = int(float64(need)/2*1.7) + 14
		if days > 730 {
			days = 730
		}
	case TF1d:
		interval, ttl = "1d", 6*time.Hour
		chartRange = longRange(float64(need)*1.7 + 30)
	case TF1w:
		interval, ttl = "1wk", 6*time.Hour
		chartRange = longRange(float64(need)*7*1.3 + 30)
	}
	if days > 0 {
		chartRange = strconv.Itoa(days) + "d"
	}
	return
}
func longRange(days float64) string {
	for _, option := range []struct {
		days  float64
		value string
	}{{30, "1mo"}, {90, "3mo"}, {180, "6mo"}, {365, "1y"}, {730, "2y"}, {1825, "5y"}, {3650, "10y"}} {
		if days <= option.days {
			return option.value
		}
	}
	return "max"
}

func yahooBars(ctx context.Context, ticker, timeframe string, need int) ([]Bar, error) {
	interval, chartRange, ttl := yahooParams(timeframe, need)
	result, err := yahooChart(ctx, ticker, interval, chartRange, false, ttl)
	if err != nil {
		return nil, err
	}
	bars := parseYahooBars(result, timeframe == TF1d || timeframe == TF1w)
	now := nowFunc()
	if timeframe == TF4h {
		return aggregate4h(bars, now), nil
	}
	closed := make([]Bar, 0, len(bars))
	for _, bar := range bars {
		switch timeframe {
		case TF1d:
			date, today := bar.OpenTime.In(newYork).Format("2006-01-02"), now.In(newYork).Format("2006-01-02")
			_, close, ok := SessionBounds(bar.OpenTime)
			if date > today || (date == today && (!ok || now.Before(close))) {
				continue
			}
		case TF1w:
			local := now.In(newYork)
			days := (int(local.Weekday()) + 6) % 7
			monday := etTime(local, 0, 0).AddDate(0, 0, -days)
			if !bar.OpenTime.Before(monday) {
				continue
			}
		default:
			_, close, ok := SessionBounds(bar.OpenTime)
			if !ok || SessionAt(bar.OpenTime) != SessionRegular {
				continue
			}
			end := bar.OpenTime.Add(durations[timeframe])
			if end.After(close) {
				end = close
			}
			if end.After(now) {
				continue
			}
		}
		closed = append(closed, bar)
	}
	return closed, nil
}

func parseYahooBars(result yahooResult, adjusted bool) []Bar {
	if len(result.Indicators.Quote) == 0 {
		return nil
	}
	q := result.Indicators.Quote[0]
	byTime := make(map[int64]Bar)
	for i, stamp := range result.Timestamp {
		fields := [][]*float64{q.Open, q.High, q.Low, q.Close, q.Volume}
		valid := true
		for _, field := range fields {
			if i >= len(field) || field[i] == nil {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		scale := 1.0
		if adjusted {
			if len(result.Indicators.AdjClose) == 0 || i >= len(result.Indicators.AdjClose[0].Values) || result.Indicators.AdjClose[0].Values[i] == nil || *q.Close[i] <= 0 {
				continue
			}
			scale = *result.Indicators.AdjClose[0].Values[i] / *q.Close[i]
		}
		byTime[stamp] = Bar{OpenTime: time.Unix(stamp, 0).UTC(), Open: *q.Open[i] * scale, High: *q.High[i] * scale, Low: *q.Low[i] * scale, Close: *q.Close[i] * scale, Volume: *q.Volume[i]}
	}
	return sortedBars(byTime)
}

// Buckets start at 09:30 ET each trading day, then 13:30 ET. The final
// bucket ends at the regular close (including shortened half-day sessions).
func aggregate4h(bars []Bar, now time.Time) []Bar {
	grouped := make(map[int64]Bar)
	for _, bar := range bars {
		open, close, ok := SessionBounds(bar.OpenTime)
		if !ok || bar.OpenTime.Before(open) || !bar.OpenTime.Before(close) {
			continue
		}
		start := open.Add((bar.OpenTime.Sub(open) / (4 * time.Hour)) * (4 * time.Hour))
		end := start.Add(4 * time.Hour)
		if end.After(close) {
			end = close
		}
		if end.After(now) {
			continue
		}
		key := start.Unix()
		current, exists := grouped[key]
		if !exists {
			current = bar
			current.OpenTime = start.UTC()
		} else {
			if bar.High > current.High {
				current.High = bar.High
			}
			if bar.Low < current.Low {
				current.Low = bar.Low
			}
			current.Close = bar.Close
			current.Volume += bar.Volume
		}
		grouped[key] = current
	}
	return sortedBars(grouped)
}
