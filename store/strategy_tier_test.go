package store

import (
	"math"
	"testing"
)

func TestStockConfigScanSizeTierDefaultsAndValidation(t *testing.T) {
	var nilCfg *StockConfig
	if got := nilCfg.EffectiveScanSizeTier(); got != StockScanSizeTierAll {
		t.Fatalf("nil EffectiveScanSizeTier() = %q, want %q", got, StockScanSizeTierAll)
	}

	for _, tc := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{"", StockScanSizeTierAll, true},
		{"  ", StockScanSizeTierAll, true},
		{"all", StockScanSizeTierAll, true},
		{"ALL", StockScanSizeTierAll, true},
		{"mega_large", StockScanSizeTierMegaLarge, true},
		{"MEGA_LARGE", StockScanSizeTierMegaLarge, true},
		{"mid_small", StockScanSizeTierMidSmall, true},
		{" Mid_Small ", StockScanSizeTierMidSmall, true},
		{"micro", "micro", false},
		{"etf", "etf", false},
	} {
		cfg := &StockConfig{Symbols: []string{"AAPLBUSDT"}, ScanSizeTier: tc.raw}
		if got := cfg.EffectiveScanSizeTier(); got != tc.want {
			t.Fatalf("raw=%q EffectiveScanSizeTier() = %q, want %q", tc.raw, got, tc.want)
		}
		err := cfg.Validate()
		if (err == nil) != tc.ok {
			t.Fatalf("raw=%q Validate() err=%v, want ok=%v", tc.raw, err, tc.ok)
		}
	}
}

func TestFactorQuantConfigSizeTierDefaultsAndValidation(t *testing.T) {
	var nilCfg *FactorQuantConfig
	if got := nilCfg.EffectiveSizeTier(); got != FactorQuantSizeTierAll {
		t.Fatalf("nil EffectiveSizeTier() = %q, want %q", got, FactorQuantSizeTierAll)
	}

	for _, tc := range []struct {
		tier string
		cap  float64
		want string
		ok   bool
	}{
		{"", 0, FactorQuantSizeTierAll, true},
		{"all", 0, FactorQuantSizeTierAll, true},
		{"MEGA_LARGE", 0, FactorQuantSizeTierMegaLarge, true},
		{"mid_small", 25, FactorQuantSizeTierMidSmall, true},
		{"", 100, FactorQuantSizeTierAll, true},
		{"", 0.01, FactorQuantSizeTierAll, true},
		{"invalid", 0, "invalid", false},
		{"", -1, FactorQuantSizeTierAll, false},
		{"", 100.1, FactorQuantSizeTierAll, false},
		{"", math.NaN(), FactorQuantSizeTierAll, false},
		{"", math.Inf(1), FactorQuantSizeTierAll, false},
	} {
		cfg := &FactorQuantConfig{
			Market:                 FactorQuantMarketUS,
			SizeTier:               tc.tier,
			MaxMidSmallExposurePct: tc.cap,
		}
		if got := cfg.EffectiveSizeTier(); got != tc.want {
			t.Fatalf("tier=%q EffectiveSizeTier() = %q, want %q", tc.tier, got, tc.want)
		}
		err := cfg.Validate()
		if (err == nil) != tc.ok {
			t.Fatalf("tier=%q cap=%v Validate() err=%v, want ok=%v", tc.tier, tc.cap, err, tc.ok)
		}
	}
}
