package trader

import (
	"errors"
	"fmt"
	"math"
	"nofx/kernel"
	"nofx/store"
	"nofx/trader/types"
	"testing"
	"time"
)

// Fake account only. These regressions preserve the review safety contracts.
type review03Mock struct {
	Trader
	orders                      []types.OpenOrder
	status                      map[string]interface{}
	statusErr, slErr, cancelErr error
	positionsErr                error
	invisible                   bool
	slCalls, tpCalls            int
	equity                      float64
	cancelCalls                 int
}

func (m *review03Mock) GetMarketPrice(string) (float64, error) {
	return 0, errors.New("market data disabled in this fake")
}

func (m *review03Mock) GetPositions() ([]map[string]interface{}, error) {
	if m.positionsErr != nil {
		return nil, m.positionsErr
	}
	return []map[string]interface{}{{"symbol": "XUSDT", "side": "long", "positionAmt": 1.0}}, nil
}
func (m *review03Mock) SetStopLoss(s, p string, q, price float64) error {
	m.slCalls++
	if m.slErr != nil {
		return m.slErr
	}
	if !m.invisible {
		m.orders = append(m.orders, types.OpenOrder{OrderID: fmt.Sprint(m.slCalls), Symbol: s, PositionSide: p, Type: "STOP_MARKET", Quantity: q, StopPrice: price})
	}
	return nil
}
func (m *review03Mock) SetTakeProfit(s, p string, q, price float64) error {
	m.tpCalls++
	if !m.invisible {
		m.orders = append(m.orders, types.OpenOrder{Symbol: s, PositionSide: p, Type: "TAKE_PROFIT_MARKET", Quantity: q, StopPrice: price})
	}
	return nil
}
func (m *review03Mock) GetOpenOrders(string) ([]types.OpenOrder, error) { return m.orders, nil }
func (m *review03Mock) CancelStopLossOrders(string) error {
	keep := []types.OpenOrder{}
	for _, o := range m.orders {
		if o.Type != "STOP_MARKET" {
			keep = append(keep, o)
		}
	}
	m.orders = keep
	return nil
}
func (m *review03Mock) CancelOrder(_ string, id string) error {
	m.cancelCalls++
	if m.cancelErr != nil {
		return m.cancelErr
	}
	keep := []types.OpenOrder{}
	for _, o := range m.orders {
		if o.OrderID != id {
			keep = append(keep, o)
		}
	}
	m.orders = keep
	return nil
}
func (m *review03Mock) CancelAllOrders(string) error { m.cancelCalls++; return m.cancelErr }
func (m *review03Mock) GetBalance() (map[string]interface{}, error) {
	return map[string]interface{}{"totalEquity": m.equity}, nil
}
func (m *review03Mock) GetOrderStatus(string, string) (map[string]interface{}, error) {
	return m.status, m.statusErr
}
func review03Trader(m *review03Mock) *AutoTrader {
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.positionExitMode = map[string]string{}
	return at
}
func review03Plan() *pendingEntry {
	return &pendingEntry{Symbol: "XUSDT", Side: "long", Price: 100, Quantity: 1, StopLoss: 95, TakeProfit: 110, OrderID: "entry", PlacedAt: time.Now()}
}

func TestReview03StopUpdateMustRetainNewProtection(t *testing.T) {
	m := &review03Mock{orders: []types.OpenOrder{{OrderID: "old", Type: "STOP_MARKET", PositionSide: "LONG", StopPrice: 95, Quantity: 1}}}
	at := review03Trader(m)
	if err := at.moveStopExchange("XUSDT", "long", 99); err != nil {
		t.Fatal(err)
	}
	for _, o := range m.orders {
		if o.OrderID == "old" {
			t.Fatal("old stop not retired")
		}
	}
	if !protectiveLegAtPrice(m, "XUSDT", "LONG", "SL", 99) {
		t.Fatal("successful stop update canceled the new stop together with the old stop")
	}
}
func TestReview03VerificationFailureMustKeepWatermark(t *testing.T) {
	m := &review03Mock{invisible: true}
	at := review03Trader(m)
	pe := review03Plan()
	at.protectExecutedSlice(pe, map[string]interface{}{"executedQty": 1.0, "avgPrice": 100.0})
	if pe.ProtectedQty != 0 {
		t.Fatalf("exchange has no protective orders, but ProtectedQty=%v", pe.ProtectedQty)
	}
}
func TestReview03AdditionalPartialFillMustIncreaseCoverage(t *testing.T) {
	m := &review03Mock{}
	at := review03Trader(m)
	pe := review03Plan()
	at.protectExecutedSlice(pe, map[string]interface{}{"executedQty": 0.4, "avgPrice": 100.0})
	at.protectExecutedSlice(pe, map[string]interface{}{"executedQty": 0.8, "avgPrice": 100.0})
	qty := 0.0
	for _, o := range m.orders {
		if o.Type == "STOP_MARKET" {
			qty += o.Quantity
		}
	}
	if math.Abs(qty-0.8) > 1e-9 {
		t.Fatalf("executed=0.8, watermark=%v, but actual SL coverage=%v; price-only dedupe skipped new slice", pe.ProtectedQty, qty)
	}
}
func TestReview03CanceledPartialFailureMustKeepPlan(t *testing.T) {
	m := &review03Mock{slErr: errors.New("simulated SL rejection"), status: map[string]interface{}{"status": "CANCELED", "executedQty": 0.4, "avgPrice": 100.0}}
	at := review03Trader(m)
	at.setPendingEntry(review03Plan())
	at.processPendingEntries()
	if at.getPendingEntry("XUSDT", "long") == nil {
		t.Fatal("canceled partially-filled order lost recovery plan despite failed SL")
	}
}
func TestReview03CancelUnknownFinalFillMustKeepPlan(t *testing.T) {
	m := &review03Mock{statusErr: errors.New("simulated final status timeout")}
	at := review03Trader(m)
	pe := review03Plan()
	at.setPendingEntry(pe)
	at.cancelPending(pe)
	if at.getPendingEntry("XUSDT", "long") == nil {
		t.Fatal("cancel succeeded but final fill is unknown; durable recovery plan was deleted")
	}
}
func TestReview03FilledWithoutReceiptMustKeepPlan(t *testing.T) {
	m := &review03Mock{status: map[string]interface{}{"status": "FILLED", "executedQty": 0.0, "avgPrice": 0.0}}
	at := review03Trader(m)
	at.setPendingEntry(review03Plan())
	at.processPendingEntries()
	if at.getPendingEntry("XUSDT", "long") == nil {
		t.Fatal("FILLED without quantity/price dropped pending plan and reported protected without placing any SL/TP")
	}
}
func TestReview03PauseGridMustReportCancelFailure(t *testing.T) {
	m := &review03Mock{cancelErr: errors.New("simulated cancel rejection")}
	at := review03Trader(m)
	at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "XUSDT"}
	at.gridState = &GridState{Levels: []kernel.GridLevelInfo{{State: "pending", OrderID: "entry"}}}
	if err := at.pauseGrid("review"); err == nil {
		t.Fatal("grid pause reported success while exchange entry cancellation failed")
	}
}
func TestReview03NetLossesMustTriggerLossStreak(t *testing.T) {
	now := time.Now().UnixMilli()
	rows := []store.TraderPosition{}
	for i := 0; i < 3; i++ {
		rows = append(rows, store.TraderPosition{RealizedPnL: 0.01, Fee: 0.1, ExitTime: now - int64(i+1)*60000})
	}
	if blocked, _ := lossStreakVerdict(rows, 3, now); !blocked {
		t.Fatal("three consecutive net losses reset the breaker because gross PnL is positive")
	}
}

