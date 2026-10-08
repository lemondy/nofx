package trader

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
	"nofx/trader/types"
)

// Incident 2026-10-07: 29/29 limit entries cancelled within seconds of
// placement. Root cause: pendingDirectionBlocked fetched the GENERIC quote
// (100×3m aggregated into ~20 15m bars) but validated DataQuality at the
// strategy's 60-bars-per-timeframe minimum — DATA_INSUFFICIENT fired on
// EVERY recheck and the monitor cancelled every resting entry. The recheck
// now fetches the strategy-scoped series the AI decision was approved on.

// bars builds n closed bars stepping `step` minutes, ending (n-1)·step in
// the past — the newest bar is fully CLOSED (time-based settlement keeps it).
func fix7Bars(n int, stepMinutes int64, rising bool) []market.KlineBar {
	now := time.Now()
	bars := make([]market.KlineBar, 0, n)
	price := 100.0
	for i := 0; i < n; i++ {
		ts := now.Add(-time.Duration(n-i) * time.Duration(stepMinutes) * time.Minute)
		if rising {
			price *= 1.002
		} else {
			price *= 0.999
		}
		bars = append(bars, market.KlineBar{
			Time: ts.UnixMilli(), Open: price * 0.999, High: price * 1.004, Low: price * 0.996, Close: price, Volume: 1000,
		})
	}
	return bars
}

// fix7StrategyData builds the strategy-scoped series: every configured
// timeframe carries at least 60 closed bars (what the AI analysis saw).
func fix7StrategyData(tfs []string, rising bool) *market.Data {
	steps := map[string]int64{"3m": 3, "5m": 5, "15m": 15, "1h": 60, "4h": 240, "1d": 1440}
	tfd := map[string]*market.TimeframeSeriesData{}
	for _, tf := range tfs {
		step, ok := steps[tf]
		if !ok {
			step = 15
		}
		tfd[tf] = &market.TimeframeSeriesData{Timeframe: tf, Klines: fix7Bars(62, step, rising)}
	}
	return &market.Data{Symbol: "XUSDT", CurrentPrice: 101.0, TimeframeData: tfd}
}

// fix7ShortStrategyData replicates the incident: only ~20 bars of the
// primary timeframe (the 3m-aggregated quote the recheck used to read).
func fix7ShortStrategyData(tfs []string, rising bool) *market.Data {
	data := fix7StrategyData(tfs, rising)
	for _, tf := range tfs {
		k := data.TimeframeData[tf].Klines
		if len(k) > 20 {
			data.TimeframeData[tf].Klines = k[len(k)-20:]
		}
	}
	return data
}

func fix7Config() *store.StrategyConfig {
	cfg := &store.StrategyConfig{}
	cfg.Indicators.Klines.PrimaryTimeframe = "15m"
	cfg.Indicators.Klines.PrimaryCount = 60
	cfg.Indicators.Klines.SelectedTimeframes = []string{"15m", "1h", "4h"}
	cfg.Indicators.Klines.LongerTimeframe = "4h"
	// The BTC gate calls kernel.BTC4hTrendCloses, which hits the real
	// network — NON-DETERMINISTIC in unit tests (a passing fetch validates
	// the placement, a failing one cancels it). Keep the incident test
	// hermetic; the BTC gate's semantics are covered in kernel (TestFix3*).
	off := false
	cfg.RiskControl.BTCFilterLong = &off
	cfg.RiskControl.BTCFilterShort = &off
	cfg.RiskControl.ShortTopConfirmGate = &off
	return cfg
}

// fix7AnchorDay pins the instance's daily baseline to the mock equity so the
// shared daily-anchor inheritance (registry peers anchored by EARLIER tests
// at 1000 vs this mock's 900) cannot trip the 10% daily-loss halt inside the
// recheck flow. Production halt-cancel semantics are covered separately.
func fix7AnchorDay(at *AutoTrader) {
	at.dayStartDay = time.Now().UTC().Format("2006-01-02")
	at.dayStartEquity = 900
}

func fix7Pending(at *AutoTrader) *pendingEntry {
	fix7AnchorDay(at)
	pe := &pendingEntry{Symbol: "XUSDT", Side: "long", Price: 100, Quantity: 1, StopLoss: 95, TakeProfit: 110, OrderID: "entry", PlacedAt: time.Now()}
	at.setPendingEntry(pe)
	return pe
}

