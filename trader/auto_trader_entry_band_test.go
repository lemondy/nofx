package trader

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/kernel"
	"nofx/market"
	"nofx/store"
	"nofx/trader/binance"
	"nofx/trader/paper"
	"nofx/trader/types"
)

func TestEntryBandThetaParity(t *testing.T) {
	end := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                     string
		drift, bounce, sg, theta float64
	}{
		{"bounce", 0, .0002, .00039999990874853877, .0009656853604247148},
		{"trend", .0001, 0, 0, .0008},
		{"mixed", .00005, .0003, .0005999998731228395, .0011211101495246225},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bars []market.Kline
			for i := 0; i < 1440; i++ {
				alt := 1.0
				if i%2 == 1 {
					alt = -1
				}
				bars = append(bars, market.Kline{OpenTime: end.Add(time.Duration(i-1440) * time.Minute).UnixMilli(), Close: 100 * math.Exp(tc.drift*float64(i)) * (1 + tc.bounce*alt)})
			}
			theta, sg, reason := entryBandTheta(bars, end, .0008)
			if math.Abs(theta-tc.theta) > tc.theta*1e-12 || reason != "" {
				t.Fatalf("theta=%.18g want=%.18g reason=%s", theta, tc.theta, reason)
			}
			if tc.sg > 0 && math.Abs(sg-tc.sg) > tc.sg*1e-12 {
				t.Fatalf("sg=%.18g want=%.18g", sg, tc.sg)
			}
			if tc.sg == 0 && sg > 1e-14 {
				t.Fatalf("trend sg=%g", sg)
			}
			// Current and old bars must have no influence.
			bars = append(bars, market.Kline{OpenTime: end.UnixMilli(), Close: 999}, market.Kline{OpenTime: end.Add(-1441 * time.Minute).UnixMilli(), Close: 1})
			got, _, _ := entryBandTheta(bars, end, .0008)
			if got != theta {
				t.Fatalf("out-of-window bars changed theta: %g vs %g", got, theta)
			}
		})
	}
}

func TestEntryBandThetaGapsAndDemeaning(t *testing.T) {
	end := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	bars := []market.Kline{}
	// Returns: log(1.01), log(.99), and log(1.03) in another segment.
	// The isolated return participates in the mean but never forms a lag pair.
	for i, c := range map[int]float64{-10: 100, -9: 101, -8: 99.99, -4: 100, -3: 103} {
		bars = append(bars, market.Kline{OpenTime: end.Add(time.Duration(i) * time.Minute).UnixMilli(), Close: c})
	}
	m := (math.Log(1.01) + math.Log(.99) + math.Log(1.03)) / 3
	sg := math.Sqrt(math.Max(0, -(math.Log(1.01)-m)*(math.Log(.99)-m)))
	theta, wantSG, reason := entryBandTheta(bars, end, .0008)
	want := .5 * (.0008 + math.Sqrt(.0008*.0008+4*sg*sg))
	if math.Abs(theta-want) > 1e-15 || math.Abs(wantSG-sg) > 1e-15 || reason != "" {
		t.Fatalf("gaps: %g %g %s want %g %g", theta, wantSG, reason, want, sg)
	}
	for _, bars := range [][]market.Kline{nil, {{OpenTime: end.Add(-3 * time.Minute).UnixMilli(), Close: 100}, {OpenTime: end.Add(-time.Minute).UnixMilli(), Close: 101}}} {
		theta, sg, reason := entryBandTheta(bars, end, .0008)
		if theta != .0008 || sg != 0 || reason != "theta_fallback_phi" {
			t.Fatalf("fallback: %g %g %s", theta, sg, reason)
		}
	}
}

// Reuse the existing protection fake: only entry/cancel/receipt behavior is new.
type bandMock struct {
	tickErr error
	exec1028Mock
	limits                []*types.LimitOrderRequest
	status                map[string]interface{}
	marketCalls           int
	marketQty             float64
	cancelConfirmed       bool
	raceQty               float64
	marketErr, receiptErr error
}

