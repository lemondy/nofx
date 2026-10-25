package store

import "math"

// Defaults are deliberately independent of leverage: these are price moves.
// A 2× hourly ATR move inside five minutes is exceptional; the 3% fallback
// avoids reacting to sub-percent noise when the volatility feed is unavailable.
const (
	// 2026-10-25 replay: turnover filters dead markets, not manipulation;
	// 30M excluded real candidates (MYX/PIEVERSE/SAGA: ~2.9/3.2/7.8M).
	DefaultMinQuoteVolume24hMillions  = 5.0
	DefaultVolSpike5mMult             = 8.0
	DefaultVolSpike15mMult            = 6.0
	DefaultVolSpikeMult               = 5.0
	DefaultVolSpikeHoldoffMinutes     = 30.0
	DefaultOISurge15mPct              = 8.0
	DefaultOISurge1hPct               = 15.0
	DefaultFastWindowMinutes          = 5.0
	DefaultFastAdverseATRMult         = 2.0
	DefaultFastFavorableATRMult       = 2.0
	DefaultFastAdverseReduceFrac      = 0.5
	DefaultFastAdverseCooldownMinutes = 30.0
	DefaultFastFallbackPct            = 3.0
)

func volatileKnob(v, def float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return def
	}
	return antiChaseKnob(v, def)
}
func (r RiskControlConfig) EffectiveMinQuoteVolume24hMillions() float64 {
	return volatileKnob(r.MinQuoteVolume24hMillions, DefaultMinQuoteVolume24hMillions)
}
func (r RiskControlConfig) EffectiveThinOIFullSizeMillions(minOI float64) float64 {
	return volatileKnob(r.ThinOIFullSizeMillions, 5*minOI)
}
func (r RiskControlConfig) EffectiveThinOIMinSizeFactor() float64 {
	return math.Min(1, volatileKnob(r.ThinOIMinSizeFactor, .3))
}
func (r RiskControlConfig) EffectiveVolSpikeMult() float64 {
	return volatileKnob(r.VolSpikeMult, DefaultVolSpikeMult)
}
func (r RiskControlConfig) EffectiveVolSpike5mMult() float64 {
	return volatileKnob(r.VolSpike5mMult, DefaultVolSpike5mMult)
}
func (r RiskControlConfig) EffectiveVolSpike15mMult() float64 {
	return volatileKnob(r.VolSpike15mMult, DefaultVolSpike15mMult)
}

// Unlike threshold knobs, an explicit zero disables hold-off. A pointer
// distinguishes it from an omitted field in strategies saved before this gate.
func (r RiskControlConfig) EffectiveVolSpikeHoldoffMinutes() float64 {
	if r.VolSpikeHoldoffMinutes == nil || math.IsNaN(*r.VolSpikeHoldoffMinutes) || math.IsInf(*r.VolSpikeHoldoffMinutes, 0) {
		return DefaultVolSpikeHoldoffMinutes
	}
	return math.Max(0, *r.VolSpikeHoldoffMinutes)
}
func (r RiskControlConfig) EffectiveOISurge15mPct() float64 {
	return volatileKnob(r.OISurge15mPct, DefaultOISurge15mPct)
}
func (r RiskControlConfig) EffectiveOISurge1hPct() float64 {
	return volatileKnob(r.OISurge1hPct, DefaultOISurge1hPct)
}
func (r RiskControlConfig) EffectiveFastWindowMinutes() float64 {
	return math.Min(120, volatileKnob(r.FastWindowMinutes, DefaultFastWindowMinutes))
}
func (r RiskControlConfig) EffectiveFastAdverseATRMult() float64 {
	return volatileKnob(r.FastAdverseATRMult, DefaultFastAdverseATRMult)
}
func (r RiskControlConfig) EffectiveFastFavorableATRMult() float64 {
	return volatileKnob(r.FastFavorableATRMult, DefaultFastFavorableATRMult)
}
func (r RiskControlConfig) EffectiveFastAdverseReduceFrac() float64 {
	return math.Min(.75, volatileKnob(r.FastAdverseReduceFrac, DefaultFastAdverseReduceFrac))
}
func (r RiskControlConfig) EffectiveFastAdverseCooldownMinutes() float64 {
	return volatileKnob(r.FastAdverseCooldownMinutes, DefaultFastAdverseCooldownMinutes)
}
func (r RiskControlConfig) EffectiveFastFallbackPct() float64 {
	return volatileKnob(r.FastFallbackPct, DefaultFastFallbackPct)
}
func (r RiskControlConfig) EffectiveThinOIMarkStopMillions(minOI float64) float64 {
	return volatileKnob(r.ThinOIMarkStopMillions, 3*minOI)
}
func (r RiskControlConfig) EffectiveSpreadGateFailClosedThin() bool {
	return r.SpreadGateFailClosedThin == nil || *r.SpreadGateFailClosedThin
}
