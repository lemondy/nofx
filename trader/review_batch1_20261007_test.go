package trader

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"nofx/kernel"
	"nofx/market"
	"nofx/store"
	"nofx/trader/types"
)

// review 2026-10-07 B1-1/B1-2: all execution receipts stay in memory.
type batch1RiskMock struct {
	fix06Mock
	closedQty  float64
	stopPrices []float64
}

func (m *batch1RiskMock) CloseLong(_ string, qty float64) (map[string]interface{}, error) {
	m.closeCalls++
	m.closedQty = qty
	return map[string]interface{}{}, nil
}

func (m *batch1RiskMock) CloseShort(_ string, qty float64) (map[string]interface{}, error) {
	m.closeShorts++
	m.closedQty = qty
	return map[string]interface{}{}, nil
}

func (m *batch1RiskMock) SetStopLoss(symbol, side string, qty, price float64) error {
	m.slCalls++
	m.stopPrices = append(m.stopPrices, price)
	m.orders = append(m.orders, types.OpenOrder{
		OrderID: fmt.Sprint(m.slCalls), Symbol: symbol, PositionSide: side,
		Type: "STOP_MARKET", Quantity: qty, StopPrice: price,
	})
	return nil
}

func (m *batch1RiskMock) GetMarketPrice(string) (float64, error) { return m.mark, nil }

func newBatch1RiskTrader(side string, qty, mark float64, rc store.RiskControlConfig) (*AutoTrader, *batch1RiskMock) {
	m := &batch1RiskMock{fix06Mock: fix06Mock{
		audit05Mock: audit05Mock{mark: mark}, symbol: "XUSDT", side: side, qty: qty,
	}}
	at := riskTestTrader(rc)
	at.trader = m
	at.positionInitialStopLoss = make(map[string]float64)
	at.r1TrimDone = make(map[string]bool)
	at.tpTrimDone = make(map[string]bool)
	return at, m
}

// review 2026-10-07 B1-1: signed shorts must close half with a positive order size.
func TestB1PartialCloseSignedQuantity(t *testing.T) {
	for _, tc := range []struct {
		side string
		qty  float64
	}{
		{"short", -1}, {"short", 1}, {"long", 1},
	} {
		t.Run(fmt.Sprintf("%s_%g", tc.side, tc.qty), func(t *testing.T) {
			at, m := newBatch1RiskTrader(tc.side, tc.qty, 100, store.RiskControlConfig{MinPositionSize: 1})
			record := &store.DecisionAction{}
			err := at.executePartialCloseWithRecord(&kernel.Decision{
				Symbol: "XUSDT", CloseFraction: 0.5,
			}, record, tc.side)
			if err != nil || m.closedQty != 0.5 || record.Quantity != 0.5 {
				t.Fatalf("err=%v order quantity=%g record quantity=%g", err, m.closedQty, record.Quantity)
			}
			if m.closeCalls+m.closeShorts != 1 || at.partialTrimmed["XUSDT_"+tc.side] != 0.5 {
				t.Fatalf("unexpected reduction accounting: calls=%d/%d trimmed=%v", m.closeCalls, m.closeShorts, at.partialTrimmed)
			}
		})
	}
}

// review 2026-10-07 B1-1: malformed quantities must never reach the close adapter.
func TestB1PartialCloseRejectsNonFiniteQuantity(t *testing.T) {
	for _, qty := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0} {
		t.Run(fmt.Sprint(qty), func(t *testing.T) {
			at, m := newBatch1RiskTrader("short", qty, 100, store.RiskControlConfig{})
			if err := at.executePartialCloseWithRecord(&kernel.Decision{
				Symbol: "XUSDT", CloseFraction: 0.5,
			}, &store.DecisionAction{}, "short"); err == nil {
				t.Fatal("invalid quantity accepted")
			}
			if m.closeShorts != 0 {
				t.Fatal("close submitted for an invalid quantity")
			}
		})
	}
}

// review 2026-10-07 B1-1: persisted entry size and dust checks use the short's magnitude.
func TestB1PartialCloseShortReductionAccounting(t *testing.T) {
	at, m := newBatch1RiskTrader("short", -1, 100, store.RiskControlConfig{MinPositionSize: 1})
	at.store = profitLockTestStore(t)
	at.id = "batch1"
	if err := at.store.AIManaged().Mark(at.id, "XUSDT", "short"); err != nil {
		t.Fatal(err)
	}
	if err := at.store.Position().CreateOpenPosition(&store.TraderPosition{
		TraderID: at.id, Symbol: "XUSDT", Side: "SHORT", EntryPrice: 100,
		Quantity: 1, EntryQuantity: 2, EntryTime: 1,
	}); err != nil {
		t.Fatal(err)
	}
	decision := &kernel.Decision{Symbol: "XUSDT", CloseFraction: 0.5}
	if err := at.executePartialCloseWithRecord(decision, &store.DecisionAction{}, "short"); err != nil {
		t.Fatal(err)
	}
	if m.closedQty != 0.5 || at.partialTrimmed["XUSDT_short"] != 0.75 {
		t.Fatalf("wrong entry-size accounting: qty=%g trimmed=%v", m.closedQty, at.partialTrimmed)
	}
	m.qty = -0.5
	if err := at.executePartialCloseWithRecord(decision, &store.DecisionAction{}, "short"); err == nil {
		t.Fatal("cumulative reduction above 75% accepted")
	}
	if m.closeShorts != 1 {
		t.Fatal("second close submitted beyond the cumulative cap")
	}

	dust, dustMock := newBatch1RiskTrader("short", -1, 100, store.RiskControlConfig{MinPositionSize: 60})
	if err := dust.executePartialCloseWithRecord(decision, &store.DecisionAction{}, "short"); err == nil || dustMock.closeShorts != 0 {
		t.Fatalf("dust remainder accepted: err=%v calls=%d", err, dustMock.closeShorts)
	}
}

