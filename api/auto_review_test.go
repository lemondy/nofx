package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nofx/store"
)

type reviewAIFunc func(context.Context, string, string, string) (string, error)

func (f reviewAIFunc) CallReviewAI(ctx context.Context, user, system, prompt string) (string, error) {
	return f(ctx, user, system, prompt)
}

func validAutoReview(id int64) store.AIJournalReview {
	return store.AIJournalReview{ID: id, FailureMode: "no_mistake", MistakeCategory: "none", StrategyTag: "other", ExecutedAsPlan: "yes"}
}

func TestParseAutoReviews(t *testing.T) {
	batch := []*store.TradeJournalDB{{ID: 11}, {ID: 12}}
	first, second := validAutoReview(11), validAutoReview(12)
	first.FailureMode, first.Lesson = "invented", strings.Repeat("中", 130)
	data, _ := json.Marshal([]store.AIJournalReview{first, second})
	parsed, err := parseAutoReviews("以下为复盘：\n```json\n"+string(data)+"\n```\n备注 [后续文字]", batch)
	if err != nil || len(parsed) != 2 || parsed[0].FailureMode != "other" || len([]rune(parsed[0].Lesson)) != 120 {
		t.Fatalf("lenient parse: %+v %v", parsed, err)
	}
	for name, response := range map[string]string{
		"no_array": "no JSON", "malformed": "[{", "empty": "[]", "null": "[null,null]",
		"missing":   string(data),
		"foreign":   strings.Replace(string(data), `"id":12`, `"id":99`, 1),
		"duplicate": strings.Replace(string(data), `"id":12`, `"id":11`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			inputBatch := batch
			if name == "missing" {
				inputBatch = append(append([]*store.TradeJournalDB{}, batch...), &store.TradeJournalDB{ID: 13})
			}
			if _, err := parseAutoReviews(response, inputBatch); err == nil {
				t.Fatalf("accepted invalid response: %s", response)
			}
		})
	}
}

