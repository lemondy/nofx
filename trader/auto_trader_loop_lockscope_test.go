package trader

import (
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nofx/kernel"
	"nofx/store"
)

func cryptoLockScopeTrader(t *testing.T, m *c2PendingMock) *AutoTrader {
	t.Helper()
	at := c2Trader(m)
	at.name, at.id, at.exchangeID = t.Name(), t.Name(), t.Name()
	at.config.StrategyConfig.CoinSource.StaticCoins = []string{"XUSDT", "YUSDT"}
	t.Cleanup(func() { unregisterAccountTrader(at) })
	return at
}

func cryptoLockScopeUnlocked(t *testing.T, at *AutoTrader) {
	t.Helper()
	mu := at.executionMutex()
	if !mu.TryLock() {
		t.Fatal("account execution mutex is still locked")
	}
	mu.Unlock()
}

func TestCryptoLockScopeMarketWindowReleasesExecutionMutex(t *testing.T) {
	m := &c2PendingMock{audit05Mock: audit05Mock{flat: true}}
	at := cryptoLockScopeTrader(t, m)
	entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	at.marketContextFn = func(ctx *kernel.Context) {
		if ctx.CandidateCoins != nil {
			t.Error("account context already contains candidates")
		}
		close(entered)
		<-resume
		at.enrichMarketContext(ctx)
	}
	called := false
	var cycleErr error
	go func() {
		defer close(done)
		cycleErr = at.runCycleWithDecision(func(ctx *kernel.Context) (*kernel.FullDecision, error) {
			called = true
			if len(ctx.CandidateCoins) != 2 {
				t.Errorf("market enrichment lost static candidates: %+v", ctx.CandidateCoins)
			}
			return &kernel.FullDecision{Decisions: []kernel.Decision{{Symbol: "XUSDT", Action: "wait"}}}, nil
		})
	}()
	wait := func() {
		t.Helper()
		select {
		case <-done:
			if cycleErr != nil {
				t.Errorf("cycle: %v", cycleErr)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("cycle did not finish")
		}
	}
	// Release and join before fixture cleanup, including on assertion failure.
	t.Cleanup(wait)
	t.Cleanup(release)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("market enrichment was not reached")
	}
	cryptoLockScopeUnlocked(t, at)
	release()
	wait()
	if !called {
		t.Fatal("cycle did not reach decide after market enrichment")
	}
	cryptoLockScopeUnlocked(t, at)
}

func TestCryptoLockScopeStoppedDuringMarketWindow(t *testing.T) {
	m := &c2PendingMock{audit05Mock: audit05Mock{flat: true}}
	at := cryptoLockScopeTrader(t, m)
	enriched, called := false, false
	at.marketContextFn = func(*kernel.Context) {
		enriched = true
		cryptoLockScopeUnlocked(t, at)
		at.isRunningMutex.Lock()
		at.isRunning = false
		at.isRunningMutex.Unlock()
	}
	err := at.runCycleWithDecision(func(*kernel.Context) (*kernel.FullDecision, error) {
		called = true
		return &kernel.FullDecision{Decisions: []kernel.Decision{{Symbol: "XUSDT", Action: "open_long"}}}, nil
	})
	if err != nil || !enriched || called {
		t.Fatalf("stopped cycle: enriched=%v decide=%v err=%v", enriched, called, err)
	}
	if m.closeCalls != 0 || m.slCalls != 0 || m.cancelCalls != 0 {
		t.Fatalf("stopped cycle placed orders: %+v", m)
	}
	cryptoLockScopeUnlocked(t, at)
}

func TestCryptoLockScopeMarketPanicReleasesExecutionMutex(t *testing.T) {
	m := &c2PendingMock{audit05Mock: audit05Mock{flat: true}}
	at := cryptoLockScopeTrader(t, m)
	at.marketContextFn = func(*kernel.Context) { panic("market enrichment panic") }
	called := false
	var recovered interface{}
	func() {
		defer func() { recovered = recover() }()
		_ = at.runCycleWithDecision(func(*kernel.Context) (*kernel.FullDecision, error) {
			called = true
			return &kernel.FullDecision{}, nil
		})
	}()
	if recovered != "market enrichment panic" || called {
		t.Fatalf("market panic: recovered=%v decide=%v", recovered, called)
	}
	cryptoLockScopeUnlocked(t, at)
}