// review 2026-10-07 B1-2: reject loosening before touching exchange protection.
func TestB1MoveStopExchangeTightenOnly(t *testing.T) {
	for _, tc := range []struct {
		side                string
		qty, current, loose float64
		tight               float64
	}{
		{"long", 1, 101, 99.36, 102}, {"short", -1, 99, 100.64, 98},
	} {
		t.Run(tc.side, func(t *testing.T) {
			at, m := newBatch1RiskTrader(tc.side, tc.qty, 110, store.RiskControlConfig{})
			at.SetRecordedStopLoss("XUSDT", tc.side, tc.current)
			m.orders = []types.OpenOrder{{OrderID: "old", Type: "STOP_MARKET", PositionSide: strings.ToUpper(tc.side), StopPrice: tc.current, Quantity: 1}}
			if err := at.moveStopExchange("XUSDT", tc.side, tc.loose); err == nil || !strings.Contains(err.Error(), "loosen") {
				t.Fatalf("loosening accepted: %v", err)
			}
			if m.slCalls != 0 || m.cancelCalls != 0 {
				t.Fatal("loosening touched exchange orders")
			}
			if err := at.moveStopExchange("XUSDT", tc.side, tc.tight); err != nil {
				t.Fatalf("tightening rejected: %v", err)
			}
			if m.slCalls != 1 || m.stopPrices[0] != tc.tight || m.cancelCalls != 1 {
				t.Fatalf("tightening not placed/retired: prices=%v cancels=%d", m.stopPrices, m.cancelCalls)
			}
		})
	}
}

type batch1MarketTransport func(*http.Request) (*http.Response, error)

func (f batch1MarketTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// review 2026-10-07 B1-2: exercise the entire cycle with in-memory candle responses.
func TestB1BreakevenTrailingSameCycle(t *testing.T) {
	for _, tc := range []struct {
		name, side                       string
		qty, initial, current, mark, atr float64
		armR, lockR                      float64
		wantPrices                       []float64
		wantRecorded                     float64
	}{
		{"early_and_lock", "long", 1, 95, 95, 108, 4, 0.5, 1, []float64{101}, 101},
		{"lock_only", "long", 1, 95, 95, 108, 4, 0, 1, []float64{101}, 101},
		{"early_short", "short", -1, 105, 105, 92, 4, 0.5, 1, []float64{99}, 99},
		{"opening_anchor_after_breakeven", "long", 1, 95, 101, 104, 0.5, 0, -1, nil, 101},
		{"armed_tightening", "long", 1, 95, 101, 108, 0.5, 0, -1, []float64{106.92}, 106.92},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := http.DefaultTransport
			defer func() { http.DefaultTransport = previous }()
			candleRequests := 0
			http.DefaultTransport = batch1MarketTransport(func(r *http.Request) (*http.Response, error) {
				var payload interface{}
				if r.URL.Host == "api.coinank.com" && r.URL.Path == "/api/kline/list/open" {
					candleRequests++
					duration := market.TimeframeDuration(r.URL.Query().Get("interval")).Milliseconds()
					if duration <= 0 {
						return nil, fmt.Errorf("unexpected candle interval: %s", r.URL.RawQuery)
					}
					rows := make([][]float64, 100)
					last := time.Now().UnixMilli()/duration*duration - duration
					width := tc.mark * tc.atr / 100
					for i := range rows {
						start := last - int64(99-i)*duration
						closePrice := tc.mark + float64(i%2)*0.01
						if i == 99 {
							closePrice = tc.mark
						}
						rows[i] = []float64{float64(start), float64(start + duration - 1), closePrice, closePrice, closePrice + width/2, closePrice - width/2, 100, 100, 10}
					}
					payload = map[string]interface{}{"success": true, "data": rows}
				} else {
					// These optional OI/funding lookups also stay entirely in memory.
					return nil, fmt.Errorf("optional market data disabled in batch1 fixture: %s", r.URL.Path)
				}
				body, err := json.Marshal(payload)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
			})
			at, m := newBatch1RiskTrader(tc.side, tc.qty, tc.mark, store.RiskControlConfig{
				TrailingStopEnabled: true, BreakevenArmR: tc.armR, ProfitLockAtR: tc.lockR,
			})
			at.exchange = "binance"
			at.SetInitialStopLoss("XUSDT", tc.side, tc.initial)
			at.SetRecordedStopLoss("XUSDT", tc.side, tc.current)
			// The trim is already consumed; only stop management is under test.
			at.r1TrimDone["XUSDT_"+tc.side] = true
			at.processVolTargetAndTrailing()
			if candleRequests != 4 {
				t.Fatalf("cycle did not read the mocked candles: requests=%d", candleRequests)
			}
			if len(m.stopPrices) != len(tc.wantPrices) {
				t.Fatalf("stop submissions=%v, want %v", m.stopPrices, tc.wantPrices)
			}
			for i, want := range tc.wantPrices {
				if math.Abs(m.stopPrices[i]-want) > 1e-8 {
					t.Fatalf("stop submissions=%v, want %v", m.stopPrices, tc.wantPrices)
				}
			}
			if got := at.GetRecordedStopLoss("XUSDT", tc.side); math.Abs(got-tc.wantRecorded) > 1e-8 {
				t.Fatalf("recorded stop=%g, want %g", got, tc.wantRecorded)
			}
			if got := at.GetInitialStopLoss("XUSDT", tc.side); got != tc.initial {
				t.Fatalf("opening anchor changed: %g", got)
			}
		})
	}
}
