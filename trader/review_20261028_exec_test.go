package trader

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"nofx/kernel"
	"nofx/market"
	"nofx/store"
	"nofx/trader/binance"
	"nofx/trader/types"
)

// review 2026-10-28 (execution layer): protection-order state consistency.
// #1 exchange-authoritative tighten-only, #3/#6 TP sizing on the lot grid and
// re-sizing after reductions, #4 one quantity change per round, #5 runner flag
// follows execution, #7 unknown reduce outcomes never repeat, #8 fill risk
// uses the tick-rounded stop.

// exec1028Mock is an in-memory Binance-like account: one position, resting
// protective legs, and a call log. Methods it does not define panic (nil
// embedded Trader) so an unexpected exchange call fails the test loudly.
type exec1028Mock struct {
	Trader
	symbol, side string
	qty          float64 // signed positionAmt
	mark, entry  float64
	orders       []types.OpenOrder
	step, minQty float64
	tick         float64

	slPrices    []float64
	tpQtys      []float64
	tpPrices    []float64
	closes      []float64
	cancelTP    int
	cancelByID  []string
	closeErr    error
	closeResp   map[string]interface{}
	nextOrderID int
}

func (m *exec1028Mock) GetBalance() (map[string]interface{}, error) {
	return map[string]interface{}{"totalEquity": 900.0}, nil
}

func (m *exec1028Mock) GetPositions() ([]map[string]interface{}, error) {
	if m.qty == 0 {
		return nil, nil
	}
	entry := m.entry
	if entry == 0 {
		entry = 100
	}
	return []map[string]interface{}{{
		"symbol": m.symbol, "side": m.side, "positionAmt": m.qty,
		"entryPrice": entry, "markPrice": m.mark, "leverage": 2.0,
	}}, nil
}

func (m *exec1028Mock) GetOpenOrders(string) ([]types.OpenOrder, error) {
	return append([]types.OpenOrder(nil), m.orders...), nil
}

func (m *exec1028Mock) exitSide(ps string) string {
	if strings.EqualFold(ps, "LONG") {
		return "SELL"
	}
	return "BUY"
}

func (m *exec1028Mock) SetStopLoss(symbol, ps string, qty, price float64) error {
	m.slPrices = append(m.slPrices, price)
	m.nextOrderID++
	m.orders = append(m.orders, types.OpenOrder{
		OrderID: fmt.Sprintf("sl%d", m.nextOrderID), Symbol: symbol, PositionSide: ps, Side: m.exitSide(ps),
		Type: "STOP_MARKET", StopPrice: price, ClosePosition: true, Algo: true,
	})
	return nil
}

func (m *exec1028Mock) SetTakeProfit(symbol, ps string, qty, price float64) error {
	m.tpQtys = append(m.tpQtys, qty)
	m.tpPrices = append(m.tpPrices, price)
	m.nextOrderID++
	m.orders = append(m.orders, types.OpenOrder{
		OrderID: fmt.Sprintf("tp%d", m.nextOrderID), Symbol: symbol, PositionSide: ps, Side: m.exitSide(ps),
		Type: "TAKE_PROFIT_MARKET", StopPrice: price, Quantity: qty, ClosePosition: qty <= 0, Algo: true,
	})
	return nil
}

func (m *exec1028Mock) dropTP() {
	kept := m.orders[:0:0]
	for _, o := range m.orders {
		if !strings.Contains(strings.ToUpper(o.Type), "TAKE_PROFIT") {
			kept = append(kept, o)
		}
	}
	m.orders = kept
}

func (m *exec1028Mock) CancelTakeProfitOrdersForSide(string, string) error {
	m.cancelTP++
	m.dropTP()
	return nil
}

func (m *exec1028Mock) CancelTakeProfitOrders(string) error {
	m.cancelTP++
	m.dropTP()
	return nil
}

func (m *exec1028Mock) CancelProtectiveOrder(_ string, o types.OpenOrder) error {
	m.cancelByID = append(m.cancelByID, o.OrderID)
	kept := m.orders[:0:0]
	for _, x := range m.orders {
		if x.OrderID != o.OrderID {
			kept = append(kept, x)
		}
	}
	m.orders = kept
	return nil
}

func (m *exec1028Mock) closeResult(qty float64) (map[string]interface{}, error) {
	m.closes = append(m.closes, qty)
	if m.closeErr != nil {
		return nil, m.closeErr
	}
	if m.qty > 0 {
		m.qty -= qty
	} else if m.qty < 0 {
		m.qty += qty
	}
	out := map[string]interface{}{}
	for k, v := range m.closeResp {
		out[k] = v
	}
	return out, nil
}
func (m *exec1028Mock) CloseLong(_ string, qty float64) (map[string]interface{}, error) {
	return m.closeResult(qty)
}
func (m *exec1028Mock) CloseShort(_ string, qty float64) (map[string]interface{}, error) {
	return m.closeResult(qty)
}

func (m *exec1028Mock) QuantityRules(string) (float64, float64, error) {
	if m.step <= 0 {
		return 0, 0, errors.New("rules unavailable")
	}
	return m.step, m.minQty, nil
}

