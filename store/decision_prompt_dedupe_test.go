package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"nofx/logger"

	"github.com/mattn/go-sqlite3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func newPromptDedupeTestStore(t *testing.T) *DecisionStore {
	t.Helper()
	t.Setenv("NOFX_DECISION_PROMPT_DEDUPE", "1")
	dsn := filepath.Join(t.TempDir(), "prompts.db") + "?_journal_mode=WAL&_txlock=immediate&_busy_timeout=100"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:  gormlogger.Default.LogMode(gormlogger.Silent),
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { sqlDB.Close() })
	s := NewDecisionStore(db)
	if err := s.initTables(); err != nil {
		t.Fatal(err)
	}
	return s
}

func promptDedupeFixture(prompt string, cycle int) *DecisionRecord {
	return &DecisionRecord{
		TraderID: "prompt-test", CycleNumber: cycle,
		Timestamp:    time.Date(2026, 11, 7, 1, cycle, 0, 123456789, time.UTC),
		SystemPrompt: prompt, InputPrompt: "输入\r\n\x00保持原样", CoTTrace: "分析\n\t",
		DecisionJSON: " {\"decision\": true} ", RawResponse: "原始响应\x00",
		CandidateCoins:    []string{"BTCUSDT", "ETHUSDT"},
		CandidateVerdicts: []CandidateVerdict{{Symbol: "BTCUSDT", Status: "blocked", LongFailed: []string{"reason"}}},
		ExecutionLog:      []string{"日志", "entry"}, Success: true, ErrorMessage: "detail",
		AIRequestDurationMs: 1234, ConfigHash: "config-hash",
		Decisions: []DecisionAction{{Action: "wait", Symbol: "BTCUSDT", Reasoning: "等待", Success: true}},
	}
}

func requirePromptReadAPIs(t *testing.T, s *DecisionStore, want []*DecisionRecord) {
	t.Helper()
	readers := []struct {
		name string
		read func() ([]*DecisionRecord, error)
	}{
		{"latest", func() ([]*DecisionRecord, error) { return s.GetLatestRecords("prompt-test", 1000) }},
		{"all latest", func() ([]*DecisionRecord, error) { return s.GetAllLatestRecords(1000) }},
		{"date", func() ([]*DecisionRecord, error) {
			return s.GetRecordsByDate("prompt-test", time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC))
		}},
	}
	for _, reader := range readers {
		got, err := reader.read()
		if err != nil {
			t.Fatalf("%s: %v", reader.name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s round trip changed fields\ngot  %+v\nwant %+v", reader.name, got, want)
		}
	}
}

