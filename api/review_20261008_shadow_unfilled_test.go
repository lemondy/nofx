package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"nofx/store"

	"github.com/gin-gonic/gin"
)

// review 2026-10-08 D: unfilled is a separate count and never carries R.
func TestGateShadowStatsCountsUnfilledWithoutR(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st, err := store.NewWithConfig(store.DBConfig{
		Type: store.DBTypeSQLite, Path: filepath.Join(t.TempDir(), "shadow_unfilled.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const userID, traderID = "user-unfilled", "trader-unfilled"
	if err := st.User().Create(&store.User{ID: userID, Email: "unfilled@test.com"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Trader().Create(&store.Trader{ID: traderID, UserID: userID, Name: "Unfilled"}); err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"long", "short"} {
		stop := 95.0
		if direction == "short" {
			stop = 105
		}
		row := &store.GateShadowBlock{
			TraderID: traderID, Symbol: "BTCUSDT", Direction: direction, EntryBasis: "limit_anchor",
			EntryPrice: 100, StopPrice: stop, PlanRR: 2, HorizonHours: 8,
			BlockedCodes: "RR_MAX_0.07", CreatedAt: time.Now().UTC(),
		}
		if created, err := st.GateShadow().CreateIfIdle(row); err != nil || !created {
			t.Fatalf("create: %v, %v", created, err)
		}
		// Even a positive stale exit must not give an unfilled row an R.
		if err := st.GateShadow().MarkEvaluated(row.ID, "unfilled", 110, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := st.GateShadow().MarkEvaluated48(row.ID, "unfilled", 90, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/gate-shadow-stats", nil)
	c.Set("user_id", userID)
	(&Server{store: st}).handleGateShadowStats(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	type tally struct {
		Total    int     `json:"total"`
		Unfilled int     `json:"unfilled"`
		NoData   int     `json:"no_data"`
		SumR     float64 `json:"sum_r"`
		AvgR     float64 `json:"avg_r"`
		RSamples int     `json:"r_samples"`
		WinRate  float64 `json:"win_rate_pct"`
	}
	var got struct {
		Overall   tally            `json:"overall"`
		Overall48 tally            `json:"overall_48h"`
		ByCode    map[string]tally `json:"by_code"`
		ByCode48  map[string]tally `json:"by_code_48h"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := tally{Total: 2, Unfilled: 2}
	for name, actual := range map[string]tally{
		"overall": got.Overall, "overall_48h": got.Overall48,
		"RR_MAX": got.ByCode["RR_MAX"], "RR_MAX_48h": got.ByCode48["RR_MAX"],
	} {
		if actual != want {
			t.Errorf("%s = %+v, want %+v", name, actual, want)
		}
	}
}