type cryptoLockScopePositionMock struct {
	*c2PendingMock
	decided bool
}

func (m *cryptoLockScopePositionMock) GetPositions() ([]map[string]interface{}, error) {
	positions, err := m.c2PendingMock.GetPositions()
	if m.decided {
		positions[0]["positionAmt"] = 0.5
		positions = append(positions, map[string]interface{}{
			"symbol": "YUSDT", "side": "long", "positionAmt": 0.25,
			"entryPrice": 100.0, "markPrice": 100.0, "leverage": 2.0,
			"unRealizedProfit": 0.0, "liquidationPrice": 50.0,
		})
	}
	return positions, err
}

func TestCryptoLockScopeStateChangedRejectsStaleDecisions(t *testing.T) {
	base := &c2PendingMock{audit05Mock: audit05Mock{orders: c2ProtectionOrders()}}
	at := cryptoLockScopeTrader(t, base)
	m := &cryptoLockScopePositionMock{c2PendingMock: base}
	at.trader = m
	st, err := store.New(filepath.Join(t.TempDir(), "lockscope.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	at.store = st
	err = at.runCycleWithDecision(func(ctx *kernel.Context) (*kernel.FullDecision, error) {
		if len(ctx.Positions) != 1 || ctx.Positions[0].Quantity != 1 {
			t.Fatalf("unexpected prompt positions: %+v", ctx.Positions)
		}
		m.decided = true
		return &kernel.FullDecision{Decisions: []kernel.Decision{
			{Symbol: "XUSDT", Action: "partial_close_long", CloseFraction: 0.5},
			{Symbol: "XUSDT", Action: "adjust_stop_loss", StopLoss: 98},
			{Symbol: "XUSDT", Action: "close_long"},
			{Symbol: "YUSDT", Action: "open_long", StopLoss: 95, TakeProfit: 110, Leverage: 2, Confidence: 100, PositionSizeUSD: 100},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"partial_close_long", "adjust_stop_loss"} {
		if reason := at.filterReasons["XUSDT|"+action]; !strings.HasPrefix(reason, "STATE_CHANGED:") {
			t.Errorf("%s filter reason = %q", action, reason)
		}
	}
	records, err := st.Decision().GetLatestRecords(at.id, 1)
	if err != nil || len(records) != 1 {
		t.Fatalf("saved decision records: count=%d err=%v", len(records), err)
	}
	record := records[0]
	log := strings.Join(record.ExecutionLog, "\n")
	for _, decision := range []string{"XUSDT partial_close_long", "XUSDT adjust_stop_loss", "YUSDT open_long"} {
		if !strings.Contains(log, decision+" rejected: STATE_CHANGED") {
			t.Errorf("missing rejection for %s: %s", decision, log)
		}
	}
	if strings.Contains(at.filterReasons["XUSDT|close_long"], "STATE_CHANGED") || strings.Contains(log, "XUSDT close_long rejected: STATE_CHANGED") {
		t.Fatalf("full close was rejected by the state filter: %s", log)
	}
	// The manual-position mock can fail hands-off in the executor; reaching
	// that executor still proves the full close survived the state filter.
	if len(record.Decisions) != 1 || record.Decisions[0].Action != "close_long" {
		t.Fatalf("stale decisions reached execution or full close was dropped: %+v", record.Decisions)
	}
	if at.cycleRiskReservedUSD != 0 || at.cycleLongRiskReservedUSD != 0 || len(at.cycleSymbolRiskReservedUSD) != 0 {
		t.Fatalf("rejected open reserved cycle risk: total=%g long=%g symbols=%v", at.cycleRiskReservedUSD, at.cycleLongRiskReservedUSD, at.cycleSymbolRiskReservedUSD)
	}
	cryptoLockScopeUnlocked(t, at)
}

func TestCryptoLockScopeDecisionStateChanged(t *testing.T) {
	position := func(side string, qty float64) []kernel.PositionInfo {
		return []kernel.PositionInfo{{Symbol: "XUSDT", Side: side, Quantity: qty}}
	}
	type testCase struct {
		name, action, symbol string
		prompt, fresh        []kernel.PositionInfo
		changed              bool
	}
	var cases []testCase
	for _, action := range []struct {
		name, side      string
		requirePosition bool
	}{
		{"open_long", "long", false}, {"open_long_limit", "long", false},
		{"open_short", "short", false}, {"open_short_limit", "short", false},
		{"partial_close_long", "long", true}, {"partial_close_short", "short", true},
		{"adjust_stop_loss", "long", false},
	} {
		for _, qty := range []struct {
			name          string
			before, after float64
			changed       bool
		}{
			{"unchanged", 1, 1, false},
			{"missing_both", 0, 0, action.requirePosition},
			{"appeared", 0, 1, true}, {"disappeared", 1, 0, true},
			{"reduced", 1, 0.5, true}, {"added", 1, 2, true},
			{"relative_noise", 1, 1 + 1e-7, false},
			{"relative_change", 1, 1 + 1e-5, true},
			{"absolute_noise", 1e-10, 2e-10, false},
			{"absolute_change", 1e-10, 2e-9, true},
			{"nan_prompt", math.NaN(), 1, true}, {"nan_fresh", 1, math.NaN(), true},
			{"inf_prompt", math.Inf(1), 1, true}, {"inf_fresh", 1, math.Inf(-1), true},
			{"same_inf", math.Inf(1), math.Inf(1), true},
		} {
			prompt, fresh := position(action.side, qty.before), position(action.side, qty.after)
			if qty.before == 0 {
				prompt = nil // An absent side is quantity zero.
			}
			if qty.after == 0 {
				fresh = nil
			}
			cases = append(cases, testCase{action.name + "/" + qty.name, action.name, "XUSDT", prompt, fresh, qty.changed})
		}
	}
	for _, action := range []string{"close_long", "close_short", "hold", "wait", "unknown"} {
		cases = append(cases, testCase{action, action, "XUSDT", position("long", 1), position("long", math.NaN()), false})
	}
	cases = append(cases,
		testCase{"normalized_unchanged", "open_long", " x-usdt-swap ", []kernel.PositionInfo{{Symbol: "x_usdt", Side: "long", Quantity: 1}}, position("long", 1), false},
		testCase{"normalized_changed", "open_long", "x", []kernel.PositionInfo{{Symbol: "x_usdt", Side: "long", Quantity: 1}}, position("long", 0.5), true},
		testCase{"other_symbol", "open_long", "YUSDT", position("long", 1), position("long", 0.5), false},
		testCase{"hedge_opposite_only", "open_long", "XUSDT", position("short", 1), position("short", 0.5), false},
		testCase{"hedge_long_unchanged", "open_long", "XUSDT", append(position("long", 1), position("short", 1)...), append(position("long", 1), position("short", 0.5)...), false},
		testCase{"hedge_short_unchanged", "open_short", "XUSDT", append(position("long", 1), position("short", 1)...), append(position("long", 0.5), position("short", 1)...), false},
		testCase{"adjust_short_changed", "adjust_stop_loss", "XUSDT", position("short", 1), position("short", 0.5), true},
		testCase{"adjust_short_appeared", "adjust_stop_loss", "XUSDT", nil, position("short", 1), true},
		testCase{"adjust_short_disappeared", "adjust_stop_loss", "XUSDT", position("short", 1), nil, true},
		testCase{"partial_opposite_only", "partial_close_long", "XUSDT", position("short", 1), position("short", 1), true},
		testCase{"absolute_boundary", "open_long", "XUSDT", nil, position("long", 1e-9), false},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed, detail := cryptoDecisionStateChanged(tc.action, tc.symbol, tc.prompt, tc.fresh)
			if changed != tc.changed || (changed && detail == "") || (!changed && detail != "") {
				t.Fatalf("changed=%v detail=%q, want changed=%v", changed, detail, tc.changed)
			}
		})
	}
}
