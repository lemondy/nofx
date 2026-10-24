package trader

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/kernel"
	"nofx/market"
	"nofx/market/breakout"
	"nofx/store"
)

func c2Pending() *pendingEntry {
	return &pendingEntry{Symbol: "XUSDT", Side: "long", Price: 100, Quantity: 1,
		StopLoss: 95, TakeProfit: 110, OrderID: "entry", PlacedAt: time.Now()}
}

func TestC2PendingExcludedBeforeDirectionData(t *testing.T) {
	at := c2Trader(&c2PendingMock{})
	at.config.StrategyConfig.CoinSource.ExcludedCoins = []string{" \tx-usdt \n"}
	at.cycleGateStates = map[string]*kernel.GateState{"XUSDT": {LongAllowed: false}}
	fetched := false
	at.recheckDataFn = func(string) (*market.Data, error) { fetched = true; return nil, errors.New("offline data") }
	// A pre-existing direction ban must not conceal the exclusion reason.
	if reason := at.pendingDirectionBlocked(c2Pending()); !strings.Contains(reason, "EXCLUDED_COIN") || fetched {
		t.Fatalf("exclusion must precede direction data: reason=%q fetched=%v", reason, fetched)
	}
}

func TestC2PendingExclusionCancelsAndProtects(t *testing.T) {
	for _, fullRecheck := range []bool{false, true} {
		for _, terminal := range []bool{false, true} {
			for _, executed := range []float64{0, 0.4} {
				status := "NEW"
				if executed > 0 {
					status = "PARTIALLY_FILLED"
				}
				m := &c2PendingMock{audit05Mock: audit05Mock{cancelTerminal: terminal, orders: c2ProtectionOrders(),
					status: map[string]interface{}{"status": status, "executedQty": executed, "avgPrice": 100.0}}}
				at := c2Trader(m)
				pe := c2Pending()
				at.setPendingEntry(pe) // exclusion added after the order was placed
				at.config.StrategyConfig.CoinSource.ExcludedCoins = []string{" x-usdt "}
				at.recheckDataFn = func(string) (*market.Data, error) { t.Fatal("excluded entry fetched direction data"); return nil, nil }
				at.processPendingEntries(fullRecheck)
				if m.cancelCalls != 1 || m.statusCalls < 1 {
					t.Fatalf("recheck=%v terminal=%v qty=%v: cancellation/receipt missing: %+v", fullRecheck, terminal, executed, m)
				}
				if (at.getPendingEntry(pe.Symbol, pe.Side) == nil) != terminal {
					t.Fatalf("pending ownership lost before terminal confirmation: terminal=%v", terminal)
				}
				if executed > 0 && (pe.ProtectedQty != executed || !at.isAIManaged(pe.Symbol, pe.Side) || m.closeCalls != 0) {
					t.Fatalf("excluded partial fill lost protection: %+v close=%d", pe, m.closeCalls)
				}
			}
		}
	}
}

func TestC2ExcludedPendingRestoredFromDB(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "pending.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := &c2PendingMock{audit05Mock: audit05Mock{cancelTerminal: true, orders: c2ProtectionOrders(),
		status: map[string]interface{}{"status": "PARTIALLY_FILLED", "executedQty": 0.4, "avgPrice": 100.0}}}
	at := c2Trader(m)
	at.id, at.store = "c2-restart", st
	pe := c2Pending()
	at.setPendingEntry(pe)
	at.pendingEntries = nil // simulate restart, durable shadow row survives
	at.config.StrategyConfig.CoinSource.ExcludedCoins = []string{" xUSDT "}
	at.ReconcilePendingEntries()
	rows, err := st.PendingEntry().List(at.id)
	if err != nil {
		t.Fatal(err)
	}
	if m.cancelCalls != 1 || len(rows) != 0 || at.getPendingEntry(pe.Symbol, pe.Side) != nil || !at.isAIManaged(pe.Symbol, pe.Side) || m.closeCalls != 0 {
		t.Fatalf("excluded restored entry not safely retired: cancels=%d rows=%d close=%d", m.cancelCalls, len(rows), m.closeCalls)
	}
}

func TestC2ExcludedPendingRetainedUntilProtectionAndReceiptVerify(t *testing.T) {
	for _, unavailableReceipt := range []bool{false, true} {
		m := &c2PendingMock{audit05Mock: audit05Mock{cancelTerminal: true,
			status: map[string]interface{}{"status": "PARTIALLY_FILLED", "executedQty": 0.4, "avgPrice": 100.0}}}
		at := c2Trader(m)
		pe := c2Pending()
		at.setPendingEntry(pe)
		at.config.StrategyConfig.CoinSource.ExcludedCoins = []string{"XUSDT"}
		if unavailableReceipt {
			m.statusErr = errors.New("final receipt unavailable")
		}
		// No verified protective orders: a terminal cancellation alone must
		// not retire the plan. An unreadable receipt must still attempt cancel.
		at.processPendingEntries(false)
		if m.cancelCalls != 1 || at.getPendingEntry(pe.Symbol, pe.Side) == nil || pe.ProtectedQty != 0 || m.closeCalls != 0 {
			t.Fatalf("unverified excluded entry abandoned: unavailable=%v cancel=%d protected=%v close=%d", unavailableReceipt, m.cancelCalls, pe.ProtectedQty, m.closeCalls)
		}
	}
}

func TestC2PendingBreakdownTopConfirmation(t *testing.T) {
	for _, tc := range []struct {
		universe        string
		confirmed, lost bool
	}{
		{"breakdown", false, false}, {"breakdown", true, false},
		{"gainer", false, true}, {"near_high", false, true}, {"", false, true}, {"gainer", true, false},
	} {
		shorts := []breakout.ShortSignal{{Symbol: "xusdt", Universe: tc.universe, Confirmed: tc.confirmed}}
		if got := pendingShortTopConfirmationLost("XUSDT", shorts); got != tc.lost {
			t.Errorf("universe=%q confirmed=%v lost=%v want=%v", tc.universe, tc.confirmed, got, tc.lost)
		}
	}
	if pendingShortTopConfirmationLost("XUSDT", []breakout.ShortSignal{{Symbol: "OTHERUSDT"}}) {
		t.Fatal("another symbol's confirmation affected pending")
	}
}
