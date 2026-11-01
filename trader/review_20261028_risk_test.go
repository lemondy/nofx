package trader

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
	"nofx/kernel"
	"nofx/market"
	"nofx/store"
	"nofx/trader/types"
)

// review 2026-10-28 #2 #9: extend the existing entry mock with exchange sizing rules.
type review28RiskExchange struct {
	entryMock
	positions []map[string]interface{}
	quantity  float64
	roundUp   bool
}

// review 2026-10-28 #9: the existing entry fixture now reaches adapter quantity formatting.
func (m *entryMock) FormatQuantity(_ string, qty float64) (string, error) {
	return fmt.Sprint(qty), nil
}

func (m *review28RiskExchange) GetPositions() ([]map[string]interface{}, error) {
	return m.positions, nil
}
func (m *review28RiskExchange) FormatQuantity(_ string, qty float64) (string, error) {
	if m.roundUp {
		return fmt.Sprint(math.Ceil(qty)), nil
	}
	return fmt.Sprintf("%.3f", math.Floor(qty*1000)/1000), nil
}
func (m *review28RiskExchange) OpenLong(_ string, qty float64, _ int) (map[string]interface{}, error) {
	m.opens++
	m.quantity = qty
	return nil, errors.New("review submit stopped")
}
func (m *review28RiskExchange) OpenShort(s string, qty float64, l int) (map[string]interface{}, error) {
	return m.OpenLong(s, qty, l)
}
func (m *review28RiskExchange) GetOrderBook(string, int) ([][]float64, [][]float64, error) {
	return [][]float64{{99.99, 10}}, [][]float64{{100.01, 10}}, nil
}

func review28Position(symbol, side string, qty float64) map[string]interface{} {
	return map[string]interface{}{"symbol": symbol, "side": side, "positionAmt": qty,
		"entryPrice": 100.0, "markPrice": 100.0, "leverage": 10.0}
}

// review 2026-10-28 #2: count REST snapshots while reusing the existing entry fixture.
type review28GridBatchExchange struct {
	*review28RiskExchange
	balanceCalls, positionCalls, orderCalls int
	balanceErr, submitErr                   error
}

func (m *review28GridBatchExchange) GetBalance() (map[string]interface{}, error) {
	m.balanceCalls++
	if m.balanceErr != nil {
		return nil, m.balanceErr
	}
	return m.review28RiskExchange.GetBalance()
}
func (m *review28GridBatchExchange) GetPositions() ([]map[string]interface{}, error) {
	m.positionCalls++
	return m.review28RiskExchange.GetPositions()
}
func (m *review28GridBatchExchange) GetOpenOrders(symbol string) ([]types.OpenOrder, error) {
	m.orderCalls++
	return m.review28RiskExchange.GetOpenOrders(symbol)
}
func (m *review28GridBatchExchange) PlaceLimitOrder(req *types.LimitOrderRequest) (*types.LimitOrderResult, error) {
	m.limits++
	if m.submitErr != nil {
		return nil, m.submitErr
	}
	return &types.LimitOrderResult{OrderID: fmt.Sprintf("grid-%d", m.limits), Price: req.Price, Quantity: req.Quantity}, nil
}

func review28Trader(rc store.RiskControlConfig) (*AutoTrader, *review28RiskExchange) {
	at, _ := entryTestTrader(&market.Data{Symbol: "YUSDT", CurrentPrice: 101})
	m := &review28RiskExchange{}
	at.trader = m
	at.config.StrategyConfig.RiskControl = rc
	at.exchange = "binance"
	at.id = "review28"
	return at, m
}

func review28QueryFailure(t *testing.T, st *store.Store, table string) {
	t.Helper()
	name := "review28:" + table
	err := st.GormDB().Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == table {
			tx.AddError(errors.New("review table unavailable"))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.GormDB().Callback().Query().Remove(name) })
}

