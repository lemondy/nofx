package trader

import (
	"errors"
	"nofx/kernel"
	"nofx/store"
	"nofx/trader/types"
	"sync"
	"testing"
	"time"
)

func TestAIWaitReleasesProtectionAndReacquiresAfterPanic(t *testing.T) {
	mu := &sync.Mutex{}
	mu.Lock()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic")
			}
		}()
		withoutExecutionLock(mu, func() (int, error) {
			if !mu.TryLock() {
				t.Fatal("AI request still owns execution lock")
			}
			mu.Unlock()
			panic("upstream panic")
		})
	}()
	if mu.TryLock() {
		t.Fatal("panic did not restore caller's lock")
	}
	mu.Unlock()
}

func TestProtectionRunsWhileAIWaits(t *testing.T) {
	at := review03Trader(&review03Mock{status: map[string]interface{}{"status": "PARTIALLY_FILLED", "executedQty": 0.4, "avgPrice": 100.0}})
	at.setPendingEntry(review03Plan())
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		mu := at.executionMutex()
		mu.Lock()
		defer mu.Unlock()
		defer close(done)
		withoutExecutionLock(mu, func() (int, error) { close(entered); <-release; return 0, nil })
	}()
	<-entered
	protected := make(chan struct{})
	go func() { at.safeProtectionPass(); close(protected) }()
	select {
	case <-protected:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("AI blocked fill protection")
	}
	mu := at.executionMutex()
	mu.Lock()
	qty := at.getPendingEntry("XUSDT", "long").ProtectedQty
	mu.Unlock()
	close(release)
	<-done
	if qty != 0.4 {
		t.Fatalf("partial fill protection watermark=%v", qty)
	}
}

type stopCancelMock struct {
	*review03Mock
	canceled chan struct{}
}

func (m *stopCancelMock) CancelOrder(symbol, id string) error {
	err := m.review03Mock.CancelOrder(symbol, id)
	close(m.canceled)
	return err
}
func TestStopCancelsEntriesBeforeJoiningAI(t *testing.T) {
	m := &stopCancelMock{review03Mock: &review03Mock{status: map[string]interface{}{"status": "CANCELED", "executedQty": 0.0}}, canceled: make(chan struct{})}
	at := review03Trader(m.review03Mock)
	at.trader = m
	at.setPendingEntry(review03Plan())
	at.isRunning = true
	at.stopMonitorCh = make(chan struct{})
	at.runDone = make(chan struct{})
	entered, release := make(chan struct{}), make(chan struct{})
	go func() {
		mu := at.executionMutex()
		mu.Lock()
		withoutExecutionLock(mu, func() (int, error) { close(entered); <-release; return 0, nil })
		mu.Unlock()
		close(at.runDone)
	}()
	<-entered
	stopped := make(chan struct{})
	go func() { at.Stop(); close(stopped) }()
	select {
	case <-m.canceled:
	case <-time.After(time.Second):
		close(release)
		<-stopped
		t.Fatal("stop waits for AI before canceling entries")
	}
	close(release)
	<-stopped
	if at.getPendingEntry("XUSDT", "long") != nil {
		t.Fatal("confirmed empty cancellation was not cleaned up")
	}
}

func TestReloadCannotStartFromStaleRunIntent(t *testing.T) {
	at := lifecycleFixture(t)
	if err := at.store.Trader().UpdateStatus("u", "t", true); err != nil {
		t.Fatal(err)
	}
	before, err := at.store.Trader().GetForUser("u", "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := at.store.Trader().UpdateStatus("u", "t", false); err != nil {
		t.Fatal(err)
	}
	if err := at.beginRunWithIntent(&before.RunVersion); !errors.Is(err, ErrRunIntentChanged) {
		t.Fatalf("stale reload start=%v", err)
	}
	if err := at.store.Trader().StopIfVersion("u", "t", before.RunVersion); err != nil {
		t.Fatal(err)
	}
	after, _ := at.store.Trader().GetForUser("u", "t")
	if after.IsRunning || after.RunVersion == before.RunVersion {
		t.Fatal("stop intent was overwritten")
	}
}

