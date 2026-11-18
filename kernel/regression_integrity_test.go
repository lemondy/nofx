package kernel

import (
	"context"
	"fmt"
	"math"
	"nofx/market"
	"testing"
	"time"
)

func TestClosedKlinesRemovesAllFutureBars(t *testing.T) {
	now := time.Now()
	td := &market.TimeframeSeriesData{Klines: []market.KlineBar{{Time: now.Add(-2 * time.Hour).UnixMilli()}, {Time: now.UnixMilli()}, {Time: now.Add(time.Hour).UnixMilli()}}}
	if n := len(ClosedKlines(td, now, time.Hour)); n != 1 {
		t.Fatalf("got %d closed bars", n)
	}
}
func TestBTCCacheExpiresAcrossBoundary(t *testing.T) {
	oldNow, oldGet := binanceNow, binanceGetFn
	btcClosesMu.Lock()
	oldCache, oldFetched := btcClosesCache, btcClosesFetched
	btcClosesCache = []float64{1, 2}
	btcClosesFetched = time.Now()
	btcClosesMu.Unlock()
	defer func() {
		binanceNow, binanceGetFn = oldNow, oldGet
		btcClosesMu.Lock()
		btcClosesCache, btcClosesFetched = oldCache, oldFetched
		btcClosesMu.Unlock()
	}()
	binanceNow = func() time.Time { return btcClosesFetched.Truncate(time.Hour).Add(time.Hour + time.Minute) }
	called := false
	binanceGetFn = func(_ context.Context, _ string, out interface{}) error {
		called = true
		return fmt.Errorf("test unavailable")
	}
	binanceBTC1hCloses(2)
	if !called {
		t.Fatal("old hour cache survived boundary")
	}
}

func TestBTCCorrelationAlignsReturnIntervals(t *testing.T) {
	closes := make([]float64, 74)
	times := make([]int64, 74)
	closes[0] = 100
	for i := range closes {
		times[i] = int64(i+1) * 3600000
		if i > 0 {
			closes[i] = closes[i-1] * (1 + 0.001*float64(i%7-3))
		}
	}
	got := btcCorrelation(closes[1:], closes[:73], times[1:], times[:73])
	if got == nil || math.Abs(got.Pearson-1) > 1e-9 || math.Abs(got.Beta-1) > 1e-9 {
		t.Fatalf("misaligned correlation: %+v", got)
	}
	fallback := btcCorrelation(closes, closes)
	if fallback == nil || math.Abs(fallback.Beta-1) > 1e-9 {
		t.Fatal("legacy closes fallback lost")
	}
	if got := btcCorrelation(closes[50:], closes[:24], times[50:], times[:24]); got != nil {
		t.Fatal("nonoverlap produced correlation")
	}
}

func TestPrimaryThreeMinuteFallsBackToFive(t *testing.T) {
	now := time.Now()
	data := &market.Data{Symbol: "ETHUSDT", CurrentPrice: 100, TimeframeData: map[string]*market.TimeframeSeriesData{
		"5m": buildTF("5m", now, 80, 100, false), "1h": buildTF("1h", now, 80, 100, false), "4h": buildTF("4h", now, 80, 100, false)}}
	configured := []string{"3m", "1h", "4h"}
	sig, err := ComputeSymbolSignals("ETHUSDT", data, SignalOptions{Now: now, PrimaryTF: "3m", ConfiguredTimeframes: configured})
	if err != nil || sig.PrimaryTF != "5m" || sig.Timeframes["5m"] == nil {
		t.Fatalf("five minute fallback: %+v %v", sig, err)
	}
	if configured[0] != "3m" {
		t.Fatal("mutated caller configuration")
	}
}
