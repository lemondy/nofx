package kernel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/market"
)

// ── review 2026-10-06 batch-3: the remaining P2/P3 items ──

// P2-1: bb_ride on a LAGGY feed — the newest bar's 15m window has already
// ended (it is CLOSED) but the API response still carries it. Time-based
// settlement must NOT drop it (the old unconditional [:len-1] did), because
// bbRide licenses MARKET orders via marketExceptionEvidence.
func TestFix3BBRideKeepsClosedTailOnLaggyFeed(t *testing.T) {
	// 30 bars (25 flat + 5 ride), ALL closed: shift the whole series back one
	// slot from the builder's forming-tail shape.
	bars := bbRideSeries(30, 5)
	for i := range bars {
		bars[i].Time -= 15 * 60 * 1000
	}
	data := &market.Data{Symbol: "T", TimeframeData: map[string]*market.TimeframeSeriesData{
		"15m": {Klines: bars},
	}}
	r := computeBBRide(data, time.Now())
	if r == nil || r.Windows != 5 {
		t.Fatalf("laggy feed: closed ride tail must be counted (windows=5), got %+v", r)
	}
	if !r.Ride {
		t.Fatal("laggy feed must not kill a live ride signal")
	}
}

// P2-2: the opt-in BTC hard pause must fail CLOSED on unknown BTC data —
// missing evidence cannot demonstrate "not strong bull" — aligning the
// kernel gate with the executor's pending recompute. The long side keeps its
// documented fail-open.
func TestFix3BTCRegimeUnknownGate(t *testing.T) {
	shortBTCData := make([]float64, 50) // below the 60-close convergence bar

	// Opt-in short pause + unknown data → BTC_REGIME_UNKNOWN.
	sig := lpSig(100, 99, 105)
	opt := lpOpt()
	opt.BTCFilterShort = true
	opt.BtcTrendCloses = shortBTCData
	g := computeHardEntryGate(sig, opt)
	if !hasCode(g.Short, "BTC_REGIME_UNKNOWN") {
		t.Fatalf("opt-in short pause with unknown BTC data must fail closed, failed=%v", g.Short.Failed)
	}

	// Gate NOT enabled → the unknown must not fire (no new default blocking).
	opt2 := lpOpt()
	opt2.BtcTrendCloses = shortBTCData
	g2 := computeHardEntryGate(lpSig(100, 99, 105), opt2)
	if hasCode(g2.Short, "BTC_REGIME_UNKNOWN") || hasCode(g2.Short, "BTC_4H_STRONGBULL") {
		t.Fatalf("disabled filter must not emit BTC codes, failed=%v", g2.Short.Failed)
	}

	// Long side keeps its documented fail-open on the same data.
	g3 := computeHardEntryGate(lpSig(100, 101, 95), opt2)
	for _, c := range g3.Long.Failed {
		if strings.HasPrefix(c, "BTC_") {
			t.Fatalf("long side must stay fail-open on unknown BTC data, failed=%v", g3.Long.Failed)
		}
	}
}

// P2-5: BTC's 24h must come from the SAME live ticker the coin side uses.
// Regression of the direction: BTC live +5% vs closed -11.3%, coin live +3%
// → the coin WEAKENED against live BTC and must be banned (the old
// closed-vs-live comparison passed it).
func TestFix3BTCWeakLongUsesLive24h(t *testing.T) {
	old := binanceGetFn
	defer func() { binanceGetFn = old }()
	binanceGetFn = func(_ context.Context, _ string, out interface{}) error {
		return json.Unmarshal([]byte(`[{"symbol":"BTCUSDT","lastPrice":"105","priceChangePercent":"5","quoteVolume":"1e10"}]`), out)
	}
	tickersCache = nil
	tickersFetched = time.Time{}

	declining := make([]float64, 80)
	base := 100.0
	for i := range declining {
		declining[i] = base
		base *= 0.995
	}
	sig := lpSig(100, 110, 95)
	sig.Derivatives = &DerivSignal{}
	coin24 := 3.0
	sig.Derivatives.PriceChange24hLivePct = &coin24
	opt := lpOpt()
	opt.BtcTrendCloses = declining

	g := computeHardEntryGate(sig, opt)
	if !hasCode(g.Long, "BTC_WEAK_LONG_3.0_VS_5.0") {
		t.Fatalf("coin +3%% vs LIVE BTC +5%% must be banned against the live figure, failed=%v", g.Long.Failed)
	}
}

// P2-6 (market layer lives in kernel here): concurrent callers of
// binanceBTC4hCloses must share ONE upstream fetch — the old code fetched
// outside the lock with no singleflight: cold-start losers got nil, warm
// expiry fired N requests.
func TestFix3BTC4hClosesSingleflight(t *testing.T) {
	old := binanceGetFn
	defer func() { binanceGetFn = old }()

	var calls int32
	candles := make([][]interface{}, 0, 301)
	for i := 0; i < 301; i++ {
		candles = append(candles, []interface{}{"0", "100", "101", "99", "100.5", "0", "0", float64(i), "0", "0", "0", "0"})
	}
	body, _ := json.Marshal(candles)
	binanceGetFn = func(_ context.Context, path string, out interface{}) error {
		if strings.Contains(path, "symbol=BTCUSDT") {
			atomic.AddInt32(&calls, 1)
			time.Sleep(150 * time.Millisecond) // widen the race window
		}
		return json.Unmarshal(body, out)
	}

	btcClosesMu.Lock()
	btc4hClosesCache = nil
	btc4hLastAttempt = time.Time{}
	btc4hInflight = nil
	btcClosesMu.Unlock()

	var wg sync.WaitGroup
	results := make([][]float64, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = binanceBTC4hCloses(300)
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("concurrent cold-start callers hit Binance %d times, want exactly 1", got)
	}
	for i, r := range results {
		if len(r) != 300 {
			t.Fatalf("caller %d got %d closes, want 300 (nil losers = the old race)", i, len(r))
		}
	}
}