// review 2026-10-28 #9: the audit's 100 -> 101 reprice must fit the remaining 15U.
func TestReview28FinalMarketRiskReprice(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		t.Run(side, func(t *testing.T) {
			ResetDailyBaselineRegistryForTest()
			at, m := review28Trader(store.RiskControlConfig{RiskPerTradePct: 3, MaxAccountRiskPct: 20, MaxNetDirectionalRiskPct: -1})
			m.positions = []map[string]interface{}{review28Position("XUSDT", side, 37)}
			stop, price := 99.0, 101.0
			existingStop, closingSide := 95.0, "SELL"
			if side == "short" {
				stop, price, existingStop, closingSide = 101, 99, 105, "BUY"
			}
			m.orders = []types.OpenOrder{{Symbol: "XUSDT", Type: "STOP_MARKET", PositionSide: strings.ToUpper(side), Side: closingSide, StopPrice: existingStop, Quantity: 37}}
			at.marketDataFn = func(string) (*market.Data, error) { return &market.Data{CurrentPrice: price}, nil }
			d := &kernel.Decision{Symbol: "YUSDT", Action: "open_" + side, StopLoss: stop, PositionSizeUSD: 1000, Leverage: 10}
			if side == "long" {
				d.TakeProfit = 110
			} else {
				d.TakeProfit = 90
			}
			d.CycleReservedRiskUSD = 10
			at.cycleSymbolRiskReservedUSD = map[string]float64{}
			bookReservation(at, d, side)
			rec := &store.DecisionAction{}
			var err error
			if side == "long" {
				err = at.executeOpenLongWithRecord(d, rec)
			} else {
				err = at.executeOpenShortWithRecord(d, rec)
			}
			if err == nil || !strings.Contains(err.Error(), "review submit stopped") || m.opens != 1 {
				t.Fatalf("expected one sized submission: opens=%d err=%v", m.opens, err)
			}
			if m.quantity > 7.5 || math.Abs(d.CycleReservedRiskUSD-m.quantity*2) > 1e-9 || math.Abs(at.cycleRiskReservedUSD-d.CycleReservedRiskUSD) > 1e-9 {
				t.Fatalf("final size/reservation exceeds 15U room: qty=%v decision=%v total=%v", m.quantity, d.CycleReservedRiskUSD, at.cycleRiskReservedUSD)
			}
			if rec.Quantity != m.quantity || d.PositionSizeUSD != m.quantity*price {
				t.Fatal("submission record or final notional diverged")
			}
		})
	}
}

// review 2026-10-28 #9: net direction, opposite-side symbol risk and other batch reservations bind final sizing.
func TestReview28FinalRiskBudgetsAndMinimum(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		netCap, symbolCap, minSize, wantQty float64
	}{
		{"net", 1.2, 10, 12, 1.5}, {"symbol", -1, 1.2, 12, 1.5}, {"minimum", 1.2, 10, 200, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetDailyBaselineRegistryForTest()
			at, m := review28Trader(store.RiskControlConfig{MaxAccountRiskPct: 20, MaxNetDirectionalRiskPct: tc.netCap, RiskPerTradePct: tc.symbolCap, MinPositionSize: tc.minSize})
			pendingSide, stop := "long", 95.0
			if tc.name == "symbol" {
				pendingSide, stop = "short", 105
			}
			at.setPendingEntry(&pendingEntry{Symbol: "YUSDT", Side: pendingSide, Price: 100, Quantity: 1, StopLoss: stop})
			d := &kernel.Decision{Symbol: "YUSDT", Action: "open_long", StopLoss: 98, CycleReservedRiskUSD: 5}
			at.cycleRiskReservedUSD, at.cycleLongRiskReservedUSD = 9, 9
			at.cycleSymbolRiskReservedUSD = map[string]float64{"YUSDT": 9}
			qty, err := at.finalizeOpenRisk(d, 100, 10)
			if tc.wantQty == 0 {
				if err == nil || d.CycleReservedRiskUSD != 5 || at.cycleRiskReservedUSD != 9 {
					t.Fatalf("minimum must refuse and restore old reservation: qty=%v err=%v", qty, err)
				}
			} else if err != nil || math.Abs(qty-tc.wantQty) > .0011 || math.Abs(at.cycleRiskReservedUSD-(4+qty*2)) > 1e-8 {
				t.Fatalf("remaining budget ignored or old reservation counted twice: qty=%v err=%v total=%v", qty, err, at.cycleRiskReservedUSD)
			}
			m.roundUp = true
			if _, err := at.finalizeOpenRisk(d, 100, .123); err == nil {
				t.Fatal("rounding up must not exceed final budget")
			}
		})
	}
}