func seedAutoReviewRows(t *testing.T, srv *Server, traderID string, n int, now time.Time, positionBase ...int64) []store.TradeJournalDB {
	t.Helper()
	yes := true
	base := int64(0)
	if len(positionBase) > 0 {
		base = positionBase[0]
	}
	rows := make([]store.TradeJournalDB, n)
	for i := range rows {
		rows[i] = store.TradeJournalDB{TraderID: traderID, PositionID: base + int64(i+1), Symbol: "BTCUSDT", Side: "LONG", AIManaged: &yes,
			EntryPrice: 100, ExitPrice: 101, Quantity: 1, PlannedStopLoss: 90, RealizedPnL: 1,
			EntryTime: now.Add(-2 * time.Hour).UnixMilli(), ExitTime: now.Add(-time.Duration(i+11) * time.Minute).UnixMilli()}
	}
	if err := srv.store.GormDB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

func inputAutoReviewTrades(t *testing.T, prompt string) []autoReviewTradeInput {
	t.Helper()
	var inputs []autoReviewTradeInput
	if err := json.Unmarshal([]byte(prompt[strings.Index(prompt, "\n")+1:]), &inputs); err != nil {
		t.Fatal(err)
	}
	return inputs
}

func autoReviewResponse(inputs []autoReviewTradeInput) string {
	output := make([]store.AIJournalReview, len(inputs))
	for i, input := range inputs {
		output[i] = validAutoReview(input.ID)
	}
	data, _ := json.Marshal(output)
	return string(data)
}

func TestAutoReviewOptInAndBatchCap(t *testing.T) {
	srv, _, _, user, trader := idorFixture(t)
	now := time.Now().UTC()
	rows := seedAutoReviewRows(t, srv, trader, 203, now)
	seedAutoReviewRows(t, srv, "trader-B", 1, now)
	calls := 0
	srv.reviewAI = reviewAIFunc(func(_ context.Context, gotUser, system, prompt string) (string, error) {
		calls++
		if gotUser != user || system != defaultTradeReviewSystemPrompt {
			t.Fatalf("wrong user or prompt: %s", gotUser)
		}
		inputs := inputAutoReviewTrades(t, prompt)
		if len(inputs) != 10 || inputs[0].ID != rows[202-(calls-1)*10].ID {
			t.Fatalf("batch cap or order wrong: %+v", inputs)
		}
		return autoReviewResponse(inputs), nil
	})
	srv.runAutoReview(context.Background(), now)
	if err := srv.store.ReviewPrompt().Upsert(user, "", ""); err != nil {
		t.Fatal(err)
	}
	srv.runAutoReview(context.Background(), now)
	if calls != 0 {
		t.Fatal("default off must make no model call")
	}
	if err := srv.store.ReviewPrompt().Upsert(user, "", "", true); err != nil {
		t.Fatal(err)
	}
	srv.runAutoReview(context.Background(), now)
	if calls != 20 {
		t.Fatalf("got %d calls, want twenty batches", calls)
	}
	pending, _ := srv.store.TradeJournal().ListAutoReviewPending(user, now)
	if len(pending) != 3 {
		t.Fatalf("expected 3 remaining, got %d", len(pending))
	}
	foreign, _ := srv.store.TradeJournal().Get("trader-B", rows[202].ID+1)
	if foreign == nil || foreign.ReviewStatus != "pending" {
		t.Fatalf("disabled user's rows changed: %+v", foreign)
	}
}

func TestRunDueAutoReviewRespectsThreeDayInterval(t *testing.T) {
	srv, _, _, user, trader := idorFixture(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	rows := seedAutoReviewRows(t, srv, trader, 1, now)
	if err := srv.store.ReviewPrompt().Upsert(user, "", "", true); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv.reviewAI = reviewAIFunc(func(_ context.Context, _, _, prompt string) (string, error) {
		calls.Add(1)
		inputs := inputAutoReviewTrades(t, prompt)
		return autoReviewResponse(inputs), nil
	})
	// First due run: reviews the pending row and persists the run time.
	srv.runDueAutoReview(context.Background(), now)
	got, _ := srv.store.TradeJournal().Get(trader, rows[0].ID)
	cfg, _ := srv.store.ReviewPrompt().Get(user)
	if calls.Load() != 1 || got.ReviewStatus != "ai_reviewed" || cfg == nil || cfg.AutoReviewLastRunAt != now.UnixMilli() {
		t.Fatalf("first due run: calls=%d status=%s cfg=%+v", calls.Load(), got.ReviewStatus, cfg)
	}
	// 71h later new pending rows exist, but the user is not due yet.
	seedAutoReviewRows(t, srv, trader, 2, now.Add(71*time.Hour), 100)
	srv.runDueAutoReview(context.Background(), now.Add(71*time.Hour))
	cfg, _ = srv.store.ReviewPrompt().Get(user)
	if calls.Load() != 1 || cfg.AutoReviewLastRunAt != now.UnixMilli() {
		t.Fatalf("ran before the 3-day interval elapsed: calls=%d last_run_at=%d", calls.Load(), cfg.AutoReviewLastRunAt)
	}
	// At exactly 72h the user is due again.
	srv.runDueAutoReview(context.Background(), now.Add(72*time.Hour))
	cfg, _ = srv.store.ReviewPrompt().Get(user)
	if calls.Load() != 2 || cfg.AutoReviewLastRunAt != now.Add(72*time.Hour).UnixMilli() {
		t.Fatalf("second due run: calls=%d last_run_at=%d", calls.Load(), cfg.AutoReviewLastRunAt)
	}
	// Much later the user is due again but has zero pending rows; the run
	// time must still be marked so it does not stay due forever.
	later := now.Add(6 * 72 * time.Hour)
	srv.runDueAutoReview(context.Background(), later)
	cfg, _ = srv.store.ReviewPrompt().Get(user)
	if calls.Load() != 2 || cfg.AutoReviewLastRunAt != later.UnixMilli() {
		t.Fatalf("zero-pending run must still be marked: calls=%d last_run_at=%d", calls.Load(), cfg.AutoReviewLastRunAt)
	}
	// Saving the toggle must not reset the persisted run time.
	if err := srv.store.ReviewPrompt().Upsert(user, "", "", true); err != nil {
		t.Fatal(err)
	}
	cfg, _ = srv.store.ReviewPrompt().Get(user)
	if cfg.AutoReviewLastRunAt != later.UnixMilli() {
		t.Fatalf("upsert reset the last run time: %+v", cfg)
	}
}

func TestAutoReviewHumanRaceAndDisableBetweenBatches(t *testing.T) {
	srv, _, _, user, trader := idorFixture(t)
	now := time.Now().UTC()
	rows := seedAutoReviewRows(t, srv, trader, 11, now)
	_ = srv.store.ReviewPrompt().Upsert(user, "", "", true)
	calls := 0
	srv.reviewAI = reviewAIFunc(func(_ context.Context, _, _, prompt string) (string, error) {
		calls++
		inputs := inputAutoReviewTrades(t, prompt)
		lesson := "人工优先"
		if _, err := srv.store.TradeJournal().UpdateReviewPayload(trader, inputs[0].ID, &store.JournalReviewPayload{Lesson: &lesson}); err != nil {
			t.Fatal(err)
		}
		if err := srv.store.ReviewPrompt().Upsert(user, "", "", false); err != nil {
			t.Fatal(err)
		}
		return autoReviewResponse(inputs), nil
	})
	srv.runAutoReview(context.Background(), now)
	got, _ := srv.store.TradeJournal().Get(trader, rows[10].ID)
	remaining, _ := srv.store.TradeJournal().Get(trader, rows[0].ID)
	if calls != 1 || got.ReviewStatus != "reviewed" || got.Lesson != "人工优先" || remaining.ReviewStatus != "pending" {
		t.Fatalf("race or toggle check failed: calls=%d human=%+v remaining=%+v", calls, got, remaining)
	}
}

func TestAutoReviewFailuresStopAtThree(t *testing.T) {
	for _, parseFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "call", true: "parse"}[parseFailure], func(t *testing.T) {
			srv, _, _, user, trader := idorFixture(t)
			now := time.Now().UTC()
			rows := seedAutoReviewRows(t, srv, trader, 1, now)
			_ = srv.store.ReviewPrompt().Upsert(user, "", "", true)
			calls := 0
			srv.reviewAI = reviewAIFunc(func(context.Context, string, string, string) (string, error) {
				calls++
				if parseFailure {
					return "invalid JSON", nil
				}
				return "", errors.New("fake model failure")
			})
			for i := 0; i < 4; i++ {
				srv.runAutoReview(context.Background(), now)
			}
			got, _ := srv.store.TradeJournal().Get(trader, rows[0].ID)
			if calls != 3 || got.AIReviewAttempts != 3 || got.ReviewStatus != "ai_failed" {
				t.Fatalf("unbounded retry: calls=%d %+v", calls, got)
			}
		})
	}
}

