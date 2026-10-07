package breakout

import (
	"math"
	"testing"
	"time"
)

// review 2026-10-07 S5: constant +1% per 4h bar must give accel ~ 0 (both
// windows span 30 intervals).
func TestPumpWindows5dEqualSpans(t *testing.T) {
	c := make([]float64, 70)
	p := 100.0
	for i := range c {
		c[i] = p
		p *= 1.01
	}
	r, pr, ok := pumpWindows5d(c)
	if !ok {
		t.Fatal("70 bars must be enough")
	}
	if math.Abs(r-pr) > 1e-6 {
		t.Fatalf("recent %.4f vs prior %.4f: accel must be ~0 for constant growth", r, pr)
	}
	if _, _, ok := pumpWindows5d(c[:60]); ok {
		t.Fatal("60 bars must be insufficient (need 61)")
	}
}

// S2/S3: an EMA20 break (or funding rollover) alone is not a confirmation.
func TestAnalyzeShortEMABreakNotConfirmed(t *testing.T) {
	k := make([]Kline, 0, 60)
	for i := 0; i < 58; i++ {
		k = append(k, mkCandle(100+float64(i)*0.1, 1000, 50))
	}
	for i := 58; i < 60; i++ {
		k = append(k, mkCandle(90, 1000, 50)) // sharp drop under EMA20
	}
	m := &mockDS{
		series:  map[string][]Kline{"1h": k, "4h": k},
		funding: []FundingPoint{{Rate: 0.0005, TS: 1}, {Rate: 0.0005, TS: 2}, {Rate: 0.0005, TS: 3}, {Rate: 0.0005, TS: 4}},
	}
	sig, err := AnalyzeShort("TESTUSDT", 5, nil, m)
	if err != nil {
		t.Fatal(err)
	}
	if !sig.MABreak {
		t.Fatal("fixture must produce an EMA20 break")
	}
	if sig.Confirmed && !sig.BearishDiv4h && !sig.FakeBreakout {
		t.Fatalf("Confirmed must not be driven by MABreak/FundingRollover alone: %+v", sig)
	}
	if sig.Components.Structure < 80 {
		t.Fatalf("MABreak must still feed Structure, got %v", sig.Components.Structure)
	}
}

// S6: crowding OI build-up is measured on contract quantity, not notional.
func TestAnalyzeShortOICrowdingUsesQuantity(t *testing.T) {
	k := make([]Kline, 0, 60)
	for i := 0; i < 60; i++ {
		k = append(k, mkCandle(100+float64(i), 1000, 50))
	}
	build := func(qtyGrowth, valGrowth float64) float64 {
		oi := make([]OIPoint, 60)
		for i := range oi {
			f := 0.0
			if i >= 11 { // last 49 points ramp
				f = float64(i-11) / 48
			}
			oi[i] = OIPoint{OI: 1000 * (1 + qtyGrowth*f), Value: 1e7 * (1 + valGrowth*f), TS: int64(i)}
		}
		m := &mockDS{series: map[string][]Kline{"1h": k, "4h": k}, oi: oi}
		sig, err := AnalyzeShort("TESTUSDT", 5, nil, m)
		if err != nil {
			t.Fatal(err)
		}
		return sig.Components.Crowding
	}
	notionalOnly := build(0, 0.5)
	quantityRise := build(0.5, 0)
	if notionalOnly >= quantityRise {
		t.Fatalf("notional-only rise (%.1f) must score below a real contract build-up (%.1f)", notionalOnly, quantityRise)
	}
}

// S7: the history pool uses the MAX recorded change across all days.
func TestHistoryUniverseCandidatesPeakAcrossDays(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	d1 := now.AddDate(0, 0, -1).Format("2006-01-02")
	d2 := now.AddDate(0, 0, -2).Format("2006-01-02")
	hist := &gainerHistoryFile{Days: map[string][]gainerHistEntry{
		d1: {{Symbol: "AUSDT", Chg: 30}, {Symbol: "BUSDT", Chg: 50}},
		d2: {{Symbol: "AUSDT", Chg: 90}},
	}}
	index := map[string]GainerQuote{"AUSDT": {Symbol: "AUSDT"}, "BUSDT": {Symbol: "BUSDT"}}
	for i := 0; i < 50; i++ { // map order is random; result must not be
		got := historyUniverseCandidates(hist, now, 7, index, map[string]bool{}, 10)
		if len(got) != 2 || got[0].Symbol != "AUSDT" || got[1].Symbol != "BUSDT" {
			t.Fatalf("iter %d: got %+v, want AUSDT (peak 90) before BUSDT (50)", i, got)
		}
	}
}