func (m *exec1028Mock) QuantizeTriggerPrice(_ string, price float64) (float64, error) {
	if m.tick <= 0 {
		return 0, errors.New("tick unavailable")
	}
	return math.Round(price/m.tick) * m.tick, nil
}

func exec1028Trader(rc store.RiskControlConfig, m *exec1028Mock) *AutoTrader {
	at := riskTestTrader(rc)
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.positionExitMode = map[string]string{}
	at.tpRunnerDoneMap = map[string]bool{}
	at.r1TrimDone = map[string]bool{}
	at.tpTrimDone = map[string]bool{}
	return at
}

func exec1028SL(price float64) types.OpenOrder {
	return types.OpenOrder{OrderID: "sl0", Type: "STOP_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: price, ClosePosition: true, Algo: true}
}

func exec1028TP(id string, price, qty float64) types.OpenOrder {
	return types.OpenOrder{OrderID: id, Type: "TAKE_PROFIT_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: price, Quantity: qty, ClosePosition: qty <= 0, Algo: true}
}

var exec1028Trend = store.RiskControlConfig{TrailingStopEnabled: true}

// ---------------------------------------------------------------------------
// #1 exchange-authoritative tighten-only
// ---------------------------------------------------------------------------

// The review's scenario: memory SL 90, the user tightened the exchange stop to
// 110, mark 120, program wants 102 (> memory 90). The tighter leg must stay,
// nothing is sent, and memory is pulled UP to 110 (anchor untouched).
func TestExec1028_MoveStopKeepsTighterExchangeStop(t *testing.T) {
	for _, tc := range []struct {
		side                              string
		qty, mem, exch, newSL, mark, kept float64
	}{
		{"long", 1, 90, 110, 102, 120, 110},
		{"short", -1, 110, 90, 98, 80, 90},
	} {
		t.Run(tc.side, func(t *testing.T) {
			ps := strings.ToUpper(tc.side)
			m := &exec1028Mock{symbol: "XUSDT", side: tc.side, qty: tc.qty, mark: tc.mark}
			m.orders = []types.OpenOrder{{OrderID: "manual", Type: "STOP_MARKET", PositionSide: ps, Side: m.exitSide(ps), StopPrice: tc.exch, ClosePosition: true, Algo: true}}
			at := exec1028Trader(store.RiskControlConfig{}, m)
			at.SetRecordedStopLoss("XUSDT", tc.side, tc.mem)
			at.SetInitialStopLoss("XUSDT", tc.side, tc.mem)

			err := at.moveStopExchange("XUSDT", tc.side, tc.newSL)
			kept, ok := keptTighterStop(err)
			if !ok || kept != tc.kept {
				t.Fatalf("want kept-tighter(%v), got %v", tc.kept, err)
			}
			if len(m.slPrices) != 0 || len(m.cancelByID) != 0 {
				t.Fatalf("the tighter exchange stop was touched: sl=%v cancels=%v", m.slPrices, m.cancelByID)
			}
			if got := at.GetRecordedStopLoss("XUSDT", tc.side); got != tc.kept {
				t.Fatalf("recorded stop not synced to the exchange: %v", got)
			}
			if got := at.GetInitialStopLoss("XUSDT", tc.side); got != tc.mem {
				t.Fatalf("initial R anchor must not move: %v", got)
			}
			// The caller-facing helper reports the stop actually in force, so
			// callers never write their (looser) request back to memory.
			at.SetRecordedStopLoss("XUSDT", tc.side, tc.mem) // fresh decision round
			eff, err := at.moveProfitStopEffective("XUSDT", tc.side, tc.newSL, math.Abs(tc.qty))
			if err != nil || eff != tc.kept {
				t.Fatalf("effective stop = %v err=%v, want %v", eff, err, tc.kept)
			}
		})
	}
}

// A stop that is NOT tighter than the request still moves normally, and tick
// rounding alone (exchange 102.0 vs request 101.96 -> 102.0) is not "tighter".
func TestExec1028_MoveStopStillMovesWhenExchangeNotTighter(t *testing.T) {
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 1, mark: 120, tick: 0.1}
	m.orders = []types.OpenOrder{exec1028SL(100)}
	at := exec1028Trader(store.RiskControlConfig{}, m)
	at.SetRecordedStopLoss("XUSDT", "long", 100)
	if err := at.moveStopExchange("XUSDT", "long", 102); err != nil {
		t.Fatalf("a real tighten must proceed: %v", err)
	}
	if len(m.slPrices) != 1 || m.slPrices[0] != 102 {
		t.Fatalf("SL not placed: %v", m.slPrices)
	}

	m2 := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 1, mark: 120, tick: 0.1}
	m2.orders = []types.OpenOrder{exec1028SL(102.0)}
	at2 := exec1028Trader(store.RiskControlConfig{}, m2)
	at2.SetRecordedStopLoss("XUSDT", "long", 100)
	if err := at2.moveStopExchange("XUSDT", "long", 101.96); err != nil {
		t.Fatalf("exchange stop equal to the tick-rounded request is not tighter: %v", err)
	}
	if len(m2.slPrices) != 1 {
		t.Fatalf("tick-equal request must still be applied/verified: %v", m2.slPrices)
	}
}

