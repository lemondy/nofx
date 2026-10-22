package trader

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"nofx/kernel"
	"nofx/market"
	"nofx/store"
	"nofx/trader/types"
)

// Executor-side bar-time staleness (review 2026-10-20 #8 follow-up): paths that
// price NEW risk off klines refuse a stale series; paths that PROTECT an
// existing position never stop protecting.

// freshSeries builds n flat-ish closed bars of tf, the newest opened one period
// ago and then pushed `lag` further into the past (lag 0 = fresh; lag ≥ 3
// periods = stale at the sampling clock). ATR% ≈ 1.
func freshSeries(tf string, n int, price float64, lag time.Duration) *market.TimeframeSeriesData {
	dur := market.TimeframeDuration(tf)
	now := time.Now()
	bars := make([]market.KlineBar, 0, n)
	for i := 0; i < n; i++ {
		ts := now.Add(-time.Duration(n-i)*dur - lag)
		bars = append(bars, market.KlineBar{
			Time: ts.UnixMilli(), Open: price, High: price * 1.005, Low: price * 0.995, Close: price, Volume: 1000,
		})
	}
	return &market.TimeframeSeriesData{Timeframe: tf, Klines: bars, ATR14: price * 0.01}
}

// freshnessData mimics the fetch layer: per-timeframe lags, SampledAt = now and
// StaleTimeframes precomputed exactly as market.GetWith* does.
func freshnessData(symbol string, price float64, lags map[string]time.Duration) *market.Data {
	tfd := map[string]*market.TimeframeSeriesData{}
	for tf, lag := range lags {
		tfd[tf] = freshSeries(tf, 40, price, lag)
	}
	now := time.Now()
	return &market.Data{
		Symbol: symbol, CurrentPrice: price, TimeframeData: tfd,
		SampledAt: now, StaleTimeframes: market.StaleTimeframes(symbol, tfd, now),
	}
}

var (
	allFreshLags  = map[string]time.Duration{"1h": 0, "4h": 0}
	stale1hLags   = map[string]time.Duration{"1h": 10 * time.Hour, "4h": 0}
	stale4hOnly   = map[string]time.Duration{"1h": 0, "4h": 24 * time.Hour}
	staleBothLags = map[string]time.Duration{"1h": 10 * time.Hour, "4h": 48 * time.Hour}
)

func TestFreshnessFixtureSanity(t *testing.T) {
	if d := freshnessData("XUSDT", 100, allFreshLags); len(d.StaleTimeframes) != 0 {
		t.Fatalf("fresh fixture reads stale: %v", d.StaleTimeframes)
	}
	d := freshnessData("XUSDT", 100, stale1hLags)
	if _, ok := d.StaleTimeframes["1h"]; !ok || len(d.StaleTimeframes) != 1 {
		t.Fatalf("stale-1h fixture: %v", d.StaleTimeframes)
	}
}

// ---------------------------------------------------------------------------
// New entries
// ---------------------------------------------------------------------------

