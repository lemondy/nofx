package market

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupOIRisk(t *testing.T) *time.Time {
	oldNow, oldFetch, oldGet := oiNow, oiFetch, oiGetJSON
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	oiNow = func() time.Time { return now }
	oiDataMu.Lock()
	oiSamples = map[string]*oiRing{}
	oiDataCache = map[string]oiDataEntry{}
	oiPrices = map[string]oiPrice{}
	oiDataMu.Unlock()
	t.Cleanup(func() {
		oiNow, oiFetch, oiGetJSON = oldNow, oldFetch, oldGet
		oiDataMu.Lock()
		oiSamples = map[string]*oiRing{}
		oiDataCache = map[string]oiDataEntry{}
		oiPrices = map[string]oiPrice{}
		oiDataMu.Unlock()
	})
	return &now
}

// fakeOIExchange serves both OI endpoints fetchOIData reads; the real-time
// observation is stamped with *now as the EXCHANGE time (review 2026-10-28 #20:
// only exchange observations sample the risk ring).
func fakeOIExchange(now *time.Time, value *float64) func(string, interface{}) error {
	return func(path string, out interface{}) error {
		var body any
		switch {
		case strings.HasPrefix(path, "/futures/data/openInterestHist"):
			body = []map[string]string{{"sumOpenInterest": fmt.Sprint(*value)}}
		case strings.HasPrefix(path, "/fapi/v1/openInterest"):
			body = map[string]any{"openInterest": fmt.Sprint(*value), "symbol": path[strings.Index(path, "symbol=")+len("symbol="):], "time": now.UnixMilli()}
		default:
			return fmt.Errorf("unexpected path %s", path)
		}
		b, _ := json.Marshal(body)
		return json.Unmarshal(b, out)
	}
}

func TestOIRiskSamplingAndMissingHistory(t *testing.T) {
	now := setupOIRisk(t)
	value := 100.0
	oiGetJSON = fakeOIExchange(now, &value)
	if _, ok := CachedOIChange1hPct("XUSDT"); ok {
		t.Fatal("cold start fabricated history")
	}
	for i := 0; i <= 60; i += 3 {
		if i > 0 {
			*now = now.Add(3 * time.Minute)
		}
		value = 100 + float64(i)*.5
		if _, err := getOpenInterestData("xusdt"); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok := CachedOIChange1hPct("XUSDT"); !ok || math.Abs(v-30) > 1e-8 {
		t.Fatalf("1h change %g %v", v, ok)
	}
	*now = now.Add(7 * time.Minute)
	if _, ok := CachedOIChange1hPct("XUSDT"); ok {
		t.Fatal("stale sample passed")
	}
	oiGetJSON = func(string, interface{}) error { return errors.New("offline") }
	if _, err := getOpenInterestData("XUSDT"); err == nil {
		t.Fatal("failure hidden")
	}
	if _, ok := CachedOIChange1hPct("XUSDT"); ok {
		t.Fatal("failed fetch refreshed history")
	}
}
func TestOIRiskCacheValuationExpiryAndConcurrency(t *testing.T) {
	now := setupOIRisk(t)
	oiFetch = func(string) (*OIData, error) { return &OIData{Latest: 2e6}, nil }
	getOpenInterestData("XUSDT")
	if _, ok := CachedOpenInterestMillions("XUSDT"); ok {
		t.Fatal("missing price must be unknown")
	}
	cacheOIPrice("XUSDT", 10)
	if v, ok := CachedOpenInterestMillions("xusdt"); !ok || v != 20 {
		t.Fatalf("OI value %g %v", v, ok)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				cacheOIPrice("XUSDT", 10)
				CachedOpenInterestMillions("XUSDT")
				CachedOIChange1hPct("XUSDT")
				getOpenInterestData("XUSDT")
			}
		}()
	}
	wg.Wait()
	*now = now.Add(oiDataTTL + time.Second)
	if _, ok := CachedOpenInterestMillions("XUSDT"); ok {
		t.Fatal("stale OI must be unknown")
	}
}
func TestOIRiskRingRetentionAndGap(t *testing.T) {
	now := setupOIRisk(t)
	for i := 0; i <= 960; i++ {
		oiDataMu.Lock()
		recordOISampleLocked("XUSDT", *now, 100)
		oiDataMu.Unlock()
		*now = now.Add(15 * time.Second)
	}
	oiDataMu.Lock()
	ring := oiSamples["XUSDT"]
	samples := make([]oiSample, len(ring.samples))
	for i := range samples {
		samples[len(samples)-1-i] = ring.newest(i)
	}
	oiDataMu.Unlock()
	if len(samples) > 721 || now.Sub(samples[0].at) < 2*time.Hour {
		t.Fatalf("ring retention len=%d age=%s", len(samples), now.Sub(samples[0].at))
	}
	if _, ok := CachedOIChange1hPct("XUSDT"); !ok {
		t.Fatal("continuous history missing")
	}
	oiDataMu.Lock()
	oiSamples["XUSDT"] = &oiRing{samples: []oiSample{{now.Add(-90 * time.Minute), 100}, {*now, 130}}, next: 2}
	oiDataMu.Unlock()
	if _, ok := CachedOIChange1hPct("XUSDT"); ok {
		t.Fatal("90m gap mislabeled 1h")
	}
}

