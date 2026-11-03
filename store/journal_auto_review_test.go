package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAutoReviewConditionalWritesAndAttempts(t *testing.T) {
	st := newTestStore(t)
	journal := st.TradeJournal()
	row := TradeJournalDB{TraderID: "T", PositionID: 1, Symbol: "BTCUSDT", Side: "LONG"}
	if err := st.GormDB().Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 1; i <= 3; i++ {
		if err := journal.FailAIReviews([]int64{row.ID}, now); err != nil {
			t.Fatal(err)
		}
		got, _ := journal.Get("T", row.ID)
		wantStatus := "pending"
		if i == 3 {
			wantStatus = "ai_failed"
		}
		if got.AIReviewAttempts != i || got.ReviewStatus != wantStatus {
			t.Fatalf("attempt %d: %+v", i, got)
		}
	}
	_ = journal.FailAIReviews([]int64{row.ID}, now)
	got, _ := journal.Get("T", row.ID)
	if got.AIReviewAttempts != 3 {
		t.Fatalf("terminal failures must not retry: %+v", got)
	}
	lesson, mode := "人工确认", "weak_setup"
	if _, err := journal.UpdateReviewPayload("T", row.ID, &JournalReviewPayload{Lesson: &lesson, FailureMode: &mode}); err != nil {
		t.Fatal(err)
	}
	if err := journal.UpdateAIReview("T", AIJournalReview{ID: row.ID, Lesson: "AI覆盖", FailureMode: "late_exit"}, now); err != nil {
		t.Fatal(err)
	}
	_ = journal.FailAIReviews([]int64{row.ID}, now)
	got, _ = journal.Get("T", row.ID)
	if got.ReviewStatus != "reviewed" || got.Lesson != lesson || got.FailureMode != mode || got.AIReviewAttempts != 3 {
		t.Fatalf("human review overwritten: %+v", got)
	}

	other := TradeJournalDB{TraderID: "T", PositionID: 2, Symbol: "ETHUSDT", Side: "SHORT"}
	if err := st.GormDB().Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	_ = journal.UpdateAIReview("foreign", AIJournalReview{ID: other.ID}, now)
	got, _ = journal.Get("T", other.ID)
	if got.ReviewStatus != "pending" {
		t.Fatal("foreign trader updated")
	}
	if err := journal.UpdateAIReview("T", AIJournalReview{ID: other.ID, Lesson: "首次", FailureMode: "invalid"}, now); err != nil {
		t.Fatal(err)
	}
	_ = journal.UpdateAIReview("T", AIJournalReview{ID: other.ID, Lesson: "重复"}, now)
	_ = journal.FailAIReviews([]int64{other.ID}, now)
	got, _ = journal.Get("T", other.ID)
	if got.ReviewStatus != "ai_reviewed" || got.Lesson != "首次" || got.FailureMode != "other" || got.AIReviewAttempts != 0 {
		t.Fatalf("AI write must be conditional: %+v", got)
	}
}

func TestFailureModeVocabularyAndStats(t *testing.T) {
	for _, mode := range strings.Fields("chased_entry counter_trend weak_setup stop_too_tight stop_too_wide early_exit late_exit size_too_large market_shock no_mistake other") {
		if NormalizeFailureMode(mode) != mode {
			t.Fatalf("valid mode rejected: %s", mode)
		}
	}
	for _, mode := range []string{"", "invented", "CHASED_ENTRY"} {
		if NormalizeFailureMode(mode) != "other" {
			t.Fatalf("unknown mode accepted: %s", mode)
		}
	}
	st := newTestStore(t)
	journal := st.TradeJournal()
	rows := []TradeJournalDB{
		{TraderID: "T", PositionID: 1, Symbol: "A", Side: "LONG", EntryPrice: 100, PlannedStopLoss: 90, Quantity: 2, RealizedPnL: 22, Fee: 2, FailureMode: "weak_setup", ReviewStatus: "ai_reviewed"},
		{TraderID: "T", PositionID: 2, Symbol: "B", Side: "SHORT", EntryPrice: 100, PlannedStopLoss: 110, Quantity: 1, RealizedPnL: -4, Fee: 1, FailureMode: "weak_setup", ReviewStatus: "reviewed"},
		{TraderID: "T", PositionID: 3, Symbol: "C", Side: "LONG", RealizedPnL: 3, Fee: 1, FailureMode: "weak_setup"},
		{TraderID: "T", PositionID: 4, Symbol: "D", Side: "LONG", FailureMode: "no_mistake", ReviewStatus: "ai_failed"},
		{TraderID: "T", PositionID: 5, Symbol: "E", Side: "LONG"},
		{TraderID: "foreign", PositionID: 1, Symbol: "F", Side: "LONG", FailureMode: "weak_setup", RealizedPnL: 100},
	}
	if err := st.GormDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	stats, err := journal.GetStats("T")
	if err != nil {
		t.Fatal(err)
	}
	if stats.ReviewedCount != 2 || stats.PendingCount != 2 || len(stats.ByFailureMode) != 2 {
		t.Fatalf("counts: %+v", stats)
	}
	g := stats.ByFailureMode[0]
	if g.Key != "weak_setup" || g.Count != 3 || g.TotalPnL != 17 || g.RSamples != 2 || g.AvgR == nil || *g.AvgR != .25 {
		t.Fatalf("group: %+v", g)
	}
	if stats.ByFailureMode[1].AvgR != nil {
		t.Fatal("missing planned risk must be null")
	}
	data, _ := json.Marshal(stats.ByFailureMode[1])
	if !strings.Contains(string(data), `"avg_r":null`) {
		t.Fatalf("missing R JSON: %s", data)
	}
	reviewed, err := journal.GetRecentReviewed("T", 50)
	if err != nil || len(reviewed) != 2 {
		t.Fatalf("rule extraction must include human and AI reviews only: %v %+v", err, reviewed)
	}
}

