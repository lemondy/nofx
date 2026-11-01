package market

import (
	"math"
	"os"
	"sync"
	"testing"
	"time"
)

// LiquidationStats starts a persistent websocket lazily, including in the
// pre-existing aggregator test. Keep every market test offline; coverage
// tests drive the same connection transitions directly with a fake clock.
func TestMain(m *testing.M) {
	liqRunning = true
	os.Exit(m.Run())
}

func review20261029LiquidationClock(t *testing.T) *time.Time {
	t.Helper()
	liqMu.Lock()
	oldNow, oldStarted, oldRunning := liqNow, liqStarted, liqRunning
	oldCoverage, oldEvents, oldBySymbol := liqCoverage, liqEvents, liqBySymbol
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	liqNow = func() time.Time { return now }
	liqStarted, liqCoverage = time.Time{}, liqCoverageState{}
	liqEvents, liqBySymbol = nil, map[string][]liqEvent{}
	liqRunning = true
	liqMu.Unlock()
	t.Cleanup(func() {
		liqMu.Lock()
		liqNow, liqStarted, liqRunning = oldNow, oldStarted, oldRunning
		liqCoverage, liqEvents, liqBySymbol = oldCoverage, oldEvents, oldBySymbol
		liqMu.Unlock()
	})
	return &now
}

func review20261029AssertCoverage(t *testing.T, w LiquidationWindow, online, span float64, gaps int, complete bool) {
	t.Helper()
	if math.Abs(w.WindowHours-online) > 1e-9 || math.Abs(w.SpanHours-span) > 1e-9 || w.GapCount != gaps || w.Complete != complete {
		t.Fatalf("coverage online=%g span=%g gaps=%d complete=%v; want %g/%g, %d, %v",
			w.WindowHours, w.SpanHours, w.GapCount, w.Complete, online, span, gaps, complete)
	}
}

func TestReview20261029LiquidationNeverConnected(t *testing.T) {
	now := review20261029LiquidationClock(t)
	*now = now.Add(30 * time.Hour)
	StartLiquidationFeed() // already launched, but no successful connection
	liqSetConnected(false)
	w, ok := LiquidationStats("BTCUSDT")
	if ok {
		t.Fatal("no liquidation events must still report absent")
	}
	review20261029AssertCoverage(t, w, 0, 0, 0, false)
	if !liqStarted.IsZero() {
		t.Fatal("startup/failed connection started the observation span")
	}
}

func TestReview20261029LiquidationReconnectCoverage(t *testing.T) {
	now := review20261029LiquidationClock(t)
	*now = now.Add(time.Hour) // startup/initial dialing is not coverage
	liqSetConnected(true)
	*now = now.Add(2 * time.Hour)
	liqSetConnected(true) // duplicate notification must not reset the start
	liqSetConnected(false)
	*now = now.Add(30 * time.Minute)
	liqSetConnected(false) // failed retry is not another disconnection
	liqSetConnected(true)
	*now = now.Add(time.Hour)
	recordLiquidation("BTCUSDT", liqEvent{ts: *now, longLiq: true, notional: 123})
	w, ok := LiquidationStats("btcusdt")
	if !ok || w.Count24h != 1 || w.Long24hUSD != 123 {
		t.Fatalf("coverage changed event statistics: %+v, ok=%v", w, ok)
	}
	review20261029AssertCoverage(t, w, 3, 3.5, 1, false)
}

func TestReview20261029LiquidationOngoingOutage(t *testing.T) {
	now := review20261029LiquidationClock(t)
	liqSetConnected(true)
	*now = now.Add(2 * time.Hour)
	liqSetConnected(false)
	*now = now.Add(30 * time.Minute)
	w, _ := LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 2, 2.5, 1, false)
	*now = now.Add(time.Hour)
	w, _ = LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 2, 3.5, 1, false)

	// Once the disconnection timestamp leaves the window, its count is zero,
	// but the ongoing outage must never become a complete 24h observation.
	*now = now.Add(25 * time.Hour)
	w, _ = LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 0, 24, 0, false)
	if liqCoverage.count != 0 {
		t.Fatal("expired intervals were retained")
	}
}

func TestReview20261029LiquidationRollingCutoff(t *testing.T) {
	now := review20261029LiquidationClock(t)
	liqSetConnected(true)
	*now = now.Add(2 * time.Hour)
	liqSetConnected(false)
	*now = now.Add(time.Hour)
	liqSetConnected(true)
	*now = now.Add(2 * time.Hour)
	liqSetConnected(false)
	*now = now.Add(time.Hour)
	liqSetConnected(true)

	// At hour 28 the [0,2] interval is expired, [3,5] clips to [4,5],
	// and the ongoing [6,28] interval contributes 22 more hours.
	*now = now.Add(22 * time.Hour)
	w, _ := LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 23, 24, 1, false)
	if liqCoverage.count != 1 || !liqCoverage.intervals[liqCoverage.head].start.Equal(now.Add(-liqWindow)) {
		t.Fatal("old intervals were not expired/clipped to the 24h cutoff")
	}

	// A disconnection exactly on the inclusive left boundary still counts.
	*now = now.Add(time.Hour)
	w, _ = LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 23, 24, 1, false)
	*now = now.Add(time.Hour)
	w, _ = LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 24, 24, 0, true)
}

func TestReview20261029LiquidationContinuousCoverage(t *testing.T) {
	now := review20261029LiquidationClock(t)
	liqSetConnected(true)
	*now = now.Add(23 * time.Hour)
	w, _ := LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 23, 23, 0, false)
	*now = now.Add(time.Hour)
	w, _ = LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 24, 24, 0, true)
	*now = now.Add(7 * time.Hour)
	w, _ = LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 24, 24, 0, true)
	liqSetConnected(false)
	w, _ = LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 24, 24, 1, false)
}

func TestReview20261029LiquidationCoverageRing(t *testing.T) {
	now := review20261029LiquidationClock(t)
	// Exercise more than a full ring, with the feed's minimum retry delay.
	// No gap is merged into covered time even under frequent reconnects.
	for i := 0; i < 2*liqMaxCoverageIntervals; i++ {
		liqSetConnected(true)
		*now = now.Add(time.Second)
		liqSetConnected(false)
		*now = now.Add(liqReconnectMin)
		if liqCoverage.count > liqMaxCoverageIntervals {
			t.Fatal("coverage storage exceeded its bound")
		}
	}
	w, _ := LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 4, 24, 14400, false)
}

func TestReview20261029LiquidationConcurrentStatsAndTransitions(t *testing.T) {
	now := review20261029LiquidationClock(t)
	liqSetConnected(true)
	*now = now.Add(time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				LiquidationStats("BTCUSDT")
			}
		}()
	}
	liqSetConnected(false)
	liqSetConnected(true)
	wg.Wait()
	w, _ := LiquidationStats("BTCUSDT")
	review20261029AssertCoverage(t, w, 1, 1, 1, false)
}
