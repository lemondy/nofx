package trader

import (
	"errors"
	"nofx/kernel"
	"nofx/store"
	"nofx/trader/types"
	"path/filepath"
	"testing"
	"time"
)

type review20261001Mock struct {
	Trader
	slErr, tpErr error
	canceled     bool
	orders       []types.OpenOrder
}

func (m *review20261001Mock) SetStopLoss(string, string, float64, float64) error   { return m.slErr }
func (m *review20261001Mock) SetTakeProfit(string, string, float64, float64) error { return m.tpErr }
func (m *review20261001Mock) GetOpenOrders(string) ([]types.OpenOrder, error)      { return m.orders, nil }
func (m *review20261001Mock) GetPositions() ([]map[string]interface{}, error) {
	return []map[string]interface{}{{"symbol": "XUSDT", "side": "long", "positionAmt": 1.0}}, nil
}
func (m *review20261001Mock) CancelStopLossOrders(string) error { m.canceled = true; return nil }
func (m *review20261001Mock) GetOrderStatus(string, string) (map[string]interface{}, error) {
	return map[string]interface{}{"status": "FILLED", "executedQty": 1.0, "avgPrice": 100.0}, nil
}
func review20261001Trader(m *review20261001Mock) *AutoTrader {
	at := riskTestTrader(store.RiskControlConfig{RiskPerTradePct: 1.5})
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.positionExitMode = map[string]string{}
	return at
}
func TestReview20261001HardGateBlockersMustRejectOpen(t *testing.T) {
	for _, code := range []string{"EXTENDED_PUMP_UNCONFIRMED", "DATA_INSUFFICIENT", "POOR_HISTORY", "NEG_EDGE_LOSING_SYMBOL"} {
		t.Run(code, func(t *testing.T) {
			at := review20261001Trader(&review20261001Mock{})
			at.cycleGateStates = map[string]*kernel.GateState{"XUSDT": {LongAllowed: false, LongFailed: []string{code}}}
			d := kernel.Decision{Action: "open_long_limit", Symbol: "XUSDT", Price: 100, StopLoss: 95, TakeProfit: 110, PositionSizeUSD: 100, Leverage: 3, Confidence: 90}
			got := at.applyHardRiskGates([]kernel.Decision{d}, &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}})
			if len(got) != 0 {
				t.Fatalf("hard gate %s blocked long, executor retained %d opening decisions", code, len(got))
			}
		})
	}
}
func TestReview20261001LimitEntriesMustRespectReviewRules(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.Rule().CreateRule("review", &store.RuleInput{RuleType: "hard", Name: "Block high leverage", Condition: `{"field":"leverage","op":">","value":1}`, OnViolation: "block"})
	if err != nil {
		t.Fatal(err)
	}
	at := &AutoTrader{id: "review", store: st}
	for _, action := range []string{"open_long", "open_long_limit"} {
		got := at.preTradeRuleCheck([]kernel.Decision{{Symbol: "XUSDT", Action: action, Leverage: 3}}, 1000)
		if len(got) != 0 {
			t.Errorf("%s bypassed enabled hard leverage rule", action)
		}
	}
}
func TestReview20261001ProtectionFailureMustNotAdvanceWatermark(t *testing.T) {
	m := &review20261001Mock{tpErr: errors.New("simulated TP rejection"), orders: []types.OpenOrder{{Type: "STOP_MARKET", PositionSide: "LONG"}}}
	at := review20261001Trader(m)
	pe := &pendingEntry{Symbol: "XUSDT", Side: "long", Price: 100, Quantity: 1, StopLoss: 95, TakeProfit: 110}
	at.protectExecutedSlice(pe, map[string]interface{}{"executedQty": 1.0, "avgPrice": 100.0})
	if pe.ProtectedQty != 0 {
		t.Fatalf("TP rejected yet protected watermark advanced to %.1f", pe.ProtectedQty)
	}
}
func TestReview20261001FilledSLFailureMustKeepRecoveryPlan(t *testing.T) {
	at := review20261001Trader(&review20261001Mock{slErr: errors.New("simulated SL rejection")})
	at.setPendingEntry(&pendingEntry{Symbol: "XUSDT", Side: "long", Price: 100, Quantity: 1, StopLoss: 95, TakeProfit: 110, OrderID: "test", PlacedAt: time.Now()})
	at.processPendingEntries()
	if at.getPendingEntry("XUSDT", "long") == nil {
		t.Fatal("filled entry's durable recovery plan dropped after SL placement failure")
	}
}
func TestReview20261001StopReplaceFailureMustKeepOldStop(t *testing.T) {
	m := &review20261001Mock{slErr: errors.New("simulated new stop rejection")}
	at := review20261001Trader(m)
	if err := at.moveStopExchange("XUSDT", "long", 99); err == nil {
		t.Fatal("precondition: new stop should fail")
	}
	if m.canceled {
		t.Fatal("old protective stop canceled before failed replacement; position left unprotected")
	}
}
func TestReview20261001DailyLossMustSurviveReload(t *testing.T) {
	rc := store.RiskControlConfig{DailyMaxLossPct: 5}
	old := riskTestTrader(rc)
	old.anchorDailyBaseline(1000)
	if old.dailyLossHaltBlocks(rc, 940) == "" {
		t.Fatal("precondition: 6 percent loss must halt")
	}
	reloaded := riskTestTrader(rc)
	reloaded.anchorDailyBaseline(940)
	if reloaded.dailyLossHaltBlocks(rc, 940) == "" {
		t.Fatal("reload erased daily loss halt after 6 percent equity loss")
	}
}