// The healthy path: with strategy-scoped data the placement survives its own
// recheck — this is the exact scenario that died 29/29 last night.
func TestFix7PendingSurvivesRecheckWithStrategyData(t *testing.T) {
	m := &audit05Mock{}
	at := riskTestTrader(store.RiskControlConfig{})
	at.config.StrategyConfig = fix7Config()
	at.trader = m
	at.pendingEntries = map[string]*pendingEntry{}
	pe := fix7Pending(at)

	tfs := at.recheckConfiguredTimeframes("XUSDT")
	recheckRan := false
	at.recheckDataFn = func(symbol string) (*market.Data, error) {
		recheckRan = true
		return fix7StrategyData(tfs, true), nil
	}
	at.processPendingEntries(true)

	if !recheckRan {
		t.Fatalf("injected recheck fetch never ran (seam bypassed) — halt=%q", at.pendingAccountHaltReason())
	}
	if at.getPendingEntry(pe.Symbol, pe.Side) == nil {
		t.Fatalf("a healthy placement was cancelled by its own recheck — the incident regression. halt=%q recheck=%q faults=%v",
			at.pendingAccountHaltReason(), at.pendingDirectionBlocked(pe), at.protectionFaultReason())
	}
	if m.cancelCalls != 0 {
		desc := ""
		for _, p := range at.accountPeers() {
			p.runtimeMu.RLock()
			desc += fmt.Sprintf(" [peer key=%q faults=%v]", p.executionAccountKey(), p.protectionFaults)
			p.runtimeMu.RUnlock()
		}
		t.Fatalf("healthy placement: cancel called %d times — halt=%q peers:%s",
			m.cancelCalls, at.pendingAccountHaltReason(), desc)
	}
}

// The incident shape: 20-bar series must still fail DataQuality (the
// direction-loss cancel capability is NOT removed — a genuinely insufficient
// series means the placement cannot be re-validated).
func TestFix7ShortSeriesStillFailsDataQuality(t *testing.T) {
	at := riskTestTrader(store.RiskControlConfig{})
	at.config.StrategyConfig = fix7Config()
	tfs := at.recheckConfiguredTimeframes("XUSDT")
	at.recheckDataFn = func(symbol string) (*market.Data, error) {
		return fix7ShortStrategyData(tfs, true), nil
	}
	reason := at.pendingDirectionBlocked(&pendingEntry{Symbol: "XUSDT", Side: "long"})
	if !strings.Contains(reason, "DATA_INSUFFICIENT") {
		t.Fatalf("a genuinely 20-bar series must still fail with DATA_INSUFFICIENT, got %q", reason)
	}
}

// Direction loss still cancels: the 15m trend turning against the entry side
// (MICRO_TREND code path) removes the resting order.
func TestFix7PendingStillCancelsOnDirectionLoss(t *testing.T) {
	// Initial status NEW: the recheck path must be the one that cancels.
	// audit05Mock's CancelOrder flips the status to CANCELED so the follow-up
	// GetOrderStatus inside cancelPending sees a terminal order.
	m := &audit05Mock{cancelTerminal: true, status: map[string]interface{}{"status": "NEW", "executedQty": 0.0}}
	at := riskTestTrader(store.RiskControlConfig{EntryTimingGate: true})
	at.config.StrategyConfig = fix7Config()
	at.trader = m
	at.pendingEntries = map[string]*pendingEntry{}
	pe := fix7Pending(at)

	tfs := at.recheckConfiguredTimeframes("XUSDT")
	// FALLING 15m data: the long placement's micro-trend gate now blocks.
	at.recheckDataFn = func(symbol string) (*market.Data, error) {
		data := fix7StrategyData(tfs, false)
		data.CurrentPrice = 99.0
		return data, nil
	}
	at.processPendingEntries(true)

	if m.cancelCalls == 0 {
		t.Fatal("a direction-invalidated placement was left resting")
	}
	if at.getPendingEntry(pe.Symbol, pe.Side) != nil {
		t.Fatal("cancelled placement not retired")
	}
}

