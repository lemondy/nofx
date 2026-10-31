package trader

import (
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"nofx/kernel"
	"nofx/store"
	"nofx/trader/types"
)

func TestReview20261028RemFastUnknownConsumesCooldown(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		t.Run(side, func(t *testing.T) {
			at, m, _ := newRealtimeTestTrader(store.RiskControlConfig{FastFavorableATRMult: -1, MinPositionSize: 1})
			qty, peak, retrace := 1.0, 110.0, 106.0
			if side == "short" {
				qty, peak, retrace = -1, 90, 94
				m.side, m.qty = side, qty
			}
			pos := realtimePos(side, qty)
			key := "XUSDT_" + side
			at.SetRecordedStopLoss("XUSDT", side, 100)
			at.recordTPLedger(key, 1, .5)
			m.closeErr = exec1028Unknown()
			now := time.Now()
			at.processFastMove(pos, peak, 1, now)
			trimAt := now.Add(time.Second)
			if !at.processFastMove(pos, retrace, 1, trimAt) {
				t.Fatal("UNKNOWN must consume snapshot")
			}
			st := at.fastMoves[key]
			if !st.lastTrim.Equal(trimAt) || len(st.marks) != 0 || pos["positionAmt"] != qty {
				t.Fatalf("unknown state: %+v pos=%v", st, pos)
			}
			for _, q := range []float64{qty, qty * .5} {
				pos["positionAmt"] = q
				at.processFastMove(pos, peak, 1, now.Add(2*time.Second))
				at.processFastMove(pos, retrace, 1, now.Add(3*time.Second))
			}
			if m.closeCalls+m.closeShorts != 1 || m.positionsCalls+m.priceCalls != 0 {
				t.Fatalf("closes=%d/%d REST=%d/%d", m.closeCalls, m.closeShorts, m.positionsCalls, m.priceCalls)
			}
			e, _ := at.getTPLedger(key)
			if e.posQty != .5 {
				t.Fatalf("reduction booked more than once: %+v", e)
			}
		})
	}
}

type remCloseMock struct{ *exec1028Mock }

func (m *remCloseMock) GetMarketPrice(string) (float64, error) { return 100, nil }

func TestReview20261028RemAIUnknownAllowsNextClose(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		for _, initial := range []struct {
			action   string
			quantity float64
		}{
			{"partial_close_", 0},
			{"partial_close_", 5},
			{"close_", 0},
		} {
			for _, next := range []string{"partial_close_", "close_"} {
				t.Run(initial.action+side+"_"+strconv.FormatFloat(initial.quantity, 'f', 0, 64)+"_then_"+next+side, func(t *testing.T) {
					logs := captureExitReplayLogs(t)
					qty := 10.0
					if side == "short" {
						qty = -qty
					}
					m := &exec1028Mock{symbol: "XUSDT", side: side, qty: qty, mark: 100, closeErr: exec1028Unknown()}
					at := exec1028Trader(store.RiskControlConfig{MinPositionSize: 1}, m)
					at.trader = &remCloseMock{m}
					at.recordTPLedger("XUSDT_"+side, 10, 5)
					d := &kernel.Decision{Symbol: "XUSDT", Action: initial.action + side, CloseFraction: .5}
					record := &store.DecisionAction{Quantity: initial.quantity}
					if err := at.executeDecisionWithRecord(d, record); !isReduceUnknown(err) {
						t.Fatal(err)
					}
					if !strings.Contains(logs.String(), "AI 平/减仓 result UNKNOWN") {
						t.Fatalf("missing unknown close alert: %s", logs.String())
					}
					wantQty := 10 - initial.quantity
					if initial.action == "close_" {
						wantQty = 0
					}
					e, ok := at.getTPLedger("XUSDT_" + side)
					if !ok || e.posQty != wantQty || e.tpQty != 5 {
						t.Fatalf("unknown reduction ledger: %+v; want posQty=%g", e, wantQty)
					}
					// A fresh snapshot can still show a remainder. The next AI
					// decision must be able to reduce it without ending the lifecycle.
					m.qty, m.closeErr = qty*.5, nil
					d.Action = next + side
					record = &store.DecisionAction{}
					if err := at.executeDecisionWithRecord(d, record); err != nil {
						t.Fatal("next AI close blocked", err)
					}
					if len(m.closes) != 2 {
						t.Fatalf("next AI close did not reach exchange: %v", m.closes)
					}
					if next == "partial_close_" {
						e, _ = at.getTPLedger("XUSDT_" + side)
						if record.Quantity != 2.5 || e.posQty != math.Max(0, wantQty-record.Quantity) {
							t.Fatalf("successful reduction not booked: record=%+v ledger=%+v", record, e)
						}
					}
				})
			}
		}
	}
}

