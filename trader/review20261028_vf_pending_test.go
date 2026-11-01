package trader

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"nofx/kernel"
	"nofx/market"
	"nofx/store"
	"nofx/trader/binance"
	"nofx/trader/types"
)

// All exchange state is in memory; restart retains only this fake and SQLite.
type vfIntentExchange struct {
	heldMock
	st                   *store.Store
	client               string
	postCalls, queries   int
	crash, failAfterPost bool
	queryErr             error
}

func (m *vfIntentExchange) SetMarginMode(string, bool) error { return nil }
func (m *vfIntentExchange) GetOrderBook(string, int) ([][]float64, [][]float64, error) {
	return [][]float64{{100.99, 10}}, [][]float64{{101.01, 10}}, nil
}
func (m *vfIntentExchange) PlaceLimitOrder(req *types.LimitOrderRequest) (*types.LimitOrderResult, error) {
	m.postCalls++
	m.client = binance.BrIDPrefix + req.ClientID
	m.orders = append(m.orders, types.OpenOrder{Symbol: req.Symbol, OrderID: "ack", ClientID: m.client, Side: req.Side, PositionSide: req.PositionSide, Type: "LIMIT", Price: req.Price, Quantity: req.Quantity, Status: "NEW"})
	if m.crash {
		panic("crash_after_post_ack")
	}
	if m.failAfterPost {
		if err := m.st.GormDB().Exec("CREATE TRIGGER vf_fail_shadow BEFORE INSERT ON trader_pending_entries BEGIN SELECT RAISE(FAIL, 'shadow_insert_failure'); END").Error; err != nil {
			panic(err)
		}
	}
	return &types.LimitOrderResult{OrderID: "ack", ClientID: m.client, Quantity: req.Quantity}, nil
}
func (m *vfIntentExchange) CancelOrder(s, id string) error {
	m.held = .4
	m.status = map[string]interface{}{"orderId": "ack", "status": "CANCELED", "executedQty": .4, "avgPrice": 100.0}
	return m.heldMock.CancelOrder(s, id)
}
func (m *vfIntentExchange) GetOrderStatus(_ string, id string) (map[string]interface{}, error) {
	m.queries++
	if m.queryErr != nil {
		return nil, m.queryErr
	}
	if id != "ack" && id != "client:"+m.client {
		return nil, fmt.Errorf("unexpected order ref %s", id)
	}
	return m.status, nil
}

func vfIntentTrader(t *testing.T, m *vfIntentExchange) *AutoTrader {
	at := review1020Trader(t, m)
	if m.st == nil {
		m.st = at.store
	} else {
		at.store = m.st
	}
	at.marketDataFn = func(string) (*market.Data, error) { return &market.Data{Symbol: "XUSDT", CurrentPrice: 101}, nil }
	at.oiValueFn = func(string) (float64, bool) { return 100, true }
	return at
}

func vfIntentDecision() *kernel.Decision {
	return &kernel.Decision{Symbol: "XUSDT", Action: "open_long_limit", Price: 100, PositionSizeUSD: 100, Leverage: 1, StopLoss: 95, TakeProfit: 115, ExitMode: "range"}
}