// A filled order must be untouched by the recheck (fill handling precedes it).
func TestFix7FilledPendingUnaffectedByRecheck(t *testing.T) {
	m := &audit05Mock{
		status: map[string]interface{}{"status": "FILLED", "executedQty": 1.0, "avgPrice": 100.0},
		// Full protection on the exchange so the fill path completes and
		// retires the row (incomplete coverage would KEEP it by design).
		orders: []types.OpenOrder{
			{Type: "STOP_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 95, Quantity: 1},
			{Type: "TAKE_PROFIT_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 110, Quantity: 1},
		},
	}
	at := riskTestTrader(store.RiskControlConfig{})
	at.config.StrategyConfig = fix7Config()
	at.trader = m
	at.pendingEntries = map[string]*pendingEntry{}
	at.positionInitialStopLoss = map[string]float64{}
	at.positionExitMode = map[string]string{}
	at.setPendingEntry(&pendingEntry{Symbol: "XUSDT", Side: "long", Price: 100, Quantity: 1, StopLoss: 95, TakeProfit: 110, OrderID: "entry", PlacedAt: time.Now()})
	// Full coverage available from the mock, so the fill path completes and
	// retires the pending row. Malicious recheck data must never be fetched.
	recheckRan := false
	at.recheckDataFn = func(symbol string) (*market.Data, error) {
		recheckRan = true
		return nil, fmt.Errorf("recheck must not run for filled orders")
	}
	at.processPendingEntries(true)
	if recheckRan {
		t.Fatal("the direction recheck ran for a FILLED order")
	}
	if at.getPendingEntry("XUSDT", "long") != nil {
		t.Fatal("filled placement not retired")
	}
}



// Option ② (user decision 2026-10-08): the 30s protection pass must NOT
// direction-cancel a resting entry — micro-trend/consensus wobble only
// cancels at the cycle boundary. Fast paths (fill detection, SL-crossed
// invalidation, account halt) stay on the 30s cadence.
func TestFix9ThirtySecondPassKeepsDirectionValidEntry(t *testing.T) {
	m := &audit05Mock{status: map[string]interface{}{"status": "NEW", "executedQty": 0.0}}
	at := riskTestTrader(store.RiskControlConfig{EntryTimingGate: true})
	at.config.StrategyConfig = fix7Config()
	at.trader = m
	at.pendingEntries = map[string]*pendingEntry{}
	fix7AnchorDay(at)
	tfs := at.recheckConfiguredTimeframes("XUSDT")
	// FALLING 15m data: the cycle-boundary recheck WOULD cancel this long
	// (MICRO_TREND / consensus) — but a 30s pass must leave it resting.
	at.recheckDataFn = func(symbol string) (*market.Data, error) {
		data := fix7StrategyData(tfs, false)
		data.CurrentPrice = 99.0
		return data, nil
	}
	at.setPendingEntry(&pendingEntry{Symbol: "XUSDT", Side: "long", Price: 100, Quantity: 1, StopLoss: 95, TakeProfit: 110, OrderID: "entry", PlacedAt: time.Now()})
	at.processPendingEntries(false)
	if m.cancelCalls != 0 {
		t.Fatalf("30s pass direction-cancelled a resting entry (%d cancels) — option ② regression", m.cancelCalls)
	}
	if at.getPendingEntry("XUSDT", "long") == nil {
		t.Fatal("30s pass dropped a direction-wobbled but resting entry")
	}
	// The same state on a CYCLE boundary still cancels (direction discipline
	// is preserved, just de-frequenced).
	at.processPendingEntries(true)
	if m.cancelCalls == 0 {
		t.Fatal("cycle-boundary recheck did not cancel the direction-invalidated entry")
	}
}

// SL-crossed invalidation stays on the 30s pass: price crossing the stop
// before entry is a hard fact, not a wobble.
func TestFix9ThirtySecondPassStillInvalidatesCrossedStop(t *testing.T) {
	m := &audit05Mock{cancelTerminal: true, status: map[string]interface{}{"status": "NEW", "executedQty": 0.0}}
	at := riskTestTrader(store.RiskControlConfig{EntryTimingGate: true})
	at.config.StrategyConfig = fix7Config()
	at.trader = m
	at.pendingEntries = map[string]*pendingEntry{}
	fix7AnchorDay(at)
	at.marketDataFn = func(symbol string) (*market.Data, error) {
		data := fix7StrategyData([]string{"15m"}, true)
		data.CurrentPrice = 94.0 // below the 95 stop: setup is dead
		return data, nil
	}
	at.setPendingEntry(&pendingEntry{Symbol: "XUSDT", Side: "long", Price: 100, Quantity: 1, StopLoss: 95, TakeProfit: 110, OrderID: "entry", PlacedAt: time.Now()})
	at.processPendingEntries(false)
	if m.cancelCalls == 0 {
		t.Fatal("price crossed the stop before entry — the 30s pass must invalidate immediately")
	}
	if at.getPendingEntry("XUSDT", "long") != nil {
		t.Fatal("invalidated placement not retired")
	}
}