func TestReview03GridPartialCancelMustRetainExposure(t *testing.T) {
	m := &review03Mock{status: map[string]interface{}{"status": "CANCELED", "executedQty": 0.4, "avgPrice": 100.0}}
	at := review03Trader(m)
	at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "XUSDT"}
	at.gridState = &GridState{Levels: []kernel.GridLevelInfo{{State: "pending", OrderID: "entry", OrderQuantity: 1, Price: 100}}}
	at.syncGridState()
	level := at.gridState.Levels[0]
	if level.State == "empty" || math.Abs(level.PositionSize-0.4) > 1e-9 {
		t.Fatalf("canceled order filled 0.4 but grid discarded exposure: %+v", level)
	}
}
func TestReview03GridDailyBaselineMustSurviveReload(t *testing.T) {
	m := &review03Mock{equity: 1000}
	at := review03Trader(m)
	at.gridState = &GridState{}
	at.gridEquityDailyLossPct()
	m.equity = 900
	before, _ := at.gridEquityDailyLossPct()
	at.gridState = NewGridState(&store.GridStrategyConfig{Symbol: "XUSDT"})
	after, _ := at.gridEquityDailyLossPct()
	if before != 10 || after != 10 {
		t.Fatalf("same-day reload reset actual 10%% loss: before=%v%% after=%v%%", before, after)
	}
}
func TestReview03StopGridMustCancelRestingEntries(t *testing.T) {
	m := &review03Mock{}
	at := review03Trader(m)
	at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "XUSDT"}
	at.gridState = &GridState{OrderBook: map[string]int{"entry": 0}, Levels: []kernel.GridLevelInfo{{State: "pending", OrderID: "entry", OrderQuantity: 1, Price: 100}}}
	at.Stop()
	if m.cancelCalls == 0 {
		t.Fatal("stopped grid trader left its resting entries on the exchange")
	}
}
func TestReview03GridUnknownPositionMustBlockNewRisk(t *testing.T) {
	m := &review03Mock{positionsErr: errors.New("simulated position timeout")}
	at := review03Trader(m)
	at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "XUSDT", TotalInvestment: 100, Leverage: 1}
	at.gridState = &GridState{}
	if allowed, _, _ := at.checkTotalPositionLimit("XUSDT", 10); allowed {
		t.Fatal("unreadable actual position treated as zero and additional grid exposure allowed")
	}
}
func TestReview03AccountCapMustIncludeOtherTraderPendingRisk(t *testing.T) {
	cfg := store.RiskControlConfig{MaxAccountRiskPct: 3, RiskPerTradePct: 2}
	first, second := riskTestTrader(cfg), riskTestTrader(cfg)
	first.config.ExchangeID = "same-account"
	second.config.ExchangeID = "same-account"
	first.setPendingEntry(&pendingEntry{Symbol: "AUSDT", Side: "long", Price: 100, StopLoss: 90, Quantity: 2}) // account risk=20
	ctx := &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}}
	candidate := &kernel.Decision{Action: "open_long", Symbol: "BUSDT", Price: 100, StopLoss: 90, PositionSizeUSD: 200} // another 20
	if blocked, _, _ := second.accountRiskExposureBlocks(candidate, 100, ctx); !blocked {
		t.Fatal("same-account trader sees no other pending risk: 20+20 exceeds account cap 30 but gate passed")
	}
}
