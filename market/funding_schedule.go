package market

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	fundingScheduleTTL        = time.Hour
	fundingScheduleMaxAge     = 6 * time.Hour
	fundingScheduleRetryEvery = time.Minute
)

var (
	fundingScheduleCache struct {
		sync.Mutex
		hours      map[string]float64
		fetchedAt  time.Time
		failedAt   time.Time
		refreshing bool
	}
	// fundingScheduleFetch is a test seam; production fetches the whole table.
	fundingScheduleFetch = fetchFundingSchedule
)

func fetchFundingSchedule() (map[string]float64, error) {
	resp, err := NewAPIClient().client.Get(binanceFAPIBase + "/fapi/v1/fundingInfo")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("funding schedule: HTTP %d", resp.StatusCode)
	}
	var raw []struct {
		Symbol string  `json:"symbol"`
		Hours  float64 `json:"fundingIntervalHours"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty funding schedule")
	}
	hours := make(map[string]float64, len(raw))
	for _, row := range raw {
		if row.Symbol != "" && row.Hours > 0 && row.Hours <= 24 {
			hours[Normalize(row.Symbol)] = row.Hours
		}
	}
	return hours, nil
}

// FundingScheduleHours looks up the normalized symbol in Binance's current
// schedule. Binance only lists adjusted symbols; absence means unknown, not
// an assumed eight-hour interval. Refresh failures back off for one minute
// and may serve the last successful table only while it is under six hours old.
func FundingScheduleHours(symbol string) (hours float64, ok bool) {
	symbol = Normalize(symbol)
	fundingScheduleCache.Lock()
	// One caller refreshes the whole table outside the lock; concurrent
	// callers read the current table instead of waiting on the HTTP timeout.
	now := time.Now()
	if !fundingScheduleCache.refreshing &&
		(fundingScheduleCache.hours == nil || now.Sub(fundingScheduleCache.fetchedAt) >= fundingScheduleTTL) &&
		(fundingScheduleCache.failedAt.IsZero() || now.Sub(fundingScheduleCache.failedAt) >= fundingScheduleRetryEvery) {
		fundingScheduleCache.refreshing = true
		fetch := fundingScheduleFetch
		fundingScheduleCache.Unlock()
		next, err := fetch()
		fundingScheduleCache.Lock()
		fundingScheduleCache.refreshing = false
		if err != nil || len(next) == 0 {
			fundingScheduleCache.failedAt = time.Now()
		} else {
			fundingScheduleCache.hours = next
			fundingScheduleCache.fetchedAt = time.Now()
			fundingScheduleCache.failedAt = time.Time{}
		}
	}
	defer fundingScheduleCache.Unlock()
	if time.Since(fundingScheduleCache.fetchedAt) >= fundingScheduleMaxAge {
		return 0, false
	}
	hours, ok = fundingScheduleCache.hours[symbol]
	return hours, ok
}

// MedianFundingIntervalHours returns the median positive settlement gap in
// hours, ignoring gaps above 24 hours. Timestamps must be oldest to newest.
// It returns zero when no valid gap exists; unlike exchange metadata, this
// historical fallback can still lag a recent schedule change.
func MedianFundingIntervalHours(times []int64) float64 {
	return medianFundingIntervalHours(times)
}
