package trader

import (
	"errors"
	"math"
	"nofx/kernel"
	"nofx/market"
	"nofx/store"
	"strings"
	"testing"
	"time"
)

func TestThinOISizingSharedChain(t *testing.T) {
	market.SetEquityClassificationForTesting(map[string]bool{"AAPLUSDT": true}, map[string]bool{"AAPLUSDT": true})
	defer market.SetEquityClassificationForTesting(nil, nil)
	at := riskTestTrader(store.RiskControlConfig{RiskPerTradePct: 1.5})
	cases := []struct {
		symbol string
		oi     float64
		ok     bool
		factor float64
	}{
		{"XUSDT", 15, true, .3}, {"XUSDT", 37.5, true, .5}, {"XUSDT", 75, true, 1}, {"XUSDT", 100, true, 1}, {"XUSDT", 0, true, .3}, {"XUSDT", 0, false, 1},
		{"BTCUSDT", 1, true, 1}, {"ETHUSDT", 1, true, 1}, {"AAPLUSDT", 1, true, 1}, {"ETHFIUSDT", 1, true, .3}, {"XUSDT", math.NaN(), true, 1},
	}
	for _, tc := range cases {
		for _, action := range []string{"open_long", "open_short", "open_long_limit", "open_short_limit"} {
			at.oiValueFn = func(string) (float64, bool) { return tc.oi, tc.ok }
			d := &kernel.Decision{Symbol: tc.symbol, Action: action, StopLoss: 95}
			// Risk limit first, discount second: it must never increase approved risk.
			want := 1000 * 1.5 / 5.2 * tc.factor
			if got := at.clampSizeToRisk(d, 1000, 1000, 100); math.Abs(got-want) > 1e-8 {
				t.Fatalf("%s %s OI %g got %g want %g", tc.symbol, action, tc.oi, got, want)
			}
		}
	}
	at.oiValueFn = func(string) (float64, bool) { return 1, true }
	at.config.StrategyConfig.RiskControl.ThinOIFullSizeMillions = -1
	if got := at.discountThinOISize("XUSDT", 100); got != 100 {
		t.Fatal("disabled discount", got)
	}
	at.config.StrategyConfig.RiskControl.ThinOIFullSizeMillions = 100
	at.config.StrategyConfig.RiskControl.ThinOIMinSizeFactor = -1
	if got := at.discountThinOISize("XUSDT", 100); got != 100 {
		t.Fatal("disabled factor", got)
	}
	at.config.StrategyConfig.RiskControl.ThinOIMinSizeFactor = .3
	if err := at.enforceMinPositionSize(at.discountThinOISize("XUSDT", 20)); err == nil {
		t.Fatal("discounted dust must reject, never re-inflate")
	}
}

type thinBookMock struct {
	audit05Mock
	bids, asks [][]float64
	err        error
}

