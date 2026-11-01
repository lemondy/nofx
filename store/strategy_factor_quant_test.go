package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fqf(v float64) *float64 { return &v }

func TestFactorQuantValidate(t *testing.T) {
	good := FactorQuantConfig{Market: "crypto"}
	if err := good.Validate(); err != nil {
		t.Fatalf("minimal crypto config rejected: %v", err)
	}
	if err := (&FactorQuantConfig{Market: "us"}).Validate(); err != nil {
		t.Fatalf("minimal us config rejected: %v", err)
	}
	var nilCfg *FactorQuantConfig
	if nilCfg.Validate() == nil {
		t.Fatal("nil config accepted")
	}
	bad := map[string]FactorQuantConfig{
		"market missing":      {},
		"market unknown":      {Market: "fx"},
		"horizon negative":    {Market: "crypto", Horizon: -1},
		"fraction negative":   {Market: "crypto", CapitalFraction: -0.1},
		"fraction above one":  {Market: "crypto", CapitalFraction: 1.01},
		"leverage too high":   {Market: "crypto", Leverage: 11},
		"leverage negative":   {Market: "crypto", Leverage: -1},
		"band one":            {Market: "crypto", RebalanceBandPct: 1},
		"band negative":       {Market: "crypto", RebalanceBandPct: -0.01},
		"min order negative":  {Market: "crypto", MinOrderUSDT: -1},
		"short stop negative": {Market: "crypto", ShortStopPct: fqf(-0.1)},
		"stale negative":      {Market: "crypto", MaxStaleDays: -1},
		"us fee negative":     {Market: "us", USFeeBps: fqf(-1)},
		"us fee above 100":    {Market: "us", USFeeBps: fqf(100.5)},
		"us slip negative":    {Market: "us", USSlippageBps: fqf(-0.1)},
		"us slip above 100":   {Market: "us", USSlippageBps: fqf(101)},
	}
	for name, cfg := range bad {
		cfg := cfg
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := FactorQuantConfig{Market: "crypto", Horizon: 3, CapitalFraction: 1, Leverage: 10, RebalanceBandPct: 0.99, MinOrderUSDT: 0, ShortStopPct: fqf(0), MaxStaleDays: 5, USFeeBps: fqf(100), USSlippageBps: fqf(0)}
	if err := ok.Validate(); err != nil {
		t.Fatalf("boundary values rejected: %v", err)
	}
}

func TestFactorQuantDefaults(t *testing.T) {
	c := &FactorQuantConfig{Market: "crypto"}
	if c.EffectiveHorizon() != 3 || c.EffectiveCapitalFraction() != 1 || c.EffectiveLeverage() != 3 ||
		c.EffectiveBand() != 0.10 || c.EffectiveMinOrderUSDT() != 10 || c.EffectiveShortStopPct() != 0.5 ||
		c.EffectiveMaxStaleDays(3) != 5 {
		t.Fatalf("crypto defaults wrong: %+v", c)
	}
	us := &FactorQuantConfig{Market: "us"}
	if us.EffectiveHorizon() != 0 {
		t.Fatalf("us horizon default = %d, want 0 (follow file)", us.EffectiveHorizon())
	}
	if us.EffectiveMaxStaleDays(5) != 7 {
		t.Fatalf("us max stale follows file horizon: %d", us.EffectiveMaxStaleDays(5))
	}
	if us.EffectiveUSFeeBps() != 10 || us.EffectiveUSSlippageBps() != 3 {
		t.Fatalf("us cost defaults: fee=%v slip=%v", us.EffectiveUSFeeBps(), us.EffectiveUSSlippageBps())
	}
	us2 := &FactorQuantConfig{Market: "us", USFeeBps: fqf(0), USSlippageBps: fqf(7.5)}
	if us2.EffectiveUSFeeBps() != 0 || us2.EffectiveUSSlippageBps() != 7.5 {
		t.Fatalf("explicit us costs (including 0) not honoured: %v %v", us2.EffectiveUSFeeBps(), us2.EffectiveUSSlippageBps())
	}
	c2 := &FactorQuantConfig{Market: "crypto", Horizon: 7, CapitalFraction: 0.4, Leverage: 2, RebalanceBandPct: 0.2, MinOrderUSDT: 25, ShortStopPct: fqf(0), MaxStaleDays: 9}
	if c2.EffectiveHorizon() != 7 || c2.EffectiveCapitalFraction() != 0.4 || c2.EffectiveLeverage() != 2 || c2.EffectiveBand() != 0.2 ||
		c2.EffectiveMinOrderUSDT() != 25 || c2.EffectiveShortStopPct() != 0 || c2.EffectiveMaxStaleDays(7) != 9 {
		t.Fatalf("explicit values not honoured: %+v", c2)
	}
}

func TestFactorQuantStrategyConfigValidation(t *testing.T) {
	cfg := &StrategyConfig{StrategyType: StrategyTypeFactorQuant}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "factor_quant") {
		t.Fatalf("factor_quant without config accepted: %v", err)
	}
	cfg.FactorQuantConfig = &FactorQuantConfig{Market: "crypto", Leverage: 99}
	if err := cfg.Validate(); err == nil {
		t.Fatal("bad factor_quant config accepted")
	}
	cfg.FactorQuantConfig = &FactorQuantConfig{Market: "crypto"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid factor_quant strategy rejected: %v", err)
	}
	// JSON round trip keeps the type, the pointer-valued stop and its explicit zero.
	cfg.FactorQuantConfig.ShortStopPct = fqf(0)
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back StrategyConfig
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.StrategyType != StrategyTypeFactorQuant || back.FactorQuantConfig == nil || back.FactorQuantConfig.ShortStopPct == nil || back.FactorQuantConfig.EffectiveShortStopPct() != 0 {
		t.Fatalf("round trip lost factor_quant config: %s", raw)
	}
}

func TestFactorQuantStatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fq.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.FactorQuant().Get("t1")
	if err != nil || got != nil {
		t.Fatalf("fresh state = %v %v", got, err)
	}
	at := time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)
	if err := st.FactorQuant().SetRebalanced("t1", "2026-11-01", "run-1", "rule-x", at); err != nil {
		t.Fatal(err)
	}
	if err := st.FactorQuant().SetSkipKey("t1", "status|2026-11-02|no_validated_factors"); err != nil {
		t.Fatal(err)
	}
	if err := st.FactorQuant().SetRebalanced("t1", "2026-11-04", "run-2", "rule-x", at.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st2, err := New(path) // simulated restart
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	got, err = st2.FactorQuant().Get("t1")
	if err != nil || got == nil {
		t.Fatalf("state lost across restart: %v %v", got, err)
	}
	if got.LastAsOf != "2026-11-04" || got.LastRunID != "run-2" || got.LastRule != "rule-x" {
		t.Fatalf("state = %+v", got)
	}
	if got.LastSkipKey != "status|2026-11-02|no_validated_factors" {
		t.Fatalf("skip key clobbered by SetRebalanced: %q", got.LastSkipKey)
	}
	if other, _ := st2.FactorQuant().Get("t2"); other != nil {
		t.Fatal("state leaked across traders")
	}
}

func TestFactorQuantFlattenConfig(t *testing.T) {
	for days := -2; days <= 61; days++ {
		cfg := &FactorQuantConfig{Market: "crypto", FlattenAfterDays: days}
		err := cfg.Validate()
		if (err == nil) != (days >= -1 && days <= 60) {
			t.Errorf("days=%d: validation=%v", days, err)
		}
	}
	for _, tc := range []struct {
		name              string
		cfg               FactorQuantConfig
		fileHorizon, want int
	}{
		{"crypto default", FactorQuantConfig{Market: "crypto"}, 3, 8},
		{"crypto ignores file horizon", FactorQuantConfig{Market: "crypto"}, 7, 8},
		{"us follows file", FactorQuantConfig{Market: "us"}, 5, 12},
		{"missing file fallback", FactorQuantConfig{Market: "us"}, 0, 5},
		{"configured horizon", FactorQuantConfig{Market: "us", Horizon: 7}, 3, 16},
		{"custom stale", FactorQuantConfig{Market: "crypto", MaxStaleDays: 9}, 3, 12},
		{"configured flatten", FactorQuantConfig{Market: "crypto", FlattenAfterDays: 60}, 3, 60},
		{"disabled", FactorQuantConfig{Market: "crypto", FlattenAfterDays: -1}, 3, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.EffectiveFlattenAfterDays(tc.fileHorizon); got != tc.want {
				t.Fatalf("effective flatten=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestFactorQuantRetryStateUpsertsAndClears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	// Retry can create a row; the other setters must preserve it.
	if err := st.FactorQuant().SetRetry("t1", "2026-10-31", 2); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.FactorQuant().SetRebalanced("t1", "2026-10-31", "run", "rule", now); err != nil {
		t.Fatal(err)
	}
	if err := st.FactorQuant().SetSkipKey("t1", "done"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	row, err := st.FactorQuant().Get("t1")
	if err != nil || row == nil || row.RetryAsOf != "2026-10-31" || row.RetryAttempts != 2 {
		t.Fatalf("retry lost on restart/other setters: %+v %v", row, err)
	}
	for _, retry := range []struct {
		asOf     string
		attempts int
	}{{"2026-10-31", 3}, {"", 0}} {
		if err := st.FactorQuant().SetRetry("t1", retry.asOf, retry.attempts); err != nil {
			t.Fatal(err)
		}
		got, err := st.FactorQuant().Get("t1")
		if err != nil || got == nil || got.RetryAsOf != retry.asOf || got.RetryAttempts != retry.attempts {
			t.Fatalf("retry update=%+v %v", got, err)
		}
		if got.LastAsOf != row.LastAsOf || got.LastRule != row.LastRule || got.LastRunID != row.LastRunID || got.RebalanceAt != row.RebalanceAt || got.LastSkipKey != row.LastSkipKey || got.UpdatedAt == 0 {
			t.Fatalf("SetRetry clobbered other columns: before=%+v after=%+v", row, got)
		}
	}
	if err := st.FactorQuant().SetSkipKey("defaults", "skip"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.FactorQuant().Get("defaults"); err != nil || got == nil || got.RetryAsOf != "" || got.RetryAttempts != 0 {
		t.Fatalf("retry column defaults: %+v %v", got, err)
	}
}
