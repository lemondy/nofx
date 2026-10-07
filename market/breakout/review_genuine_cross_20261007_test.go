package breakout

import (
	"math"
	"testing"
)

// C1: being outside throughout the hold window is not a cross (review 2026-10-07).
func TestComputeTFRequiresGenuineCross(t *testing.T) {
	for _, dir := range []string{DirUp, DirDown} {
		t.Run(dir, func(t *testing.T) {
			price, ahead, stale := 101.0, 105.0, "20d_low"
			if dir == DirDown {
				price, ahead, stale = 99, 95, "20d_high"
			}
			for _, n := range []int{1, 12, 40} {
				k := flat(n, price)
				rep := computeTF("1h", dir, k, []Level{{stale, 100}, {"ahead", ahead}}, &shared{})
				if rep.LevelSource != "ahead" || rep.Pattern != PatternApproach || rep.CrossAgeBars != -1 || rep.Confirmed {
					t.Fatalf("%d outside bars: level=%s pattern=%s age=%d confirmed=%v, want approach to ahead", n, rep.LevelSource, rep.Pattern, rep.CrossAgeBars, rep.Confirmed)
				}
			}
			// A genuine cross before the hold window must also expire (review 2026-10-07).
			k := flat(40, price)
			k[0] = mkCandle(100, 1000, 0.5)
			rep := computeTF("1h", dir, k, []Level{{stale, 100}}, &shared{})
			if rep.Pattern != PatternApproach || rep.CrossAgeBars != -1 || rep.LevelSource == stale {
				t.Fatalf("expired cross selected: %+v", rep)
			}
		})
	}
}

// C1: a high-priority unbroken extreme must not outrank a real cross (review 2026-10-07).
func TestComputeTFIgnoresUncrossedPriorityLevel(t *testing.T) {
	for _, dir := range []string{DirUp, DirDown} {
		inside, outside, stale := 95.0, 101.0, 80.0
		if dir == DirDown {
			inside, outside, stale = 105, 99, 120
		}
		k := flat(40, inside)
		for i := 36; i < len(k); i++ {
			k[i] = mkCandle(outside, 1000, 0.5)
		}
		rep := computeTF("1h", dir, k, []Level{{"20d_extreme", stale}, {"vpvr_poc", 100}}, &shared{})
		if rep.LevelSource != "vpvr_poc" || rep.Pattern != PatternBreakout || rep.CrossAgeBars != 3 {
			t.Fatalf("%s: level=%s pattern=%s age=%d, want the genuine cross at age 3", dir, rep.LevelSource, rep.Pattern, rep.CrossAgeBars)
		}
	}
}

// C1: equality is inside, including the close just before the window boundary (review 2026-10-07).
func TestComputeTFFirstGenuineCrossAge(t *testing.T) {
	for _, dir := range []string{DirUp, DirDown} {
		for _, tc := range []struct {
			name     string
			n, cross int
		}{
			{"window_boundary", 40, 16},
			{"recent", 40, 36},
			{"last_bar", 40, 39},
			{"short_series", 8, 1},
		} {
			t.Run(dir+"/"+tc.name, func(t *testing.T) {
				outside := 101.0
				if dir == DirDown {
					outside = 99
				}
				k := flat(tc.n, 100)
				for i := tc.cross; i < tc.n; i++ {
					k[i] = mkCandle(outside, 1000, 0.5)
				}
				rep := computeTF("1h", dir, k, []Level{{"trigger", 100}}, &shared{})
				if rep.CrossAgeBars != tc.n-1-tc.cross || !rep.Confirmed {
					t.Fatalf("age=%d confirmed=%v, want age=%d and held", rep.CrossAgeBars, rep.Confirmed, tc.n-1-tc.cross)
				}
			})
		}
	}
}