func TestAutoReviewNoOverlapAndCancellation(t *testing.T) {
	srv, _, _, user, trader := idorFixture(t)
	now := time.Now().UTC()
	rows := seedAutoReviewRows(t, srv, trader, 1, now)
	_ = srv.store.ReviewPrompt().Upsert(user, "", "", true)
	started := make(chan struct{})
	var calls atomic.Int32
	srv.reviewAI = reviewAIFunc(func(ctx context.Context, _, _, _ string) (string, error) {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.autoReviewLoop(ctx, time.Millisecond, time.Hour)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("startup run never began")
	}
	srv.runAutoReview(context.Background(), now)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("background call did not cancel")
	}
	got, _ := srv.store.TradeJournal().Get(trader, rows[0].ID)
	if calls.Load() != 1 || got.AIReviewAttempts != 0 || got.ReviewStatus != "pending" {
		t.Fatalf("overlap or shutdown counted as failure: %+v calls=%d", got, calls.Load())
	}
	srv.startAutoReview()
	srv.startAutoReview()
	if err := srv.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if srv.autoReviewCancel != nil || srv.autoReviewDone != nil {
		t.Fatal("server shutdown left loop running")
	}
}

func TestAutoReviewPromptContextBudget(t *testing.T) {
	srv, _, _, _, trader := idorFixture(t)
	now := time.Now().UTC()
	rows := seedAutoReviewRows(t, srv, trader, 1, now)
	row := &rows[0]
	actions, _ := json.Marshal([]store.DecisionAction{{Symbol: row.Symbol, Action: "open_long_limit", Success: true, StopLoss: 90, Reasoning: strings.Repeat("中文", 1500)}})
	if err := srv.store.GormDB().Create(&store.DecisionRecordDB{TraderID: trader, Timestamp: time.UnixMilli(row.EntryTime).Add(-time.Minute), Decisions: string(actions), CoTTrace: "ETHUSDT 无关推理\nBTCUSDT 入场推理"}).Error; err != nil {
		t.Fatal(err)
	}
	prompt, err := srv.buildAutoReviewPrompt([]*store.TradeJournalDB{row})
	if err != nil {
		t.Fatal(err)
	}
	inputs := inputAutoReviewTrades(t, prompt)
	if len([]rune(inputs[0].EntryContext)) > 2000 || strings.Contains(prompt, "ETHUSDT") || inputs[0].PricePath != "价格路径未提供" || inputs[0].R == nil || *inputs[0].R != .1 {
		t.Fatalf("context budget/facts wrong: %+v", inputs[0])
	}
	if got := symbolReviewCoT("ETHUSDT 无关推理\nBTCUSDT 入场推理", "BTCUSDT"); got != "BTCUSDT 入场推理" {
		t.Fatalf("unrelated CoT included: %s", got)
	}
}