// Which resting orders count as "a tighter stop on this side": one-way BOTH
// rows count (the retire loop would cancel them), TP legs / the opposite hedge
// side / entry stops (wrong exit direction) / already-terminal rows do not.
func TestExec1028_ExchangeTighterStopFilter(t *testing.T) {
	legs := []types.OpenOrder{
		{OrderID: "tp", Type: "TAKE_PROFIT_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 130},
		{OrderID: "other-side", Type: "STOP_MARKET", PositionSide: "SHORT", Side: "BUY", StopPrice: 130},
		{OrderID: "entry-stop", Type: "STOP_MARKET", PositionSide: "BOTH", Side: "BUY", StopPrice: 130},
		{OrderID: "done", Type: "STOP_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 130, Status: "CANCELED"},
		{OrderID: "loose", Type: "STOP_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 95},
	}
	if p, ok := exchangeTighterStop(legs, "long", 102); ok {
		t.Fatalf("no qualifying tighter stop, got %v", p)
	}
	legs = append(legs, types.OpenOrder{OrderID: "oneway", Type: "STOP_MARKET", PositionSide: "BOTH", Side: "SELL", StopPrice: 108})
	if p, ok := exchangeTighterStop(legs, "long", 102); !ok || p != 108 {
		t.Fatalf("one-way BOTH stop must count: %v %v", p, ok)
	}
}

// The R-rules record the stop actually in force: with a manual stop already
// past breakeven, the 1R lock keeps it (memory = exchange), places nothing,
// and still trims (breakeven is confirmed by the tighter leg).
func TestExec1028_BreakevenKeepsTighterExchangeStop(t *testing.T) {
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 10, mark: 106,
		closeResp: map[string]interface{}{"executedQty": 5.0}}
	m.orders = []types.OpenOrder{exec1028SL(95), {OrderID: "manual", Type: "STOP_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 101.5, ClosePosition: true, Algo: true}}
	at := exec1028Trader(store.RiskControlConfig{ProfitLockAtR: 1, MinPositionSize: 1}, m)
	at.SetInitialStopLoss("XUSDT", "long", 95)
	at.SetRecordedStopLoss("XUSDT", "long", 95)
	pos := map[string]interface{}{"symbol": "XUSDT", "side": "long", "positionAmt": 10.0, "entryPrice": 100.0, "markPrice": 106.0}
	at.processRProfitRules(pos, 106)
	if got := at.GetRecordedStopLoss("XUSDT", "long"); got != 101.5 {
		t.Fatalf("recorded stop = %v, want the exchange's 101.5 (never the looser breakeven request)", got)
	}
	if len(m.slPrices) != 0 {
		t.Fatalf("breakeven stop was placed over a tighter one: %v", m.slPrices)
	}
	if len(m.closes) != 1 || m.closes[0] != 5 {
		t.Fatalf("1R trim should still proceed on a confirmed-tight stop: closes=%v", m.closes)
	}
}

// ---------------------------------------------------------------------------
// #3/#6 TP sizing on the lot grid
// ---------------------------------------------------------------------------

func TestExec1028_QuantizeTPQty(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		pos, frac, step, min, want float64
	}{
		{"half_of_even", 10, 0.5, 1, 1, 5},
		{"review_tail_3x50pct_rounds_to_step", 3, 0.5, 1, 1, 2}, // 1.5 -> 2, never 1.5 (the unplaceable tail)
		{"remainder_after_trim", 5, 0.5, 1, 1, 3},               // 2.5 -> 3
		{"fine_step", 5, 0.5, 0.1, 0.1, 2.5},
		{"at_least_one_step", 1, 0.5, 1, 1, 1},
		{"min_qty_floor", 4, 0.25, 1, 3, 3},
		{"capped_at_position", 2, 0.9, 1, 5, 2},
		{"full_fraction_is_whole_position", 7.3, 1, 1, 1, 7.3},
		{"no_step_keeps_raw_product", 3, 0.5, 0, 0, 1.5},
		{"non_positive_position", 0, 0.5, 1, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quantizeTPQty(tc.pos, tc.frac, tc.step, tc.min); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("quantizeTPQty(%v,%v,%v,%v) = %v, want %v", tc.pos, tc.frac, tc.step, tc.min, got, tc.want)
			}
		})
	}
}

// #6: a sub-step shortfall is covered; chasing it used to fail the lot filter
// (and the adapter then degraded to a whole-position TP). A full step missing
// is still placed.
func TestExec1028_TPTailIsNotMissingProtection(t *testing.T) {
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 3, mark: 100, step: 1, minQty: 1}
	m.orders = []types.OpenOrder{exec1028TP("tp0", 110, 2)}
	if err := ensureProtectiveCoverage(m, "XUSDT", "LONG", "TP", 110, 2.5); err != nil {
		t.Fatal(err)
	}
	if len(m.tpQtys) != 0 {
		t.Fatalf("a 0.5 tail (< one 1.0 step) must not be re-placed: %v", m.tpQtys)
	}
	if err := ensureProtectiveCoverage(m, "XUSDT", "LONG", "TP", 110, 4); err != nil {
		t.Fatal(err)
	}
	if len(m.tpQtys) != 1 || m.tpQtys[0] != 2 {
		t.Fatalf("a full missing step must be placed (gap 2): %v", m.tpQtys)
	}
}