// C1: rebreaking must preserve the first age and failed-hold haircut (review 2026-10-07).
func TestComputeTFRebreakKeepsFailedHold(t *testing.T) {
	for _, dir := range []string{DirUp, DirDown} {
		for _, firstCross := range []int{20, 37} {
			inside, outside := 99.0, 101.0
			if dir == DirDown {
				inside, outside = 101, 99
			}
			k := flat(40, inside)
			for i := firstCross; i < len(k); i++ {
				k[i] = mkCandle(outside, 1000, 0.5)
			}
			k[firstCross+1] = mkCandle(inside, 1000, 0.5)
			rep := computeTF("1h", dir, k, []Level{{"trigger", 100}}, &shared{})
			pattern := PatternExtended
			if 39-firstCross < crossWindowBars {
				pattern = PatternBreakout
			}
			if rep.CrossAgeBars != 39-firstCross || rep.Confirmed || rep.Pattern != pattern {
				t.Fatalf("%s cross=%d: age=%d confirmed=%v pattern=%s", dir, firstCross, rep.CrossAgeBars, rep.Confirmed, rep.Pattern)
			}
			want := rep.RawScore * rep.Alpha * rep.Beta * confirmPenalty
			if math.Abs(rep.Score-want) > 1e-9 {
				t.Fatalf("score=%v, want failed-hold score %v", rep.Score, want)
			}
		}
	}
}

// C2: distinguish daily 15m slots from the old three-day step (review 2026-10-07).
func Test15mVolumeUsesDaily96BarSlots(t *testing.T) {
	k := make([]Kline, 865)
	for i := range k {
		k[i] = mkCandle(100, 9000, 0.5)
	}
	for d := 1; d <= 3; d++ {
		k[len(k)-1-d*96] = mkCandle(100, float64(d)*1000, 0.5)
	}
	k[len(k)-1] = mkCandle(100, 6000, 0.5)
	ds := &mockDS{series: map[string][]Kline{"1d": flat(90, 100), "15m": k, "1h": flat(169, 100)}}
	rep, err := Analyze("TESTUSDT", ds)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Context.VolMultiple15m != 3 {
		t.Fatalf("15m volume multiple=%v, want 6000/mean(1000,2000,3000)=3", rep.Context.VolMultiple15m)
	}
	p := GetParams()
	want := 0.5*sigmoidScore(3, p.VolCenter, 0.8) + 0.5*sigmoidScore(1, 1.15, 0.15)
	if got := rep.Timeframes["15m"][DirUp].Dims.Volume; math.Abs(got-want) > 1e-9 {
		t.Fatalf("15m volume dim=%v, want %v", got, want)
	}
	want1h := 0.5*sigmoidScore(volSlotMultiple(k, 24), p.VolCenter, 0.8) + 0.5*sigmoidScore(1, 1.15, 0.15)
	if got := computeTF("1h", DirUp, k, nil, &shared{}).Dims.Volume; math.Abs(got-want1h) > 1e-9 {
		t.Fatalf("1h volume dim=%v, want unchanged 24-bar slot score %v", got, want1h)
	}
}

// C3: price +10% and quantity -5% raises notional but adds no positions (review 2026-10-07).
func TestAnalyzeOIUsesQuantityInsteadOfNotional(t *testing.T) {
	ds := breakoutSetup(100, true, false)
	for _, tf := range []string{"15m", "1h"} {
		k := ds.series[tf]
		back := 4
		if tf == "1h" {
			back = 1
		}
		k[len(k)-1] = mkCandle(k[len(k)-1-back].Close*1.10, 5000, 0.7)
	}
	ds.oi = make([]OIPoint, 49)
	for i := range ds.oi {
		ds.oi[i] = OIPoint{OI: 100, Value: 10000}
	}
	ds.oi[48] = OIPoint{OI: 95, Value: 10000 * 1.10 * 0.95}
	rep, err := Analyze("TESTUSDT", ds)
	if err != nil {
		t.Fatal(err)
	}
	for _, chg := range []*float64{rep.Context.OIChange1hPct, rep.Context.OIChange4hPct} {
		if chg == nil || math.Abs(*chg+5) > 1e-9 {
			t.Fatalf("OI quantity change=%v, want -5%%", chg)
		}
	}
	for _, tf := range []string{"15m", "1h"} {
		up := rep.Timeframes[tf][DirUp]
		if up.Alpha != GetParams().AlphaWeak || up.Alpha == alphaHealthyOI {
			t.Fatalf("%s alpha=%v, want shrinking-quantity alpha", tf, up.Alpha)
		}
		wantFlow := 0.6*sigmoidScore(takerBull(lastN(ds.series[tf], 4)), 0.05, 0.04) + 0.4*sigmoidScore(-5, 0.4, 0.4)
		if math.Abs(up.Dims.Flow-wantFlow) > 1e-9 {
			t.Fatalf("%s flow=%v, want quantity-based flow %v", tf, up.Dims.Flow, wantFlow)
		}
	}
	ds.oi[48].Value = 9500
	other, err := Analyze("TESTUSDT", ds)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SelectedScore != other.SelectedScore || rep.Crowding != other.Crowding {
		t.Fatal("changing only OI notional altered the final score or crowding")
	}
}

