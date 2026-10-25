package market

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func setupOIRisk(t *testing.T) *time.Time {
	oldNow, oldFetch := oiNow, oiFetch
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	oiNow = func() time.Time { return now }
	oiDataMu.Lock()
	oiSamples = map[string]*oiRing{}
	oiDataCache = map[string]oiDataEntry{}
	oiPrices = map[string]oiPrice{}
	oiDataMu.Unlock()
	t.Cleanup(func() {
		oiNow, oiFetch = oldNow, oldFetch
		oiDataMu.Lock()
		oiSamples = map[string]*oiRing{}
		oiDataCache = map[string]oiDataEntry{}
		oiPrices = map[string]oiPrice{}
		oiDataMu.Unlock()
	})
	return &now
}
func TestOIRiskSamplingAndMissingHistory(t *testing.T) {
	now := setupOIRisk(t)
	value := 100.0
	oiFetch = func(string) (*OIData, error) { return &OIData{Latest: value}, nil }
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
	*now = now.Add(4 * time.Minute)
	if _, ok := CachedOIChange1hPct("XUSDT"); ok {
		t.Fatal("stale sample passed")
	}
	oiFetch = func(string) (*OIData, error) { return nil, errors.New("offline") }
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