// review 2026-10-28 #2: grid entries share gross/net/symbol breakers, with exact configured stop risk.
func TestReview28GridAccountAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		stop, gross, net, symbol float64
		halt                     string
	}{
		{"gross", 5, 2, -1, 10, "max_account_risk_pct"},
		{"unprotected", 0, 3, -1, 10, "max_account_risk_pct"},
		{"net", 5, 20, 2, 10, "max_net_directional_risk_pct"},
		{"symbol", 5, 20, -1, 2, "gross long+short"},
		{"daily", 5, 20, -1, 10, "daily loss halt"},
		{"drawdown", 5, 20, -1, 10, "account drawdown"},
		{"pass", 5, 20, -1, 10, "review"},
	} {
		for _, side := range []string{"BUY", "SELL"} {
			t.Run(tc.name+side, func(t *testing.T) {
				ResetDailyBaselineRegistryForTest()
				at, m := review28Trader(store.RiskControlConfig{DailyMaxLossPct: 10, AccountMaxDrawdownPct: 10, RiskPerTradePct: tc.symbol, MaxAccountRiskPct: tc.gross, MaxNetDirectionalRiskPct: tc.net})
				cfg := &store.GridStrategyConfig{Symbol: "YUSDT", GridCount: 1, TotalInvestment: 1000, Leverage: 1, StopLossPct: tc.stop}
				at.config.StrategyConfig.GridConfig = cfg
				at.gridState = &GridState{Config: cfg, Levels: []kernel.GridLevelInfo{{}}, OrderBook: map[string]int{}}
				if tc.name == "daily" {
					at.dailyHaltDay, at.dailyHaltReason = dailyRiskNow().UTC().Format("2006-01-02"), "test latched halt"
				}
				if tc.name == "drawdown" {
					at.initialBalance = 1200
				}
				err := at.placeGridLimitOrder(&kernel.Decision{Symbol: "YUSDT", Price: 100, Quantity: 5, LevelIndex: 0}, side)
				if tc.name == "pass" {
					if m.limits != 1 {
						t.Fatalf("compliant grid did not reach adapter: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tc.halt) || m.limits != 0 {
					t.Fatalf("grid bypassed %s: calls=%d err=%v", tc.name, m.limits, err)
				}
			})
		}
	}
	if risk := pendingEntryStopRisk(&pendingEntry{Price: 100, Quantity: 5}); risk != 40 {
		t.Fatalf("stopless pending grid risk=%v", risk)
	}
}

// review 2026-10-28 #2: N levels share one refresh and consume cumulative risk/margin.
func TestReview28GridBatchSnapshotAndCumulativeRisk(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		gross, net, symbol, margin, stop float64
		wantAccepted                     int
		reconciled                       bool
		reason                           string
	}{
		{"gross", 2.5, -1, 10, 0, 5, 2, false, "max_account_risk_pct"},
		{"net", 20, 2.5, 10, 0, 5, 2, false, "max_net_directional_risk_pct"},
		{"symbol", 20, -1, 2.5, 0, 5, 2, false, "gross long+short"},
		{"unprotected", 2.5, -1, 10, 0, 0, 1, false, "max_account_risk_pct"},
		{"margin", 20, -1, 10, .45, 5, 2, false, "margin budget"},
		{"filled_during_batch", 2.5, -1, 10, 0, 5, 2, true, "max_account_risk_pct"},
	} {
		for _, side := range []string{"long", "short"} {
			t.Run(tc.name+"/"+side, func(t *testing.T) {
				ResetDailyBaselineRegistryForTest()
				t.Cleanup(ResetDailyBaselineRegistryForTest)
				at, base := review28Trader(store.RiskControlConfig{MaxAccountRiskPct: tc.gross, MaxNetDirectionalRiskPct: tc.net, RiskPerTradePct: tc.symbol, MaxMarginUsage: tc.margin})
				m := &review28GridBatchExchange{review28RiskExchange: base}
				at.trader = m
				m.positions = []map[string]interface{}{review28Position("XUSDT", side, 1)}
				stop, closingSide := 99.0, "SELL"
				if side == "short" {
					stop, closingSide = 101, "BUY"
				}
				m.orders = []types.OpenOrder{{Symbol: "XUSDT", Type: "STOP_MARKET", PositionSide: strings.ToUpper(side), Side: closingSide, StopPrice: stop, Quantity: 1}}
				cfg := &store.GridStrategyConfig{Symbol: "YUSDT", GridCount: 5, TotalInvestment: 2000, Leverage: 1, StopLossPct: tc.stop}
				at.config.StrategyConfig.GridConfig = cfg
				at.gridState = &GridState{Config: cfg, Levels: make([]kernel.GridLevelInfo, cfg.GridCount), OrderBook: map[string]int{}}
				batch := &gridEntryBatch{}
				action := "place_buy_limit"
				if side == "short" {
					action = "place_sell_limit"
				}
				for i := 0; i < cfg.GridCount; i++ {
					err := at.executeGridDecisionWithBatch(&kernel.Decision{Action: action, Symbol: cfg.Symbol, Price: 100, Quantity: 2, LevelIndex: i}, batch)
					if i < tc.wantAccepted {
						if err != nil {
							t.Fatalf("level %d should fit: %v", i, err)
						}
						if tc.reconciled {
							level := &at.gridState.Levels[i]
							level.State, level.OrderID, level.ExecutedQuantity, level.PositionSize = "filled", "", 2, 2
							level.OrderQuantity = 0
						}
					} else if err == nil || !strings.Contains(err.Error(), tc.reason) {
						t.Fatalf("level %d bypassed cumulative %s: %v", i, tc.name, err)
					}
				}
				if m.limits != tc.wantAccepted || m.balanceCalls != 1 || m.positionCalls != 1 || m.orderCalls != 1 {
					t.Fatalf("batch counts: submissions=%d want=%d balance=%d positions=%d orders=%d", m.limits, tc.wantAccepted, m.balanceCalls, m.positionCalls, m.orderCalls)
				}
				if err := (&gridEntryBatch{}).refresh(at); err != nil || m.balanceCalls != 2 || m.positionCalls != 2 || m.orderCalls != 2 {
					t.Fatalf("new batch reused stale snapshot: balance=%d positions=%d orders=%d err=%v", m.balanceCalls, m.positionCalls, m.orderCalls, err)
				}
			})
		}
	}
}

