package breakout

import (
	"math"
	"testing"
)

// ── review 2026-10-04 #1: computeTF cross semantics ──
//
// The old age definition ("bars since the most recent far-side close") made
// held and Confirmed tautologically true: confirmPenalty was dead code, the
// retest scan started on the crossing bar (whose low sits at the level by
// construction), and a break-lose-rebreak coin re-aged as a pristine fresh
// breakout. These tests pin the first-cross semantics.

// tfBars builds n bars from (high, low, close) triples.
func tfBars(n int, fn func(i int) (h, l, c float64)) []Kline {
	k := make([]Kline, n)
	for i := range k {
		h, l, c := fn(i)
		k[i] = Kline{OpenTime: int64(i), Open: c, High: h, Low: l, Close: c, Volume: 1000, QuoteVolume: 1000 * c, TakerBuyBase: 500, TakerBuyQty: 500 * c}
	}
	return k
}

// tfSeries: 20 bars below 100, cross at bar 20 (close 101), then `tail`
// closes per the callback. Returns the bar slice.
func tfSeries(tail func(i int) (h, l, c float64)) []Kline {
	return tfBars(34, func(i int) (h, l, c float64) {
		if i < 20 {
			return 95.2, 94.9, 95.0
		}
		if i == 20 {
			return 101.5, 99.0, 101.0 // the crossing bar — deep-dip low
		}
		return tail(i)
	})
}

// A coin that crossed, closed back inside, and recovered must carry
// Confirmed=false (confirmPenalty ×0.5) — the old code re-aged it as a
// clean hold and confirmed everything.
func TestComputeTFLostLevelPenalized(t *testing.T) {
	levels := []Level{{Name: "20d_high", Price: 100}}

	lost := computeTF("1h", DirUp, tfSeries(func(i int) (h, l, c float64) {
		if i == 21 {
			return 100.8, 99.2, 99.5 // falls back inside the level
		}
		return 101.8, 101.2, 101.5
	}), levels, &shared{})
	if lost.Pattern != PatternExtended {
		t.Fatalf("pattern = %q (age=%d), want extended for a lose-and-recover cross", lost.Pattern, lost.CrossAgeBars)
	}
	if lost.Confirmed {
		t.Fatal("a cross that closed back inside must NOT be confirmed — confirmPenalty is dead code again")
	}
	if lost.CrossAgeBars != 13 {
		t.Fatalf("cross age = %d, want 13 (bars since the FIRST close beyond)", lost.CrossAgeBars)
	}

	clean := computeTF("1h", DirUp, tfSeries(func(i int) (h, l, c float64) {
		return 101.8, 101.2, 101.5
	}), levels, &shared{})
	if !clean.Confirmed || clean.Pattern != PatternExtended {
		t.Fatalf("clean hold: pattern=%q confirmed=%v, want extended+confirmed", clean.Pattern, clean.Confirmed)
	}
	// The ×0.5 confirmPenalty must actually move the TF score (allowing the
	// small ATR drift between the two series).
	if lost.Score > clean.Score*0.62 {
		t.Fatalf("penalized score %.2f should be ≤ ~0.5× clean %.2f — confirmPenalty not applied", lost.Score, clean.Score)
	}
}

// A fresh cross (age < crossWindowBars) with a far-side close in between is
// an UNCONFIRMED breakout, not a pristine one.
func TestComputeTFBreakoutLoseUnconfirmed(t *testing.T) {
	k := tfBars(34, func(i int) (h, l, c float64) {
		if i < 30 {
			return 95.2, 94.9, 95.0
		}
		if i == 30 {
			return 101.5, 100.2, 101.0 // cross
		}
		if i == 31 {
			return 100.8, 99.2, 99.5 // lost the level
		}
		return 102.3, 101.7, 102.0 // recovered
	})
	rep := computeTF("1h", DirUp, k, []Level{{Name: "20d_high", Price: 100}}, &shared{})
	if rep.Pattern != PatternBreakout || rep.CrossAgeBars != 3 {
		t.Fatalf("pattern=%q age=%d, want breakout @ age 3", rep.Pattern, rep.CrossAgeBars)
	}
	if rep.Confirmed {
		t.Fatal("breakout with an inside close must be unconfirmed (confirmPenalty path)")
	}
}

