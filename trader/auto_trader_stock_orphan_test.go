package trader

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

// Tests for booking kept live entries that filled across a switch to paper
// (review 2026-10-22): idempotent booking into the live ledger, never an order,
// persistent deduplicated alerts, protection resumed after switching back.

const orphanLimitDecision = `[{"symbol":"AAPLBUSDT","action":"open_long","entry_type":"limit","limit_price":119,"stop_loss":115,"take_profit":130,"confidence":80,"thesis":"t","invalidation":"i","reasoning":"r"}]`

// orphanFixture places a live resting limit entry, then restarts the trader in
// paper mode with exec as the temporary orphan executor. status is the order's
// state when the paper instance looks at it. prepare is NOT run yet.
func orphanFixture(t *testing.T, status map[string]interface{}, exec func(*fakeSpot) types.SpotStockTrader) *stockFixture {
	t.Helper()
	fx := newStockFixture(t, false, stockTestNow, func(int) string { return orphanLimitDecision })
	fx.cycle(t)
	pend, _ := fx.at.store.StockPaper().ListPending("t1")
	if len(pend) != 1 {
		t.Fatalf("pending not placed: %+v", pend)
	}
	fx.spot.statuses[pend[0].OrderID] = status
	fx.at.StopForRestart()
	paper := true
	fx.at.config.StrategyConfig.StockConfig.PaperTrading = &paper
	fx.at.stockTrader = nil
	old := stockOrphanExecutor
	t.Cleanup(func() { stockOrphanExecutor = old })
	e := types.SpotStockTrader(fx.spot)
	if exec != nil {
		e = exec(fx.spot)
	}
	stockOrphanExecutor = func(*AutoTrader) (types.SpotStockTrader, error) { return e, nil }
	fx.at.isRunning = true
	return fx
}

func (fx *stockFixture) placedNothingNew(t *testing.T, buysBefore int) {
	t.Helper()
	if len(fx.spot.buys) != buysBefore || len(fx.spot.sells) != 0 || len(fx.spot.setProt) != 0 || fx.spot.zeroSells != 0 {
		t.Fatalf("paper instance placed orders: buys=%v sells=%v setProt=%v", fx.spot.buys, fx.spot.sells, fx.spot.setProt)
	}
}

func hasAlert(fx *stockFixture, sub string) int {
	n := 0
	for _, a := range fx.alert {
		if strings.HasPrefix(a, "ALERT|") && strings.Contains(a, sub) {
			n++
		}
	}
	return n
}

// The filled shares are booked into the live ledger with the first-protection
// intent; no order is placed; the entry is forgotten; the API shows them.
func TestStockOrphanFillBookedWithoutOrders(t *testing.T) {
	fx := orphanFixture(t, pendingStatus("FILLED", 3, 357, 0), nil)
	buysBefore := len(fx.spot.buys)
	fx.spot.hold["AAPLBUSDT"] = 3
	if _, err := fx.at.prepareStockRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if qty, entry := ownedQty(t, fx); !near(qty, 3) || !near(entry, 119) {
		t.Fatalf("owned %v @ %v, want 3 @ 119", qty, entry)
	}
	rows, _ := fx.at.store.Position().StockOwnedRows("t1", "AAPLBUSDT")
	if len(rows) != 1 || !near(rows[0].InitialStopLoss, 115) {
		t.Fatalf("opening stop not recorded for the later protection: %+v", rows)
	}
	if intents, _ := fx.at.store.StockPaper().ListProtectionIntents("t1"); !intents["AAPLBUSDT"] {
		t.Fatalf("protection intent missing: %v", intents)
	}
	if pend, _ := fx.at.store.StockPaper().ListPending("t1"); len(pend) != 0 {
		t.Fatalf("booked terminal entry must be forgotten: %+v", pend)
	}
	fx.placedNothingNew(t, buysBefore)
	if hasAlert(fx, "无保护单") != 1 {
		t.Fatalf("want exactly one unprotected alert: %v", fx.alert)
	}
	info, err := fx.at.stockAccountInfo()
	if err != nil {
		t.Fatal(err)
	}
	un, _ := info["unprotected_live_positions"].([]stockUnprotectedPosition)
	if len(un) != 1 || un[0].Symbol != "AAPLBUSDT" || !near(un[0].Quantity, 3) || !near(un[0].Stop, 115) {
		t.Fatalf("API state must expose the unprotected holding: %+v", info["unprotected_live_positions"])
	}
}

