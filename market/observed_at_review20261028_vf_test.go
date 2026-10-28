package market

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
)

func TestReview20261028VFRequestObservation(t *testing.T) {
	for _, execution := range []bool{false, true} {
		t.Run(strconv.FormatBool(execution), func(t *testing.T) {
			symbol := "VFOBS" + strconv.FormatInt(time.Now().UnixNano(), 10) + "USDT"
			// Keep every dependency local: no HTTP or exchange calls.
			fundingRateMap.Store(symbol, &FundingRateCache{UpdatedAt: time.Now()})
			fundingIntervalMap.Store(symbol, &FundingIntervalCache{Hours: 8, UpdatedAt: time.Now()})
			fundingHistoryMap.Store(symbol, &fundingHistoryCache{UpdatedAt: time.Now()})
			t.Cleanup(func() {
				fundingRateMap.Delete(symbol)
				fundingIntervalMap.Delete(symbol)
				fundingHistoryMap.Delete(symbol)
			})
			oldOI := oiFetch
			oiFetch = func(string) (*OIData, error) { return &OIData{}, nil }
			t.Cleanup(func() { oiFetch = oldOI })
			started := map[string]time.Time{}
			var boundary time.Time
			fake := func(_ string, tf, _ string, _ int) ([]Kline, error) {
				started[tf] = time.Now()
				if tf == "1m" {
					// A synthetic bar boundary keeps this regression fast and
					// deterministic instead of waiting for a real minute boundary.
					boundary = time.Now().Add(20 * time.Millisecond).Truncate(time.Millisecond)
				} else {
					time.Sleep(time.Until(boundary.Add(10 * time.Millisecond)))
				}
				dur := TimeframeDuration(tf)
				lastOpen := boundary.Add(-dur)
				bars := make([]Kline, 200)
				for i := range bars {
					p := 100 + float64(i%7)
					open := lastOpen.Add(-time.Duration(199-i) * dur)
					bars[i] = Kline{OpenTime: open.UnixMilli(), CloseTime: open.Add(dur).UnixMilli() - 1,
						Open: p, High: p + 1, Low: p - 1, Close: p, Volume: 1}
				}
				return bars, nil
			}
			patch := gomonkey.NewPatches()
			defer patch.Reset()
			patch.ApplyFunc(getKlinesWithFallback, fake)
			patch.ApplyFunc(getExecutionKlines, fake)
			before := time.Now()
			data, err := getWithTimeframes(symbol, []string{"1m", "4h"}, "1m", 60, "binance", 103, execution)
			if err != nil {
				t.Fatal(err)
			}
			if !data.SampledAt.After(boundary) {
				t.Fatal("fixture did not cross the first timeframe's close boundary")
			}
			for _, tf := range []string{"1m", "4h"} {
				field := reflect.ValueOf(data.TimeframeData[tf]).Elem().FieldByName("ObservedAt")
				if !field.IsValid() {
					t.Errorf("%s has no request observation timestamp", tf)
					continue
				}
				at := field.Interface().(time.Time)
				if at.Before(before) || at.After(started[tf]) {
					t.Errorf("%s ObservedAt=%v must be recorded before request start %v", tf, at, started[tf])
				}
				if tf == "1m" && !at.Before(boundary) {
					t.Errorf("first series was promoted to closed after later fetch: %v >= %v", at, boundary)
				}
				blob, err := json.Marshal(data.TimeframeData[tf])
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(blob, &fields)
				if _, ok := fields["ObservedAt"]; ok {
					t.Fatal("ObservedAt leaked into JSON")
				}
			}
		})
	}
}
