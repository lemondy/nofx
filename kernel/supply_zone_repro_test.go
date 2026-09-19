package kernel

import (
	"math"
	"testing"
	"time"

	"nofx/market"
)

// Reproduction of the 2026-09-19 BTCUSDT loop geometry: with real 15m data
// present (the kernel snapshot shape), a quiet-15m anchor 0.22% under a
// swing high must be suppressed or blessed consistently with what the
// execution gate computes from the same yardstick.
func TestSuppressionFiresOnLiveLoopNumbers(t *testing.T) {
	now := time.Date(2026, 9, 19, 13, 49, 0, 0, time.UTC)

	// 15m bars: a pivot high at 81232.4 three bars from the end, live price
	// just below it — the exact geometry of the live prompt (res[0]=81232.4,
	// CurrentPrice=81232.3, anchor=81056.6 = live×(1−0.216%)).
	mk15m := func() *market.TimeframeSeriesData {
		tfd := &market.TimeframeSeriesData{}
		base := 79800.0
		for i := 0; i < 100; i++ {
			p := base * (1 + 0.0022*float64(i))
			p *= 1 + 0.006*math.Sin(float64(i)*1.7)
			ts := now.Add(time.Duration(i-100) * 15 * time.Minute).UnixMilli()
			hi, lo := p*1.006, p*0.995
			cl := p
			switch i {
			case 95:
				p = 81200
				hi, cl = 81232.4, 81190
			case 96, 97, 98, 99:
				p = 81150
				hi, lo, cl = 81210, 81080, 81120
			}
			tfd.Klines = append(tfd.Klines, market.KlineBar{
				Time: ts, Open: p * 0.999, High: hi, Low: lo, Close: cl, Volume: 1000,
			})
		}
		return tfd
	}

	// 1h bars: pivots above the anchor in the 81270/81500 region.
	mk1h := func() *market.TimeframeSeriesData {
		tfd := &market.TimeframeSeriesData{}
		for i := 0; i < 100; i++ {
			p := 78500 + float64(i)*27.0
			p *= 1 + 0.004*math.Sin(float64(i)*1.3)
			ts := now.Add(time.Duration(i-100) * time.Hour).UnixMilli()
			tfd.Klines = append(tfd.Klines, market.KlineBar{
				Time: ts, Open: p * 0.999, High: p * 1.005, Low: p * 0.995, Close: p, Volume: 1000,
			})
		}
		return tfd
	}

	data := withVendor(&market.Data{
		Symbol:       "BTCUSDT",
		CurrentPrice: 81232.3,
		TimeframeData: map[string]*market.TimeframeSeriesData{
			"15m": mk15m(), "1h": mk1h(),
		},
		FundingRate: 0.0001, FundingRateOK: true, FundingSettleHours: 8,
		FundingHistory: []float64{0.0001, 0.0001, 0.0001, 0.0001},
	}, 0.05)

	sig, err := ComputeSymbolSignals("BTCUSDT", data, SignalOptions{
		Now:                    now,
		PrimaryTF:              "15m",
		CurrentPrice:           81232.3,
		SupplyZonePct:          0.5,
		MinRR:                  1.5,
		MaxVendorDivergencePct: 2.0,
	})
	if err != nil {
		t.Fatalf("ComputeSymbolSignals: %v", err)
	}

	breath := AnchorBreathingPct(sig.ExecutionATRPct(), AnchorOffsetConfig{}, 0.5)
	t.Logf("limit_buy=%.1f execATR=%.3f%% breath=%.2f%%", sig.LimitBuyPrice, sig.ExecutionATRPct(), breath)
	for _, tf := range []string{"15m", "1h"} {
		tfSig := sig.Timeframes[tf]
		if tfSig == nil {
			t.Fatalf("%s signal missing", tf)
		}
		t.Logf("%s atr_pct=%.3f resistance=%v", tf, tfSig.ATRPct, tfSig.Resistance)
	}

	// Whatever the arrays hold, if ANY resistance within the breathing band
	// of the anchor exists, the suppression must zero the buy anchor.
	for _, tf := range []string{"15m", "1h"} {
		for _, r := range sig.Timeframes[tf].Resistance {
			if d := (r - sig.LimitBuyPrice) / sig.LimitBuyPrice * 100; sig.LimitBuyPrice > 0 && d > 0 && d < breath {
				t.Fatalf("UNSUPPRESSED: %s resistance %.1f sits %.3f%% above anchor %.1f (< %.3f%%) — prompt-side suppression did not fire (execution gate rejects this every cycle)",
					tf, r, d, sig.LimitBuyPrice, breath)
			}
		}
	}
}

// mkTF builds a market-side timeframe series the way
// market.calculateTimeframeSeries would (kernel tests cannot reach the
// unexported builder).
func mkTF(tf string, bars []market.Kline) *market.TimeframeSeriesData {
	out := &market.TimeframeSeriesData{Timeframe: tf}
	for _, k := range bars {
		out.Klines = append(out.Klines, market.KlineBar{
			Time: k.OpenTime, Open: k.Open, High: k.High, Low: k.Low, Close: k.Close, Volume: k.Volume,
		})
	}
	return out
}