func TestReview20261001UnknownCandidateMustRejectOpen(t *testing.T) {
	at := review20261001Trader(&review20261001Mock{})
	d := kernel.Decision{Action: "open_long_limit", Symbol: "NOT_IN_POOL_USDT", Price: 100, StopLoss: 95, TakeProfit: 110, PositionSizeUSD: 100, Leverage: 3, Confidence: 90}
	got := at.applyHardRiskGates([]kernel.Decision{d}, &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}, CandidateCoins: []kernel.CandidateCoin{{Symbol: "XUSDT"}}})
	if len(got) != 0 {
		t.Fatal("limit decision outside candidate pool and without gate state survives hard risk gates")
	}
}

type review20261001GridMock struct {
	review20261001Mock
	slCalls int
}

func (m *review20261001GridMock) SetStopLoss(string, string, float64, float64) error {
	m.slCalls++
	return nil
}
func (m *review20261001GridMock) GetBalance() (map[string]interface{}, error) {
	return map[string]interface{}{"totalEquity": 900.0, "totalWalletBalance": 900.0, "totalUnrealizedProfit": -100.0}, nil
}
func TestReview20261001UnsupportedGridMustNotFabricateEntry(t *testing.T) {
	m := &review20261001GridMock{}
	result, err := NewGridTraderAdapter(m).PlaceLimitOrder(&LimitOrderRequest{Symbol: "XUSDT", Side: "BUY", Price: 100, Quantity: 1, ClientID: "review-client"})
	if err == nil {
		t.Fatalf("unsupported native grid entry reported success with synthetic exchange order ID %s; SL calls=%d", result.OrderID, m.slCalls)
	}
}
func TestReview20261001GridDrawdownMustUseActualEquity(t *testing.T) {
	at := &AutoTrader{trader: &review20261001GridMock{}, config: AutoTraderConfig{StrategyConfig: &store.StrategyConfig{GridConfig: &store.GridStrategyConfig{MaxDrawdownPct: 15}}}, gridState: &GridState{PeakEquity: 1000}}
	halted, dd := at.checkMaxDrawdown()
	if halted || dd != 10 {
		t.Fatalf("OKX-shaped balance has true equity 900 / peak 1000 (10%% DD), got %.0f%% halted=%v", dd, halted)
	}
}