func (m *bandMock) GetBalance() (map[string]interface{}, error) {
	return map[string]interface{}{"totalEquity": 1000.0, "availableBalance": 1000.0}, nil
}
func (m *bandMock) GetMarketPrice(string) (float64, error) { return m.mark, nil }
func (m *bandMock) GetSymbolTickSize(string) (float64, error) {
	if m.tickErr != nil {
		return 0, m.tickErr
	}
	return .025, nil
}
func (m *bandMock) SetMarginMode(string, bool) error { return nil }
func (m *bandMock) FormatQuantity(_ string, q float64) (string, error) {
	return fmt.Sprintf("%.6f", math.Floor(q*1e6)/1e6), nil
}
func (m *bandMock) GetOrderBook(string, int) ([][]float64, [][]float64, error) {
	return [][]float64{{m.mark - .01, 10}}, [][]float64{{m.mark + .01, 10}}, nil
}
func (m *bandMock) PlaceLimitOrder(r *types.LimitOrderRequest) (*types.LimitOrderResult, error) {
	copy := *r
	m.limits = append(m.limits, &copy)
	m.status = map[string]interface{}{"orderId": "123", "status": "NEW", "executedQty": 0.0, "avgPrice": 0.0}
	return &types.LimitOrderResult{OrderID: "123", Quantity: r.Quantity, Price: r.Price}, nil
}
func (m *bandMock) GetOrderStatus(_ string, id string) (map[string]interface{}, error) {
	if m.receiptErr != nil {
		return nil, m.receiptErr
	}
	if id == "456" {
		return map[string]interface{}{"orderId": int64(456), "status": "FILLED", "executedQty": m.marketQty, "avgPrice": m.mark}, nil
	}
	return m.status, nil
}
func (m *bandMock) CancelOrder(_ string, id string) error {
	m.cancelByID = append(m.cancelByID, id)
	if m.cancelConfirmed {
		m.status["status"] = "CANCELED"
	}
	if m.raceQty > 0 {
		m.qty = m.raceQty
		if m.side == "short" {
			m.qty = -m.qty
		}
		m.status["executedQty"] = m.raceQty
		m.status["avgPrice"] = 100.0
	}
	return nil
}
func (m *bandMock) OpenLong(_ string, q float64, _ int) (map[string]interface{}, error) {
	return m.open(q)
}
func (m *bandMock) OpenShort(_ string, q float64, _ int) (map[string]interface{}, error) {
	return m.open(q)
}
func (m *bandMock) open(q float64) (map[string]interface{}, error) {
	m.marketCalls++
	m.marketQty = q
	if m.marketErr != nil {
		return nil, m.marketErr
	}
	m.qty = q
	if m.side == "short" {
		m.qty = -q
	}
	return map[string]interface{}{"orderId": int64(456), "avgPrice": m.mark, "executedQty": q, "status": "FILLED"}, nil
}

func bandFixture(t *testing.T, side string) (*AutoTrader, *bandMock, *kernel.Decision) {
	t.Helper()
	m := &bandMock{exec1028Mock: exec1028Mock{symbol: "XUSDT", side: side, mark: 100, entry: 100, step: .000001, tick: .025}, cancelConfirmed: true}
	at := exec1028Trader(store.RiskControlConfig{EntryBandEnabled: true, EntryBandMode: "fixed", EntryBandFixedBps: 8}, &m.exec1028Mock)
	at.trader = m
	at.config.StrategyConfig.CoinSource.SourceType = "static"
	at.strategyEngine = kernel.NewStrategyEngine(at.config.StrategyConfig)
	at.oiValueFn = func(string) (float64, bool) { return 100, true }
	at.marketDataFn = func(string) (*market.Data, error) { return &market.Data{Symbol: "XUSDT", CurrentPrice: m.mark}, nil }
	at.recheckDataFn = func(string) (*market.Data, error) { return nil, errors.New("direction data unavailable") }
	at.store = lifecycleFixture(t).store
	at.id = "t"
	at.name = "band-test"
	at.exchange = "paper"
	d := &kernel.Decision{Action: "open_" + side, Symbol: "XUSDT", PositionSizeUSD: 100, Leverage: 2, StopLoss: 95, TakeProfit: 115, ExitMode: "range"}
	if side == "short" {
		d.StopLoss = 105
		d.TakeProfit = 85
	}
	return at, m, d
}
func bandEvents(t *testing.T, at *AutoTrader) []store.EntryBandEvent {
	t.Helper()
	var out []store.EntryBandEvent
	if err := at.store.GormDB().Order("id").Find(&out).Error; err != nil {
		t.Fatal(err)
	}
	return out
}
func bandLastOutcome(t *testing.T, at *AutoTrader, want string) {
	t.Helper()
	events := bandEvents(t, at)
	if len(events) == 0 || events[len(events)-1].Outcome != want {
		t.Fatalf("want %s events=%+v", want, events)
	}
}
func bandPlace(t *testing.T, at *AutoTrader, d *kernel.Decision) *pendingEntry {
	t.Helper()
	if err := at.executeDecisionWithRecord(d, &store.DecisionAction{}); err != nil {
		t.Fatal(err)
	}
	side := strings.TrimPrefix(d.Action, "open_")
	pe := at.getPendingEntry(d.Symbol, side)
	if pe == nil {
		t.Fatal("band pending missing")
	}
	return pe
}