// Replaying the same terminal status (crash between booking and forgetting the
// row, or a second start) never books the fill twice.
func TestStockOrphanFillBookingIsIdempotent(t *testing.T) {
	fx := orphanFixture(t, pendingStatus("FILLED", 3, 357, 0), nil)
	fx.spot.hold["AAPLBUSDT"] = 3
	if _, err := fx.at.prepareStockRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Crash leftover: the booking committed (watermark advanced) but the row
	// was not deleted yet.
	if err := fx.at.store.StockPaper().UpsertPending(&store.StockPendingEntry{TraderID: "t1", Symbol: "AAPLBUSDT", OrderID: "1001",
		Action: "open_long", Quantity: 3, FilledQty: 3, FilledQuote: 357, LimitPrice: 119, Stop: 115, PlacedAt: stockTestNow.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	fx.at.cancelOrphanStockPendings("replay")
	if qty, _ := ownedQty(t, fx); !near(qty, 3) {
		t.Fatalf("replay double booked: %v", qty)
	}
	if pend, _ := fx.at.store.StockPaper().ListPending("t1"); len(pend) != 0 {
		t.Fatalf("settled entry must be forgotten: %+v", pend)
	}
	// A stale in-memory copy of the row (old watermark) replayed against the
	// kept row is a no-op as well.
	stale := &store.StockPendingEntry{TraderID: "t1", Symbol: "AAPLBUSDT", OrderID: "1001", Quantity: 3, Stop: 115}
	if err := fx.at.store.StockPaper().UpsertPending(&store.StockPendingEntry{TraderID: "t1", Symbol: "AAPLBUSDT", OrderID: "1001",
		Action: "open_long", Quantity: 3, FilledQty: 3, FilledQuote: 357, Stop: 115, PlacedAt: stockTestNow.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	line, err := fx.at.stockBookOrphanFill(fx.spot, stale, pendingStatus("FILLED", 3, 357, 0), stockTestNow)
	if err != nil || line != "" {
		t.Fatalf("stale replay booked: %q %v", line, err)
	}
	if qty, _ := ownedQty(t, fx); !near(qty, 3) {
		t.Fatalf("stale replay double booked: %v", qty)
	}
}

// A partial fill whose order is still not terminal after the cancel is booked
// (idempotently) but the row is kept for the next retry.
func TestStockOrphanPartialFillNonTerminalKeepsRow(t *testing.T) {
	fx := orphanFixture(t, pendingStatus("PARTIALLY_FILLED", 2, 238, 0), func(f *fakeSpot) types.SpotStockTrader {
		return &stickyStatusSpot{fakeSpot: f}
	})
	fx.spot.hold["AAPLBUSDT"] = 2
	if _, err := fx.at.prepareStockRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if qty, _ := ownedQty(t, fx); !near(qty, 2) {
		t.Fatalf("partial fill not booked: %v", qty)
	}
	pend, _ := fx.at.store.StockPaper().ListPending("t1")
	if len(pend) != 1 || !near(pend[0].FilledQty, 2) {
		t.Fatalf("row must stay with the advanced watermark: %+v", pend)
	}
	// Retry while still not terminal: nothing new is booked.
	fx.at.cancelOrphanStockPendings("retry")
	if qty, _ := ownedQty(t, fx); !near(qty, 2) {
		t.Fatalf("retry double booked: %v", qty)
	}
	// Once terminal with more shares, only the delta is booked, then forgotten.
	fx.spot.statuses["1001"] = pendingStatus("CANCELED", 3, 359, 0)
	fx.at.stockOrphanOverride(t, fx.spot)
	fx.at.cancelOrphanStockPendings("terminal")
	if qty, _ := ownedQty(t, fx); !near(qty, 3) {
		t.Fatalf("delta not booked: %v", qty)
	}
	if pend, _ := fx.at.store.StockPaper().ListPending("t1"); len(pend) != 0 {
		t.Fatalf("terminal entry must be forgotten: %+v", pend)
	}
	fx.placedNothingNew(t, len(fx.spot.buys))
}

// A cancel that errors because the order is already terminal (retry of an
// earlier pass) is not a failure and does not alert as one.
func TestStockOrphanCancelErrorOnTerminalOrderIsSettled(t *testing.T) {
	fx := orphanFixture(t, pendingStatus("CANCELED", 0, 0, 0), func(f *fakeSpot) types.SpotStockTrader {
		return &cancelFailsSpot{fakeSpot: f}
	})
	if _, err := fx.at.prepareStockRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pend, _ := fx.at.store.StockPaper().ListPending("t1"); len(pend) != 0 {
		t.Fatalf("terminal order must be forgotten: %+v", pend)
	}
	if hasAlert(fx, "无人管理") != 0 {
		t.Fatalf("unexpected failure alert: %v", fx.alert)
	}
}

// Unprotected real shares are re-announced once per interval (deduplicated)
// until the protection intent is gone.
func TestStockUnprotectedLiveReminderDedup(t *testing.T) {
	fx := orphanFixture(t, pendingStatus("FILLED", 3, 357, 0), nil)
	fx.spot.hold["AAPLBUSDT"] = 3
	if _, err := fx.at.prepareStockRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	base := hasAlert(fx, "无保护单")
	at := fx.at
	at.stockRemindUnprotectedLive(stockTestNow) // starts the window
	at.stockRemindUnprotectedLive(stockTestNow.Add(30 * time.Minute))
	if hasAlert(fx, "无保护单") != base {
		t.Fatalf("alert repeated inside the window: %v", fx.alert)
	}
	at.stockRemindUnprotectedLive(stockTestNow.Add(61 * time.Minute))
	at.stockRemindUnprotectedLive(stockTestNow.Add(90 * time.Minute))
	if got := hasAlert(fx, "无保护单"); got != base+1 {
		t.Fatalf("want one reminder after the window, got %d (base %d): %v", got, base, fx.alert)
	}
	at.stockRemindUnprotectedLive(stockTestNow.Add(125 * time.Minute))
	if got := hasAlert(fx, "无保护单"); got != base+2 {
		t.Fatalf("reminders must keep coming: %d", got)
	}
	// Handled (intent cleared): silence.
	if err := at.store.StockPaper().ClearProtectionIntent("t1", "AAPLBUSDT"); err != nil {
		t.Fatal(err)
	}
	at.stockRemindUnprotectedLive(stockTestNow.Add(200 * time.Minute))
	if got := hasAlert(fx, "无保护单"); got != base+2 {
		t.Fatalf("reminder after resolution: %d", got)
	}
}

// Switching back to live: the booked holding is protected from the persisted
// opening stop by the normal protection tick.
func TestStockOrphanFillProtectedAfterSwitchBackToLive(t *testing.T) {
	fx := orphanFixture(t, pendingStatus("FILLED", 3, 357, 0), nil)
	fx.spot.hold["AAPLBUSDT"] = 3
	if _, err := fx.at.prepareStockRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	live := false
	fx.at.config.StrategyConfig.StockConfig.PaperTrading = &live
	fx.at.stockTrader = fx.spot
	fx.at.trader = fx.spot
	if _, err := fx.at.prepareStockRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	fx.at.stockProtectionTick(context.Background())
	if len(fx.spot.setProt) != 1 || fx.spot.setProt[0].Symbol != "AAPLBUSDT" || !near(fx.spot.setProt[0].Qty, 3) || !near(fx.spot.setProt[0].Stop, 115) {
		t.Fatalf("protection not placed from the booked intent: %+v", fx.spot.setProt)
	}
	if intents, _ := fx.at.store.StockPaper().ListProtectionIntents("t1"); intents["AAPLBUSDT"] {
		t.Fatalf("intent must clear once protected: %v", intents)
	}
	if un, _, _ := fx.at.stockUnprotectedLive(); len(un) != 0 {
		t.Fatalf("still reported unprotected: %+v", un)
	}
}

// stockOrphanOverride re-points the orphan executor (the status/seam of a
// retry) at e.
func (at *AutoTrader) stockOrphanOverride(t *testing.T, e types.SpotStockTrader) {
	t.Helper()
	old := stockOrphanExecutor
	t.Cleanup(func() { stockOrphanExecutor = old })
	stockOrphanExecutor = func(*AutoTrader) (types.SpotStockTrader, error) { return e, nil }
}

// stickyStatusSpot never lets a cancelled order become terminal (PENDING_CANCEL
// style) until the test changes the status itself.
type stickyStatusSpot struct{ *fakeSpot }

func (s *stickyStatusSpot) CancelOrder(symbol, orderID string) error {
	s.cancelled = append(s.cancelled, symbol+":"+orderID)
	return nil
}

// cancelFailsSpot rejects every cancel (the order is already gone).
type cancelFailsSpot struct{ *fakeSpot }

func (s *cancelFailsSpot) CancelOrder(symbol, orderID string) error {
	return fmt.Errorf("unknown order")
}