// #6 + #5 at placement: the planned split TP for 3 @ 50% is 2 (lot grid), the
// runner flag is NOT set merely because a TP is resting, and the ledger holds
// the evidence for later fill detection.
func TestExec1028_PlacementQuantizedAndRunnerNotMarked(t *testing.T) {
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 3, mark: 100, step: 1, minQty: 1}
	at := exec1028Trader(exec1028Trend, m)
	d := &kernel.Decision{Symbol: "XUSDT", Action: "open_long", StopLoss: 95, TakeProfit: 110, ExitMode: kernel.ExitModeTrend}
	slErr, tpErr := at.placeProtectiveOrders(d, "LONG", 3, 100, 100)
	if slErr != nil || tpErr != nil {
		t.Fatalf("placement failed: sl=%v tp=%v", slErr, tpErr)
	}
	if len(m.tpQtys) != 1 || m.tpQtys[0] != 2 {
		t.Fatalf("split TP quantity = %v, want the lot-grid value 2", m.tpQtys)
	}
	if at.tpRunnerDone("XUSDT_long") {
		t.Fatal("review #5: a resting split TP must not flag the runner as taken over")
	}
	if e, ok := at.getTPLedger("XUSDT_long"); !ok || e.posQty != 3 || e.tpQty != 2 {
		t.Fatalf("ledger = %+v ok=%v, want {3 2}", e, ok)
	}
}

// Pending partial-fill path: when earlier slices already shed the WHOLE TP
// share (tpDone covers it) that IS an observed fill -> runner. A residual TP
// still due keeps the runner clear.
func TestExec1028_PendingTPDoneMarksRunnerOnlyWhenShareRealized(t *testing.T) {
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 5, mark: 100, step: 1, minQty: 1}
	at := exec1028Trader(exec1028Trend, m)
	d := &kernel.Decision{Symbol: "XUSDT", Action: "open_long", StopLoss: 95, TakeProfit: 110, ExitMode: kernel.ExitModeTrend}
	if slErr, tpErr := at.placeProtectiveOrdersTPDone(d, "LONG", 10, 100, 100, 5); slErr != nil || tpErr != nil {
		t.Fatalf("sl=%v tp=%v", slErr, tpErr)
	}
	if !at.tpRunnerDone("XUSDT_long") {
		t.Fatal("the whole TP share was realized by earlier slices: remainder is the runner")
	}

	m2 := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 8, mark: 100, step: 1, minQty: 1}
	at2 := exec1028Trader(exec1028Trend, m2)
	if slErr, tpErr := at2.placeProtectiveOrdersTPDone(d, "LONG", 10, 100, 100, 2); slErr != nil || tpErr != nil {
		t.Fatalf("sl=%v tp=%v", slErr, tpErr)
	}
	if len(m2.tpQtys) != 1 || m2.tpQtys[0] != 3 {
		t.Fatalf("residual TP = %v, want 3 (5 planned − 2 realized)", m2.tpQtys)
	}
	if at2.tpRunnerDone("XUSDT_long") {
		t.Fatal("a residual TP is still due: runner must stay clear")
	}
}

// ---------------------------------------------------------------------------
// #3 watchdog: TP re-sized to the live remainder
// ---------------------------------------------------------------------------

func exec1028Watchdog(t *testing.T, mode string, qty float64, orders []types.OpenOrder) (*AutoTrader, *exec1028Mock) {
	t.Helper()
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: qty, mark: 100, step: 1, minQty: 1, orders: orders}
	at := exec1028Trader(exec1028Trend, m)
	at.positionExitMode["XUSDT_long"] = mode
	at.SetRecordedStopLoss("XUSDT", "long", 95)
	at.SetInitialStopLoss("XUSDT", "long", 95)
	at.recordOpenTakeProfit("XUSDT", "long", 110)
	return at, m
}

func TestExec1028_WatchdogResizesOversizedTP(t *testing.T) {
	for _, tc := range []struct {
		name string
		tp   types.OpenOrder
	}{
		{"quantity_sized_for_old_position", exec1028TP("tp0", 110, 5)},
		{"whole_position_closePosition_leg", exec1028TP("tp0", 110, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Position trimmed 10 -> 5; trend share 50% of the remainder = 2.5 -> 3.
			at, m := exec1028Watchdog(t, kernel.ExitModeTrend, 5, []types.OpenOrder{exec1028SL(95), tc.tp})
			at.processProtectionWatchdog()
			if m.cancelTP != 1 {
				t.Fatalf("oversized TP not retired: cancels=%d", m.cancelTP)
			}
			if len(m.tpQtys) != 1 || m.tpQtys[0] != 3 {
				t.Fatalf("TP re-armed at %v, want the quantized target 3", m.tpQtys)
			}
			if at.tpRunnerDone("XUSDT_long") {
				t.Fatal("re-arming a TP is not a runner takeover")
			}
			// Converged: a second pass changes nothing.
			at.processProtectionWatchdog()
			if m.cancelTP != 1 || len(m.tpQtys) != 1 {
				t.Fatalf("not idempotent: cancels=%d placements=%v", m.cancelTP, m.tpQtys)
			}
		})
	}
}

