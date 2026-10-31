package market

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func setupOIReview20261028(t *testing.T) *time.Time {
	t.Helper()
	now := setupOIRisk(t)
	oldGet := oiGetJSON
	t.Cleanup(func() { oiGetJSON = oldGet })
	oiFetch = fetchOIData
	return now
}

func TestReview20261028OIExchangeSamples(t *testing.T) {
	now := setupOIReview20261028(t)
	start := *now
	sourceAt, value := start, 100.0
	oiGetJSON = func(path string, out interface{}) error {
		if strings.HasPrefix(path, "/futures/data/openInterestHist?") {
			// The hourly observation never updates during the entire scenario.
			return json.Unmarshal([]byte(`[{"sumOpenInterest":"100"}]`), out)
		}
		if !strings.HasPrefix(path, "/fapi/v1/openInterest?symbol=XUSDT") {
			return fmt.Errorf("unexpected endpoint %s", path)
		}
		body := fmt.Sprintf(`{"openInterest":%q,"symbol":"XUSDT","time":%d}`, fmt.Sprint(value), sourceAt.UnixMilli())
		return json.Unmarshal([]byte(body), out)
	}
	fetch := func() {
		t.Helper()
		d, err := getOpenInterestData("xusdt")
		if err != nil || d.Latest != value || d.Average != 100 {
			t.Fatalf("OI=%+v err=%v", d, err)
		}
	}
	fetch()
	*now = now.Add(3 * time.Minute)
	fetch() // parsed cache expired, but the exchange timestamp is unchanged
	if got := len(oiSamples["XUSDT"].samples); got != 1 {
		t.Fatalf("duplicate source time produced %d samples", got)
	}
	for i := 2; i <= 10; i++ {
		sourceAt = start.Add(time.Duration(i) * 3 * time.Minute)
		*now = sourceAt.Add(time.Second) // received later than the exchange time
		if i > 5 {
			value = 100 + float64(i-5)*10
		}
		fetch()
	}
	if got := oiSamples["XUSDT"].newest(0).at; !got.Equal(sourceAt) {
		t.Fatalf("sample time=%v want exchange time=%v", got, sourceAt)
	}
	if change, ok := CachedOIChange15mPct("XUSDT"); !ok || math.Abs(change-50) > 1e-9 {
		t.Fatalf("hourly OI stayed 100, real OI reached 150: change=%g known=%v", change, ok)
	}
	// A later local fetch of the same exchange observation cannot freshen it.
	*now = sourceAt.Add(7 * time.Minute)
	fetch()
	if _, ok := CachedOIChange15mPct("XUSDT"); ok {
		t.Fatal("stale exchange timestamp was refreshed by local fetch time")
	}
}

func TestReview20261028OIRejectsDuplicateAndOlderSourceTimes(t *testing.T) {
	now := setupOIReview20261028(t)
	oiDataMu.Lock()
	recordOISampleLocked("XUSDT", *now, 100)
	recordOISampleLocked("XUSDT", *now, 150)
	recordOISampleLocked("XUSDT", now.Add(-time.Hour), 200)
	ring := oiSamples["XUSDT"]
	oiDataMu.Unlock()
	if len(ring.samples) != 1 || ring.newest(0).value != 100 {
		t.Fatalf("duplicate/out-of-order source observations changed ring: %+v", ring)
	}
}

func TestReview20261028OIMissingAndGapUnknown(t *testing.T) {
	now := setupOIReview20261028(t)
	if _, ok := CachedOIChange15mPct("XUSDT"); ok {
		t.Fatal("missing history reported known")
	}
	oiDataMu.Lock()
	recordOISampleLocked("XUSDT", now.Add(-30*time.Minute), 100)
	recordOISampleLocked("XUSDT", *now, 150)
	oiDataMu.Unlock()
	if _, ok := CachedOIChange15mPct("XUSDT"); ok {
		t.Fatal("reference outside source-time tolerance reported known")
	}
}

