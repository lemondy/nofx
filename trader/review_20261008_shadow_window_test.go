package trader

import (
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

// review 2026-10-08 D/E: synthetic bars keep fill and window checks offline.
func TestReview20261008ShadowWindow(t *testing.T) {
	base := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		basis       string
		short       bool
		aligned     bool
		startOffset time.Duration
		horizon     time.Duration
		fillWindow  time.Duration
		now         time.Duration
		bars        func([]market.KlineBar) []market.KlineBar
		outcome     string
		exit        float64
	}{
		{
			name: "containing bar SL ignored", basis: "live_price", outcome: "timeout", exit: 102,
			bars: func(b []market.KlineBar) []market.KlineBar { b[0].Low = 90; return b },
		},
		{
			name: "hour 9 TP ignored and horizon close used", outcome: "timeout", exit: 103,
			bars: func(b []market.KlineBar) []market.KlineBar {
				b[31].Close = 103
				b[36].High, b[36].Close = 120, 115
				return b
			},
		},
		{
			name: "forming bar after window ignored", now: 8*time.Hour + 10*time.Minute, outcome: "timeout", exit: 102,
			bars: func(b []market.KlineBar) []market.KlineBar { b[32].High = 120; return b },
		},
		{
			name: "forming last needed bar cannot complete window", now: 7*time.Hour + 50*time.Minute, outcome: "no_data",
			bars: func(b []market.KlineBar) []market.KlineBar { b[31].High = 120; return b },
		},
		{
			name: "data starts well after block", outcome: "no_data",
			bars: func(b []market.KlineBar) []market.KlineBar { return b[4:] },
		},
		{
			name: "last needed bar missing", outcome: "no_data",
			bars: func(b []market.KlineBar) []market.KlineBar { return append(b[:31], b[32:]...) },
		},
		{
			name: "early TP does not excuse incomplete coverage", outcome: "no_data",
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].High = 115; return b[:31] },
		},
		{
			name: "interior coverage gap", outcome: "no_data",
			bars: func(b []market.KlineBar) []market.KlineBar { return append(b[:4], b[5:]...) },
		},
		{
			name: "limit never fills despite TP", basis: "limit_anchor", outcome: "unfilled",
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].High = 115; return b },
		},
		{
			name: "limit touch after lifetime ignored", basis: "limit_anchor", outcome: "unfilled",
			bars: func(b []market.KlineBar) []market.KlineBar { b[4].Low, b[5].High = 99, 115; return b },
		},
		{
			name: "fill window open boundary excluded", basis: "limit_anchor", fillWindow: 25 * time.Minute, outcome: "unfilled",
			bars: func(b []market.KlineBar) []market.KlineBar { b[2].Low = 99; return b },
		},
		{
			name: "limit fills then TP later", basis: "limit_anchor", outcome: "tp_first", exit: 110,
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].Low, b[2].High = 100, 110; return b },
		},
		{
			name: "limit fill bar SL conservative", basis: "limit_anchor", outcome: "sl_first", exit: 95,
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].Low, b[1].High = 95, 110; return b },
		},
		{
			name: "limit fill bar TP alone ignored", basis: "limit_anchor", outcome: "timeout", exit: 102,
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].Low, b[1].High = 100, 110; return b },
		},
		{
			name: "live TP on first bar", basis: "live_price", outcome: "tp_first", exit: 110,
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].High = 110; return b },
		},
		{
			name: "legacy SL on first bar", outcome: "sl_first", exit: 95,
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].Low = 95; return b },
		},
		{
			name: "live both touch chooses SL", basis: "live_price", outcome: "sl_first", exit: 95,
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].Low, b[1].High = 95, 110; return b },
		},
		{
			name: "short limit fills then TP later", basis: "limit_anchor", short: true, outcome: "tp_first", exit: 90,
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].High, b[2].Low = 100, 90; return b },
		},
		{
			name: "short limit fill bar SL", basis: "limit_anchor", short: true, outcome: "sl_first", exit: 105,
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].High, b[1].Low = 105, 90; return b },
		},
		{
			name: "short fill bar TP alone ignored", basis: "limit_anchor", short: true, outcome: "timeout", exit: 98,
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].High, b[1].Low = 100, 90; return b },
		},
		{
			name: "short never fills despite TP", basis: "limit_anchor", short: true, outcome: "unfilled",
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].Low = 90; return b },
		},
		{
			name: "aligned start includes first bar and exact horizon close", aligned: true, outcome: "tp_first", exit: 110,
			bars: func(b []market.KlineBar) []market.KlineBar { b[31].High = 110; return b },
		},
		{
			name: "aligned start missing first bar", aligned: true, outcome: "no_data",
			bars: func(b []market.KlineBar) []market.KlineBar { return b[1:] },
		},
		{
			name: "48h has separate horizon close", horizon: 48 * time.Hour, outcome: "timeout", exit: 103,
			bars: func(b []market.KlineBar) []market.KlineBar { b[191].Close, b[196].High = 103, 115; return b },
		},
		{
			name: "hh05 limit fills at hh15 then TP at hh45", basis: "limit_anchor", outcome: "tp_first", exit: 110,
			bars: func(b []market.KlineBar) []market.KlineBar { b[1].Low, b[3].High = 100, 110; return b },
		},
		{
			name: "hh05 no entry before hh35 despite TP at hh45", basis: "limit_anchor", outcome: "unfilled",
			bars: func(b []market.KlineBar) []market.KlineBar { b[3].Low, b[3].High = 100, 110; return b },
		},
		{
			name: "hh40 limit fills at hh45", basis: "limit_anchor", startOffset: 40 * time.Minute, outcome: "timeout", exit: 102,
			bars: func(b []market.KlineBar) []market.KlineBar { b[3].Low = 100; return b },
		},
		{
			name: "hh05 second candidate at hh30 fills then TP at hh45", basis: "limit_anchor", outcome: "tp_first", exit: 110,
			bars: func(b []market.KlineBar) []market.KlineBar { b[2].Low, b[3].High = 100, 110; return b },
		},
		{name: "empty klines", outcome: "no_data", bars: func([]market.KlineBar) []market.KlineBar { return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := &store.GateShadowBlock{
				CreatedAt: base.Add(5 * time.Minute), Direction: "long", EntryBasis: tc.basis,
				EntryPrice: 100, StopPrice: 95, TakeProfit: 110,
			}
			if tc.startOffset != 0 {
				row.CreatedAt = base.Add(tc.startOffset)
			}
			if tc.aligned {
				row.CreatedAt = base
			}
			if tc.short {
				row.Direction, row.StopPrice, row.TakeProfit = "short", 105, 90
			}
			horizon, fillWindow, now := tc.horizon, tc.fillWindow, tc.now
			if horizon == 0 {
				horizon = 8 * time.Hour
			}
			if fillWindow == 0 {
				fillWindow = 30 * time.Minute
			}
			if now == 0 {
				now = horizon + 4*time.Hour
			}
			bars := make([]market.KlineBar, int(horizon/(15*time.Minute))+6)
			for i := range bars {
				bars[i] = market.KlineBar{Time: base.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), High: 104, Low: 101, Close: 102}
				if tc.short {
					bars[i].High, bars[i].Low, bars[i].Close = 99, 96, 98
				}
			}
			if tc.bars != nil {
				bars = tc.bars(bars)
			}
			outcome, exit := resolveShadowOutcome(row, bars, horizon, fillWindow, base.Add(now))
			if outcome != tc.outcome || exit != tc.exit {
				t.Fatalf("resolveShadowOutcome = (%q, %v), want (%q, %v)", outcome, exit, tc.outcome, tc.exit)
			}
		})
	}
}