func TestEntryBandPlacementAndMarketQuantity(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		t.Run(side, func(t *testing.T) {
			at, m, d := bandFixture(t, side)
			rec := &store.DecisionAction{}
			if err := at.executeDecisionWithRecord(d, rec); err != nil {
				t.Fatal(err)
			}
			want := 99.9
			if side == "short" {
				want = 100.1
			}
			if m.marketCalls != 0 || len(m.limits) != 1 || math.Abs(m.limits[0].Price-want) > 1e-12 || rec.EntryMode != "band" || rec.OrderID != 123 || rec.EntryOrderID != "123" {
				t.Fatalf("placement: %+v %+v", rec, m.limits)
			}
			pe := at.getPendingEntry("XUSDT", side)
			if d.CycleReservedRiskUSD != 0 {
				t.Fatal("band risk double-counted as a cycle reservation")
			}
			if pe == nil || pe.Mode != "band" || pe.RefPrice != 100 || pe.ThetaBps != 8 || pe.Quantity != rec.Quantity || pe.StopLoss != d.StopLoss || pe.TakeProfit != d.TakeProfit || pe.Leverage != d.Leverage || pe.ExitMode != "range" || pe.Deadline.Sub(pe.PlacedAt) != 30*time.Minute {
				t.Fatalf("pending: %+v", pe)
			}
			rows, err := at.store.PendingEntry().List(at.id)
			if err != nil || len(rows) != 1 || rows[0].Mode != "band" || rows[0].RefPrice != 100 || rows[0].ThetaBps != 8 || !rows[0].Deadline.Equal(pe.Deadline) {
				t.Fatalf("durable: %+v %v", rows, err)
			}
			restored := pendingEntryFromRow(rows[0])
			if restored.Mode != "band" || !restored.Deadline.Equal(pe.Deadline) {
				t.Fatal("restart lost band")
			}
			marketAt, marketM, marketD := bandFixture(t, side)
			marketAt.config.StrategyConfig.RiskControl.EntryBandEnabled = false
			if err := marketAt.executeDecisionWithRecord(marketD, &store.DecisionAction{}); err != nil {
				t.Fatal(err)
			}
			if marketM.marketQty != pe.Quantity || marketM.marketCalls != 1 || len(marketM.limits) != 0 || len(bandEvents(t, marketAt)) != 0 {
				t.Fatalf("market quantity=%g vs band %g", marketM.marketQty, pe.Quantity)
			}
		})
	}
}

func TestEntryBandStopGuard(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		at, m, d := bandFixture(t, side)
		at.config.StrategyConfig.RiskControl.EntryBandFixedBps = 600
		if err := at.executeDecisionWithRecord(d, &store.DecisionAction{}); err != nil {
			t.Fatal(err)
		}
		if m.marketCalls != 1 || len(m.limits) != 0 || at.getPendingEntry("XUSDT", side) != nil {
			t.Fatal("stop guard didn't market open")
		}
		bandLastOutcome(t, at, "stop_guard")
	}
}

