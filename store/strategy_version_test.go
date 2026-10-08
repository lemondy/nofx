package store

import (
	"encoding/json"
	"testing"
	"time"
)

// Version-record flow: create baseline → save change (diff recorded) →
// identical re-record deduped → another save diffs against the LATEST row.
func TestConfigVersionRecordAndDedup(t *testing.T) {
	st := newTestStore(t)
	s := st.Strategy()
	if err := s.initTables(); err != nil {
		t.Fatalf("init: %v", err)
	}

	old := `{"risk_control":{"max_positions":3,"min_confidence":75},"coin_source":{"source_type":"ai500"}}`
	next := `{"risk_control":{"max_positions":5,"min_confidence":75},"coin_source":{"source_type":"ai500"}}`
	final := `{"risk_control":{"max_positions":5,"min_confidence":80},"coin_source":{"source_type":"ai500"},"stats_window_days":14}`

	if err := s.RecordConfigChange("S1", old, 0, next, "save"); err != nil {
		t.Fatalf("record v2: %v", err)
	}
	// identical re-record must dedup (drift detector + save path race)
	if err := s.RecordConfigChange("S1", old, 0, next, "save"); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	// same config via a different old snapshot must also dedup
	if err := s.RecordConfigChange("S1", next, 0, next, "external"); err != nil {
		t.Fatalf("re-record same: %v", err)
	}
	if err := s.RecordConfigChange("S1", next, 0, final, "save"); err != nil {
		t.Fatalf("record v3: %v", err)
	}

	rows, err := s.ListConfigVersions("S1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows (baseline + 2 saves), got %d", len(rows))
	}
	if rows[0].Source != "baseline" {
		t.Fatalf("row0 source = %q, want baseline", rows[0].Source)
	}
	if rows[1].Source != "save" || rows[2].Source != "save" {
		t.Fatalf("save sources = %q/%q", rows[1].Source, rows[2].Source)
	}
	// risk hash mirrors the risk_control-only hash
	if rows[1].RiskHash == "" || rows[1].RiskHash == rows[1].ConfigHash {
		t.Fatalf("risk hash missing or equal to config hash: %q vs %q", rows[1].RiskHash, rows[1].ConfigHash)
	}

	// diff of the second save: old = latest row (max_positions:5 config),
	// new changes min_confidence + adds stats_window_days
	var diff []ConfigDiffEntry
	if err := json.Unmarshal([]byte(rows[2].Summary), &diff); err != nil {
		t.Fatalf("summary: %v", err)
	}
	paths := map[string]bool{}
	for _, d := range diff {
		paths[d.Path] = true
	}
	if !paths["risk_control.min_confidence"] || !paths["stats_window_days"] {
		t.Fatalf("diff paths missing: %v", diff)
	}
	if paths["risk_control.max_positions"] {
		t.Fatalf("max_positions must not appear in v3 diff (unchanged vs latest): %v", diff)
	}
}

// Lazy anchors: EnsureConfigVersionBaseline seeds exactly once; RecordConfigChange
// with no history and a known predecessor produces baseline+change.
func TestConfigVersionBaselineSeeding(t *testing.T) {
	st := newTestStore(t)
	s := st.Strategy()
	if err := s.initTables(); err != nil {
		t.Fatalf("init: %v", err)
	}

	cfg := `{"risk_control":{"max_positions":3}}`
	if err := s.EnsureConfigVersionBaseline("S2", cfg, "baseline", 12345); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if err := s.EnsureConfigVersionBaseline("S2", `{"risk_control":{"max_positions":9}}`, "baseline", 99999); err != nil {
		t.Fatalf("anchor 2: %v", err)
	}
	rows, err := s.ListConfigVersions("S2")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ChangedAt != 12345 {
		t.Fatalf("want exactly the first anchor row, got %+v", rows)
	}

	// no history + predecessor known → baseline seeded then diffed change
	if err := s.RecordConfigChange("S3", cfg, 1000, `{"risk_control":{"max_positions":4}}`, "external"); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, err = s.ListConfigVersions("S3")
	if err != nil {
		t.Fatalf("list S3: %v", err)
	}
	if len(rows) != 2 || rows[0].Source != "baseline" || rows[1].Source != "external" {
		t.Fatalf("want baseline+external, got %+v", rows)
	}
}