// Full-close templates (range/quick): a whole-position leg is RIGHT, an
// exactly-sized leg is RIGHT, but a leg sized for a bigger position is not.
func TestExec1028_WatchdogFullModeOnlyShrinksOversizedQtyLegs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tp         types.OpenOrder
		wantCancel int
		wantQty    float64
	}{
		{"closePosition_leg_ok", exec1028TP("tp0", 110, 0), 0, 0},
		{"exact_qty_ok", exec1028TP("tp0", 110, 5), 0, 0},
		{"leg_for_bigger_position", exec1028TP("tp0", 110, 12), 1, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, m := exec1028Watchdog(t, kernel.ExitModeRange, 5, []types.OpenOrder{exec1028SL(95), tc.tp})
			at.processProtectionWatchdog()
			if m.cancelTP != tc.wantCancel {
				t.Fatalf("cancels=%d want %d", m.cancelTP, tc.wantCancel)
			}
			if tc.wantCancel == 0 && len(m.tpQtys) != 0 {
				t.Fatalf("a correct TP was re-placed: %v", m.tpQtys)
			}
			if tc.wantCancel == 1 && (len(m.tpQtys) != 1 || m.tpQtys[0] != tc.wantQty) {
				t.Fatalf("re-armed at %v, want %v", m.tpQtys, tc.wantQty)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// #5 runner flag follows execution
// ---------------------------------------------------------------------------

// The split TP fills (leg gone AND the position shrank by its size): the
// watchdog flags the runner and does NOT carve a new TP out of it.
func TestExec1028_WatchdogMarksRunnerOnlyAfterTPFill(t *testing.T) {
	at, m := exec1028Watchdog(t, kernel.ExitModeTrend, 5, []types.OpenOrder{exec1028SL(95)})
	at.recordTPLedger("XUSDT_long", 10, 5) // 10 held, 5 resting at placement
	at.processProtectionWatchdog()
	if !at.tpRunnerDone("XUSDT_long") {
		t.Fatal("TP leg gone and position shrank by its size: that is a fill — runner")
	}
	if len(m.tpQtys) != 0 || m.cancelTP != 0 {
		t.Fatalf("a runner must not get a fresh TP: placements=%v cancels=%d", m.tpQtys, m.cancelTP)
	}
	// The anti-"repeated halving" guard: every later pass (price pulling back
	// below the TP level, ledger already dropped) still leaves the runner alone.
	for i := 0; i < 3; i++ {
		at.processProtectionWatchdog()
	}
	if len(m.tpQtys) != 0 || m.cancelTP != 0 {
		t.Fatalf("runner got a TP on a later pass: placements=%v cancels=%d", m.tpQtys, m.cancelTP)
	}
}

// The TP merely VANISHED (cancelled/expired/manual) while the position is
// intact: no runner flag, the watchdog re-arms it. (Pre-fix the placement-time
// flag made the watchdog skip it forever.)
func TestExec1028_WatchdogRearmsVanishedSplitTP(t *testing.T) {
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 10, mark: 100, step: 1, minQty: 1, orders: []types.OpenOrder{exec1028SL(95)}}
	at := exec1028Trader(exec1028Trend, m)
	at.positionExitMode["XUSDT_long"] = kernel.ExitModeTrend
	d := &kernel.Decision{Symbol: "XUSDT", Action: "open_long", StopLoss: 95, TakeProfit: 110, ExitMode: kernel.ExitModeTrend}
	at.SetRecordedStopLoss("XUSDT", "long", 95)
	at.SetInitialStopLoss("XUSDT", "long", 95)
	if slErr, tpErr := at.placeProtectiveOrders(d, "LONG", 10, 100, 100); slErr != nil || tpErr != nil {
		t.Fatalf("sl=%v tp=%v", slErr, tpErr)
	}
	if len(m.tpQtys) != 1 || m.tpQtys[0] != 5 {
		t.Fatalf("initial TP = %v", m.tpQtys)
	}
	m.dropTP() // the TP leg is cancelled away; position untouched
	at.processProtectionWatchdog()
	if at.tpRunnerDone("XUSDT_long") {
		t.Fatal("a vanished (not filled) TP must not flag the runner")
	}
	if len(m.tpQtys) != 2 || m.tpQtys[1] != 5 {
		t.Fatalf("vanished split TP was not re-armed: %v", m.tpQtys)
	}
}

// A 1R/vol reduction is not the TP filling: booked against the ledger it can
// never be mistaken for a runner takeover (the old trigger for repeated
// halving was the missing flag; the new guard is this ledger).
func TestExec1028_TrimIsNotMistakenForTPFill(t *testing.T) {
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 5, mark: 100, step: 1, minQty: 1, orders: []types.OpenOrder{exec1028SL(95)}}
	at := exec1028Trader(exec1028Trend, m)
	at.positionExitMode["XUSDT_long"] = kernel.ExitModeTrend
	at.SetRecordedStopLoss("XUSDT", "long", 95)
	at.SetInitialStopLoss("XUSDT", "long", 95)
	at.recordOpenTakeProfit("XUSDT", "long", 110)
	at.recordTPLedger("XUSDT_long", 10, 5)
	at.noteTPLedgerReduction("XUSDT_long", 5) // the position shrank because of a TRIM
	at.processProtectionWatchdog()
	if at.tpRunnerDone("XUSDT_long") {
		t.Fatal("a trimmed position with its TP gone is NOT a filled TP")
	}
	if len(m.tpQtys) != 1 || m.tpQtys[0] != 3 {
		t.Fatalf("TP should be re-armed for the remainder (3), got %v", m.tpQtys)
	}
}

// Ledger entries die with the position.
func TestExec1028_LedgerPrunedWhenPositionGone(t *testing.T) {
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 0, mark: 100}
	at := exec1028Trader(exec1028Trend, m)
	at.recordTPLedger("XUSDT_long", 10, 5)
	at.processProtectionWatchdog()
	if _, ok := at.getTPLedger("XUSDT_long"); ok {
		t.Fatal("stale ledger entry survived its position")
	}
}