// The retest scan must SKIP the crossing bar: its low sits at/below the
// level by construction (it opened near the prior far-side close), and
// counting it graded almost every break as retest_hold.
func TestComputeTFRetestExcludesCrossBar(t *testing.T) {
	levels := []Level{{Name: "20d_high", Price: 100}}
	// Cross bar dips to 99, then price runs away with lows at 102.6 — no
	// retest after the cross ⇒ extended, not retest_hold.
	chase := computeTF("1h", DirUp, tfSeries(func(i int) (h, l, c float64) {
		return 103.3, 102.6, 103.0
	}), levels, &shared{})
	if chase.Pattern != PatternExtended {
		t.Fatalf("pattern = %q, want extended — the crossing bar's low must not count as a retest", chase.Pattern)
	}
	// A LATER bar that dips to the level and holds above it IS a retest.
	retest := computeTF("1h", DirUp, tfBars(34, func(i int) (h, l, c float64) {
		if i < 20 {
			return 95.2, 94.9, 95.0
		}
		if i == 20 {
			return 101.5, 99.0, 101.0
		}
		if i == 33 {
			return 101.6, 100.1, 101.0 // the real pullback, held
		}
		return 101.8, 101.2, 101.5
	}), levels, &shared{})
	if retest.Pattern != PatternRetestHold || !retest.Confirmed {
		t.Fatalf("pattern=%q confirmed=%v, want retest_hold+confirmed for a post-cross pullback", retest.Pattern, retest.Confirmed)
	}
}

// ── review 2026-10-04 #2: DirDown room ──
//
// The DirDown next-level scan could never fire (nextLv seeded +Inf against a
// "> nextLv" test): breakdown RoomATR was the constant 3.0 and the
// ×0.75/×0.9 Price discounts were long-only. Mirror-symmetry pin: a series
// and its price-mirror must produce the SAME room.
func TestComputeTFRoomMirrorSymmetry(t *testing.T) {
	up := tfBars(40, func(i int) (h, l, c float64) {
		if i < 36 {
			return 95.2, 94.8, 95.0
		}
		if i == 36 {
			return 102.3, 101.7, 102.0 // cross
		}
		return 105.3, 104.7, 105.0
	})
	down := make([]Kline, len(up))
	for i, k := range up {
		down[i] = Kline{
			OpenTime: k.OpenTime,
			Open:     200 - k.Open, High: 200 - k.Low, Low: 200 - k.High, Close: 200 - k.Close,
			Volume: k.Volume, QuoteVolume: k.QuoteVolume, TakerBuyBase: k.Volume - k.TakerBuyBase, TakerBuyQty: k.QuoteVolume - k.TakerBuyQty,
		}
	}

	repUp := computeTF("1h", DirUp, up, []Level{{Name: "20d_high", Price: 100}, {Name: "resistance", Price: 106.5}}, &shared{})
	repDown := computeTF("1h", DirDown, down, []Level{{Name: "20d_low", Price: 100}, {Name: "support", Price: 93.5}}, &shared{})

	if repUp.RoomATR <= 0 || repUp.RoomATR >= 3 {
		t.Fatalf("up room = %.2f, want (0,3) so the assertion below is meaningful", repUp.RoomATR)
	}
	if repDown.RoomATR != repUp.RoomATR {
		t.Fatalf("mirror rooms differ: up=%.2f down=%.2f — DirDown room is not being measured", repUp.RoomATR, repDown.RoomATR)
	}
}

// ── review 2026-10-04 #5: the single capped grading exit ──
func TestFinalizeShortGradeCapsUnconfirmed(t *testing.T) {
	if g := finalizeShortGrade(84, false); g != "medium" {
		t.Fatalf("unconfirmed 84 → %q, want medium (strong cap)", g)
	}
	if g := finalizeShortGrade(84, true); g != "strong" {
		t.Fatalf("confirmed 84 → %q, want strong", g)
	}
	if g := finalizeShortGrade(71.4, false); g != "medium" {
		t.Fatalf("unconfirmed 71.4 → %q, want medium — the raw-84 ×0.85 flip-back", g)
	}
	if g := finalizeShortGrade(45, false); g != "weak" {
		t.Fatalf("45 → %q, want weak", g)
	}
}