func TestReview20261028VFIntentCrashRecovery(t *testing.T) {
	for _, mode := range []string{"crash_after_post_ack", "shadow_insert_failure"} {
		t.Run(mode, func(t *testing.T) {
			m := &vfIntentExchange{crash: mode == "crash_after_post_ack", failAfterPost: mode == "shadow_insert_failure"}
			m.equity = 1000
			at := vfIntentTrader(t, m)
			func() {
				defer func() {
					if p := recover(); p != nil && p != "crash_after_post_ack" {
						panic(p)
					}
				}()
				_ = at.executeOpenLimitLongWithRecord(vfIntentDecision(), &store.DecisionAction{})
			}()
			if m.postCalls != 1 {
				t.Fatalf("fixture did not reach POST: %d", m.postCalls)
			}
			rows, err := m.st.PendingEntry().List(at.id)
			if err != nil || len(rows) != 1 {
				t.Fatalf("POST lost its durable plan: rows=%d err=%v", len(rows), err)
			}
			if rows[0].StopLoss != 95 || rows[0].ExitMode != "range" {
				t.Fatal("incomplete durable plan")
			}
			m.st.GormDB().Exec("DROP TRIGGER IF EXISTS vf_fail_shadow")
			m.crash, m.failAfterPost, m.held = false, false, .4
			m.status = map[string]interface{}{"orderId": "ack", "status": "PARTIALLY_FILLED", "executedQty": .4, "avgPrice": 100.0}
			restarted := vfIntentTrader(t, m)
			restarted.ReconcilePendingEntries()
			if !restarted.isAIManaged("XUSDT", "long") || m.slCalls == 0 || restarted.GetInitialStopLoss("XUSDT", "long") != 95 {
				t.Fatalf("residue .4 unprotected/unmanaged: managed=%v SL=%d anchor=%g", restarted.isAIManaged("XUSDT", "long"), m.slCalls, restarted.GetInitialStopLoss("XUSDT", "long"))
			}
		})
	}
}

func TestReview20261028VFIntentWriteFailurePreventsPost(t *testing.T) {
	m := &vfIntentExchange{}
	m.equity = 1000
	at := vfIntentTrader(t, m)
	if err := at.store.GormDB().Exec("CREATE TRIGGER vf_fail_intent BEFORE INSERT ON trader_pending_entries BEGIN SELECT RAISE(FAIL, 'intent unavailable'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := at.executeOpenLimitLongWithRecord(vfIntentDecision(), &store.DecisionAction{}); err == nil || m.postCalls != 0 {
		t.Fatalf("intent write failure must stop POST: err=%v posts=%d", err, m.postCalls)
	}
}

func TestReview20261028VFOrphanFinalReceiptRetained(t *testing.T) {
	m := &vfIntentExchange{queryErr: errors.New("status unavailable")}
	at := vfIntentTrader(t, m)
	m.client = binance.BrIDPrefix + at.entryClientID()
	m.orders = []types.OpenOrder{{Symbol: "XUSDT", OrderID: "ack", ClientID: m.client, Side: "BUY", PositionSide: "LONG", Type: "LIMIT", Price: 100, Quantity: 1}}
	at.ReconcilePendingEntries()
	rows, err := at.store.PendingEntry().List(at.id)
	// Unknown final state of a planless orphan is escalated, never turned
	// into a planless record whose later recovery would emergency-close.
	if m.queries == 0 || err != nil || len(rows) != 0 || m.closes != 0 {
		t.Fatalf("orphan with unreadable state: queries=%d rows=%d closes=%d err=%v", m.queries, len(rows), m.closes, err)
	}
}

// A rejection the adapter classified (not SubmitUnknown) voids the write-ahead
// intent: no row, no in-memory slot, so the next cycle may retry normally.
type vfRejectExchange struct{ vfIntentExchange }

func (m *vfRejectExchange) PlaceLimitOrder(*types.LimitOrderRequest) (*types.LimitOrderResult, error) {
	m.postCalls++
	return nil, errors.New("-2019 Margin is insufficient")
}

func TestReview20261028VFRejectedSubmitDropsIntent(t *testing.T) {
	m := &vfRejectExchange{}
	m.equity = 1000
	at := vfIntentTrader(t, &m.vfIntentExchange)
	at.trader = m
	if err := at.executeOpenLimitLongWithRecord(vfIntentDecision(), &store.DecisionAction{}); err == nil || m.postCalls != 1 {
		t.Fatalf("expected a rejected submit: err=%v posts=%d", err, m.postCalls)
	}
	rows, err := at.store.PendingEntry().List(at.id)
	if err != nil || len(rows) != 0 || at.getPendingEntry("XUSDT", "long") != nil {
		t.Fatalf("rejected submit left an intent behind: rows=%d err=%v pending=%v", len(rows), err, at.getPendingEntry("XUSDT", "long"))
	}
}