func TestEntryBandRollFetchFailure(t *testing.T) {
	at, m, d := bandFixture(t, "long")
	rc := &at.config.StrategyConfig.RiskControl
	rc.EntryBandMode = ""
	rc.EntryBandTimeoutMin = 7
	calls := 0
	at.entryBandKlinesFn = func(_ string, start, end time.Time) ([]market.Kline, error) {
		calls++
		if end.Sub(start) != 1440*time.Minute || !end.Equal(end.Truncate(time.Minute)) {
			t.Fatal("bad fetch window")
		}
		return nil, errors.New("offline")
	}
	pe := bandPlace(t, at, d)
	if calls != 1 || len(m.limits) != 1 || pe.ThetaBps != 8 || pe.ThetaReason != "theta_fallback_phi" || pe.Deadline.Sub(pe.PlacedAt) != 7*time.Minute {
		t.Fatalf("roll fallback: %+v", pe)
	}
}

func TestEntryBandFilledProtection(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		at, m, d := bandFixture(t, side)
		pe := bandPlace(t, at, d)
		m.qty = pe.Quantity
		if side == "short" {
			m.qty = -m.qty
		}
		m.status = map[string]interface{}{"status": "FILLED", "executedQty": pe.Quantity, "avgPrice": pe.Price}
		at.processPendingEntries(true)
		if len(m.slPrices) == 0 || len(m.tpPrices) == 0 || !at.isAIManaged(pe.Symbol, side) || at.ExitModeFor(pe.Symbol, side) != "range" || at.getPendingEntry(pe.Symbol, side) != nil {
			t.Fatalf("protection incomplete: %+v", pe)
		}
		bandLastOutcome(t, at, "filled")
		e := bandEvents(t, at)[0]
		if e.FillPrice != pe.Price || e.FillTime == nil {
			t.Fatalf("fill attribution: %+v", e)
		}
	}
}

func TestEntryBandDeadlineFallback(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		at, m, d := bandFixture(t, side)
		pe := bandPlace(t, at, d)
		pe.Deadline = time.Now().Add(-time.Second)
		m.mark = 100.2
		at.processPendingEntries(true)
		if len(m.cancelByID) != 1 || m.marketCalls != 1 || len(m.limits) != 1 || at.getPendingEntry(pe.Symbol, side) != nil {
			t.Fatalf("fallback cancels=%v market=%d limits=%d", m.cancelByID, m.marketCalls, len(m.limits))
		}
		bandLastOutcome(t, at, "fallback")
		e := bandEvents(t, at)[0]
		if e.FallbackOrderID != "456" {
			t.Fatalf("fallback ID: %+v", e)
		}
	}
}

func TestEntryBandDeadlineWaitsForCancelConfirmation(t *testing.T) {
	at, m, d := bandFixture(t, "long")
	pe := bandPlace(t, at, d)
	pe.Deadline = time.Now().Add(-time.Second)
	m.cancelConfirmed = false
	at.processPendingEntries(false)
	if m.marketCalls != 0 || at.getPendingEntry(pe.Symbol, pe.Side) == nil || len(bandEvents(t, at)) != 0 {
		t.Fatal("unconfirmed cancel allowed fallback")
	}
	m.cancelConfirmed = true
	at.processPendingEntries(false)
	if m.marketCalls != 1 {
		t.Fatal("confirmed cancel didn't fallback")
	}
}

func TestEntryBandFallbackFailureInvalidated(t *testing.T) {
	for _, mode := range []string{"submit", "risk", "quote"} {
		t.Run(mode, func(t *testing.T) {
			at, m, d := bandFixture(t, "long")
			pe := bandPlace(t, at, d)
			pe.Deadline = time.Now().Add(-time.Second)
			switch mode {
			case "submit":
				m.marketErr = errors.New("market rejected")
			case "risk":
				m.mark = 112 // live RR below configured minimum
				at.config.StrategyConfig.RiskControl.MinRiskRewardRatio = 2
			case "quote":
				at.marketDataFn = func(string) (*market.Data, error) { return nil, errors.New("quote unavailable") }
			}
			at.processPendingEntries(false)
			bandLastOutcome(t, at, "invalidated")
			if at.cycleRiskReservedUSD != 0 {
				t.Fatal("failed fallback retained cycle reservation")
			}
			e := bandEvents(t, at)[0]
			if e.Error == "" || at.getPendingEntry(pe.Symbol, pe.Side) != nil || len(m.limits) != 1 {
				t.Fatalf("bad invalidation %+v", e)
			}
		})
	}
}