func TestAutoReviewBatchSelection(t *testing.T) {
	st := newTestStore(t)
	journal := st.TradeJournal()
	for _, trader := range []Trader{{ID: "T1", UserID: "U"}, {ID: "T2", UserID: "U"}, {ID: "foreign", UserID: "other"}} {
		if err := st.Trader().Create(&trader); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	yes, no := true, false
	rows := make([]TradeJournalDB, 0, 70)
	for i := 0; i < 55; i++ {
		trader := "T1"
		if i%2 == 1 {
			trader = "T2"
		}
		rows = append(rows, TradeJournalDB{TraderID: trader, PositionID: int64(i + 1), Symbol: "BTCUSDT", Side: "LONG", AIManaged: &yes, ExitTime: now.Add(-time.Duration(i+11) * time.Minute).UnixMilli()})
	}
	for i, row := range []TradeJournalDB{
		{TraderID: "T1", AIManaged: &no, ExitTime: now.Add(-time.Hour).UnixMilli()},
		{TraderID: "T1", AIManaged: nil, ExitTime: now.Add(-time.Hour).UnixMilli()},
		{TraderID: "T1", AIManaged: &yes, ExitTime: now.Add(-91 * 24 * time.Hour).UnixMilli()},
		{TraderID: "T1", AIManaged: &yes, ExitTime: now.Add(-9 * time.Minute).UnixMilli()},
		{TraderID: "T1", AIManaged: &yes, ExitTime: 0},
		{TraderID: "T1", AIManaged: &yes, ExitTime: now.Add(time.Hour).UnixMilli()},
		{TraderID: "T1", AIManaged: &yes, ExitTime: now.Add(-time.Hour).UnixMilli(), ReviewStatus: "reviewed"},
		{TraderID: "T1", AIManaged: &yes, ExitTime: now.Add(-time.Hour).UnixMilli(), ReviewStatus: "ai_reviewed"},
		{TraderID: "T1", AIManaged: &yes, ExitTime: now.Add(-time.Hour).UnixMilli(), ReviewStatus: "ai_failed"},
		{TraderID: "foreign", AIManaged: &yes, ExitTime: now.Add(-time.Hour).UnixMilli()},
	} {
		row.PositionID, row.Symbol, row.Side = int64(100+i), "excluded", "LONG"
		rows = append(rows, row)
	}
	if err := st.GormDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	got, err := journal.ListAutoReviewPending("U", now)
	if err != nil || len(got) != 50 {
		t.Fatalf("selection: %v %d", err, len(got))
	}
	for i, row := range got {
		if row.ID != rows[54-i].ID {
			t.Fatalf("oldest first across traders: index %d got %d want %d", i, row.ID, rows[54-i].ID)
		}
	}
}

func TestOpeningReviewDecisionLatestSuccessfulPreEntry(t *testing.T) {
	st := newTestStore(t)
	journal := st.TradeJournal()
	_ = st.Decision() // Initialize decision records.
	entryTime := time.Now().UTC().Truncate(time.Second)
	entry := &TradeJournalDB{TraderID: "T", Symbol: "BTCUSDT", Side: "SHORT", EntryTime: entryTime.UnixMilli()}
	for i, tc := range []struct {
		minutes        int
		symbol, action string
		success        bool
	}{
		{-31, "BTCUSDT", "open_short", true},
		{-20, "BTCUSDT", "open_short", true},
		{-10, "BTCUSDT", "open_short_limit", true},
		{-9, "BTCUSDT", "open_short", false},
		{-8, "BTCUSDT", "open_long", true},
		{-7, "ETHUSDT", "open_short", true},
		{1, "BTCUSDT", "open_short", true},
	} {
		actions, _ := json.Marshal([]DecisionAction{{Symbol: tc.symbol, Action: tc.action, Success: tc.success, Reasoning: "匹配理由"}})
		rec := DecisionRecordDB{TraderID: "T", CycleNumber: i, Timestamp: entryTime.Add(time.Duration(tc.minutes) * time.Minute), Decisions: string(actions), CoTTrace: "BTCUSDT 开仓分析"}
		if err := st.GormDB().Create(&rec).Error; err != nil {
			t.Fatal(err)
		}
	}
	rec, action, err := journal.OpeningReviewDecision(entry)
	if err != nil || rec == nil || action == nil || action.Action != "open_short_limit" || !rec.Timestamp.Equal(entryTime.Add(-10*time.Minute)) {
		t.Fatalf("wrong decision: %+v %+v %v", rec, action, err)
	}
}

func TestReviewPromptAutoReviewDefaultsOff(t *testing.T) {
	st := newTestStore(t)
	prompts := st.ReviewPrompt()
	if err := prompts.Upsert("U", "", ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := prompts.Get("U")
	if err != nil || cfg.AutoReviewEnabled {
		t.Fatalf("toggle default: %+v %v", cfg, err)
	}
	users, _ := prompts.ListAutoReviewUsers()
	if len(users) != 0 {
		t.Fatal("default config enables a user")
	}
	_ = prompts.Upsert("U", "custom", "", true)
	_ = prompts.Upsert("U", "updated", "")
	cfg, _ = prompts.Get("U")
	if !cfg.AutoReviewEnabled || cfg.ReviewSystemPrompt != "updated" {
		t.Fatalf("template updates must preserve toggle: %+v", cfg)
	}
	_ = prompts.Upsert("U", "", "", false)
	users, _ = prompts.ListAutoReviewUsers()
	if len(users) != 0 {
		t.Fatal("explicit false was not persisted")
	}
}

func TestAutoReviewMigrationPreservesLegacyRows(t *testing.T) {
	st := newTestStore(t)
	for _, sql := range []string{
		"DROP TABLE review_prompt_configs",
		"CREATE TABLE review_prompt_configs (id INTEGER PRIMARY KEY, user_id TEXT NOT NULL UNIQUE, review_system_prompt TEXT, rule_extract_system_prompt TEXT, created_at DATETIME, updated_at DATETIME)",
		"INSERT INTO review_prompt_configs (id,user_id,review_system_prompt,rule_extract_system_prompt) VALUES (1,'legacy','custom','rules')",
		"DROP TABLE trade_journal",
		"CREATE TABLE trade_journal (id INTEGER PRIMARY KEY, trader_id TEXT, position_id INTEGER, symbol TEXT, side TEXT)",
		"INSERT INTO trade_journal (id,trader_id,position_id,symbol,side) VALUES (1,'T',1,'BTCUSDT','LONG')",
	} {
		if err := st.GormDB().Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := st.ReviewPrompt().initTables(); err != nil {
			t.Fatal(err)
		}
		if err := st.TradeJournal().initTables(); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := st.ReviewPrompt().Get("legacy")
	if err != nil || cfg == nil || cfg.AutoReviewEnabled || cfg.ReviewSystemPrompt != "custom" {
		t.Fatalf("legacy prompt migration: %+v %v", cfg, err)
	}
	row, err := st.TradeJournal().Get("T", 1)
	if err != nil || row.Symbol != "BTCUSDT" || row.FailureMode != "" || row.AIReviewAttempts != 0 || row.ReviewStatus != "pending" {
		t.Fatalf("legacy journal migration: %+v %v", row, err)
	}
}

func TestAutoReviewSelectionTimeBoundaries(t *testing.T) {
	st := newTestStore(t)
	if err := st.Trader().Create(&Trader{ID: "T", UserID: "U"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	oldest, newest := now.Add(-90*24*time.Hour), now.Add(-10*time.Minute)
	yes := true
	for i, exit := range []time.Time{oldest.Add(-time.Millisecond), oldest, newest, newest.Add(time.Millisecond)} {
		row := TradeJournalDB{TraderID: "T", PositionID: int64(i + 1), Symbol: "BTCUSDT", Side: "LONG", AIManaged: &yes, ExitTime: exit.UnixMilli()}
		if err := st.GormDB().Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.TradeJournal().ListAutoReviewPending("U", now)
	if err != nil || len(rows) != 2 || rows[0].PositionID != 2 || rows[1].PositionID != 3 {
		t.Fatalf("incorrect inclusive time window: %+v %v", rows, err)
	}
}
