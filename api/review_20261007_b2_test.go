package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"nofx/manager"
	"nofx/store"
	"nofx/trader"

	"github.com/gin-gonic/gin"
)

// syncBalanceFixture builds a minimal two-table store (trader + enabled
// binance exchange + enabled AI model) and a Server wiring it, so
// handleSyncBalance can run end-to-end with its network probe and background
// reload injected. No real trader is ever started.
// review 2026-10-07 B2-1
func syncBalanceFixture(t *testing.T, exchangeEnabled bool) (*Server, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st, err := store.NewWithConfig(store.DBConfig{
		Type: store.DBTypeSQLite,
		Path: filepath.Join(t.TempDir(), "syncbalance.db"),
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	userID := "user-sync"
	if err := st.User().Create(&store.User{ID: userID, Email: "sync@test.com"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	exchangeID, err := st.Exchange().Create(userID, "binance", "ProbeAccount", exchangeEnabled,
		// Empty credentials: EncryptedString rejects non-empty values without
		// the global crypto service, and the injected probe never touches them.
		"", "", "", false,
		"", false, "", "", "", "", "", "", 0)
	if err != nil {
		t.Fatalf("create exchange: %v", err)
	}
	if err := st.AIModel().Create(userID, "model-sync", "Sync Model", "deepseek", true, "", ""); err != nil {
		t.Fatalf("create ai model: %v", err)
	}
	traderID := "trader-sync"
	if err := st.Trader().Create(&store.Trader{
		ID:             traderID,
		UserID:         userID,
		Name:           "Sync",
		AIModelID:      "model-sync",
		ExchangeID:     exchangeID,
		InitialBalance: 100,
		IsRunning:      false,
	}); err != nil {
		t.Fatalf("create trader: %v", err)
	}

	srv := &Server{
		store:                     st,
		traderManager:             manager.NewTraderManager(),
		exchangeAccountStateCache: NewExchangeAccountStateCache(),
	}
	return srv, userID, traderID
}

func serveSyncBalance(srv *Server, userID, traderID string) *httptest.ResponseRecorder {
	r := gin.New()
	r.POST("/sync/:id", func(c *gin.Context) {
		c.Set("user_id", userID)
		srv.handleSyncBalance(c)
	})
	req := httptest.NewRequest(http.MethodPost, "/sync/"+traderID, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// review 2026-10-07 B2-1 (P1-1): the remove→reload chain (Stop() joins the
// in-flight AI cycle, routinely minutes) must run in the background — the
// HTTP response must already be committed and the DB write persisted while
// the reload is still blocked, never waiting for it.
func TestSyncBalanceDoesNotWaitForReload(t *testing.T) {
	srv, userID, traderID := syncBalanceFixture(t, true)

	origFetch, origReload := syncBalanceFetchBalance, syncBalanceReloadInBackground
	t.Cleanup(func() { syncBalanceFetchBalance, syncBalanceReloadInBackground = origFetch, origReload })

	reloadStarted := make(chan struct{})
	reloadRelease := make(chan struct{})
	reloadDone := make(chan struct{})
	syncBalanceFetchBalance = func(trader.Trader) (map[string]interface{}, error) {
		return map[string]interface{}{"total_equity": 123.45}, nil
	}
	syncBalanceReloadInBackground = func(s *Server, uid, tid string) {
		go func() {
			defer close(reloadDone)
			close(reloadStarted)
			// Simulate Stop() joining a multi-minute in-flight AI cycle.
			<-reloadRelease
		}()
	}

	rec := serveSyncBalance(srv, userID, traderID)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The response returned while the reload gate is still closed — the
	// handler provably did not wait for the reload.
	select {
	case <-reloadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("background reload was not spawned before the response returned")
	}

	// The locked DB-write phase must have completed before the response too.
	got, err := srv.store.Trader().GetForUser(userID, traderID)
	if err != nil {
		t.Fatalf("get trader: %v", err)
	}
	if got.InitialBalance != 123.45 {
		t.Fatalf("initial_balance = %v, want 123.45 persisted before responding", got.InitialBalance)
	}

	close(reloadRelease)
	select {
	case <-reloadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("background reload goroutine did not finish after release")
	}
}

// review 2026-10-07 B2-1 (P1-1): the GetBalance network probe must run
// WITHOUT holding the server-global traderOpsMu. The test grabs the lock the
// way a concurrent update/delete would and requires the probe to run anyway.
func TestSyncBalanceProbeRunsOutsideOpsLock(t *testing.T) {
	srv, userID, traderID := syncBalanceFixture(t, true)

	origFetch, origReload := syncBalanceFetchBalance, syncBalanceReloadInBackground
	t.Cleanup(func() { syncBalanceFetchBalance, syncBalanceReloadInBackground = origFetch, origReload })

	probeEntered := make(chan struct{})
	probeRelease := make(chan struct{})
	syncBalanceFetchBalance = func(trader.Trader) (map[string]interface{}, error) {
		close(probeEntered)
		<-probeRelease
		return map[string]interface{}{"total_equity": 55.5}, nil
	}
	syncBalanceReloadInBackground = func(*Server, string, string) {} // not under test here

	// Hold the global ops lock across the whole request.
	srv.traderOpsMu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			srv.traderOpsMu.Unlock()
		}
	}()

	respCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		respCh <- serveSyncBalance(srv, userID, traderID)
	}()

	select {
	case <-probeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("balance probe blocked behind traderOpsMu — the network call is back under the global lock")
	}
	srv.traderOpsMu.Unlock()
	unlocked = true
	close(probeRelease)

	select {
	case rec := <-respCh:
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not finish after the ops lock was released")
	}
}

// review 2026-10-07 B2-1: the validation phase must reject a disabled
// exchange before any network probe is attempted.
func TestSyncBalanceRejectsDisabledExchange(t *testing.T) {
	srv, userID, traderID := syncBalanceFixture(t, false)

	origFetch, origReload := syncBalanceFetchBalance, syncBalanceReloadInBackground
	t.Cleanup(func() { syncBalanceFetchBalance, syncBalanceReloadInBackground = origFetch, origReload })
	syncBalanceFetchBalance = func(trader.Trader) (map[string]interface{}, error) {
		t.Error("probe must not run for a disabled exchange")
		return nil, nil
	}
	syncBalanceReloadInBackground = func(*Server, string, string) {}

	rec := serveSyncBalance(srv, userID, traderID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
}