func TestEntryBandPartialDeadlineAndCancelRace(t *testing.T) {
	for _, race := range []bool{false, true} {
		at, m, d := bandFixture(t, "long")
		pe := bandPlace(t, at, d)
		pe.Deadline = time.Now().Add(-time.Second)
		if race {
			m.raceQty = .4
		} else {
			m.qty = .4
			m.status = map[string]interface{}{"status": "PARTIALLY_FILLED", "executedQty": .4, "avgPrice": 99.9}
		}
		at.processPendingEntries(false)
		if m.marketCalls != 0 || at.getPendingEntry(pe.Symbol, pe.Side) != nil || pe.ProtectedQty != .4 || !at.isAIManaged(pe.Symbol, pe.Side) {
			t.Fatalf("partial mishandled race=%v %+v", race, pe)
		}
		bandLastOutcome(t, at, "partial")
	}
}

func TestEntryBandDirectionSkippedRiskCancellationPreserved(t *testing.T) {
	for _, mode := range []string{"direction", "sl", "excluded", "halt"} {
		t.Run(mode, func(t *testing.T) {
			at, m, d := bandFixture(t, "long")
			pe := bandPlace(t, at, d)
			at.cycleGateStates = map[string]*kernel.GateState{"XUSDT": {LongAllowed: false, LongFailed: []string{"DIRECTION_BLOCKED"}}}
			switch mode {
			case "sl":
				m.mark = 94
			case "excluded":
				at.config.StrategyConfig.CoinSource.ExcludedCoins = []string{"XUSDT"}
			case "halt":
				at.safeMode = true
			}
			at.processPendingEntries(true)
			if mode == "direction" {
				if len(m.cancelByID) != 0 || at.getPendingEntry(pe.Symbol, pe.Side) == nil {
					t.Fatal("direction recheck cancelled band")
				}
			} else {
				if len(m.cancelByID) == 0 || at.getPendingEntry(pe.Symbol, pe.Side) != nil || m.marketCalls != 0 {
					t.Fatal("risk cancellation lost")
				}
				bandLastOutcome(t, at, "invalidated")
			}
		})
	}
	// An ordinary limit still follows direction rechecks.
	at, m, d := bandFixture(t, "long")
	pe := bandPlace(t, at, d)
	pe.Mode = ""
	at.config.StrategyConfig.RiskControl.EntryBandEnabled = false
	at.cycleGateStates = map[string]*kernel.GateState{"XUSDT": {LongAllowed: false, LongFailed: []string{"DIRECTION_BLOCKED"}}}
	at.processPendingEntries(true)
	if len(m.cancelByID) == 0 || at.getPendingEntry(pe.Symbol, pe.Side) != nil {
		t.Fatal("legacy direction behavior changed")
	}
}

func TestEntryBandDispatch(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		for _, action := range []string{"close_", "partial_close_"} {
			at, m, d := bandFixture(t, side)
			pe := bandPlace(t, at, d)
			if err := at.executeDecisionWithRecord(d, &store.DecisionAction{}); err != nil {
				t.Fatal(err)
			}
			if len(m.cancelByID) != 0 || len(m.limits) != 1 || m.marketCalls != 0 || at.getPendingEntry(pe.Symbol, side) != pe {
				t.Fatal("duplicate replaced order")
			}
			bandLastOutcome(t, at, "duplicate_ignored")
			m.cancelConfirmed = false
			close := &kernel.Decision{Symbol: pe.Symbol, Action: action + side}
			if err := at.executeDecisionWithRecord(close, &store.DecisionAction{}); err == nil {
				t.Fatal("unconfirmed AI cancel reported success")
			}
			if at.getPendingEntry(pe.Symbol, side) == nil {
				t.Fatal("AI cancel lost ownership")
			}
			m.cancelConfirmed = true
			if err := at.executeDecisionWithRecord(close, &store.DecisionAction{}); err != nil {
				t.Fatal(err)
			}
			bandLastOutcome(t, at, "ai_cancelled")
			if at.getPendingEntry(pe.Symbol, side) != nil || m.marketCalls != 0 {
				t.Fatal("AI cancel failed")
			}
		}
	}
}