func TestReview20261028RemAIDefiniteFailureRetries(t *testing.T) {
	m := &exec1028Mock{symbol: "XUSDT", side: "long", qty: 10, mark: 100, closeErr: errors.New("rejected")}
	at := exec1028Trader(store.RiskControlConfig{MinPositionSize: 1}, m)
	d := &kernel.Decision{Symbol: "XUSDT", Action: "partial_close_long", CloseFraction: .5}
	at.executeDecisionWithRecord(d, &store.DecisionAction{})
	at.executeDecisionWithRecord(d, &store.DecisionAction{})
	if len(m.closes) != 2 {
		t.Fatal(m.closes)
	}
}

// Close/reopen SQLite as well as replacing AutoTrader, so no in-memory state
// or connection can carry the runner and NON-TP reductions across restart.
func TestReview20261028RemTPStateRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	open := func() *store.Store {
		st, err := store.NewWithConfig(store.DBConfig{Type: store.DBTypeSQLite, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st := open()
	for _, symbol := range []string{"XUSDT", "YUSDT", "ZUSDT"} {
		if err := st.Position().CreateOpenPosition(&store.TraderPosition{TraderID: "rem", Symbol: symbol, Side: "LONG", Quantity: 5, EntryPrice: 100, EntryQuantity: 10, Status: "OPEN"}); err != nil {
			t.Fatal(err)
		}
	}
	at := &AutoTrader{id: "rem", store: st}
	at.recordTPLedger("XUSDT_long", 10, 5)
	at.markTPRunner("XUSDT_long")
	at.dropTPLedger("XUSDT_long")
	at.recordTPLedger("YUSDT_long", 10, 5)
	at.noteTPLedgerReduction("YUSDT_long", 5)
	at.recordTPLedger("ZUSDT_long", 10, 5)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = open()
	defer st.Close()
	at2, m := exec1028Watchdog(t, kernel.ExitModeTrend, 5, []types.OpenOrder{exec1028SL(95)})
	at2.id, at2.store = "rem", st
	e, ok := at2.getTPLedger("YUSDT_long")
	if !ok || e.posQty != 5 || e.tpQty != 5 || at2.tpLegFilled("YUSDT_long", 5, 0, 1) {
		t.Fatalf("trim evidence lost: %+v", e)
	}
	if !at2.tpLegFilled("ZUSDT_long", 5, 0, 1) {
		t.Fatal("TP fill evidence lost")
	}
	at2.markAIManaged("XUSDT", "long")
	at2.processProtectionWatchdog()
	if !at2.tpRunnerDone("XUSDT_long") || len(m.tpQtys) != 0 {
		t.Fatalf("runner was rearmed after restart: %v", m.tpQtys)
	}
	if _, ok := at2.getTPLedger("XUSDT_long"); ok {
		t.Fatal("dropped ledger restored")
	}
	at2.ClearPeakPnLCache("XUSDT", "long")
	at2.ClearPeakPnLCache("YUSDT", "long")
	at5 := &AutoTrader{id: "rem", store: st}
	if at5.tpRunnerDone("XUSDT_long") {
		t.Fatal("runner survived lifecycle cleanup")
	}
	if _, ok := at5.getTPLedger("YUSDT_long"); ok {
		t.Fatal("ledger survived lifecycle cleanup")
	}
}

func TestReview20261028RemCoTMarketRewrite(t *testing.T) {
	at := forceMarketTestTrader(false, true)
	proposed := []kernel.Decision{{Symbol: "XUSDT", Action: "open_long_limit"}, {Symbol: "XUSDT", Action: "open_short_limit"}}
	original := map[string]string{}
	rewritten := at.forceMarketEntries(proposed, original)
	for _, failure := range []bool{false, true} {
		executed := []store.DecisionAction{}
		for _, d := range rewritten {
			a := store.DecisionAction{Symbol: d.Symbol, Action: d.Action, Success: !failure}
			if failure {
				a.Error = "exchange rejected"
			}
			executed = append(executed, a)
		}
		summaries := at.buildCoTSummaries(proposed, executed, original)
		for i, s := range summaries {
			if s.Action != proposed[i].Action || s.OK == failure || (failure && s.ErrText != "exchange rejected") {
				t.Fatalf("summary %+v", s)
			}
		}
	}
	at.setFilterReason(rewritten[0], "risk cap")
	summaries := at.buildCoTSummaries(proposed, nil, original)
	if !strings.Contains(summaries[0].ErrText, "risk cap") {
		t.Fatal(summaries)
	}
	// A later cycle without a rewrite must not match by symbol alone.
	if s := at.buildCoTSummaries(proposed, []store.DecisionAction{{Symbol: "XUSDT", Action: "open_long", Success: true}}); s[0].OK {
		t.Fatal("mapping leaked into next cycle")
	}
}

func TestReview20261028RemRebuildBreakevenAndFee(t *testing.T) {
	for _, side := range []string{"LONG", "SHORT", "BOTH"} {
		t.Run(side, func(t *testing.T) {
			openSide, closeSide := "BUY", "SELL"
			if side == "SHORT" {
				openSide, closeSide = closeSide, openSide
			}
			now := time.Now()
			trades := []TradeRecord{
				{TradeID: "o", Symbol: "X", PositionSide: side, Side: openSide, Price: 100, Quantity: 10, Fee: 10, Time: now},
				{TradeID: "c1", Symbol: "X", PositionSide: side, Side: closeSide, Price: 100, Quantity: 4, Fee: 1, Time: now.Add(time.Second)},
				{TradeID: "c2", Symbol: "X", PositionSide: side, Side: closeSide, Price: 100, Quantity: 6, Fee: 2, Time: now.Add(2 * time.Second)},
			}
			records := RebuildPositionsFromTrades(trades)
			if len(records) != 2 || records[0].EntryPrice != 100 || records[0].Fee != 5 || records[1].Fee != 8 {
				t.Fatalf("records %+v", records)
			}
			if math.Abs(records[0].Fee+records[1].Fee-13) > 1e-9 {
				t.Fatal("entry fee counted twice")
			}
		})
	}
	// Explicit intent must win even with inconsistent hedge metadata.
	trades := []TradeRecord{{Symbol: "X", OrderAction: "open_short", PositionSide: "LONG", Side: "BUY", Price: 100, Quantity: 1}, {Symbol: "X", OrderAction: "close_short", PositionSide: "LONG", Side: "BUY", Price: 100, Quantity: 1}}
	if records := RebuildPositionsFromTrades(trades); len(records) != 1 || records[0].Side != "short" {
		t.Fatal(records)
	}
}

// Unknown durable state cannot be interpreted as a fresh TP plan, and a
// failed read must never let write-through erase the existing evidence.
func TestReview20261028RemTPRestoreFailureKeepsEvidence(t *testing.T) {
	st := profitLockTestStore(t)
	if err := st.Position().CreateOpenPosition(&store.TraderPosition{TraderID: "rem", Symbol: "XUSDT", Side: "LONG", Quantity: 5, EntryPrice: 100, Status: "OPEN"}); err != nil {
		t.Fatal(err)
	}
	original := `{"runner_done":true,"pos_qty":5,"tp_qty":5}`
	if err := st.Position().SetTPProtectionState("rem", "XUSDT", "long", original); err != nil {
		t.Fatal(err)
	}
	const callback = "rem_tp_restore_failure"
	if err := st.GormDB().Callback().Query().Before("gorm:query").Register(callback, func(db *gorm.DB) {
		if db.Statement.Table == "trader_positions" {
			db.AddError(errors.New("injected TP read failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	at := &AutoTrader{id: "rem", store: st}
	if !at.tpRunnerDone("XUSDT_long") {
		t.Fatal("unreadable runner state allowed TP rearm")
	}
	at.recordTPLedger("XUSDT_long", 20, 10)
	at.noteTPLedgerReduction("XUSDT_long", 2)
	at.dropTPLedger("XUSDT_long")
	st.GormDB().Callback().Query().Remove(callback)
	p, err := st.Position().GetOpenPositionBySymbol("rem", "XUSDT", "LONG")
	if err != nil || p == nil || p.TPProtectionState != original {
		t.Fatalf("durable evidence overwritten: %+v %v", p, err)
	}
	if !at.tpRunnerDone("XUSDT_long") {
		t.Fatal("recovery lost runner")
	}
	e, ok := at.getTPLedger("XUSDT_long")
	if !ok || e.posQty != 5 || e.tpQty != 5 {
		t.Fatal(e)
	}
}

// A protection placement can precede the order-sync OPEN row. Keep the
// pending write and retry it when the next protection pass reads the state.
func TestReview20261028RemTPWriteBeforePositionSync(t *testing.T) {
	st := profitLockTestStore(t)
	at := &AutoTrader{id: "rem", store: st}
	at.recordTPLedger("XUSDT_long", 10, 5)
	at.noteTPLedgerReduction("XUSDT_long", 2)
	if err := st.Position().CreateOpenPosition(&store.TraderPosition{TraderID: "rem", Symbol: "XUSDT", Side: "LONG", Quantity: 8, EntryPrice: 100, Status: "OPEN"}); err != nil {
		t.Fatal(err)
	}
	at.getTPLedger("XUSDT_long") // retry the pending write
	restarted := &AutoTrader{id: "rem", store: st}
	e, ok := restarted.getTPLedger("XUSDT_long")
	if !ok || e.posQty != 8 || e.tpQty != 5 {
		t.Fatalf("TP write lost before OrderSync: %+v", e)
	}
}

func TestReview20261028RemPruneMissingPositionWritesOnce(t *testing.T) {
	st := profitLockTestStore(t)
	at := &AutoTrader{id: "rem", store: st}
	key := "XUSDT_long"
	at.recordTPLedger(key, 10, 5) // no OPEN row: the write stays dirty
	if !at.tpLedger.dirty[key] || !at.tpLedger.loaded[key] {
		t.Fatal("expected pending state before lifecycle cleanup")
	}
	updates := 0
	const callback = "rem_tp_clear_updates"
	if err := st.GormDB().Callback().Update().Before("gorm:update").Register(callback, func(db *gorm.DB) {
		if db.Statement.Table == "trader_positions" {
			updates++
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.GormDB().Callback().Update().Remove(callback) })
	logs := captureExitReplayLogs(t)
	at.pruneTPLedger(nil)
	if updates != 1 || at.tpLedger.dirty[key] || at.tpLedger.loaded[key] {
		t.Fatalf("cleanup retained pending state: updates=%d dirty=%v loaded=%v", updates, at.tpLedger.dirty, at.tpLedger.loaded)
	}
	at.pruneTPLedger(nil)
	if updates != 1 {
		t.Fatalf("second prune retried ended lifecycle: updates=%d", updates)
	}
	if strings.Contains(strings.ToLower(logs.String()), "warn") {
		t.Fatalf("missing closed position produced WARN: %s", logs.String())
	}
}

func TestReview20261028RemRebuildIncompleteOneWay(t *testing.T) {
	records := RebuildPositionsFromTrades([]TradeRecord{{Symbol: "X", PositionSide: "BOTH", Side: "SELL", Quantity: 1, Price: 110, RealizedPnL: 10}})
	if len(records) != 1 || records[0].Side != "long" || records[0].EntryPrice != 100 {
		t.Fatalf("incomplete history lost closing trade: %+v", records)
	}
}
