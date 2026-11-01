package kernel

import (
	"reflect"
	"testing"
	"time"

	"nofx/market"
)

// Reflection lets this regression run against the original schema as well,
// exposing the incorrect settlement instead of failing to compile.
func vfObserve(td *market.TimeframeSeriesData, at time.Time) {
	field := reflect.ValueOf(td).Elem().FieldByName("ObservedAt")
	if field.IsValid() {
		field.Set(reflect.ValueOf(at))
	}
}

func vfObservedSeries(tf string, lastOpen time.Time, lastClose float64) *market.TimeframeSeriesData {
	dur := market.TimeframeDuration(tf)
	bars := make([]market.KlineBar, 200)
	for i := range bars {
		c := 100.0
		if i == len(bars)-1 {
			c = lastClose
		}
		bars[i] = market.KlineBar{Time: lastOpen.Add(-time.Duration(len(bars)-1-i) * dur).UnixMilli(),
			Open: c, High: c + 1, Low: c - 1, Close: c, Volume: 1}
	}
	return &market.TimeframeSeriesData{Timeframe: tf, Klines: bars[140:], IndicatorKlines: bars}
}

func TestReview20261028VFObservedBeforeBoundary(t *testing.T) {
	boundary := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	observed, sampled := boundary.Add(-30*time.Second), boundary.Add(5*time.Second)
	for _, tf := range []string{"1m", "15m", "1h", "4h"} {
		t.Run(tf, func(t *testing.T) {
			td := vfObservedSeries(tf, boundary.Add(-market.TimeframeDuration(tf)), 151)
			vfObserve(td, observed)
			data := &market.Data{Symbol: "BTCUSDT", CurrentPrice: 151, SampledAt: sampled,
				TimeframeData: map[string]*market.TimeframeSeriesData{tf: td}}
			before, _ := computeTFSignal(tf, td, observed, 151)
			after, _ := computeTFSignal(tf, td, snapshotNow(data, sampled.Add(time.Hour)), 151)
			if before.BarsUsed != 59 || after.BarsUsed != before.BarsUsed || !after.UnclosedDropped || after.LastClose != 100 {
				t.Errorf("cross-boundary signal: before=%d after=%d dropped=%v close=%v", before.BarsUsed, after.BarsUsed, after.UnclosedDropped, after.LastClose)
			}
			if bars := ClosedKlines(td, sampled, market.TimeframeDuration(tf)); len(bars) != 59 {
				t.Errorf("ClosedKlines used %d bars, want 59", len(bars))
			}
			if got := RegimeLine(data, tf, sampled.Add(time.Hour)); got != 100 {
				t.Errorf("RegimeLine=%v want 100", got)
			}
			if tf == "15m" {
				if _, trend := ExecutionMicroTrend(data, sampled); trend != "range" {
					t.Errorf("micro trend=%s want range", trend)
				}
			}
			// Missing per-TF metadata falls back to the snapshot clock.
			vfObserve(td, time.Time{})
			control, _ := computeTFSignal(tf, td, snapshotNow(data, time.Time{}), 151)
			if control.BarsUsed != 60 || control.UnclosedDropped {
				t.Errorf("SampledAt fallback changed: %+v", control)
			}
		})
	}
}

func TestReview20261028VFRegimeObservationAndHistoricalCutoff(t *testing.T) {
	boundary := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	observed, sampled := boundary.Add(-30*time.Second), boundary.Add(5*time.Second)
	t1 := vfObservedSeries("1h", boundary.Add(-time.Hour), 151)
	t4 := vfObservedSeries("4h", boundary.Add(-4*time.Hour), 151)
	vfObserve(t1, observed)
	vfObserve(t4, observed)
	data := &market.Data{SampledAt: sampled, TimeframeData: map[string]*market.TimeframeSeriesData{"1h": t1, "4h": t4}}
	want := computeMarketRegime(data, observed)
	got := computeMarketRegime(data, sampled.Add(time.Hour))
	if !want.BarsClosed || !reflect.DeepEqual(got, want) {
		t.Errorf("market regime advanced past observations: got=%+v want=%+v", got, want)
	}
	for _, tc := range []struct {
		cutoff time.Time
		want   int
	}{{sampled, 59}, {boundary.Add(-time.Hour - time.Nanosecond), 58}} {
		if bars := closedKlinesAt(t1, tc.cutoff, time.Hour); len(bars) != tc.want {
			t.Errorf("historical cutoff %v: bars=%d want=%d", tc.cutoff, len(bars), tc.want)
		}
	}
	// Legacy data still freezes at SampledAt when no TF clock is available.
	vfObserve(t1, time.Time{})
	vfObserve(t4, time.Time{})
	data.SampledAt = observed
	if got := computeMarketRegime(data, sampled); !reflect.DeepEqual(got, want) {
		t.Errorf("market regime ignored SampledAt fallback: got=%+v want=%+v", got, want)
	}
}