// SubmitUnknown keeps the durable intent keyed by its stable client ID.
type vfUnknownExchange struct{ vfIntentExchange }

func (m *vfUnknownExchange) PlaceLimitOrder(req *types.LimitOrderRequest) (*types.LimitOrderResult, error) {
	m.postCalls++
	return nil, &binance.SubmitUnknownError{ClientOrderID: binance.BrIDPrefix + req.ClientID}
}

func TestReview20261028VFUnknownSubmitKeepsClientIDIntent(t *testing.T) {
	m := &vfUnknownExchange{}
	m.equity = 1000
	at := vfIntentTrader(t, &m.vfIntentExchange)
	at.trader = m
	_ = at.executeOpenLimitLongWithRecord(vfIntentDecision(), &store.DecisionAction{})
	rows, err := at.store.PendingEntry().List(at.id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("unknown submit lost its intent: rows=%d err=%v", len(rows), err)
	}
	r := rows[0]
	if r.State != "submitting" || r.ClientOrderID == "" || r.OrderID != binance.ClientOrderRefPrefix+r.ClientOrderID || r.StopLoss != 95 || r.OriginalR != 5 {
		t.Fatalf("incomplete submitting row: %+v", r)
	}
}

// A submitting row whose status cannot be read at boot must own its resting
// order: the orphan sweep matches it by client ID and never cancels it.
func TestReview20261028VFSubmittingRowOwnsOrderAgainstSweep(t *testing.T) {
	m := &vfIntentExchange{}
	m.equity = 1000
	at := vfIntentTrader(t, m)
	client := binance.BrIDPrefix + at.entryClientID()
	m.client = client
	m.orders = []types.OpenOrder{{Symbol: "XUSDT", OrderID: "ack", ClientID: client, Side: "BUY", PositionSide: "LONG", Type: "LIMIT", Price: 100, Quantity: 1, Status: "NEW"}}
	m.queryErr = errors.New("status temporarily unavailable")
	if err := at.persistPendingEntry(&pendingEntry{State: "submitting", ClientOrderID: client, OrderID: binance.ClientOrderRefPrefix + client,
		Symbol: "XUSDT", Side: "long", Price: 100, Quantity: 1, StopLoss: 95, TakeProfit: 115, PlacedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	at.ReconcilePendingEntries()
	if m.cancelCalls != 0 || at.getPendingEntry("XUSDT", "long") == nil {
		t.Fatalf("submitting row lost ownership: cancels=%d pending=%v", m.cancelCalls, at.getPendingEntry("XUSDT", "long"))
	}
}

// An orphan (no plan row) whose cancel raced a fill: the final receipt is
// read, and the residue is escalated — never auto-claimed or auto-closed (a
// symbol+side close would take the user's manual holdings with it).
func TestReview20261028VFOrphanResidueWithoutPlanIsEscalated(t *testing.T) {
	m := &vfIntentExchange{}
	m.equity = 1000
	at := vfIntentTrader(t, m)
	client := binance.BrIDPrefix + at.entryClientID()
	m.client = client
	m.orders = []types.OpenOrder{{Symbol: "XUSDT", OrderID: "ack", ClientID: client, Side: "BUY", PositionSide: "LONG", Type: "LIMIT", Price: 100, Quantity: 1, Status: "NEW"}}
	m.status = map[string]interface{}{"orderId": "ack", "status": "CANCELED", "executedQty": .4, "avgPrice": 100.0}
	at.ReconcilePendingEntries()
	if m.cancelCalls != 1 || m.queries == 0 || at.isAIManaged("XUSDT", "long") || m.closes != 0 {
		t.Fatalf("orphan residue: cancels=%d queries=%d managed=%v closes=%d", m.cancelCalls, m.queries, at.isAIManaged("XUSDT", "long"), m.closes)
	}
}
