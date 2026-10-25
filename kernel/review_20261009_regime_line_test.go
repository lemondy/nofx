package kernel

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

func TestReview20261009RegimeLineAlignment(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	td := &market.TimeframeSeriesData{EMA50Values: []float64{1.417, 1.5}}
	for i := 0; i < 4; i++ {
		td.Klines = append(td.Klines, market.KlineBar{Time: now.Add(time.Duration(i-4) * time.Hour).UnixMilli()})
	}
	data := &market.Data{TimeframeData: map[string]*market.TimeframeSeriesData{"1h": td}}
	if got := RegimeLine(data, "1h", now); got != 1.5 {
		t.Fatalf("closed tail alignment: got %g", got)
	}
	td.Klines[3].Time = now.UnixMilli()
	if got := RegimeLine(data, "1h", now); got != 1.417 {
		t.Fatalf("forming tail must select previous aligned value: got %g", got)
	}
	td.EMA50Values = []float64{1.5}
	if got := RegimeLine(data, "1h", now); got != 0 {
		t.Fatalf("previous bar has no EMA50: got %g", got)
	}
	for _, tf := range []string{"15m", "invalid"} {
		if got := RegimeLine(data, tf, now); got != 0 {
			t.Fatalf("unavailable %s: got %g", tf, got)
		}
	}
	if RegimeLine(nil, "1h", now) != 0 || RegimeLine(&market.Data{}, "1h", now) != 0 {
		t.Fatal("missing data must return zero")
	}
}

func TestReview20261009RegimeLineBreached(t *testing.T) {
	for _, tc := range []struct {
		long     bool
		px, line float64
		want     bool
	}{{true, 1, 2, true}, {false, 2, 1, true}, {true, 2, 1, false}, {false, 1, 2, false}, {true, 1, 1, false}, {false, 1, 1, false}, {true, 0, 1, false}, {false, 1, 0, false}, {true, -1, 1, false}, {false, 1, -1, false}} {
		if got := RegimeLineBreached(tc.long, tc.px, tc.line); got != tc.want {
			t.Fatalf("%+v: got %v", tc, got)
		}
	}
}

func reviewRegimeData(now time.Time, long bool, live, line float64) *market.Data {
	data := &market.Data{Symbol: "XRPUSDT", CurrentPrice: live, TimeframeData: map[string]*market.TimeframeSeriesData{}}
	for _, tf := range []string{"15m", "1h", "4h"} {
		td := &market.TimeframeSeriesData{Timeframe: tf}
		for i := 0; i < 60; i++ {
			p := 1.70 - 0.007*float64(i)
			if long {
				p = 1.08 + 0.007*float64(i)
			}
			if tf == "15m" && i == 59 {
				p = 1.3829
			}
			td.Klines = append(td.Klines, market.KlineBar{Time: now.Add(time.Duration(i-60) * market.TimeframeDuration(tf)).UnixMilli(), Open: p, High: p + 0.002, Low: p - 0.002, Close: p, Volume: 1000})
			td.EMA50Values = append(td.EMA50Values, line)
		}
		data.TimeframeData[tf] = td
	}
	return data
}

