package kernel

import (
	"math"
	"testing"
	"time"

	"nofx/market"
)

// Market regime: EMA/ATR warm up on the full history, so the EMA inputs do not
// depend on the display count; the ATR percentile keeps the visible window as
// its population.
func TestReview20261028MarketRegimeIndependentOfDisplayCount(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	b1 := r1028Walk(300, 21, now.Add(-time.Hour), time.Hour)
	b4 := r1028Walk(300, 22, now.Add(-4*time.Hour), 4*time.Hour)
	measure := func(count int) *MarketRegime {
		data := &market.Data{TimeframeData: map[string]*market.TimeframeSeriesData{
			"1h": r1028Series("1h", b1, count), "4h": r1028Series("4h", b4, count)}}
		return measureMarketRegime(data, now).state
	}
	ref := measure(120)
	if ref.Inputs.EMAAlignment1h == "UNKNOWN" || ref.Inputs.EMASlope1hPct == nil || ref.Inputs.EMASlope4hPct == nil {
		t.Fatalf("reference regime incomplete: %+v", ref.Inputs)
	}
	for _, count := range []int{60, 80, 150, 250} {
		got := measure(count)
		if got.Inputs.EMASlope1hPct == nil || got.Inputs.EMASlope4hPct == nil {
			t.Fatalf("count=%d: regime incomplete: %+v", count, got.Inputs)
		}
		if got.Inputs.EMAAlignment1h != ref.Inputs.EMAAlignment1h || got.Inputs.EMAAlignment4h != ref.Inputs.EMAAlignment4h ||
			*got.Inputs.EMASlope1hPct != *ref.Inputs.EMASlope1hPct || *got.Inputs.EMASlope4hPct != *ref.Inputs.EMASlope4hPct {
			t.Errorf("count=%d: EMA inputs %+v differ from reference %+v", count, got.Inputs, ref.Inputs)
		}
	}

	// ATR percentile: population = visible window, values warmed on the full
	// history. Independent recomputation for count=80.
	series := wilderATRSeries(barsToKlines(b1), 14)
	var vals []float64
	for i := len(b1) - 80 + 14; i < len(b1); i++ {
		vals = append(vals, series[i-14]/b1[i].Close*100)
	}
	cur := vals[len(vals)-1]
	less, equal := 0, 0
	for _, v := range vals {
		if v < cur {
			less++
		} else if math.Abs(v-cur) <= 1e-12 {
			equal++
		}
	}
	want := round2((float64(less) + 0.5*float64(equal)) / float64(len(vals)) * 100)
	if got := measure(80).Inputs.ATRPercentile1h; got == nil || *got != want {
		t.Errorf("ATR percentile = %v, want %v", ptrVal(got), want)
	}

	// Full history absent: identical to the display-only computation.
	disp := barsToKlines(b1[len(b1)-80:])
	wantP, ok := atrPercentile(disp, 14)
	gotP, ok2 := atrPercentileWarm(disp, disp, 14)
	if !ok || !ok2 || wantP != gotP {
		t.Errorf("fallback percentile %v/%v != %v/%v", gotP, ok2, wantP, ok)
	}

	// Step change: display-only EMA reads flat, the warmed one reads UP.
	step := barsToKlines(r1028Hours(200, func(i int) float64 {
		if i < 130 {
			return 100
		}
		return 200
	}, now))
	if a, _, _ := emaRegimeEvidence(step[130:]); a != "FLAT" {
		t.Errorf("display-only alignment = %s, want FLAT", a)
	}
	if a, _, ok := emaRegimeEvidenceWarm(step[130:], step); !ok || a != "UP" {
		t.Errorf("warmed alignment = %s ok=%v, want UP", a, ok)
	}
	// Minimum-bar requirement stays on the visible window.
	if _, _, ok := emaRegimeEvidenceWarm(step[200-(50+regimeSlopeLookback)+1:], step); ok {
		t.Errorf("display shorter than the minimum must stay UNKNOWN")
	}
}
