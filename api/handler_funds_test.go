package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"nofx/fundsledger"
	"nofx/store"
	"nofx/trader/types"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fundsFixture(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	srv, r, token, _, traderID := idorFixture(t)
	r.GET("/api/funds/overview", srv.authMiddleware(), srv.handleFundsOverview)
	now := time.Now().UTC()
	if err := srv.store.GormDB().Model(&store.Trader{}).Where("id = ?", traderID).Updates(map[string]interface{}{"exchange_id": "live", "initial_balance": 100}).Error; err != nil {
		t.Fatal(err)
	}
	for _, e := range []store.Exchange{{ID: "live", UserID: "user-aaa", Name: "Binance", ExchangeType: "binance"}, {ID: "paper", UserID: "user-aaa", Name: "Paper", ExchangeType: "paper"}} {
		if err := srv.store.GormDB().Create(&e).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.store.Trader().Create(&store.Trader{ID: "paper-a", UserID: "user-aaa", Name: "paper", ExchangeID: "paper", InitialBalance: 10000}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		id     string
		equity float64
	}{{traderID, 110}, {"trader-B", 9999}, {"paper-a", 20000}} {
		for i, eq := range []float64{100, r.equity} {
			if err := srv.store.Equity().Save(&store.EquitySnapshot{TraderID: r.id, Timestamp: now.Add(time.Duration(-2+i) * time.Hour), TotalEquity: eq}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return srv, r, token
}
func fundsRequest(r http.Handler, token, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/funds/overview"+query, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func TestFundsValidationAuthAndIsolation(t *testing.T) {
	_, r, token := fundsFixture(t)
	for _, q := range []string{"?days=0", "?days=366", "?days=-1", "?days=1.5", "?days=abc", "?days=", "?days=7&days=8", "?tz=Invalid/Zone", "?tz=Local", "?tz=", "?include_paper=2", "?include_paper="} {
		w := fundsRequest(r, token, q)
		if w.Code != 400 {
			t.Errorf("%s got %d: %s", q, w.Code, w.Body.String())
		}
	}
	if w := fundsRequest(r, "", ""); w.Code != 401 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, q := range []string{"", "?days=1&tz=UTC", "?days=365&tz=" + url.QueryEscape("Asia/Shanghai"), "?trader_id=trader-B"} {
		w := fundsRequest(r, token, q)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var out fundsledger.Overview
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Accounts) != 1 || out.Accounts[0].TraderID != "trader-A" || out.Totals.CurrentEquity != 110 || strings.Contains(w.Body.String(), "trader-B") {
			t.Fatal(w.Body.String())
		}
	}
	w := fundsRequest(r, token, "?include_paper=1")
	var out fundsledger.Overview
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 2 || out.Totals.CurrentEquity != 110 {
		t.Fatal(w.Body.String())
	}
	if len(out.Accounts[0].Warnings) == 0 {
		t.Fatal("unloaded reader must be a warning, not HTTP failure")
	}
}

type fundsReaderFunc func(context.Context, time.Time, time.Time) ([]fundsledger.Income, error)

func (f fundsReaderFunc) ReadIncomeHistory(ctx context.Context, s, e time.Time) ([]fundsledger.Income, error) {
	return f(ctx, s, e)
}

func TestFundsSyncBackfillIncrementOverlapAndPartialRetry(t *testing.T) {
	srv, _, _ := fundsFixture(t)
	now := time.Now().UTC()
	state := srv.fundsSyncEntry("fixture")
	calls := 0
	reader := fundsReaderFunc(func(_ context.Context, start, end time.Time) ([]fundsledger.Income, error) {
		calls++
		if calls == 1 && !start.Equal(now.AddDate(0, 0, -90)) {
			t.Fatal(start)
		}
		if !end.Equal(now) {
			t.Fatal(end)
		}
		return []fundsledger.Income{{TranID: "t", IncomeType: "TRANSFER", Asset: "USDT", Amount: 500, TimeMS: now.Add(-2 * time.Hour).UnixMilli()}}, nil
	})
	srv.readFundsIncome(context.Background(), "sync", fundsledger.LiveFutures, now, state, reader)
	latest := now.Add(-2 * time.Hour).Truncate(time.Millisecond)
	reader = fundsReaderFunc(func(_ context.Context, start, end time.Time) ([]fundsledger.Income, error) {
		if !start.Equal(latest.Add(-time.Hour)) {
			t.Fatalf("overlap %s", start)
		}
		return []fundsledger.Income{{TranID: "partial", IncomeType: "TRANSFER", Asset: "USDT", Amount: 10, TimeMS: now.Add(-time.Hour).UnixMilli()}}, types.ErrIncomeHistoryUnavailable
	})
	warnings := srv.readFundsIncome(context.Background(), "sync", fundsledger.LiveFutures, now, state, reader)
	if len(warnings) < 2 {
		t.Fatal(warnings)
	}
	// A partially saved recent row must not advance past the failed window.
	reader = fundsReaderFunc(func(_ context.Context, start, end time.Time) ([]fundsledger.Income, error) {
		if !start.Equal(latest.Add(-time.Hour)) {
			t.Fatalf("lost failed window: %s", start)
		}
		return nil, nil
	})
	srv.readFundsIncome(context.Background(), "sync", fundsledger.LiveFutures, now, state, reader)
	if !state.retryFrom[fundsledger.LiveFutures].IsZero() {
		t.Fatal(state.retryFrom)
	}
	rows, err := srv.store.AccountIncome().List("sync", 0, now.UnixMilli())
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	// Spot starts its own 90-day cursor even on the same exchange ID.
	reader = fundsReaderFunc(func(_ context.Context, start, end time.Time) ([]fundsledger.Income, error) {
		if !start.Equal(now.AddDate(0, 0, -90)) {
			t.Fatal(start)
		}
		return nil, errors.New("permission denied")
	})
	if len(srv.readFundsIncome(context.Background(), "sync", fundsledger.LiveSpot, now, state, reader)) == 0 {
		t.Fatal("missing spot warning")
	}
}
func TestFundsSyncSerializesAndThrottlesExchangeIncludingBothWallets(t *testing.T) {
	srv := &Server{}
	state := srv.fundsSyncEntry("u/e")
	account := fundsSyncAccount{exchangeID: "e", traders: map[string]string{fundsledger.LiveFutures: "f", fundsledger.LiveSpot: "s"}}
	now := time.Now()
	var calls atomic.Int64
	var wg sync.WaitGroup
	read := func(kind, id string) []string { calls.Add(1); return []string{kind} }
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); runFundsSync(state, account, now, read) }()
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
	runFundsSync(state, account, now.Add(15*time.Minute-time.Nanosecond), read)
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
	got := runFundsSync(state, account, now.Add(15*time.Minute), read)
	if calls.Load() != 4 {
		t.Fatal(calls.Load())
	}
	got[fundsledger.LiveSpot][0] = "changed"
	if state.warnings[fundsledger.LiveSpot][0] == "changed" {
		t.Fatal("returned mutable cache")
	}
}

func TestFundsForeignExchangeReferenceCannotLeakIncome(t *testing.T) {
	srv, r, token := fundsFixture(t)
	now := time.Now().UTC()
	if err := srv.store.GormDB().Create(&store.Exchange{ID: "foreign-exchange", UserID: "user-bbb", Name: "secret", ExchangeType: "binance"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := srv.store.GormDB().Model(&store.Trader{}).Where("id = ?", "trader-A").Update("exchange_id", "foreign-exchange").Error; err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AccountIncome().Save("foreign-exchange", []fundsledger.Income{{TranID: "secret-income", IncomeType: "TRANSFER", Asset: "USDT", Amount: 999, TimeMS: now.Add(-90 * time.Minute).UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	w := fundsRequest(r, token, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret-income") || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Body.String())
	}
}