func promptSideCount(t *testing.T, s *DecisionStore) int64 {
	t.Helper()
	var count int64
	if err := s.db.Model(&decisionSystemPrompt{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func promptRawRows(t *testing.T, s *DecisionStore) []*DecisionRecordDB {
	t.Helper()
	var rows []*DecisionRecordDB
	if err := s.db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

// Use SQL to create genuine legacy rows, including deliberately formatted JSON.
func insertLegacyPrompt(t *testing.T, s *DecisionStore, prompt string, cycle int) {
	t.Helper()
	err := s.db.Exec(`INSERT INTO decision_records
		(trader_id, cycle_number, timestamp, system_prompt, input_prompt, cot_trace,
		decision_json, raw_response, candidate_coins, candidate_verdicts, execution_log,
		decisions, success, error_message, ai_request_duration_ms, config_hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"prompt-test", cycle, promptDedupeFixture(prompt, cycle).Timestamp, prompt,
		"input\r\n\x00", "trace\x00", " {\"keep\": true} ", "raw\r\n",
		` ["BTCUSDT", "ETHUSDT"] `, ` [{"symbol":"BTCUSDT","status":"held"}] `,
		` ["execution", "日志"] `, ` [{"action":"wait", "symbol":"BTCUSDT"}] `,
		true, "error detail", 5432, "legacy-config", time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)).Error
	if err != nil {
		t.Fatal(err)
	}
}

func TestDecisionPromptRoundTrip(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	// The default is enabled. Do not trim, normalize Unicode, or truncate at NUL.
	t.Setenv("NOFX_DECISION_PROMPT_DEDUPE", "")
	prompt := " \r\n中文 e\u0301 é\x00'\"\t尾部  "
	want := promptDedupeFixture(prompt, 1)
	if err := s.LogDecision(want); err != nil {
		t.Fatal(err)
	}
	requirePromptReadAPIs(t, s, []*DecisionRecord{want})
	row := promptRawRows(t, s)[0]
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))
	if row.SystemPrompt != "" || row.SystemPromptHash != hash {
		t.Fatalf("inline=%q hash=%q", row.SystemPrompt, row.SystemPromptHash)
	}
	if promptSideCount(t, s) != 1 {
		t.Fatal("expected one stored prompt")
	}
	var stored decisionSystemPrompt
	if err := s.db.First(&stored).Error; err != nil || stored.Text != prompt || stored.CreatedAt.IsZero() {
		t.Fatalf("stored prompt=%+v err=%v", stored, err)
	}
}

func TestDecisionPromptDedupeAndBatchReads(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	var want []*DecisionRecord
	for i, prompt := range []string{"same", "same", "same", "different"} {
		rec := promptDedupeFixture(prompt, i+1)
		if err := s.LogDecision(rec); err != nil {
			t.Fatal(err)
		}
		want = append(want, rec)
	}
	if promptSideCount(t, s) != 2 {
		t.Fatal("expected two distinct prompts")
	}
	queries := 0
	if err := s.db.Callback().Query().After("gorm:query").Register("prompt-test:batch", func(tx *gorm.DB) {
		if tx.Statement.Table == "decision_system_prompts" {
			queries++
			if !strings.Contains(tx.Statement.SQL.String(), " IN ") || len(tx.Statement.Vars) != 2 {
				t.Errorf("expected one IN query with two distinct hashes, got %s %v", tx.Statement.SQL.String(), tx.Statement.Vars)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	requirePromptReadAPIs(t, s, want)
	if queries != 3 {
		t.Fatalf("expected one side-table query per API, got %d", queries)
	}
}

func TestDecisionPromptLegacyBackfillLosslessAndIdempotent(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	for i, prompt := range []string{"legacy\r\n\x00中文", "legacy\r\n\x00中文", "different", ""} {
		insertLegacyPrompt(t, s, prompt, i+1)
	}
	beforeRows := promptRawRows(t, s)
	before, err := s.toRecords(beforeRows)
	if err != nil {
		t.Fatal(err)
	}
	requirePromptReadAPIs(t, s, before)
	migrated, err := s.BackfillSystemPromptHashes(context.Background(), 2, time.Nanosecond)
	if err != nil || migrated != 3 {
		t.Fatalf("migrated=%d err=%v", migrated, err)
	}
	requirePromptReadAPIs(t, s, before)
	afterRows := promptRawRows(t, s)
	after, err := s.toRecords(afterRows)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("full toRecord output changed, err=%v", err)
	}
	for i, row := range afterRows {
		if beforeRows[i].SystemPrompt != "" && (row.SystemPrompt != "" || row.SystemPromptHash == "") {
			t.Fatalf("row %d was not deduped", row.ID)
		}
		row.SystemPrompt = beforeRows[i].SystemPrompt
		row.SystemPromptHash = ""
		if !reflect.DeepEqual(row, beforeRows[i]) {
			t.Fatalf("other database columns changed for row %d", row.ID)
		}
	}
	migrated, err = s.BackfillSystemPromptHashes(context.Background(), 2, time.Nanosecond)
	if err != nil || migrated != 0 || promptSideCount(t, s) != 2 {
		t.Fatalf("second run migrated=%d err=%v", migrated, err)
	}
}

func TestDecisionPromptBackfillCancelAndResume(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	for i := 1; i <= 5; i++ {
		insertLegacyPrompt(t, s, fmt.Sprintf("legacy-%d", i), i)
	}
	before, err := s.toRecords(promptRawRows(t, s))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		migrated int
		err      error
	}
	done := make(chan result, 1)
	go func() {
		migrated, err := s.BackfillSystemPromptHashes(ctx, 1, time.Hour)
		done <- result{migrated, err}
	}()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for promptSideCount(t, s) == 0 {
		select {
		case <-deadline:
			t.Fatal("first batch did not commit")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case got := <-done:
		if got.migrated != 1 || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancel result=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt the pause")
	}
	// A fresh store has no in-memory cursor from the canceled run.
	s = NewDecisionStore(s.db)
	migrated, err := s.BackfillSystemPromptHashes(context.Background(), 2, time.Nanosecond)
	if err != nil || migrated != 4 {
		t.Fatalf("resume migrated=%d err=%v", migrated, err)
	}
	requirePromptReadAPIs(t, s, before)
}

func TestDecisionPromptBackfillConcurrentWriterGuard(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	insertLegacyPrompt(t, s, "before change", 1)
	insertLegacyPrompt(t, s, "unaffected", 2)
	// SQLite serializes real writers. A trigger deterministically interleaves a
	// text change after selection/side insertion and before the guarded UPDATE.
	if err := s.db.Exec(`CREATE TRIGGER prompt_test_change AFTER INSERT ON decision_system_prompts
		WHEN NEW.text = 'before change' BEGIN
		UPDATE decision_records SET system_prompt='concurrent text' WHERE cycle_number=1;
		END`).Error; err != nil {
		t.Fatal(err)
	}
	migrated, err := s.BackfillSystemPromptHashes(context.Background(), 1, time.Nanosecond)
	if err != nil || migrated != 1 {
		t.Fatalf("migrated=%d err=%v", migrated, err)
	}
	row := promptRawRows(t, s)[0]
	if row.SystemPrompt != "concurrent text" || row.SystemPromptHash != "" {
		t.Fatalf("concurrent change clobbered: %+v", row)
	}
	migrated, err = s.BackfillSystemPromptHashes(context.Background(), 1, time.Nanosecond)
	if err != nil || migrated != 1 {
		t.Fatalf("resume changed row migrated=%d err=%v", migrated, err)
	}
	got, err := s.GetLatestRecords("prompt-test", 10)
	if err != nil || got[0].SystemPrompt != "concurrent text" {
		t.Fatalf("changed text lost: got=%+v err=%v", got, err)
	}
}

func TestDecisionPromptMismatchKeepsText(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	prompt := "original exact text"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(prompt)))
	if err := s.db.Create(&decisionSystemPrompt{Hash: hash, Text: "corrupt or colliding text"}).Error; err != nil {
		t.Fatal(err)
	}
	rec := promptDedupeFixture(prompt, 1)
	if err := s.LogDecision(rec); err != nil {
		t.Fatal(err)
	}
	insertLegacyPrompt(t, s, prompt, 2)
	insertLegacyPrompt(t, s, "healthy later row", 3)
	migrated, err := s.BackfillSystemPromptHashes(context.Background(), 1, time.Nanosecond)
	if err != nil || migrated != 1 {
		t.Fatalf("mismatches must be skipped without looping: migrated=%d err=%v", migrated, err)
	}
	rows := promptRawRows(t, s)
	for _, row := range rows[:2] {
		if row.SystemPrompt != prompt || row.SystemPromptHash != "" {
			t.Fatalf("mismatch lost inline text: %+v", row)
		}
	}
	got, err := s.GetLatestRecords("prompt-test", 10)
	if err != nil || got[0].SystemPrompt != prompt || got[1].SystemPrompt != prompt {
		t.Fatalf("read mismatch rows: got=%+v err=%v", got, err)
	}
}

func TestDecisionPromptKillSwitch(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	deduped := promptDedupeFixture("existing deduped", 1)
	if err := s.LogDecision(deduped); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOFX_DECISION_PROMPT_DEDUPE", "0")
	legacy := promptDedupeFixture("legacy write", 2)
	if err := s.LogDecision(legacy); err != nil {
		t.Fatal(err)
	}
	migrated, err := s.BackfillSystemPromptHashes(context.Background(), 1, time.Nanosecond)
	if err != nil || migrated != 0 {
		t.Fatalf("disabled migrated=%d err=%v", migrated, err)
	}
	s.StartSystemPromptBackfill(context.Background())
	row := promptRawRows(t, s)[1]
	if row.SystemPrompt != legacy.SystemPrompt || row.SystemPromptHash != "" || promptSideCount(t, s) != 1 {
		t.Fatalf("kill switch did not preserve legacy writes: %+v", row)
	}
	requirePromptReadAPIs(t, s, []*DecisionRecord{deduped, legacy})
}

func TestDecisionPromptEmpty(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	rec := promptDedupeFixture("", 1)
	if err := s.LogDecision(rec); err != nil {
		t.Fatal(err)
	}
	requirePromptReadAPIs(t, s, []*DecisionRecord{rec})
	row := promptRawRows(t, s)[0]
	if row.SystemPrompt != "" || row.SystemPromptHash != "" || promptSideCount(t, s) != 0 {
		t.Fatalf("empty prompt was hashed: %+v", row)
	}
	migrated, err := s.BackfillSystemPromptHashes(context.Background(), 0, time.Nanosecond)
	if err != nil || migrated != 0 {
		t.Fatalf("empty prompt migrated=%d err=%v", migrated, err)
	}
}

func TestDecisionPromptMissingHashLogsOnceAndLegacyTakesPrecedence(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	insertLegacyPrompt(t, s, "inline wins", 1)
	insertLegacyPrompt(t, s, "", 2)
	if err := s.db.Exec("UPDATE decision_records SET system_prompt_hash='missing'").Error; err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previousOutput := logger.Log.Out
	logger.Log.SetOutput(&logs)
	t.Cleanup(func() { logger.Log.SetOutput(previousOutput) })
	before, err := s.toRecords(promptRawRows(t, s))
	if err != nil || before[0].SystemPrompt != "inline wins" || before[1].SystemPrompt != "" {
		t.Fatalf("legacy/missing resolution: got=%+v err=%v", before, err)
	}
	requirePromptReadAPIs(t, s, before)
	if count := strings.Count(logs.String(), "Decision system prompt missing"); count != 1 {
		t.Fatalf("missing hash logged %d times", count)
	}
}

func TestDecisionPromptTransactionsRollback(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		s := newPromptDedupeTestStore(t)
		if err := s.db.Exec(`CREATE TRIGGER prompt_test_fail BEFORE INSERT ON decision_records
			BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END`).Error; err != nil {
			t.Fatal(err)
		}
		rec := promptDedupeFixture("rollback prompt", 1)
		if err := s.LogDecision(rec); err == nil {
			t.Fatal("expected insert failure")
		}
		if len(promptRawRows(t, s)) != 0 || promptSideCount(t, s) != 0 || rec.SystemPrompt != "rollback prompt" {
			t.Fatal("write transaction leaked partial data")
		}
	})
	t.Run("backfill", func(t *testing.T) {
		s := newPromptDedupeTestStore(t)
		insertLegacyPrompt(t, s, "first", 1)
		insertLegacyPrompt(t, s, "second", 2)
		before := promptRawRows(t, s)
		if err := s.db.Exec(`CREATE TRIGGER prompt_test_fail BEFORE UPDATE ON decision_records
			WHEN OLD.cycle_number=2 BEGIN SELECT RAISE(ABORT, 'injected update failure'); END`).Error; err != nil {
			t.Fatal(err)
		}
		migrated, err := s.BackfillSystemPromptHashes(context.Background(), 2, time.Nanosecond)
		if err == nil || migrated != 0 || promptSideCount(t, s) != 0 || !reflect.DeepEqual(before, promptRawRows(t, s)) {
			t.Fatalf("batch transaction leaked partial data: migrated=%d err=%v", migrated, err)
		}
	})
}

func TestDecisionPromptBackfillRetriesBusy(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	insertLegacyPrompt(t, s, "first", 1)
	insertLegacyPrompt(t, s, "second", 2)
	updates := 0
	if err := s.db.Callback().Raw().Before("gorm:raw").Register("prompt-test:busy", func(tx *gorm.DB) {
		if strings.HasPrefix(tx.Statement.SQL.String(), "UPDATE decision_records SET system_prompt=''") {
			updates++
			if updates == 2 {
				tx.AddError(sqlite3.Error{Code: sqlite3.ErrBusy, ExtendedCode: sqlite3.ErrBusySnapshot})
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	migrated, err := s.BackfillSystemPromptHashes(context.Background(), 2, time.Nanosecond)
	if err != nil || migrated != 2 || updates != 4 || promptSideCount(t, s) != 2 {
		t.Fatalf("busy retry accounting: migrated=%d updates=%d err=%v", migrated, updates, err)
	}
}

func TestDecisionPromptSchemaUpgrade(t *testing.T) {
	s := newPromptDedupeTestStore(t)
	if err := s.db.Migrator().DropColumn(&DecisionRecordDB{}, "system_prompt_hash"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Migrator().DropTable(&decisionSystemPrompt{}); err != nil {
		t.Fatal(err)
	}
	insertLegacyPrompt(t, s, "old schema text", 1)
	if err := s.initTables(); err != nil {
		t.Fatal(err)
	}
	row := promptRawRows(t, s)[0]
	if row.SystemPrompt != "old schema text" || row.SystemPromptHash != "" {
		t.Fatalf("schema upgrade changed legacy row: %+v", row)
	}
	var column struct {
		Type      string
		NotNull   int `gorm:"column:notnull"`
		DfltValue string
	}
	if err := s.db.Raw("SELECT type, [notnull], dflt_value FROM pragma_table_info('decision_records') WHERE name='system_prompt_hash'").Scan(&column).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(column.Type, "TEXT") || column.NotNull != 1 || column.DfltValue != "\"\"" {
		b, _ := json.Marshal(column)
		t.Fatalf("unexpected hash column definition: %s", b)
	}
	migrated, err := s.BackfillSystemPromptHashes(context.Background(), 0, time.Nanosecond)
	if err != nil || migrated != 1 {
		t.Fatalf("upgraded schema backfill migrated=%d err=%v", migrated, err)
	}
}