func TestReview20261009RegimeLineHardGate(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                    string
		long                    bool
		live, line, anchor      float64
		broken, suppressed, off bool
	}{
		{"XRP replay", false, 1.3829, 1.417, 1.39141, false, false, false},
		{"short live broken", false, 1.43, 1.417, 1.44, true, false, false},
		{"short anchor broken", false, 1.3829, 1.417, 1.43, false, true, false},
		{"long valid", true, 1.3829, 1.35, 1.37, false, false, false},
		{"long live broken", true, 1.34, 1.35, 1.33, true, false, false},
		{"long anchor broken", true, 1.3829, 1.35, 1.34, false, true, false},
		{"short down trigger off", false, 1.43, 1.417, 1.44, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := reviewRegimeData(now, tc.long, tc.live, tc.line)
			if tc.off {
				data.TimeframeData["15m"].Klines[59].Close = 1.287
			}
			offset := math.Abs(tc.anchor/tc.live-1) * 100
			sig, err := ComputeSymbolSignals(data.Symbol, data, SignalOptions{Now: now, PrimaryTF: "15m", EntryTimingGate: true, LimitEntryEnabled: true, LimitEntryOffsetMode: "fixed", LimitEntryOffsetPct: offset})
			if err != nil {
				t.Fatal(err)
			}
			wantTrend := "rally"
			if tc.long {
				wantTrend = "pullback"
			}
			if tc.off {
				wantTrend = "down"
			}
			tf, trend := ExecutionMicroTrend(data, now)
			if tf != "15m" || trend != wantTrend || sig.ExecutionFilter.MicroTrend != trend {
				t.Fatalf("micro parity: %s/%s vs %+v, want %s", tf, trend, sig.ExecutionFilter, wantTrend)
			}
			g, anchor := sig.HardGate.Short, sig.LimitSellPrice
			if tc.long {
				g, anchor = sig.HardGate.Long, sig.LimitBuyPrice
			}
			has := func(code string) bool {
				for _, f := range g.Failed {
					if f == code {
						return true
					}
				}
				return false
			}
			if has("REGIME_LINE_BROKEN") != tc.broken {
				t.Fatalf("broken=%v: %+v", tc.broken, g)
			}
			if has("LIMIT_ANCHOR_SUPPRESSED") != tc.suppressed || g.LimitAllowed == tc.suppressed {
				t.Fatalf("suppressed=%v: %+v", tc.suppressed, g)
			}
			if tc.suppressed {
				if anchor != 0 || !strings.Contains(strings.Join(sig.Warnings, " "), "1h regime line (EMA50)") || g.EntryBasis != "live_price" || g.MarketException {
					t.Fatalf("suppression not reflected in anchor/warning/basis: %+v, %v", g, sig.Warnings)
				}
			} else if math.Abs(anchor-tc.anchor) > 1e-9 {
				t.Fatalf("anchor got %g, want %g", anchor, tc.anchor)
			}
			if g.Allowed != (!tc.broken && !tc.suppressed) {
				t.Fatalf("unexpected verdict: %+v", g)
			}
			var rendered map[string]interface{}
			if err := json.Unmarshal([]byte(RenderSignalJSON(sig)), &rendered); err != nil {
				t.Fatal(err)
			}
			rl := rendered["regime_line"].(map[string]interface{})
			if rl["tf"] != "1h" || rl["ema50"] != tc.line {
				t.Fatalf("line missing from prompt JSON: %v", rl)
			}
		})
	}
}

func TestReview20261009MicroDefinitionAndRoles(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	data := reviewRegimeData(now, false, 1.3829, 1.417)
	data.TimeframeData["5m"] = data.TimeframeData["15m"]
	if tf, _ := ExecutionMicroTrend(data, now); tf != "15m" {
		t.Fatalf("finer timeframe selected: %s", tf)
	}
	data.TimeframeData["30m"] = data.TimeframeData["15m"]
	delete(data.TimeframeData, "15m")
	if tf, _ := ExecutionMicroTrend(data, now); tf != "30m" {
		t.Fatalf("fallback: %s", tf)
	}
	delete(data.TimeframeData, "30m")
	if tf, _ := ExecutionMicroTrend(data, now); tf != "" {
		t.Fatalf("1h/5m must not trigger: %s", tf)
	}
	config := &store.StrategyConfig{}
	config.Indicators.Klines.PrimaryTimeframe = "5m"
	roles := ResolveRoleTimeframes(config)
	if roles.ExecutionTF != "5m" || roles.TrendTF != "1h" || ResolveRoleTimeframes(nil).TrendTF != "1h" {
		t.Fatalf("roles: %+v", roles)
	}
}

func TestReview20261009RetestAnchorAndDisabledGate(t *testing.T) {
	sig := &SymbolSignal{Price: 1.3829, LimitBuyPrice: 1.34, LimitSellPrice: 1.4, ExecutionFilter: &ExecutionFilter{MicroTF: "15m", MicroTrend: "pullback", LongAllowed: true}, RegimeLine: &RegimeLineSignal{TF: "1h", EMA50: 1.35}, LongPullback: &LongPullbackPlan{Active: true, Entry: 1.34}}
	g := computeHardEntryGate(sig, SignalOptions{EntryTimingGate: true, LimitEntryEnabled: true}).Long
	if sig.LimitBuyPrice != 0 || g.LimitAllowed || g.EntryBasis != "live_price" {
		t.Fatalf("retest anchor bypassed guard: %+v", g)
	}
	sig.LimitBuyPrice = sig.LongPullback.Entry
	g = computeHardEntryGate(sig, SignalOptions{EntryTimingGate: false, LimitEntryEnabled: true}).Long
	if !g.Allowed || sig.LimitBuyPrice != 1.34 {
		t.Fatalf("disabled gate changed anchor: %+v", g)
	}
	sig.RegimeLine = nil
	if strings.Contains(RenderSignalJSON(sig), "regime_line") {
		t.Fatal("unavailable line must be omitted")
	}
}

