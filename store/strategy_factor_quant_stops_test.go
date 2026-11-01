package store

import (
	"math"
	"testing"
)

func TestFactorQuantStopBounds(t *testing.T) {
	for _, tc := range []struct {
		name            string
		value           float64
		longOK, shortOK bool
	}{
		{"disabled", 0, true, true},
		{"default", 0.2, true, true},
		{"below one", math.Nextafter(1, 0), true, true},
		{"one", 1, false, true},
		{"short maximum", 10, false, true},
		{"above maximum", math.Nextafter(10, 11), false, false},
		{"negative", -0.001, false, false},
		{"nan", math.NaN(), false, false},
		{"positive infinity", math.Inf(1), false, false},
		{"negative infinity", math.Inf(-1), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			long := &FactorQuantConfig{Market: "crypto", LongStopPct: fqf(tc.value)}
			short := &FactorQuantConfig{Market: "crypto", ShortStopPct: fqf(tc.value)}
			if err := long.Validate(); (err == nil) != tc.longOK {
				t.Fatalf("long %v: %v", tc.value, err)
			}
			if err := short.Validate(); (err == nil) != tc.shortOK {
				t.Fatalf("short %v: %v", tc.value, err)
			}
		})
	}
	var cfg *FactorQuantConfig
	if cfg.EffectiveLongStopPct() != 0.2 || cfg.EffectiveShortStopPct() != 0.2 {
		t.Fatal("nil config stop defaults")
	}
	cfg = &FactorQuantConfig{LongStopPct: fqf(0.1), ShortStopPct: fqf(0.3)}
	if cfg.EffectiveLongStopPct() != 0.1 || cfg.EffectiveShortStopPct() != 0.3 {
		t.Fatal("configured stop percentages ignored")
	}
}
