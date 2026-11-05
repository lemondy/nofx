package store

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestFactorQuantUSShortConfig(t *testing.T) {
	var absent *FactorQuantConfig
	for _, cfg := range []*FactorQuantConfig{absent, {Market: "us"}} {
		if cfg.EffectiveUSShortMode() != "off" || cfg.EffectiveUSShortFeeBps() != 4 {
			t.Fatalf("short defaults: %+v", cfg)
		}
	}
	for _, mode := range []string{"", "off", "paper"} {
		for _, fee := range []*float64{nil, fqf(0), fqf(4), fqf(100)} {
			cfg := &FactorQuantConfig{Market: "us", USShortMode: mode, USShortFeeBps: fee}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("valid short config %+v: %v", cfg, err)
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var restored FactorQuantConfig
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			want := 4.0
			if fee != nil {
				want = *fee
			}
			if restored.EffectiveUSShortFeeBps() != want || (restored.USShortFeeBps == nil) != (fee == nil) {
				t.Fatalf("fee default / explicit zero lost: %s", data)
			}
		}
	}
	for name, cfg := range map[string]FactorQuantConfig{
		"live":         {Market: "us", USShortMode: "live"},
		"unknown":      {Market: "us", USShortMode: "yes"},
		"crypto paper": {Market: "crypto", USShortMode: "paper"},
		"crypto off":   {Market: "crypto", USShortMode: "off"},
		"crypto fee":   {Market: "crypto", USShortFeeBps: fqf(0)},
		"negative":     {Market: "us", USShortFeeBps: fqf(-1)},
		"too high":     {Market: "us", USShortFeeBps: fqf(100.1)},
		"NaN":          {Market: "us", USShortFeeBps: fqf(math.NaN())},
		"infinite":     {Market: "us", USShortFeeBps: fqf(math.Inf(1))},
	} {
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "us_short_") {
			t.Errorf("%s: expected short validation error, got %v", name, err)
		}
	}
}
