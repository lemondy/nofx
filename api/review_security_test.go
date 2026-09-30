package api

import (
	"bytes"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"nofx/auth"
	"nofx/market/breakout"
	"nofx/store"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPasswordRequiresCurrentAndRevokesOldTokens(t *testing.T) {
	srv, r, token, userID, _ := idorFixture(t)
	hash, err := auth.HashPassword("old-password")
	if err != nil {
		t.Fatal(err)
	}
	// UpdatePassword bumps version, so mint a fresh fixture token afterward.
	if err := srv.store.User().UpdatePassword(userID, hash); err != nil {
		t.Fatal(err)
	}
	user, _ := srv.store.User().GetByID(userID)
	token, err = auth.GenerateJWTWithVersion(userID, user.Email, user.TokenVersion)
	if err != nil {
		t.Fatal(err)
	}
	r.PUT("/password", srv.authMiddleware(), srv.handleChangePassword)
	for _, tc := range []struct {
		current string
		want    int
	}{{"wrong", 401}, {"old-password", 200}} {
		req := httptest.NewRequest("PUT", "/password", strings.NewReader(`{"current_password":"`+tc.current+`","new_password":"new-password"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		if out.Code != tc.want {
			t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
		}
	}
	req := httptest.NewRequest("GET", "/ai-costs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != 401 {
		t.Fatal("old token survived password change")
	}
}

func TestUnverifiedResetAndPrivateKeyEndpointsDisabled(t *testing.T) {
	srv, r, _, _, _ := idorFixture(t)
	r.POST("/reset", srv.handleResetPassword)
	r.POST("/wallet", srv.handleWalletValidate)
	r.POST("/generate", srv.handleWalletGenerate)
	for _, tc := range []struct {
		path, body string
		status     int
	}{{"/reset", `{"email":"user-aaa@test.com","new_password":"attacker"}`, 410}, {"/wallet", `{"private_key":"0x123"}`, 400}, {"/generate", "{}", 410}} {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		if out.Code != tc.status {
			t.Fatalf("%s got %d", tc.path, out.Code)
		}
	}
}

func TestLogoutRevocationPersistsAcrossStoreReopen(t *testing.T) {
	srv, r, token, _, _ := idorFixture(t)
	r.POST("/logout", srv.authMiddleware(), srv.handleLogout)
	req := httptest.NewRequest("POST", "/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("%d %s", out.Code, out.Body.String())
	}
	// A fresh store wrapper has no in-memory blacklist state of its own.
	var databases []struct{ File string }
	if err := srv.store.GormDB().Raw("PRAGMA database_list").Scan(&databases).Error; err != nil || len(databases) == 0 {
		t.Fatal("database path unavailable", err)
	}
	fresh, err := store.New(databases[0].File)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	revoked, err := fresh.TokenRevoked(token)
	if err != nil || !revoked {
		t.Fatalf("revoked=%v err=%v", revoked, err)
	}
}

func TestPromptCompetitionAndGridRejectForeignTrader(t *testing.T) {
	srv, r, token, _, _ := idorFixture(t)
	r.PUT("/traders/:id/prompt", srv.authMiddleware(), srv.handleUpdateTraderPrompt)
	r.PUT("/traders/:id/competition", srv.authMiddleware(), srv.handleToggleCompetition)
	r.GET("/traders/:id/grid", srv.authMiddleware(), srv.handleGetGridRiskInfo)
	for _, tc := range []struct{ method, path string }{{"PUT", "prompt"}, {"PUT", "competition"}, {"GET", "grid"}} {
		req := httptest.NewRequest(tc.method, "/traders/trader-B/"+tc.path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		if out.Code != http.StatusNotFound {
			t.Fatalf("%s status %d", tc.path, out.Code)
		}
	}
}

func TestRequestBodyAndRateLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestLimits())
	r.POST("/api/login", func(c *gin.Context) { c.Status(200) })
	req := httptest.NewRequest("POST", "/api/login", bytes.NewReader(make([]byte, (2<<20)+1)))
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	if out.Code != 413 {
		t.Fatal(out.Code)
	}
	for i := 0; i < 11; i++ {
		out = httptest.NewRecorder()
		r.ServeHTTP(out, httptest.NewRequest("POST", "/api/login", nil))
		want := 200
		if i == 10 {
			want = 429
		}
		if out.Code != want {
			t.Fatalf("attempt %d: %d", i, out.Code)
		}
	}
}

func TestBreakoutCacheSingleFlightBoundAndScanLimit(t *testing.T) {
	original := breakoutAnalyze
	defer func() { breakoutAnalyze = original; breakoutCache = map[string]*breakoutCacheEntry{}; scanCache = nil }()
	breakoutCache = map[string]*breakoutCacheEntry{}
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	breakoutAnalyze = func(string) (*breakout.Report, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return &breakout.Report{}, nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); getBreakoutReport("BTCUSDT") }()
	<-entered
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); getBreakoutReport("BTCUSDT") }()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
	for i := 0; i < 300; i++ {
		getBreakoutReport(strings.Repeat("X", i+1))
	}
	if len(breakoutCache) > 256 {
		t.Fatal("unbounded cache")
	}
	scanCache = &struct {
		results []breakout.ScanResult
		expires time.Time
	}{make([]breakout.ScanResult, 30), time.Now().Add(time.Minute)}
	r := gin.New()
	r.GET("/scan", (&Server{}).handleBreakoutScan)
	for _, limit := range []string{"5", "20"} {
		out := httptest.NewRecorder()
		r.ServeHTTP(out, httptest.NewRequest("GET", "/scan?limit="+limit, nil))
		if out.Code != 200 || !strings.Contains(out.Body.String(), `"count":`+limit) {
			t.Fatal(out.Body.String())
		}
	}
}
