package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFactorQuantPeakEquityHistoryBoundary(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "equity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	start := time.Date(2026, 11, 1, 2, 0, 0, 0, time.FixedZone("UTC+8", 8*3600))
	for _, tc := range []struct {
		name, trace, prompt string
		want                float64
	}{
		{"no marker", "AI decision", "AI system", 0},
		{"rebalance without orders", "因子量化调仓（模拟盘，仅模拟盘，未调用大模型）", "", 14000},
		{"skip", "因子量化 · 未调仓（模拟盘）", "", 14000},
		{"risk", "因子量化 · 组合回撤预警", "", 14000},
		{"AI with similar summary", "因子量化 · AI generated text", "AI system", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, rec := range []*DecisionRecord{
				{TraderID: tc.name, Timestamp: start.Add(-time.Hour), CoTTrace: "AI decision", SystemPrompt: "AI system"},
				{TraderID: "other", Timestamp: start.Add(-time.Hour), CoTTrace: "因子量化 · 未调仓"},
				{TraderID: tc.name, Timestamp: start.Add(time.Hour), CoTTrace: tc.trace, SystemPrompt: tc.prompt}, // insert later marker first
				{TraderID: tc.name, Timestamp: start, CoTTrace: tc.trace, SystemPrompt: tc.prompt},
			} {
				if err := st.Decision().LogDecision(rec); err != nil {
					t.Fatal(err)
				}
			}
			for _, snap := range []*EquitySnapshot{
				{TraderID: tc.name, Timestamp: start.Add(-time.Second), TotalEquity: 50000},
				{TraderID: tc.name, Timestamp: start, TotalEquity: 14000}, // inclusive boundary, before the later marker
				{TraderID: tc.name, Timestamp: start.Add(2 * time.Hour), TotalEquity: 10000},
				{TraderID: "other", Timestamp: start, TotalEquity: 999999},
			} {
				if err := st.Equity().Save(snap); err != nil {
					t.Fatal(err)
				}
			}
			peak, since, err := st.Equity().FactorQuantPeakEquity(tc.name)
			if err != nil || peak != tc.want || (tc.want == 0 && !since.IsZero()) || (tc.want != 0 && !since.Equal(start)) {
				t.Fatalf("peak=%v since=%v err=%v, want peak=%v start=%v", peak, since, err, tc.want, start)
			}
		})
	}
	peak, since, err := st.Equity().FactorQuantPeakEquity("unknown")
	if err != nil || peak != 0 || !since.IsZero() {
		t.Fatalf("unknown trader: peak=%v since=%v err=%v", peak, since, err)
	}
	if err := st.Decision().LogDecision(&DecisionRecord{TraderID: "no snapshots", Timestamp: start, CoTTrace: "因子量化 · 未调仓"}); err != nil {
		t.Fatal(err)
	}
	peak, since, err = st.Equity().FactorQuantPeakEquity("no snapshots")
	if err != nil || peak != 0 || !since.Equal(start) {
		t.Fatalf("empty fq history: peak=%v since=%v err=%v", peak, since, err)
	}
}
