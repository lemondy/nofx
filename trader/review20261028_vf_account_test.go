package trader

import (
	"errors"
	"math"
	"testing"

	"nofx/kernel"
	"nofx/store"
	"nofx/trader/types"
)

type vfAccountExchange struct {
	heldMock
	openErr  error
	allCalls int
}

func (m *vfAccountExchange) GetOpenOrders(symbol string) ([]types.OpenOrder, error) {
	if symbol == "" {
		m.allCalls++
		if m.openErr != nil {
			return nil, m.openErr
		}
	}
	return m.orders, nil
}

func vfManualOrder() types.OpenOrder {
	return types.OpenOrder{Symbol: "MANUALUSDT", OrderID: "manual", ClientID: "ui", Side: "BUY", PositionSide: "LONG", Type: "LIMIT", Price: 100, Quantity: 8, Status: "NEW"}
}

func TestReview20261028VFExternalReservations(t *testing.T) {
	for _, tc := range []string{"manual_800_plus_200", "same_order_id", "client_id_only", "reduce_only", "close_position", "stop_order", "closing_direction", "snapshot_failure"} {
		t.Run(tc, func(t *testing.T) {
			m := &vfAccountExchange{}
			m.equity = 1000
			o := vfManualOrder()
			at := review1020Trader(t, m)
			at.config.StrategyConfig.RiskControl.MaxMarginUsage = .9
			wantBlock := tc == "manual_800_plus_200" || tc == "snapshot_failure"
			switch tc {
			case "same_order_id":
				at.setPendingEntry(&pendingEntry{Symbol: o.Symbol, Side: "long", OrderID: o.OrderID, Price: 100, Quantity: 8, Leverage: 10, StopLoss: 95})
			case "client_id_only":
				at.setPendingEntry(&pendingEntry{Symbol: o.Symbol, Side: "long", OrderID: "client:ui", Price: 100, Quantity: 8, Leverage: 10, StopLoss: 95})
			case "reduce_only":
				o.ReduceOnly = true
			case "close_position":
				o.ClosePosition = true
			case "stop_order":
				o.Type, o.StopPrice = "STOP_MARKET", 90
			case "closing_direction":
				o.Side = "SELL"
			case "snapshot_failure":
				m.openErr = errors.New("exchange unavailable")
			}
			m.orders = []types.OpenOrder{o}
			blocked, why := at.marginBudgetBlocksOpen("NEWUSDT", "long", 200, 1, 1000)
			if blocked != wantBlock {
				t.Fatalf("margin blocked=%v want=%v: %s", blocked, wantBlock, why)
			}
			ctx := &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}}
			at.config.StrategyConfig.RiskControl.MaxAccountRiskPct = 1
			riskBlock, _ := at.accountRiskExposureSnapshotBlocks(&kernel.Decision{Symbol: "NEWUSDT", Action: "open_long"}, 0, ctx, at.accountPendingEntries(), false)
			if (tc == "manual_800_plus_200" || tc == "snapshot_failure") && !riskBlock {
				t.Fatal("external unknown stop-risk was omitted")
			}
			if m.allCalls != 1 {
				t.Fatalf("all-account snapshot must be cached, calls=%d", m.allCalls)
			}
			// The ownership ledger must never acquire manual orders: breakers cancel this ledger.
			for _, pe := range at.accountPendingEntries() {
				if pe != nil && pe.OrderID == "manual" && tc != "same_order_id" {
					t.Fatal("manual order entered cancellation ledger")
				}
			}
			if m.cancelCalls != 0 {
				t.Fatal("manual order was modified")
			}
			if tc == "snapshot_failure" {
				m.held = .4
				at.markAIManaged("XUSDT", "long", "owned")
				if err := at.executeCloseLongWithRecord(&kernel.Decision{Symbol: "XUSDT"}, &store.DecisionAction{}); err != nil || m.closes != 1 {
					t.Fatalf("snapshot failure must allow close: %v, closes=%d", err, m.closes)
				}
			}
		})
	}
}

func TestReview20261028VFMarginSnapshotIncludesManual(t *testing.T) {
	m := &vfAccountExchange{}
	m.orders = []types.OpenOrder{vfManualOrder()}
	at := review1020Trader(t, m)
	blocked, why := at.marginBudgetBlocksSnapshot(200, 1, 1000, 0, 0)
	if !blocked {
		t.Fatalf("grid/batch admission omitted manual margin: %s", why)
	}
	if math.IsNaN(at.pendingMarginReserved("")) {
		t.Fatal("invalid reservations")
	}
}