func TestGridCancelPreservesUnrelatedOrders(t *testing.T) {
	m := &review03Mock{status: map[string]interface{}{"status": "CANCELED", "executedQty": 0.4, "avgPrice": 100.0}, orders: []types.OpenOrder{{OrderID: "entry", Type: "LIMIT"}, {OrderID: "manual", Type: "LIMIT"}, {OrderID: "protection", Type: "STOP_MARKET"}}}
	at := review03Trader(m)
	at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "XUSDT"}
	at.gridState = &GridState{OrderBook: map[string]int{"entry": 0}, Levels: []kernel.GridLevelInfo{{State: "pending", OrderID: "entry", OrderQuantity: 1, Price: 100}}}
	if err := at.cancelAllGridOrders(); err != nil {
		t.Fatal(err)
	}
	if len(m.orders) != 2 || m.orders[0].OrderID != "manual" || m.orders[1].OrderID != "protection" {
		t.Fatalf("unrelated orders canceled: %+v", m.orders)
	}
	if at.gridState.Levels[0].PositionSize != 0.4 {
		t.Fatal("partial cancel lost filled lot")
	}
}

type gridExitMock struct {
	*review03Mock
	exitUnknown bool
	closeQty    float64
	closeCalls  int
}

func (m *gridExitMock) GetMarketPrice(string) (float64, error) { return 90, nil }
func (m *gridExitMock) CloseLong(_ string, qty float64) (map[string]interface{}, error) {
	m.closeCalls++
	m.closeQty = qty
	return map[string]interface{}{"orderId": "exit"}, nil
}
func (m *gridExitMock) GetOrderStatus(_ string, id string) (map[string]interface{}, error) {
	if id == "exit" {
		if m.exitUnknown {
			return nil, errors.New("receipt timeout")
		}
		return map[string]interface{}{"status": "FILLED", "executedQty": 0.6, "avgPrice": 90.0}, nil
	}
	return map[string]interface{}{"status": "CANCELED", "executedQty": 0.6, "avgPrice": 100.0}, nil
}
func TestGridPartialStopReconcilesLateFillAndUnknownExit(t *testing.T) {
	m := &gridExitMock{review03Mock: &review03Mock{}, exitUnknown: true}
	at := review03Trader(m.review03Mock)
	at.trader = m
	at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "XUSDT", StopLossPct: 5}
	at.gridState = &GridState{OrderBook: map[string]int{"entry": 0}, Levels: []kernel.GridLevelInfo{{Side: "buy", State: "pending", OrderID: "entry", OrderQuantity: 1, ExecutedQuantity: 0.4, PositionSize: 0.4, PositionEntry: 100}}}
	at.checkAndExecuteStopLoss()
	at.checkAndExecuteStopLoss()
	if m.closeQty != 0.6 || m.closeCalls != 1 || at.gridState.Levels[0].ExitOrderID != "exit" {
		t.Fatalf("late fill/unknown exit lost: qty=%v calls=%d level=%+v", m.closeQty, m.closeCalls, at.gridState.Levels[0])
	}
	m.exitUnknown = false
	at.checkAndExecuteStopLoss()
	at.checkAndExecuteStopLoss()
	if m.closeCalls != 1 || at.gridState.Levels[0].PositionSize != 0 || at.gridState.TotalProfit != -6 {
		t.Fatalf("exit not reconciled once: %+v", at.gridState)
	}
}

func TestGridCheckpointRetainsPartiallyCanceledExposureOnReload(t *testing.T) {
	at := lifecycleFixture(t)
	at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "XUSDT", GridCount: 10, Leverage: 1, TotalInvestment: 100, LowerPrice: 80, UpperPrice: 120}
	at.trader = &review03Mock{status: map[string]interface{}{"status": "CANCELED", "executedQty": 0.4, "avgPrice": 100.0}}
	at.gridState = NewGridState(at.config.StrategyConfig.GridConfig)
	at.gridState.IsInitialized = true
	at.gridState.Levels = []kernel.GridLevelInfo{{Side: "buy", State: "pending", OrderID: "entry", OrderQuantity: 1, Price: 100}}
	if err := at.persistGridLedger(); err != nil {
		t.Fatal(err)
	}
	next := &AutoTrader{id: at.id, userID: at.userID, store: at.store, config: at.config, trader: at.trader}
	if err := next.InitializeGrid(); err != nil {
		t.Fatal(err)
	}
	if next.gridState.Levels[0].PositionSize != 0.4 || next.gridState.Levels[0].State != "filled" {
		t.Fatalf("restored ledger=%+v", next.gridState.Levels)
	}
}