// The BTC-bull haircut must re-grade THROUGH the cap: an unconfirmed strong
// discounted to 71.4 used to flip back to strong (shortGradeOf alone), and
// slow-top entries merged after the loop skipped the haircut entirely.
func TestApplyShortBtcRegimePreservesCap(t *testing.T) {
	out := []ShortSignal{
		{Symbol: "PUMPUSDT", Score: 84, Grade: "medium", Confirmed: false}, // raw 84 unconfirmed: capped at medium pre-discount
		{Symbol: "GRINDUSDT", Score: 80, Grade: "strong", Confirmed: true}, // confirmed strong
		{Symbol: "LATEUSDT", Score: 50, Grade: "weak", Confirmed: false},
	}
	applyShortBtcRegime(out, 0.85, "btc_bull")
	if out[0].Score != 71.4 || out[0].Grade != "medium" {
		t.Fatalf("unconfirmed 84 → (%.1f, %s), want (71.4, medium) — the cap must survive the discount", out[0].Score, out[0].Grade)
	}
	if out[1].Score != 68 || out[1].Grade != "medium" {
		t.Fatalf("confirmed 80 → (%.1f, %s), want (68, medium)", out[1].Score, out[1].Grade)
	}
	if out[2].Grade != "weak" || out[2].Score != 42.5 {
		t.Fatalf("50 → (%.1f, %s), want (42.5, weak)", out[2].Score, out[2].Grade)
	}
	for i := range out {
		if out[i].BtcRegime != "btc_bull" {
			t.Fatalf("%s BtcRegime = %q, want btc_bull", out[i].Symbol, out[i].BtcRegime)
		}
	}
	// Pass-through regime (mult 1.0) must not touch scores but still label.
	applyShortBtcRegime(out, 1.0, "chop")
	if out[0].Score != 71.4 {
		t.Fatalf("pass-through regime must not re-apply a haircut, got %.1f", out[0].Score)
	}
	if out[0].BtcRegime != "chop" {
		t.Fatalf("label = %q, want chop", out[0].BtcRegime)
	}
}

// ── review 2026-10-04 #8: FakeBreakout requires the price to STILL be inside ──
func TestAnalyzeShortFakeBreakoutNeedsCurrentBreak(t *testing.T) {
	build := func(recovered bool) *mockDS {
		k := make([]Kline, 0, 60)
		for i := 0; i < 55; i++ {
			p := 90 + float64(i)*0.25
			k = append(k, Kline{OpenTime: int64(i), Open: p, High: p + 0.4, Low: p - 0.2, Close: p, Volume: 1000, QuoteVolume: 1000 * p, TakerBuyBase: 500, TakerBuyQty: 500 * p})
		}
		// Bar 55 (inside the last-5 scan window): spike above the ~103.6
		// swing, closes back below it.
		k = append(k, Kline{OpenTime: 55, Open: 103.5, High: 107, Low: 102, Close: 102, Volume: 1500, QuoteVolume: 1500 * 102, TakerBuyBase: 700, TakerBuyQty: 700 * 102})
		tailC := 101.0 // still below the swing ⇒ the fake stands
		if recovered {
			tailC = 105.5 // reclaimed and held ⇒ trend continuation, not a top
		}
		for i := 56; i < 60; i++ {
			k = append(k, Kline{OpenTime: int64(i), Open: tailC, High: tailC + 0.3, Low: tailC - 0.3, Close: tailC, Volume: 1000, QuoteVolume: 1000 * tailC, TakerBuyBase: 500, TakerBuyQty: 500 * tailC})
		}
		return &mockDS{series: map[string][]Kline{"1h": k, "4h": k}, funding: []FundingPoint{{Rate: 0.0002, TS: 1}}}
	}
	if sig, err := AnalyzeShort("TESTUSDT", 5, nil, build(true)); err != nil {
		t.Fatalf("AnalyzeShort: %v", err)
	} else if sig.FakeBreakout {
		t.Fatalf("a fake from 5 bars ago that has since been RECLAIMED must not flag (structure=%v)", sig.Components.Structure)
	}
	if sig, err := AnalyzeShort("TESTUSDT", 5, nil, build(false)); err != nil {
		t.Fatalf("AnalyzeShort: %v", err)
	} else if !sig.FakeBreakout {
		t.Fatal("spike-above-close-below with price still under the swing must flag FakeBreakout")
	}
}

// ── review 2026-10-04 #9: crowding data conventions ──
//
// A funding-history failure used to zero the WHOLE crowding component,
// discarding working OI and L/S evidence; and fundPart scored the raw
// per-settlement rate, blind to 1h/4h settlement intervals.
func TestAnalyzeShortCrowdingSurvivesFundingFailure(t *testing.T) {
	k := make([]Kline, 0, 60)
	for i := 0; i < 60; i++ {
		k = append(k, mkCandle(100+float64(i), 1000, 50))
	}
	m := &mockDS{
		series: map[string][]Kline{"1h": k, "4h": k},
		oi:     []OIPoint{{OI: 250, Value: 25_000_000, TS: 1}, {OI: 260, Value: 26_000_000, TS: 2}},
		ls:     []LongShortPoint{{Ratio: 2.0, TS: 1}},
		// no funding → FundingInfo AND FundingHistory both fail
	}
	sig, err := AnalyzeShort("TESTUSDT", 5, nil, m)
	if err != nil {
		t.Fatalf("AnalyzeShort: %v", err)
	}
	if sig.FundingAnnualPct != 0 {
		t.Fatalf("FundingAnnualPct = %.2f, want 0 (no funding data)", sig.FundingAnnualPct)
	}
	if sig.Components.Crowding <= 0 {
		t.Fatalf("crowding = %.1f — OI+L/S evidence must survive a funding-history failure", sig.Components.Crowding)
	}
	if math.IsNaN(sig.Components.Crowding) {
		t.Fatal("crowding is NaN")
	}
}