func TestReview20261028OIRealtimeFailureKeepsHourlyData(t *testing.T) {
	setupOIReview20261028(t)
	for _, response := range []string{"offline", `{}`, `{"symbol":"OTHERUSDT","openInterest":"150","time":123}`, `{"symbol":"XUSDT","openInterest":"NaN","time":123}`, `{"symbol":"XUSDT","openInterest":"150","time":0}`} {
		t.Run(response, func(t *testing.T) {
			oiGetJSON = func(path string, out interface{}) error {
				if strings.HasPrefix(path, "/futures/data/") {
					return json.Unmarshal([]byte(`[{"sumOpenInterest":"80"},{"sumOpenInterest":"100"}]`), out)
				}
				if response == "offline" {
					return errors.New("offline")
				}
				return json.Unmarshal([]byte(response), out)
			}
			d, err := fetchOIData("XUSDT")
			if err != nil || d.Latest != 100 || d.Average != 90 {
				t.Fatalf("hourly fallback=%+v err=%v", d, err)
			}
			if ring := oiSamples["XUSDT"]; ring != nil && len(ring.samples) != 0 {
				t.Fatalf("real-time failure fabricated source sample: %+v", ring)
			}
		})
	}
}

func TestReview20261028Rolling4hScenarioTwo(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 1, 0, 0, time.UTC)
	target := now.Add(-4 * time.Hour)
	fine := priceHistory{[]Kline{
		{CloseTime: target.Add(-time.Minute).UnixMilli(), Close: 100},
		{CloseTime: target.Add(time.Minute).UnixMilli(), Close: 110}, // after target
		{CloseTime: now.Add(time.Minute).UnixMilli(), Open: 110, Close: 111},
	}, 3 * time.Minute}
	coarse := priceHistory{[]Kline{{CloseTime: target.Add(-time.Minute).UnixMilli(), Close: 90}}, time.Hour}
	got := rolling4hPriceChange("SCENARIOUSDT", now, 111, (111.0/110-1)*100, coarse, fine)
	if math.Abs(got-11) > 1e-9 {
		t.Fatalf("rolling 4h=%g want 11%% (former current-bar return 0.91%%)", got)
	}
}

func TestReview20261028Rolling4hCoarserAndLegacyFallback(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 1, 0, 0, time.UTC)
	target := now.Add(-4 * time.Hour)
	short := priceHistory{[]Kline{{CloseTime: target.Add(time.Minute).UnixMilli(), Close: 110}}, 3 * time.Minute}
	for _, bar := range []time.Duration{15 * time.Minute, time.Hour} {
		coarse := priceHistory{[]Kline{{CloseTime: target.Add(-time.Minute).UnixMilli(), Close: 100}}, bar}
		if got := rolling4hPriceChange("FALLBACKUSDT", now, 111, .91, short, coarse); math.Abs(got-11) > 1e-9 {
			t.Fatalf("%s fallback=%g want 11", bar, got)
		}
	}
	stale := priceHistory{[]Kline{{CloseTime: target.Add(-3*time.Minute - time.Millisecond).UnixMilli(), Close: 100}}, 3 * time.Minute}
	priceChangeFallbackLog.Lock()
	oldLog := priceChangeFallbackLog.at
	priceChangeFallbackLog.at = map[string]time.Time{}
	priceChangeFallbackLog.Unlock()
	t.Cleanup(func() {
		priceChangeFallbackLog.Lock()
		priceChangeFallbackLog.at = oldLog
		priceChangeFallbackLog.Unlock()
	})
	for _, elapsed := range []time.Duration{0, 30 * time.Minute, time.Hour} {
		if got := rolling4hPriceChange("FALLBACKUSDT", now.Add(elapsed), 111, .91, short, stale); got != .91 {
			t.Fatalf("short history fabricated rolling value %g", got)
		}
		wantLogAt := now
		if elapsed == time.Hour {
			wantLogAt = now.Add(time.Hour)
		}
		if got := priceChangeFallbackLog.at["FALLBACKUSDT"]; !got.Equal(wantLogAt) {
			t.Fatalf("fallback logging time=%v want %v", got, wantLogAt)
		}
	}
}
