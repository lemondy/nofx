package market

import "math"

// WilderATRSeries returns an index-aligned ATR series. The first complete
// value, at index period, is the mean of TR[1..period]; bar zero has no
// previous close and is excluded. Earlier entries (and insufficient or
// mismatched inputs) are zero, matching calculateATR's unavailable value.
func WilderATRSeries(high, low, close []float64, period int) []float64 {
	out := make([]float64, len(close))
	if period <= 0 || len(close) <= period || len(high) != len(close) || len(low) != len(close) {
		return out
	}
	trueRange := func(i int) float64 {
		return math.Max(high[i]-low[i], math.Max(math.Abs(high[i]-close[i-1]), math.Abs(low[i]-close[i-1])))
	}
	seed := 0.0
	for i := 1; i <= period; i++ {
		seed += trueRange(i)
	}
	out[period] = seed / float64(period)
	for i := period + 1; i < len(close); i++ {
		out[i] = (out[i-1]*float64(period-1) + trueRange(i)) / float64(period)
	}
	return out
}