func TestAnalyzeShortCrowdingFundPartAnnualized(t *testing.T) {
	k := make([]Kline, 0, 60)
	for i := 0; i < 60; i++ {
		k = append(k, mkCandle(100+float64(i), 1000, 50))
	}
	crowdFor := func(intervalHours float64) float64 {
		m := &mockDS{
			series: map[string][]Kline{"1h": k, "4h": k},
			funding: []FundingPoint{{Rate: 0.0001, TS: 1}},
		}
		// Override the settlement interval the FundingInfo reports.
		m.fundingInfoHours = intervalHours
		sig, err := AnalyzeShort("TESTUSDT", 5, nil, m)
		if err != nil {
			t.Fatalf("AnalyzeShort: %v", err)
		}
		return sig.Components.Crowding
	}
	fast := crowdFor(1)  // 0.01%/h ≈ 876%/yr — genuinely crowded
	slow := crowdFor(8)  // 0.01%/8h ≈ 11%/yr — mild
	if fast <= slow+20 {
		t.Fatalf("same per-period rate: 1h settlement crowding %.1f must far exceed 8h %.1f — fundPart is interval-blind again", fast, slow)
	}
}

func TestClamp100NaN(t *testing.T) {
	if got := clamp100(math.NaN()); got != 0 {
		t.Fatalf("clamp100(NaN) = %v, want 0", got)
	}
}

// ── review 2026-10-04 #3: regime adjustment re-grades and re-ranks ──
func TestApplyRegimeAdjustmentRecomputesGradeAndPercentile(t *testing.T) {
	results := []ScanResult{
		{Symbol: "AAAUSDT", Direction: DirUp, Score: 90, Grade: "strong"},
		{Symbol: "BBBUSDT", Direction: DirDown, Score: 88, Grade: "strong"},
		{Symbol: "BTCUSDT", Direction: DirUp, Score: 70, Grade: "medium"},
	}
	applyRegimeAdjustment(results, "btc_bull")
	// BBB (short in a bull regime) takes ×0.85 → 74.8; its Grade label must
	// follow the FINAL score, not stay at the pre-adjustment "strong".
	bySym := map[string]ScanResult{}
	for _, r := range results {
		bySym[r.Symbol] = r
	}
	if b := bySym["BBBUSDT"]; b.Score != 74.8 || b.Grade != "medium" {
		t.Fatalf("BBB = (%.1f, %s), want (74.8, medium) — stale grade after the haircut", b.Score, b.Grade)
	}
	if b := bySym["AAAUSDT"]; b.Score != 90 || b.Grade != "strong" || b.Regime != "btc_bull" {
		t.Fatalf("AAA = (%.1f, %s, %s) — aligned long in bull regime must pass unlubed", b.Score, b.Grade, b.Regime)
	}
	// BTC itself is the regime driver — never haircut.
	if b := bySym["BTCUSDT"]; b.Score != 70 {
		t.Fatalf("BTC score = %.1f, want untouched 70", b.Score)
	}
	// Percentile must reflect the POST-haircut order: AAA 90 > BBB 74.8 > BTC 70.
	if results[0].Symbol != "AAAUSDT" || results[1].Symbol != "BBBUSDT" || results[2].Symbol != "BTCUSDT" {
		t.Fatalf("final order %s/%s/%s, want AAA/BBB/BTC", results[0].Symbol, results[1].Symbol, results[2].Symbol)
	}
	if p := results[1].Percentile; p < 66 || p > 67 {
		t.Fatalf("BBB percentile = %.1f, want ≈66.7 from the post-haircut ranking", p)
	}
}

func TestApplyRegimeAdjustmentChopGradesFollow(t *testing.T) {
	results := []ScanResult{
		{Symbol: "AAAUSDT", Direction: DirUp, Score: 41, Grade: "weak"},
	}
	applyRegimeAdjustment(results, "chop")
	if results[0].Score != 38.95 {
		t.Fatalf("score = %.2f, want 38.95 (41 × 0.95)", results[0].Score)
	}
	if results[0].Grade != "noise" {
		t.Fatalf("grade = %s, want noise — 38.95 crossed the weak floor and the label must follow", results[0].Grade)
	}
}