func TestEntryBandEventWriteFailureDoesNotFailExecution(t *testing.T) {
	at, m, d := bandFixture(t, "long")
	pe := bandPlace(t, at, d)
	if err := at.store.GormDB().Exec("DROP TABLE entry_band_events").Error; err != nil {
		t.Fatal(err)
	}
	if err := at.executeDecisionWithRecord(d, &store.DecisionAction{}); err != nil {
		t.Fatal(err)
	}
	pe.Deadline = time.Now().Add(-time.Second)
	at.processPendingEntries(false)
	if m.marketCalls != 1 {
		t.Fatal("event write blocked fallback")
	}
}

func TestEntryBandUnknownSubmitRetainsMode(t *testing.T) {
	at, _, d := bandFixture(t, "long")
	plan := pendingEntry{Mode: "band", Symbol: d.Symbol, Side: "long", Deadline: time.Now().Add(time.Minute), ThetaBps: 8}
	err := at.trackUnknownSubmit(&binance.SubmitUnknownError{ClientOrderID: "band-unknown"}, "long", plan)
	if err == nil {
		t.Fatal("expected unknown error")
	}
	pe := at.getPendingEntry(d.Symbol, "long")
	if pe == nil || pe.Mode != "band" || pe.ThetaBps != 8 || pe.Deadline.IsZero() {
		t.Fatalf("unknown lost band: %+v", pe)
	}
}

func TestEntryBandDeadlineUsesPersistedTimeOnly(t *testing.T) {
	at, m, d := bandFixture(t, "long")
	pe := bandPlace(t, at, d)
	pe.PlacedAt = time.Now().Add(-48 * time.Hour)
	pe.Deadline = time.Now().Add(time.Hour)
	at.config.StrategyConfig.RiskControl.LimitEntryMaxCycles = 1
	at.processPendingEntries(true)
	if len(m.cancelByID) != 0 || m.marketCalls != 0 {
		t.Fatal("legacy lifetime expired a band entry")
	}
}

func TestEntryBandPartialDirectionSkipped(t *testing.T) {
	at, m, d := bandFixture(t, "long")
	pe := bandPlace(t, at, d)
	m.qty = .4
	m.status = map[string]interface{}{"status": "PARTIALLY_FILLED", "executedQty": .4, "avgPrice": 99.9}
	at.cycleGateStates = map[string]*kernel.GateState{"XUSDT": {LongAllowed: false, LongFailed: []string{"DIRECTION_BLOCKED"}}}
	at.processPendingEntries(true)
	if len(m.cancelByID) != 0 || pe.ProtectedQty != .4 || at.getPendingEntry(pe.Symbol, pe.Side) == nil {
		t.Fatal("partial band cancelled or left unprotected")
	}
}

func TestEntryBandFillAtDeadlineCancelRace(t *testing.T) {
	at, m, d := bandFixture(t, "long")
	pe := bandPlace(t, at, d)
	pe.Deadline = time.Now().Add(-time.Second)
	m.raceQty = pe.Quantity
	at.processPendingEntries(false)
	if m.marketCalls != 0 || pe.ProtectedQty != pe.Quantity || at.getPendingEntry(pe.Symbol, pe.Side) != nil {
		t.Fatal("full cancel-race fill replaced or unprotected")
	}
	bandLastOutcome(t, at, "filled")
}

func TestEntryBandEventUsesActualPartialReceipt(t *testing.T) {
	at, m, d := bandFixture(t, "long")
	pe := bandPlace(t, at, d)
	pe.Deadline = time.Now().Add(-time.Second)
	filledAt := time.Now().Add(-time.Second).Truncate(time.Millisecond)
	m.qty = .4
	m.status = map[string]interface{}{"status": "PARTIALLY_FILLED", "executedQty": .4, "avgPrice": 99.85, "updateTime": filledAt.UnixMilli()}
	at.processPendingEntries(false)
	e := bandEvents(t, at)[0]
	if e.FillPrice != 99.85 || e.FillTime == nil || !e.FillTime.Equal(filledAt) {
		t.Fatalf("actual receipt lost: %+v", e)
	}
}

