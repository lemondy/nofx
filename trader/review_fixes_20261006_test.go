package trader

import (
	"testing"

	"nofx/store"
	"nofx/trader/types"
)

// Deployment-gate fix for review 2026-10-06 P0-1: the protection watchdog
// classified every AI-managed SHORT as "quantity unavailable" because Binance
// keeps positionAmt negative for shorts — a permanent per-symbol protection
// fault that blocked ALL new account risk and skipped the position's own
// stop repair / crossed-stop exit. Mirror tests: the same scenario must
// behave identically for a negative-quantity short (exchange shape) and a
// positive-quantity long, and short qty must be normalized to positive for
// every downstream consumer (coverage math, stop placement, exits).

type fix06Mock struct {
	audit05Mock
	symbol, side string
	qty          float64
	closeShorts  int
	lastSLQty    float64
}

func (m *fix06Mock) SetStopLoss(symbol, positionSide string, quantity, stopPrice float64) error {
	m.slCalls++
	m.lastSLQty = quantity
	return nil
}

func (m *fix06Mock) GetPositions() ([]map[string]interface{}, error) {
	if m.flat {
		return nil, nil
	}
	return []map[string]interface{}{{
		"symbol": m.symbol, "side": m.side, "positionAmt": m.qty,
		"entryPrice": 100.0, "markPrice": m.mark, "leverage": 2.0,
	}}, nil
}

func (m *fix06Mock) CloseShort(string, float64) (map[string]interface{}, error) {
	m.closeShorts++
	if m.closeErr != nil {
		return nil, m.closeErr
	}
	if m.flatOnClose {
		m.flat = true
	}
	return map[string]interface{}{}, nil
}

// Fully protected negative-quantity short: no fault, no repair, protection
// recognized — and the account stays free to open new risk elsewhere.
func TestFix06NegativeQtyShortFullyProtected(t *testing.T) {
	m := &fix06Mock{audit05Mock: audit05Mock{mark: 100, orders: []types.OpenOrder{
		{Type: "STOP_MARKET", PositionSide: "SHORT", Side: "BUY", StopPrice: 105, Quantity: 1},
		{Type: "TAKE_PROFIT_MARKET", PositionSide: "SHORT", Side: "BUY", StopPrice: 90, Quantity: 1},
	}}, symbol: "XUSDT", side: "short", qty: -1}
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	for i := 0; i < 2; i++ { // two watchdog rounds: the fault must never appear
		at.processProtectionWatchdog()
	}
	if at.protectionFaultReason() != "" {
		t.Fatalf("negative-qty short with full protection raised a fault: %v (P0-1)", at.protectionFaultReason())
	}
	if m.slCalls != 0 || m.closeShorts != 0 {
		t.Fatalf("protected short must not be repaired or exited: sl=%d close=%d", m.slCalls, m.closeShorts)
	}
	if got := at.GetRecordedStopLoss("XUSDT", "short"); got != 105 {
		t.Fatalf("recorded SL not seeded from exchange leg: %v", got)
	}
	if err := at.entryExecutionBlocked("YUSDT", "short"); err != nil {
		t.Fatalf("healthy short position blocked all new account risk: %v", err)
	}
}

// Insufficient SL coverage on a negative-quantity short must be REPAIRED
// (pre-fix: the watchdog skipped the position entirely — coverage math never
// even saw the position). The repair quantity must be the ABSOLUTE size 1.
func TestFix06NegativeQtyShortMissingSLRepaired(t *testing.T) {
	m := &fix06Mock{audit05Mock: audit05Mock{mark: 100, orders: []types.OpenOrder{
		{Type: "STOP_MARKET", PositionSide: "SHORT", Side: "BUY", StopPrice: 105, Quantity: 0.4},
		{Type: "TAKE_PROFIT_MARKET", PositionSide: "SHORT", Side: "BUY", StopPrice: 90, Quantity: 1},
	}}, symbol: "XUSDT", side: "short", qty: -1}
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.processProtectionWatchdog()
	if m.slCalls == 0 {
		t.Fatal("negative-qty short with 0.4/1.0 SL coverage was not repaired — watchdog skipped it (P0-1 shape)")
	}
	if m.lastSLQty != 0.6 {
		t.Fatalf("repair quantity = %v, want the coverage GAP 0.6 (1 planned − 0.4 held; pre-fix the negative positionAmt never even reached this math)", m.lastSLQty)
	}
	if got := at.GetRecordedStopLoss("XUSDT", "short"); got != 105 {
		t.Fatalf("recorded SL after repair = %v, want the leg 105", got)
	}
}

