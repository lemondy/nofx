package store

import (
	"math"
	"testing"
	"time"

	"nofx/fundsledger"
)

func TestReview20261029TWRDrawdownProvenance(t *testing.T) {
	base := time.Date(2026, 10, 25, 23, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		equities     []float64
		flow         float64 // at snapshot 2
		want         float64
		peak, trough int
		ok           bool
	}{
		{"withdrawal_and_loss", []float64{100, 110, 60, 58}, -45, (1 - (105.0/110)*(58.0/60)) * 100, 1, 3, true},
		{"deposit_hides_loss", []float64{100, 80, 180, 162}, 100, 28, 0, 3, true},
		{"index_peak_below_raw_peak", []float64{100, 110, 60, 80, 72}, -45, 10, 3, 4, true},
		{"earlier_max_survives_new_peak", []float64{100, 80, 120, 110}, 0, 20, 0, 1, true},
		{"first_usable_opening", []float64{0, 100, 90}, 0, 10, 1, 2, true},
		{"withdrawal_without_loss", []float64{100, 110, 65}, -45, 0, 0, 0, true},
		{"rising", []float64{100, 110, 120}, 0, 0, 0, 0, true},
		{"flat", []float64{100, 100}, 0, 0, 0, 0, true},
		{"no_interval", []float64{100}, 0, 0, 0, 0, false},
		{"no_positive_opening", []float64{0, 10}, 0, 0, 0, 0, false},
		{"empty", nil, 0, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			points := make([]equityPoint, len(tc.equities))
			for i, eq := range tc.equities {
				points[i] = equityPoint{At: base.Add(time.Duration(i) * time.Hour), Equity: eq}
			}
			var flows []cashFlow
			if tc.flow != 0 {
				flows = []cashFlow{{At: points[2].At, Amount: tc.flow}}
			}
			dd, ok := twrMaxDrawdown(points, flows)
			if ok != tc.ok || math.Abs(dd.Pct-tc.want) > 1e-9 {
				t.Fatalf("dd=%+v ok=%v, want %.10f ok=%v", dd, ok, tc.want, tc.ok)
			}
			if tc.want == 0 {
				if dd != (twrDrawdown{}) {
					t.Fatalf("zero drawdown must have empty provenance: %+v", dd)
				}
				return
			}
			if !dd.PeakAt.Equal(points[tc.peak].At) || !dd.TroughAt.Equal(points[tc.trough].At) ||
				dd.PeakEquity != tc.equities[tc.peak] || dd.TroughEquity != tc.equities[tc.trough] {
				t.Fatalf("wrong index peak/trough provenance: %+v", dd)
			}
		})
	}
}

func TestReview20261029StatsDrawdownProvenance(t *testing.T) {
	st := newTestStore(t)
	for _, row := range []interface{}{
		&Trader{ID: "twr", ExchangeID: "e"},
		&Exchange{ID: "e", ExchangeType: "binance"},
		&TraderPosition{TraderID: "fallback", Status: "CLOSED", RealizedPnL: -5},
	} {
		if err := st.GormDB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	for _, trader := range []string{"twr", "flat"} {
		for i, eq := range []float64{100, 110, 60, 58} {
			if trader == "flat" {
				eq = 100
			}
			if err := st.Equity().Save(&EquitySnapshot{TraderID: trader, Timestamp: base.Add(time.Duration(i) * time.Hour), TotalEquity: eq}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := st.AccountIncome().Save("e", []fundsledger.Income{{TranID: "withdrawal", IncomeType: "WITHDRAW", Asset: "USDT", Amount: -45, TimeMS: base.Add(2 * time.Hour).UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	for _, days := range []int{0, 30} {
		stats, err := st.Position().GetRollingStats("twr", 100, days)
		if err != nil {
			t.Fatal(err)
		}
		want := (1 - (105.0/110)*(58.0/60)) * 100
		if stats.MaxDrawdownBasis != "equity_twr" || math.Abs(stats.MaxDrawdownPct-want) > 1e-9 ||
			stats.MaxDrawdownPeakAt == nil || stats.MaxDrawdownTroughAt == nil ||
			!stats.MaxDrawdownPeakAt.Equal(base.Add(time.Hour)) || !stats.MaxDrawdownTroughAt.Equal(base.Add(3*time.Hour)) ||
			stats.MaxDrawdownPeakEquity != 110 || stats.MaxDrawdownTroughEquity != 58 {
			t.Fatalf("window=%d stats=%+v", days, stats)
		}
		out := vfStatsJSON(t, stats)
		vfNumber(t, out, "max_drawdown_peak_equity", 110)
		vfNumber(t, out, "max_drawdown_trough_equity", 58)
		if out["max_drawdown_peak_at"] != base.Add(time.Hour).Format(time.RFC3339) ||
			out["max_drawdown_trough_at"] != base.Add(3*time.Hour).Format(time.RFC3339) {
			t.Fatalf("JSON timestamps: %+v", out)
		}
	}
	for _, tc := range []struct {
		trader, basis string
		want          float64
	}{{"flat", "equity_twr", 0}, {"fallback", "closed_trades", 5}, {"missing", "unavailable", 0}} {
		stats, err := st.Position().GetFullStats(tc.trader, 100)
		if err != nil {
			t.Fatal(err)
		}
		if stats.MaxDrawdownBasis != tc.basis || math.Abs(stats.MaxDrawdownPct-tc.want) > 1e-9 {
			t.Fatalf("stats=%+v, want basis=%s dd=%v", stats, tc.basis, tc.want)
		}
		out := vfStatsJSON(t, stats)
		for _, key := range []string{"max_drawdown_peak_at", "max_drawdown_trough_at", "max_drawdown_peak_equity", "max_drawdown_trough_equity"} {
			if _, exists := out[key]; exists {
				t.Fatalf("%s must omit %s: %+v", tc.trader, key, out)
			}
		}
	}
}
