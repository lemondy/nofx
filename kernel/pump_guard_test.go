package kernel

import (
	"math"
	"strings"
	"testing"
	"time"

	"nofx/market"
	"nofx/store"
)

// Extended-pump long guard (user directive 2026-09-20 收紧): vertical pumps
// (FILUSDT/SAGAUSDT 2026-09-19, 4h +29%/+30%) carry an EMA-lagged "up" label
// while they round-trip — a long into the first pullback needs CONFIRMATION
// (15m back above its EMA20 AND a higher 15m swing low), else the long
// direction fails with EXTENDED_PUMP_UNCONFIRMED.
func TestPumpGuardBlocksUnconfirmedExtendedPump(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	// 4h: strong pump; the last five closed bars alone exceed the 20% guard.
	var k4h []market.Kline
	for i := 0; i < 30; i++ {
		p := 100 * math.Pow(1.05, float64(i))
		k4h = append(k4h, market.Kline{
			OpenTime: now.Add(time.Duration(i-30) * 4 * time.Hour).UnixMilli(),
			Open:     p * 0.999, High: p * 1.004, Low: p * 0.996, Close: p, Volume: 10,
		})
	}

	// 15m: pumped to a top, then a steady pullback — closes below the fast
	// EMA and each dip undercutting the prior swing low (NO confirmation).
	mkUnconfirmed := func() []market.Kline {
		var out []market.Kline
		for i := 0; i < 40; i++ {
			var p float64
			if i < 25 {
				p = 140 + float64(i)*0.5 // pump leg
			} else {
				p = 152 - float64(i-25)*0.8 // pullback leg: price sinking
			}
			out = append(out, market.Kline{
				OpenTime: now.Add(time.Duration(i-40) * 15 * time.Minute).UnixMilli(),
				Open:     p * 1.001, High: p * 1.002, Low: p * 0.997, Close: p, Volume: 10,
			})
		}
		return out
	}

	// 15m: the pump leg zigzags upward (oscillation pivot lows along the
	// way), then a SHALLOW dip that bottoms ABOVE the last zigzag low and
	// recovers — close back over the fast EMA with a higher swing low: the
	// confirmation shape.
	mkConfirmed := func() []market.Kline {
		var out []market.Kline
		// Pump leg: 5-bar groups [M, M+0.5, M-0.4, M+0.3, M+0.8], M +1.2 per
		// group — one true swing low per group (pivot low with shoulder 2),
		// each 1.2 above the previous.
		for g := 0; g < 5; g++ {
			m := 140 + 1.2*float64(g)
			for _, off := range [5]float64{0, 0.5, -0.4, 0.3, 0.8} {
				p := m + off
				out = append(out, market.Kline{
					OpenTime: now.Add(time.Duration(len(out)-40) * 15 * time.Minute).UnixMilli(),
					Open:     p * 0.999, High: p * 1.002, Low: p * 0.997, Close: p, Volume: 10,
				})
			}
		}
		// Shallow dip bottoming at 145.6 — ABOVE the last group swing low
		// (144.4): a higher low, only ~1% off the top — then recovery.
		dip := [15]float64{146.5, 146.0, 145.7, 145.6, 146.2, 147.0, 147.8, 148.6, 149.4, 150.2, 151.0, 151.8, 152.6, 153.4, 154.2}
		for _, p := range dip {
			out = append(out, market.Kline{
				OpenTime: now.Add(time.Duration(len(out)-40) * 15 * time.Minute).UnixMilli(),
				Open:     p * 0.999, High: p * 1.002, Low: p * 0.997, Close: p, Volume: 10,
			})
		}
		return out
	}

	run := func(k15 []market.Kline, guard float64) *SymbolSignal {
		sig, err := ComputeSymbolSignals("TESTUSDT", &market.Data{
			Symbol: "TESTUSDT", CurrentPrice: 149,
			TimeframeData: map[string]*market.TimeframeSeriesData{
				"15m": mkTF("15m", k15),
				"4h":  mkTF("4h", k4h),
			},
		}, SignalOptions{Now: now, PrimaryTF: "15m", CurrentPrice: 149, PumpGuard4hPct: guard})
		if err != nil {
			t.Fatalf("ComputeSymbolSignals: %v", err)
		}
		return sig
	}

	hasCode := func(g *DirectionGate, code string) bool {
		for _, c := range g.Failed {
			if strings.Contains(c, code) {
				return true
			}
		}
		return false
	}

	// 1. Extended + unconfirmed pullback → long blocked.
	sig := run(mkUnconfirmed(), 20)
	if sig.PumpGuard == nil || !sig.PumpGuard.Extended {
		t.Fatalf("pump_guard = %+v, want extended", sig.PumpGuard)
	}
	if sig.PumpGuard.Confirmed {
		t.Fatalf("unconfirmed pullback marked confirmed: %+v", sig.PumpGuard)
	}
	if !hasCode(sig.HardGate.Long, "EXTENDED_PUMP_UNCONFIRMED") {
		t.Fatalf("long gate missing EXTENDED_PUMP_UNCONFIRMED: %v (pump_guard=%+v)", sig.HardGate.Long.Failed, sig.PumpGuard)
	}
	if hasCode(sig.HardGate.Short, "EXTENDED_PUMP_UNCONFIRMED") {
		t.Fatal("guard must be long-side only")
	}

	// 2. Extended + confirmed pullback (higher low + fast EMA recovered) →
	// the guard stays silent.
	sigOK := run(mkConfirmed(), 20)
	if sigOK.PumpGuard == nil || !sigOK.PumpGuard.Extended || !sigOK.PumpGuard.Confirmed {
		t.Fatalf("confirmed pullback misjudged: %+v", sigOK.PumpGuard)
	}
	if hasCode(sigOK.HardGate.Long, "EXTENDED_PUMP_UNCONFIRMED") {
		t.Fatalf("confirmed long still blocked: %v", sigOK.HardGate.Long.Failed)
	}

	// 3. Disabled via negative config → no pump_guard evidence at all.
	if sig := run(mkUnconfirmed(), -1); sig.PumpGuard != nil {
		t.Fatalf("disabled guard must emit no block: %+v", sig.PumpGuard)
	}

	// 4. Dormant on a non-pumped coin: same unconfirmed 15m shape, flat 4h —
	// no block (guard only arms on extended pumps).
	var k4hFlat []market.Kline
	for i := 0; i < 30; i++ {
		p := 100 + float64(i%3)
		k4hFlat = append(k4hFlat, market.Kline{
			OpenTime: now.Add(time.Duration(i-30) * 4 * time.Hour).UnixMilli(),
			Open:     p, High: p * 1.001, Low: p * 0.999, Close: p, Volume: 10,
		})
	}
	sigFlat, err := ComputeSymbolSignals("TESTUSDT", &market.Data{
		Symbol: "TESTUSDT", CurrentPrice: 149,
		TimeframeData: map[string]*market.TimeframeSeriesData{
			"15m": mkTF("15m", mkUnconfirmed()),
			"4h":  mkTF("4h", k4hFlat),
		},
	}, SignalOptions{Now: now, PrimaryTF: "15m", CurrentPrice: 149, PumpGuard4hPct: 20})
	if err != nil {
		t.Fatalf("ComputeSymbolSignals flat: %v", err)
	}
	if sigFlat.PumpGuard == nil || sigFlat.PumpGuard.Extended || !sigFlat.PumpGuard.Confirmed {
		t.Fatalf("dormant guard wrong: %+v", sigFlat.PumpGuard)
	}
	if hasCode(sigFlat.HardGate.Long, "EXTENDED_PUMP_UNCONFIRMED") {
		t.Fatal("dormant guard must not block")
	}

	// 5. Resolver semantics: nil/0 = default 20, negative = off.
	if got := PumpGuard4h(nil); got != 20 {
		t.Fatalf("PumpGuard4h(nil) = %v, want 20", got)
	}
	if got := PumpGuard4h(&store.RiskControlConfig{}); got != 20 {
		t.Fatalf("PumpGuard4h(zero) = %v, want 20", got)
	}
	if got := PumpGuard4h(&store.RiskControlConfig{PumpGuard4hPct: -1}); got != 0 {
		t.Fatalf("PumpGuard4h(-1) = %v, want 0 (disabled)", got)
	}
	if got := PumpGuard4h(&store.RiskControlConfig{PumpGuard4hPct: 35}); got != 35 {
		t.Fatalf("PumpGuard4h(35) = %v, want 35", got)
	}
}

func TestPumpGuardUsesFiveClosed4hCandles(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	var bars []market.Kline
	for i := 0; i < 30; i++ {
		p := 100 + 2*float64(i)
		if i >= 25 {
			p = 148 // last five 4h candles are flat after the earlier pump
		}
		bars = append(bars, market.Kline{
			OpenTime: now.Add(time.Duration(i-30) * 4 * time.Hour).UnixMilli(),
			Open:     p, High: p * 1.001, Low: p * 0.999, Close: p, Volume: 10,
		})
	}
	sig := &SymbolSignal{Timeframes: map[string]*TFSignal{"4h": {ReturnPct: 25}}}
	data := &market.Data{TimeframeData: map[string]*market.TimeframeSeriesData{
		"4h": mkTF("4h", bars),
	}}
	guard := computePumpGuard(sig, data, 20, now)
	if guard.Extended || guard.Return4hPct != 0 {
		t.Fatalf("older 20-bar pump must not arm the 5-bar guard: %+v", guard)
	}
}
