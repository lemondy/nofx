package store

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestAccountDrawdownBasisValidation(t *testing.T) {
	for _, tc := range []struct {
		basis string
		valid bool
	}{
		{"", true}, {"initial", true}, {"peak", true},
		{"Peak", false}, {"initial ", false}, {"equity", false},
	} {
		t.Run("basis="+tc.basis, func(t *testing.T) {
			cfg := StrategyConfig{RiskControl: RiskControlConfig{AccountDrawdownBasis: tc.basis}}
			err := cfg.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("basis %q validation = %v", tc.basis, err)
			}
			if !tc.valid && !strings.Contains(err.Error(), "account_drawdown_basis") {
				t.Fatalf("missing field name in error: %v", err)
			}
		})
	}
}

func TestAccountDrawdownBasisLegacyJSON(t *testing.T) {
	var legacy RiskControlConfig
	if err := json.Unmarshal([]byte(`{"account_max_drawdown_pct":20}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.AccountDrawdownBasis != "" || legacy.AccountMaxDrawdownPct != 20 {
		t.Fatalf("legacy configuration changed: %+v", legacy)
	}
	blank, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blank), "account_drawdown_basis") {
		t.Fatal("unset field changed legacy JSON/config hash")
	}
	legacy.AccountDrawdownBasis = "initial"
	explicit, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Replace(string(explicit), `,"account_drawdown_basis":"initial"`, "", 1) != string(blank) {
		t.Fatal("adding the optional basis changed other serialized fields")
	}
}

func TestMaxAccountEquity(t *testing.T) {
	st := newReview07Store(t)
	for _, row := range []Trader{
		{ID: "peer", UserID: "u", ExchangeID: "account"},
		{ID: "other-account", UserID: "u", ExchangeID: "other"},
		{ID: "other-user", UserID: "other", ExchangeID: "account"},
	} {
		if err := st.Trader().Create(&row); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []EquitySnapshot{
		{TraderID: "own", TotalEquity: 11000},
		{TraderID: "peer", TotalEquity: 13000},
		{TraderID: "peer", TotalEquity: 12000},
		{TraderID: "other-account", TotalEquity: 90000},
		{TraderID: "other-user", TotalEquity: 80000},
	} {
		if err := st.Equity().Save(&row); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, trader, user, account string
		want                        float64
	}{
		{"account peers", "own", "u", "account", 13000},
		{"identity fallback", "own", "u", "", 11000},
		{"unknown trader", "missing", "u", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.Equity().MaxAccountEquity(tc.trader, tc.user, tc.account)
			if err != nil || got != tc.want {
				t.Fatalf("peak = %v, %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestAccountPeakConcurrentUpdatesAreMonotonic(t *testing.T) {
	st := newReview07Store(t)
	if _, err := st.RiskState().UpdatePeak("account", 10000); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, equity := range []float64{13000, 9000, 14000, 11000, 15000} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.RiskState().UpdatePeak("account", equity); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	peak, err := st.RiskState().UpdatePeak("account", 8000)
	if err != nil || peak != 15000 {
		t.Fatalf("peak = %v, %v, want 15000", peak, err)
	}
}
