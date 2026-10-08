package store

import (
	"fmt"
	"testing"
)

// review 2026-10-09 I: version stats ownership / sample size / cross-version.

func vsBool(b bool) *bool { return &b }

func seedVS(t *testing.T, st *Store, rows []TradeJournalDB) {
	t.Helper()
	if err := st.TradeJournal().initTables(); err != nil {
		t.Fatalf("journal init: %v", err)
	}
	for i := range rows {
		if err := st.TradeJournal().db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func TestVersionStatsOwnershipFilter(t *testing.T) {
	st := newTestStore(t)
	seedVS(t, st, []TradeJournalDB{
		{AIManaged: vsBool(true), TraderID: "T1", PositionID: 1, Symbol: "A", EntryTime: 1100, ExitTime: 1200, RealizedPnL: 2},
		{AIManaged: vsBool(false), TraderID: "T1", PositionID: 2, Symbol: "B", EntryTime: 1100, ExitTime: 1200, RealizedPnL: 50},
		{AIManaged: vsBool(false), TraderID: "T1", PositionID: 3, Symbol: "C", EntryTime: 1100, ExitTime: 1200, RealizedPnL: 50},
		{AIManaged: nil, TraderID: "T1", PositionID: 4, Symbol: "D", EntryTime: 1100, ExitTime: 1200, RealizedPnL: 50},
	})
	s, err := st.TradeJournal().StatsForWindow([]string{"T1"}, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if s.Trades != 1 || s.NetPnL != 2 || s.ExcludedManual != 2 || s.ExcludedUnattributed != 1 {
		t.Fatalf("unexpected stats: %+v", s)
	}
}

func TestVersionStatsLowSampleBoundary(t *testing.T) {
	for _, n := range []int{19, 20} {
		st := newTestStore(t)
		rows := make([]TradeJournalDB, n)
		for i := range rows {
			rows[i] = TradeJournalDB{AIManaged: vsBool(true), TraderID: "T1", PositionID: int64(i + 1), Symbol: fmt.Sprintf("S%d", i), EntryTime: 1100, ExitTime: 1200, RealizedPnL: 1}
		}
		seedVS(t, st, rows)
		s, err := st.TradeJournal().StatsForWindow([]string{"T1"}, 1000, 2000)
		if err != nil {
			t.Fatal(err)
		}
		if want := n < 20; s.LowSample != want || s.Trades != n {
			t.Fatalf("n=%d low_sample=%v trades=%d", n, s.LowSample, s.Trades)
		}
	}
}

func TestWilson95(t *testing.T) {
	lo, hi := wilson95(7, 10)
	if absF(lo-39.7) > 0.1 || absF(hi-89.2) > 0.1 {
		t.Fatalf("wilson(7,10) = %.2f–%.2f, want ≈39.7–89.2", lo, hi)
	}
	if lo, hi := wilson95(0, 0); lo != 0 || hi != 0 {
		t.Fatalf("wilson(0,0) = %v,%v", lo, hi)
	}
}

func TestVersionStatsCrossedVersion(t *testing.T) {
	st := newTestStore(t)
	seedVS(t, st, []TradeJournalDB{
		{AIManaged: vsBool(true), TraderID: "T1", PositionID: 1, Symbol: "A", EntryTime: 1100, ExitTime: 1500, RealizedPnL: 1},
		{AIManaged: vsBool(true), TraderID: "T1", PositionID: 2, Symbol: "B", EntryTime: 1900, ExitTime: 2500, RealizedPnL: 1},
	})
	s, err := st.TradeJournal().StatsForWindow([]string{"T1"}, 1000, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if s.Trades != 2 || s.CrossedVersion != 1 {
		t.Fatalf("bounded window: %+v", s)
	}
	s, err = st.TradeJournal().StatsForWindow([]string{"T1"}, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Trades != 2 || s.CrossedVersion != 0 {
		t.Fatalf("open window: %+v", s)
	}
}