// review 2026-10-28 #2: cache a failed refresh, and never reserve rejected submissions.
func TestReview28GridBatchFailures(t *testing.T) {
	for _, refreshFails := range []bool{false, true} {
		t.Run(fmt.Sprint(refreshFails), func(t *testing.T) {
			ResetDailyBaselineRegistryForTest()
			t.Cleanup(ResetDailyBaselineRegistryForTest)
			at, base := review28Trader(store.RiskControlConfig{MaxAccountRiskPct: 1.1, MaxNetDirectionalRiskPct: -1, RiskPerTradePct: 10})
			m := &review28GridBatchExchange{review28RiskExchange: base, submitErr: errors.New("submit failed")}
			if refreshFails {
				m.balanceErr = errors.New("balance unavailable")
			}
			at.trader = m
			cfg := &store.GridStrategyConfig{Symbol: "YUSDT", GridCount: 5, TotalInvestment: 2000, Leverage: 1, StopLossPct: 5}
			at.config.StrategyConfig.GridConfig = cfg
			at.gridState = &GridState{Config: cfg, Levels: make([]kernel.GridLevelInfo, cfg.GridCount), OrderBook: map[string]int{}}
			batch := &gridEntryBatch{}
			for i := 0; i < cfg.GridCount; i++ {
				err := at.executeGridDecisionWithBatch(&kernel.Decision{Action: "place_buy_limit", Symbol: cfg.Symbol, Price: 100, Quantity: 2, LevelIndex: i}, batch)
				wantReason := "submit failed"
				if refreshFails {
					wantReason = "balance unavailable"
				}
				if err == nil || !strings.Contains(err.Error(), wantReason) {
					t.Fatalf("level %d unexpected error: %v", i, err)
				}
			}
			wantPositions, wantSubmissions := 1, 5
			if refreshFails {
				wantPositions, wantSubmissions = 0, 0
			}
			if m.balanceCalls != 1 || m.positionCalls != wantPositions || m.limits != wantSubmissions || len(batch.pending) != 0 {
				t.Fatalf("failed batch: balance=%d positions=%d submissions=%d pending=%d", m.balanceCalls, m.positionCalls, m.limits, len(batch.pending))
			}
		})
	}
}