// Price crossing a recorded short SL must submit the program exit.
func TestFix06NegativeQtyShortCrossedStopExits(t *testing.T) {
	m := &fix06Mock{audit05Mock: audit05Mock{mark: 110, orders: []types.OpenOrder{
		{Type: "TAKE_PROFIT_MARKET", PositionSide: "SHORT", Side: "BUY", StopPrice: 90, Quantity: 1},
	}}, symbol: "XUSDT", side: "short", qty: -1}
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.SetRecordedStopLoss("XUSDT", "short", 105)
	at.SetInitialStopLoss("XUSDT", "short", 105)
	at.processProtectionWatchdog()
	if m.closeShorts == 0 {
		t.Fatal("mark 110 crossed recorded short SL 105 — no emergency exit submitted")
	}
}

// Mirror sanity: the same positive-quantity scenarios behave the same, and
// quantity handed to protection placement is the ABSOLUTE size.
func TestFix06PositiveQtyMirrorAndAbsQuantity(t *testing.T) {
	m := &fix06Mock{audit05Mock: audit05Mock{mark: 100, orders: []types.OpenOrder{
		{Type: "TAKE_PROFIT_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 110, Quantity: 1},
	}}, symbol: "XUSDT", side: "long", qty: 1}
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.SetRecordedStopLoss("XUSDT", "long", 95)
	at.SetInitialStopLoss("XUSDT", "long", 95)
	at.processProtectionWatchdog()
	if m.slCalls == 0 {
		t.Fatal("positive-qty long without SL must still be repaired")
	}
	if m.lastSLQty != 1 {
		t.Fatalf("SL placement quantity = %v, want absolute 1", m.lastSLQty)
	}
}


// P2-7: the watchdog must issue ONE GetOpenOrders per unique symbol per pass
// — hedge mode puts LONG+SHORT rows of the same symbol in the loop and the
// per-row call doubled the request on exactly those.
func TestFix3WatchdogDedupesOpenOrdersBySymbol(t *testing.T) {
	m := &fix06MultiMock{}
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.processProtectionWatchdog()
	if m.orderCalls != 1 {
		t.Fatalf("GetOpenOrders called %d times for one symbol with two sides, want 1", m.orderCalls)
	}
}

type fix06MultiMock struct {
	audit05Mock
	orderCalls int
}

func (m *fix06MultiMock) GetPositions() ([]map[string]interface{}, error) {
	return []map[string]interface{}{
		{"symbol": "XUSDT", "side": "long", "positionAmt": 1.0, "entryPrice": 100.0, "markPrice": 100.0, "leverage": 2.0},
		{"symbol": "XUSDT", "side": "short", "positionAmt": -1.0, "entryPrice": 100.0, "markPrice": 100.0, "leverage": 2.0},
	}, nil
}

func (m *fix06MultiMock) GetOpenOrders(string) ([]types.OpenOrder, error) {
	m.orderCalls++
	return []types.OpenOrder{
		{Type: "STOP_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 95, Quantity: 1},
		{Type: "TAKE_PROFIT_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 110, Quantity: 1},
		{Type: "STOP_MARKET", PositionSide: "SHORT", Side: "BUY", StopPrice: 105, Quantity: 1},
		{Type: "TAKE_PROFIT_MARKET", PositionSide: "SHORT", Side: "BUY", StopPrice: 90, Quantity: 1},
	}, nil
}