func TestOIRiskZeroIsARealCollapse(t *testing.T) {
	now := setupOIRisk(t)
	oiDataMu.Lock()
	recordOISampleLocked("XUSDT", now.Add(-time.Hour), 100)
	recordOISampleLocked("XUSDT", *now, 0)
	oiDataMu.Unlock()
	if v, ok := CachedOIChange1hPct("XUSDT"); !ok || v != -100 {
		t.Fatalf("zero OI collapse lost: %g %v", v, ok)
	}
}

func TestOIRiskShortWindowsSampleTolerance(t *testing.T) {
	now := setupOIRisk(t)
	for _, window := range []time.Duration{15 * time.Minute, time.Hour} {
		tolerance := max(time.Duration(float64(window)*.35), 5*time.Minute)
		for _, tc := range []struct {
			name                 string
			refOffset, latestAge time.Duration
			ok                   bool
		}{
			{"exact", 0, 0, true},
			{"tolerance-edge", tolerance, 0, true},
			{"too-old-reference", tolerance + time.Nanosecond, 0, false},
			{"reference-after-target", -time.Nanosecond, 0, false},
			{"latest-six-minutes", 0, 6 * time.Minute, true},
			{"latest-too-old", 0, 6*time.Minute + time.Nanosecond, false},
			{"latest-future", 0, -time.Nanosecond, false},
		} {
			t.Run(window.String()+"/"+tc.name, func(t *testing.T) {
				oiDataMu.Lock()
				oiSamples["WINDOWUSDT"] = &oiRing{samples: []oiSample{
					{now.Add(-window - tc.refOffset), 100},
					{now.Add(-tc.latestAge), 120},
				}, next: 0}
				oiDataMu.Unlock()
				v, ok := CachedOIChangePct("windowusdt", window)
				if ok != tc.ok || (ok && math.Abs(v-20) > 1e-8) {
					t.Fatalf("change=%g valid=%v want %v", v, ok, tc.ok)
				}
			})
		}
	}
	if _, ok := CachedOIChange15mPct("MISSINGUSDT"); ok {
		t.Fatal("missing 15m history passed")
	}
	if _, ok := CachedOIChange1hPct("MISSINGUSDT"); ok {
		t.Fatal("missing 1h history passed")
	}
	if _, ok := CachedOIChangePct("WINDOWUSDT", 0); ok {
		t.Fatal("zero window passed")
	}
}

func TestOIRiskTargetsNowAndChoosesClosestPriorSample(t *testing.T) {
	now := setupOIRisk(t)
	oiDataMu.Lock()
	for _, s := range []oiSample{
		{now.Add(-time.Hour), 50},
		{now.Add(-20 * time.Minute), 80},
		{now.Add(-15 * time.Minute), 100},
		{now.Add(-14 * time.Minute), 200}, // after target; must be skipped
		{now.Add(-5 * time.Minute), 110},  // latest is cached, not now
	} {
		recordOISampleLocked("XUSDT", s.at, s.value)
	}
	oiDataMu.Unlock()
	if v, ok := CachedOIChange15mPct("XUSDT"); !ok || math.Abs(v-10) > 1e-8 {
		t.Fatalf("15m uses now target: %g %v", v, ok)
	}
	if v, ok := CachedOIChange1hPct("XUSDT"); !ok || math.Abs(v-120) > 1e-8 {
		t.Fatalf("1h uses now target: %g %v", v, ok)
	}
}

func TestOIRiskCacheHitDoesNotSampleAndUnknownReference(t *testing.T) {
	now := setupOIRisk(t)
	value := 100.0
	oiGetJSON = fakeOIExchange(now, &value)
	for i := 0; i < 3; i++ {
		if _, err := getOpenInterestData("CACHEUSDT"); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
	}
	oiDataMu.Lock()
	count := len(oiSamples["CACHEUSDT"].samples)
	oiDataMu.Unlock()
	if count != 1 {
		t.Fatal("cache hit fabricated observations", count)
	}
	for _, ref := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		oiDataMu.Lock()
		oiSamples["BADUSDT"] = &oiRing{samples: []oiSample{{now.Add(-15 * time.Minute), ref}, {*now, 100}}, next: 0}
		oiDataMu.Unlock()
		if _, ok := CachedOIChange15mPct("BADUSDT"); ok {
			t.Fatal("invalid denominator passed", ref)
		}
	}
}