func TestEntryDataStaleBlock(t *testing.T) {
	at := riskTestTrader(store.RiskControlConfig{})
	d := &kernel.Decision{Action: "open_long", Symbol: "XUSDT"}

	// Fresh data: unchanged behavior.
	if err := at.entryDataStaleBlock(d, freshnessData("XUSDT", 100, allFreshLags)); err != nil {
		t.Fatalf("fresh data refused: %v", err)
	}
	// 1h stale: the floor yardstick series → refuse, with the typed code.
	err := at.entryDataStaleBlock(d, freshnessData("XUSDT", 100, stale1hLags))
	if err == nil || !strings.Contains(err.Error(), "DATA_STALE_1h") {
		t.Fatalf("stale 1h must refuse with DATA_STALE_1h, got %v", err)
	}
	// 4h stale: the cap yardstick series → refuse.
	err = at.entryDataStaleBlock(d, freshnessData("XUSDT", 100, stale4hOnly))
	if err == nil || !strings.Contains(err.Error(), "DATA_STALE_4h") {
		t.Fatalf("stale 4h must refuse with DATA_STALE_4h, got %v", err)
	}
	err = at.entryDataStaleBlock(d, freshnessData("XUSDT", 100, staleBothLags))
	if err == nil || !strings.Contains(err.Error(), "DATA_STALE_1h_4h") {
		t.Fatalf("both stale must name both timeframes, got %v", err)
	}
	// A stale series the stop band does NOT read does not block: the 15m series
	// is stale but ATR rides 1h/4h.
	data := freshnessData("XUSDT", 100, allFreshLags)
	data.StaleTimeframes = map[string]string{"15m": "newest 15m bar opened long ago"}
	if err := at.entryDataStaleBlock(d, data); err != nil {
		t.Fatalf("stale series outside the stop-band yardsticks must not block: %v", err)
	}
	// Hand-built snapshot with SampledAt but no precomputed map is evaluated
	// here (same fallback as the kernel signal layer).
	hand := freshnessData("XUSDT", 100, stale1hLags)
	hand.StaleTimeframes = nil
	if err := at.entryDataStaleBlock(d, hand); err == nil {
		t.Fatal("hand-built stale snapshot (SampledAt set, no precomputed map) must be evaluated and refused")
	}
	// No sampling clock (legacy / tests): not judged.
	legacy := freshnessData("XUSDT", 100, stale1hLags)
	legacy.StaleTimeframes, legacy.SampledAt = nil, time.Time{}
	if err := at.entryDataStaleBlock(d, legacy); err != nil {
		t.Fatalf("snapshot without a sampling clock must not be judged: %v", err)
	}
	// Fallback chain: with no 1h series the floor yardstick rides 4h — a stale
	// 4h then blocks, a stale (unused) 1h key does not exist.
	only4h := freshnessData("XUSDT", 100, map[string]time.Duration{"4h": 24 * time.Hour})
	if err := at.entryDataStaleBlock(d, only4h); err == nil || !strings.Contains(err.Error(), "DATA_STALE_4h") {
		t.Fatalf("4h-only stale chain must refuse, got %v", err)
	}
}

// entryMock lets the open executors run up to (and, when stale data wrongly
// slips through, past) the stale gate; any order submission is a failure.
type entryMock struct {
	audit05Mock
	opens, limits int
}

func (m *entryMock) GetBalance() (map[string]interface{}, error) {
	return map[string]interface{}{"totalEquity": 1000.0, "availableBalance": 1000.0}, nil
}
func (m *entryMock) OpenLong(string, float64, int) (map[string]interface{}, error) {
	m.opens++
	return nil, errors.New("must not be reached")
}
func (m *entryMock) OpenShort(string, float64, int) (map[string]interface{}, error) {
	m.opens++
	return nil, errors.New("must not be reached")
}
func (m *entryMock) PlaceLimitOrder(*types.LimitOrderRequest) (*types.LimitOrderResult, error) {
	m.limits++
	return nil, errors.New("must not be reached")
}
func (m *entryMock) SetMarginMode(string, bool) error { return nil }

func entryTestTrader(data *market.Data) (*AutoTrader, *entryMock) {
	m := &entryMock{audit05Mock: audit05Mock{mark: 100}}
	at := riskTestTrader(store.RiskControlConfig{MinRiskRewardRatio: 1})
	at.trader = m
	at.pendingEntries = map[string]*pendingEntry{}
	at.marketDataFn = func(string) (*market.Data, error) { return data, nil }
	return at, m
}