func TestReview20261008ShadowKlineCount(t *testing.T) {
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		age  time.Duration
		want int
	}{
		{0, 2},
		{-time.Hour, 2},
		{15 * time.Minute, 3},
		{15*time.Minute + time.Second, 4},
		{8 * time.Hour, 34},
		{8*time.Hour + time.Minute, 35},
		{48 * time.Hour, 194},
		{72 * time.Hour, 290},
		{1497 * 15 * time.Minute, 1499},
		{1498 * 15 * time.Minute, 1500},
		{375 * time.Hour, 1500},
		{2000 * time.Hour, 1500},
	} {
		if got := shadowKlineCount(start, start.Add(tc.age)); got != tc.want {
			t.Errorf("age %s: count = %d, want %d", tc.age, got, tc.want)
		}
	}
}

func TestReview20261008ShadowFillWindowConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		strategy   *store.StrategyConfig
		interval   time.Duration
		wantCycles int
		wantWindow time.Duration
	}{
		{"nil strategy", nil, 5 * time.Minute, 3, 30 * time.Minute},
		{"unset cycles", &store.StrategyConfig{}, 20 * time.Minute, 3, time.Hour},
		{"configured cycles", &store.StrategyConfig{RiskControl: store.RiskControlConfig{LimitEntryMaxCycles: 4}}, 20 * time.Minute, 4, 80 * time.Minute},
		{"invalid cycles", &store.StrategyConfig{RiskControl: store.RiskControlConfig{LimitEntryMaxCycles: -1}}, 20 * time.Minute, 3, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := &AutoTrader{config: AutoTraderConfig{StrategyConfig: tc.strategy, ScanInterval: tc.interval}}
			cycles := at.limitEntryMaxCycles()
			window := limitEntryLifetime(cycles, at.config.ScanInterval)
			if cycles != tc.wantCycles || window != tc.wantWindow {
				t.Fatalf("cycles/window = %d/%s, want %d/%s", cycles, window, tc.wantCycles, tc.wantWindow)
			}
		})
	}
}
