package store

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func TestVolatileRiskDefaultsAndDisable(t *testing.T) {
	cases := []struct {
		field string
		get   func(RiskControlConfig) float64
		def   float64
	}{
		{"MinQuoteVolume24hMillions", RiskControlConfig.EffectiveMinQuoteVolume24hMillions, 5},
		{"ThinOIFullSizeMillions", func(r RiskControlConfig) float64 { return r.EffectiveThinOIFullSizeMillions(15) }, 75},
		{"ThinOIMinSizeFactor", RiskControlConfig.EffectiveThinOIMinSizeFactor, .3},
		{"VolSpike5mMult", RiskControlConfig.EffectiveVolSpike5mMult, 8},
		{"VolSpike15mMult", RiskControlConfig.EffectiveVolSpike15mMult, 6},
		{"VolSpikeMult", RiskControlConfig.EffectiveVolSpikeMult, 5},
		{"OISurge15mPct", RiskControlConfig.EffectiveOISurge15mPct, 8},
		{"OISurge1hPct", RiskControlConfig.EffectiveOISurge1hPct, 15},
		{"FastWindowMinutes", RiskControlConfig.EffectiveFastWindowMinutes, 5},
		{"FastAdverseATRMult", RiskControlConfig.EffectiveFastAdverseATRMult, 2},
		{"FastFavorableATRMult", RiskControlConfig.EffectiveFastFavorableATRMult, 2},
		{"FastAdverseReduceFrac", RiskControlConfig.EffectiveFastAdverseReduceFrac, .5},
		{"FastAdverseCooldownMinutes", RiskControlConfig.EffectiveFastAdverseCooldownMinutes, 30},
		{"FastFallbackPct", RiskControlConfig.EffectiveFastFallbackPct, 3},
		{"ThinOIMarkStopMillions", func(r RiskControlConfig) float64 { return r.EffectiveThinOIMarkStopMillions(15) }, 45},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			for _, v := range []float64{0, -1, .6, math.NaN(), math.Inf(1)} {
				var r RiskControlConfig
				reflect.ValueOf(&r).Elem().FieldByName(tc.field).SetFloat(v)
				want := v
				if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					want = tc.def
				}
				if v < 0 {
					want = 0
				}
				if got := tc.get(r); got != want {
					t.Fatalf("input %g got %g want %g", v, got, want)
				}
			}
		})
	}
	var r RiskControlConfig
	if r.EffectiveThinOIFullSizeMillions(20) != 100 || r.EffectiveThinOIMarkStopMillions(20) != 60 {
		t.Fatal("defaults must follow candidate floor")
	}
	r.ThinOIMinSizeFactor, r.FastAdverseReduceFrac, r.FastWindowMinutes = 4, 4, 1000
	if r.EffectiveThinOIMinSizeFactor() != 1 || r.EffectiveFastAdverseReduceFrac() != .75 || r.EffectiveFastWindowMinutes() != 120 {
		t.Fatal("unsafe factor/fraction/window not capped")
	}
	if !r.EffectiveSpreadGateFailClosedThin() {
		t.Fatal("spread policy must default true")
	}
	off := false
	r.SpreadGateFailClosedThin = &off
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var copy RiskControlConfig
	if err := json.Unmarshal(b, &copy); err != nil || copy.EffectiveSpreadGateFailClosedThin() {
		t.Fatal("explicit false lost in JSON")
	}
}

func TestVolatileHoldoffOptionalDefaultAndJSON(t *testing.T) {
	if got := (RiskControlConfig{}).EffectiveVolSpikeHoldoffMinutes(); got != 30 {
		t.Fatal("omitted default", got)
	}
	for _, v := range []float64{0, -1, 15, math.NaN(), math.Inf(1)} {
		r := RiskControlConfig{VolSpikeHoldoffMinutes: &v}
		want := math.Max(0, v)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			want = 30
		}
		if got := r.EffectiveVolSpikeHoldoffMinutes(); got != want {
			t.Fatal(v, got, want)
		}
	}
	for _, raw := range []string{`{}`, `{"vol_spike_holdoff_minutes":null}`, `{"vol_spike_holdoff_minutes":0}`, `{"vol_spike_holdoff_minutes":-1}`, `{"vol_spike_holdoff_minutes":15}`} {
		var r, copy RiskControlConfig
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &copy); err != nil {
			t.Fatal(err)
		}
		if copy.EffectiveVolSpikeHoldoffMinutes() != r.EffectiveVolSpikeHoldoffMinutes() || (r.VolSpikeHoldoffMinutes == nil) != (copy.VolSpikeHoldoffMinutes == nil) {
			t.Fatal("holdoff zero/omitted lost", raw, string(b))
		}
	}
}