func TestEntryBandRealtimeAndOfflineAttribution(t *testing.T) {
	for _, path := range []string{"realtime", "offline"} {
		t.Run(path, func(t *testing.T) {
			at, m, d := bandFixture(t, "long")
			pe := bandPlace(t, at, d)
			m.qty = pe.Quantity
			if path == "realtime" {
				if !at.finalizeRealtimeFill(binance.OrderUpdate{Symbol: pe.Symbol, OrderID: pe.OrderID, Status: "FILLED", FilledQuantity: pe.Quantity, AveragePrice: pe.Price}) {
					t.Fatal("realtime fill not finalized")
				}
			} else {
				m.status = map[string]interface{}{"status": "FILLED", "executedQty": pe.Quantity, "avgPrice": pe.Price}
				at.pendingEntries = nil
				at.ReconcilePendingEntries()
			}
			if len(m.slPrices) == 0 || !at.isAIManaged(pe.Symbol, pe.Side) {
				t.Fatal("filled entry unprotected")
			}
			bandLastOutcome(t, at, "filled")
		})
	}
}

type bandPaperMarket struct {
	now  time.Time
	bars []paper.Candle
}

func (m *bandPaperMarket) Price(string) (float64, error)                       { return 100, nil }
func (m *bandPaperMarket) Candles1m(string, time.Time) ([]paper.Candle, error) { return m.bars, nil }
func (m *bandPaperMarket) QuantityRules(string) (paper.QuantityRule, error) {
	return paper.QuantityRule{TickSize: "0.025", StepSize: "0.000001", MinQty: .000001, MinNotional: 1}, nil
}
func (m *bandPaperMarket) OrderBook(string, int) ([][]float64, [][]float64, error) {
	return [][]float64{{99.99, 10}}, [][]float64{{100.01, 10}}, nil
}

func TestEntryBandPaperTradeThroughLifecycle(t *testing.T) {
	at, _, d := bandFixture(t, "long")
	feed := &bandPaperMarket{now: time.Now()}
	p := paper.New(paper.Config{ExchangeID: "band-paper", InitialBalance: 1000, DefaultLeverage: 2, LimitTradeThroughBps: 10, Now: func() time.Time { return feed.now }}, at.store, feed)
	at.trader = p
	pe := bandPlace(t, at, d)
	// First closed candle only touches L. Band remains NEW with trade-through on.
	barStart := feed.now.Truncate(time.Minute).Add(time.Minute)
	feed.bars = []paper.Candle{{Time: barStart, Open: 100, Low: pe.Price, High: 100, Close: 100}}
	feed.now = barStart.Add(time.Minute)
	if err := p.Advance(feed.now); err != nil {
		t.Fatal(err)
	}
	at.processPendingEntries(true)
	if at.getPendingEntry(pe.Symbol, pe.Side) == nil {
		t.Fatal("touch filled trade-through entry")
	}
	// A later candle trades through L by more than 10 bps.
	feed.bars = append(feed.bars, paper.Candle{Time: feed.now, Open: 100, Low: pe.Price * .9989, High: 100, Close: 100})
	feed.now = feed.now.Add(time.Minute)
	if err := p.Advance(feed.now); err != nil {
		t.Fatal(err)
	}
	at.processPendingEntries(true)
	if at.getPendingEntry(pe.Symbol, pe.Side) != nil || !at.isAIManaged(pe.Symbol, pe.Side) {
		t.Fatal("paper band fill not finalized")
	}
	orders, err := p.GetOpenOrders(pe.Symbol)
	if err != nil {
		t.Fatal(err)
	}
	sl, tp := false, false
	for _, o := range orders {
		sl = sl || o.Type == "STOP_MARKET"
		tp = tp || o.Type == "TAKE_PROFIT_MARKET"
	}
	if !sl || !tp {
		t.Fatalf("paper fill missing protection: %+v", orders)
	}
	bandLastOutcome(t, at, "filled")
}

func TestEntryBandTickUnavailableKeepsMarketEntry(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		at, m, d := bandFixture(t, side)
		m.tickErr = errors.New("tick unavailable")
		if err := at.executeDecisionWithRecord(d, &store.DecisionAction{}); err != nil {
			t.Fatal(err)
		}
		if m.marketCalls != 1 || len(m.limits) != 0 || at.getPendingEntry("XUSDT", side) != nil {
			t.Fatalf("%s: tick failure must fall back to the market order", side)
		}
		bandLastOutcome(t, at, "tick_unavailable")
	}
}