// C3: percentage-point changes within +/-0.1% are neutral (review 2026-10-07).
func TestComputeTFOIAlphaPercentUnits(t *testing.T) {
	for _, dir := range []string{DirUp, DirDown} {
		k := flat(40, 100)
		price := 110.0
		if dir == DirDown {
			price = 90
		}
		k[len(k)-1] = mkCandle(price, 1000, 0.5)
		for _, tc := range []struct{ chg, alpha float64 }{
			{-0.11, GetParams().AlphaWeak}, {-0.1, alphaNeutral}, {-0.05, alphaNeutral},
			{0, alphaNeutral}, {0.05, alphaNeutral}, {0.1, alphaNeutral}, {0.11, alphaHealthyOI},
		} {
			rep := computeTF("1h", dir, k, []Level{{"trigger", 100}}, &shared{oiChg1h: &tc.chg})
			if rep.Alpha != tc.alpha {
				t.Fatalf("%s OI=%v%%: alpha=%v, want %v", dir, tc.chg, rep.Alpha, tc.alpha)
			}
		}
	}
}

// C4: an old up cross loses to the other side after its chase haircut (review 2026-10-07).
func TestAnalyzeSelectsDirectionAfterExtendedPenalty(t *testing.T) {
	ds := &mockDS{series: map[string][]Kline{"1d": flat(90, 90)}}
	for i := range ds.series["1d"] {
		ds.series["1d"][i].High, ds.series["1d"][i].Low = 100, 80
	}
	for _, tc := range []struct {
		tf string
		n  int
	}{{"15m", 865}, {"1h", 169}} {
		k := flat(tc.n, 95)
		for i := len(k) - 14; i < len(k); i++ {
			k[i] = mkCandle(104, 1000, 0.45)
		}
		ds.series[tc.tf] = k
	}
	rep, err := Analyze("TESTUSDT", ds)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Timeframes["1h"][DirUp].Pattern != PatternExtended || rep.Timeframes["1h"][DirDown].Pattern == PatternExtended {
		t.Fatalf("fixture patterns: up=%s down=%s", rep.Timeframes["1h"][DirUp].Pattern, rep.Timeframes["1h"][DirDown].Pattern)
	}
	if rep.Breakout.Score <= rep.Breakdown.Score || rep.Breakout.Score*extendedPenalty >= rep.Breakdown.Score {
		t.Fatalf("fixture scores: raw up=%v down=%v, discounted up=%v", rep.Breakout.Score, rep.Breakdown.Score, rep.Breakout.Score*extendedPenalty)
	}
	if rep.Selected != DirDown || rep.SelectedScore != rep.Breakdown.Score || rep.Grade != gradeOf(rep.SelectedScore) {
		t.Fatalf("selected=%s score=%v grade=%s, want undiscounted down=%v", rep.Selected, rep.SelectedScore, rep.Grade, rep.Breakdown.Score)
	}
	// Selected crowd metadata must also follow the final winning side (review 2026-10-07).
	ds.ls = []LongShortPoint{{Ratio: 3}}
	crowded, err := Analyze("TESTUSDT", ds)
	if err != nil {
		t.Fatal(err)
	}
	wantCrowding := crowdLSW * sigmoidScore(1.0/3-1, 0.5, 0.35)
	wantScore := crowded.Breakdown.Score * (1 - crowdMaxPenalty*wantCrowding/100)
	if crowded.Selected != DirDown || crowded.Crowding != round2(wantCrowding) || math.Abs(crowded.SelectedScore-wantScore) > 1e-9 || crowded.CrowdLSRatio == nil || *crowded.CrowdLSRatio != 3 {
		t.Fatalf("selected crowd metadata or score mismatch: %+v", crowded)
	}
}

