package trader

import (
	"strings"
	"testing"

	"nofx/kernel"
	"nofx/store"
)

// Pins for the market-exception enforcement at the open dispatch
// (QUANT_REVIEW_2026-09-22 B1): in limit-entry mode a market open needs
// program evidence (GateState.MarketException) plus confidence ≥ the
// published bar; without it the decision degrades to the anchor limit, and
// with no anchor it is rejected. Market-default strategies are exempt.
func TestMarketExceptionGate(t *testing.T) {
	on := true
	rc := store.RiskControlConfig{LimitEntryEnabled: on}
	at := riskTestTrader(rc)
	at.cycleGateStates = map[string]*kernel.GateState{
		"TESTUSDT": {
			LongLimitAllowed:  true,
			LongEntryPrice:    99.5,
			ShortLimitAllowed: false,
		},
	}
	rec := &store.DecisionAction{}

	// 1. No evidence anywhere, live long anchor → degrade to the anchor limit.
	d := &kernel.Decision{Action: "open_long", Symbol: "TESTUSDT", Confidence: 90}
	out, err := at.marketExceptionGate(d, rec, true)
	if err != nil {
		t.Fatalf("degrade path returned error: %v", err)
	}
	if out.Action != "open_long_limit" || out.Price != 99.5 || !out.MarketDegraded {
		t.Fatalf("expected degrade to open_long_limit @99.5, got %s @%.2f degraded=%v", out.Action, out.Price, out.MarketDegraded)
	}

	// 2. No evidence, no anchor (short) → fail-closed rejection.
	d = &kernel.Decision{Action: "open_short", Symbol: "TESTUSDT", Confidence: 90}
	_, err = at.marketExceptionGate(d, rec, false)
	if err == nil || !strings.Contains(err.Error(), "no live anchor") {
		t.Fatalf("expected fail-closed rejection without anchor, got err=%v", err)
	}

	// 3./4. Short exception present (anchor still dead).
	at.cycleGateStates["TESTUSDT"] = &kernel.GateState{ShortMarketException: true}

	// 3. Evidence but confidence below the bar → still fail-closed.
	d = &kernel.Decision{Action: "open_short", Symbol: "TESTUSDT", Confidence: kernel.MarketExceptionMinScore - 1}
	_, err = at.marketExceptionGate(d, rec, false)
	if err == nil || !strings.Contains(err.Error(), "no live anchor") {
		t.Fatalf("short exception with low confidence must not pass and short has no anchor, got err=%v", err)
	}

	// 4. Evidence + confidence → market open passes untouched.
	d = &kernel.Decision{Action: "open_short", Symbol: "TESTUSDT", Confidence: kernel.MarketExceptionMinScore}
	out, err = at.marketExceptionGate(d, rec, false)
	if err != nil || out.Action != "open_short" || out.MarketDegraded {
		t.Fatalf("bona-fide exception must pass untouched, got action=%s err=%v", out.Action, err)
	}

	// 5. Market-default strategy (limit entry off) is exempt entirely.
	atFree := riskTestTrader(store.RiskControlConfig{LimitEntryEnabled: false})
	d = &kernel.Decision{Action: "open_long", Symbol: "TESTUSDT", Confidence: 10}
	out, err = atFree.marketExceptionGate(d, rec, true)
	if err != nil || out.Action != "open_long" {
		t.Fatalf("market-default strategy must bypass the gate, got action=%s err=%v", out.Action, err)
	}

	// 6. Missing gate state → fail-closed (decisions execute in the same
	// cycle their state was captured; absence is an anomaly).
	at.cycleGateStates = nil
	d = &kernel.Decision{Action: "open_long", Symbol: "TESTUSDT", Confidence: 90}
	_, err = at.marketExceptionGate(d, rec, true)
	if err == nil || !strings.Contains(err.Error(), "no hard-gate state") {
		t.Fatalf("missing gate state must fail closed, got err=%v", err)
	}
}