func TestTerminalReceiptCannotReuseRevokedProtectionWatermark(t *testing.T) {
	m := &review03Mock{}
	at := review03Trader(m)
	pe := review03Plan()
	at.setPendingEntry(pe)
	at.protectExecutedSlice(pe, map[string]interface{}{"executedQty": 0.4, "avgPrice": 100.0})
	if pe.ProtectedQty != 0.4 {
		t.Fatal("fixture protection failed")
	}
	m.orders = nil
	m.invisible = true
	m.status = map[string]interface{}{"status": "CANCELED", "executedQty": 0.4, "avgPrice": 100.0}
	at.processPendingEntries(true)
	if at.getPendingEntry("XUSDT", "long") == nil || pe.ProtectedQty != 0 {
		t.Fatal("disappeared coverage cleaned up with stale watermark")
	}
}
func TestMalformedOrRegressingTerminalReceiptsKeepRecovery(t *testing.T) {
	for _, receipt := range []map[string]interface{}{{"status": "CANCELED", "executedQty": "garbage"}, {"status": "FILLED", "executedQty": "NaN", "avgPrice": 100.0}, {"status": "UNKNOWN", "executedQty": 0.0}, {"status": "CANCELED", "executedQty": 0.0}} {
		m := &review03Mock{status: receipt}
		at := review03Trader(m)
		pe := review03Plan()
		pe.ExecutedQty = 0.4
		at.setPendingEntry(pe)
		at.processPendingEntries(true)
		if at.getPendingEntry("XUSDT", "long") == nil {
			t.Fatalf("untrusted receipt erased plan: %v", receipt)
		}
	}
}

func TestDurablePeerReservationSurvivesTraderUnregistration(t *testing.T) {
	first := lifecycleFixture(t)
	first.config.ExchangeID = "shared"
	first.exchangeID = "shared"
	if err := first.store.Trader().Create(&store.Trader{ID: "peer", UserID: "u", ExchangeID: "shared"}); err != nil {
		t.Fatal(err)
	}
	if err := first.store.PendingEntry().Upsert(&store.PendingEntryDB{TraderID: "peer", Symbol: "AUSDT", Side: "long", Price: 100, StopLoss: 90, Quantity: 2, Leverage: 1, OrderID: "peer-entry"}); err != nil {
		t.Fatal(err)
	}
	first.config.StrategyConfig.RiskControl = store.RiskControlConfig{MaxAccountRiskPct: 3, RiskPerTradePct: 2}
	blocked, reason, _ := first.accountRiskExposureBlocks(&kernel.Decision{Action: "open_long", Symbol: "BUSDT", StopLoss: 90, PositionSizeUSD: 200}, 100, &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}})
	if !blocked {
		t.Fatalf("unregistered durable peer reservation ignored: %s", reason)
	}
}

func TestGridRestartResumesOnlyConfirmedUserStop(t *testing.T) {
	for _, reason := range []string{"trader stopped", "daily loss limit", "unresolved pause: trader stopped"} {
		t.Run(reason, func(t *testing.T) {
			at := lifecycleFixture(t)
			at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "XUSDT", GridCount: 10, Leverage: 1, TotalInvestment: 100, LowerPrice: 80, UpperPrice: 120}
			at.trader = &review03Mock{}
			at.gridState = NewGridState(at.config.StrategyConfig.GridConfig)
			at.gridState.IsInitialized, at.gridState.IsPaused, at.gridState.PauseReason = true, true, reason
			at.gridState.Levels = []kernel.GridLevelInfo{{Side: "buy", State: "filled", PositionSize: 0.4, PositionEntry: 100}}
			if err := at.persistGridLedger(); err != nil {
				t.Fatal(err)
			}
			next := &AutoTrader{id: at.id, userID: at.userID, store: at.store, config: at.config, trader: at.trader}
			if err := next.InitializeGrid(); err != nil {
				t.Fatal(err)
			}
			if next.gridState.IsPaused != (reason != "trader stopped") {
				t.Fatalf("unexpected pause after restart: %+v", next.gridState)
			}
			if next.gridState.Levels[0].PositionSize != 0.4 {
				t.Fatal("restart lost exposure")
			}
		})
	}
}

func TestGridEmergencyExitRequiresReceiptAndDoesNotRecloseUnknown(t *testing.T) {
	m := &gridExitMock{review03Mock: &review03Mock{}, exitUnknown: true}
	at := review03Trader(m.review03Mock)
	at.trader = m
	at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "XUSDT"}
	at.gridState = &GridState{OrderBook: map[string]int{}, Levels: []kernel.GridLevelInfo{{Side: "buy", State: "filled", PositionSize: 0.6, PositionEntry: 100}}}
	if err := at.emergencyExit("max drawdown"); err == nil {
		t.Fatal("unknown exit reported success")
	}
	if err := at.emergencyExit("max drawdown"); err == nil || m.closeCalls != 1 {
		t.Fatal("unknown exit resubmitted")
	}
	m.exitUnknown = false
	if err := at.emergencyExit("max drawdown"); err != nil {
		t.Fatal(err)
	}
	if m.closeCalls != 1 || at.gridState.Levels[0].PositionSize != 0 {
		t.Fatal("final receipt failed to close ledger")
	}
}