func TestReviewPromptToggleAPIAndRuleExtractionAIReviewed(t *testing.T) {
	srv, router, token, user, trader := idorFixture(t)
	router.GET("/prompt", srv.authMiddleware(), srv.handleGetReviewPromptConfig)
	router.PUT("/prompt", srv.authMiddleware(), srv.handleUpdateReviewPromptConfig)
	router.POST("/extract", srv.authMiddleware(), srv.handleAIExtractRules)
	do := func(method, path, body string) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		if out.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, out.Code, out.Body.String())
		}
		return out.Body.String()
	}
	if !strings.Contains(do("GET", "/prompt", ""), `"auto_review_enabled":false`) {
		t.Fatal("GET default should be false")
	}
	do("PUT", "/prompt", `{"auto_review_enabled":true}`)
	do("PUT", "/prompt", `{"review_system_prompt":"custom"}`)
	cfg, _ := srv.store.ReviewPrompt().Get(user)
	if !cfg.AutoReviewEnabled || cfg.ReviewSystemPrompt != "custom" {
		t.Fatalf("partial update lost config: %+v", cfg)
	}
	do("PUT", "/prompt", `{"auto_review_enabled":false}`)
	if !strings.Contains(do("GET", "/prompt", ""), `"auto_review_enabled":false`) {
		t.Fatal("false toggle not saved")
	}
	rows := seedAutoReviewRows(t, srv, trader, 1, time.Now().UTC())
	review := validAutoReview(rows[0].ID)
	review.Lesson, review.FailureMode = "独有的AI教训", "weak_setup"
	if err := srv.store.TradeJournal().UpdateAIReview(trader, review, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv.reviewAI = reviewAIFunc(func(_ context.Context, _, _, prompt string) (string, error) {
		calls++
		if !strings.Contains(prompt, "独有的AI教训") || !strings.Contains(prompt, "failure_mode=weak_setup") {
			t.Fatal("AI reviewed row missing from extraction prompt")
		}
		return "[]", nil
	})
	do("POST", "/extract?trader_id="+trader, "")
	if calls != 1 {
		t.Fatalf("extraction never called fake AI: %d", calls)
	}
}

func TestParseAutoReviewsNormalizesOffVocabularyLabels(t *testing.T) {
	batch := []*store.TradeJournalDB{{ID: 7}}
	reviews, err := parseAutoReviews(`[{"id":7,"failure_mode":"fomo","mistake_category":"psychology","strategy_tag":"swing","executed_as_plan":"mostly"}]`, batch)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := reviews[0]
	if r.FailureMode != "other" || r.MistakeCategory != "" || r.StrategyTag != "other" || r.ExecutedAsPlan != "unknown" {
		t.Fatalf("labels not normalized: %+v", r)
	}
}

func TestSymbolReviewCoTMatchesBaseSymbol(t *testing.T) {
	got := symbolReviewCoT("BTC 横盘\nZEC 放量突破\nETH 观望", "ZECUSDT")
	if got != "ZEC 放量突破" {
		t.Fatalf("got %q", got)
	}
}