// review 2026-10-28 #10: latest trigger, full 24h formation span and 48h query history agree with prompt.
func TestReview28LossStreakWindowAndPrompt(t *testing.T) {
	st := profitLockTestStore(t)
	now := time.Now()
	positions := []store.TraderPosition{mkClosed("YUSDT", 2, -1, now.UnixMilli()), mkClosed("YUSDT", 24, -1, now.UnixMilli()), mkClosed("YUSDT", 25, -1, now.UnixMilli())}
	for i := range positions {
		positions[i].TraderID, positions[i].AIManaged, positions[i].Side = "review28", true, "LONG"
		if err := st.GormDB().Create(&positions[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	at := &AutoTrader{id: "review28", store: st}
	_, until := lossStreakState(positions, 3, now.UnixMilli())
	if !until.Equal(time.UnixMilli(positions[0].ExitTime).Add(24 * time.Hour)) {
		t.Fatalf("wrong trigger: %v", until)
	}
	if blocked, _ := at.lossStreakBlocks("YUSDT", 3); !blocked {
		t.Fatal("older losses sliding out of 24h must not end the ban")
	}
	if got := at.lossStreakBannedMap(3)["YUSDT"]; !got.Equal(until) {
		t.Fatalf("prompt/executor mismatch: %v vs %v", got, until)
	}
	positions[2].ExitTime = positions[0].ExitTime - lossStreakWindow.Milliseconds() - 1
	if blocked, _ := lossStreakVerdict(positions, 3, now.UnixMilli()); blocked {
		t.Fatal("formation span beyond 24h must pass")
	}
	positions[2].ExitTime++
	if blocked, _ := lossStreakVerdict(positions, 3, until.UnixMilli()); blocked {
		t.Fatal("ban must expire exactly at deadline")
	}
}

// review 2026-10-28 #11: normalized exact majors only; ETHFI/BTCDOM use the 1x altcoin cap.
func TestReview28ExactMajorClassification(t *testing.T) {
	at := riskTestTrader(store.RiskControlConfig{})
	for _, symbol := range []string{"ETHFIUSDT", "BTCDOMUSDT"} {
		if got, capped := at.enforcePositionValueRatio(1000, 950, symbol); !capped || got != 950 {
			t.Fatalf("%s misclassified: %v %v", symbol, got, capped)
		}
	}
	for _, symbol := range []string{"btc", "ethusdt", "BTCUSDT", "ETHUSDT"} {
		if !isBTCETH(symbol) {
			t.Fatalf("normalized major rejected: %s", symbol)
		}
	}
}

// review 2026-10-28 #12: table failures block entries while missing exit data permits de-risking.
func TestReview28LossHistoryFailureAndUnknownExit(t *testing.T) {
	st := profitLockTestStore(t)
	at, _ := review28Trader(store.RiskControlConfig{LossStreakBanEnabled: true})
	at.store = st
	review28QueryFailure(t, st, "trader_positions")
	if blocked, reason := at.lossStreakBlocks("YUSDT", 3); !blocked || !strings.Contains(reason, "LOSS_HISTORY_UNAVAILABLE") {
		t.Fatalf("unreadable history passed: %v %s", blocked, reason)
	}
	if err := at.entryExecutionBlocked("YUSDT", "long"); err == nil || !strings.Contains(err.Error(), "LOSS_HISTORY_UNAVAILABLE") {
		t.Fatalf("executor passed: %v", err)
	}
	if got := at.lossStreakBannedMap(3); got != nil {
		t.Fatal("prompt failure remains log-only")
	}
	for _, side := range []string{"long", "short"} {
		at.positionFirstSeenTime["YUSDT_"+side] = time.Now().Add(-time.Hour).UnixMilli()
		for _, data := range []*market.Data{nil, {}, {TimeframeData: map[string]*market.TimeframeSeriesData{"1h": {}}}} {
			if blocked, reason := at.earlyCloseBlocksClose("YUSDT", side, 100, 100, data); blocked {
				t.Fatalf("unknown exit data trapped %s: %s", side, reason)
			}
		}
	}
}

// review 2026-10-28 #12: preserve confirmed AI/manual ownership and deduplicate concurrent failures.
func TestReview28OwnershipFailurePolicy(t *testing.T) {
	st := profitLockTestStore(t)
	at := &AutoTrader{id: "review28", store: st}
	if err := st.AIManaged().Mark(at.id, "AIUSDT", "long"); err != nil {
		t.Fatal(err)
	}
	if !at.isAIManaged("AIUSDT", "long") || at.isAIManaged("MANUALUSDT", "long") {
		t.Fatal("ownership seed failed")
	}
	review28QueryFailure(t, st, "ai_managed_positions")
	if !at.isAIManaged("AIUSDT", "long") || at.isAIManaged("MANUALUSDT", "long") {
		t.Fatal("last successful ownership was lost")
	}
	if err := at.entryExecutionBlocked("AIUSDT", "long"); err != nil {
		t.Fatalf("known cached result must be usable: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !at.isAIManaged("UNKNOWNUSDT", "long") {
				t.Error("unknown must retain protection")
			}
		}()
	}
	wg.Wait()
	if err := at.entryExecutionBlocked("UNKNOWNUSDT", "long"); err == nil || !strings.Contains(err.Error(), "AI_MANAGED_UNAVAILABLE") {
		t.Fatalf("unknown ownership admitted: %v", err)
	}
	if got := at.gateNotify["ownership:UNKNOWNUSDT_long"].Streak; got != 9 {
		t.Fatalf("failure dedup calls=%d", got)
	}
}

// review 2026-10-28 #12: ownership failure must not skip an actual crossed-stop exit.
func TestReview28UnknownOwnershipKeepsWatchdogProtection(t *testing.T) {
	st := profitLockTestStore(t)
	review28QueryFailure(t, st, "ai_managed_positions")
	m := &audit05Mock{mark: 90, flatOnClose: true}
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader, at.store = m, st
	at.positionInitialStopLoss = map[string]float64{}
	at.SetRecordedStopLoss("XUSDT", "long", 95)
	at.processProtectionWatchdog()
	if m.closeCalls == 0 {
		t.Fatal("unknown ownership suppressed the crossed-stop emergency exit")
	}
}

// review 2026-10-28 #2 #12: reducing grid exits bypass entry breakers and unavailable history.
func TestReview28GridExitBypassesEntryFailures(t *testing.T) {
	st := profitLockTestStore(t)
	review28QueryFailure(t, st, "trader_positions")
	m := &gridExitMock{review03Mock: &review03Mock{}}
	at := review03Trader(m.review03Mock)
	at.store, at.trader = st, m
	at.config.StrategyConfig.RiskControl.LossStreakBanEnabled = true
	at.config.StrategyConfig.GridConfig = &store.GridStrategyConfig{Symbol: "XUSDT"}
	at.dailyHaltDay, at.dailyHaltReason = dailyRiskNow().UTC().Format("2006-01-02"), "test latch"
	at.gridState = &GridState{Levels: []kernel.GridLevelInfo{{Side: "buy", State: "filled", PositionSize: .6, PositionEntry: 100}}}
	if err := at.closeGridLevel(0); err != nil || m.closeCalls != 1 || at.gridState.Levels[0].PositionSize != 0 {
		t.Fatalf("entry failure blocked grid exit: calls=%d err=%v", m.closeCalls, err)
	}
}

// review 2026-10-28 #12: dashboard and executor share the durable latch, including read errors.
func TestReview28DailyLatchReadAndDashboard(t *testing.T) {
	ResetDailyBaselineRegistryForTest()
	t.Cleanup(ResetDailyBaselineRegistryForTest)
	st := profitLockTestStore(t)
	at, _ := review28Trader(store.RiskControlConfig{DailyMaxLossPct: 10})
	at.store = st
	at.anchorDailyBaseline(1000)
	day := dailyRiskNow().UTC().Format("2006-01-02")
	if err := st.RiskState().LatchDayHalt(at.dailyBaselineKey(), day, time.Now()); err != nil {
		t.Fatal(err)
	}
	status := at.RiskStatus()
	if !status.DailyHalted || status.DailyHaltUnknown || status.DailyLossPct != 0 {
		t.Fatalf("dashboard lost latch after recovery: %+v", status)
	}
	if at.dailyHaltDay != "" {
		t.Fatal("dashboard mutated executor latch")
	}
	review28QueryFailure(t, st, "risk_baselines")
	if reason := at.dailyLossHaltBlocks(at.config.StrategyConfig.RiskControl, 1000); !strings.Contains(reason, "DAILY_HALT_UNAVAILABLE") {
		t.Fatalf("unreadable latch passed: %s", reason)
	}
	status = at.RiskStatus()
	if !status.DailyHaltUnknown {
		t.Fatal("dashboard concealed unknown latch")
	}
	at.dailyHaltDay, at.dailyHaltReason = day, "memory latch"
	if status = at.RiskStatus(); !status.DailyHalted || status.DailyHaltUnknown {
		t.Fatal("confirmed memory latch must remain authoritative")
	}
}

// review 2026-10-28 #12: observed loss and executor latch independently imply a halt.
func TestReview28DailyStatusBeforeLatch(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		cap                                       float64
		memory, durable, readErr, halted, unknown bool
	}{
		{"at_cap", 20, false, false, false, true, false},
		{"above_cap", 10, false, false, false, true, false},
		{"below_cap", 21, false, false, false, false, false},
		{"unknown_above_cap", 10, false, false, true, true, true},
		{"unknown_below_cap", 21, false, false, true, false, true},
		{"disabled", -1, false, false, false, false, false},
		{"memory_disabled", -1, true, false, false, true, false},
		{"durable_disabled", -1, false, true, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetDailyBaselineRegistryForTest()
			t.Cleanup(ResetDailyBaselineRegistryForTest)
			at, _ := review28Trader(store.RiskControlConfig{DailyMaxLossPct: tc.cap})
			day := dailyRiskNow().UTC().Format("2006-01-02")
			at.dayStartDay, at.dayStartEquity = day, 1250
			if tc.memory {
				at.dailyHaltDay, at.dailyHaltReason = day, "memory latch"
			}
			if tc.durable || tc.readErr {
				at.store = profitLockTestStore(t)
				if tc.durable {
					if _, err := at.store.RiskState().AnchorDayBaseline(at.dailyBaselineKey(), day, 1250); err != nil {
						t.Fatal(err)
					}
					if err := at.store.RiskState().LatchDayHalt(at.dailyBaselineKey(), day, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				if tc.readErr {
					review28QueryFailure(t, at.store, "risk_baselines")
				}
			}
			status := at.RiskStatus()
			if status.DailyLossPct != 20 || status.DailyHalted != tc.halted || status.DailyHaltUnknown != tc.unknown {
				t.Fatalf("loss/latch status mismatch: %+v", status)
			}
			if !tc.memory && at.dailyHaltReason != "" {
				t.Fatal("dashboard latched execution state")
			}
		})
	}
}

// review 2026-10-28 #2 #12: retain the dedicated operational notification keys.
func TestReview28HardGateDedicatedBreakers(t *testing.T) {
	for _, tc := range []struct {
		name, key, reason string
	}{
		{"drawdown", "acctdd:YUSDT", "account drawdown ≥ cap"},
		{"daily", "dailyhalt:" + time.Now().Format("2006-01-02"), "daily loss halt"},
		{"daily_unknown", "dailyhalt:" + time.Now().Format("2006-01-02"), "DAILY_HALT_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetDailyBaselineRegistryForTest()
			t.Cleanup(ResetDailyBaselineRegistryForTest)
			at := riskTestTrader(store.RiskControlConfig{AccountMaxDrawdownPct: 10, DailyMaxLossPct: 10})
			if tc.name == "drawdown" {
				at.initialBalance = 1200
			} else if tc.name == "daily" {
				at.dailyHaltDay, at.dailyHaltReason = dailyRiskNow().UTC().Format("2006-01-02"), "memory halt"
			} else {
				at.store = profitLockTestStore(t)
				review28QueryFailure(t, at.store, "risk_baselines")
			}
			ctx := &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}, CandidateCoins: []kernel.CandidateCoin{{Symbol: "YUSDT"}}}
			decisions := []kernel.Decision{{Symbol: "YUSDT", Action: "open_long"}, {Symbol: "YUSDT", Action: "close_long"}}
			got := at.applyHardRiskGates(decisions, ctx)
			if len(got) != 1 || got[0].Action != "close_long" {
				t.Fatalf("breaker must only block new risk: %v", got)
			}
			if at.gateNotify[tc.key] == nil || at.gateNotify["exposure:YUSDT"] != nil {
				t.Fatalf("dedicated breaker replaced by exposure notification: %v", at.gateNotify)
			}
			if reason := at.popFilterReason(decisions[0]); !strings.Contains(reason, tc.reason) {
				t.Fatalf("dedicated reason lost: %s", reason)
			}
		})
	}
}