func (m *thinBookMock) GetOrderBook(string, int) ([][]float64, [][]float64, error) {
	return m.bids, m.asks, m.err
}
func TestThinSpreadUnavailablePolicy(t *testing.T) {
	market.SetEquityClassificationForTesting(map[string]bool{"AAPLUSDT": true}, map[string]bool{"AAPLUSDT": true})
	defer market.SetEquityClassificationForTesting(nil, nil)
	for _, mode := range []string{"no interface", "error", "empty", "malformed", "zero"} {
		for _, tc := range []struct {
			symbol    string
			oi        float64
			ok, block bool
		}{
			{"XUSDT", 20, true, true}, {"XUSDT", 100, true, false}, {"XUSDT", 75, true, false}, {"XUSDT", 0, false, true}, {"BTCUSDT", 0, false, false}, {"ETHUSDT", 0, false, false}, {"ETHFIUSDT", 0, false, true}, {"AAPLUSDT", 0, false, false},
		} {
			at := riskTestTrader(store.RiskControlConfig{})
			at.oiValueFn = func(string) (float64, bool) { return tc.oi, tc.ok }
			m := &thinBookMock{}
			switch mode {
			case "error":
				m.err = errors.New("offline")
			case "malformed":
				m.bids = [][]float64{{}}
				m.asks = [][]float64{{100}}
			case "zero":
				m.bids = [][]float64{{100}}
				m.asks = [][]float64{{100}}
			}
			at.trader = m
			if mode == "no interface" {
				at.trader = &audit05Mock{}
			}
			got, why := at.spreadBlocksOpen(tc.symbol)
			if got != tc.block || (got && !strings.Contains(why, "fail-closed")) {
				t.Fatalf("%s %s got %v reason %s", mode, tc.symbol, got, why)
			}
			off := false
			at.config.StrategyConfig.RiskControl.SpreadGateFailClosedThin = &off
			if blocked, _ := at.spreadBlocksOpen(tc.symbol); blocked {
				t.Fatal("explicit false must restore fail-open")
			}
		}
	}
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = &thinBookMock{bids: [][]float64{{99}}, asks: [][]float64{{101}}}
	at.oiValueFn = func(string) (float64, bool) { return 100, true }
	if blocked, _ := at.spreadBlocksOpen("XUSDT"); !blocked {
		t.Fatal("liquid wide book must still block")
	}
	at.config.StrategyConfig.RiskControl.MaxSpreadPct = -1
	if blocked, _ := at.spreadBlocksOpen("XUSDT"); blocked {
		t.Fatal("disabled spread gate")
	}
}
func TestVolatileCodesAbsoluteForEveryOpen(t *testing.T) {
	for _, code := range []string{"THIN_VOLUME_1_LT_5", "VOL_SPIKE_5M_9_GT_8", "VOL_SPIKE_15M_7_GT_6", "VOL_SPIKE_1H_6_GT_5", "OI_SURGE_15M_9_GT_8", "OI_SURGE_1H_16_GT_15", "OI_SURGE_31_GT_30", "VOL_HOLDOFF_30m"} {
		at := riskTestTrader(store.RiskControlConfig{})
		at.cycleGateStates = map[string]*kernel.GateState{"XUSDT": {LongFailed: []string{code}, ShortFailed: []string{code}, LongMarketException: true, ShortMarketException: true}}
		ctx := &kernel.Context{Account: kernel.AccountInfo{TotalEquity: 1000}, CandidateCoins: []kernel.CandidateCoin{{Symbol: "XUSDT"}}}
		for _, action := range []string{"open_long", "open_short", "open_long_limit", "open_short_limit"} {
			if got := at.applyHardRiskGates([]kernel.Decision{{Symbol: "XUSDT", Action: action, Confidence: 99}}, ctx); len(got) != 0 {
				t.Fatalf("%s bypassed by %s", code, action)
			}
		}
		for _, side := range []string{"long", "short"} {
			if err := at.entryExecutionBlocked("XUSDT", side); err == nil {
				t.Fatalf("%s final entry authorization bypass", code)
			}
		}
		if !absoluteBanCode(code) {
			t.Fatal("pending recheck must use absolute ban", code)
		}
	}
}

func TestThinSpreadKeepsExistingBstockWideBookGate(t *testing.T) {
	market.SetEquityClassificationForTesting(map[string]bool{"AAPLUSDT": true}, map[string]bool{"AAPLUSDT": true})
	defer market.SetEquityClassificationForTesting(nil, nil)
	at := riskTestTrader(store.RiskControlConfig{})
	at.trader = &thinBookMock{bids: [][]float64{{99}}, asks: [][]float64{{101}}}
	if blocked, _ := at.spreadBlocksOpen("AAPLUSDT"); !blocked {
		t.Fatal("bstock exemption must not relax the existing measured wide-spread gate")
	}
}
func TestPendingFreshVolatilityBan(t *testing.T) {
	at := riskTestTrader(store.RiskControlConfig{})
	at.config.StrategyConfig = fix7Config()
	at.btcTrendClosesFn = func(int) []float64 { return nil }
	data := fix7StrategyData(at.recheckConfiguredTimeframes("VOLPENDINGUSDT"), true)
	data.SampledAt = time.Now()
	// A short-window event must cancel resting orders even while hourly
	// bars are quiet. Include the forming 5m tail, as the live fetch does.
	bars := make([]market.KlineBar, 200)
	for i := range bars {
		bars[i] = market.KlineBar{Time: data.SampledAt.Truncate(5 * time.Minute).Add(time.Duration(i-199) * 5 * time.Minute).UnixMilli(), Open: 100, High: 101, Low: 99, Close: 100, Volume: 100}
	}
	bars[199].High = 119
	data.TimeframeData["5m"] = &market.TimeframeSeriesData{Timeframe: "5m", Klines: bars}
	at.recheckDataFn = func(string) (*market.Data, error) { return data, nil }
	for _, side := range []string{"long", "short"} {
		if why := at.pendingDirectionBlocked(&pendingEntry{Symbol: "VOLPENDINGUSDT", Side: side, Price: 100}); !strings.Contains(why, "VOL_SPIKE_5M_") {
			t.Fatalf("pending %s failed to cancel: %s", side, why)
		}
	}
	bars[199].High = 101
	for _, side := range []string{"long", "short"} {
		if why := at.pendingDirectionBlocked(&pendingEntry{Symbol: "VOLPENDINGUSDT", Side: side, Price: 100}); !strings.Contains(why, "VOL_HOLDOFF_30m") {
			t.Fatalf("pending %s bypassed observed holdoff: %s", side, why)
		}
	}

}