// Version stats: NET caliber (RealizedPnL − Fee), entry-time windowing, R
// from the planned stop.
func TestVersionStatsForWindow(t *testing.T) {
	st := newTestStore(t)
	if err := st.TradeJournal().initTables(); err != nil {
		t.Fatalf("journal init: %v", err)
	}
	now := time.Now().UnixMilli()
	rows := []TradeJournalDB{
		// inside window: win 2.0 gross − 0.2 fee = 1.8 net; R risk = |100−95|×2 = 10 → 0.18R
		{AIManaged: vsBool(true), TraderID: "T1", PositionID: 1, Symbol: "AUSDT", EntryTime: now - 1000, ExitTime: now, EntryPrice: 100, ExitPrice: 110, PlannedStopLoss: 95, Quantity: 2, RealizedPnL: 2.0, Fee: 0.2},
		// inside window: loss −1.0 gross − 0.1 fee = −1.1 net; R = −1.1/|50−51|×1 = −1.1
		{AIManaged: vsBool(true), TraderID: "T1", PositionID: 2, Symbol: "BUSDT", EntryTime: now - 900, ExitTime: now, EntryPrice: 50, ExitPrice: 49, PlannedStopLoss: 51, Quantity: 1, RealizedPnL: -1.0, Fee: 0.1},
		// outside window (entry too early)
		{AIManaged: vsBool(true), TraderID: "T1", PositionID: 3, Symbol: "CUSDT", EntryTime: now - 99999, ExitTime: now, EntryPrice: 10, ExitPrice: 11, RealizedPnL: 5.0, Fee: 0.1},
		// other trader, inside window — must count when included
		{AIManaged: vsBool(true), TraderID: "T2", PositionID: 4, Symbol: "AUSDT", EntryTime: now - 800, ExitTime: now, EntryPrice: 10, ExitPrice: 11, RealizedPnL: 1.0, Fee: 0.0},
	}
	for i := range rows {
		if err := st.TradeJournal().db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	stats, err := st.TradeJournal().StatsForWindow([]string{"T1"}, now-5000, now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Trades != 2 || stats.Wins != 1 || stats.Losses != 1 {
		t.Fatalf("counts: %+v", stats)
	}
	if absF(stats.NetPnL-0.7) > 1e-9 {
		t.Fatalf("net pnl = %v, want 0.7 (2.0-0.2-1.0-0.1)", stats.NetPnL)
	}
	if absF(stats.WinRate-50) > 1e-9 {
		t.Fatalf("win rate = %v", stats.WinRate)
	}
	if absF(stats.ProfitFactor-1.8/1.1) > 1e-9 {
		t.Fatalf("profit factor = %v, want 1.8/1.1 (net caliber, same as JournalStats)", stats.ProfitFactor)
	}
	if stats.RCount != 2 || absF(stats.AvgR-((0.18-1.1)/2)) > 1e-9 {
		t.Fatalf("R: count=%d avg=%v", stats.RCount, stats.AvgR)
	}

	both, err := st.TradeJournal().StatsForWindow([]string{"T1", "T2"}, now-5000, now)
	if err != nil {
		t.Fatalf("stats both: %v", err)
	}
	if both.Trades != 3 {
		t.Fatalf("both traders trades = %d, want 3", both.Trades)
	}
}

// Canonical hashing: key order and whitespace must not change the identity.
// (The trader.RiskControlHash ↔ store.HashRiskControlOf equality contract is
// pinned from the trader package — store cannot import trader.)
func TestConfigHashCanonicalization(t *testing.T) {
	a := `{"risk_control":{"max_positions":3},"coin_source":{"source_type":"ai500"}}`
	b := `{ "coin_source" : { "source_type" : "ai500" }, "risk_control" : { "max_positions" : 3 } }`
	if HashConfigJSON(a) != HashConfigJSON(b) {
		t.Fatalf("canonical hash differs on key order/whitespace: %s vs %s", HashConfigJSON(a), HashConfigJSON(b))
	}
}