func TestReview20261009RegimeLineMarketException(t *testing.T) {
	for _, long := range []bool{true, false} {
		sig := &SymbolSignal{Price: 100, LimitBuyPrice: 98, LimitSellPrice: 102, ExecutionFilter: &ExecutionFilter{MicroTF: "15m", LongAllowed: long, ShortAllowed: !long}, Derivatives: &DerivSignal{FundingAnnualizedPct: fp(0)}}
		if long {
			sig.ExecutionFilter.MicroTrend = "pullback"
			sig.RegimeLine = &RegimeLineSignal{TF: "1h", EMA50: 99}
			sig.MarketRegime = &MarketRegime{Regime: "TREND_UP", ConfirmedBars: 2}
			sig.BBRide = &BBRide{Ride: true}
		} else {
			sig.ExecutionFilter.MicroTrend = "rally"
			sig.RegimeLine = &RegimeLineSignal{TF: "1h", EMA50: 101}
			sig.MarketRegime = &MarketRegime{Regime: "TREND_DOWN", ConfirmedBars: 2}
			sig.ShortRide = &BBShortRide{Ride: true}
		}
		gate := computeHardEntryGate(sig, SignalOptions{EntryTimingGate: true, LimitEntryEnabled: true})
		g := gate.Short
		if long {
			g = gate.Long
		}
		if !g.Allowed || !g.MarketException || g.LimitAllowed || g.EntryBasis != "live_price" {
			t.Fatalf("market exception lost after suppression: %+v", g)
		}
		if long {
			sig.Price = 98
		} else {
			sig.Price = 102
		}
		gate = computeHardEntryGate(sig, SignalOptions{EntryTimingGate: true})
		g = gate.Short
		if long {
			g = gate.Long
		}
		if g.Allowed || !hasCode(g, "REGIME_LINE_BROKEN") {
			t.Fatalf("market evidence bypassed live reversal: %+v", g)
		}
	}
}

func TestReview20261009RetestAnchorIntegration(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	data := reviewRegimeData(now, true, 102.8, 101)
	for _, tf := range []string{"15m", "4h"} {
		for i := range data.TimeframeData[tf].Klines {
			b := &data.TimeframeData[tf].Klines[i]
			b.Open *= 102.8 / 1.3829
			b.High *= 102.8 / 1.3829
			b.Low *= 102.8 / 1.3829
			b.Close *= 102.8 / 1.3829
		}
	}
	td := data.TimeframeData["1h"]
	for i := range td.Klines {
		p := 100.0
		if i >= 52 {
			p = 102.2
		}
		td.Klines[i].Open = p
		td.Klines[i].High = p + 0.2
		td.Klines[i].Low = p - 0.2
		td.Klines[i].Close = p
	}
	td.Klines[59].Volume = 2000
	sig, err := ComputeSymbolSignals(data.Symbol, data, SignalOptions{Now: now, PrimaryTF: "15m", OI1hPct: fp(0.5), LongPullbackEntry: true, EntryTimingGate: true, LimitEntryEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if sig.LongPullback == nil || !sig.LongPullback.Active || sig.LongPullback.Entry != 100.2 {
		t.Fatalf("fixture must produce retest override: breakout=%+v plan=%+v", sig.Breakout, sig.LongPullback)
	}
	if sig.ExecutionFilter.MicroTrend != "pullback" || sig.LimitBuyPrice != 0 || !hasCode(sig.HardGate.Long, "LIMIT_ANCHOR_SUPPRESSED") {
		t.Fatalf("retest bypassed final suppression: micro=%+v anchor=%g gate=%+v", sig.ExecutionFilter, sig.LimitBuyPrice, sig.HardGate.Long)
	}
}