// ---------------------------------------------------------------------------
// #3 re-size TP right after a confirmed reduction
// ---------------------------------------------------------------------------

func exec1028Trim(t *testing.T, resp map[string]interface{}, qty float64, orders []types.OpenOrder) (*AutoTrader, *exec1028Mock) {
	t.Helper()
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: qty, mark: 106, step: 1, minQty: 1, closeResp: resp, orders: orders}
	at := exec1028Trader(store.RiskControlConfig{ProfitLockAtR: 1, MinPositionSize: 1, TrailingStopEnabled: true}, m)
	at.positionExitMode["XUSDT_long"] = kernel.ExitModeTrend
	at.SetInitialStopLoss("XUSDT", "long", 95)
	at.SetRecordedStopLoss("XUSDT", "long", 95)
	at.recordOpenTakeProfit("XUSDT", "long", 120)
	return at, m
}

func exec1028Pos(qty float64) map[string]interface{} {
	return map[string]interface{}{"symbol": "XUSDT", "side": "long", "positionAmt": qty, "entryPrice": 100.0, "markPrice": 106.0}
}

func TestExec1028_1RTrimResizesTPToRemainder(t *testing.T) {
	// 12 held, TP leg 6 (50% at entry). 1R trims 6 (confirmed) -> remainder 6,
	// TP share 3. The stale 6-lot TP would otherwise sell the WHOLE runner.
	at, m := exec1028Trim(t, map[string]interface{}{"executedQty": 6.0}, 12, []types.OpenOrder{exec1028SL(95), exec1028TP("tp0", 120, 6)})
	at.recordTPLedger("XUSDT_long", 12, 6)
	at.processRProfitRules(exec1028Pos(12), 106)
	if len(m.closes) != 1 || m.closes[0] != 6 {
		t.Fatalf("trim = %v", m.closes)
	}
	if m.cancelTP != 1 || len(m.tpQtys) != 1 || m.tpQtys[0] != 3 {
		t.Fatalf("TP not re-sized to the remainder: cancels=%d placements=%v", m.cancelTP, m.tpQtys)
	}
	if e, _ := at.getTPLedger("XUSDT_long"); e.posQty != 6 || e.tpQty != 3 {
		t.Fatalf("ledger = %+v, want {6 3}", e)
	}
	if at.tpRunnerDone("XUSDT_long") {
		t.Fatal("trim must not flag the runner")
	}
}

// ACK-only receipt (no confirmed fill): leave the re-arm to the watchdog, but
// still book the reduction so it cannot read as a TP fill.
func TestExec1028_1RTrimUnconfirmedFillLeavesTPToWatchdog(t *testing.T) {
	at, m := exec1028Trim(t, map[string]interface{}{}, 12, []types.OpenOrder{exec1028SL(95), exec1028TP("tp0", 120, 6)})
	at.recordTPLedger("XUSDT_long", 12, 6)
	at.processRProfitRules(exec1028Pos(12), 106)
	if len(m.closes) != 1 {
		t.Fatalf("trim = %v", m.closes)
	}
	if m.cancelTP != 0 || len(m.tpQtys) != 0 {
		t.Fatalf("no confirmed fill: TP must be left alone (cancels=%d placements=%v)", m.cancelTP, m.tpQtys)
	}
	if e, _ := at.getTPLedger("XUSDT_long"); e.posQty != 6 {
		t.Fatalf("reduction not booked on the ledger: %+v", e)
	}
	// ...and the watchdog then converges: leg 6 vs remainder 6 -> target 3.
	at.processProtectionWatchdog()
	if m.cancelTP != 1 || len(m.tpQtys) != 1 || m.tpQtys[0] != 3 {
		t.Fatalf("watchdog did not converge: cancels=%d placements=%v", m.cancelTP, m.tpQtys)
	}
}

