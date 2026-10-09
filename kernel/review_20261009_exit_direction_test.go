package kernel

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

// 2026-10-09 ONUSDT incident: the exit rule evaluated ONLY the long-side
// semantics (break of structure low / RSI>80 / trend down + bearish close)
// and rendered the flag for ANY held position. A SHORT sitting in a working
// 15m downtrend read "exit_rule_triggered": true as a program exit
// directive and closed a profitable short into a realized loss — trend-down
// + bearish is precisely the short WORKING. The rule must evaluate the HELD
// side's mirror only.

// buildMixedTrendTF builds a primary-TF series whose RSI stays mid-range:
// flat alternating bars set the baseline, the final 3 bars drift ±1% to
// establish the trend and the last closed candle's form. buildTrendTF's
// one-directional steps saturate RSI past BOTH sides' thresholds (>80/<20),
// which makes both sides' rules fire — useless for a direction truth table.
func buildMixedTrendTF(tf string, now time.Time, bars int, base float64, driftUp bool) *market.TimeframeSeriesData {
	dur := marketTFDuration(tf)
	data := &market.TimeframeSeriesData{Timeframe: tf}
	p := base
	for i := 0; i < bars; i++ {
		step := 0.005
		if i%2 == 1 {
			step = -0.005
		}
		if remaining := bars - 1 - i; remaining < 3 {
			if driftUp {
				step = 0.01
			} else {
				step = -0.01
			}
		}
		open := p
		p *= 1 + step
		barStart := now.Add(time.Duration(i-bars) * dur) // last bar ends at now → closed
		data.Klines = append(data.Klines, market.KlineBar{
			Time: barStart.UnixMilli(), Open: open,
			High: math.Max(open, p) * 1.001, Low: math.Min(open, p) * 0.999,
			Close: p, Volume: 1000,
		})
	}
	return data
}

func lastClosedClose(td *market.TimeframeSeriesData) float64 {
	return td.Klines[len(td.Klines)-1].Close
}

func TestExitRuleDirectionAware(t *testing.T) {
	now := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	newData := func(up bool) *market.Data {
		td := buildMixedTrendTF("15m", now, 60, 100, up)
		return &market.Data{
			Symbol: "TESTUSDT", CurrentPrice: lastClosedClose(td),
			TimeframeData: map[string]*market.TimeframeSeriesData{"15m": td},
		}
	}
	downData, upData := newData(false), newData(true)

	fixture := func(data *market.Data, wantTrend, wantCandle string) {
		t.Helper()
		sig, err := ComputeSymbolSignals(data.Symbol, data, SignalOptions{Now: now, PrimaryTF: "15m"})
		if err != nil {
			t.Fatalf("fixture compute: %v", err)
		}
		prim := sig.Timeframes["15m"]
		if prim.Trend != wantTrend || prim.LastClosedCandle != wantCandle {
			t.Fatalf("fixture: want %s+%s, got trend=%q candle=%q", wantTrend, wantCandle, prim.Trend, prim.LastClosedCandle)
		}
		if prim.RSI14 == nil || *prim.RSI14 <= 20 || *prim.RSI14 >= 80 {
			t.Fatalf("fixture: RSI must stay mid-range so the truth table isolates the trend rule, got %v", prim.RSI14)
		}
	}
	fixture(downData, "down", "bearish")
	fixture(upData, "up", "bullish")

	for _, tc := range []struct {
		side string
		up   bool
		want bool
	}{
		{"long", false, true},   // downtrend breakdown invalidates a LONG
		{"short", false, false}, // the short is WORKING — the ONUSDT misread
		{"", false, false},      // unknown side (not held) → false
		{"short", true, true},   // mirror: uptrend rally invalidates a SHORT
		{"long", true, false},
		{"", true, false},
	} {
		data := newData(tc.up)
		sig, err := ComputeSymbolSignals("TESTUSDT", data, SignalOptions{Now: now, PrimaryTF: "15m", PositionSide: tc.side})
		if err != nil {
			t.Fatalf("compute side=%q: %v", tc.side, err)
		}
		if sig.ExitTriggered != tc.want {
			t.Fatalf("side=%q up=%v: ExitTriggered=%v, want %v", tc.side, tc.up, sig.ExitTriggered, tc.want)
		}
	}
}

// regLineExitData builds 1h (trend TF) klines whose last CLOSED close is
// lastClose with a flat EMA50 = line, plus a flat 15m primary for the signal
// stack — ONUSDT price scale, the incident's exact numbers.
func regLineExitData(now time.Time, lastClose, live, line float64) *market.Data {
	td := &market.TimeframeSeriesData{Timeframe: "1h"}
	for i := 0; i < 60; i++ {
		p := lastClose * 0.999
		if i == 59 {
			p = lastClose
		}
		td.Klines = append(td.Klines, market.KlineBar{
			Time: now.Add(time.Duration(i-60) * time.Hour).UnixMilli(),
			Open: p * 1.0005, High: p * 1.001, Low: p * 0.999, Close: p, Volume: 1000,
		})
		td.EMA50Values = append(td.EMA50Values, line)
	}
	td15 := &market.TimeframeSeriesData{Timeframe: "15m"}
	for i := 0; i < 60; i++ {
		td15.Klines = append(td15.Klines, market.KlineBar{
			Time: now.Add(time.Duration(i-60) * market.TimeframeDuration("15m")).UnixMilli(),
			Open: lastClose, High: lastClose * 1.001, Low: lastClose * 0.999,
			Close: lastClose, Volume: 1000,
		})
	}
	return &market.Data{
		Symbol: "ONUSDT", CurrentPrice: live,
		TimeframeData: map[string]*market.TimeframeSeriesData{"1h": td, "15m": td15},
	}
}

