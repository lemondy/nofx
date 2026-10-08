package trader

import (
	"strings"
	"testing"
	"time"

	"nofx/kernel"
	"nofx/market"
	"nofx/store"
)

// ── recheck 2026-10-07 (CODE_REVIEW_2026-10-07_RECHECK.md) residuals ──

// R1: BTC_REGIME_UNKNOWN must be an ABSOLUTE ban — the market dispatch and
// the execution pre-check block it just like BTC_4H_STRONGBULL (the short
// hard pause is opt-in; its unknown state is fail-closed by convention).
func TestFix8BTCRegimeUnknownReachesMarketExecution(t *testing.T) {
	for _, code := range []string{"BTC_REGIME_UNKNOWN", "BTC_4H_STRONGBULL"} {
		at := riskTestTrader(store.RiskControlConfig{})
		at.cycleGateStates = map[string]*kernel.GateState{"XUSDT": {ShortAllowed: false, ShortFailed: []string{code}}}
		ctx := &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}}
		d := kernel.Decision{Symbol: "XUSDT", Action: "open_short", Price: 100, StopLoss: 105, TakeProfit: 90, PositionSizeUSD: 100, Confidence: 90}
		if got := at.applyHardRiskGates([]kernel.Decision{d}, ctx); len(got) != 0 {
			t.Errorf("%s must block the market dispatch, got %d decisions through", code, len(got))
		}
	}
	if !absoluteBanCode("BTC_REGIME_UNKNOWN") {
		t.Fatal("BTC_REGIME_UNKNOWN not registered in absoluteBanCode")
	}
}

// R2: the pending recheck mirrors the per-side BTC policy on unknown data —
// the long placement SURVIVES (documented fail-open) while the short
// placement under the opt-in filter is CANCELLED with BTC_REGIME_UNKNOWN.
func TestFix8PendingBTCUnknownMirrorsPolicy(t *testing.T) {
	shortBTC := make([]float64, 40) // below the 60-close convergence bar

	build := func(side string, filterShort bool) (*AutoTrader, *pendingEntry, *audit05Mock) {
		m := &audit05Mock{cancelTerminal: true, status: map[string]interface{}{"status": "NEW", "executedQty": 0.0}}
		at := riskTestTrader(store.RiskControlConfig{})
		at.config.StrategyConfig = fix7Config()
		off := false
		at.config.StrategyConfig.RiskControl.BTCFilterLong = &off
		at.config.StrategyConfig.RiskControl.BTCFilterShort = &filterShort
		at.config.StrategyConfig.RiskControl.ShortTopConfirmGate = &off
		at.trader = m
		at.pendingEntries = map[string]*pendingEntry{}
		at.positionInitialStopLoss = map[string]float64{}
		at.positionExitMode = map[string]string{}
		fix7AnchorDay(at)
		tfs := at.recheckConfiguredTimeframes("XUSDT")
		at.recheckDataFn = func(symbol string) (*market.Data, error) {
			return fix7StrategyData(tfs, true), nil
		}
		at.btcTrendClosesFn = func(limit int) []float64 { return shortBTC }
		pe := &pendingEntry{Symbol: "XUSDT", Side: side, Price: 100, Quantity: 1, StopLoss: 95, TakeProfit: 110, OrderID: "entry", PlacedAt: time.Now()}
		at.setPendingEntry(pe)
		return at, pe, m
	}

	// Long + filter enabled + unknown BTC → the documented fail-open keeps it.
	atL, peL, mL := build("long", false)
	atL.processPendingEntries(true)
	if atL.getPendingEntry(peL.Symbol, peL.Side) == nil || mL.cancelCalls != 0 {
		t.Fatalf("long placement must survive unknown BTC data (fail-open policy), cancelled=%d", mL.cancelCalls)
	}

	// Short + opt-in filter + unknown BTC → BTC_REGIME_UNKNOWN cancels it.
	atS, _, mS := build("short", true)
	atS.processPendingEntries(true)
	if mS.cancelCalls == 0 {
		t.Fatal("short placement under the opt-in filter must fail closed on unknown BTC data")
	}
	if atS.protectionFaultReason() != "" && !strings.Contains(atS.protectionFaultReason(), "pending") {
		t.Logf("short cancel fault state: %q", atS.protectionFaultReason())
	}
}