// The split TP had ALREADY filled before the 1R trim: the remainder is the
// runner, so the trim must not re-arm a TP against it.
func TestExec1028_1RTrimAfterTPFillDoesNotRearm(t *testing.T) {
	at, m := exec1028Trim(t, map[string]interface{}{"executedQty": 3.0}, 6, []types.OpenOrder{exec1028SL(95)})
	at.recordTPLedger("XUSDT_long", 12, 6) // TP of 6 was resting at 12 held; now 6 held, leg gone
	at.processRProfitRules(exec1028Pos(6), 106)
	if len(m.closes) != 1 {
		t.Fatalf("trim = %v", m.closes)
	}
	if !at.tpRunnerDone("XUSDT_long") {
		t.Fatal("TP fill evidence (leg gone, 6 shed) must flag the runner")
	}
	if len(m.tpQtys) != 0 || m.cancelTP != 0 {
		t.Fatalf("re-armed a TP against the runner: cancels=%d placements=%v", m.cancelTP, m.tpQtys)
	}
}

// ---------------------------------------------------------------------------
// #7 unknown reduce outcomes never repeat
// ---------------------------------------------------------------------------

func exec1028Unknown() error {
	return fmt.Errorf("failed to close long position: %w", &binance.SubmitUnknownError{Symbol: "XUSDT", ClientOrderID: "br_x"})
}

func TestExec1028_1RTrimUnknownConsumesMarkerAndDoesNotRepeat(t *testing.T) {
	at, m := exec1028Trim(t, nil, 10, []types.OpenOrder{exec1028SL(95)})
	m.closeErr = exec1028Unknown()
	at.processRProfitRules(exec1028Pos(10), 106)
	at.processRProfitRules(exec1028Pos(10), 106) // next round, response was lost, position now 5 on the exchange
	at.processRProfitRules(exec1028Pos(5), 106)
	if len(m.closes) != 1 {
		t.Fatalf("an UNKNOWN trim was repeated: closes=%v (50%% then 50%% of the rest = 75%%)", m.closes)
	}
	at.tpTrimMutex.Lock()
	done := at.r1TrimDone["XUSDT_long"]
	at.tpTrimMutex.Unlock()
	if !done {
		t.Fatal("r1TrimDone must be consumed on UNKNOWN")
	}
}

// Contrast: a definite failure still retries next round (unchanged contract).
func TestExec1028_1RTrimDefiniteFailureStillRetries(t *testing.T) {
	at, m := exec1028Trim(t, nil, 10, []types.OpenOrder{exec1028SL(95)})
	m.closeErr = errors.New("insufficient margin")
	at.processRProfitRules(exec1028Pos(10), 106)
	at.processRProfitRules(exec1028Pos(10), 106)
	if len(m.closes) != 2 {
		t.Fatalf("definite failure must retry: closes=%v", m.closes)
	}
}

// ---------------------------------------------------------------------------
// #4 one quantity-changing action per position per round
// ---------------------------------------------------------------------------

type exec1028VolMock struct {
	batch1RiskMock
	closes []float64
	resp   map[string]interface{}
	err    error
}

func (m *exec1028VolMock) CloseLong(_ string, qty float64) (map[string]interface{}, error) {
	m.closes = append(m.closes, qty)
	if m.err != nil {
		return nil, m.err
	}
	out := map[string]interface{}{}
	for k, v := range m.resp {
		out[k] = v
	}
	return out, nil
}

// Vol-target reduce at mark 120 sells down to the target; the same round's 1R
// lock must NOT size a 50% trim off the stale pre-reduce quantity (pre-fix:
// second order qty*0.5 of the OLD size; with a 2/3 reduce that is 3/2 of what
// remained — the position is flattened).
func exec1028VolRound(t *testing.T, rc store.RiskControlConfig, err error) (*AutoTrader, *exec1028VolMock, float64) {
	t.Helper()
	data := freshnessData("XUSDT", 120, allFreshLags)
	atrPct := oneHourATRPct(data)
	if atrPct <= 0 {
		t.Fatalf("fixture ATR unavailable")
	}
	target := volTargetNotional(900, 1.5, atrPct)
	qty := 3 * target / 120 // 300% of target -> "reduce" zone
	m := &exec1028VolMock{batch1RiskMock: batch1RiskMock{fix06Mock: fix06Mock{
		audit05Mock: audit05Mock{mark: 120}, symbol: "XUSDT", side: "long", qty: qty,
	}}, err: err}
	at := riskTestTrader(rc)
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.positionExitMode = map[string]string{"XUSDT_long": kernel.ExitModeTrend}
	at.r1TrimDone = map[string]bool{}
	at.tpTrimDone = map[string]bool{}
	at.SetInitialStopLoss("XUSDT", "long", 90)
	at.SetRecordedStopLoss("XUSDT", "long", 90)
	at.marketDataFn = func(string) (*market.Data, error) { return freshnessData("XUSDT", 120, allFreshLags), nil }
	at.processVolTargetAndTrailing()
	return at, m, qty
}

func TestExec1028_VolReduceSkipsStaleSnapshotRRules(t *testing.T) {
	at, m, qty := exec1028VolRound(t, store.RiskControlConfig{VolTargetEnabled: true, ProfitLockAtR: 1, MinPositionSize: 1}, nil)
	if len(m.closes) != 1 {
		t.Fatalf("closes=%v: one quantity-changing action per round (pre-fix the R trim stacked a second order off qty=%v)", m.closes, qty)
	}
	if m.closes[0] >= qty || m.closes[0] <= 0 {
		t.Fatalf("vol reduce size %v not a partial reduce of %v", m.closes[0], qty)
	}
	if at.volResizeAllowed("XUSDT_long") {
		t.Fatal("the resize cooldown was not consumed")
	}
}