// The regime_line block must carry the precomputed comparisons so the model
// never re-derives "above the line" from raw decimals — 0.1107 vs 0.110776
// was read as "升破" and a short was closed on it.
func TestRegimeLinePrecomputedComparisons(t *testing.T) {
	now := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	const line = 0.110776
	for _, tc := range []struct {
		name                string
		lastClose, live     float64
		wantClose, wantLive bool
	}{
		{"ONUSDT replay: both below", 0.1107, 0.1097, false, false},
		{"live pokes above, close below", 0.1107, 0.1115, false, true},
		{"close above, live falls back", 0.1109, 0.1100, true, false},
		{"both above", 0.1109, 0.1115, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := regLineExitData(now, tc.lastClose, tc.live, line)
			sig, err := ComputeSymbolSignals(data.Symbol, data, SignalOptions{Now: now, PrimaryTF: "15m"})
			if err != nil {
				t.Fatal(err)
			}
			rl := sig.RegimeLine
			if rl == nil || rl.EMA50 != line {
				t.Fatalf("regime_line missing or wrong line: %+v", rl)
			}
			if rl.CloseAbove != tc.wantClose || rl.LiveAbove != tc.wantLive {
				t.Fatalf("close_above=%v live_above=%v, want %v/%v", rl.CloseAbove, rl.LiveAbove, tc.wantClose, tc.wantLive)
			}
		})
	}
	// The booleans must survive into the rendered prompt JSON.
	data := regLineExitData(now, 0.1107, 0.1097, line)
	sig, err := ComputeSymbolSignals(data.Symbol, data, SignalOptions{Now: now, PrimaryTF: "15m"})
	if err != nil {
		t.Fatal(err)
	}
	var rendered map[string]interface{}
	if err := json.Unmarshal([]byte(RenderSignalJSON(sig)), &rendered); err != nil {
		t.Fatal(err)
	}
	rl, ok := rendered["regime_line"].(map[string]interface{})
	if !ok {
		t.Fatalf("regime_line missing from prompt JSON: %v", rendered)
	}
	if _, ok := rl["last_close_above"]; !ok {
		t.Fatalf("last_close_above missing from prompt JSON: %v", rl)
	}
	if _, ok := rl["live_price_above"]; !ok {
		t.Fatalf("live_price_above missing from prompt JSON: %v", rl)
	}
}

// The prompt builder must thread the HELD position's side into the signal —
// the flag the model sees has to reflect the position it is actually in.
func TestComputeCoinSignalThreadsPositionSide(t *testing.T) {
	now := time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)
	cfg := store.GetDefaultStrategyConfig("zh")
	e := NewStrategyEngine(&cfg)
	mkCtx := func(side string) *Context {
		ctx := &Context{
			MarketDataMap: map[string]*market.Data{},
			Account:       AccountInfo{TotalEquity: 200},
		}
		ctx.CandidateCoins = append(ctx.CandidateCoins, CandidateCoin{Symbol: "ONUSDT"})
		td := buildMixedTrendTF("15m", now, 60, 100, false)
		data := withVendor(&market.Data{
			Symbol: "ONUSDT", CurrentPrice: lastClosedClose(td),
			TimeframeData: map[string]*market.TimeframeSeriesData{"15m": td},
		}, 0.05)
		ctx.MarketDataMap["ONUSDT"] = data
		if side != "" {
			ctx.Positions = []PositionInfo{{Symbol: "ONUSDT", Side: side, EntryPrice: lastClosedClose(td), Quantity: 1}}
		}
		return ctx
	}

	// Short holder on a working 15m downtrend: the flag must stay false.
	// This exact shape is what the model cited when it closed ONUSDT 10-09.
	if sig := e.computeCoinSignal(mkCtx("short").MarketDataMap["ONUSDT"], nil, mkCtx("short"), &CandidateCoin{Symbol: "ONUSDT"}); sig == nil || sig.ExitTriggered {
		t.Fatalf("short on a working downtrend must NOT see exit_rule_triggered (sig=%v)", sig != nil)
	}
	// Same data, long holder: the long-side invalidation fires.
	if sig := e.computeCoinSignal(mkCtx("long").MarketDataMap["ONUSDT"], nil, mkCtx("long"), &CandidateCoin{Symbol: "ONUSDT"}); sig == nil || !sig.ExitTriggered {
		t.Fatalf("long on a downtrend breakdown must see exit_rule_triggered (sig=%v)", sig != nil)
	}
	// Not held: phantom suppression (empty side → false).
	if sig := e.computeCoinSignal(mkCtx("").MarketDataMap["ONUSDT"], nil, mkCtx(""), &CandidateCoin{Symbol: "ONUSDT"}); sig == nil || sig.ExitTriggered {
		t.Fatalf("not-held symbol must not see exit_rule_triggered (sig=%v)", sig != nil)
	}
}
