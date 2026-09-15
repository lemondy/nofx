package kernel

import (
	"math"

	"nofx/market"
)

// Bollinger-band ride detection (user directive 2026-09-11): when a symbol's
// short-term volume surges while price hugs the UPPER Bollinger band rising
// for ≥3 consecutive 15m windows, momentum entries at market are allowed to
// bypass the limit-order default. The kernel pre-computes the verdict so the
// model reads one flag instead of assembling band math itself.
//
// 09-16: SHORT-side mirror (closes the 09-13 todo "bb_ride 空头版"). A
// plunge riding the LOWER band with volume surge is exactly the case where
// waiting for a bounce to the (suppressed) short limit anchor is design
// non-fill — short_ride.ride=true is the program evidence that lets the
// model claim the third market-order exception (open_short only). The
// micro-trend gate is NOT bypassed: a lower-band plunge has 15m trend down,
// which the gate already allows.

const (
	bbRidePeriod    = 20.0 // Bollinger length (SMA of closes)
	bbRideMult      = 2.0  // Bollinger width (σ multiplier)
	bbRideMinBars   = 3    // "连续3个窗口以上"
	bbRideVolMult   = 1.5  // "交易量暴增" — same bar as breakout volume confirmation
	bbRideVolumeAvg = 20   // volume average window per bar
)

// BBRide is the pre-computed 15m upper-band ride verdict.
type BBRide struct {
	Ride        bool    `json:"ride"`         // ride && volume_surge both hold
	Windows     int     `json:"windows"`      // trailing consecutive qualifying closed 15m bars
	UpperBand   float64 `json:"upper_band"`   // current upper Bollinger (20, 2σ), closed bars
	VolumeSurge bool    `json:"volume_surge"` // any qualifying bar's volume ≥1.5× its prior-20 average
}

// BBShortRide is the pre-computed 15m lower-band plunge-ride verdict — the
// short-side mirror of BBRide.
type BBShortRide struct {
	Ride        bool    `json:"ride"`
	Windows     int     `json:"windows"`
	LowerBand   float64 `json:"lower_band"`
	VolumeSurge bool    `json:"volume_surge"`
}

// bbRideWalk is the shared trailing-streak engine for both ride sides.
// barQualifies(b, band) combines the candle-sign and band-hug test for one
// closed 15m bar; bandOf computes that bar's own last-20-close band. A
// non-qualifying bar or a doji stops the count; volume surge = any counted
// bar trading ≥1.5× the average volume of its own prior 20 bars.
func bbRideWalk(data *market.Data, barQualifies func(b market.KlineBar, band float64) bool, bandOf func([]float64) float64) (int, bool, float64) {
	windows := 0
	surge := false
	band := 0.0
	tf, ok := data.TimeframeData["15m"]
	if !ok || tf == nil || len(tf.Klines) < 8 {
		return windows, surge, band
	}
	bars := tf.Klines[:len(tf.Klines)-1] // closed bars only — the forming bar never counts
	// Current band from the most recent `period` closed closes.
	if n := len(bars); n >= int(bbRidePeriod) {
		band = bandOf(closesOf(bars[n-int(bbRidePeriod):]))
	}
	for i := len(bars) - 1; i >= 0 && windows < 12; i-- {
		b := bars[i]
		if i < int(bbRidePeriod)-1 {
			break // not enough history for this bar's own band
		}
		if !barQualifies(b, bandOf(closesOf(bars[i-int(bbRidePeriod)+1:i+1]))) {
			break
		}
		windows++
		if !surge && i >= bbRideVolumeAvg {
			avg := 0.0
			for _, pb := range bars[i-bbRideVolumeAvg : i] {
				avg += pb.Volume
			}
			avg /= bbRideVolumeAvg
			if avg > 0 && b.Volume >= bbRideVolMult*avg {
				surge = true
			}
		}
	}
	return windows, surge, band
}

func computeBBRide(data *market.Data) *BBRide {
	if data == nil {
		return nil
	}
	if tf, ok := data.TimeframeData["15m"]; !ok || tf == nil || len(tf.Klines) < 8 {
		return nil
	}
	w, surge, band := bbRideWalk(data, func(b market.KlineBar, upper float64) bool {
		return b.Close > b.Open && b.High >= upper
	}, bollUpper)
	return &BBRide{
		Ride:        w >= bbRideMinBars && surge,
		Windows:     w,
		UpperBand:   band,
		VolumeSurge: surge,
	}
}

func computeBBShortRide(data *market.Data) *BBShortRide {
	if data == nil {
		return nil
	}
	if tf, ok := data.TimeframeData["15m"]; !ok || tf == nil || len(tf.Klines) < 8 {
		return nil
	}
	w, surge, band := bbRideWalk(data, func(b market.KlineBar, lower float64) bool {
		return b.Close < b.Open && b.Low <= lower
	}, bollLower)
	return &BBShortRide{
		Ride:        w >= bbRideMinBars && surge,
		Windows:     w,
		LowerBand:   band,
		VolumeSurge: surge,
	}
}

func closesOf(bars []market.KlineBar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

// bollUpper returns SMA(period) + 2σ (population) over the given closes.
func bollUpper(closes []float64) float64 {
	return bollAt(closes, 1)
}

// bollLower returns SMA(period) − 2σ (population) — the plunge-ride floor.
func bollLower(closes []float64) float64 {
	return bollAt(closes, -1)
}

func bollAt(closes []float64, sign float64) float64 {
	n := float64(len(closes))
	if n == 0 {
		return 0
	}
	sum := 0.0
	for _, c := range closes {
		sum += c
	}
	mean := sum / n
	var2 := 0.0
	for _, c := range closes {
		d := c - mean
		var2 += d * d
	}
	sd := math.Sqrt(var2 / n)
	return mean + sign*bbRideMult*sd
}