// review 2026-10-28 #13: only the disallowed direction's extended-pump code is mirrored.
func TestReview28MarketExtendedPumpMirror(t *testing.T) {
	for _, side := range []string{"long", "short"} {
		for _, allowed := range []bool{false, true} {
			at := riskTestTrader(store.RiskControlConfig{LimitEntryEnabled: false})
			gs := &kernel.GateState{LongAllowed: true, ShortAllowed: true}
			if side == "long" {
				gs.LongAllowed, gs.LongFailed = allowed, []string{"EXTENDED_PUMP_UNCONFIRMED"}
			} else {
				gs.ShortAllowed, gs.ShortFailed = allowed, []string{"EXTENDED_PUMP_UNCONFIRMED"}
			}
			at.cycleGateStates = map[string]*kernel.GateState{"YUSDT": gs}
			d := kernel.Decision{Symbol: "YUSDT", Action: "open_" + side, Price: 100, PositionSizeUSD: 100, StopLoss: 95}
			_, err := at.marketExceptionGate(&d, &store.DecisionAction{}, side == "long")
			if (err == nil) != allowed {
				t.Fatalf("market default side=%s allowed=%v err=%v", side, allowed, err)
			}
			if err := at.entryExecutionBlocked("YUSDT", side); (err == nil) != allowed {
				t.Fatalf("direct execution side=%s allowed=%v err=%v", side, allowed, err)
			}
			ctx := &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}, CandidateCoins: []kernel.CandidateCoin{{Symbol: "YUSDT"}}, MarketDataMap: map[string]*market.Data{"YUSDT": {CurrentPrice: 100}}}
			if got := at.applyHardRiskGates([]kernel.Decision{d}, ctx); (len(got) == 1) != allowed {
				t.Fatalf("hard gate side=%s allowed=%v decisions=%v", side, allowed, got)
			}
		}
	}
}
