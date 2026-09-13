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

const (
	bbRidePeriod    = 20.0 // Bollinger length (SMA of closes)
	bbRideMult      = 2.0  // Bollinger width (σ multiplier)
	bbRideMinBars   = 3    // "连续3个窗口以上"
	bbRideVolMult   = 1.5  // "交易量暴增" — same bar as breakout volume confirmation
	bbRideVolumeAvg = 20   // volume average window per bar
)

// BBRide is the pre-computed 15m upper-band ride verdict.
type BBRide struct {
	Ride        bool    `json:"ride"`          // ride && volume_surge both hold
	Windows     int     `json:"windows"`       // trailing consecutive qualifying closed 15m bars
	UpperBand   float64 `json:"upper_band"`    // current upper Bollinger (20, 2σ), closed bars
	VolumeSurge bool    `json:"volume_surge"`  // any qualifying bar's volume ≥1.5× its prior-20 average
}

// computeBBRide walks the CLOSED 15m bars backwards and counts trailing bars
// that are (a) bullish (close > open) and (b) touching the upper Bollinger
// band (bar high ≥ band computed over that bar's own last-20 closes). A
// non-qualifying bar or a doji stops the count. Volume surge = any counted
// bar trading ≥1.5× the average volume of its own prior 20 bars. Returns nil
// when no 15m data exists.
func computeBBRide(data *market.Data) *BBRide {
	if data == nil {
		return nil
	}
	tf, ok := data.TimeframeData["15m"]
	if !ok || tf == nil || len(tf.Klines) < 8 {
		return nil
	}
	bars := tf.Klines[:len(tf.Klines)-1] // closed bars only — the forming bar never counts

	r := &BBRide{}
	// Current upper band from the most recent `period` closed closes.
	if n := len(bars); n >= int(bbRidePeriod) {
		r.UpperBand = bollUpper(closesOf(bars[n-int(bbRidePeriod):]))
	}
	for i := len(bars) - 1; i >= 0 && r.Windows < 12; i-- {
		b := bars[i]
		if b.Close <= b.Open {
			break // not an up window (doji included)
		}
		if i < int(bbRidePeriod)-1 {
			break // not enough history for this bar's own band
		}
		upper := bollUpper(closesOf(bars[i-int(bbRidePeriod)+1 : i+1]))
		if b.High < upper {
			break // not hugging the upper band
		}
		r.Windows++
		if !r.VolumeSurge && i >= bbRideVolumeAvg {
			avg := 0.0
			for _, pb := range bars[i-bbRideVolumeAvg : i] {
				avg += pb.Volume
			}
			avg /= bbRideVolumeAvg
			if avg > 0 && b.Volume >= bbRideVolMult*avg {
				r.VolumeSurge = true
			}
		}
	}
	r.Ride = r.Windows >= bbRideMinBars && r.VolumeSurge
	return r
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
	return mean + bbRideMult*sd
}