// The quantity-free trailing check still runs after the reduce and a safe
// tighten is kept.
func TestExec1028_VolReduceStillRunsTrailing(t *testing.T) {
	at, m, _ := exec1028VolRound(t, store.RiskControlConfig{VolTargetEnabled: true, TrailingStopEnabled: true, ProfitLockAtR: -1, MinPositionSize: 1}, nil)
	if len(m.closes) != 1 {
		t.Fatalf("closes=%v", m.closes)
	}
	if m.slCalls != 1 || at.GetRecordedStopLoss("XUSDT", "long") <= 90 {
		t.Fatalf("trailing must still tighten after a vol reduce: slCalls=%d SL=%v", m.slCalls, at.GetRecordedStopLoss("XUSDT", "long"))
	}
}

// #7 for the vol reduce: an UNKNOWN outcome burns the cooldown and the round
// takes no further quantity action.
func TestExec1028_VolReduceUnknownDoesNotRepeat(t *testing.T) {
	at, m, _ := exec1028VolRound(t, store.RiskControlConfig{VolTargetEnabled: true, ProfitLockAtR: 1, MinPositionSize: 1}, exec1028Unknown())
	if len(m.closes) != 1 {
		t.Fatalf("closes=%v", m.closes)
	}
	if at.volResizeAllowed("XUSDT_long") {
		t.Fatal("UNKNOWN reduce must consume the resize cooldown (no blind retry next cycle)")
	}
}

// The realtime snapshot is replaced so the fast loop cannot trim off the
// pre-reduce size either.
func TestExec1028_VolReducePublishesReducedRealtimeSnapshot(t *testing.T) {
	data := freshnessData("XUSDT", 120, allFreshLags)
	target := volTargetNotional(900, 1.5, oneHourATRPct(data))
	qty := 3 * target / 120
	m := &exec1028VolMock{batch1RiskMock: batch1RiskMock{fix06Mock: fix06Mock{
		audit05Mock: audit05Mock{mark: 120}, symbol: "XUSDT", side: "long", qty: qty,
	}}, resp: map[string]interface{}{"executedQty": qty - target/120}}
	at := riskTestTrader(store.RiskControlConfig{VolTargetEnabled: true, MinPositionSize: 1})
	at.trader = m
	at.positionInitialStopLoss = map[string]float64{}
	at.positionExitMode = map[string]string{"XUSDT_long": kernel.ExitModeTrend}
	at.SetRecordedStopLoss("XUSDT", "long", 90)
	at.realtime = newRealtimeProtection()
	at.marketDataFn = func(string) (*market.Data, error) { return freshnessData("XUSDT", 120, allFreshLags), nil }
	at.processVolTargetAndTrailing()
	got, ok := at.realtime.positions["XUSDT_long"]
	if !ok {
		t.Fatal("no realtime snapshot")
	}
	amt, _ := got.pos["positionAmt"].(float64)
	if math.Abs(amt-target/120) > 1e-6*qty {
		t.Fatalf("realtime snapshot qty=%v, want the post-reduce %v", amt, target/120)
	}
}

// ---------------------------------------------------------------------------
// #8 fill risk uses the tick-rounded stop
// ---------------------------------------------------------------------------

func TestExec1028_FillRiskUsesTickRoundedStop(t *testing.T) {
	rc := store.RiskControlConfig{RiskPerTradePct: 1.5, EntryRoundTripCostBps: 12}
	d := &kernel.Decision{Symbol: "XUSDT", Action: "open_long", StopLoss: 99.74, TakeProfit: 105}
	// Review numbers: equity 1000 -> budget 15; 39.473 lots; the exchange stop
	// rests at 99.7 (0.1 tick), not 99.74.
	m := &exec1028Mock{tick: 0.1}
	at := riskTestTrader(rc)
	at.trader = m
	err := at.actualFillRisk(d, 100, 39.473, 1000)
	if err == nil || !strings.Contains(err.Error(), "exceeds budget") {
		t.Fatalf("risk at the REAL stop (99.7) is 16.58 > 15.75: must be rejected, got %v", err)
	}
	// Without the rounding hook the plan value is used (adapter-agnostic).
	at.trader = &exec1028Mock{}
	if err := at.actualFillRisk(d, 100, 39.473, 1000); err != nil {
		t.Fatalf("no quantizer -> raw plan stop (risk 15.0): %v", err)
	}
	// A stop whose rounded value crosses the fill is rejected as crossed.
	d2 := &kernel.Decision{Symbol: "XUSDT", Action: "open_long", StopLoss: 99.96, TakeProfit: 105}
	at.trader = &exec1028Mock{tick: 0.1}
	if err := at.actualFillRisk(d2, 100, 1, 1000); err == nil || !strings.Contains(err.Error(), "crossed") {
		t.Fatalf("stop rounding to the fill must read as crossed: %v", err)
	}
}