// End-to-end parity regression for the 2026-09-19 BTCUSDT loop. The
// execution dataset (market.GetWithExchange) carried NO 15m series, so
// ExecutionATRPct silently fell back to the longest TF (4h, ATR ~1.1%) and
// the supply-zone gate rejected at 0.55% anchors the prompt-side suppression
// had blessed at the 15m floor (0.15%) — the same setup died every cycle.
// With 15m aggregated from 3m into the execution dataset, both sides must
// resolve the same threshold from the same yardstick.
func TestExecutionATRParityOnExecutionShapedData(t *testing.T) {
	now := time.Date(2026, 9, 19, 13, 49, 0, 0, time.UTC)

	// Quiet 3m bars (±0.12% ranges → 15m ATR well under 0.5%) ending with a
	// swing high ~0.22% above the anchor zone — the live loop geometry.
	var k3m []market.Kline
	for i := 0; i < 100; i++ {
		ts := now.Add(time.Duration(i-100) * 3 * time.Minute).UnixMilli()
		p := 81000 + float64(i)*2.3
		hi, lo := p*1.0012, p*0.9988
		cl := p
		if i == 97 {
			p, hi, cl = 81200, 81232, 81190
		}
		k3m = append(k3m, market.Kline{
			OpenTime: ts, Open: p * 0.999, High: hi, Low: lo, Close: cl, Volume: 10,
		})
	}
	// The execution dataset as market.executionTimeframeData now builds it:
	// 3m plus a 15m series AGGREGATED from those 3m bars (quiet ±0.12%
	// ranges), 1h and 4h alongside.
	var k15 []market.Kline
	for i := 0; i < 20; i++ {
		p := 81000 + float64(i)*7
		hi, lo, cl := p*1.0013, p*0.9987, p*1.0002
		if i == 19 { // trailing forming bucket — dropped by settlement
			hi = 81232
		}
		k15 = append(k15, market.Kline{OpenTime: now.Add(time.Duration(i-20) * 15 * time.Minute).UnixMilli(),
			Open: p, High: hi, Low: lo, Close: cl, Volume: 50})
	}
	var k1h, k4h []market.Kline
	for i := 0; i < 100; i++ {
		p := 80500 + float64(i)*8
		k1h = append(k1h, market.Kline{OpenTime: now.Add(time.Duration(i-100) * time.Hour).UnixMilli(),
			Open: p, High: p * 1.004, Low: p * 0.996, Close: p, Volume: 10})
		k4h = append(k4h, market.Kline{OpenTime: now.Add(time.Duration(i-100) * 4 * time.Hour).UnixMilli(),
			Open: p, High: p * 1.01, Low: p * 0.99, Close: p, Volume: 10})
	}

	// Fallback regression: without a 15m key, the yardstick must be 0 (→
	// fixed fallback), never the longest available series.
	sigOld, err := ComputeSymbolSignals("BTCUSDT", &market.Data{
		Symbol: "BTCUSDT", CurrentPrice: 81100,
		TimeframeData: map[string]*market.TimeframeSeriesData{
			"1h": mkTF("1h", k1h), "4h": mkTF("4h", k4h),
		},
	}, SignalOptions{Now: now, PrimaryTF: "15m", CurrentPrice: 81100, SupplyZonePct: 0.5})
	if err != nil {
		t.Fatalf("ComputeSymbolSignals (no 15m): %v", err)
	}
	if got := sigOld.ExecutionATRPct(); got != 0 {
		t.Fatalf("ExecutionATRPct = %.3f%% without a 15m series — silent longest-TF fallback is back", got)
	}

	sig, err := ComputeSymbolSignals("BTCUSDT", &market.Data{
		Symbol: "BTCUSDT", CurrentPrice: 81100,
		TimeframeData: map[string]*market.TimeframeSeriesData{
			"15m": mkTF("15m", k15),
			"1h":  mkTF("1h", k1h),
			"4h":  mkTF("4h", k4h),
		},
	}, SignalOptions{Now: now, PrimaryTF: "15m", CurrentPrice: 81100, SupplyZonePct: 0.5})
	if err != nil {
		t.Fatalf("ComputeSymbolSignals: %v", err)
	}

	execATR := sig.ExecutionATRPct()
	if execATR <= 0 {
		t.Fatal("ExecutionATRPct = 0 on execution-shaped data — 15m aggregation missing")
	}
	if t15 := sig.Timeframes["15m"]; t15 == nil || execATR != t15.ATRPct {
		t.Fatalf("ExecutionATRPct %.3f%% != 15m ATR — not reading the exact TF", execATR)
	}
	// The yardstick must be the 15m series, NOT the longest-TF fallback
	// (the 4h ATR here is ~8x the 15m's — that gap WAS the bug).
	if t4h := sig.Timeframes["4h"]; t4h != nil && execATR > t4h.ATRPct/2 {
		t.Fatalf("ExecutionATRPct %.3f%% tracks the 4h ATR %.3f%% — fallback regression", execATR, t4h.ATRPct)
	}
	breath := AnchorBreathingPct(execATR, AnchorOffsetConfig{}, 0.5)
	if breath > 0.3 {
		t.Fatalf("breathing threshold %.2f%% inflated (want the quiet-15m floor region, <=0.3%%)", breath)
	}
	// Prompt-side suppression and execution gate must agree at this
	// threshold: an anchor 0.22% under the swing high stays valid (it would
	// have been rejected at the old bogus 0.55%).
	if 0.22 < breath {
		t.Fatalf("anchor distance 0.22%% inside breathing %.2f%% — prompt and gate will disagree", breath)
	}
}