func TestStaleDataRefusesMarketAndLimitEntries(t *testing.T) {
	long := &kernel.Decision{Action: "open_long", Symbol: "YUSDT", Leverage: 3, PositionSizeUSD: 100, StopLoss: 95, TakeProfit: 115, Price: 99.5}
	short := &kernel.Decision{Action: "open_short", Symbol: "YUSDT", Leverage: 3, PositionSizeUSD: 100, StopLoss: 105, TakeProfit: 85, Price: 100.5}

	for name, run := range map[string]func(at *AutoTrader) error{
		"market_long": func(at *AutoTrader) error {
			d := *long
			return at.executeOpenLongWithRecord(&d, &store.DecisionAction{})
		},
		"market_short": func(at *AutoTrader) error {
			d := *short
			return at.executeOpenShortWithRecord(&d, &store.DecisionAction{})
		},
		"limit_long": func(at *AutoTrader) error {
			d := *long
			d.Action = "open_long_limit"
			return at.executeOpenLimitLongWithRecord(&d, &store.DecisionAction{})
		},
		"limit_short": func(at *AutoTrader) error {
			d := *short
			d.Action = "open_short_limit"
			return at.executeOpenLimitShortWithRecord(&d, &store.DecisionAction{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			at, m := entryTestTrader(freshnessData("YUSDT", 100, stale1hLags))
			err := run(at)
			if err == nil || !strings.Contains(err.Error(), "DATA_STALE_1h") {
				t.Fatalf("stale data must refuse the entry with DATA_STALE_1h, got %v", err)
			}
			if m.opens != 0 || m.limits != 0 {
				t.Fatalf("an order reached the exchange on stale data: opens=%d limits=%d", m.opens, m.limits)
			}
		})
	}
}

// Fresh data walks past the stale gate (it fails later, at the mock's order
// submission) — the gate adds no behavior change on healthy feeds.
func TestFreshDataPassesStaleGateOnEntries(t *testing.T) {
	at, m := entryTestTrader(freshnessData("YUSDT", 100, allFreshLags))
	d := &kernel.Decision{Action: "open_long", Symbol: "YUSDT", Leverage: 3, PositionSizeUSD: 100, StopLoss: 95, TakeProfit: 115}
	err := at.executeOpenLongWithRecord(d, &store.DecisionAction{})
	if err != nil && strings.Contains(err.Error(), "DATA_STALE") {
		t.Fatalf("fresh data refused as stale: %v", err)
	}
	_ = m
}

// Cycle gate states carrying DATA_STALE_* now stop MARKET opens (the code used
// to be a plain direction-gate failure with no market-open teeth).
func TestDataStaleIsAnAbsoluteBanCode(t *testing.T) {
	for _, code := range []string{"DATA_STALE_1h", "DATA_STALE_15m_1h_4h"} {
		if !absoluteBanCode(code) {
			t.Fatalf("%s must be an absolute ban code (market opens + pending recheck)", code)
		}
	}
	at := riskTestTrader(store.RiskControlConfig{})
	at.cycleGateStates = map[string]*kernel.GateState{"XUSDT": {LongFailed: []string{"DATA_STALE_1h"}}}
	if err := at.entryExecutionBlocked("XUSDT", "long"); err == nil || !strings.Contains(err.Error(), "DATA_STALE_1h") {
		t.Fatalf("entryExecutionBlocked must refuse on DATA_STALE_*, got %v", err)
	}
	if err := at.entryExecutionBlocked("XUSDT", "short"); err != nil {
		t.Fatalf("unrelated side must stay open: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Pending limit-entry recheck
// ---------------------------------------------------------------------------

func shiftSeries(data *market.Data, tf string, lag time.Duration) {
	for i := range data.TimeframeData[tf].Klines {
		data.TimeframeData[tf].Klines[i].Time -= lag.Milliseconds()
	}
}

func TestPendingRecheckCancelsOnStaleFeed(t *testing.T) {
	newTrader := func(lag time.Duration) *AutoTrader {
		at := riskTestTrader(store.RiskControlConfig{})
		at.config.StrategyConfig = fix7Config()
		at.trader = &audit05Mock{}
		at.pendingEntries = map[string]*pendingEntry{}
		fix7AnchorDay(at)
		tfs := at.recheckConfiguredTimeframes("XUSDT")
		at.recheckDataFn = func(string) (*market.Data, error) {
			data := fix7StrategyData(tfs, true)
			for tf := range data.TimeframeData {
				shiftSeries(data, tf, lag)
			}
			data.SampledAt = time.Now()
			data.StaleTimeframes = market.StaleTimeframes(data.Symbol, data.TimeframeData, data.SampledAt)
			return data, nil
		}
		return at
	}
	pe := &pendingEntry{Symbol: "XUSDT", Side: "long", Price: 100, Quantity: 1, StopLoss: 95, TakeProfit: 110, OrderID: "entry", PlacedAt: time.Now()}

	// Healthy feed: the placement survives its recheck exactly as before.
	if reason := newTrader(0).pendingDirectionBlocked(pe); reason != "" {
		t.Fatalf("fresh feed must not cancel a resting entry, got %q", reason)
	}
	// Dead feed (all series 12h behind): the resting order is cancelled — it was
	// priced and gated off a market that no longer exists.
	reason := newTrader(12 * time.Hour).pendingDirectionBlocked(pe)
	if !strings.Contains(reason, "DATA_STALE_") {
		t.Fatalf("stale feed must cancel the resting entry with DATA_STALE_*, got %q", reason)
	}
}

// ---------------------------------------------------------------------------
// Trailing / vol-target yardstick
// ---------------------------------------------------------------------------

func trailingFixture(lags map[string]time.Duration) (*AutoTrader, *batch1RiskMock) {
	at, m := newBatch1RiskTrader("long", 1, 120, store.RiskControlConfig{TrailingStopEnabled: true, ProfitLockAtR: -1})
	at.positionExitMode = map[string]string{}
	at.SetInitialStopLoss("XUSDT", "long", 90)
	at.SetRecordedStopLoss("XUSDT", "long", 90)
	at.marketDataFn = func(string) (*market.Data, error) { return freshnessData("XUSDT", 120, lags), nil }
	return at, m
}

func TestTrailingSkippedOnStaleYardstick(t *testing.T) {
	// Fresh: the trail ratchets the stop up (armed at 20% profit vs 15% arm).
	at, m := trailingFixture(allFreshLags)
	at.processVolTargetAndTrailing()
	if m.slCalls != 1 || at.GetRecordedStopLoss("XUSDT", "long") <= 90 {
		t.Fatalf("fresh ATR must still trail: slCalls=%d SL=%v", m.slCalls, at.GetRecordedStopLoss("XUSDT", "long"))
	}

	// Stale 1h (the yardstick series): no trailing move, the stop already on the
	// exchange / in the record stays untouched, and the fast-loop ATR cache is
	// dropped so it cannot trail off an older number either.
	at, m = trailingFixture(stale1hLags)
	r := newRealtimeProtection()
	r.atr["XUSDT"] = 1.0
	at.realtime = r
	at.processVolTargetAndTrailing()
	if m.slCalls != 0 || at.GetRecordedStopLoss("XUSDT", "long") != 90 {
		t.Fatalf("stale ATR yardstick must not trail: slCalls=%d SL=%v", m.slCalls, at.GetRecordedStopLoss("XUSDT", "long"))
	}
	if _, ok := r.atr["XUSDT"]; ok {
		t.Fatal("stale cycle must evict the cached fast-loop ATR")
	}
	// processTrailingForPosition with the evicted (zero) ATR is a no-op too.
	at.processTrailingForPosition(realtimePos("long", 1), 120, r.atr["XUSDT"])
	if m.slCalls != 0 {
		t.Fatal("fast trailing moved the stop without an ATR")
	}
}

// A stale yardstick must not disable the ATR-FREE protection rules: the 1R
// breakeven lock still runs and moves the stop to protect profit.
func TestStaleYardstickKeepsATRFreeProfitLock(t *testing.T) {
	at, m := newBatch1RiskTrader("long", 1, 120, store.RiskControlConfig{ProfitLockAtR: 1, MinPositionSize: 1})
	at.positionExitMode = map[string]string{}
	at.SetInitialStopLoss("XUSDT", "long", 90)
	at.SetRecordedStopLoss("XUSDT", "long", 90)
	at.marketDataFn = func(string) (*market.Data, error) { return freshnessData("XUSDT", 120, stale1hLags), nil }
	at.processVolTargetAndTrailing()
	if m.slCalls == 0 {
		t.Fatal("breakeven lock (ATR-free) must still move the stop when the ATR yardstick is stale")
	}
	if got := at.GetRecordedStopLoss("XUSDT", "long"); got < 100 {
		t.Fatalf("breakeven stop not applied: SL=%v", got)
	}
}

// ---------------------------------------------------------------------------
// Watchdog: naked-position protection never waits for a healthy feed
// ---------------------------------------------------------------------------

func TestComputedProtectionATR(t *testing.T) {
	// Fresh: the series' own ATR, not degraded.
	atr, deg := computedProtectionATR(100, freshnessData("XUSDT", 100, allFreshLags), nil)
	if deg != "" || atr != 1.0 {
		t.Fatalf("fresh: atr=%v degraded=%q, want 1 / \"\"", atr, deg)
	}
	// Stale + calm ATR: never narrower than the fixed fallback (3% = 1.5×2).
	atr, deg = computedProtectionATR(100, freshnessData("XUSDT", 100, stale1hLags), nil)
	if deg == "" || atr < 2.0-1e-9 {
		t.Fatalf("stale calm ATR must widen to the 3%% fallback: atr=%v degraded=%q", atr, deg)
	}
	// Stale + violent ATR: kept, capped at 8% (1.5×ATR ≤ 8).
	violent := freshnessData("XUSDT", 100, stale1hLags)
	violent.TimeframeData["1h"].ATR14 = 3.0 // 4.5% distance
	if atr, _ = computedProtectionATR(100, violent, nil); atr != 3.0 {
		t.Fatalf("stale violent ATR within the cap must be kept, got %v", atr)
	}
	violent.TimeframeData["1h"].ATR14 = 30 // would be 45%
	if atr, _ = computedProtectionATR(100, violent, nil); atr*1.5 > 8.0+1e-9 {
		t.Fatalf("stale ATR must be capped at 8%%, got distance %v%%", atr*1.5)
	}
	// Missing series / failed fetch: fixed K-line-free fallback, still protects.
	atr, deg = computedProtectionATR(100, nil, errors.New("feed down"))
	if atr <= 0 || !strings.Contains(deg, "feed down") || atr*1.5 != 3.0 {
		t.Fatalf("fetch failure: atr=%v degraded=%q", atr, deg)
	}
	if atr, deg = computedProtectionATR(100, &market.Data{TimeframeData: map[string]*market.TimeframeSeriesData{}}, nil); atr <= 0 || deg == "" {
		t.Fatalf("no 1h series: atr=%v degraded=%q", atr, deg)
	}
	if atr, _ = computedProtectionATR(0, nil, nil); atr != 0 {
		t.Fatal("no usable mark must not fabricate a stop")
	}
}

type nakedWatchdogMock struct {
	audit05Mock
	slPrices, tpPrices []float64
}

func (m *nakedWatchdogMock) GetPositions() ([]map[string]interface{}, error) {
	return []map[string]interface{}{{"symbol": "XUSDT", "side": "long", "positionAmt": 1.0, "entryPrice": 100.0, "markPrice": 100.0, "leverage": 2.0}}, nil
}
func (m *nakedWatchdogMock) SetStopLoss(symbol, side string, qty, price float64) error {
	m.slCalls++
	m.slPrices = append(m.slPrices, price)
	m.orders = append(m.orders, types.OpenOrder{OrderID: fmt.Sprintf("sl%d", m.slCalls), Symbol: symbol, PositionSide: side, Side: "SELL", Type: "STOP_MARKET", Quantity: qty, StopPrice: price})
	return nil
}
func (m *nakedWatchdogMock) SetTakeProfit(symbol, side string, qty, price float64) error {
	m.tpPrices = append(m.tpPrices, price)
	m.orders = append(m.orders, types.OpenOrder{OrderID: fmt.Sprintf("tp%d", len(m.tpPrices)), Symbol: symbol, PositionSide: side, Side: "SELL", Type: "TAKE_PROFIT_MARKET", Quantity: qty, StopPrice: price})
	return nil
}

func nakedWatchdog(dataFn func(string) (*market.Data, error)) (*AutoTrader, *nakedWatchdogMock) {
	m := &nakedWatchdogMock{audit05Mock: audit05Mock{mark: 100}}
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.computedProtectionDataFn = dataFn
	return at, m
}

func TestWatchdogProtectsNakedPositionOnFreshStaleAndMissingFeeds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dataFn func(string) (*market.Data, error)
		wantSL float64 // mark 100, long: SL = 100 − 1.5×ATR
	}{
		// Fresh 1h ATR = 1 → SL 98.5 (unchanged behavior).
		{"fresh", func(string) (*market.Data, error) { return freshnessData("XUSDT", 100, allFreshLags), nil }, 98.5},
		// Stale 1h: still protected; calm stale ATR widened to the 3% fallback.
		{"stale", func(string) (*market.Data, error) { return freshnessData("XUSDT", 100, stale1hLags), nil }, 97},
		// Fetch failure: still protected at the fixed 3%.
		{"fetch_failed", func(string) (*market.Data, error) { return nil, errors.New("klines unavailable") }, 97},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, m := nakedWatchdog(tc.dataFn)
			at.processProtectionWatchdog()
			if m.slCalls == 0 || len(m.tpPrices) == 0 {
				t.Fatalf("naked position left unprotected: sl=%v tp=%v", m.slPrices, m.tpPrices)
			}
			if got := m.slPrices[0]; got < tc.wantSL-1e-6 || got > tc.wantSL+1e-6 {
				t.Fatalf("SL = %v, want %v", got, tc.wantSL)
			}
			if rec := at.GetRecordedStopLoss("XUSDT", "long"); rec != m.slPrices[0] {
				t.Fatalf("recorded SL %v does not match the placed stop %v", rec, m.slPrices[0])
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Grid ATR bounds
// ---------------------------------------------------------------------------

func TestGridATRBoundsFallBackWhenFourHourStale(t *testing.T) {
	cfg := &store.GridStrategyConfig{GridCount: 10, ATRMultiplier: 2}
	build := func(lag time.Duration) *market.Data {
		d := freshnessData("XUSDT", 100, map[string]time.Duration{"4h": lag})
		d.LongerTermContext = &market.LongerTermData{ATR14: 5}
		return d
	}
	at := &AutoTrader{gridState: &GridState{}}
	at.calculateATRBounds(100, build(0), cfg)
	if at.gridState.UpperPrice != 110 || at.gridState.LowerPrice != 90 {
		t.Fatalf("fresh ATR bounds changed: %v-%v, want 90-110", at.gridState.LowerPrice, at.gridState.UpperPrice)
	}
	at.calculateATRBounds(100, build(48*time.Hour), cfg)
	if at.gridState.UpperPrice != 103 || at.gridState.LowerPrice != 97 {
		t.Fatalf("stale 4h must use the default ±3%% bounds, got %v-%v", at.gridState.LowerPrice, at.gridState.UpperPrice)
	}
	at.calculateATRBoundsLocked(100, build(0), cfg)
	if at.gridState.UpperPrice != 110 {
		t.Fatalf("locked fresh bounds changed: %v", at.gridState.UpperPrice)
	}
	at.calculateATRBoundsLocked(100, build(48*time.Hour), cfg)
	if at.gridState.UpperPrice != 103 || at.gridState.LowerPrice != 97 {
		t.Fatalf("locked: stale 4h must use the default bounds, got %v-%v", at.gridState.LowerPrice, at.gridState.UpperPrice)
	}
}
