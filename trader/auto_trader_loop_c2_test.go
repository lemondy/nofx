package trader

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"nofx/kernel"
	"nofx/market"
	"nofx/store"
	"nofx/trader/types"
)

type c2PendingMock struct {
	audit05Mock
	statusCalls int
	statusErr   error
}

func (m *c2PendingMock) GetPositions() ([]map[string]interface{}, error) {
	if m.flat {
		return nil, nil
	}
	return []map[string]interface{}{{"symbol": "XUSDT", "side": "long", "positionAmt": 1.0,
		"entryPrice": 100.0, "markPrice": 100.0, "leverage": 2.0,
		"unRealizedProfit": 0.0, "liquidationPrice": 50.0}}, nil
}

func (m *c2PendingMock) GetOrderStatus(symbol, id string) (map[string]interface{}, error) {
	m.statusCalls++
	if m.statusErr != nil {
		return nil, m.statusErr
	}
	return m.audit05Mock.GetOrderStatus(symbol, id)
}

func c2Trader(m *c2PendingMock) *AutoTrader {
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = m
	at.config.StrategyConfig.CoinSource.SourceType = "static"
	at.strategyEngine = kernel.NewStrategyEngine(at.config.StrategyConfig)
	at.isRunning = true
	at.lastResetTime = time.Now()
	at.initialBalance = 900
	at.positionInitialStopLoss = make(map[string]float64)
	at.positionExitMode = make(map[string]string)
	fix7AnchorDay(at)
	at.marketDataFn = func(string) (*market.Data, error) { return nil, errors.New("offline test") }
	return at
}

func c2ProtectionOrders() []types.OpenOrder {
	return []types.OpenOrder{
		{Type: "STOP_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 95, Quantity: 1},
		{Type: "TAKE_PROFIT_MARKET", PositionSide: "LONG", Side: "SELL", StopPrice: 110, Quantity: 1},
	}
}

func TestC2EmptyCandidatesStillManagePositions(t *testing.T) {
	for _, source := range []string{"static", "unavailable-source", "excluded"} {
		t.Run(source, func(t *testing.T) {
			m := &c2PendingMock{audit05Mock: audit05Mock{orders: c2ProtectionOrders()}}
			at := c2Trader(m)
			if source == "excluded" {
				at.config.StrategyConfig.CoinSource.StaticCoins = []string{"XUSDT"}
				at.config.StrategyConfig.CoinSource.ExcludedCoins = []string{"XUSDT"}
			} else {
				at.config.StrategyConfig.CoinSource.SourceType = source
			}
			at.markAIManaged("XUSDT", "long", "entry")
			called := false
			stop := errors.New("model seam reached")
			err := at.runCycleWithDecision(func(ctx *kernel.Context) (*kernel.FullDecision, error) {
				called = true
				if len(ctx.CandidateCoins) != 0 || len(ctx.Positions) != 1 || !ctx.Positions[0].Managed {
					t.Fatalf("management context lost: %+v", ctx)
				}
				return nil, stop
			})
			if !called || !errors.Is(err, stop) {
				t.Fatalf("empty candidates skipped position management: called=%v err=%v", called, err)
			}
		})
	}
}

func TestC2EmptyIdleCycleSkipsButPendingCycleContinues(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "pending"}[pending], func(t *testing.T) {
			m := &c2PendingMock{audit05Mock: audit05Mock{flat: true}}
			at := c2Trader(m)
			if pending {
				at.config.StrategyConfig = nil // no market-data recheck needed for this lifecycle test
				at.setPendingEntry(&pendingEntry{Symbol: "XUSDT", Side: "long", OrderID: "entry", PlacedAt: time.Now()})
			}
			called := false
			stop := errors.New("model seam reached")
			err := at.runCycleWithDecision(func(*kernel.Context) (*kernel.FullDecision, error) {
				called = true
				return nil, stop
			})
			if called != pending || (pending && !errors.Is(err, stop)) || (!pending && err != nil) {
				t.Fatalf("pending=%v called=%v err=%v", pending, called, err)
			}
		})
	}
}

func TestC2ManagementOnlyCycleRejectsAllOpenActions(t *testing.T) {
	for _, limitEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "market", true: "limit"}[limitEnabled], func(t *testing.T) {
			m := &c2PendingMock{audit05Mock: audit05Mock{orders: c2ProtectionOrders()}}
			at := c2Trader(m)
			at.config.StrategyConfig.RiskControl.LimitEntryEnabled = limitEnabled
			st, err := store.New(filepath.Join(t.TempDir(), "management.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			at.id, at.store = "c2-management", st // rule-store availability must not mask an entry-filter regression
			at.markAIManaged("XUSDT", "long", "entry")
			// cycleEntryPool includes this managed position for adds. The cycle must
			// reject even those opens before sizing/execution when entry candidates vanish.
			at.marketDataFn = func(string) (*market.Data, error) {
				t.Fatal("management-only cycle attempted entry market-data fetch")
				return nil, nil
			}
			err = at.runCycleWithDecision(func(*kernel.Context) (*kernel.FullDecision, error) {
				decisions := []kernel.Decision{{Symbol: "XUSDT", Action: "hold"}}
				for _, action := range []string{"open_long", "open_short", "open_long_limit", "open_short_limit"} {
					decisions = append(decisions, kernel.Decision{Symbol: "XUSDT", Action: action, Price: 100,
						StopLoss: 95, TakeProfit: 110, Leverage: 2, Confidence: 100, PositionSizeUSD: 100})
				}
				return &kernel.FullDecision{Decisions: decisions}, nil
			})
			if err != nil || at.cycleRiskReservedUSD != 0 || m.closeCalls != 0 || len(at.filterReasons) != 0 {
				t.Fatalf("management-only opens reached risk gates/execution: err=%v reserved=%v close=%d filters=%v", err, at.cycleRiskReservedUSD, m.closeCalls, at.filterReasons)
			}

		})
	}
}
