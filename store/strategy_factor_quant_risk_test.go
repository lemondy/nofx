package store

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFactorQuantRiskConfig(t *testing.T) {
	for _, market := range []string{"crypto", "us"} {
		var cfg FactorQuantConfig
		if err := json.Unmarshal([]byte(`{"market":"`+market+`"}`), &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.EffectiveMaxDrawdownPct() != 0.25 || cfg.EffectiveDrawdownWarnPct() != 0.15 || cfg.EffectiveDailyLossPct() != 0.07 {
			t.Fatalf("%s defaults: %+v", market, cfg)
		}
		cfg.MaxDrawdownPct, cfg.DrawdownWarnPct, cfg.DailyLossPct = fqf(0), fqf(0), fqf(0)
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var back FactorQuantConfig
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if back.Validate() != nil || back.EffectiveMaxDrawdownPct() != 0 || back.EffectiveDrawdownWarnPct() != 0 || back.EffectiveDailyLossPct() != 0 {
			t.Fatalf("%s zero lost: %s", market, raw)
		}
	}
	var nilCfg *FactorQuantConfig
	if nilCfg.EffectiveMaxDrawdownPct() != 0.25 || nilCfg.EffectiveDrawdownWarnPct() != 0.15 || nilCfg.EffectiveDailyLossPct() != 0.07 {
		t.Fatal("nil config risk defaults")
	}
	for _, field := range []string{"max_drawdown_pct", "drawdown_warn_pct", "daily_loss_pct"} {
		for _, value := range []float64{-0.01, 1, 1.01, math.NaN(), math.Inf(1), math.Inf(-1)} {
			cfg := FactorQuantConfig{Market: "crypto"}
			switch field {
			case "max_drawdown_pct":
				cfg.MaxDrawdownPct = fqf(value)
			case "drawdown_warn_pct":
				cfg.DrawdownWarnPct = fqf(value)
			case "daily_loss_pct":
				cfg.DailyLossPct = fqf(value)
			}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("%s=%v: %v", field, value, err)
			}
		}
	}
	for _, cfg := range []FactorQuantConfig{
		{Market: "crypto", DrawdownWarnPct: fqf(0.25)}, // effective default cap
		{Market: "us", MaxDrawdownPct: fqf(0.15)},      // effective default warning
		{Market: "us", DrawdownWarnPct: fqf(0.3), MaxDrawdownPct: fqf(0.2)},
		{Market: "crypto", KillResetID: strings.Repeat("复", 65)},
		{Market: "crypto", KillResetID: "reset\x00id"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("invalid risk config accepted: %+v", cfg)
		}
	}
	for _, cfg := range []FactorQuantConfig{
		{Market: "us", MaxDrawdownPct: fqf(0), DrawdownWarnPct: fqf(0.9)},
		{Market: "us", MaxDrawdownPct: fqf(0.01), DrawdownWarnPct: fqf(0)},
		{Market: "crypto", KillResetID: strings.Repeat("复", 64)},
	} {
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid risk config rejected: %v", err)
		}
	}
}

func TestFactorQuantRiskStateMigrationAndSetters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "risk.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an existing installation whose table predates risk columns.
	db := st.GormDB()
	if err := db.Migrator().DropTable(&FactorQuantState{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE factor_quant_state (trader_id text PRIMARY KEY, last_as_of text NOT NULL DEFAULT '')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO factor_quant_state (trader_id, last_as_of) VALUES ('legacy', '2026-10-31')").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.FactorQuant().initTables(); err != nil {
		t.Fatal(err)
	}
	row, err := st.FactorQuant().Get("legacy")
	if err != nil || row.LastAsOf != "2026-10-31" || row.PeakEquity != 0 || row.DayStartDate != "" || row.HaltKind != "" || row.WarnActive || row.KillResetApplied != "" {
		t.Fatalf("migrated defaults=%+v %v", row, err)
	}
	s := st.FactorQuant()
	steps := []func() error{
		func() error { return s.SetPeakEquity("t1", 12000) },
		func() error { return s.SetDayStart("t1", "2026-11-01", 11000) },
		func() error { return s.SetHalt("t1", "drawdown", "", "回撤熔断", 123) },
		func() error { return s.SetWarnActive("t1", true) },
		func() error { return s.SetRebalanced("t1", "2026-10-31", "run", "rule", time.Now()) },
		func() error { return s.SetRetry("t1", "2026-10-31", 2) },
		func() error { return s.SetSkipKey("t1", "halt|drawdown|123") },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s = st.FactorQuant()
	row, err = s.Get("t1")
	if err != nil || row.PeakEquity != 12000 || row.DayStartEquity != 11000 || row.DayStartDate != "2026-11-01" || row.HaltKind != "drawdown" || row.HaltedAt != 123 || row.HaltReason != "回撤熔断" || !row.WarnActive || row.RetryAttempts != 2 || row.LastAsOf != "2026-10-31" || row.LastSkipKey != "halt|drawdown|123" {
		t.Fatalf("risk state lost/clobbered: %+v %v", row, err)
	}
	if err := s.ApplyKillReset("t1", "reset-1", 9000, true); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Get("t1")
	if row.PeakEquity != 9000 || row.HaltKind != "" || row.HaltedAt != 0 || row.HaltReason != "" || row.WarnActive || row.KillResetApplied != "reset-1" || row.DayStartEquity != 11000 || row.RetryAttempts != 2 || row.LastAsOf != "2026-10-31" {
		t.Fatalf("reset clobbered unrelated columns: %+v", row)
	}
	if err := s.SetHalt("t1", "daily", "2026-11-01", "日亏损", 456); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyKillReset("t1", "reset-2", 8500, false); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Get("t1")
	if row.HaltKind != "daily" || row.HaltDay != "2026-11-01" || row.HaltReason != "日亏损" || row.HaltedAt != 456 || row.KillResetApplied != "reset-2" {
		t.Fatalf("reset cleared daily halt: %+v", row)
	}
	if err := s.SetHalt("t1", "", "", "", 0); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Get("t1")
	if row.HaltKind != "" || row.HaltDay != "" || row.HaltedAt != 0 || row.PeakEquity != 8500 || row.DayStartEquity != 11000 {
		t.Fatalf("clear halt clobbered baselines: %+v", row)
	}
	// Every narrow setter can also create a row with defaults for other fields.
	for id, create := range map[string]func() error{
		"day":   func() error { return s.SetDayStart("day", "2026-11-01", 100) },
		"halt":  func() error { return s.SetHalt("halt", "daily", "2026-11-01", "亏损", 1) },
		"warn":  func() error { return s.SetWarnActive("warn", true) },
		"reset": func() error { return s.ApplyKillReset("reset", "r", 100, false) },
	} {
		if err := create(); err != nil {
			t.Fatal(err)
		}
		row, err := s.Get(id)
		if err != nil || row == nil || row.LastAsOf != "" || row.RetryAttempts != 0 {
			t.Fatalf("%s upsert defaults: %+v %v", id, row, err)
		}
	}
}