// C4: reciprocal account ratios and funding percentiles mirror both sides (review 2026-10-07).
func TestDirectionCrowdingMirrorSymmetry(t *testing.T) {
	upRatio, downRatio, upFunding, downFunding, oi := 3.0, 1.0/3, 98.0, 2.0, 10.0
	up, upCrowd := finalDirectionScore(nil, DirUp, 80, &shared{fundPct: &upFunding, oiChg4h: &oi}, &upRatio)
	down, downCrowd := finalDirectionScore(nil, DirDown, 80, &shared{fundPct: &downFunding, oiChg4h: &oi}, &downRatio)
	if math.Abs(upCrowd-downCrowd) > 1e-9 || math.Abs(up-down) > 1e-9 {
		t.Fatalf("mirror scores/crowding differ: up=%v/%v down=%v/%v", up, upCrowd, down, downCrowd)
	}
	wantCrowd := crowdFundW*sigmoidScore(98, 88, 9) + crowdOIW*sigmoidScore(10, 8, 6) + crowdLSW*sigmoidScore(2, 0.5, 0.35)
	if math.Abs(upCrowd-wantCrowd) > 1e-9 || math.Abs(up-80*(1-crowdMaxPenalty*wantCrowd/100)) > 1e-9 {
		t.Fatal("direction score must include all three crowding inputs")
	}
}

// C5: exact flat OHLC must produce finite timeframe and final scores (review 2026-10-07).
func TestComputeTFAndAnalyzeFlatATRFinite(t *testing.T) {
	for _, price := range []float64{100, 1e-9, 0} {
		k := flat(90, price)
		for i := range k {
			k[i].Open, k[i].High, k[i].Low, k[i].Close = price, price, price, price
		}
		ds := &mockDS{series: map[string][]Kline{"1d": k, "15m": k, "1h": k}}
		rep, err := Analyze("TESTUSDT", ds)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []float64{rep.SelectedScore, rep.Breakout.Score, rep.Breakdown.Score, rep.Context.ATR15m, rep.Context.ATR1h} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("price=%v: non-finite report value %v", price, v)
			}
		}
		for _, tf := range []string{"15m", "1h"} {
			for _, dir := range []string{DirUp, DirDown} {
				s := rep.Timeframes[tf][dir]
				if math.IsNaN(s.Score) || math.IsInf(s.Score, 0) || math.IsNaN(s.RawScore) {
					t.Fatalf("price=%v %s/%s: non-finite score %+v", price, tf, dir, s)
				}
			}
		}
		if rep.Context.ATR15m <= 0 || rep.Context.ATR1h <= 0 {
			t.Fatal("flat ATR must have a positive fallback")
		}
	}
}

// C5: non-finite ranges also need a finite normalization baseline (review 2026-10-07).
func TestComputeTFNonFiniteATRFiniteScore(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1)} {
		k := flat(40, 100)
		k[len(k)-1].High = bad
		for _, dir := range []string{DirUp, DirDown} {
			rep := computeTF("1h", dir, k, nil, &shared{})
			if math.IsNaN(rep.Score) || math.IsInf(rep.Score, 0) || math.IsNaN(rep.RawScore) {
				t.Fatalf("ATR input=%v %s: non-finite score %+v", bad, dir, rep)
			}
		}
	}
}
